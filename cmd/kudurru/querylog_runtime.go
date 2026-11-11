package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/httpbatch"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// querylogRuntime owns one queue/tail and at most one local database for the
// node lifetime. Immutable captured routes borrow these owners through final
// DNS emission and queue drain; a reload never closes an old writer underneath
// them. Local history remains readable when admission is disabled or uses slog.
// The additional sinks of querylog.sinks run on the sink host (D371, D372): every
// route holds one reference on each of its sinks, and a sink stops when the
// last route that named it is released.
type querylogRuntime struct {
	async       *querylog.Async
	tail        *querylog.Tail
	metrics     *querylog.Metrics
	slog        *querylog.SlogSink
	dir, node   string
	logger      *slog.Logger
	domains     func() int64
	mu          sync.Mutex // preparation only; DNS never acquires it
	writer      atomic.Pointer[sqlitelog.Writer]
	firstID     atomic.Uint64
	current     atomic.Pointer[querylog.Captured]
	settings    atomic.Pointer[config.Querylog]
	available   chan struct{}
	unavailable atomic.Bool
	sinks       *querylog.SinkHost
	// rawAudit records the start of every raw dnstap sink (D372); serve
	// binds the node's audit recorders to it once they exist.
	rawAudit *rawExportAudit
	// owned is the route whose sink references this runtime itself holds:
	// the startup route and every route of a legacy reload. Complete
	// publications own theirs through the bundle's release hook.
	ownMu sync.Mutex
	owned *querylog.Captured
}

// querylogSinkInputs are the node's own inputs to the sink types.
type querylogSinkInputs struct {
	Logger *slog.Logger
	// Credentials reads the credential references of an entry with what
	// this node's operator delegated (querylogSinkCredentials).
	Credentials httpbatch.Resolver
	// Node is the dnstap identity of a sink without one.
	Node string
	// AuditRaw records the start of a raw dnstap sink; nil refuses them.
	AuditRaw func(context.Context, dnstap.RawStart) error
}

// querylogSinkFactories are the types of querylog.sinks (D371): log lines,
// and the HTTP batch sinks for ClickHouse and Loki. A new type adds its
// factory here and its option and settings to the schema (internal/config);
// TestQuerylogSinkTypesHaveFactories keeps them in step. A factory only
// builds the sink: the host opens it from the sink's own goroutine when the
// first event arrives.
var querylogSinkFactories = func(in querylogSinkInputs) map[string]querylog.SinkFactory {
	factories := sinks.Factories(sinks.Options{Resolve: in.Credentials, Logger: in.Logger, UserAgent: "kudurru/" + version.Get().Version, Node: in.Node, AuditRaw: in.AuditRaw})
	factories[config.QuerylogSinkSlog] = func(spec querylog.SinkSpec) (querylog.Sink, error) {
		return &querylog.SlogSink{Logger: in.Logger.With("sink", spec.Name), Level: slog.LevelInfo}, nil
	}
	return factories
}

// borrowedQuerylogSink leaves the bounded shared sink owned by the runtime,
// rather than closing it when the stable collector finishes its queue.
type borrowedQuerylogSink struct{ querylog.Sink }

func (s borrowedQuerylogSink) Close() error { return nil }
func (s borrowedQuerylogSink) WriteBatch(ctx context.Context, events []*querylog.Event, mode querylog.Mode) error {
	if sink, ok := s.Sink.(querylog.BatchSink); ok {
		return sink.WriteBatch(ctx, events, mode)
	}
	return s.Write(events)
}

// newQuerylogRuntime builds the runtime of a node that reads no Secrets:
// every driver but Kubernetes, and tests.
func newQuerylogRuntime(ctx context.Context, cfg *config.Config, reg prometheus.Registerer, logger *slog.Logger, domains func() int64) (*querylogRuntime, error) {
	return newQuerylogRuntimeWithSecrets(ctx, cfg, reg, logger, domains, nil)
}

