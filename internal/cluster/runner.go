// Package cluster runs the node's runnables and owns the leader-election
// loop (dev-docs/design-record.md §3.1, §5.3, decisions D44 and D78).
//
// A Runner holds two groups. Always-on runnables (dnsserver, zonecache,
// api, querylog, ops) start as soon as Run is called, on every replica.
// Leader-only runnables (controller, listfetcher, notifier, ...) start as a
// group under a term context when the store's Elector reports a new term and
// are cancelled when the term ends. Losing leadership stops only the
// leader-only group; the process never exits on leadership loss, and a new
// term restarts the group from scratch, which is safe because leader-only
// work is idempotent and level-triggered (§4.6).
//
// Shutdown runs in a fixed order: lameduck (readiness fails, the node
// drains), then the leader group is cancelled and the lease released, then
// the always-on group is cancelled.
//
// Membership, scatter-gather and the internal listener (§3.4) join this
// package in later milestones.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// ElectionController is the election every v0.1 leader-only runnable shares:
// Store.Elector(ElectionController) is the Lease "kudurru-controller" in the
// kube driver (D78).
const ElectionController = "controller"

// Defaults for Options.
const (
	DefaultRestartBackoff    = time.Second
	DefaultMaxRestartBackoff = 30 * time.Second
	DefaultShutdownTimeout   = 30 * time.Second
	// stuckWarnInterval is how often a runnable that ignores its cancelled
	// context is reported while the Runner waits for it.
	stuckWarnInterval = 10 * time.Second
)

// RunFunc is a runnable's body. It runs until ctx is cancelled and should
// return nil then. Returning early with an error is fatal for an always-on
// runnable and triggers a restart with backoff for a leader-only one;
// returning early with nil means the work is finished.
type RunFunc func(ctx context.Context) error

