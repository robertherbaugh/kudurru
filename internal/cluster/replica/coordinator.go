package replica

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ServingPin prevents publication changing during eligibility/CAS. Root's
// complete bundle publisher supplies it; nil never grants promotion eligibility.
type ServingPin func(context.Context) (replication.Generation, func(), error)

// CoordinatorOptions supplies pull scheduling, serving pins and operator policy.
type CoordinatorOptions struct {
	Store             *sqlite.Store
	Source            follower.Source
	NodeName          string
	Interval, Timeout time.Duration
	Served            ServingPin
	AuthorizeOperator func(context.Context) error
	// Elapsed is a monotonic duration since coordinator startup, not wall time.
	// Default time.Since(start), including Go's monotonic clock. Tests inject it.
	Elapsed func() time.Duration
	WallNow func() time.Time
}

// Coordinator tracks monotonic pull loss and gates explicit store promotion CAS.
type Coordinator struct {
	opts                      CoordinatorOptions
	puller                    *follower.Puller
	mu                        sync.Mutex
	window                    bool
	since, lastMiss, lastTick time.Duration
	misses                    int
	reason                    string
}

// CoordinatorStatus distinguishes installed data, served data and reachability.
type CoordinatorStatus struct {
	Authority         replication.Authority
	Installed, Served replication.Generation
	LastContact       time.Time
	UnreachableFor    time.Duration
	MissedPulls       int
	Eligible          bool
	Reason            string
}

// PromoteRequest confirms a node and the exact operator-reviewed installed cut.
type PromoteRequest struct {
	ConfirmNode       string
	ExpectedTerm      replication.Term
	ExpectedInstalled replication.Generation
	RequestID         string
}

// NewCoordinator begins a fresh missed-pull window regardless of persisted state.
func NewCoordinator(o CoordinatorOptions) (*Coordinator, error) {
	if o.Store == nil || o.Source == nil || o.NodeName == "" || o.Served == nil || o.AuthorizeOperator == nil {
		return nil, errors.New("replica: store, source, node, serving pin and operator authorizer required")
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.Interval > time.Duration(1<<63-1)/2 {
		return nil, ErrProtocol
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Elapsed == nil {
		start := time.Now()
		o.Elapsed = func() time.Duration { return time.Since(start) }
	}
	if o.WallNow == nil {
		o.WallNow = time.Now
	}
	return &Coordinator{opts: o, puller: &follower.Puller{Mirror: o.Store, Source: o.Source, Interval: o.Interval, Timeout: o.Timeout, Now: o.WallNow}}, nil
}
func (c *Coordinator) tickLocked() time.Duration {
	now := c.opts.Elapsed()
	if now < 0 || now < c.lastTick {
		c.window = false
		c.misses = 0
		c.reason = "monotonic_clock_reset"
		now = max(now, 0)
	}
	c.lastTick = now
	return now
}

// Pull serializes installs and updates continuous network-loss eligibility.
func (c *Coordinator) Pull(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	err := c.puller.Pull(ctx)
	now := c.tickLocked()
	var exchange *ExchangeError
	reached := errors.As(err, &exchange) && exchange.Reached
	switch {
	case err == nil || reached:
		c.window = false
		c.misses = 0
		c.reason = ""
		if err != nil {
			c.reason = "replication_rejected"
		}
	case errors.Is(err, context.Canceled):
		// Cancellation says nothing about reachability; do not grant eligibility.
		c.window = false
		c.misses = 0
		c.reason = "pull_cancelled"
	case errors.Is(err, ErrUnavailable):
		if !c.window {
			c.window = true
			c.since = now
			c.lastMiss = now
			c.misses = 1
		} else if now-c.lastMiss >= c.opts.Interval {
			c.lastMiss = now
			c.misses++
		}
		c.reason = "primary_unreachable"
	default:
		c.window = false
		c.misses = 0
		c.reason = "replication_rejected"
	}
	return err
}
func (c *Coordinator) statusLocked(ctx context.Context, served replication.Generation, pinErr error) CoordinatorStatus {
	now := c.tickLocked()
	s := CoordinatorStatus{Authority: c.opts.Store.Authority(), Served: served, LastContact: c.puller.Status().LastContact, MissedPulls: c.misses, Reason: c.reason}
	if c.window {
		s.UnreachableFor = now - c.since
	}
	image, err := c.opts.Store.ReadGeneration(ctx)
	if err == nil {
		s.Installed = image.Generation()
	}
	switch {
	case s.Authority.Mode != replication.Follower:
		s.Reason = "not_follower"
	case err != nil || !s.Installed.Term.Same(s.Authority.Term):
		s.Reason = "no_installed_image"
	case s.Authority.Conflict != nil || s.Authority.Divergence != nil || c.opts.Store.Info().Fenced:
		s.Reason = "fenced"
	case pinErr != nil || !s.Served.Same(s.Installed):
		s.Reason = "serving_generation_pending"
	case !c.window || c.misses < 2 || s.UnreachableFor < 2*c.opts.Interval:
		s.Reason = "awaiting_missed_pulls"
	default:
		s.Eligible = true
		s.Reason = "eligible"
	}
	return s
}

// Status evaluates eligibility while the serving generation is pinned.
func (c *Coordinator) Status(ctx context.Context) CoordinatorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	served, release, err := c.opts.Served(ctx)
	if release != nil {
		defer release()
	}
	if release == nil && err == nil {
		err = ErrIneligible
	}
	return c.statusLocked(ctx, served, err)
}

// Promote requires current authorization, serving agreement, network loss and CAS.
func (c *Coordinator) Promote(ctx context.Context, req PromoteRequest) (replication.Authority, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.opts.AuthorizeOperator(ctx); err != nil {
		return replication.Authority{}, err
	}
	if req.ConfirmNode != c.opts.NodeName {
		return replication.Authority{}, ErrIneligible
	}
	a := c.opts.Store.Authority()
	storeRequest := replication.PromoteRequest{ExpectedTerm: req.ExpectedTerm, ExpectedInstalled: req.ExpectedInstalled, RequestID: req.RequestID}
	if a.Mode == replication.Primary && a.LastPromotionRequest == req.RequestID && a.Parent != nil && a.Parent.Same(req.ExpectedInstalled) && a.Parent.Term.Same(req.ExpectedTerm) {
		return c.opts.Store.Promote(ctx, storeRequest)
	}
	served, release, err := c.opts.Served(ctx)
	if release != nil {
		defer release()
	}
	if release == nil && err == nil {
		err = ErrIneligible
	}
	status := c.statusLocked(ctx, served, err)
	if !status.Eligible {
		return a, ErrIneligible
	}
	if !status.Authority.Term.Same(req.ExpectedTerm) || !status.Installed.Same(req.ExpectedInstalled) {
		return a, replication.ErrCAS
	}
	return c.opts.Store.Promote(ctx, storeRequest)
}

// Run pulls until cancellation; rejected exchanges remain visible through Status.
func (c *Coordinator) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = c.Pull(ctx)
		timer := time.NewTimer(c.opts.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