// newQuerylogRuntimeWithSecrets also lets the sinks of querylog.sinks read
// the Secrets their settings reference; secrets is nil where there are none.
func newQuerylogRuntimeWithSecrets(ctx context.Context, cfg *config.Config, reg prometheus.Registerer, logger *slog.Logger, domains func() int64, secrets *querylogSecrets) (*querylogRuntime, error) {
	credentials, err := querylogSinkCredentials(cfg, secrets)
	if err != nil {
		return nil, err
	}
	r := &querylogRuntime{dir: cfg.QuerylogDataDir(), node: cfg.Node.Name, logger: logger, domains: domains, available: make(chan struct{}), rawAudit: &rawExportAudit{node: cfg.Node.Name}}
	initial := cfg.Querylog
	r.settings.Store(&initial)
	r.metrics = querylog.NewMetrics(reg)
	r.tail = querylog.NewTail(0, 0, r.metrics)
	r.slog = &querylog.SlogSink{Logger: logger, Level: slog.LevelInfo}
	if err := r.slog.Open(ctx, nil, nil); err != nil {
		return nil, err
	}
	r.async = querylog.NewAsyncWith(querylog.AsyncOptions{Sink: borrowedQuerylogSink{r.slog}, Tail: r.tail, Metrics: r.metrics, FirstID: 1, Logger: logger})
	r.sinks = querylog.NewSinkHost(querylog.SinkHostOptions{Collector: r.async, Factories: querylogSinkFactories(querylogSinkInputs{Logger: logger, Credentials: credentials, Node: cfg.Node.Name, AuditRaw: r.rawAudit.record}), Metrics: r.metrics, Registerer: reg, Logger: logger})
	route, err := r.prepare(ctx, cfg.Querylog, nil)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = r.close(cleanup)
		return nil, err
	}
	r.adopt(route, cfg.Querylog)
	r.own(route)
	return r, nil
}

// own makes route the one whose sink references the runtime holds and
// releases the previous one. Nil hands ownership to the publications.
func (r *querylogRuntime) own(route *querylog.Captured) {
	r.ownMu.Lock()
	previous := r.owned
	r.owned = route
	r.ownMu.Unlock()
	if previous != route {
		previous.Release()
	}
}

func (r *querylogRuntime) ensureWriter(ctx context.Context) (*sqlitelog.Writer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w := r.writer.Load(); w != nil {
		return w, nil
	}
	// Preparation may be rejected. Until adoption, maintenance must retain
	// the last accepted retention/privacy rather than a candidate's values.
	opts, err := sqlitelog.OptionsFrom(*r.settings.Load(), r.dir, r.node)
	if err != nil {
		return nil, err
	}
	opts.Metrics, opts.Tail, opts.Logger, opts.DomainsOnLists = r.metrics, r.tail, r.logger, r.domains
	w, err := sqlitelog.Open(ctx, opts)
	if err != nil {
		return nil, err
	}
	r.firstID.Store(w.NextID())
	r.writer.Store(w)
	close(r.available)
	return w, nil
}

