package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// ElectorConfig configures a LeaseElector.
type ElectorConfig struct {
	// Namespace and Name locate the coordination.k8s.io/v1 Lease.
	Namespace string
	Name      string
	// Identity is this candidate's holder identity; it must be unique among
	// the candidates (the pod name in Kubernetes).
	Identity string
	// Election timings (§5.3: 15 s / 10 s / 2 s).
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
	// KeepLeaseOnShutdown disables the release of a held Lease when Run's
	// context ends. By default the Lease is released, after the leader term
	// has fully stopped, so a successor does not wait for expiry.
	KeepLeaseOnShutdown bool
	// Logger receives election logs. Default slog.Default().
	Logger *slog.Logger
}

// LeaseElector is the kube driver's store.Elector: a client-go
// leaderelection.LeaderElector over a Lease, run in a loop whose lifetime
// is the caller's context, not the process and not the lease (D44).
//
// Each successful acquisition starts a term. The term's context is
// cancelled when the lease is lost or Run's context ends; onStopped then
// fires, and on lease loss the elector re-enters the election after
// RetryPeriod. Losing the lease never returns from Run and never exits the
// process: Run returns only when its context is cancelled.
type LeaseElector struct {
	cfg    ElectorConfig
	leases coordinationv1client.LeasesGetter
	log    *slog.Logger

	running atomic.Bool
	leading atomic.Bool
	current atomic.Pointer[leaderelection.LeaderElector]

	mu       sync.Mutex
	shutdown bool  // set once Run's context is done; no new term may start
	term     *term // the active term, if any
}

// term is one leadership period.
type term struct {
	cancel context.CancelFunc
	done   chan struct{} // closed after onStopped returned
}

var _ store.Elector = (*LeaseElector)(nil)

