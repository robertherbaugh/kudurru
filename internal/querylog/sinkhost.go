package querylog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// Defaults of one additional sink (querylog.sinks, D371). The settings
// schema documents the same values.
const (
	DefaultSinkQueueSize = config.DefaultQuerylogSinkQueueSize
	DefaultSinkBatchSize = config.DefaultQuerylogSinkBatchSize
	DefaultSinkInterval  = config.DefaultQuerylogSinkFlushInterval
	DefaultSinkTimeout   = config.DefaultQuerylogSinkTimeout
	DefaultSinkAttempts  = config.DefaultQuerylogSinkAttempts
	DefaultSinkBackoff   = config.DefaultQuerylogSinkRetryBackoff
	// DefaultSinkDrain bounds the final drain of a removed sink.
	DefaultSinkDrain = 5 * time.Second
	// maxSinkBackoff caps the wait between two attempts, whatever the sink
	// asked for.
	maxSinkBackoff = 30 * time.Second
)

// ErrSinkHostClosed is returned by Prepare after Close.
var ErrSinkHostClosed = errors.New("querylog: sink host closed")

// SinkDrainError is returned by SinkHost.Close when its context ended before
// every sink had written what it held. It concerns the additional sinks
// only: the collector, the primary sink and the live tail are not involved.
type SinkDrainError struct {
	// Sinks are the sinks that had not finished, as they were when the
	// context ended: Queued events were still waiting then.
	Sinks []SinkStatus
	// Err is the context's error.
	Err error
}

func (e *SinkDrainError) Error() string {
	names := make([]string, len(e.Sinks))
	for i, st := range e.Sinks {
		names[i] = st.Name
	}
	return fmt.Sprintf("querylog: additional sinks did not drain (%s): %v", strings.Join(names, ", "), e.Err)
}

func (e *SinkDrainError) Unwrap() error { return e.Err }

// SinkSpec is one additional sink: an entry of querylog.sinks with its
// defaults applied. Two specs that differ in any field are different sinks.
type SinkSpec struct {
	// Name labels the sink in metrics and logs; unique within one set.
	Name string `json:"name"`
	// Type selects the SinkFactory.
	Type string `json:"type"`
	// QueueSize bounds the events waiting for this sink.
	QueueSize int `json:"queueSize"`
	// BatchSize and Interval are the batch policy: one write per BatchSize
	// events or per Interval, whichever comes first.
	BatchSize int           `json:"batchSize"`
	Interval  time.Duration `json:"interval"`
	// Timeout bounds Open and each write attempt.
	Timeout time.Duration `json:"timeout"`
	// Attempts is the total number of writes of one batch when the sink
	// reports a retryable failure; 1 never retries.
	Attempts int `json:"attempts"`
	// Backoff is the wait before the second attempt; it doubles up to 30 s.
	Backoff time.Duration `json:"backoff"`
	// Config is the type's own settings, handed to Sink.Open unchanged.
	Config json.RawMessage `json:"config,omitempty"`
}

func (s SinkSpec) withDefaults() SinkSpec {
	if s.QueueSize <= 0 {
		s.QueueSize = DefaultSinkQueueSize
	}
	if s.BatchSize <= 0 {
		s.BatchSize = DefaultSinkBatchSize
	}
	if s.BatchSize > s.QueueSize {
		s.BatchSize = s.QueueSize
	}
	if s.Interval <= 0 {
		s.Interval = DefaultSinkInterval
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultSinkTimeout
	}
	if s.Attempts <= 0 {
		s.Attempts = DefaultSinkAttempts
	}
	if s.Backoff <= 0 {
		s.Backoff = DefaultSinkBackoff
	}
	return s
}

// key identifies a running sink: an unchanged entry keeps its queue and
// goroutine across reloads, any change starts a new sink.
func (s SinkSpec) key() string {
	b, err := json.Marshal(s)
	if err != nil {
		return s.Name + "\x00" + s.Type
	}
	return string(b)
}

