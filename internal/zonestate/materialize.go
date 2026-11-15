package zonestate

import (
	"context"
	"sync"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// ZoneBuilder builds solely from frozen committed configuration and raw data.
type ZoneBuilder func(context.Context, *View) (*zone.Zone, error)

// Version binds one immutable model to one durable UID, sequence and root.
type Version struct {
	Zone           zonekinds.ZoneID
	CommitSequence uint64
	RootDigest     string
}

// BuiltPrimary is an opaque prebuild; its fields cannot be altered after Prepare.
type BuiltPrimary struct {
	version            Version
	model, unavailable *zone.Zone
	expiry             time.Time
	view               *View
	retired            bool
}

// MaterializerOptions sets finite zone/waiter bounds and the primary build adapter.
type MaterializerOptions struct {
	Build                ZoneBuilder
	Limits               Limits
	MaxZones, MaxWaiters int
	Now                  func() time.Time
}

// Materializer serves memory-only immutable generations with bounded shared-channel waiters.
type Materializer struct {
	mu         sync.RWMutex
	options    MaterializerOptions
	zones      map[string]*BuiltPrimary
	changed    chan struct{}
	waiters    int
	snapshot   *zone.Snapshot
	routes     map[string]*BuiltPrimary
	generation uint64
	targets    map[DesiredTarget]bool
}

// NewMaterializer validates the builder and applies finite admission limits.
func NewMaterializer(o MaterializerOptions) (*Materializer, error) {
	if o.Build == nil {
		return nil, ErrInvalid
	}
	if o.MaxZones <= 0 {
		o.MaxZones = 10000
	}
	if o.MaxWaiters <= 0 {
		o.MaxWaiters = 4096
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Limits = o.Limits.Defaults()
	m := &Materializer{options: o, zones: map[string]*BuiltPrimary{}, changed: make(chan struct{}), targets: map[DesiredTarget]bool{}}
	m.rebuild()
	return m, nil
}

// Prepare independently verifies the prospective root and prebuilds unavailable coverage.
func (m *Materializer) Prepare(ctx context.Context, v *View) (*BuiltPrimary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == nil || v.Head == nil || v.Aggregate == nil || v.Head.Spec.Lifecycle != zonekinds.LifecycleActive {
		return nil, ErrInvalid
	}
	// Re-encode/verify prevents callers from binding a mismatched aggregate to a valid head.
	p, err := Prepare(v.Aggregate, nil, m.options.Limits)
	if err != nil {
		return nil, err
	}
	if p.Head.Spec.RootDigest != v.Head.Spec.RootDigest || p.Head.Spec.ContentHash != v.Head.Spec.ContentHash || p.Head.Spec.Serial != v.Head.Spec.Serial || p.Head.Spec.AuthDigest != v.Head.Spec.AuthDigest || p.Head.Spec.AuthEpoch != v.Head.Spec.AuthEpoch || v.Head.Validate() != nil {
		return nil, ErrInvalid
	}
	detached, err := VerifyPrepared(p, m.options.Limits)
	if err != nil {
		return nil, err
	}
	owned := &View{Head: v.Head.DeepCopy(), Aggregate: detached, Expected: v.Expected}
	model, err := m.options.Build(ctx, owned)
	if err != nil {
		return nil, err
	}
	if model == nil || model.Name() != v.Head.Spec.Zone.Apex || model.Serial() != v.Head.Spec.Serial {
		return nil, ErrInvalid
	}
	unavailable, err := zone.BuildCommittedFailureCoverage(model)
	if err != nil {
		return nil, err
	}
	return &BuiltPrimary{version: Version{Zone: v.Head.Spec.Zone, CommitSequence: v.Head.Spec.CommitSequence, RootDigest: v.Head.Spec.RootDigest}, model: model, unavailable: unavailable, expiry: detached.Expiry, view: owned}, nil
}

// Install publishes exactly the root that was prebuilt, after durable CAS. UID
// replacement requires explicit Retire; ordinary late callbacks cannot resurrect it.
func (m *Materializer) Install(ctx context.Context, v *View, b *BuiltPrimary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v == nil || v.Head == nil || b == nil || b.view == nil || b.view.Head == nil || v.Head.ResourceVersion == "" || v.Head.Validate() != nil || v.Head.Spec != b.view.Head.Spec || v.Head.Spec.Lifecycle != zonekinds.LifecycleActive {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := b.version.Zone
	key := id.Apex
	if old := m.zones[key]; old != nil {
		if old.retired || old.version.Zone != id {
			return ErrConflict
		}
		if b.version.CommitSequence < old.version.CommitSequence {
			return nil
		}
		if b.version.CommitSequence == old.version.CommitSequence {
			if b.version.RootDigest != old.version.RootDigest {
				m.zones[key] = &BuiltPrimary{version: old.version, model: old.unavailable, unavailable: old.unavailable, view: old.view}
				m.signal()
				return ErrInvalid
			}
			return nil
		}
	} else if len(m.zones) >= m.options.MaxZones {
		return ErrCapacity
	}
	m.zones[key] = b
	m.signal()
	return nil
}
func (m *Materializer) signal() { m.rebuild(); close(m.changed); m.changed = make(chan struct{}) }
func (m *Materializer) rebuild() {
	models := make([]*zone.Zone, 0, len(m.zones))
	routes := make(map[string]*BuiltPrimary, len(m.zones))
	for key, b := range m.zones {
		models = append(models, b.model)
		routes[key] = b
	}
	m.generation++
	m.snapshot, _ = zone.NewSnapshot(m.generation, true, models...)
	m.routes = routes
}

// Cover publishes prebuilt unavailable coverage before an initial head exists.
// Root supplies its trusted lifecycle ZoneID and a no-data coverage model, built
// outside the query path. Existing data is never replaced by a desired watcher.
func (m *Materializer) Cover(id zonekinds.ZoneID, coverage *zone.Zone) error {
	if validateZone(id) != nil || coverage == nil || coverage.Type() != public.ZoneTypePrimary || coverage.Name() != id.Apex || coverage.SOA() != nil || coverage.AvailableAt(m.options.Now()) {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.zones[id.Apex]; old != nil {
		if old.version.Zone != id {
			return ErrConflict
		}
		return nil
	}
	if len(m.zones) >= m.options.MaxZones {
		return ErrCapacity
	}
	m.zones[id.Apex] = &BuiltPrimary{version: Version{Zone: id}, model: coverage, unavailable: coverage}
	m.signal()
	return nil
}

func (m *Materializer) installed(v *View) bool {
	if v == nil || v.Head == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b := m.zones[v.Head.Spec.Zone.Apex]
	return b != nil && !b.retired && b.model != b.unavailable && b.version == (Version{Zone: v.Head.Spec.Zone, CommitSequence: v.Head.Spec.CommitSequence, RootDigest: v.Head.Spec.RootDigest})
}

// Load verifies/prebuilds a durable root for restart or an externally committed update.
func (m *Materializer) Load(ctx context.Context, v *View) error {
	b, err := m.Prepare(ctx, v)
	if err != nil {
		return err
	}
	return m.Install(ctx, v, b)
}

// Retire is a root lifecycle adapter: deactivate a UID and retain fail-closed
// coverage. The returned marker rejects old callbacks until a new UID is
// explicitly activated by ReplaceRetired. This is not a store tombstone.
func (m *Materializer) Retire(id zonekinds.ZoneID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.zones[id.Apex]
	if b == nil || b.version.Zone != id {
		return ErrConflict
	}
	cp := *b
	cp.model = b.unavailable
	cp.expiry = time.Time{}
	cp.retired = true
	m.zones[id.Apex] = &cp
	m.signal()
	return nil
}

// Capture captures now once and selects prebuilt unavailable coverage at expiry.
// Root wraps this primary set with its lifecycle membership and source gates.
func (m *Materializer) Capture(now time.Time) zone.Set {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return capturedPrimarySet{snapshot: m.snapshot, routes: m.routes, now: now}
}

type capturedPrimarySet struct {
	snapshot *zone.Snapshot
	routes   map[string]*BuiltPrimary
	now      time.Time
}

func (s capturedPrimarySet) Synced() bool    { return s.snapshot.Synced() }
func (s capturedPrimarySet) Gate() zone.Gate { return s.snapshot.Gate() }
func (s capturedPrimarySet) Match(name string) *zone.Zone {
	model := s.snapshot.Match(name)
	if model == nil {
		return nil
	}
	b := s.routes[model.Name()]
	if b != nil && !b.expiry.IsZero() && !s.now.Before(b.expiry) {
		return b.unavailable
	}
	return model
}

// CapturedVersion exposes the ID/version from the SAME captured routing cut.
// Root uses this for its committed lifecycle/membership overlay, never a live
// manager lookup paired with a previously captured model.
func CapturedVersion(set zone.Set, apex string) (Version, bool) {
	if s, ok := set.(interface{ Version(string) (Version, bool) }); ok {
		return s.Version(apex)
	}
	return Version{}, false
}
func (s capturedPrimarySet) Version(apex string) (Version, bool) {
	model := s.snapshot.Zone(apex)
	if model == nil {
		return Version{}, false
	}
	b := s.routes[model.Name()]
	if b == nil {
		return Version{}, false
	}
	return b.version, true
}

// ReplaceRetired explicitly activates a DIFFERENT committed UID after root's
// durable lifecycle/membership barrier. Ordinary Install cannot cross this cut.
func (m *Materializer) ReplaceRetired(ctx context.Context, oldID zonekinds.ZoneID, v *View) error {
	b, err := m.Prepare(ctx, v)
	if err != nil {
		return err
	}
	if v.Head.ResourceVersion == "" || b.version.Zone.Apex != oldID.Apex || b.version.Zone.UID == oldID.UID {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.zones[oldID.Apex]
	if old == nil || !old.retired || old.version.Zone != oldID {
		return ErrConflict
	}
	m.zones[oldID.Apex] = b
	m.signal()
	return nil
}

// Desired reads durable source projection evidence from installed memory only.
func (m *Materializer) Desired(t DesiredTarget) DesiredObservation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.desired(t)
}
func (m *Materializer) desired(t DesiredTarget) DesiredObservation {
	if b := m.zones[t.Zone.Apex]; b != nil {
		return observe(b.view, t, b.model != b.unavailable && (b.expiry.IsZero() || m.options.Now().Before(b.expiry)))
	}
	return DesiredObservation{Target: t, Outcome: "pending"}
}

// WaitDesired uses the caller goroutine, a bounded admission count and one
// shared wake channel. Subscription and observation occur under the same lock.
func (m *Materializer) WaitDesired(ctx context.Context, t DesiredTarget) (DesiredObservation, error) {
	m.mu.Lock()
	if m.waiters >= m.options.MaxWaiters {
		m.mu.Unlock()
		return DesiredObservation{Target: t, Outcome: "pending"}, ErrCapacity
	}
	m.waiters++
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.waiters--; m.mu.Unlock() }()
	for {
		m.mu.RLock()
		wake := m.changed
		out := m.desired(t)
		m.mu.RUnlock()
		if out.Outcome == "superseded" || (out.Outcome != "pending" && out.Outcome != "unknown" && out.Published) {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-wake:
		}
	}
}

// RegisterDesired admits an already durably accepted target. Root calls it
// before scheduling reconciliation, and ForgetDesired when observation ends.
func (m *Materializer) RegisterDesired(t DesiredTarget) error {
	if err := validateTarget(t); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.targets[t] && len(m.targets) >= m.options.MaxWaiters {
		return ErrCapacity
	}
	count := 0
	versions := map[struct {
		Source    SourceToken
		Operation string
	}]bool{}
	for existing := range m.targets {
		if existing.Zone == t.Zone && existing.Source.ID == t.Source.ID {
			key := struct {
				Source    SourceToken
				Operation string
			}{existing.Source, existing.Operation}
			if !versions[key] {
				count++
				versions[key] = true
			}
		}
	}
	key := struct {
		Source    SourceToken
		Operation string
	}{t.Source, t.Operation}
	if !versions[key] && count >= 256 {
		return ErrCapacity
	}
	m.targets[t] = true
	return nil
}

// PendingTargets returns a detached bounded cut captured BEFORE actual reads.
func (m *Materializer) PendingTargets(id zonekinds.ZoneID) []DesiredTarget {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []DesiredTarget
	for t := range m.targets {
		if t.Zone == id {
			out = append(out, t)
		}
	}
	return out
}

// ForgetDesired releases accepted-target admission, without deleting durable receipts.
func (m *Materializer) ForgetDesired(t DesiredTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.targets, t)
}