// NewLeaseElector validates cfg and returns an elector. It does no I/O.
func NewLeaseElector(leases coordinationv1client.LeasesGetter, cfg ElectorConfig) (*LeaseElector, error) {
	if leases == nil {
		return nil, errors.New("store/kube: elector needs a Leases client")
	}
	if cfg.Namespace == "" || cfg.Identity == "" {
		return nil, errors.New("store/kube: elector needs a namespace and an identity")
	}
	if errs := validation.IsDNS1123Subdomain(cfg.Name); len(errs) > 0 {
		return nil, fmt.Errorf("store/kube: invalid lease name %q: %v", cfg.Name, errs)
	}
	if cfg.LeaseDuration <= cfg.RenewDeadline {
		return nil, fmt.Errorf("store/kube: lease duration %s must exceed renew deadline %s", cfg.LeaseDuration, cfg.RenewDeadline)
	}
	if float64(cfg.RenewDeadline) <= leaderelection.JitterFactor*float64(cfg.RetryPeriod) || cfg.RetryPeriod <= 0 {
		return nil, fmt.Errorf("store/kube: renew deadline %s must exceed %.1f x retry period %s", cfg.RenewDeadline, leaderelection.JitterFactor, cfg.RetryPeriod)
	}
	if cfg.LeaseDuration < time.Second {
		return nil, fmt.Errorf("store/kube: lease duration %s is below the Lease's one-second resolution", cfg.LeaseDuration)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &LeaseElector{
		cfg:    cfg,
		leases: leases,
		log:    cfg.Logger.With("component", "store/kube/elector", "lease", cfg.Namespace+"/"+cfg.Name, "identity", cfg.Identity),
	}, nil
}

// Elector implements store.Store. Names map to Leases as
// Options.LeasePrefix+name in Options.Namespace ("controller" →
// "kudurru-controller"); the same name always returns the same elector.
// An invalid name yields an elector whose Run returns the validation error.
func (s *Store) Elector(name string) store.Elector {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.electors[name]; ok {
		return e
	}
	e, err := NewLeaseElector(s.leases, ElectorConfig{
		Namespace:     s.opts.Namespace,
		Name:          s.opts.LeasePrefix + name,
		Identity:      s.opts.Identity,
		LeaseDuration: s.opts.LeaseDuration,
		RenewDeadline: s.opts.RenewDeadline,
		RetryPeriod:   s.opts.RetryPeriod,
		Logger:        s.opts.Logger,
	})
	if err != nil {
		return brokenElector{err: err}
	}
	s.electors[name] = e
	return e
}

// IsLeader reports whether a term is active: true from just before
// onStarted until just before onStopped.
func (e *LeaseElector) IsLeader() bool { return e.leading.Load() }

// Leader returns the last observed holder identity, or "".
func (e *LeaseElector) Leader() string {
	if le := e.current.Load(); le != nil {
		return le.GetLeader()
	}
	return ""
}

// Run implements store.Elector. onStarted is called when a term starts and
// must not block for long; onStopped is called when it ends (lease lost or
// ctx done) and must stop the leader-only work before returning, because
// a successor may start once the lease expires. Either callback may be nil.
// Run returns nil once ctx is cancelled, or an error immediately if the
// elector is already running.
func (e *LeaseElector) Run(ctx context.Context, onStarted, onStopped func()) error {
	return e.RunWithContext(ctx, func(context.Context) {
		if onStarted != nil {
			onStarted()
		}
	}, onStopped)
}

// RunWithContext is Run with a term context: onStarted receives a context
// that is cancelled when the term ends and may block until then (the shape
// of a leader-only runnable group). onStopped, if non-nil, runs after
// onStarted has returned and the term context is done. The next election
// attempt starts only after onStopped returns, and on shutdown the Lease is
// released only after it returns, so two terms never overlap in this
// process.
func (e *LeaseElector) RunWithContext(ctx context.Context, onStarted func(context.Context), onStopped func()) error {
	if onStarted == nil {
		return errors.New("store/kube: RunWithContext needs onStarted")
	}
	if !e.running.CompareAndSwap(false, true) {
		return errors.New("store/kube: elector is already running")
	}
	defer e.running.Store(false)
	e.mu.Lock()
	e.shutdown = false
	e.mu.Unlock()

	// The election runs under its own context so that, on shutdown, the
	// renew loop keeps the lease until the term has stopped.
	electionCtx, cancelElection := context.WithCancel(context.Background())
	defer cancelElection()
	stopWatch := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.shutdown = true
		t := e.term
		e.mu.Unlock()
		if t != nil {
			t.cancel()
			<-t.done
		}
		cancelElection()
	})
	defer stopWatch()

	heldAtShutdown := false
	for electionCtx.Err() == nil {
		res, err := e.runOnce(ctx, electionCtx, onStarted, onStopped)
		if err != nil {
			// Only reachable through an invalid config, which
			// NewLeaseElector rejects; keep the contract by waiting.
			e.log.Error("election setup failed", "err", err)
		}
		if electionCtx.Err() != nil {
			heldAtShutdown = res.held
			break
		}
		wait := e.cfg.RetryPeriod
		if res.steppedDown {
			// A panicking term gives the lease away and sits out one
			// lease duration so another replica can take over.
			e.release()
			wait = e.cfg.LeaseDuration
		}
		e.log.Info("left the leader term; re-entering the election", "after", wait)
		select {
		case <-electionCtx.Done():
		case <-time.After(wait):
		}
	}
	// Wait for the AfterFunc (term stop) to finish before releasing.
	<-electionCtx.Done()
	e.mu.Lock()
	t := e.term
	e.mu.Unlock()
	if t != nil {
		<-t.done
	}
	if heldAtShutdown && !e.cfg.KeepLeaseOnShutdown {
		e.release()
	}
	return nil
}

// onceResult describes how one election round ended.
type onceResult struct {
	held        bool // this candidate held the lease when the round ended
	steppedDown bool // the term panicked and the round was abandoned
}

