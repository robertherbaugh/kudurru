package audit

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Source is a node-local store events are shipped from (the audit spool):
// ids only increase, and ReadAfter returns events oldest first.
type Source interface {
	// SourceID identifies the store; a new file gets a new one.
	SourceID() string
	// Node is the node that records into the store.
	Node() string
	ReadAfter(ctx context.Context, after int64, n int) ([]Event, error)
}

// Target is a cluster-wide store events are shipped to (the AuditLog CRD,
// a central database). It keys events by (Source.SourceID, Event.ID).
type Target interface {
	// Cursor returns the id of the last event of src the target holds; 0
	// when it holds none.
	Cursor(ctx context.Context, src Source) (int64, error)
	// Append stores evs, the events of src after the cursor, oldest first.
	// A failed Append may have stored some of them: the shipper reads the
	// cursor again before it retries.
	Append(ctx context.Context, src Source, evs []Event) error
}

// Shipper defaults.
const (
	DefaultShipBatch    = 500
	DefaultShipInterval = 2 * time.Second
	DefaultShipDelay    = 5 * time.Second
	DefaultShipMinGap   = 30 * time.Second
	DefaultShipBackoff  = time.Minute
)

// Shipper forwards the events of a Source to a Target: at least once,
// in order, and without loss while the target is down (the source keeps
// them). It batches: a batch waits until its oldest event is Delay old
// and MinGap has passed since the last Append, unless Batch events are
// waiting. It is an always-on runnable; target failures are retried with
// backoff and never end it.
type Shipper struct {
	Source Source
	Target Target
	// Batch is the most events per Append (default DefaultShipBatch).
	Batch int
	// Interval is how often the source is polled (default
	// DefaultShipInterval); Delay and MinGap shape batches (defaults
	// DefaultShipDelay, DefaultShipMinGap); MaxBackoff caps the retry
	// delay (default DefaultShipBackoff).
	Interval, Delay, MinGap, MaxBackoff time.Duration
	Logger                              *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time

	mu      sync.Mutex
	shipped int64 // the target's cursor, when known
	known   bool
}

// Run ships until ctx ends, then ships what is waiting once more.
func (s *Shipper) Run(ctx context.Context) error {
	s.defaults()
	log := s.Logger.With("component", "audit", "source", s.Source.SourceID())
	backoff := time.Duration(0)
	var lastAppend time.Time
	for {
		wait := s.Interval
		shipped, err := s.cursor(ctx)
		if err == nil {
			var evs []Event
			evs, err = s.Source.ReadAfter(ctx, shipped, s.Batch)
			switch {
			case err != nil:
			case len(evs) == 0:
			case len(evs) < s.Batch && (s.Now().Sub(evs[0].Time) < s.Delay || s.Now().Sub(lastAppend) < s.MinGap):
				// Let the batch fill.
			default:
				if err = s.Target.Append(ctx, s.Source, evs); err == nil {
					s.setCursor(evs[len(evs)-1].ID)
					lastAppend = s.Now()
					if len(evs) == s.Batch {
						wait = 0
					}
				} else {
					s.forget()
				}
			}
		}
		if err != nil && ctx.Err() == nil {
			backoff = min(max(2*backoff, time.Second), s.MaxBackoff)
			wait = backoff
			log.Warn("audit shipping failed; retrying", "err", err, "retry_in", backoff)
		} else if err == nil {
			backoff = 0
		}
		select {
		case <-ctx.Done():
			s.drain(log)
			return nil
		case <-time.After(wait):
		}
	}
}

// drain ships the waiting events once, with a short deadline, when the
// process stops (lameduck): batching rules do not apply.
func (s *Shipper) drain(log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		shipped, err := s.cursor(ctx)
		if err != nil {
			return
		}
		evs, err := s.Source.ReadAfter(ctx, shipped, s.Batch)
		if err != nil || len(evs) == 0 {
			return
		}
		if err := s.Target.Append(ctx, s.Source, evs); err != nil {
			log.Warn("audit shipping at shutdown failed; the events stay in the spool", "err", err)
			return
		}
		s.setCursor(evs[len(evs)-1].ID)
	}
}

// Pending returns the source's events the target does not hold yet,
// oldest first, at most n: a reader of the target adds them so the node
// that recorded an event shows it before it is shipped. It returns none
// until the target's cursor has been read once.
func (s *Shipper) Pending(ctx context.Context, n int) ([]Event, error) {
	s.mu.Lock()
	shipped, known := s.shipped, s.known
	s.mu.Unlock()
	if !known {
		return nil, nil
	}
	return s.Source.ReadAfter(ctx, shipped, n)
}

func (s *Shipper) cursor(ctx context.Context) (int64, error) {
	s.mu.Lock()
	shipped, known := s.shipped, s.known
	s.mu.Unlock()
	if known {
		return shipped, nil
	}
	c, err := s.Target.Cursor(ctx, s.Source)
	if err != nil {
		return 0, err
	}
	s.setCursor(c)
	return c, nil
}

func (s *Shipper) setCursor(c int64) {
	s.mu.Lock()
	s.shipped, s.known = c, true
	s.mu.Unlock()
}

// forget drops the cursor after a failed Append, which may have stored
// part of the batch: the next round reads it from the target.
func (s *Shipper) forget() {
	s.mu.Lock()
	s.known = false
	s.mu.Unlock()
}

func (s *Shipper) defaults() {
	if s.Batch <= 0 {
		s.Batch = DefaultShipBatch
	}
	if s.Interval <= 0 {
		s.Interval = DefaultShipInterval
	}
	if s.Delay < 0 {
		s.Delay = 0
	} else if s.Delay == 0 {
		s.Delay = DefaultShipDelay
	}
	if s.MinGap < 0 {
		s.MinGap = 0
	} else if s.MinGap == 0 {
		s.MinGap = DefaultShipMinGap
	}
	if s.MaxBackoff <= 0 {
		s.MaxBackoff = DefaultShipBackoff
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
}