// SinkFactory builds the unopened sink of one spec. It validates spec.Config
// and returns without I/O and without acquiring anything: a prepared route
// may never be published. The host calls Open from the sink's own goroutine
// when the first event arrives, with spec.Config and a nil Registerer (the
// host owns the per-sink instruments), and Close only after a successful
// Open. Open may read credential files and dial; it is retried with backoff.
// A sink that implements BatchSink receives each attempt's deadline and the
// privacy mode of the batch. Its errors are logged: they must not contain
// credentials or event contents.
type SinkFactory func(spec SinkSpec) (Sink, error)

// retryableError marks a failed write the host may repeat.
type retryableError struct {
	err   error
	after time.Duration
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Retryable marks a failed write whose batch the sink knows was not applied
// (the connection was refused, the server answered "try again"), so the
// host may write the same batch again within the sink's attempt budget. Any
// other error drops the batch: an uncertain outcome is never replayed.
func Retryable(err error) error { return RetryableAfter(err, 0) }

// RetryableAfter is Retryable with the wait the destination asked for (an
// HTTP Retry-After); the host caps it at 30 s.
func RetryableAfter(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &retryableError{err: err, after: after}
}

// retryable reports whether err is marked retryable and the wait it carries.
func retryable(err error) (time.Duration, bool) {
	var r *retryableError
	if errors.As(err, &r) {
		return r.after, true
	}
	return 0, false
}

// SinkHostOptions configures NewSinkHost.
type SinkHostOptions struct {
	// Collector orders the retirement of a sink behind the events already
	// queued for it. Nil retires at once (tests without a collector).
	Collector *Async
	// Factories maps a sink type onto its constructor.
	Factories map[string]SinkFactory
	// Metrics counts drops in kudurru_querylog_dropped_total{stage="sink"}.
	Metrics *Metrics
	// Registerer receives the kudurru_sink_* collector; nil skips it.
	Registerer prometheus.Registerer
	Logger     *slog.Logger
	// Clock drives flush intervals, backoff and the drain bound.
	Clock clock.WithTickerAndDelayedExecution
	// Drain bounds the final drain of a removed sink (default 5 s).
	Drain time.Duration
}

// SinkHost runs the additional sinks of querylog.sinks (D371). Every sink
// has its own bounded queue, goroutine, batch policy, attempt budget and
// counters, so a slow or failing sink loses only its own events: the
// collector offers without waiting, and the primary sink and the live tail
// never see it. Events arrive after the privacy transform and are shared;
// a sink must not modify them.
//
// Sinks follow the captured-route model of the primary sink (D266). Prepare
// returns one reference per sink to the caller's route; an entry that did
// not change keeps its running sink, a new entry starts with the first
// event of a route that holds it, and a sink no route references drains
// what was queued for it once and closes. Nothing is replayed in any case.
type SinkHost struct {
	collector *Async
	factories map[string]SinkFactory
	metrics   *Metrics
	logger    *slog.Logger
	clock     clock.WithTickerAndDelayedExecution
	drain     time.Duration

	// stopping is set by Close; offer reads it without the lock.
	stopping atomic.Bool

	mu     sync.Mutex
	closed bool
	// live are the sinks at least one route references, by spec key.
	live map[string]*sinkWorker
	// unfinished are live and draining sinks.
	unfinished map[*sinkWorker]struct{}
	// names are the counters of every unfinished sink name.
	names map[string]*sinkCounters
}

// sinkCounters are shared by every worker of one sink name, so a changed
// entry keeps its series while the old worker drains.
type sinkCounters struct {
	users int // unfinished workers; under SinkHost.mu
	// referenced counts the workers of the name that a route still
	// references: above zero while the name is configured. It is written
	// under SinkHost.mu and read by offer without it.
	referenced  atomic.Int32
	sent        atomic.Uint64
	dropped     atomic.Uint64
	errors      atomic.Uint64
	lastSuccess atomic.Int64 // Unix nanoseconds; 0 for never
}

var (
	sinkSentDesc = prometheus.NewDesc("kudurru_sink_sent_total",
		"Query events an additional query-log sink accepted.", []string{"sink"}, nil)
	sinkErrorsDesc = prometheus.NewDesc("kudurru_sink_errors_total",
		"Failed attempts of an additional query-log sink to open, write, flush or close.", []string{"sink"}, nil)
	sinkQueueDesc = prometheus.NewDesc("kudurru_sink_queue_length",
		"Query events waiting in the queue of an additional query-log sink.", []string{"sink"}, nil)
	sinkLastSuccessDesc = prometheus.NewDesc("kudurru_sink_last_success_timestamp_seconds",
		"Unix time of the last successful write of an additional query-log sink; absent before the first.", []string{"sink"}, nil)
)

// NewSinkHost creates a host with no sinks.
func NewSinkHost(o SinkHostOptions) *SinkHost {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Drain <= 0 {
		o.Drain = DefaultSinkDrain
	}
	h := &SinkHost{
		collector: o.Collector, factories: o.Factories, metrics: o.Metrics, logger: o.Logger, clock: o.Clock, drain: o.Drain,
		live: map[string]*sinkWorker{}, unfinished: map[*sinkWorker]struct{}{}, names: map[string]*sinkCounters{},
	}
	if o.Registerer != nil {
		o.Registerer.MustRegister(h)
	}
	return h
}

// SinkSet is one route's reference on its additional sinks.
type SinkSet struct {
	host    *SinkHost
	workers []*sinkWorker
	once    sync.Once
}

// Names lists the sinks of the set in configuration order.
func (s *SinkSet) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.workers))
	for i, w := range s.workers {
		out[i] = w.spec.Name
	}
	return out
}

