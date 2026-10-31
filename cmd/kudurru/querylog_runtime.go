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
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// querylogRuntime owns one queue/tail and at most one local database for the
// node lifetime. Immutable captured routes borrow these owners through final
// DNS emission and queue drain; a reload never closes an old writer underneath
// them. Local history remains readable when admission is disabled or uses slog.
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

func newQuerylogRuntime(ctx context.Context, cfg *config.Config, reg prometheus.Registerer, logger *slog.Logger, domains func() int64) (*querylogRuntime, error) {
	r := &querylogRuntime{dir: cfg.QuerylogDataDir(), node: cfg.Node.Name, logger: logger, domains: domains, available: make(chan struct{})}
	initial := cfg.Querylog
	r.settings.Store(&initial)
	r.metrics = querylog.NewMetrics(reg)
	r.tail = querylog.NewTail(0, 0, r.metrics)
	r.slog = &querylog.SlogSink{Logger: logger, Level: slog.LevelInfo}
	if err := r.slog.Open(ctx, nil, nil); err != nil {
		return nil, err
	}
	r.async = querylog.NewAsyncWith(querylog.AsyncOptions{Sink: borrowedQuerylogSink{r.slog}, Tail: r.tail, Metrics: r.metrics, FirstID: 1, Logger: logger})
	route, err := r.prepare(ctx, cfg.Querylog, nil)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = r.close(cleanup)
		return nil, err
	}
	r.adopt(route, cfg.Querylog)
	return r, nil
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
	var sink querylog.Sink = borrowedQuerylogSink{r.slog}
	var firstID uint64 = 1
	if cfg.Enabled {
		// Read existing history before assigning new tail IDs, including when
		// this start uses slog. A later switch must not collide with old rows.
		_, statErr := os.Stat(filepath.Join(r.dir, sqlitelog.FileName))
		existing := statErr == nil
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		if cfg.Sink == "sqlite" || existing {
			w, openErr := r.ensureWriter(ctx)
			if openErr != nil {
				return nil, openErr
			}
			firstID = r.firstID.Load()
			if cfg.Sink == "sqlite" {
				sink = borrowedQuerylogSink{w}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.async.PrepareCaptured(sink, p, cfg.Enabled, firstID, previous), nil
}

func (r *querylogRuntime) adopt(route *querylog.Captured, cfg config.Querylog) {
	if !r.async.AdoptCaptured(route) {
		return
	}
	r.current.Store(route)
	settings := cfg
	r.settings.Store(&settings)
	if w := r.writer.Load(); w != nil {
		mode, _ := route.Privacy()
		w.SetPrivacy(mode)
		w.SetRetention(cfg.Retention.Raw.Std(), int64(cfg.Retention.MaxDBBytes))
	}
}

func (r *querylogRuntime) reload(ctx context.Context, cfg config.Querylog) error {
	route, err := r.prepare(ctx, cfg, r.current.Load())
	if err != nil {
		return err
	}
	r.adopt(route, cfg)
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

func (r *querylogRuntime) close(ctx context.Context) error {
	if err := r.async.Close(ctx); err != nil {
		return err // never close or reuse a writer while its batch is pending
	}
	if w := r.writer.Load(); w != nil {
		return w.CloseContext(ctx)
	}
	return nil
}
