package sqlitelog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// RetentionReport describes one retention run.
type RetentionReport struct {
	// RawDeleted counts fact rows removed by age.
	RawDeleted int64 `json:"raw_deleted"`
	// SizeDeleted counts fact rows removed to get under maxDBBytes.
	SizeDeleted int64 `json:"size_deleted"`
	// RollupsDeleted counts expired rollup rows.
	RollupsDeleted int64 `json:"rollups_deleted"`
	// DictPruned counts dictionary rows removed by last_seen.
	DictPruned int64 `json:"dict_pruned"`
	// DBBytesBefore and DBBytesAfter bracket the run.
	DBBytesBefore int64 `json:"db_bytes_before"`
	DBBytesAfter  int64 `json:"db_bytes_after"`
}

// Retain runs the daily retention job now: fact rows older than
// RawRetention, the size cap, expired rollups (_1m 35 d, _1h 400 d) and
// dictionary entries not seen for 400 days (or since the oldest fact row,
// whichever is older). Deletes run in DeleteBatch-row transactions with an
// incremental_vacuum after each, releasing the write lock in between so
// ingest continues.
func (w *Writer) Retain(ctx context.Context) (RetentionReport, error) {
	var rep RetentionReport
	now := w.clock.Now()
	var err error
	if rep.DBBytesBefore, err = w.DBBytes(ctx); err != nil {
		return rep, err
	}
	if w.opts.RawRetention > 0 {
		cutoff := now.Add(-w.opts.RawRetention).UnixMicro()
		if rep.RawDeleted, err = w.deleteFacts(ctx, `DELETE FROM query WHERE id IN (SELECT id FROM query WHERE ts < ? LIMIT `+fmt.Sprint(w.deleteBatch)+`)`, cutoff); err != nil {
			return rep, err
		}
	}
	if rep.SizeDeleted, err = w.enforceSize(ctx); err != nil {
		return rep, err
	}
	for _, t := range minuteTables {
		n, err := w.deleteRollups(ctx, t, now.Add(-MinuteRetention).Unix())
		if err != nil {
			return rep, err
		}
		rep.RollupsDeleted += n
	}
	for _, t := range hourTables {
		n, err := w.deleteRollups(ctx, t, now.Add(-HourRetention).Unix())
		if err != nil {
			return rep, err
		}
		rep.RollupsDeleted += n
	}
	if rep.DictPruned, err = w.pruneDicts(ctx, now); err != nil {
		return rep, err
	}
	err = w.locked(func() error {
		if _, err := w.w.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return err
		}
		return writeMeta(ctx, w.w, metaLastRetention, now.Unix())
	})
	if err != nil {
		return rep, err
	}
	rep.DBBytesAfter, err = w.DBBytes(ctx)
	w.updateGauges(ctx)
	return rep, err
}

// locked runs fn under the writer lock unless the writer is closed.
func (w *Writer) locked(fn func() error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return fn()
}

// deleteFacts runs a batched fact delete until a batch removes fewer than
// DeleteBatch rows.
func (w *Writer) deleteFacts(ctx context.Context, q string, args ...any) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n int64
		err := w.locked(func() error {
			res, err := w.w.ExecContext(ctx, q, args...)
			if err != nil {
				return fmt.Errorf("sqlitelog: retention delete: %w", err)
			}
			n, _ = res.RowsAffected()
			if _, err := w.w.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
				return fmt.Errorf("sqlitelog: incremental_vacuum: %w", err)
			}
			w.rows.Add(-n)
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(w.deleteBatch) {
			return total, nil
		}
	}
}

