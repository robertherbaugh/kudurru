// Package pgquerylog keeps the query log and statistics of every node in
// the central database (internal/central, D160).
//
// Every node keeps writing its querylog.db first; Shipper copies its new
// fact rows (by id) and its rollups into tables that mirror querylog.db,
// with the database's own dictionary ids, so the query-log reader
// (sqlitelog.NewCentralReader) answers cluster-wide questions with the
// same code as a node's own. The cursor of every stream is written in the
// same transaction as the rows it covers, so an outage or a crash is
// resumed exactly; a node keeps the rows it has not shipped (the
// retention guard) unless its size cap needs the space.
package pgquerylog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// Local is the node's query log (*sqlitelog.Writer).
type Local interface {
	SourceID(ctx context.Context) (string, error)
	Export(ctx context.Context, after int64, limit int) ([]sqlitelog.ExportRow, error)
	ExportRollup(ctx context.Context, t sqlitelog.RollupTable, from, until int64) ([]sqlitelog.RollupRow, error)
	RollupProgress() (hourDone, dayDone int64)
	SetShippedThrough(f func() int64)
}

var _ Local = (*sqlitelog.Writer)(nil)

// Streams of ship_cursor.
const (
	streamFacts  = "query"
	streamMinute = "rollup_1m"
	streamHour   = "rollup_1h"
	streamDay    = "rollup_1d"
)

// Shipper defaults.
const (
	DefaultBatch          = 5000
	DefaultInterval       = time.Second
	DefaultRollupInterval = 30 * time.Second
	// minuteReship re-sends the minutes before the cursor every round: a
	// late event can still add to a minute already flushed.
	minuteReship = 10 * 60
)

// Shipper copies a node's query log into the central database. It is an
// always-on runnable; failures are retried with backoff and never end it.
type Shipper struct {
	DB    *central.DB
	Local Local
	// Node and Version are recorded with the source.
	Node, Version string
	// Facts ships the fact rows (the query log); false ships only the
	// statistics (database.querylog.facts).
	Facts bool
	// Retention is the central raw retention: older fact rows are not
	// shipped (a fresh database is not filled with what it would drop).
	Retention time.Duration
	// Batch, Interval, RollupInterval default to DefaultBatch,
	// DefaultInterval and DefaultRollupInterval; MaxBackoff to 1 min.
	Batch                                int
	Interval, RollupInterval, MaxBackoff time.Duration
	Registerer                           prometheus.Registerer
	Logger                               *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time

	source     string
	shipped    atomic.Int64
	lastRollup time.Time
	dicts      *dicts
	partsMu    sync.Mutex
	parts      map[int64]bool
	metrics    *metrics
}

// ShippedThrough is the id of the last fact row delivered (the local
// retention guard).
func (s *Shipper) ShippedThrough() int64 { return s.shipped.Load() }

