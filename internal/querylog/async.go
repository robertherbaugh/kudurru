package querylog

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"
)

// Async is the collector of dev-docs/design-record.md §11.2: a bounded queue
// in front of one goroutine that applies the exclude lists and the privacy
// transform once, stamps ids, publishes to the live tail and hands batches
// of BatchSize events (or whatever arrived within Interval) to the Sink.
// Overflow drops and counts; the query path never waits. After the tail it
// offers each event, without waiting, to the bounded queue of every
// additional sink its captured route holds (SinkHost, D371).
type Async struct {
	sink        Sink
	queue       chan queuedEvent
	batchSize   int
	interval    time.Duration
	privacy     atomic.Pointer[Privacy]
	tail        *Tail
	metrics     *Metrics
	clock       clock.WithTicker
	nextID      uint64
	dropped     atomic.Uint64
	logger      *slog.Logger
	wg          sync.WaitGroup
	stop        chan struct{}
	stopOnce    sync.Once
	admission   sync.RWMutex // Enqueue uses TryRLock, so DNS never waits
	closed      bool
	configure   sync.Mutex
	destination atomic.Pointer[destination]
	done        chan struct{}
	closeErr    error
	writeCtx    context.Context
	cancelWrite context.CancelFunc
	// deferred holds AfterQueued callbacks the collector has not ordered
	// behind its queue yet; drained is set once the collector has exited.
	deferMu  sync.Mutex
	deferred []func()
	drained  bool
	wake     chan struct{}
}

type destination struct {
	sink    Sink
	firstID uint64
	privacy *Privacy
	enabled bool
	// sinks are the additional sinks of this route; nil for none.
	sinks *SinkSet
}

// barrier runs fn once the collector has admitted remaining more events.
type barrier struct {
	remaining int
	fn        func()
}

type queuedEvent struct {
	event       *Event
	destination *destination
}

// Captured is an immutable logging decision within one serving publication.
// Its sink and privacy never consult a successor's operational settings.
type Captured struct {
	owner       *Async
	destination *destination
}

// Enqueue emits through this original owner. A disabled or unavailable owner
// records nothing; it never falls back to the collector's current route.
func (c *Captured) Enqueue(ev *Event) bool {
	return c != nil && c.owner != nil && c.owner.enqueue(ev, c.destination)
}

// Pseudonym uses this owner's original privacy lifecycle.
func (c *Captured) Pseudonym(addr netip.Addr) string {
	if c == nil || c.destination == nil {
		return ""
	}
	return c.destination.privacy.Pseudonym(addr)
}

// Enabled reports the captured logging decision.
func (c *Captured) Enabled() bool { return c != nil && c.destination != nil && c.destination.enabled }

// Privacy reports the captured transform mode and pseudonym setting.
func (c *Captured) Privacy() (Mode, bool) {
	if c == nil || c.destination == nil || c.destination.privacy == nil {
		return PrivacyNothing, false
	}
	p := c.destination.privacy
	return p.mode, p.pseudonym
}

// Release returns this route's reference on its additional sinks. It is
// idempotent and nil-safe. Call it once the route can admit no more events:
// a sink that no route references drains behind the events already accepted
// and closes (SinkSet.Release).
func (c *Captured) Release() {
	if c != nil && c.destination != nil {
		c.destination.sinks.Release()
	}
}

// PrepareCaptured derives privacy continuity only from an actually published
// predecessor. Preparing or rejecting a candidate never mutates that owner.
func (a *Async) PrepareCaptured(sink Sink, p *Privacy, enabled bool, firstID uint64, previous *Captured) *Captured {
	return a.PrepareCapturedSinks(sink, p, enabled, firstID, previous, nil)
}

// PrepareCapturedSinks is PrepareCaptured for a route that also feeds the
// additional sinks of set. The route takes over the caller's reference on
// set; whoever owns the route calls Release.
func (a *Async) PrepareCapturedSinks(sink Sink, p *Privacy, enabled bool, firstID uint64, previous *Captured, set *SinkSet) *Captured {
	var prior *Privacy
	if previous != nil && previous.owner == a && previous.destination != nil {
		prior = previous.destination.privacy
	}
	return &Captured{owner: a, destination: &destination{sink: sink, privacy: retainPrivacy(p, prior), enabled: enabled, firstID: firstID, sinks: set}}
}