// prepare performs bounded local preparation, never operational adoption. The
// predecessor must be the logging component of the actually served bundle.
// The returned route holds one reference on each additional sink; the caller
// releases it (Captured.Release) when the route can admit nothing more, or
// at once when it is never published. Preparing starts and opens no sink.
func (r *querylogRuntime) prepare(ctx context.Context, cfg config.Querylog, previous *querylog.Captured) (_ *querylog.Captured, err error) {
	defer func() { r.unavailable.Store(err != nil) }()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Sink != "sqlite" && cfg.Sink != "slog" {
		return nil, errors.New("querylog: unsupported sink")
	}
	p, err := querylog.PrivacyFrom(cfg)
	if err != nil {
		return nil, err
	}
	specs, err := querylog.SinkSpecsFrom(cfg)
	if err != nil {
		return nil, err
	}
	var sink querylog.Sink = borrowedQuerylogSink{r.slog}
	var firstID uint64 = 1
	// Retained history, maintenance and shipping survive disabled/slog
	// restarts. Missing files stay lazy unless enabled SQLite needs one.
	// Load the existing ID floor before any later enabled route can emit.
	_, statErr := os.Stat(filepath.Join(r.dir, sqlitelog.FileName))
	existing := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if existing || cfg.Enabled && cfg.Sink == "sqlite" {
		w, openErr := r.ensureWriter(ctx)
		if openErr != nil {
			return nil, openErr
		}
		firstID = r.firstID.Load()
		if cfg.Sink == "sqlite" {
			sink = borrowedQuerylogSink{w}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Last, so that no failure above leaves a reference behind.
	set, err := r.sinks.Prepare(specs)
	if err != nil {
		return nil, err
	}
	return r.async.PrepareCapturedSinks(sink, p, cfg.Enabled, firstID, previous, set), nil
}

func (r *querylogRuntime) adopt(route *querylog.Captured, cfg config.Querylog) bool {
	if !r.async.AdoptCaptured(route) {
		return false
	}
	r.current.Store(route)
	settings := cfg
	r.settings.Store(&settings)
	if w := r.writer.Load(); w != nil {
		mode, _ := route.Privacy()
		w.SetPrivacy(mode)
		w.SetRetention(cfg.Retention.Raw.Std(), int64(cfg.Retention.MaxDBBytes))
	}
	return true
}

// adoptPublished adopts the logging owner of a complete publication. The
// bundle releases that route's sinks itself, so the runtime gives up the
// reference it held for startup.
func (r *querylogRuntime) adoptPublished(route *querylog.Captured, cfg config.Querylog) {
	if r.adopt(route, cfg) {
		r.own(nil)
	}
}

func (r *querylogRuntime) reload(ctx context.Context, cfg config.Querylog) error {
	route, err := r.prepare(ctx, cfg, r.current.Load())
	if err != nil {
		return err
	}
	if !r.adopt(route, cfg) {
		route.Release()
		return errors.New("querylog: prepared route was not adopted")
	}
	// The successor holds its references before the predecessor's are
	// returned, so a sink both name keeps running. Requests on this path do
	// not hold their route: a query that read the predecessor just before
	// the adoption is still recorded locally, but a sink whose entry changed
	// has retired by then and counts it as dropped (D371).
	r.own(route)
	return nil
}

func (r *querylogRuntime) reader() *sqlitelog.Reader {
	if w := r.writer.Load(); w != nil {
		return w.Reader()
	}
	return nil
}

func (r *querylogRuntime) waitWriter(ctx context.Context) *sqlitelog.Writer {
	select {
	case <-ctx.Done():
		return nil
	case <-r.available:
		return r.writer.Load()
	}
}

func (r *querylogRuntime) run(ctx context.Context) error {
	if w := r.waitWriter(ctx); w != nil {
		return w.Run(ctx)
	}
	return nil
}

func (r *querylogRuntime) ready() (bool, []string) {
	if r.unavailable.Load() {
		return false, []string{"query log sink preparation unavailable"}
	}
	return true, nil
}

func (r *querylogRuntime) Purge(ctx context.Context, before time.Time) (int64, error) {
	if w := r.writer.Load(); w != nil {
		return w.Purge(ctx, before)
	}
	return 0, nil
}

// close flushes the collector and closes the local database. Its error is
// about those two only: the caller reports it as an incomplete local flush.
// The additional sinks stop within the same bound, and the ones that did
// not drain are reported on their own (closeSinks), or together when the
// collector did not flush (stopSinks).
func (r *querylogRuntime) close(ctx context.Context) error {
	if err := r.async.Close(ctx); err != nil {
		// Never close or reuse a writer while its batch is pending. The
		// additional sinks still stop within the same bound, which has
		// already ended: they are stopped, not drained.
		r.stopSinks(ctx)
		return err
	}
	// The collector has offered everything it accepted, so the database has
	// no batch pending. The additional sinks drain beside its close: an
	// exporter that does not finish in time cannot keep it open or use up
	// its share of the shutdown bound.
	sinks := make(chan struct{})
	go func() {
		defer close(sinks)
		r.closeSinks(ctx)
	}()
	var err error
	if w := r.writer.Load(); w != nil {
		err = w.CloseContext(ctx)
	}
	<-sinks
	return err
}

// stopSinks stops the additional sinks after the collector failed to flush
// within ctx, which has therefore ended. Whether a sink still finishes its
// last write in that instant is a race, so the sinks are not reported one
// by one as undrained (an idle sink would be named as often as not): when
// any sink was running, one line names them all and says that they were
// stopped with the collector's incomplete flush, which the caller reports
// as the local loss. queued counts what the sinks that had not finished
// still held.
func (r *querylogRuntime) stopSinks(ctx context.Context) {
	running := r.sinks.Status()
	err := r.sinks.Close(ctx)
	var undrained *querylog.SinkDrainError
	if err != nil && !errors.As(err, &undrained) {
		r.logger.Warn("additional query-log sinks did not stop", "err", err)
		return
	}
	if len(running) == 0 {
		return
	}
	names := make([]string, len(running))
	for i, st := range running {
		names[i] = st.Name
	}
	queued := 0
	if undrained != nil {
		for _, st := range undrained.Sinks {
			queued += st.Queued
		}
	}
	r.logger.Warn("additional query-log sinks stopped because the query log did not flush", "sinks", names, "queued", queued, "err", ctx.Err())
}

// closeSinks stops the additional sinks within ctx. A destination that is
// slow, stalled or unreachable when the server stops loses what its sink had
// not written. That is a loss at the exporter, not in the local query log,
// so it is logged per sink, apart from the collector's flush.
func (r *querylogRuntime) closeSinks(ctx context.Context) {
	err := r.sinks.Close(ctx)
	if err == nil {
		return
	}
	var undrained *querylog.SinkDrainError
	if !errors.As(err, &undrained) {
		r.logger.Warn("additional query-log sinks did not stop", "err", err)
		return
	}
	for _, st := range undrained.Sinks {
		// queued were still waiting and are lost with the pending write;
		// dropped is what the sink had already lost while it ran.
		r.logger.Warn("additional query-log sink did not drain", "sink", st.Name, "type", st.Type, "queued", st.Queued, "dropped", st.Dropped, "err", undrained.Err)
	}
}