// Release returns the reference. It is idempotent and nil-safe. A sink that
// no set references anymore stops accepting once the collector has offered
// it every event that was already queued, drains within the host's bound
// and closes. Release only when the route can admit no more events: an
// event that a released route still delivers finds its sink retired and is
// lost for it (counted while the name is still configured, see missed).
func (s *SinkSet) Release() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		h := s.host
		var idle []*sinkWorker
		h.mu.Lock()
		for _, w := range s.workers {
			if w.refs--; w.refs == 0 {
				if h.live[w.key] == w {
					delete(h.live, w.key)
				}
				w.counters.referenced.Add(-1)
				idle = append(idle, w)
			}
		}
		h.mu.Unlock()
		for _, w := range idle {
			if h.collector != nil {
				h.collector.AfterQueued(w.retire)
			} else {
				w.retire()
			}
		}
	})
}

// Prepare returns a reference on the sinks of specs. It does no I/O and
// starts nothing: a set that is released before any event reaches it leaves
// no trace. An unknown type, a duplicate name or a factory error refuses
// the whole set and changes nothing.
func (h *SinkHost) Prepare(specs []SinkSpec) (*SinkSet, error) {
	resolved := make([]SinkSpec, len(specs))
	seen := make(map[string]bool, len(specs))
	for i, s := range specs {
		s = s.withDefaults()
		switch {
		case s.Name == "":
			return nil, fmt.Errorf("querylog: sink %d has no name", i)
		case seen[s.Name]:
			return nil, fmt.Errorf("querylog: duplicate sink name %q", s.Name)
		case h.factories[s.Type] == nil:
			return nil, fmt.Errorf("querylog: sink %q: unsupported type %q", s.Name, s.Type)
		}
		seen[s.Name] = true
		resolved[i] = s
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrSinkHostClosed
	}
	set := &SinkSet{host: h, workers: make([]*sinkWorker, 0, len(resolved))}
	var fresh []*sinkWorker
	for _, s := range resolved {
		key := s.key()
		w := h.live[key]
		if w == nil {
			sink, err := h.factories[s.Type](s)
			if err == nil && sink == nil {
				err = errors.New("no sink")
			}
			if err != nil {
				for _, unused := range fresh {
					unused.cancel()
				}
				return nil, fmt.Errorf("querylog: sink %q: %w", s.Name, err)
			}
			w = &sinkWorker{host: h, spec: s, key: key, sink: sink, queue: make(chan sinkItem, s.QueueSize), stop: make(chan struct{}), done: make(chan struct{})}
			w.writeCtx, w.cancel = context.WithCancel(context.Background())
			fresh = append(fresh, w)
		}
		set.workers = append(set.workers, w)
	}
	for _, w := range fresh {
		c := h.names[w.spec.Name]
		if c == nil {
			c = &sinkCounters{}
			h.names[w.spec.Name] = c
		}
		c.users++
		c.referenced.Add(1)
		w.counters = c
		h.live[w.key] = w
		h.unfinished[w] = struct{}{}
	}
	for _, w := range set.workers {
		w.refs++
	}
	return set, nil
}