// AdoptCaptured updates legacy admission and local operational reporting. DNS
// requests use their captured component directly, including after this adoption.
func (a *Async) AdoptCaptured(c *Captured) bool {
	if c == nil || c.owner != a || c.destination == nil {
		return false
	}
	a.configure.Lock()
	defer a.configure.Unlock()
	a.privacy.Store(c.destination.privacy)
	a.destination.Store(c.destination)
	return true
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
		queue:     make(chan queuedEvent, o.QueueSize),
		batchSize: o.BatchSize,
		interval:  o.Interval,
		tail:      o.Tail,
		metrics:   o.Metrics,
		clock:     o.Clock,
		nextID:    o.FirstID,
		logger:    o.Logger,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
	}
	a.privacy.Store(o.Privacy)
	a.writeCtx, a.cancelWrite = context.WithCancel(context.Background())
	a.destination.Store(&destination{sink: o.Sink, firstID: o.FirstID, privacy: o.Privacy, enabled: true})
	ticker := a.clock.NewTicker(a.interval)
	a.wg.Add(1)
	go a.run(ticker)
	return a
}

// Enqueue implements Collector.
func (a *Async) Enqueue(ev *Event) bool {
	return a.enqueue(ev, a.destination.Load())
}

func (a *Async) enqueue(ev *Event, dst *destination) bool {
	if !a.admission.TryRLock() {
		a.dropQueue()
		return false
	}
	defer a.admission.RUnlock()
	if a.closed || dst == nil || !dst.enabled {
		return false
	}
	item := queuedEvent{event: ev, destination: dst}
	select {
	case a.queue <- item:
		if a.metrics != nil {
			a.metrics.Events.Inc()
		}
		return true
	default:
		a.dropQueue()
		return false
	}
}

func (a *Async) dropQueue() {
	a.dropped.Add(1)
	a.metrics.Drop(DropStageQueue, "", 1)
}

// SetPrivacy replaces the transform applied to events admitted from now on
// (querylog.privacy, anonymize and exclude.* hot reload); nil logs
// everything unchanged. Events already queued keep the old transform only
// if admitted before the swap.
func (a *Async) SetPrivacy(p *Privacy) {
	a.configure.Lock()
	defer a.configure.Unlock()
	next := retainPrivacy(p, a.privacy.Load())
	a.privacy.Store(next)
	if current := a.destination.Load(); current != nil {
		dst := *current
		dst.privacy = next
		a.destination.Store(&dst)
	}
}

func retainPrivacy(p, previous *Privacy) *Privacy {
	next := p
	if p != nil && previous != nil && p.mode == PrivacyAnonymizeClient && previous.mode == p.mode && p.pseudonym && previous.pseudonym {
		// Exclusion-only changes keep one daily identity, including an
		// Apply/Pseudonym already holding the previous filter. Copy the
		// immutable filter; never modify the caller's published instance.
		updated := *p
		updated.keys = previous.keys
		next = &updated
	} else if p != nil && previous != nil {
		// Reusing a previously prepared filter after a mode change must
		// not restore the old pseudonym lifecycle either.
		updated := *p
		updated.keys = &pseudonymKeys{clock: p.keys.clock}
		next = &updated
	}
	return next
}

// Pseudonym is the current filter's Pseudonym of addr: the client name its
// queries are logged under, "" when clients are not pseudonymised.
func (a *Async) Pseudonym(addr netip.Addr) string { return a.privacy.Load().Pseudonym(addr) }

// Enabled reports operational admission for local reporting, not a DNS query's
// captured decision.
func (a *Async) Enabled() bool { return a.destination.Load().enabled }

// PrivacySetting reports the operational transform for local block-page lookup.
func (a *Async) PrivacySetting() (Mode, bool) {
	return (&Captured{destination: a.destination.Load()}).Privacy()
}

// Dropped returns the number of events dropped since start.
func (a *Async) Dropped() uint64 { return a.dropped.Load() }