// deleteRollups removes rows older than cutoff from a WITHOUT ROWID rollup
// table, 60 buckets per transaction.
func (w *Writer) deleteRollups(ctx context.Context, table string, cutoff int64) (int64, error) {
	var total int64
	for {
		var n int64
		err := w.locked(func() error {
			res, err := w.w.ExecContext(ctx, `DELETE FROM `+table+` WHERE ts IN (SELECT DISTINCT ts FROM `+table+` WHERE ts < ? ORDER BY ts LIMIT 60)`, cutoff) //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
			if err != nil {
				return fmt.Errorf("sqlitelog: expire %s: %w", table, err)
			}
			n, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// pruneDicts removes domain and client dictionary rows whose last_seen is
// older than both the _1h retention and the oldest fact row, with the
// per-key _1d rows that reference them (those keys have not been seen for
// 400 days; the aggregate rollup_1d totals are kept forever).
func (w *Writer) pruneDicts(ctx context.Context, now time.Time) (int64, error) {
	var total int64
	err := w.locked(func() error {
		cutoff := now.Add(-HourRetention).Unix()
		var oldest sql.NullInt64
		if err := w.w.QueryRowContext(ctx, `SELECT min(ts) FROM query`).Scan(&oldest); err != nil {
			return err
		}
		if oldest.Valid && oldest.Int64/1e6 < cutoff {
			cutoff = oldest.Int64 / 1e6
		}
		cutoff -= int64(dictSlack / time.Second)
		tx, err := w.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, q := range []string{
			`DELETE FROM rollup_domain_1d WHERE domain IN (SELECT id FROM dict_domain WHERE last_seen < ?1)`,
			`DELETE FROM rollup_client_1d WHERE client IN (SELECT id FROM dict_client WHERE last_seen < ?1)`,
		} {
			if _, err := tx.ExecContext(ctx, q, cutoff); err != nil {
				return fmt.Errorf("sqlitelog: prune dictionaries: %w", err)
			}
		}
		for _, q := range []string{
			`DELETE FROM dict_domain WHERE last_seen < ?1`,
			`DELETE FROM dict_client WHERE last_seen < ?1`,
		} {
			res, err := tx.ExecContext(ctx, q, cutoff)
			if err != nil {
				return fmt.Errorf("sqlitelog: prune dictionaries: %w", err)
			}
			n, _ := res.RowsAffected()
			total += n
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if total > 0 {
			w.dicts.purge()
		}
		return nil
	})
	return total, err
}

// enforceSize deletes the oldest fact rows while the database exceeds
// MaxDBBytes, down to 95 % of it, and returns the rows deleted.
func (w *Writer) enforceSize(ctx context.Context) (int64, error) {
	if w.opts.MaxDBBytes <= 0 {
		return 0, nil
	}
	size, err := w.DBBytes(ctx)
	if err != nil || size <= w.opts.MaxDBBytes {
		return 0, err
	}
	target := w.opts.MaxDBBytes / 100 * 95
	var total int64
	for size > target {
		n, err := w.deleteFactsOnce(ctx, `DELETE FROM query WHERE id IN (SELECT id FROM query ORDER BY id LIMIT `+fmt.Sprint(w.deleteBatch)+`)`)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			break
		}
		if size, err = w.DBBytes(ctx); err != nil {
			return total, err
		}
	}
	if total > 0 {
		w.log.Warn("querylog.db exceeded querylog.retention.maxDBBytes; deleted the oldest rows",
			"rows", total, "maxDBBytes", w.opts.MaxDBBytes, "dbBytes", size)
	}
	return total, nil
}

// deleteFactsOnce runs one batch of a fact delete.
func (w *Writer) deleteFactsOnce(ctx context.Context, q string) (int64, error) {
	var n int64
	err := w.locked(func() error {
		res, err := w.w.ExecContext(ctx, q)
		if err != nil {
			return fmt.Errorf("sqlitelog: size-cap delete: %w", err)
		}
		n, _ = res.RowsAffected()
		if _, err := w.w.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return fmt.Errorf("sqlitelog: incremental_vacuum: %w", err)
		}
		w.rows.Add(-n)
		return nil
	})
	return n, err
}

// DBBytes returns page_count × page_size: the logical database size
// including WAL-resident changes, excluding the WAL file itself.
func (w *Writer) DBBytes(ctx context.Context) (int64, error) {
	var pages, size int64
	err := w.locked(func() error {
		if err := w.w.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
			return err
		}
		return w.w.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size)
	})
	return pages * size, err
}

// WALBytes returns the size of the -wal file (0 when absent).
func (w *Writer) WALBytes() int64 {
	fi, err := os.Stat(w.opts.Path + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}

// Checkpoint runs wal_checkpoint(TRUNCATE) when no read is in flight (a
// checkpoint cannot complete past an active reader, and retrying under
// load is what makes WAL files grow). It reports whether it ran.
func (w *Writer) Checkpoint(ctx context.Context) (bool, error) {
	if w.readers.Load() > 0 {
		return false, nil
	}
	err := w.locked(func() error {
		_, err := w.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		return err
	})
	return err == nil, err
}

// analyze refreshes planner statistics (weekly) with a bounded scan.
func (w *Writer) analyze(ctx context.Context, now time.Time) error {
	err := w.locked(func() error {
		if _, err := w.w.ExecContext(ctx, `PRAGMA analysis_limit=1000; ANALYZE`); err != nil {
			return fmt.Errorf("sqlitelog: analyze: %w", err)
		}
		return writeMeta(ctx, w.w, metaLastAnalyze, now.Unix())
	})
	if err == nil {
		w.lastAnalyze = now
	}
	return err
}

// updateGauges refreshes the size, row and oldest-row gauges.
func (w *Writer) updateGauges(ctx context.Context) {
	if b, err := w.DBBytes(ctx); err == nil {
		w.metrics.DBBytes.Set(float64(b))
	}
	w.metrics.WALBytes.Set(float64(w.WALBytes()))
	w.metrics.Rows.Set(float64(w.rows.Load()))
	var oldest sql.NullInt64
	_ = w.locked(func() error {
		return w.w.QueryRowContext(ctx, `SELECT min(ts) FROM query`).Scan(&oldest)
	})
	if oldest.Valid {
		w.metrics.OldestTS.Set(float64(oldest.Int64) / 1e6)
	} else {
		w.metrics.OldestTS.Set(0)
	}
}
