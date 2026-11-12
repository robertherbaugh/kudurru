package tracing

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
)

// Defaults of the tracing.* settings (D382).
const (
	DefaultRatio         = 0.001
	DefaultSlow          = 250 * time.Millisecond
	DefaultQueueSize     = 2048
	DefaultBatchSize     = 256
	DefaultFlushInterval = 2 * time.Second
	DefaultTimeout       = 10 * time.Second
)

// RootName is the name of the server span of every query.
const RootName = "kudurru.dns.query"

// Exporter sends finished, kept traces somewhere. Export is called by one
// goroutine at a time, never from the query path. It must return within
// ctx; an error drops the batch (counted), it is never sent again.
type Exporter interface {
	Export(ctx context.Context, traces []*Trace) error
}

// Config configures a Tracer.
type Config struct {
	// Ratio is the head sampling ratio for queries that do not continue a
	// sampled or unsampled caller trace, from 0 to 1.
	Ratio float64
	// Slow keeps every query that took at least this long; zero disables
	// the slow rule.
	Slow time.Duration
	// OptionCode is the EDNS option code of TRACEPARENT (DefaultOptionCode
	// when zero).
	OptionCode uint16
	// AcceptIncoming lets a TRACEPARENT from a trusted peer (an
	// authenticated PROXYv2 header, D45/D56) continue the caller's trace.
	// From anyone else the option is always removed and ignored.
	AcceptIncoming bool
	// QueueSize, BatchSize, FlushInterval and Timeout are the export queue
	// policy; zero values take the defaults.
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	Timeout       time.Duration
	Exporter      Exporter
	Registerer    prometheus.Registerer
	Logger        *slog.Logger
}

// Tracer starts traces and exports the kept ones.
type Tracer struct {
	ratioBound uint64
	slow       time.Duration
	code       uint16
	accept     bool
	exporter   Exporter
	batch      int
	flush      time.Duration
	timeout    time.Duration
	logger     *slog.Logger
	metrics    *metrics

	queue chan *Trace
	mu    sync.Mutex
	// closed is set by Close; offers after it are dropped as shutdown.
	closed  bool
	stop    chan struct{}
	done    chan struct{}
	started bool
}

// New checks c and builds a tracer. Run must be started for kept traces
// to leave the queue.
func New(c Config) (*Tracer, error) {
	switch {
	case c.Ratio < 0 || c.Ratio > 1 || c.Ratio != c.Ratio:
		return nil, errors.New("tracing: the sampling ratio must be between 0 and 1")
	case c.Slow < 0:
		return nil, errors.New("tracing: the slow threshold must not be negative")
	case c.OptionCode != 0 && (c.OptionCode < MinOptionCode || c.OptionCode > MaxOptionCode):
		return nil, errors.New("tracing: the TRACEPARENT option code must be in the local range 65001-65534")
	case c.Exporter == nil:
		return nil, errors.New("tracing: an exporter is required")
	}
	t := &Tracer{
		slow: c.Slow, code: c.OptionCode, accept: c.AcceptIncoming, exporter: c.Exporter,
		batch: c.BatchSize, flush: c.FlushInterval, timeout: c.Timeout, logger: c.Logger,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if t.code == 0 {
		t.code = DefaultOptionCode
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if t.batch <= 0 {
		t.batch = DefaultBatchSize
	}
	if t.flush <= 0 {
		t.flush = DefaultFlushInterval
	}
	if t.timeout <= 0 {
		t.timeout = DefaultTimeout
	}
	if t.logger == nil {
		t.logger = slog.Default()
	}
	// The ratio sampler of OpenTelemetry (TraceIDRatioBased): the low 63
	// bits of the trace id's last eight bytes against ratio * 2^63, so every
	// hop that uses the same ratio makes the same decision.
	switch {
	case c.Ratio >= 1:
		t.ratioBound = 1 << 63
	case c.Ratio > 0:
		t.ratioBound = uint64(c.Ratio * (1 << 63))
	}
	t.queue = make(chan *Trace, c.QueueSize)
	m, err := newMetrics(c.Registerer, func() float64 { return float64(len(t.queue)) })
	if err != nil {
		return nil, err
	}
	t.metrics = m
	return t, nil
}

// OptionCode returns the configured TRACEPARENT option code.
func (t *Tracer) OptionCode() uint16 {
	if t == nil {
		return DefaultOptionCode
	}
	return t.code
}

func (t *Tracer) ratioSampled(id TraceID) bool {
	if t.ratioBound >= 1<<63 {
		return true
	}
	return binary.BigEndian.Uint64(id[8:])>>1 < t.ratioBound
}

// Begin starts the trace of one query received at at. It removes every
// TRACEPARENT option from m, whoever sent it, so no later stage, cache key
// or answer sees it. trusted reports an authenticated edge peer (the
// request's PROXYv2 header passed the proxy policy): only then, and only
// with AcceptIncoming, does a well-formed option make this query part of
// the caller's trace, and such a trace is always kept: the peer sent the
// option because it traces the query (parent-based sampling where the
// option's presence is the parent's decision). Every other query starts a
// new trace that the ratio sampler decides. carried reports whether the query held the option.
func (t *Tracer) Begin(m *dns.Msg, trusted bool, at time.Time) (tr *Trace, carried bool) {
	if t == nil {
		return nil, false
	}
	ex := TakeOption(m, t.code)
	tr = &Trace{tracer: t}
	tr.spans, tr.attrs = tr.inline[:0], tr.inlineAttrs[:0]
	switch {
	case !ex.Present:
	case !trusted:
		t.metrics.traceparent(resultUntrusted)
	case !t.accept:
		t.metrics.traceparent(resultIgnored)
	case !ex.Valid:
		t.metrics.traceparent(resultMalformed)
	default:
		t.metrics.traceparent(resultAccepted)
		tr.id, tr.remote, tr.fromPeer = ex.Remote.TraceID, ex.Remote.SpanID, true
	}
	if !tr.id.IsValid() {
		tr.id = newTraceID()
	}
	// A trusted peer sends TRACEPARENT only for a query it traces (dnsdist:
	// SetTraceAction on the rules the operator chose), so the option itself
	// is the parent's sampling decision. Its TRACEFLAGS are not used:
	// dnsdist 2.1.3 sends 00 for the queries it traces (observed against
	// the pinned image, D382), and honoring that would drop every edge trace.
	if tr.fromPeer {
		tr.sampled = true
	} else {
		tr.sampled = t.ratioSampled(tr.id)
	}
	tr.Start(0, RootName, KindServer, at)
	return tr, ex.Present
}

func (t *Tracer) offer(tr *Trace) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		t.metrics.trace(resultShutdown, 1)
		return
	}
	select {
	case t.queue <- tr:
	default:
		t.metrics.trace(resultQueueFull, 1)
	}
}