// finished forgets a worker that has closed its sink, and the series of a
// name no sink carries anymore (the sink label stays bounded by the
// configuration).
func (h *SinkHost) finished(w *sinkWorker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.unfinished[w]; !ok {
		return
	}
	delete(h.unfinished, w)
	if w.counters.users--; w.counters.users == 0 {
		delete(h.names, w.spec.Name)
		if h.metrics != nil {
			h.metrics.Dropped.DeleteLabelValues(DropStageSink, w.spec.Name)
		}
	}
}

// SinkStatus is the state of one additional sink name.
type SinkStatus struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Queued and QueueSize are the events waiting and the bound.
	Queued    int `json:"queued"`
	QueueSize int `json:"queue_size"`
	// Sent, Dropped and Errors count events written, events lost (queue
	// overflow or an abandoned batch) and failed attempts.
	Sent    uint64 `json:"sent"`
	Dropped uint64 `json:"dropped"`
	Errors  uint64 `json:"errors"`
	// LastSuccess is the time of the last successful write; zero for never.
	LastSuccess time.Time `json:"last_success,omitzero"`
}

// Status lists the sinks that are running or draining, by name.
func (h *SinkHost) Status() []SinkStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	byName := make(map[string]*SinkStatus, len(h.names))
	for w := range h.unfinished {
		st := byName[w.spec.Name]
		if st == nil {
			c := w.counters
			st = &SinkStatus{Name: w.spec.Name, Sent: c.sent.Load(), Dropped: c.dropped.Load(), Errors: c.errors.Load()}
			if ns := c.lastSuccess.Load(); ns != 0 {
				st.LastSuccess = time.Unix(0, ns)
			}
			byName[w.spec.Name] = st
		}
		st.Queued += len(w.queue)
		// The entry in force is the one routes still reference.
		if st.Type == "" || h.live[w.key] == w {
			st.Type, st.QueueSize = w.spec.Type, w.spec.QueueSize
		}
	}
	out := make([]SinkStatus, 0, len(byName))
	for _, st := range byName {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Describe implements prometheus.Collector.
func (h *SinkHost) Describe(ch chan<- *prometheus.Desc) {
	ch <- sinkSentDesc
	ch <- sinkErrorsDesc
	ch <- sinkQueueDesc
	ch <- sinkLastSuccessDesc
}

// Collect implements prometheus.Collector: one series per configured sink
// name, gone when the sink is.
func (h *SinkHost) Collect(ch chan<- prometheus.Metric) {
	for _, st := range h.Status() {
		ch <- prometheus.MustNewConstMetric(sinkSentDesc, prometheus.CounterValue, float64(st.Sent), st.Name)
		ch <- prometheus.MustNewConstMetric(sinkErrorsDesc, prometheus.CounterValue, float64(st.Errors), st.Name)
		ch <- prometheus.MustNewConstMetric(sinkQueueDesc, prometheus.GaugeValue, float64(st.Queued), st.Name)
		if !st.LastSuccess.IsZero() {
			ch <- prometheus.MustNewConstMetric(sinkLastSuccessDesc, prometheus.GaugeValue, float64(st.LastSuccess.UnixNano())/1e9, st.Name)
		}
	}
}

// Close retires every sink and waits for the drains. Close the collector
// first so everything it accepted has been offered. When ctx ends first the
// pending writes are canceled and Close returns a *SinkDrainError that names
// the sinks still running and wraps ctx's error; a sink that ignores
// cancellation keeps its goroutine and is never closed under its own write.
// Close may be called again to join.
func (h *SinkHost) Close(ctx context.Context) error {
	h.stopping.Store(true)
	h.mu.Lock()
	h.closed = true
	workers := make([]*sinkWorker, 0, len(h.unfinished))
	for w := range h.unfinished {
		workers = append(workers, w)
	}
	h.live = map[string]*sinkWorker{}
	h.mu.Unlock()
	for _, w := range workers {
		w.retire()
	}
	for _, w := range workers {
		select {
		case <-w.done:
		case <-ctx.Done():
			// Read before the cancellation moves queued events to the
			// dropped counter.
			undrained := h.Status()
			for _, pending := range workers {
				pending.cancel()
			}
			if len(undrained) == 0 {
				return nil // the last sink closed as the context ended
			}
			return &SinkDrainError{Sinks: undrained, Err: ctx.Err()}
		}
	}
	return nil
}

// sinkItem is one queued event and the privacy mode it was admitted under.
type sinkItem struct {
	event *Event
	mode  Mode
}

// sinkWorker is one running sink: its queue and the goroutine that batches,
// writes, retries and counts.
type sinkWorker struct {
	host     *SinkHost
	spec     SinkSpec
	key      string
	sink     Sink
	counters *sinkCounters
	queue    chan sinkItem
	refs     int // under SinkHost.mu

	// lifecycle orders the first start against retirement; the flags are
	// atomic so offer and Status read them without it.
	lifecycle sync.Mutex
	started   atomic.Bool
	retired   atomic.Bool
	stop      chan struct{} // closed by retire
	done      chan struct{} // closed when the goroutine has closed the sink

	writeCtx context.Context
	cancel   context.CancelFunc
}

// offer queues an event without waiting; the collector goroutine is the
// only caller. The first event starts the sink.
func (w *sinkWorker) offer(item sinkItem) {
	if w.retired.Load() {
		w.missed()
		return
	}
	select {
	case w.queue <- item:
		if !w.started.Load() {
			w.start()
		}
	default:
		w.drop(1, "queue full", nil)
	}
}

// missed accounts for an event that reached this sink after it retired.
//
// A route whose references were returned can still deliver an event where
// requests do not hold their route: Async.Enqueue reads the adopted route
// and then queues, so a reload that replaces the route in between (the
// reload subscriber of the Kubernetes driver) leaves that event with the
// predecessor. When the entry was changed, the name is still configured and
// the event is a loss for it: it is counted, and it is not handed to the
// successor, whose route made its own privacy decision. When the entry was
// removed, no sink was to receive the event and nothing is counted, so the
// removed name's series stay gone. A request that holds a complete
// publication never gets here: the publication returns its references only
// after its last request.
//
// The other way here is a host that was closed while the collector could
// not drain; those events are lost with the shutdown and counted too.
func (w *sinkWorker) missed() {
	if w.counters.referenced.Load() == 0 {
		return
	}
	reason := "sink replaced"
	if w.host.stopping.Load() {
		reason = "sink host stopped"
	}
	w.drop(1, reason, nil)
}

func (w *sinkWorker) start() {
	w.lifecycle.Lock()
	defer w.lifecycle.Unlock()
	if w.started.Load() || w.retired.Load() {
		return
	}
	w.started.Store(true)
	go w.run()
}

// retire stops admission and lets the goroutine drain within the bound. It
// runs after the collector has offered every event queued for this sink.
func (w *sinkWorker) retire() {
	w.lifecycle.Lock()
	if w.retired.Load() {
		w.lifecycle.Unlock()
		return
	}
	w.retired.Store(true)
	close(w.stop)
	started := w.started.Load()
	w.lifecycle.Unlock()
	if started {
		w.host.clock.AfterFunc(w.host.drain, w.cancel)
		return
	}
	// Never started, so never opened. Only a host closed under a collector
	// that is still running can leave an event here.
	w.drop(len(w.queue), "sink retired before it started", nil)
	w.cancel()
	w.host.finished(w)
	close(w.done)
}

func (w *sinkWorker) drop(n int, reason string, err error) {
	if n <= 0 {
		return
	}
	total := w.counters.dropped.Add(uint64(n))
	w.host.metrics.Drop(DropStageSink, w.spec.Name, n)
	// Power-of-ten warnings: a stalled sink drops every event, so log the
	// first loss and then each tenfold total.
	if crossedPowerOfTen(total-uint64(n), total) {
		w.host.logger.Warn("query log sink dropped events", "sink", w.spec.Name, "type", w.spec.Type, "reason", reason, "dropped", total, "err", err)
	}
}

func (w *sinkWorker) failed(what string, err error) {
	if n := w.counters.errors.Add(1); isPowerOfTen(n) {
		w.host.logger.Warn("query log sink "+what+" failed", "sink", w.spec.Name, "type", w.spec.Type, "failures", n, "err", err)
	}
}

// backoff is the wait before attempt+1.
func (w *sinkWorker) backoff(attempt int, asked time.Duration) time.Duration {
	d := w.spec.Backoff
	for i := 1; i < attempt && d < maxSinkBackoff; i++ {
		d *= 2
	}
	return min(max(d, asked), maxSinkBackoff)
}

// wait sleeps for d; false when the sink was retired first.
func (w *sinkWorker) wait(d time.Duration) bool {
	t := w.host.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-w.stop:
		return false
	}
}