// Options configures a Runner.
type Options struct {
	// Elector is the election that gates leader-only runnables, normally
	// st.Elector(ElectionController). It is required only when a
	// leader-only runnable is added; without leader-only runnables the
	// Runner never campaigns.
	Elector store.Elector
	// ElectionName labels kudurru_leader{role}. Default ElectionController.
	ElectionName string
	// Lameduck runs first on shutdown, before anything is cancelled: flip
	// readiness (health.Checker.Lameduck) here. It may block to drain; it
	// receives a context bounded by ShutdownTimeout.
	Lameduck func(ctx context.Context)
	// LameduckDuration is waited after Lameduck returns so load balancers
	// observe the failing readiness probe before listeners close.
	LameduckDuration time.Duration
	// ShutdownTimeout bounds how long Run waits for each group to stop
	// after cancelling it; a runnable that ignores its context is reported
	// and abandoned after this. Default DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// RestartBackoff and MaxRestartBackoff bound the exponential backoff
	// between restarts of a failing leader-only runnable within one term.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	// Registerer receives kudurru_leader and kudurru_role_active; nil
	// leaves them unregistered.
	Registerer prometheus.Registerer
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// RunnableStatus is a point-in-time view of one runnable.
type RunnableStatus struct {
	Name       string
	LeaderOnly bool
	Active     bool
	Restarts   int
	LastError  string
}

// runnable is one registered unit of work.
type runnable struct {
	name       string
	leaderOnly bool
	fn         RunFunc

	mu       sync.Mutex
	active   bool
	restarts int
	lastErr  string
}

// Runner schedules always-on and leader-only runnables. Construct it with
// New, register runnables with Add, then call Run once.
type Runner struct {
	opts    Options
	log     *slog.Logger
	metrics *runnerMetrics

	mu        sync.Mutex
	started   bool
	runnables []*runnable
	names     map[string]bool

	leading atomic.Bool
	terms   atomic.Int64
}

// New returns a Runner. It does no I/O.
func New(opts Options) (*Runner, error) {
	if opts.ElectionName == "" {
		opts.ElectionName = ElectionController
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = DefaultShutdownTimeout
	}
	if opts.RestartBackoff <= 0 {
		opts.RestartBackoff = DefaultRestartBackoff
	}
	if opts.MaxRestartBackoff < opts.RestartBackoff {
		opts.MaxRestartBackoff = max(DefaultMaxRestartBackoff, opts.RestartBackoff)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	m, err := newRunnerMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	m.leader.WithLabelValues(opts.ElectionName).Set(0)
	return &Runner{
		opts:    opts,
		log:     opts.Logger.With("component", "cluster/runner", "election", opts.ElectionName),
		metrics: m,
		names:   map[string]bool{},
	}, nil
}

// Add registers a runnable. Names must be unique; they label
// kudurru_role_active{role}. Add fails once Run has been called.
func (r *Runner) Add(name string, leaderOnly bool, fn RunFunc) error {
	if name == "" || fn == nil {
		return errors.New("cluster: Add needs a name and a function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("cluster: Add(%q) after Run", name)
	}
	if r.names[name] {
		return fmt.Errorf("cluster: runnable %q added twice", name)
	}
	if leaderOnly && r.opts.Elector == nil {
		return fmt.Errorf("cluster: leader-only runnable %q needs Options.Elector", name)
	}
	r.names[name] = true
	r.runnables = append(r.runnables, &runnable{name: name, leaderOnly: leaderOnly, fn: fn})
	r.metrics.active.WithLabelValues(name).Set(0)
	return nil
}

// IsLeader reports whether this process is in a leader term.
func (r *Runner) IsLeader() bool { return r.leading.Load() }

// Terms returns how many leader terms this Runner has started.
func (r *Runner) Terms() int64 { return r.terms.Load() }

// Status returns the state of every runnable in registration order.
func (r *Runner) Status() []RunnableStatus {
	r.mu.Lock()
	rs := append([]*runnable(nil), r.runnables...)
	r.mu.Unlock()
	out := make([]RunnableStatus, 0, len(rs))
	for _, rn := range rs {
		rn.mu.Lock()
		out = append(out, RunnableStatus{Name: rn.name, LeaderOnly: rn.leaderOnly, Active: rn.active, Restarts: rn.restarts, LastError: rn.lastErr})
		rn.mu.Unlock()
	}
	return out
}

// Run starts the always-on group, campaigns for leadership when leader-only
// runnables exist, and blocks until ctx is cancelled or an always-on
// runnable fails. It then shuts down in order (lameduck, leader group,
// always-on group) and returns the first always-on failure, or nil. Run may
// be called once.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("cluster: Run called twice")
	}
	r.started = true
	var always, leader []*runnable
	for _, rn := range r.runnables {
		if rn.leaderOnly {
			leader = append(leader, rn)
		} else {
			always = append(always, rn)
		}
	}
	r.mu.Unlock()

	fatal := make(chan error, len(always)+1)

	// Always-on group: its own context, cancelled last.
	alwaysCtx, cancelAlways := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelAlways()
	var alwaysWG sync.WaitGroup
	for _, rn := range always {
		alwaysWG.Add(1)
		go func() {
			defer alwaysWG.Done()
			if err := r.runAlways(alwaysCtx, rn); err != nil {
				fatal <- err
			}
		}()
	}

	// Election: its own context, cancelled after lameduck.
	electCtx, cancelElect := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelElect()
	electDone := make(chan struct{})
	if len(leader) > 0 {
		go func() {
			defer close(electDone)
			if err := r.campaign(electCtx, leader); err != nil {
				fatal <- err
			}
		}()
	} else {
		close(electDone)
	}

	var firstErr error
	select {
	case <-ctx.Done():
		r.log.Info("shutting down")
	case firstErr = <-fatal:
		r.log.Error("runnable failed; shutting down", "err", firstErr)
	}

	// 1. Lameduck: readiness fails while everything still serves.
	if r.opts.Lameduck != nil {
		lctx, cancel := context.WithTimeout(context.Background(), r.opts.ShutdownTimeout)
		r.opts.Lameduck(lctx)
		cancel()
	}
	if r.opts.LameduckDuration > 0 {
		r.log.Info("lameduck", "duration", r.opts.LameduckDuration)
		time.Sleep(r.opts.LameduckDuration)
	}
	// 2. Leader group: cancel the election; the elector ends the term,
	// waits for the group, then releases the lease.
	cancelElect()
	r.wait("leader group", electDone)
	// 3. Always-on group.
	cancelAlways()
	alwaysDone := make(chan struct{})
	go func() { alwaysWG.Wait(); close(alwaysDone) }()
	r.wait("always-on group", alwaysDone)
	r.log.Info("stopped")
	return firstErr
}

// wait blocks until done or ShutdownTimeout, logging stragglers.
func (r *Runner) wait(what string, done <-chan struct{}) {
	t := time.NewTimer(r.opts.ShutdownTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		r.log.Error("did not stop within the shutdown timeout; abandoning it", "group", what, "timeout", r.opts.ShutdownTimeout, "status", r.Status())
	}
}

// runAlways runs one always-on runnable once. An early return with an
// error, or a panic, is reported as fatal.
func (r *Runner) runAlways(ctx context.Context, rn *runnable) error {
	err := r.invoke(ctx, rn)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("%s: %w", rn.name, err)
	}
	if err == nil && ctx.Err() == nil {
		r.log.Info("always-on runnable finished", "role", rn.name)
	}
	return nil
}

