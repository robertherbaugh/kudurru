package querylog

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"
)

// Async is the collector of docs/architecture.md §11.2: a bounded queue
// in front of one goroutine that applies the exclude lists and the privacy
// transform once, stamps ids, publishes to the live tail and hands batches
// of BatchSize events (or whatever arrived within Interval) to the Sink.
// Overflow drops and counts; the query path never waits.
type Async struct {
	sink      Sink
	queue     chan *Event
	batchSize int
	interval  time.Duration
	privacy   atomic.Pointer[Privacy]
	tail      *Tail
	metrics   *Metrics
	clock     clock.WithTicker
	nextID    uint64
	dropped   atomic.Uint64
	logger    *slog.Logger
	wg        sync.WaitGroup
	stop      chan struct{}
	stopOnce  sync.Once
}

// AsyncOptions configures NewAsyncWith. Only Sink is required.
type AsyncOptions struct {
	Sink Sink
	// QueueSize bounds the hand-off queue (default 10 000).
	QueueSize int
	// BatchSize and Interval are the batch policy: flush at BatchSize
	// events or every Interval, whichever comes first (defaults 1 000 and
	// 1 s).
	BatchSize int
	Interval  time.Duration
	// Privacy is applied once per event before any consumer sees it; nil
	// logs everything unchanged.
	Privacy *Privacy
	// Tail receives every logged event (not under privacy nothing).
	Tail *Tail
	// Metrics counts events and drops; nil disables them.
	Metrics *Metrics
	// FirstID is the id stamped on the first event; later events count up.
	// Zero leaves ids unset (the sink assigns them, if at all). The SQLite
	// writer's NextID supplies it.
	FirstID uint64
	// Clock drives the batch interval; nil means the wall clock.
	Clock  clock.WithTicker
	Logger *slog.Logger
}

// NewAsync starts the writer goroutine with the given batch policy and no
// privacy transform, tail or metrics. Call Close to flush and stop.
func NewAsync(sink Sink, queueSize, batchSize int, interval time.Duration, logger *slog.Logger) *Async {
	return NewAsyncWith(AsyncOptions{Sink: sink, QueueSize: queueSize, BatchSize: batchSize, Interval: interval, Logger: logger})
}

// NewAsyncWith starts the collector goroutine. Call Close to flush and stop.
func NewAsyncWith(o AsyncOptions) *Async {
	if o.QueueSize <= 0 {
		o.QueueSize = 10000
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 1000
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	a := &Async{
		sink:      o.Sink,
		queue:     make(chan *Event, o.QueueSize),
		batchSize: o.BatchSize,
		interval:  o.Interval,
		tail:      o.Tail,
		metrics:   o.Metrics,
		clock:     o.Clock,
		nextID:    o.FirstID,
		logger:    o.Logger,
		stop:      make(chan struct{}),
	}
	a.privacy.Store(o.Privacy)
	ticker := a.clock.NewTicker(a.interval)
	a.wg.Add(1)
	go a.run(ticker)
	return a
}

// Enqueue implements Collector.
func (a *Async) Enqueue(ev *Event) bool {
	select {
	case a.queue <- ev:
		if a.metrics != nil {
			a.metrics.Events.Inc()
		}
		return true
	default:
		n := a.dropped.Add(1)
		a.metrics.Drop(DropStageQueue, "", 1)
		if n == 1 || n%10 == 0 && isPowerOfTen(n) {
			a.logger.Warn("query log queue full, dropping events", "dropped", n, "sink", a.sink.Name())
		}
		return false
	}
}

// SetPrivacy replaces the transform applied to events admitted from now on
// (querylog.privacy, anonymize and exclude.* hot reload); nil logs
// everything unchanged. Events already queued keep the old transform only
// if admitted before the swap.
func (a *Async) SetPrivacy(p *Privacy) { a.privacy.Store(p) }

// Dropped returns the number of events dropped since start.
func (a *Async) Dropped() uint64 { return a.dropped.Load() }

// Close flushes pending events and stops the writer.
func (a *Async) Close(ctx context.Context) error {
	a.stopOnce.Do(func() { close(a.stop) })
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.sink.Close()
}

// admit applies privacy, stamps the id and publishes to the tail. It
// returns false for excluded events.
func (a *Async) admit(ev *Event) bool {
	privacy := a.privacy.Load()
	if privacy != nil && !privacy.Apply(ev) {
		return false
	}
	if a.nextID != 0 {
		ev.ID = a.nextID
		a.nextID++
	}
	if a.tail != nil && (privacy == nil || privacy.Mode().StoresFacts()) {
		a.tail.Publish(ev)
	}
	return true
}

func (a *Async) run(ticker clock.Ticker) {
	defer a.wg.Done()
	defer ticker.Stop()
	batch := make([]*Event, 0, a.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := a.sink.Write(batch); err != nil {
			a.metrics.Drop(DropStageSink, a.sink.Name(), len(batch))
			a.logger.Error("query log sink write failed", "sink", a.sink.Name(), "err", err, "events", len(batch))
		}
		// Sinks may keep references to the events (they are immutable),
		// never to the slice, which is reused.
		clear(batch)
		batch = batch[:0]
	}
	add := func(ev *Event) {
		if a.admit(ev) {
			batch = append(batch, ev)
		}
	}
	for {
		select {
		case ev := <-a.queue:
			add(ev)
			if len(batch) >= a.batchSize {
				flush()
			}
		case <-ticker.C():
			flush()
		case <-a.stop:
			for {
				select {
				case ev := <-a.queue:
					add(ev)
					if len(batch) >= a.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func isPowerOfTen(n uint64) bool {
	for n > 1 {
		if n%10 != 0 {
			return false
		}
		n /= 10
	}
	return n == 1
}