// open retries Open until it succeeds or the sink is retired. An exporter
// that is down at start never delays startup or a reload; meanwhile the
// queue fills to its bound and the overflow is counted.
func (w *sinkWorker) open() bool {
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(w.writeCtx, w.spec.Timeout)
		err := w.sink.Open(ctx, w.spec.Config, nil)
		cancel()
		if err == nil {
			return true
		}
		w.failed("open", err)
		if !w.wait(w.backoff(attempt, 0)) {
			return false
		}
	}
}

func (w *sinkWorker) run() {
	defer close(w.done)
	defer w.host.finished(w)
	defer w.cancel()
	if !w.open() {
		w.drop(len(w.queue), "sink never opened", nil)
		return
	}
	defer func() {
		if err := w.sink.Close(); err != nil {
			w.failed("close", err)
		}
	}()
	ticker := w.host.clock.NewTicker(w.spec.Interval)
	defer ticker.Stop()
	batch := make([]*Event, 0, w.spec.BatchSize)
	mode := PrivacyFull
	flush := func() {
		if len(batch) > 0 {
			w.write(batch, mode)
			// Sinks may keep the events (they are immutable), never the
			// slice, which is reused.
			clear(batch)
			batch = batch[:0]
		}
	}
	add := func(item sinkItem) {
		if item.mode != mode {
			flush()
			mode = item.mode
		}
		batch = append(batch, item.event)
		if len(batch) >= w.spec.BatchSize {
			flush()
		}
	}
	for {
		select {
		case item := <-w.queue:
			add(item)
		case <-ticker.C():
			flush()
		case <-w.stop:
			// Retired: the collector offers nothing more. Write what is
			// queued once; after the drain bound writeCtx is canceled and
			// write counts the rest as dropped.
			for {
				select {
				case item := <-w.queue:
					add(item)
				default:
					flush()
					ctx, cancel := context.WithTimeout(w.writeCtx, w.spec.Timeout)
					if err := w.sink.Flush(ctx); err != nil {
						w.failed("flush", err)
					}
					cancel()
					return
				}
			}
		}
	}
}

