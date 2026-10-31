package pgquerylog

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// Jobs are the leader's housekeeping of the central query log: daily
// partitions three days ahead, expired fact partitions dropped whole (raw
// is database.querylog.retention.raw; 0 keeps them) and expired rollups
// (_1m 35 days, _1h 400 days, _1d forever, as on the nodes), then a
// bounded sweep of dictionaries unreferenced by any remaining facts/rollups.
func Jobs(raw time.Duration) []central.Job {
	return []central.Job{
		{Name: "querylog-stopped-source-rollups", Timeout: backfillTimeout, Run: backfillStopped},
		{Name: "querylog-partitions", Run: func(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(partitionLock)); err != nil {
				return 0, err
			}
			today := floorDay(now.UnixMicro())
			for d := today - dayUS; d <= today+3*dayUS; d += dayUS {
				//nolint:gosec // the name is built from a date; the bounds are integers
				if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF query FOR VALUES FROM (%d) TO (%d)`,
					PartitionName(d), d, d+dayUS)); err != nil {
					return 0, err
				}
			}
			return 0, nil
		}},
		{Name: "querylog-raw-retention", Run: func(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
			if raw <= 0 {
				return 0, nil
			}
			cutoff := floorDay(now.Add(-raw).UnixMicro())
			parts, err := partitions(ctx, tx)
			if err != nil {
				return 0, err
			}
			var dropped int64
			for name, day := range parts {
				if day+dayUS <= cutoff {
					var pending bool
					if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rollup_input_hour f LEFT JOIN rollup_source_state s ON s.source=f.source LEFT JOIN rollup_bucket b ON b.source=f.source AND b.resolution=3600 AND b.ts=f.ts
      WHERE f.ts>=$1 AND f.ts<$2 AND (coalesce(s.legacy_before,0)>f.ts OR b.source IS NULL OR b.origin='pending' OR b.origin='synthetic' AND b.revision<>f.revision))`, day/1000000, (day+dayUS)/1000000).Scan(&pending); err != nil {
						return dropped, err
					}
					if pending {
						continue
					}
					if _, err := tx.Exec(ctx, `UPDATE rollup_input_hour SET input_complete=false WHERE ts>=$1 AND ts<$2`, day/1000000, (day+dayUS)/1000000); err != nil {
						return dropped, err
					}
					if _, err := tx.Exec(ctx, `DELETE FROM query_domain_hour WHERE ts>=$1 AND ts<$2`, day/1000000, (day+dayUS)/1000000); err != nil {
						return dropped, err
					}
					if _, err := tx.Exec(ctx, `ALTER TABLE query DETACH PARTITION `+pgx.Identifier{name}.Sanitize()); err != nil {
						return dropped, err
					}
					if _, err := tx.Exec(ctx, `DROP TABLE IF EXISTS `+pgx.Identifier{name}.Sanitize()); err != nil {
						return dropped, err
					}
					dropped++
				}
			}
			return dropped, nil
		}},
		{Name: "querylog-rollup-retention", Run: func(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
			var n int64
			for _, t := range sqlitelog.RollupTables {
				var keep time.Duration
				switch t.Res {
				case 60:
					keep = sqlitelog.MinuteRetention
				case 3600:
					keep = sqlitelog.HourRetention
				default:
					continue
				}
				//nolint:gosec // table names are package constants
				deleteSQL := `DELETE FROM ` + t.Name + ` r WHERE r.ts < $1 AND r.source IS NOT NULL AND NOT EXISTS(
     SELECT 1 FROM rollup_source_state s WHERE s.source=r.source AND r.ts<s.legacy_before)
     AND NOT EXISTS(SELECT 1 FROM rollup_input_hour f LEFT JOIN rollup_bucket h ON h.source=f.source AND h.resolution=3600 AND h.ts=f.ts
     LEFT JOIN rollup_bucket d ON d.source=f.source AND d.resolution=86400 AND d.ts=(f.ts/86400)*86400
     WHERE f.source=r.source AND f.ts=(r.ts/3600)*3600 AND (h.source IS NULL OR h.origin='pending' OR h.origin='synthetic' AND h.revision<>f.revision OR d.source IS NULL OR d.origin='pending' OR d.origin='synthetic' AND d.revision<>(SELECT sum(i.revision) FROM rollup_input_hour i WHERE i.source=f.source AND i.ts>=(f.ts/86400)*86400 AND i.ts<(f.ts/86400+1)*86400)))`

				if t.Key == "" {
					marked := `UPDATE rollup_input_hour f SET minute_complete=false FROM expired e WHERE f.source=e.source AND f.ts=(e.ts/3600)*3600`
					if t.Res == 3600 {
						marked = `UPDATE rollup_bucket b SET retained=false FROM expired e WHERE b.source=e.source AND b.resolution=3600 AND b.ts=e.ts`
					}
					var removed int64
					if err := tx.QueryRow(ctx, `WITH expired AS (`+deleteSQL+` RETURNING r.source,r.ts), marked AS (`+marked+` RETURNING 1) SELECT count(*) FROM expired`, now.Add(-keep).Unix()).Scan(&removed); err != nil {
						return n, err
					}
					n += removed
					continue
				}
				res, err := tx.Exec(ctx, deleteSQL, now.Add(-keep).Unix())
				if err != nil {
					return n, err
				}
				n += res.RowsAffected()
			}
			return n, nil
		}},
		{Name: "querylog-dictionary-retention", Timeout: dictionaryPruneTimeout, Run: pruneDictionaries},
	}
}

