package querylog

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Async is a Collector that batches events onto a Sink from one goroutine.
// Overflow drops and counts; the query path never waits.
type Async struct {
	sink      Sink
	queue     chan *Event
	batchSize int
	interval  time.Duration
	dropped   atomic.Uint64
	logger    *slog.Logger
	wg        sync.WaitGroup
	stop      chan struct{}
	stopOnce  sync.Once
}

// NewAsync starts the writer goroutine. Call Close to flush and stop.
func NewAsync(sink Sink, queueSize, batchSize int, interval time.Duration, logger *slog.Logger) *Async {
	if queueSize <= 0 {
		queueSize = 10000
	}
	if batchSize <= 0 {
		batchSize = 1000
	}
	if interval <= 0 {
		interval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Async{
		sink:      sink,
		queue:     make(chan *Event, queueSize),
		batchSize: batchSize,
		interval:  interval,
		logger:    logger,
		stop:      make(chan struct{}),
	}
	a.wg.Add(1)
	go a.run()
	return a
}

// Enqueue implements Collector.
func (a *Async) Enqueue(ev *Event) bool {
	select {
	case a.queue <- ev:
		return true
	default:
		n := a.dropped.Add(1)
		if n == 1 || n%10 == 0 && isPowerOfTen(n) {
			a.logger.Warn("query log queue full, dropping events", "dropped", n, "sink", a.sink.Name())
		}
		return false
	}
}

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

func (a *Async) run() {
	defer a.wg.Done()
	batch := make([]*Event, 0, a.batchSize)
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := a.sink.Write(batch); err != nil {
			a.logger.Error("query log sink write failed", "sink", a.sink.Name(), "err", err, "events", len(batch))
		}
		batch = batch[:0]
	}
	for {
		select {
		case ev := <-a.queue:
			batch = append(batch, ev)
			if len(batch) >= a.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-a.stop:
			for {
				select {
				case ev := <-a.queue:
					batch = append(batch, ev)
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