// campaign runs the election loop until ctx ends. Elector.Run is expected
// to return only when ctx is done; an early error (e.g. an invalid lease
// name) is fatal, an early nil re-enters the election after a backoff.
func (r *Runner) campaign(ctx context.Context, group []*runnable) error {
	e := r.opts.Elector
	for {
		err := e.Run(ctx, func(term context.Context) { r.lead(term, group) }, func() {
			r.leading.Store(false)
			r.metrics.leader.WithLabelValues(r.opts.ElectionName).Set(0)
			r.log.Info("leader term ended; leader-only runnables stopped")
		})
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("election %s: %w", r.opts.ElectionName, err)
		}
		r.log.Warn("elector returned before shutdown; re-entering the election", "after", r.opts.RestartBackoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.opts.RestartBackoff):
		}
	}
}

// lead runs the leader-only group for one term and returns when every
// runnable has stopped. It blocks past the end of the term until the group
// has stopped, so terms never overlap in this process.
func (r *Runner) lead(term context.Context, group []*runnable) {
	n := r.terms.Add(1)
	r.leading.Store(true)
	r.metrics.leader.WithLabelValues(r.opts.ElectionName).Set(1)
	r.log.Info("leader term started; starting leader-only runnables", "term", n, "runnables", len(group))
	var wg sync.WaitGroup
	for _, rn := range group {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.superviseLeader(term, rn)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// Every runnable finished on its own; keep the term (and the
		// lease) until it ends so finished work is not redone elsewhere.
		<-term.Done()
		return
	case <-term.Done():
	}
	tick := time.NewTicker(stuckWarnInterval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			r.log.Error("leader-only runnables ignore the end of the term; the next term waits for them", "status", r.Status())
		}
	}
}

// superviseLeader runs a leader-only runnable for the term, restarting it
// with exponential backoff when it fails or panics.
func (r *Runner) superviseLeader(term context.Context, rn *runnable) {
	backoff := r.opts.RestartBackoff
	for {
		err := r.invoke(term, rn)
		if term.Err() != nil {
			return
		}
		if err == nil {
			r.log.Info("leader-only runnable finished", "role", rn.name)
			return
		}
		rn.mu.Lock()
		rn.restarts++
		rn.mu.Unlock()
		r.log.Error("leader-only runnable failed; restarting", "role", rn.name, "err", err, "after", backoff)
		select {
		case <-term.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, r.opts.MaxRestartBackoff)
	}
}

// invoke runs fn once, marking the runnable active and converting a panic
// into an error: a bug in one runnable must not crash the dataplane (D44).
func (r *Runner) invoke(ctx context.Context, rn *runnable) (err error) {
	rn.mu.Lock()
	rn.active = true
	rn.mu.Unlock()
	r.metrics.active.WithLabelValues(rn.name).Set(1)
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			r.log.Error("runnable panicked", "role", rn.name, "panic", p, "stack", string(debug.Stack()))
		}
		rn.mu.Lock()
		rn.active = false
		if err != nil && !errors.Is(err, context.Canceled) {
			rn.lastErr = err.Error()
		}
		rn.mu.Unlock()
		r.metrics.active.WithLabelValues(rn.name).Set(0)
	}()
	return rn.fn(ctx)
}

// runnerMetrics are the Runner's gauges.
type runnerMetrics struct {
	leader *prometheus.GaugeVec
	active *prometheus.GaugeVec
}

func newRunnerMetrics(reg prometheus.Registerer) (*runnerMetrics, error) {
	m := &runnerMetrics{
		leader: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_leader",
			Help: "1 while this node holds the named election's leader term (role = election name, e.g. controller).",
		}, []string{"role"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_role_active",
			Help: "1 while the named runnable is running on this node.",
		}, []string{"role"}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.leader, m.active} {
			if err := reg.Register(c); err != nil {
				return nil, fmt.Errorf("cluster: register metrics: %w", err)
			}
		}
	}
	return m, nil
}