// Run exports queued traces in batches until ctx ends or Close is called.
func (t *Tracer) Run(ctx context.Context) {
	t.mu.Lock()
	if t.started || t.closed {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()
	defer close(t.done)
	ticker := time.NewTicker(t.flush)
	defer ticker.Stop()
	batch := make([]*Trace, 0, t.batch)
	for {
		select {
		case tr := <-t.queue:
			batch = append(batch, tr)
			if len(batch) >= t.batch {
				batch = t.send(ctx, batch)
			}
		case <-ticker.C:
			batch = t.send(ctx, batch)
		case <-ctx.Done():
			t.metrics.trace(resultShutdown, len(batch)+len(t.queue))
			return
		case <-t.stop:
			t.drain(ctx, batch)
			return
		}
	}
}

// send exports batch, counts the outcome and returns the emptied batch.
func (t *Tracer) send(ctx context.Context, batch []*Trace) []*Trace {
	if len(batch) == 0 {
		return batch
	}
	ectx, cancel := context.WithTimeout(ctx, t.timeout)
	err := t.exporter.Export(ectx, batch)
	cancel()
	if err != nil {
		t.metrics.trace(resultFailed, len(batch))
		t.logger.Warn("trace export failed; the batch is dropped", "traces", len(batch), "err", err)
	} else {
		t.metrics.trace(resultExported, len(batch))
	}
	clear(batch)
	return batch[:0]
}

// drain exports batch and what is queued now.
func (t *Tracer) drain(ctx context.Context, batch []*Trace) {
	for {
		select {
		case tr := <-t.queue:
			batch = append(batch, tr)
			if len(batch) >= t.batch {
				batch = t.send(ctx, batch)
			}
		default:
			t.send(ctx, batch)
			return
		}
	}
}

// Close stops accepting traces and exports what is queued, waiting for it
// until ctx ends. When Run never started, Close exports the queue itself.
// It is safe to call more than once.
func (t *Tracer) Close(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		select {
		case <-t.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.closed = true
	running := t.started
	t.started = true
	t.mu.Unlock()
	if !running {
		defer close(t.done)
		t.drain(ctx, nil)
		return ctx.Err()
	}
	close(t.stop)
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
