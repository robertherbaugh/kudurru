package providerhost

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerentitlement"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// New starts denied and starts no workers. It never reads a Store image or
// infers restart continuity from license bytes. Root must mount the lifecycle.
func New(in Options) (*Owner, error) {
	if in.Store == nil || in.Resources == nil || in.Publisher == nil || in.License == nil || in.PinAuthKeys == nil || in.Withdraw == nil || in.Schema.Scheme == nil || in.Schema.SchemaVersion < 1 || len(in.Schema.Kinds) == 0 || len(in.Schema.Kinds) > 1024 || !in.Static.Current() {
		return nil, ErrInvalid
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	a, blocked := in.Store.ServingAuthority()
	if blocked || a.Term.Validate() != nil {
		return nil, ErrInvalid
	}
	cut := in.License.Capture()
	if !cut.Current() {
		return nil, ErrInvalid
	}
	if !in.Static.retain() {
		return nil, ErrInvalid
	}
	in.Schema.Kinds = slices.Clone(in.Schema.Kinds)
	o := &Owner{opts: in, changes: make(chan struct{})}
	p, until, _ := providerentitlement.Profile(cut)
	o.value.Store(&generation{serial: 1, cluster: a.Term.ClusterID, static: in.Static, license: cut, profile: p, until: until, phase: denied, baseline: map[string]bool{}})
	return o, nil
}
func lock(ctx context.Context, gate *sync.RWMutex, write bool) error {
	if ctx == nil {
		return ErrInvalid
	}
	try := gate.TryRLock
	if write {
		try = gate.TryLock
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if try() {
		return nil
	}
	timer := time.NewTicker(time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if try() {
				return nil
			}
		}
	}
}
func (o *Owner) check(ctx context.Context) error {
	if o == nil || ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.closed.Load() {
		return ErrClosed
	}
	return nil
}
func (o *Owner) signal() {
	o.changesMu.Lock()
	close(o.changes)
	o.changes = make(chan struct{})
	o.changesMu.Unlock()
	o.opts.Publisher.Wake()
}

// Changes broadcasts replacement hints. Subscribe before capturing.
func (o *Owner) Changes() <-chan struct{} {
	o.changesMu.Lock()
	defer o.changesMu.Unlock()
	return o.changes
}
func (o *Owner) current(g *generation) bool {
	if g == nil || o.closed.Load() || o.value.Load() != g || !g.static.Current() || !g.license.Current() {
		return false
	}
	if !g.until.IsZero() && !o.opts.Now().Before(g.until) {
		return false
	}
	// Denied generations remain valid beyond a positive provider deadline only
	// after ObserveLicense has installed a new denying policy interval.
	if g.phase == pending && (g.writer == nil || g.writer.Err() != nil) {
		return false
	}
	return true
}

// PinHost holds license/host/auth-key replacement exclusion through the whole
// synchronous callback. Caller already holds resources; no outer reentry.
func (o *Owner) PinHost(ctx context.Context, fn func() error) error {
	if fn == nil {
		return ErrInvalid
	}
	return o.PinHostCurrent(ctx, func(_ func() bool) error { return fn() })
}

// PinHostCurrent supplies the exact pinned local memory/clock predicate. It
// reads no Store/registry, takes no lock, and denies after the callback exits.
// Root must retain its independent original-writer admission and final checks.
func (o *Owner) PinHostCurrent(ctx context.Context, fn func(func() bool) error) error {
	if err := o.check(ctx); err != nil {
		return err
	}
	if fn == nil {
		return ErrInvalid
	}
	return o.opts.License.WithCurrent(ctx, func(cut license.Cut) error {
		if err := lock(ctx, &o.gate, false); err != nil {
			return err
		}
		defer o.gate.RUnlock()
		g := o.value.Load()
		if !o.current(g) || g.license.Identity() != cut.Identity() {
			return ErrStale
		}
		return o.opts.PinAuthKeys(ctx, func() error {
			if !o.current(g) {
				return ErrStale
			}
			var live atomic.Bool
			live.Store(true)
			defer live.Store(false)
			return fn(func() bool { return live.Load() && ctx.Err() == nil && o.current(g) && cut.Current() })
		})
	})
}
func copyGeneration(g *generation) (*generation, error) {
	if g.serial == math.MaxUint64 || len(g.baseline) > providerregistry.MaxAuthorities {
		return nil, ErrBounds
	}
	next := *g
	next.serial++
	next.baseline = make(map[string]bool, len(g.baseline))
	for k, v := range g.baseline {
		next.baseline[k] = v
	}
	return &next, nil
}
func fence(g *generation) {
	if g.entitlement != "" {
		addBaseline(g, g.entitlement)
	}
	g.phase = waiting
	g.entitlement = ""
	g.writer = nil
	g.target.Term = replication.Term{}
	g.target.Incarnation = 0
}

// taint requires a fresh actual installed-image baseline. The last observed E
// can lag an already installed replica, so it cannot prove post-change recovery.
// The first validated current image after taint grants nothing; only a later
// different primary activation can clear the follower fence.
func taint(g *generation) {
	fence(g)
	g.observed = false
}

func addBaseline(g *generation, value string) {
	if g.baseline[value] {
		return
	}
	if len(g.baseline) == providerregistry.MaxAuthorities {
		g.historyFull = true
		return
	}
	g.baseline[value] = true
}

// mutate is exclusive resource -> license -> host write -> actual key guard.
// It does not acquire publisher; Reset wraps it in the publisher writer gate.
func (o *Owner) mutate(ctx context.Context, fn func(*providerresources.Replacement, *generation, license.Cut) error) error {
	if err := o.check(ctx); err != nil {
		return err
	}
	return o.opts.Resources.WithReplacement(ctx, func(r *providerresources.Replacement) error {
		return o.opts.License.WithCurrent(ctx, func(cut license.Cut) error {
			if err := lock(ctx, &o.gate, true); err != nil {
				return err
			}
			defer o.gate.Unlock()
			if err := o.check(ctx); err != nil {
				return err
			}
			return o.opts.PinAuthKeys(ctx, func() error { return fn(r, o.value.Load(), cut) })
		})
	})
}

// ObserveLicense fences installation or phase changes. Identical polling and
// active/grace transitions keep the same generation; root arranges deadline wakes.
func (o *Owner) ObserveLicense(ctx context.Context) error {
	changed := false
	err := o.mutate(ctx, func(r *providerresources.Replacement, g *generation, cut license.Cut) error {
		p, until, _ := providerentitlement.Profile(cut)
		if g.license.Identity() == cut.Identity() && g.profile == p && g.license.Current() {
			return nil
		}
		n, err := copyGeneration(g)
		if err != nil {
			r.Retire()
			return err
		}
		taint(n)
		n.license = cut
		n.profile = p
		n.until = until
		o.value.Store(n)
		r.Retire()
		changed = true
		return nil
	})
	if changed {
		o.signal()
	}
	return err
}

// ReplaceStatic installs prepared immutable input while fencing authority. It
// grants nothing; primary Reset or new follower receipt is required afterward.
func (o *Owner) ReplaceStatic(ctx context.Context, s *StaticResources) error {
	if !s.Current() {
		return ErrInvalid
	}
	var old *StaticResources
	err := o.mutate(ctx, func(r *providerresources.Replacement, g *generation, cut license.Cut) error {
		if !s.Current() {
			return ErrStale
		}
		if s == g.static {
			return nil
		}
		n, err := copyGeneration(g)
		if err != nil {
			r.Retire()
			return err
		}
		if !s.retain() {
			return ErrStale
		}
		taint(n)
		n.static = s
		n.license = cut
		n.profile, n.until, _ = providerentitlement.Profile(cut)
		old = g.static
		o.value.Store(n)
		r.Retire()
		return nil
	})
	if old != nil {
		old.release()
		o.signal()
	}
	return err
}

// Close invalidates captures without closing borrowed owners. Call outside all
// callbacks, after joining coordinator and releasing complete serving bundles.
func (o *Owner) Close(ctx context.Context) error {
	if o == nil || ctx == nil {
		return ErrInvalid
	}
	if o.closed.Load() {
		return nil
	}
	var old *StaticResources
	closeMemory := func() error {
		if err := lock(ctx, &o.gate, true); err != nil {
			return err
		}
		defer o.gate.Unlock()
		if o.closed.Swap(true) {
			return nil
		}
		old = o.value.Load().static
		return nil
	}
	err := o.opts.Resources.WithReplacement(ctx, func(r *providerresources.Replacement) error {
		if err := closeMemory(); err != nil {
			return err
		}
		r.Retire()
		return nil
	})
	// Coordinator.Close closes its dedicated resources. Their closed owner has
	// no current callbacks or new acquisitions; remaining host refs still need
	// deterministic release without requiring that borrowed owner to reopen.
	if errors.Is(err, providerresources.ErrClosed) {
		err = closeMemory()
	}
	if old != nil {
		old.release()
		o.signal()
	}
	return err
}
