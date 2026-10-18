// Package sqlitelog is the built-in query-log sink: the per-node SQLite
// fact table with dictionary encoding, minute/hour/day rollups, retention,
// and the read API the dashboard and the stats endpoints use
// (dev-docs/design-record.md §10, §11.2, D59).
//
// Data flow: querylog.Async (the collector) applies privacy, stamps ids,
// publishes to the live tail and hands batches to Writer.Write, which
// inserts the facts in one BEGIN IMMEDIATE…COMMIT and folds the batch into
// in-memory minute accumulators. Writer.Run (an always-on runnable) flushes
// the accumulators at every minute boundary, derives the _1h and _1d
// tables, runs retention, checkpoints and refreshes the gauges. Reader
// answers the §10 queries from the rollups plus the not-yet-flushed
// accumulators, so the dashboard is current to the last batch.
package sqlitelog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("sqlitelog: writer closed")

// Retention and scheduling constants (dev-docs/design-record.md §11.2, D59).
const (
	// MinuteRetention keeps the _1m rollups 35 days.
	MinuteRetention = 35 * 24 * time.Hour
	// HourRetention keeps the _1h rollups 400 days; _1d is kept forever.
	HourRetention = 400 * 24 * time.Hour
	// DeleteBatch is the retention delete batch size.
	DeleteBatch = 10000

	flushDelay         = 2 * time.Second  // minute M is flushed at M+60s+flushDelay
	hourGrace          = 90 * time.Second // hour H is derived at H+1h+hourGrace
	checkpointInterval = 5 * time.Minute
	analyzeInterval    = 7 * 24 * time.Hour
	sizeCheckInterval  = time.Minute
	gaugeInterval      = 10 * time.Second
	dictSlack          = 2 * time.Hour // last_seen lags the truth by at most touchInterval
	maxRuleBytes       = 256
	maxAnswerBytes     = 256
)

// Options configures a Writer.
type Options struct {
	// Path is the database file (normally <dataDir>/querylog.db).
	Path string
	// Node is this node's name (dict_node, meta node_id, the API scope).
	Node string
	// Privacy decides whether fact rows and per-client/per-domain rollups
	// are stored. The collector applies the transform itself; the writer
	// only needs the mode.
	Privacy querylog.Mode
	// RawRetention bounds the fact table by age; 0 means size-only.
	RawRetention time.Duration
	// MaxDBBytes bounds the database size; 0 disables the cap.
	MaxDBBytes int64
	// Metrics are shared with the collector; nil creates an unregistered set.
	Metrics *querylog.Metrics
	// Tail backs Reader.Subscribe and the memory source of Queries.
	Tail *querylog.Tail
	// DomainsOnLists reports the number of blocked domains for the summary
	// tile (from the blocking engine); nil reports 0.
	DomainsOnLists func() int64
	// Clock drives scheduling; nil means the wall clock.
	Clock clock.WithTicker
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// ReadConns is the reader pool size (default 4).
	ReadConns int
	// DictCacheSize is the domain id LRU size (default 65 536; the other
	// dictionaries scale from it).
	DictCacheSize int
	// MaintenanceInterval is Run's tick (default 1 s).
	MaintenanceInterval time.Duration
}

// OptionsFrom maps the querylog settings onto Options.
func OptionsFrom(c config.Querylog, dataDir, node string) (Options, error) {
	mode, err := querylog.ParseMode(c.Privacy)
	if err != nil {
		return Options{}, err
	}
	return Options{
		Path:         filepath.Join(dataDir, FileName),
		Node:         node,
		Privacy:      mode,
		RawRetention: c.Retention.Raw.Std(),
		MaxDBBytes:   int64(c.Retention.MaxDBBytes),
	}, nil
}

// Stats are cumulative writer counters.
type Stats struct {
	Batches uint64 `json:"batches"`
	Rows    uint64 `json:"rows"`
	Flushes uint64 `json:"rollup_flushes"`
}