// AfterQueued runs fn on the collector goroutine once every event accepted
// before this call has been admitted and offered to its sinks, and on the
// caller once the collector has drained and exited. It never waits. fn must
// not block. The sink host retires a sink through it, so a removed sink still
// receives everything that was queued for it and nothing is replayed.
func (a *Async) AfterQueued(fn func()) {
	a.deferMu.Lock()
	if a.drained {
		a.deferMu.Unlock()
		fn()
		return
	}
	a.deferred = append(a.deferred, fn)
	a.deferMu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// takeDeferred returns the pending AfterQueued callbacks; final marks the
// collector drained so later callbacks run on their caller.
func (a *Async) takeDeferred(final bool) []func() {
	a.deferMu.Lock()
	defer a.deferMu.Unlock()
	pending := a.deferred
	a.deferred = nil
	if final {
		a.drained = true
	}
	return pending
}

// fanout offers an admitted event to the additional sinks of its route. A
// full or retired sink queue never delays the collector. Under privacy
// nothing no per-query fact leaves the node, the rule the live tail follows.
func (a *Async) fanout(dst *destination, ev *Event, privacy *Privacy) {
	if dst.sinks == nil {
		return
	}
	mode := PrivacyFull
	if privacy != nil {
		mode = privacy.Mode()
	}
	if !mode.StoresFacts() {
		return
	}
	for _, w := range dst.sinks.workers {
		w.offer(sinkItem{event: ev, mode: mode})
	}
}

// Close flushes pending events and stops the writer.
func (a *Async) Close(ctx context.Context) error {
	a.stopOnce.Do(func() {
		a.admission.Lock()
		a.closed = true
		close(a.stop)
		a.admission.Unlock()
		go func() {
			a.wg.Wait()
			a.cancelWrite()
			a.closeErr = a.sink.Close()
			close(a.done)
		}()
	})
	select {
	case <-a.done:
		return a.closeErr
	case <-ctx.Done():
		a.cancelWrite()
		return ctx.Err()
	}
}

// admit applies privacy, stamps the id and publishes to the tail. It
// returns false for excluded events.
func (a *Async) admit(ev *Event) bool {
	return a.admitWith(ev, a.privacy.Load())
}

func (a *Async) admitWith(ev *Event, privacy *Privacy) bool {
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
	var current *destination
	var privacy *Privacy
	flush := func() {
		if len(batch) == 0 {
			return
		}
		var err error
		if sink, ok := current.sink.(BatchSink); ok {
			mode := PrivacyFull
			if privacy != nil {
				mode = privacy.Mode()
			}
			err = sink.WriteBatch(a.writeCtx, batch, mode)
		} else {
			err = current.sink.Write(batch)
		}
		if err != nil {
			a.metrics.Drop(DropStageSink, current.sink.Name(), len(batch))
			a.logger.Error("query log sink write failed", "sink", current.sink.Name(), "err", err, "events", len(batch))
		}
		// Sinks may keep references to the events (they are immutable),
		// never to the slice, which is reused.
		clear(batch)
		batch = batch[:0]
	}
	// Barriers order AfterQueued callbacks behind the events that were
	// queued when the collector saw them. The queue is FIFO and this is its
	// only reader, so counting admitted events is exact.
	var barriers []barrier
	arm := func() {
		for _, fn := range a.takeDeferred(false) {
			if n := len(a.queue); n > 0 {
				barriers = append(barriers, barrier{remaining: n, fn: fn})
			} else {
				fn()
			}
		}
	}
	passed := func() {
		if len(barriers) == 0 {
			return
		}
		kept := barriers[:0]
		for _, b := range barriers {
			if b.remaining--; b.remaining > 0 {
				kept = append(kept, b)
			} else {
				b.fn()
			}
		}
		clear(barriers[len(kept):])
		barriers = kept
	}
	add := func(item queuedEvent) {
		if current != item.destination {
			flush()
			current, privacy = item.destination, item.destination.privacy
			a.nextID = max(a.nextID, current.firstID)
		}
		if a.admitWith(item.event, privacy) {
			batch = append(batch, item.event)
			a.fanout(current, item.event, privacy)
		}
		passed()
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
		case <-a.wake:
			arm()
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
					// Admission is closed and the queue is empty: every
					// accepted event has been offered to its sinks.
					for _, b := range barriers {
						b.fn()
					}
					for _, fn := range a.takeDeferred(true) {
						fn()
					}
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
