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
// (_1m 35 days, _1h 400 days, _1d forever, as on the nodes).
func Jobs(raw time.Duration) []central.Job {
	return []central.Job{
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
				res, err := tx.Exec(ctx, `DELETE FROM `+t.Name+` WHERE ts < $1`, now.Add(-keep).Unix())
				if err != nil {
					return n, err
				}
				n += res.RowsAffected()
			}
			return n, nil
		}},
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
					if _, err := tx.Exec(ctx, `TRUNCATE `+table); err != nil {
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
