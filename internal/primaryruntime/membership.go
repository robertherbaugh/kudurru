package primaryruntime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Runtime owns immutable background-built membership cuts.
type Runtime struct {
	mu          sync.RWMutex
	notifyMu    sync.Mutex
	notifyAfter string
	o           Options
	entries     map[string]*entry
	generation  uint64
	cut         Cut
}

// Cut pins routing, lifecycle and versions together. Its maps and models are
// private and immutable; Member returns a detached copy of its parent slice.
type Cut struct {
	snapshot *zone.Snapshot
	entries  map[string]*entry
	now      time.Time
	ready    map[string]bool
}

// Synced reports whether routing is initialized.
func (c Cut) Synced() bool { return c.snapshot != nil && c.snapshot.Synced() }

// Gate returns the captured query gate.
func (c Cut) Gate() zone.Gate {
	if c.snapshot == nil {
		return zone.GateUnsynced
	}
	return c.snapshot.Gate()
}

// Match chooses a captured zone or unavailable authoritative coverage.
func (c Cut) Match(name string) *zone.Zone {
	if c.snapshot == nil {
		return nil
	}
	z := c.snapshot.Match(name)
	if z == nil {
		return nil
	}
	e := c.entries[z.Name()]
	if e == nil {
		return nil
	}
	if !c.eligible(e) || (!e.expiry.IsZero() && !c.now.Before(e.expiry)) {
		return e.coverage
	}
	return z
}
func (c Cut) eligible(e *entry) bool { return c.ready[e.member.Zone.Apex] }
func (c Cut) readiness() map[string]bool {
	ready := make(map[string]bool, len(c.entries))
	for apex, e := range c.entries {
		ready[apex] = e.member.Phase == zk.PhaseActive
		for _, p := range e.member.Parents {
			parent := c.entries[p.Zone.Apex]
			if parent == nil || parent.member.Phase != zk.PhaseActive || parent.member.Incarnation != p.Incarnation || parent.member.Version.Zone != p.Zone || parent.member.Version.CommitSequence < p.CommitSequence || (parent.member.Version.CommitSequence == p.CommitSequence && parent.member.Version.RootDigest != p.RootDigest) || !hasFence(parent.view, apex) {
				ready[apex] = false
			}
		}
	}
	for apex, child := range c.entries {
		if child.member.Phase != zk.PhaseActive {
			continue
		}
		labels := dns.SplitDomainName(apex)
		for i := 1; i <= len(labels); i++ {
			ancestor := strings.Join(labels[i:], ".") + "."
			parent := c.entries[ancestor]
			if parent == nil || parent.member.Phase != zk.PhaseActive || parent.member.Type != public.ZoneTypePrimary {
				continue
			}
			found := false
			for _, p := range child.member.Parents {
				if p.Zone == parent.member.Version.Zone && p.Incarnation == parent.member.Incarnation {
					found = true
					break
				}
			}
			if !found || !hasFence(parent.view, apex) {
				ready[apex] = false
				ready[ancestor] = false
			}
		}
	}
	return ready
}

// Member returns detached membership diagnostics.
func (c Cut) Member(apex string) (Member, bool) {
	apex = canonical(apex)
	e := c.entries[apex]
	if e == nil {
		return Member{}, false
	}
	m := e.member
	m.Parents = append([]zk.ParentFence(nil), m.Parents...)
	m.Published = m.Published && c.eligible(e) && (e.expiry.IsZero() || c.now.Before(e.expiry))
	return m, true
}

// Version returns the captured durable head version.
func (c Cut) Version(apex string) (zonestate.Version, bool) {
	m, ok := c.Member(apex)
	return m.Version, ok
}
func canonical(name string) string { return dns.CanonicalName(name) }