// Writer is the SQLite sink. It implements querylog.Sink.
type Writer struct {
	opts    Options
	metrics *querylog.Metrics
	clock   clock.WithTicker
	log     *slog.Logger

	w *sql.DB // single writer connection
	r *sql.DB // query_only reader pool

	// mu serialises the writer connection, the dictionaries and the
	// accumulators.
	mu       sync.Mutex
	closed   bool
	nodeID   int64
	dicts    *dictionaries
	ins      *sql.Stmt
	upBase   *sql.Stmt
	upClient *sql.Stmt
	upQType  *sql.Stmt
	upUp     *sql.Stmt
	acc      *accSet
	hourDone int64 // _1h derived for hours < hourDone (unix s)
	dayDone  int64 // _1d derived for days < dayDone
	firstTS  int64
	maxID    int64
	// shippedThrough, when set, bounds age-based retention to rows a
	// shipper delivered (SetShippedThrough).
	shippedThrough func() int64

	deleteBatch int // DeleteBatch; smaller in tests

	// live holds the settings SetPrivacy and SetRetention change at
	// runtime (settings hot reload); opts keeps the values at Open.
	live atomic.Pointer[liveSettings]

	flushSeq atomic.Uint64 // bumped after every committed rollup flush
	rows     atomic.Int64
	readers  atomic.Int64 // reads in flight (checkpoint gate)
	stats    struct{ batches, rows, flushes atomic.Uint64 }

	// maintenance state, used only by the Run goroutine
	nextRetention  time.Time
	lastCheckpoint time.Time
	lastSizeCheck  time.Time
	lastGauges     time.Time
	lastAnalyze    time.Time
}

var _ querylog.Sink = (*Writer)(nil)

// New returns an unopened Writer; Open (the Sink method) opens it.
func New(opts Options) *Writer {
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = querylog.NewMetrics(nil)
	}
	if opts.ReadConns <= 0 {
		opts.ReadConns = 4
	}
	if opts.DictCacheSize <= 0 {
		opts.DictCacheSize = 65536
	}
	if opts.MaintenanceInterval <= 0 {
		opts.MaintenanceInterval = time.Second
	}
	w := &Writer{opts: opts, metrics: opts.Metrics, clock: opts.Clock, log: opts.Logger, acc: newAccSet(), deleteBatch: DeleteBatch}
	w.live.Store(&liveSettings{privacy: opts.Privacy, raw: opts.RawRetention, maxDBBytes: opts.MaxDBBytes})
	return w
}

// liveSettings are the runtime-changeable writer settings.
type liveSettings struct {
	privacy    querylog.Mode
	raw        time.Duration
	maxDBBytes int64
}