// partitions returns the daily partitions of query by name and day start.
func partitions(ctx context.Context, tx pgx.Tx) (map[string]int64, error) {
	rows, err := tx.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'query'::regclass`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		d, err := time.Parse("20060102", strings.TrimPrefix(name, "query_"))
		if err != nil {
			continue // not ours
		}
		out[name] = d.UnixMicro()
	}
	return out, rows.Err()
}

// Purger deletes the per-query rows of every node from the central
// database older than before, or all of them when before is zero
// (DELETE /api/v1/querylog with a central database). Statistics are kept,
// as on a node. Rows a node has not shipped yet arrive later, and other
// nodes' own query logs are not purged.
type Purger struct {
	DB *central.DB
}

// Purge implements api.QuerylogPurger.
//
// Whole days go with TRUNCATE (their partitions stay, for the raw
// retention to drop); only the day the cutoff falls in is deleted row by
// row. The pool's statement_timeout does not apply.
func (p Purger) Purge(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := p.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := central.NoStatementTimeout(ctx, tx); err != nil {
			return err
		}
		cutoff := int64(math.MaxInt64)
		if !before.IsZero() {
			cutoff = before.UnixMicro()
		}
		var gated bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rollup_input_hour f JOIN rollup_source_state s ON s.source=f.source WHERE f.ts<s.legacy_before AND f.ts*1000000<$1)`, cutoff).Scan(&gated); err != nil {
			return err
		}
		if gated {
			return errLegacyProvenance
		}
		if _, err := tx.Exec(ctx, `UPDATE rollup_source_state SET input_partial=true WHERE source IN (SELECT DISTINCT source FROM query WHERE ts<$1)`, cutoff); err != nil {
			return err
		}
		parts, err := partitions(ctx, tx)
		if err != nil {
			return err
		}
		for name, day := range parts {
			table := pgx.Identifier{name}.Sanitize()
			switch {
			case day+dayUS <= cutoff:
				var rows int64
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil {
					return err
				}
				if rows > 0 {
					if _, err := tx.Exec(ctx, `DELETE FROM query_domain_hour WHERE ts>=$1 AND ts<$2`, day/1000000, (day+dayUS)/1000000); err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, `TRUNCATE `+table+`, query_domain_guard`); err != nil {
						return err
					}
				}
				n += rows
			case day < cutoff:
				res, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE ts < $1`, cutoff)
				if err != nil {
					return err
				}
				n += res.RowsAffected()
			}
		}
		return nil
	})
	return n, err
}