func (s *Shipper) defaults() {
	if s.Batch <= 0 {
		s.Batch = DefaultBatch
	}
	if s.Interval <= 0 {
		s.Interval = DefaultInterval
	}
	if s.RollupInterval <= 0 {
		s.RollupInterval = DefaultRollupInterval
	}
	if s.MaxBackoff <= 0 {
		s.MaxBackoff = time.Minute
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	s.dicts = newDicts()
	s.parts = map[int64]bool{}
	s.metrics = newMetrics(s.Registerer)
}

// Run ships until ctx ends.
func (s *Shipper) Run(ctx context.Context) error {
	s.defaults()
	log := s.Logger.With("component", "central", "stream", "querylog")
	src, err := s.Local.SourceID(ctx)
	if err != nil {
		return err
	}
	s.source = src
	// Until the first cursor is read, retention keeps every row.
	s.shipped.Store(0)
	s.Local.SetShippedThrough(s.ShippedThrough)
	backoff := time.Duration(0)
	quiet := true
	for {
		wait := s.Interval
		err := s.round(ctx)
		switch {
		case errors.Is(err, errMore):
			wait, err = 0, nil
		case errors.Is(err, central.ErrUnavailable), errors.Is(err, central.ErrIncompatible):
			// The database's own runnable reports its state.
			err, wait = nil, 5*time.Second
		}
		if err != nil && ctx.Err() == nil {
			backoff = min(max(2*backoff, time.Second), s.MaxBackoff)
			wait = backoff
			if quiet {
				log.Warn("query-log shipping failed; the rows stay in querylog.db and are retried", "err", err, "retry_in", backoff)
			}
			quiet = false
		} else if err == nil {
			backoff, quiet = 0, true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// errMore: a full batch was shipped; ship again at once.
var errMore = errors.New("more")

// round ships one batch of facts and, every RollupInterval, the rollups.
func (s *Shipper) round(ctx context.Context) error {
	pool, err := s.DB.Pool()
	if err != nil {
		return err
	}
	var more bool
	if s.Facts {
		if more, err = s.shipFacts(ctx); err != nil {
			return err
		}
	} else if s.shipped.Load() == 0 {
		// Nothing is shipped from the fact table: retention needs no guard.
		s.shipped.Store(1<<62 - 1)
	}
	if s.Now().Sub(s.lastRollup) >= s.RollupInterval {
		if err := s.shipRollups(ctx, pool); err != nil {
			return err
		}
		s.lastRollup = s.Now()
	}
	if more {
		return errMore
	}
	return nil
}

// tx runs fn in a transaction with the source registered.
func (s *Shipper) tx(ctx context.Context, fn func(tx pgx.Tx, sid int32) error) error {
	return s.DB.Tx(ctx, func(tx pgx.Tx) error {
		sid, err := central.EnsureSource(ctx, tx, s.source, "querylog", s.Node, s.Version)
		if err != nil {
			return err
		}
		return fn(tx, sid)
	})
}

// shipFacts ships one batch of fact rows; more reports a full batch.
func (s *Shipper) shipFacts(ctx context.Context) (more bool, err error) {
	pool, err := s.DB.Pool()
	if err != nil {
		return false, err
	}
	after, err := central.Cursor(ctx, pool, s.source, streamFacts)
	if err != nil {
		return false, err
	}
	s.shipped.Store(after)
	rows, err := s.Local.Export(ctx, after, s.Batch)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	var keep []sqlitelog.ExportRow
	if s.Retention > 0 {
		cutoff := s.Now().Add(-s.Retention).UnixMicro()
		for _, r := range rows {
			if r.TS >= cutoff {
				keep = append(keep, r)
			}
		}
	} else {
		keep = rows
	}
	if err := s.ensurePartitions(ctx, keep); err != nil {
		return false, err
	}
	last := rows[len(rows)-1].ID
	err = s.tx(ctx, func(tx pgx.Tx, sid int32) error {
		// Under the transaction: another round of this source may have
		// shipped meanwhile (it cannot: one shipper per file, but a retry
		// after an unknown commit outcome can).
		cur, err := central.Cursor(ctx, tx, s.source, streamFacts)
		if err != nil {
			return err
		}
		var batch []sqlitelog.ExportRow
		for _, r := range keep {
			if r.ID > cur {
				batch = append(batch, r)
			}
		}
		if len(batch) > 0 {
			if err := s.insertFacts(ctx, tx, sid, batch); err != nil {
				return err
			}
		}
		return central.SetCursor(ctx, tx, sid, streamFacts, max(last, cur))
	})
	if err != nil {
		return false, err
	}
	s.shipped.Store(last)
	s.metrics.rows.WithLabelValues(streamFacts).Add(float64(len(keep)))
	s.metrics.lag.WithLabelValues(streamFacts).Set(s.Now().Sub(time.UnixMicro(rows[len(rows)-1].TS)).Seconds())
	return len(rows) == s.Batch, nil
}

var factColumns = []string{"ts", "latency_us", "node", "source", "source_id", "proto", "client", "domain", "qtype", "qclass",
	"status", "rcode", "reply_type", "upstream", "zone", "list", "rule", "cname", "dnssec", "ede", "flags", "ecs", "trace_id", "answer", "policy"}

func (s *Shipper) insertFacts(ctx context.Context, tx pgx.Tx, sid int32, rows []sqlitelog.ExportRow) error {
	ids, err := s.dicts.resolveFacts(ctx, tx, rows, s.Now().Unix())
	if err != nil {
		return err
	}
	data := make([][]any, len(rows))
	for i, r := range rows {
		data[i] = []any{
			r.TS, r.LatencyUS, ids.node[r.Node], sid, r.ID, r.Proto, ids.client[clientKey(r.ClientIP, r.ClientName)], ids.domain[r.Domain],
			r.QType, r.QClass, r.Status, r.Rcode, r.ReplyType,
			optID(ids.upstream, r.Upstream), optID(ids.zone, r.Zone), optID(ids.list, r.List), r.Rule, optID(ids.domain, r.CNAME),
			r.DNSSEC, r.EDE, r.Flags, r.ECS, r.TraceID, r.Answer, optID(ids.policy, r.Policy),
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"query"}, factColumns, pgx.CopyFromRows(data)); err != nil {
		return fmt.Errorf("pgquerylog: insert %d rows: %w", len(rows), err)
	}
	return nil
}

func optID(m map[string]int64, name *string) any {
	if name == nil {
		return nil
	}
	return m[*name]
}

// shipRollups ships the minute rollups (re-sending the last ten minutes)
// and the hour and day rollups the node has completed since the last
// round, in chunks of one transaction each.
func (s *Shipper) shipRollups(ctx context.Context, pool *pgxpool.Pool) error {
	now := s.Now().Unix()
	hourDone, dayDone := s.Local.RollupProgress()
	type plan struct {
		stream     string
		res, chunk int64
		from       func(cur int64) int64
		until      int64
	}
	plans := []plan{
		{streamMinute, 60, 86400, func(cur int64) int64 {
			if cur == 0 {
				return now - int64(sqlitelog.MinuteRetention/time.Second)
			}
			return cur - minuteReship
		}, now + 60},
		{streamHour, 3600, 7 * 86400, func(cur int64) int64 {
			return max(cur, now-int64(sqlitelog.HourRetention/time.Second))
		}, hourDone},
		{streamDay, 86400, 365 * 86400, func(cur int64) int64 { return max(cur, now-20*365*86400) }, dayDone},
	}
	for _, p := range plans {
		cur, err := central.Cursor(ctx, pool, s.source, p.stream)
		if err != nil {
			return err
		}
		from := max(p.from(cur), 0)
		if p.stream != streamMinute && from >= p.until {
			continue
		}
		for start := from; start < p.until; start += p.chunk {
			end := min(start+p.chunk, p.until)
			n, last, err := s.shipRollupRange(ctx, p.res, p.stream, start, end, cur)
			if err != nil {
				return err
			}
			cur = last
			s.metrics.rows.WithLabelValues(p.stream).Add(float64(n))
		}
	}
	return nil
}

// shipRollupRange copies the rows of every table of resolution res in
// [from, until) and moves the stream's cursor: for minutes to the newest
// minute shipped, for hours and days to until (the node's progress).
func (s *Shipper) shipRollupRange(ctx context.Context, res int64, stream string, from, until, cur int64) (int, int64, error) {
	var tables []sqlitelog.RollupTable
	for _, t := range sqlitelog.RollupTables {
		if t.Res == res {
			tables = append(tables, t)
		}
	}
	exported := make([][]sqlitelog.RollupRow, len(tables))
	newest := cur
	n := 0
	for i, t := range tables {
		rows, err := s.Local.ExportRollup(ctx, t, from, until)
		if err != nil {
			return 0, cur, err
		}
		exported[i] = rows
		n += len(rows)
		for _, r := range rows {
			newest = max(newest, r.TS)
		}
	}
	next := until
	if stream == streamMinute {
		next = newest
	}
	err := s.tx(ctx, func(tx pgx.Tx, sid int32) error {
		for i, t := range tables {
			if len(exported[i]) == 0 {
				continue
			}
			if err := s.upsertRollup(ctx, tx, t, exported[i]); err != nil {
				return err
			}
		}
		return central.SetCursor(ctx, tx, sid, stream, next)
	})
	if err != nil {
		return 0, cur, err
	}
	return n, next, nil
}

// upsertRollup replaces the node's rows of t: the node's local row is the
// whole truth for its (ts, node, key), so a resend is idempotent.
func (s *Shipper) upsertRollup(ctx context.Context, tx pgx.Tx, t sqlitelog.RollupTable, rows []sqlitelog.RollupRow) error {
	ids, err := s.dicts.resolveRollup(ctx, tx, t.Key, rows, s.Now().Unix())
	if err != nil {
		return err
	}
	cols := []string{"ts", "node"}
	if t.Key != "" {
		cols = append(cols, t.Key)
	}
	cols = append(cols, t.Cols...)
	tmp := "tmp_" + t.Name
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+tmp+` (LIKE `+t.Name+`) ON COMMIT DELETE ROWS`); err != nil {
		return fmt.Errorf("pgquerylog: %s: %w", t.Name, err)
	}
	data := make([][]any, 0, len(rows))
	for _, r := range rows {
		row := []any{r.TS, ids.node[r.Node]}
		switch t.Key {
		case "":
		case "qtype":
			q, err := strconv.ParseInt(r.Key, 10, 32)
			if err != nil {
				return fmt.Errorf("pgquerylog: %s: qtype %q", t.Name, r.Key)
			}
			row = append(row, q)
		case "client":
			row = append(row, ids.client[clientKey(r.Key, r.ClientName)])
		default:
			row = append(row, ids.byKey[r.Key])
		}
		for _, v := range r.Values {
			row = append(row, v)
		}
		data = append(data, row)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{tmp}, cols, pgx.CopyFromRows(data)); err != nil {
		return fmt.Errorf("pgquerylog: %s: %w", t.Name, err)
	}
	pk := "ts, node"
	if t.Key != "" {
		pk += ", " + t.Key
	}
	set := ""
	for i, c := range t.Cols {
		if i > 0 {
			set += ", "
		}
		set += c + " = excluded." + c
	}
	colList := ""
	for i, c := range cols {
		if i > 0 {
			colList += ", "
		}
		colList += c
	}
	//nolint:gosec // identifiers are package constants
	if _, err := tx.Exec(ctx, `INSERT INTO `+t.Name+` (`+colList+`) SELECT `+colList+` FROM `+tmp+
		` ON CONFLICT (`+pk+`) DO UPDATE SET `+set); err != nil {
		return fmt.Errorf("pgquerylog: upsert %s: %w", t.Name, err)
	}
	return nil
}

// ensurePartitions creates the daily partitions the rows fall in.
func (s *Shipper) ensurePartitions(ctx context.Context, rows []sqlitelog.ExportRow) error {
	days := map[int64]bool{}
	s.partsMu.Lock()
	for _, r := range rows {
		d := floorDay(r.TS)
		if !s.parts[d] {
			days[d] = true
		}
	}
	s.partsMu.Unlock()
	for d := range days {
		if err := EnsurePartition(ctx, s.DB, d); err != nil {
			return err
		}
		s.partsMu.Lock()
		s.parts[d] = true
		s.partsMu.Unlock()
	}
	return nil
}

const dayUS = 86400 * 1_000_000

func floorDay(tsUS int64) int64 { return tsUS - ((tsUS%dayUS)+dayUS)%dayUS }

// PartitionName is the partition of the UTC day starting at dayUS.
func PartitionName(dayStartUS int64) string {
	return "query_" + time.UnixMicro(dayStartUS).UTC().Format("20060102")
}

// partitionLock serialises partition DDL across nodes.
const partitionLock = 0x6b75647571

// EnsurePartition creates the partition of the UTC day starting at
// dayStartUS (unix µs) unless it exists.
func EnsurePartition(ctx context.Context, db *central.DB, dayStartUS int64) error {
	return db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(partitionLock)); err != nil {
			return err
		}
		//nolint:gosec // the name is built from a date; the bounds are integers
		_, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF query FOR VALUES FROM (%d) TO (%d)`,
			PartitionName(dayStartUS), dayStartUS, dayStartUS+dayUS))
		if err != nil {
			return fmt.Errorf("pgquerylog: create partition %s: %w", PartitionName(dayStartUS), err)
		}
		return nil
	})
}

type metrics struct {
	rows *prometheus.CounterVec
	lag  *prometheus.GaugeVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		rows: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kudurru_central_ship_rows_total",
			Help: "Rows shipped to the central database, by stream."}, []string{"stream"}),
		lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kudurru_central_ship_lag_seconds",
			Help: "Age of the newest row shipped to the central database, by stream."}, []string{"stream"}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.rows, m.lag} {
			if err := reg.Register(c); err != nil {
				var are prometheus.AlreadyRegisteredError
				if errors.As(err, &are) {
					switch prev := are.ExistingCollector.(type) {
					case *prometheus.CounterVec:
						m.rows = prev
					case *prometheus.GaugeVec:
						m.lag = prev
					}
				}
			}
		}
	}
	return m
}