// runOnce runs one client-go election round: acquire, then lead until the
// lease is lost, the term panics, or electionCtx ends.
func (e *LeaseElector) runOnce(ctx, electionCtx context.Context, onStarted func(context.Context), onStopped func()) (onceResult, error) {
	acquired := make(chan struct{})
	termDone := make(chan struct{})
	roundCtx, abandon := context.WithCancel(electionCtx)
	defer abandon()
	var panicked atomic.Bool
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Namespace: e.cfg.Namespace, Name: e.cfg.Name},
			Client:     e.leases,
			LockConfig: resourcelock.ResourceLockConfig{Identity: e.cfg.Identity},
		},
		LeaseDuration: e.cfg.LeaseDuration,
		RenewDeadline: e.cfg.RenewDeadline,
		RetryPeriod:   e.cfg.RetryPeriod,
		// Never release from inside client-go: it would release while the
		// term may still be running and, after a lost lease, could clear a
		// successor's record. Release happens in RunWithContext.
		ReleaseOnCancel: false,
		Name:            e.cfg.Name,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leCtx context.Context) {
				close(acquired)
				defer close(termDone)
				if e.runTerm(ctx, leCtx, onStarted, onStopped) {
					panicked.Store(true)
					abandon()
				}
			},
			// The term goroutine owns onStopped; see runTerm.
			OnStoppedLeading: func() {},
			OnNewLeader: func(identity string) {
				e.log.Info("observed leader", "leader", identity)
			},
		},
	})
	if err != nil {
		return onceResult{}, err
	}
	e.current.Store(le)
	le.Run(roundCtx)
	select {
	case <-acquired:
	default:
		if roundCtx.Err() != nil {
			// Shutdown before (or racing with) acquisition: a pending
			// OnStartedLeading sees e.shutdown and starts no term.
			return onceResult{held: le.IsLeader()}, nil
		}
		// acquire only gives up when its context ends, so the lease was
		// acquired and OnStartedLeading is about to run.
	}
	<-termDone
	return onceResult{held: true, steppedDown: panicked.Load()}, nil
}

// runTerm runs one leadership term on client-go's OnStartedLeading
// goroutine and reports whether onStarted panicked.
func (e *LeaseElector) runTerm(callerCtx, leCtx context.Context, onStarted func(context.Context), onStopped func()) (panicked bool) {
	e.mu.Lock()
	if e.shutdown || leCtx.Err() != nil {
		e.mu.Unlock()
		return false
	}
	termCtx, cancel := context.WithCancel(leCtx)
	t := &term{cancel: cancel, done: make(chan struct{})}
	e.term = t
	e.leading.Store(true)
	e.mu.Unlock()
	stop := context.AfterFunc(callerCtx, cancel)

	defer func() {
		stop()
		cancel()
		e.mu.Lock()
		if e.term == t {
			e.term = nil
		}
		e.mu.Unlock()
		close(t.done)
	}()

	e.log.Info("started leading")
	if panicked = e.guard("onStarted", func() { onStarted(termCtx) }); panicked {
		cancel()
	}
	<-termCtx.Done()
	e.leading.Store(false)
	e.log.Info("stopped leading")
	if onStopped != nil {
		e.guard("onStopped", onStopped)
	}
	return panicked
}

// guard runs a callback, logging a panic instead of crashing the process:
// a bug in leader-only work must not take the DNS dataplane down (D44).
func (e *LeaseElector) guard(name string, f func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			e.log.Error("leader callback panicked", "callback", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	f()
	return false
}

// release gives up a held Lease on shutdown (the ReleaseOnCancel
// semantics D44 reserves for SIGTERM): it clears the holder only if the
// Lease still names this identity, using the read resourceVersion as a
// precondition.
func (e *LeaseElector) release() {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.RenewDeadline)
	defer cancel()
	for i := 0; i < 3; i++ {
		lease, err := e.leases.Leases(e.cfg.Namespace).Get(ctx, e.cfg.Name, metav1.GetOptions{})
		if err != nil {
			e.log.Warn("lease release: get failed", "err", err)
			return
		}
		if ptr.Deref(lease.Spec.HolderIdentity, "") != e.cfg.Identity {
			return
		}
		now := metav1.NewMicroTime(time.Now())
		lease.Spec.HolderIdentity = ptr.To("")
		lease.Spec.LeaseDurationSeconds = ptr.To[int32](1)
		lease.Spec.RenewTime = &now
		lease.Spec.AcquireTime = &now
		_, err = e.leases.Leases(e.cfg.Namespace).Update(ctx, lease, metav1.UpdateOptions{})
		if err == nil {
			e.log.Info("released lease on shutdown")
			return
		}
		if !apierrors.IsConflict(err) {
			e.log.Warn("lease release failed", "err", err)
			return
		}
	}
}

// brokenElector reports a configuration error from Run.
type brokenElector struct{ err error }

func (b brokenElector) Run(context.Context, func(), func()) error { return b.err }
func (b brokenElector) IsLeader() bool                            { return false }
func (b brokenElector) Leader() string                            { return "" }