// write hands one batch to the sink: at most Attempts times, and again only
// after an error the sink marked retryable. Each attempt is bounded by
// Timeout. A batch that is not written is counted and dropped, never
// offered to another sink.
func (w *sinkWorker) write(batch []*Event, mode Mode) {
	for attempt := 1; ; attempt++ {
		if err := w.writeCtx.Err(); err != nil {
			w.drop(len(batch), "drain bound reached", err)
			return
		}
		ctx, cancel := context.WithTimeout(w.writeCtx, w.spec.Timeout)
		var err error
		if sink, ok := w.sink.(BatchSink); ok {
			err = sink.WriteBatch(ctx, batch, mode)
		} else {
			err = w.sink.Write(batch)
		}
		cancel()
		if err == nil {
			w.counters.sent.Add(uint64(len(batch)))
			w.counters.lastSuccess.Store(w.host.clock.Now().UnixNano())
			return
		}
		w.failed("write", err)
		asked, again := retryable(err)
		switch {
		case !again:
			w.drop(len(batch), "write failed", err)
			return
		case attempt >= w.spec.Attempts:
			w.drop(len(batch), "attempts exhausted", err)
			return
		}
		// A retired sink retries at once: its drain is bounded as a whole.
		if !w.retired.Load() {
			w.wait(w.backoff(attempt, asked))
		}
	}
}

// crossedPowerOfTen reports whether a power of ten lies in (prev, total].
func crossedPowerOfTen(prev, total uint64) bool {
	if total == 0 {
		return false
	}
	p := uint64(1) // the largest power of ten that is at most total
	for p <= total/10 {
		p *= 10
	}
	return p > prev
}