// New constructs a bounded committed-primary runtime.
func New(o Options) (*Runtime, error) {
	if o.Store == nil || o.Repository == nil {
		return nil, ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxZones <= 0 {
		o.MaxZones = 10000
	}
	if o.MaxZones > 100000 {
		return nil, ErrCapacity
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.MaxAttempts > 64 {
		return nil, ErrCapacity
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	o.Limits = o.Limits.Defaults()
	o.UpdateLimits = o.UpdateLimits.Defaults()
	if o.Compiler == nil {
		o.Compiler = primaryzone.NewCompiler(primaryzone.CompilerOptions{Limits: o.Limits})
	}
	r := &Runtime{o: o, entries: map[string]*entry{}}
	r.rebuild()
	return r, nil
}

// Capture pins memory-only membership and the exact expiry time.
func (r *Runtime) Capture(now time.Time) Cut {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := r.cut
	c.now = now
	return c
}
func (r *Runtime) rebuild() {
	r.generation++
	models := make([]*zone.Zone, 0, len(r.entries))
	entries := make(map[string]*entry, len(r.entries))
	for apex, e := range r.entries {
		models = append(models, e.model)
		entries[apex] = e
	}
	snapshot, _ := zone.NewSnapshot(r.generation, true, models...)
	r.cut = Cut{snapshot: snapshot, entries: entries}
	r.cut.ready = r.cut.readiness()
}

func (r *Runtime) build(ctx context.Context, l *zk.ZoneLifecycle, v *zonestate.View, config *public.DNSZone) (*entry, error) {
	if l.Validate() != nil || l.ResourceVersion == "" {
		return nil, ErrInvalid
	}
	l = l.DeepCopy()
	e := &entry{lifecycle: l, member: Member{Zone: l.Spec.Zone, Type: l.Spec.Type, Incarnation: l.Spec.Incarnation, Sequence: l.Spec.Sequence, Phase: l.Spec.Phase, Parents: append([]zk.ParentFence(nil), l.Spec.Parents...)}}
	if v != nil {
		if v.Head == nil || v.Aggregate == nil || v.Head.Validate() != nil || v.Head.Spec.Zone != l.Spec.HeadZone() || v.Head.ResourceVersion == "" || (l.Spec.Phase == zk.PhaseActive && v.Head.Spec.Lifecycle != zk.LifecycleActive) {
			return nil, ErrInvalid
		}
		if config == nil {
			config = &public.DNSZone{}
			if err := json.Unmarshal(v.Aggregate.Config, config); err != nil {
				return nil, err
			}
		}
		active := *v
		p, err := zonestate.Prepare(v.Aggregate, nil, r.o.Limits)
		if err != nil {
			return nil, err
		}
		owned, err := zonestate.VerifyPrepared(p, r.o.Limits)
		if err != nil {
			return nil, err
		}
		active.Aggregate = owned
		active.Head = v.Head.DeepCopy()
		active.Head.Spec.Lifecycle = zk.LifecycleActive
		m, err := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: primaryzone.BuildCommitted, Limits: r.o.Limits})
		if err != nil {
			return nil, err
		}
		if err := m.Load(ctx, &active); err != nil {
			return nil, err
		}
		e.model = m.Capture(r.o.Now()).Match(l.Spec.Zone.Apex)
		e.coverage, err = zone.BuildCommittedFailureCoverage(e.model)
		if err != nil {
			return nil, err
		}
		e.view = &zonestate.View{Head: v.Head.DeepCopy(), Aggregate: owned, Expected: v.Expected}
		e.member.Version = version(v)
		e.expiry = v.Aggregate.Expiry
		if v.Head.Spec.Lifecycle != zk.LifecycleActive || l.Spec.Phase != zk.PhaseActive {
			e.model = e.coverage
		}
	} else {
		if config == nil {
			return nil, ErrInvalid
		}
		cp := config.DeepCopy()
		cp.Spec.Type = public.ZoneTypePrimary
		var err error
		e.coverage, err = zone.BuildCommittedUnavailable(cp)
		if err != nil || e.coverage.Name() != l.Spec.Zone.Apex {
			return nil, ErrInvalid
		}
		e.model = e.coverage
	}
	return e, nil
}

// Observe consumes a root-captured, verified durable lifecycle/head pair for
// restart or source publication. It does not grant write authority. Root must
// supply a complete carrier cut; absence/errors must never call this as prune.
func (r *Runtime) Observe(ctx context.Context, l *zk.ZoneLifecycle, v *zonestate.View, config *public.DNSZone) error {
	e, err := r.build(ctx, l, v, config)
	if err != nil {
		return err
	}
	return r.install(e)
}
func (r *Runtime) install(e *entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.entries[e.member.Zone.Apex]
	if old != nil {
		if old.lifecycle.Name != e.lifecycle.Name {
			return ErrConflict
		}
		if e.member.Incarnation < old.member.Incarnation || (e.member.Incarnation == old.member.Incarnation && e.member.Sequence < old.member.Sequence) {
			return ErrConflict
		}
		if e.member.Incarnation == old.member.Incarnation {
			if e.member.Sequence == old.member.Sequence && !reflect.DeepEqual(e.lifecycle.Spec, old.lifecycle.Spec) {
				return ErrConflict
			}
			if phaseOrder(e.member.Phase) < phaseOrder(old.member.Phase) {
				return ErrConflict
			}
			if e.member.Zone != old.member.Zone || e.member.Type != old.member.Type {
				return ErrConflict
			}
			if old.member.Phase == zk.PhaseRetired && e.member.Phase != zk.PhaseRetired {
				return ErrConflict
			}
			if old.view != nil && e.view != nil && (e.member.Version.CommitSequence < old.member.Version.CommitSequence || (e.member.Version.CommitSequence == old.member.Version.CommitSequence && e.member.Version.RootDigest != old.member.Version.RootDigest)) {
				return ErrConflict
			}
			if old.view != nil && old.view.Head.Spec.Lifecycle == zk.LifecycleTombstoned && e.view != nil && e.view.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
				return ErrConflict
			}
		} else if e.member.Incarnation != old.member.Incarnation+1 || old.member.Phase != zk.PhaseRetired {
			return ErrConflict
		}
	} else if len(r.entries) >= r.o.MaxZones {
		return ErrCapacity
	}
	// Same source may change apex only after its previous incarnation retired.
	for apex, other := range r.entries {
		if apex != e.member.Zone.Apex && other.lifecycle.Name == e.lifecycle.Name {
			if other.member.Phase != zk.PhaseRetired || e.member.Incarnation != other.member.Incarnation+1 {
				return ErrConflict
			}
			delete(r.entries, apex)
		}
	}
	r.entries[e.member.Zone.Apex] = e
	r.rebuild()
	return nil
}

func (r *Runtime) publish(ctx context.Context, e *entry) error {
	if r.o.Publish == nil {
		return ErrUnavailable
	}
	c := r.Capture(r.o.Now())
	m, ok := c.Member(e.member.Zone.Apex)
	if !ok || m.Incarnation != e.member.Incarnation || m.Sequence != e.member.Sequence || m.Version != e.member.Version {
		return ErrConflict
	}
	if err := r.o.Publish(ctx, c, m); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	live := r.entries[m.Zone.Apex]
	if live != e {
		return ErrConflict
	}
	cp := *e
	cp.member.Published = true
	r.entries[m.Zone.Apex] = &cp
	r.rebuild()
	return nil
}