// SetPrivacy changes the privacy mode for batches written from now on
// (querylog.privacy hot reload): whether fact rows, per-client and
// per-domain rollups and answer summaries are stored. Rows already written
// keep what they hold. The collector's transform is changed separately
// (querylog.Async.SetPrivacy).
func (w *Writer) SetPrivacy(mode querylog.Mode) {
	for {
		cur := w.live.Load()
		next := *cur
		next.privacy = mode
		if w.live.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// SetRetention changes querylog.retention.raw and maxDBBytes; the next
// maintenance tick (size check) and the daily retention run use them.
func (w *Writer) SetRetention(raw time.Duration, maxDBBytes int64) {
	for {
		cur := w.live.Load()
		next := *cur
		next.raw, next.maxDBBytes = raw, maxDBBytes
		if w.live.CompareAndSwap(cur, &next) {
			return
		}
	}
}

func (w *Writer) privacy() querylog.Mode      { return w.live.Load().privacy }
func (w *Writer) rawRetention() time.Duration { return w.live.Load().raw }
func (w *Writer) maxDBBytes() int64           { return w.live.Load().maxDBBytes }

// Open creates New(opts) and opens it.
func Open(ctx context.Context, opts Options) (*Writer, error) {
	w := New(opts)
	if err := w.Open(ctx, nil, nil); err != nil {
		return nil, err
	}
	return w, nil
}

// Name implements querylog.Sink.
func (w *Writer) Name() string { return "sqlite" }

// Open implements querylog.Sink: it creates or migrates the database,
// prepares statements and loads the persisted state. cfg and reg are
// unused; the Writer is configured through Options and shares the
// collector's Metrics.
func (w *Writer) Open(ctx context.Context, _ json.RawMessage, _ prometheus.Registerer) (err error) {
	if w.opts.Path == "" {
		return errors.New("sqlitelog: Options.Path is required")
	}
	if w.opts.Node == "" {
		return errors.New("sqlitelog: Options.Node is required")
	}
	if err := os.MkdirAll(filepath.Dir(w.opts.Path), 0o750); err != nil {
		return fmt.Errorf("sqlitelog: %w", err)
	}
	defer func() {
		if err != nil {
			w.closeDBs()
		}
	}()
	// Create the file 0600 before SQLite does (it would use 0644): the
	// query log is personal data.
	if f, err := os.OpenFile(w.opts.Path, os.O_RDWR|os.O_CREATE, 0o600); err == nil { // #nosec G304 -- operator-supplied data path
		_ = f.Close()
	}
	if w.w, err = sql.Open("sqlite", dsn(w.opts.Path, true)); err != nil {
		return fmt.Errorf("sqlitelog: open %s: %w", w.opts.Path, err)
	}
	w.w.SetMaxOpenConns(1)
	w.w.SetMaxIdleConns(1)
	w.w.SetConnMaxLifetime(0)
	w.w.SetConnMaxIdleTime(0)
	created, err := migrate(ctx, w.w)
	if err != nil {
		return err
	}
	if err := w.prepare(ctx); err != nil {
		return err
	}
	if err := w.loadState(ctx, created); err != nil {
		return err
	}
	if w.r, err = sql.Open("sqlite", dsn(w.opts.Path, false)); err != nil {
		return fmt.Errorf("sqlitelog: open %s: %w", w.opts.Path, err)
	}
	w.r.SetMaxOpenConns(w.opts.ReadConns)
	w.r.SetMaxIdleConns(w.opts.ReadConns)
	now := w.clock.Now()
	w.nextRetention = nextRetentionTime(now, 10+rand.IntN(41)) //nolint:gosec // scheduling jitter, not security
	w.lastCheckpoint, w.lastSizeCheck = now, time.Time{}
	w.updateGauges(ctx)
	return nil
}

func (w *Writer) prepare(ctx context.Context) error {
	var err error
	if w.dicts, err = prepareDicts(ctx, w.w, w.opts.DictCacheSize); err != nil {
		return err
	}
	if w.ins, err = w.w.PrepareContext(ctx, `INSERT INTO query(id, ts, latency_us, node, proto, client, domain, qtype, qclass, status, rcode,
		reply_type, upstream, zone, list, rule, cname, dnssec, ede, flags, ecs, trace_id, answer, policy)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`); err != nil {
		return fmt.Errorf("sqlitelog: prepare insert: %w", err)
	}
	upsert := func(table string, keys []string, cols []string) (*sql.Stmt, error) {
		all := append(append([]string{}, keys...), cols...)
		sets := make([]string, len(cols))
		for i, c := range cols {
			sets[i] = c + "=coalesce(" + c + ",0)+excluded." + c
		}
		q := `INSERT INTO ` + table + `(` + strings.Join(all, ",") + `) VALUES(` + placeholders(len(all)) +
			`) ON CONFLICT(` + strings.Join(keys, ",") + `) DO UPDATE SET ` + strings.Join(sets, ",")
		return w.w.PrepareContext(ctx, q)
	}
	if w.upBase, err = upsert("rollup_1m", []string{"ts", "node"}, baseCols); err != nil {
		return err
	}
	if w.upClient, err = upsert("rollup_client_1m", []string{"ts", "node", "client"}, []string{"total", "blocked"}); err != nil {
		return err
	}
	if w.upQType, err = upsert("rollup_qtype_1m", []string{"ts", "node", "qtype"}, []string{"total"}); err != nil {
		return err
	}
	if w.upUp, err = upsert("rollup_upstream_1m", []string{"ts", "node", "upstream"}, upCols); err != nil {
		return err
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// loadState registers the node, reads meta and seeds the pending-hour cache.
func (w *Writer) loadState(ctx context.Context, created bool) error {
	if err := w.w.QueryRowContext(ctx, `INSERT INTO dict_node(name) VALUES(?) ON CONFLICT(name) DO UPDATE SET name=excluded.name RETURNING id`,
		w.opts.Node).Scan(&w.nodeID); err != nil {
		return fmt.Errorf("sqlitelog: register node: %w", err)
	}
	meta, err := readMeta(ctx, w.w)
	if err != nil {
		return err
	}
	now := w.clock.Now().Unix()
	w.hourDone = metaInt(meta, metaHourDone, floorTo(now, 3600))
	w.dayDone = metaInt(meta, metaDayDone, floorTo(now, 86400))
	w.firstTS = metaInt(meta, metaFirstTS, 0)
	if la := metaInt(meta, metaLastAnalyze, 0); la > 0 {
		w.lastAnalyze = time.Unix(la, 0)
	} else {
		w.lastAnalyze = w.clock.Now()
	}
	set := map[string]any{metaPrivacy: w.privacy().String(), metaHourDone: w.hourDone, metaDayDone: w.dayDone}
	if created {
		set[metaNodeID] = w.opts.Node
		set[metaLastAnalyze] = w.lastAnalyze.Unix()
	}
	for k, v := range set {
		if err := writeMeta(ctx, w.w, k, v); err != nil {
			return err
		}
	}
	var maxID sql.NullInt64
	var rows int64
	if err := w.w.QueryRowContext(ctx, `SELECT max(id), count(*) FROM query`).Scan(&maxID, &rows); err != nil {
		return fmt.Errorf("sqlitelog: count rows: %w", err)
	}
	w.maxID = maxID.Int64
	w.rows.Store(rows)
	return w.seedHours(ctx)
}

// seedHours rebuilds the pending-hour cache from the facts of hours not
// yet derived (after a restart).
func (w *Writer) seedHours(ctx context.Context) error {
	from := w.hourDone * 1e6
	type q struct {
		sql string
		add func(h *hourAcc, k, a, b int64)
	}
	for _, s := range []q{
		{`SELECT (ts/3600000000)*3600, domain, count(*), sum(status BETWEEN 16 AND 23) FROM query WHERE ts >= ? GROUP BY 1, 2`,
			func(h *hourAcc, k, a, b int64) { h.addDomain(k, a, b) }},
		{`SELECT (ts/3600000000)*3600, list, sum(status BETWEEN 16 AND 23), sum(status NOT BETWEEN 16 AND 23) FROM query WHERE ts >= ? AND list IS NOT NULL GROUP BY 1, 2`,
			func(h *hourAcc, k, a, b int64) { addPair(h.lists, k, a, b) }},
		{`SELECT (ts/3600000000)*3600, zone, count(*), 0 FROM query WHERE ts >= ? AND zone IS NOT NULL GROUP BY 1, 2`,
			func(h *hourAcc, k, a, _ int64) { h.zones[k] += a }},
	} {
		rows, err := w.w.QueryContext(ctx, s.sql, from)
		if err != nil {
			return fmt.Errorf("sqlitelog: seed pending hours: %w", err)
		}
		for rows.Next() {
			var hour, k, a, b int64
			if err := rows.Scan(&hour, &k, &a, &b); err != nil {
				_ = rows.Close()
				return err
			}
			s.add(w.acc.hour(hour), k, a, b)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

// NextID returns the id the collector should stamp on the next event
// (querylog.AsyncOptions.FirstID).
func (w *Writer) NextID() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return uint64(w.maxID) + 1 //nolint:gosec // rowids are positive
}

// Stats returns cumulative counters.
func (w *Writer) Stats() Stats {
	return Stats{Batches: w.stats.batches.Load(), Rows: w.stats.rows.Load(), Flushes: w.stats.flushes.Load()}
}

// Reader returns the read API over this database.
func (w *Writer) Reader() *Reader { return &Reader{w: w} }

// Write implements querylog.Sink: one transaction per batch.
func (w *Writer) Write(batch []*querylog.Event) error {
	if len(batch) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	start := time.Now()
	ctx := context.Background()
	delta := newAccSet()
	tx, err := w.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitelog: begin: %w", err)
	}
	mode := w.privacy()
	facts := mode.StoresFacts()
	ins := tx.StmtContext(ctx, w.ins)
	maxID, first := w.maxID, w.firstTS
	for _, ev := range batch {
		id, err := w.add(ctx, tx, ins, ev, delta, facts)
		if err != nil {
			_ = tx.Rollback()
			w.dicts.purge()
			return err
		}
		maxID = max(maxID, id)
		if first == 0 {
			first = ev.Time.UnixMicro()
			if err := writeMeta(ctx, tx, metaFirstTS, first); err != nil {
				_ = tx.Rollback()
				w.dicts.purge()
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		w.dicts.purge()
		return fmt.Errorf("sqlitelog: commit: %w", err)
	}
	w.maxID, w.firstTS = maxID, first
	w.acc.merge(delta)
	if facts {
		w.rows.Add(int64(len(batch)))
	}
	w.stats.batches.Add(1)
	w.stats.rows.Add(uint64(len(batch)))
	w.metrics.BatchRows.Observe(float64(len(batch)))
	w.metrics.WriteDuration.Observe(time.Since(start).Seconds())
	return nil
}

// add inserts one event (when facts are stored) and accumulates it into
// delta. It returns the row id (0 when no row was written).
func (w *Writer) add(ctx context.Context, tx *sql.Tx, ins *sql.Stmt, ev *querylog.Event, delta *accSet, facts bool) (int64, error) {
	sec := ev.Time.Unix()
	minute, hour := floorTo(sec, 60), floorTo(sec, 3600)
	var upID int64
	if up := upstreamName(ev); up != "" {
		var err error
		if upID, err = w.dicts.upstream.nameID(ctx, tx, up); err != nil {
			return 0, err
		}
	}
	m := delta.minute(minute)
	addBase(&m.base, ev)
	m.qtypes[ev.QType]++
	if upID != 0 {
		u := m.upstreams[upID]
		if u == nil {
			u = new([nUp]int64)
			m.upstreams[upID] = u
		}
		addUpstream(u, ev)
	}
	if hour < w.hourDone {
		delta.dirty[hour] = struct{}{}
	}
	if !facts {
		return 0, nil
	}
	ip := ""
	if ev.Client.IsValid() {
		ip = ev.Client.String()
	}
	clientID, err := w.dicts.clientID(ctx, tx, ip, ev.ClientName, ev.ClientGroup, sec)
	if err != nil {
		return 0, err
	}
	domainID, err := w.dicts.domainID(ctx, tx, storedName(ev.Name), sec)
	if err != nil {
		return 0, err
	}
	var zoneID, listID, cnameID, policyID int64
	if ev.Zone != "" {
		if zoneID, err = w.dicts.zone.nameID(ctx, tx, storedName(ev.Zone)); err != nil {
			return 0, err
		}
	}
	if ev.List != "" {
		if listID, err = w.dicts.list.nameID(ctx, tx, ev.List); err != nil {
			return 0, err
		}
	}
	if ev.CNAME != "" {
		if cnameID, err = w.dicts.domainID(ctx, tx, storedName(ev.CNAME), sec); err != nil {
			return 0, err
		}
	}
	if ev.Policy != "" {
		if policyID, err = w.dicts.policy.nameID(ctx, tx, ev.Policy); err != nil {
			return 0, err
		}
	}
	blocked := isBlocked(ev.Status)
	var latency, ede, rule, ecs, trace, answer, id any
	if answered(ev) {
		latency = ev.Latency.Microseconds()
	}
	if ev.HasEDE {
		ede = int64(ev.EDE)
	}
	if ev.Rule != "" {
		rule = truncate(ev.Rule, maxRuleBytes)
	}
	if ev.ECS.IsValid() {
		ecs = ev.ECS.String()
	}
	if ev.Traced() {
		trace = ev.TraceID[:]
	}
	if ev.Answer != "" && w.privacy() == querylog.PrivacyFull {
		answer = truncate(ev.Answer, maxAnswerBytes)
	}
	if ev.ID != 0 {
		id = int64(ev.ID) //nolint:gosec // ids fit in int64
	}
	res, err := ins.ExecContext(ctx, id, ev.Time.UnixMicro(), latency, w.nodeID, protoCode(ev.Proto), clientID, domainID,
		int64(ev.QType), int64(qclass(ev.QClass)), int64(ev.Status), int64(ev.Rcode), int64(ev.ReplyType),
		nullID(upID), nullID(zoneID), nullID(listID), rule, nullID(cnameID), int64(ev.DNSSEC), ede,
		int64(ev.Flags.Bits()), ecs, trace, answer, nullID(policyID))
	if err != nil {
		return 0, fmt.Errorf("sqlitelog: insert: %w", err)
	}
	rowID, _ := res.LastInsertId()

	var b int64
	if blocked {
		b = 1
	}
	addPair(m.clients, clientID, 1, b)
	if hour >= w.hourDone {
		h := delta.hour(hour)
		h.addDomain(domainID, 1, b)
		if listID != 0 {
			addPair(h.lists, listID, b, 1-b)
		}
		if zoneID != 0 {
			h.zones[zoneID]++
		}
	}
	return rowID, nil
}

// Flush implements querylog.Sink: every pending minute accumulator is
// written (additively) to the _1m rollups.
func (w *Writer) Flush(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return w.flushMinutes(ctx, math.MaxInt64)
}

// flushMinutes writes accumulated minutes that end at or before cutoff
// (unix s). Caller holds mu.
func (w *Writer) flushMinutes(ctx context.Context, cutoff int64) error {
	var due []int64
	for ts := range w.acc.minutes {
		if ts+60 <= cutoff || cutoff == math.MaxInt64 {
			due = append(due, ts)
		}
	}
	if len(due) == 0 {
		return nil
	}
	tx, err := w.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	base, cl, qt, up := tx.StmtContext(ctx, w.upBase), tx.StmtContext(ctx, w.upClient), tx.StmtContext(ctx, w.upQType), tx.StmtContext(ctx, w.upUp)
	for _, ts := range due {
		m := w.acc.minutes[ts]
		args := []any{ts, w.nodeID}
		for _, v := range m.base {
			args = append(args, v)
		}
		if _, err := base.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("sqlitelog: flush rollup_1m: %w", err)
		}
		for c, v := range m.clients {
			if _, err := cl.ExecContext(ctx, ts, w.nodeID, c, v[0], v[1]); err != nil {
				return fmt.Errorf("sqlitelog: flush rollup_client_1m: %w", err)
			}
		}
		for q, v := range m.qtypes {
			if _, err := qt.ExecContext(ctx, ts, w.nodeID, int64(q), v); err != nil {
				return fmt.Errorf("sqlitelog: flush rollup_qtype_1m: %w", err)
			}
		}
		for u, v := range m.upstreams {
			args := []any{ts, w.nodeID, u}
			for _, x := range v {
				args = append(args, x)
			}
			if _, err := up.ExecContext(ctx, args...); err != nil {
				return fmt.Errorf("sqlitelog: flush rollup_upstream_1m: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, ts := range due {
		delete(w.acc.minutes, ts)
	}
	w.flushSeq.Add(1)
	w.stats.flushes.Add(1)
	return nil
}

// Close implements querylog.Sink: flushes the accumulators, writes back
// dictionary last_seen values, runs PRAGMA optimize and checkpoints the WAL.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.w == nil {
		return nil
	}
	ctx := context.Background()
	var errs []error
	errs = append(errs, w.flushMinutes(ctx, math.MaxInt64))
	if tx, err := w.w.BeginTx(ctx, nil); err == nil {
		if err := w.dicts.flushSeen(ctx, tx); err != nil {
			_ = tx.Rollback()
			errs = append(errs, err)
		} else {
			errs = append(errs, tx.Commit())
		}
	}
	if _, err := w.w.ExecContext(ctx, `PRAGMA analysis_limit=1000; PRAGMA optimize`); err != nil {
		w.log.Warn("querylog.db optimize on close failed", "err", err)
	}
	if _, err := w.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		w.log.Warn("querylog.db checkpoint on close failed", "err", err)
	}
	w.closeDBs()
	return errors.Join(errs...)
}

func (w *Writer) closeDBs() {
	if w.dicts != nil {
		w.dicts.close()
	}
	for _, s := range []*sql.Stmt{w.ins, w.upBase, w.upClient, w.upQType, w.upUp} {
		if s != nil {
			_ = s.Close()
		}
	}
	if w.r != nil {
		_ = w.r.Close()
	}
	if w.w != nil {
		_ = w.w.Close()
	}
}

// Run is the always-on maintenance runnable: minute flushes, hourly and
// daily derivation, retention, checkpoints, ANALYZE and gauges. It returns
// nil when ctx ends; Close (through the collector) does the final flush.
func (w *Writer) Run(ctx context.Context) error {
	t := w.clock.NewTicker(w.opts.MaintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
			if err := w.Maintain(ctx); err != nil && !errors.Is(err, ErrClosed) && ctx.Err() == nil {
				w.log.Error("query log maintenance failed", "err", err)
			}
		}
	}
}

// Maintain runs one maintenance step at the clock's current time. Run
// calls it every MaintenanceInterval; tests call it directly.
func (w *Writer) Maintain(ctx context.Context) error {
	now := w.clock.Now()
	if err := w.rollup(ctx, now); err != nil {
		return err
	}
	if !now.Before(w.nextRetention) {
		if _, err := w.Retain(ctx); err != nil {
			return err
		}
		w.nextRetention = nextRetentionTime(now.Add(time.Minute), w.nextRetention.Minute())
	}
	if w.maxDBBytes() > 0 && now.Sub(w.lastSizeCheck) >= sizeCheckInterval {
		w.lastSizeCheck = now
		if _, err := w.enforceSize(ctx); err != nil {
			return err
		}
	}
	if now.Sub(w.lastCheckpoint) >= checkpointInterval {
		if ok, err := w.Checkpoint(ctx); err != nil {
			return err
		} else if ok {
			w.lastCheckpoint = now
		}
	}
	if now.Sub(w.lastAnalyze) >= analyzeInterval {
		if err := w.analyze(ctx, now); err != nil {
			return err
		}
	}
	if now.Sub(w.lastGauges) >= gaugeInterval {
		w.lastGauges = now
		w.updateGauges(ctx)
	}
	return nil
}

// readableProgress returns the hour and day before which the _1h and _1d
// rollups hold every event: hourDone and dayDone, held back to the first
// dirty hour. A late event for a derived hour (or, on a new database, for
// the hour before it was opened) leaves that hour's _1h and _1d rows stale
// until the next derive, so reads take it from the finer tiers meanwhile.
// Caller holds mu.
func (w *Writer) readableProgress() (hourDone, dayDone int64) {
	hourDone, dayDone = w.hourDone, w.dayDone
	for h := range w.acc.dirty {
		hourDone = min(hourDone, h)
		dayDone = min(dayDone, floorTo(h, 86400))
	}
	return hourDone, dayDone
}

// rollup flushes closed minutes and derives due or dirty hours and days.
func (w *Writer) rollup(ctx context.Context, now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if err := w.flushMinutes(ctx, now.Add(-flushDelay).Unix()); err != nil {
		return err
	}
	return w.derive(ctx, now)
}

// derive builds the _1h tables for every complete hour (and the dirty
// ones), then the _1d tables. Caller holds mu.
func (w *Writer) derive(ctx context.Context, now time.Time) error {
	ready := now.Add(-hourGrace).Unix()
	var hours []int64
	for h := range w.acc.dirty {
		if h+3600 <= ready || h < w.hourDone {
			hours = append(hours, h)
		}
	}
	for h := w.hourDone; h+3600 <= ready; h += 3600 {
		if _, dup := w.acc.dirty[h]; !dup {
			hours = append(hours, h)
		}
	}
	if len(hours) == 0 {
		return nil
	}
	// Minutes of the hours about to be derived must be on disk.
	if err := w.flushMinutes(ctx, math.MaxInt64); err != nil {
		return err
	}
	dirtyDays := map[int64]struct{}{}
	tx, err := w.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	newDone := w.hourDone
	for _, h := range hours {
		if err := deriveHour(ctx, tx, h); err != nil {
			return err
		}
		if h >= newDone {
			newDone = h + 3600
		}
		if d := floorTo(h, 86400); d < w.dayDone {
			dirtyDays[d] = struct{}{}
		}
	}
	newDay := w.dayDone
	for d := w.dayDone; d+86400 <= newDone; d += 86400 {
		dirtyDays[d] = struct{}{}
		newDay = d + 86400
	}
	for d := range dirtyDays {
		if err := deriveDay(ctx, tx, d); err != nil {
			return err
		}
	}
	if err := writeMeta(ctx, tx, metaHourDone, newDone); err != nil {
		return err
	}
	if err := writeMeta(ctx, tx, metaDayDone, newDay); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, h := range hours {
		delete(w.acc.dirty, h)
		if h < newDone {
			delete(w.acc.hours, h)
		}
	}
	w.hourDone, w.dayDone = newDone, newDay
	return nil
}

func sumCols(cols []string) string {
	s := make([]string, len(cols))
	for i, c := range cols {
		s[i] = "sum(" + c + ")"
	}
	return strings.Join(s, ",")
}

// deriveHour recomputes every _1h table for the hour starting at h (unix s):
// the additive tables from the _1m rollups, domain/list/zone from the facts.
func deriveHour(ctx context.Context, tx *sql.Tx, h int64) error {
	end := h + 3600
	us, endUS := h*1e6, end*1e6
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO rollup_1h(ts,node,` + strings.Join(baseCols, ",") + `) SELECT ?1,node,` + sumCols(baseCols) +
			` FROM rollup_1m WHERE ts>=?1 AND ts<?2 GROUP BY node`, []any{h, end}},
		{`INSERT INTO rollup_client_1h(ts,node,client,total,blocked) SELECT ?1,node,client,sum(total),sum(blocked) FROM rollup_client_1m WHERE ts>=?1 AND ts<?2 GROUP BY node,client`, []any{h, end}},
		{`INSERT INTO rollup_qtype_1h(ts,node,qtype,total) SELECT ?1,node,qtype,sum(total) FROM rollup_qtype_1m WHERE ts>=?1 AND ts<?2 GROUP BY node,qtype`, []any{h, end}},
		{`INSERT INTO rollup_upstream_1h(ts,node,upstream,` + strings.Join(upCols, ",") + `) SELECT ?1,node,upstream,` + sumCols(upCols) +
			` FROM rollup_upstream_1m WHERE ts>=?1 AND ts<?2 GROUP BY node,upstream`, []any{h, end}},
		{`INSERT INTO rollup_domain_1h(ts,node,domain,total,blocked) SELECT ?1,node,domain,count(*),sum(status BETWEEN 16 AND 23) FROM query WHERE ts>=?2 AND ts<?3 GROUP BY node,domain`, []any{h, us, endUS}},
		{`INSERT INTO rollup_list_1h(ts,node,list,hits) SELECT ?1,node,list,count(*) FROM query WHERE ts>=?2 AND ts<?3 AND list IS NOT NULL AND status BETWEEN 16 AND 23 GROUP BY node,list`, []any{h, us, endUS}},
		{`INSERT INTO rollup_zone_1h(ts,node,zone,total) SELECT ?1,node,zone,count(*) FROM query WHERE ts>=?2 AND ts<?3 AND zone IS NOT NULL GROUP BY node,zone`, []any{h, us, endUS}},
	}
	for _, t := range hourTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE ts=?`, h); err != nil { //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
			return fmt.Errorf("sqlitelog: derive %s: %w", t, err)
		}
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
			return fmt.Errorf("sqlitelog: derive hour %d: %w", h, err)
		}
	}
	return nil
}

// deriveDay recomputes the _1d tables for the UTC day starting at d.
func deriveDay(ctx context.Context, tx *sql.Tx, d int64) error {
	end := d + 86400
	for _, t := range []string{"rollup_1d", "rollup_client_1d", "rollup_domain_1d"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE ts=?`, d); err != nil { //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
			return fmt.Errorf("sqlitelog: derive %s: %w", t, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO rollup_1d(ts,node,` + strings.Join(baseCols, ",") + `) SELECT ?1,node,` + sumCols(baseCols) + ` FROM rollup_1h WHERE ts>=?1 AND ts<?2 GROUP BY node`,
		`INSERT INTO rollup_client_1d(ts,node,client,total,blocked) SELECT ?1,node,client,sum(total),sum(blocked) FROM rollup_client_1h WHERE ts>=?1 AND ts<?2 GROUP BY node,client`,
		`INSERT INTO rollup_domain_1d(ts,node,domain,total,blocked) SELECT ?1,node,domain,sum(total),sum(blocked) FROM rollup_domain_1h WHERE ts>=?1 AND ts<?2 GROUP BY node,domain`,
	} {
		if _, err := tx.ExecContext(ctx, q, d, end); err != nil {
			return fmt.Errorf("sqlitelog: derive day %d: %w", d, err)
		}
	}
	return nil
}

// Helpers.

func floorTo(v, step int64) int64 {
	r := v % step
	if r < 0 {
		r += step
	}
	return v - r
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func qclass(c uint16) uint16 {
	if c == 0 {
		return 1
	}
	return c
}

// protoCode maps the transport onto the query.proto column: 0 udp, 1 tcp,
// 2 dot, 3 doh, 4 doq, 5 dnscrypt (reserved), 6 doh3.
func protoCode(p pipeline.Proto) int64 {
	if p == pipeline.ProtoDoH3 {
		return 6
	}
	return int64(p)
}

// protoName is the inverse of protoCode for the API.
func protoName(code int64) string {
	switch code {
	case 5:
		return "dnscrypt"
	case 6:
		return "doh3"
	}
	return pipeline.Proto(code).String() //nolint:gosec // 0-4
}

// storedName is the canonical form kept in dict_domain: lower case, no
// trailing dot, "." for the root.
func storedName(name string) string {
	if name == "." || name == "" {
		return "."
	}
	return strings.TrimSuffix(strings.ToLower(name), ".")
}

// upstreamName is the dict_upstream key of an event: the upstream the
// pipeline reported, "cache" for cache hits, "zone:<zone>" for local
// answers.
func upstreamName(ev *querylog.Event) string {
	if ev.Upstream != "" {
		return ev.Upstream
	}
	switch ev.Status {
	case pipeline.StatusCached, pipeline.StatusCachedStale:
		return "cache"
	case pipeline.StatusAuthoritative, pipeline.StatusLocalRecord:
		if ev.Zone != "" {
			return "zone:" + storedName(ev.Zone)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Do not split a UTF-8 sequence.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func writeMeta(ctx context.Context, db execer, key string, value any) error {
	if _, err := db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
		return fmt.Errorf("sqlitelog: write meta %s: %w", key, err)
	}
	return nil
}

func readMeta(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, CAST(value AS TEXT) FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("sqlitelog: read meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	m := map[string]string{}
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v.String
	}
	return m, rows.Err()
}

func metaInt(m map[string]string, key string, def int64) int64 {
	if v, err := strconv.ParseInt(m[key], 10, 64); err == nil {
		return v
	}
	return def
}

// nextRetentionTime is the next 03:<minute> local time after now.
func nextRetentionTime(now time.Time, minute int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 3, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}
