package servingzones

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fixture struct {
	t        *testing.T
	store    *sqlite.Store
	scheme   *runtime.Scheme
	registry []replication.Kind
	runtime  *pr.Runtime
	now      time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{public.AddToScheme, zk.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, store: st, scheme: scheme, now: time.Now()}
	// Expected kinds derive from the local schema, including explicit zero
	// manifests. The actual candidate never defines its own expected registry.
	known := scheme.KnownTypes(public.GroupVersion)
	for name, typ := range known {
		if _, list := known[name+"List"]; !list {
			continue
		}
		if _, ok := reflect.New(typ).Interface().(store.Object); ok {
			f.registry = append(f.registry, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
		}
	}
	slices.SortFunc(f.registry, func(a, b replication.Kind) int {
		if a.Kind < b.Kind {
			return -1
		}
		if a.Kind > b.Kind {
			return 1
		}
		return 0
	})
	adapter, err := sqliteadapter.New(sqliteadapter.Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := st.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	f.runtime, err = pr.New(pr.Options{Store: st, Repository: zonestate.NewRepository(backend, zonestate.Limits{}), Reader: primaryzone.NewReader(st, primaryzone.ReaderOptions{}), LifecycleCAS: adapter.LifecycleCAS, Admit: adapter.Admission, Publish: func(context.Context, pr.Cut, pr.Member) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) apply(obj store.Object) store.Object {
	f.t.Helper()
	saved, err := f.store.Apply(f.t.Context(), obj, store.ApplyOptions{Manager: "api"})
	if err != nil {
		f.t.Fatal(err)
	}
	return saved
}
func (f *fixture) config(name, apex string, typ public.ZoneType) *public.DNSZone {
	f.t.Helper()
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: apex, Type: typ, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	switch typ {
	case public.ZoneTypePrimary:
		z.Spec.Nameservers = []string{"ns." + apex}
	case public.ZoneTypeForward:
		z.Spec.Forward = &public.ForwardSpec{UpstreamGroup: "corp"}
	case public.ZoneTypeSecondary:
		z.Spec.Secondary = &public.SecondarySpec{Primaries: []string{"127.0.0.1:5300"}}
	case public.ZoneTypeStub:
		z.Spec.Stub = &public.StubSpec{Nameservers: []string{"127.0.0.1:5300"}}
	}
	return f.apply(z).(*public.DNSZone)
}
func (f *fixture) begin(z *public.DNSZone, parents ...*zk.ZoneLifecycle) *zk.ZoneLifecycle {
	f.t.Helper()
	l, err := pr.NewLifecycle(f.store.Info().ClusterID, z, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, p := range parents {
		l.Spec.Parents = append(l.Spec.Parents, pr.ParentObligation(p, l))
	}
	l, err = f.runtime.Transition(f.t.Context(), nil, l)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}
func (f *fixture) active(z *public.DNSZone, parents ...*zk.ZoneLifecycle) *zk.ZoneLifecycle {
	f.t.Helper()
	l := f.begin(z, parents...)
	if z.Spec.Type == public.ZoneTypePrimary {
		if _, err := f.runtime.Reconcile(f.t.Context(), l.Spec.Zone); err != nil {
			f.t.Fatal(err)
		}
	} else {
		fenced, err := f.runtime.FenceParents(f.t.Context(), l.Spec.Zone)
		if err != nil {
			f.t.Fatal(err)
		}
		next := fenced.DeepCopy()
		next.Spec.Sequence++
		next.Spec.Phase = zk.PhaseActive
		active, err := f.runtime.Transition(f.t.Context(), fenced, next)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := f.runtime.Observe(f.t.Context(), active, nil, z); err != nil {
			f.t.Fatal(err)
		}
	}
	l, err := f.runtime.ReadLifecycle(f.t.Context(), l.Spec.Zone)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}
func (f *fixture) image() replication.Image {
	f.t.Helper()
	i, err := f.store.ReadGeneration(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	return *i
}
func (f *fixture) options(image replication.Image) Options {
	f.t.Helper()
	primary, err := pr.PrepareImage(f.t.Context(), image, pr.ImageOptions{ClusterID: f.store.Info().ClusterID, Scheme: f.scheme, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: f.registry}, Now: f.now})
	if err != nil {
		f.t.Fatal(err)
	}
	var objects []store.Object
	publicKinds := zonecache.DefaultKinds()
	for _, raw := range image.Payload.Objects {
		var meta metav1.TypeMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			f.t.Fatal(err)
		}
		if !slices.Contains(publicKinds, store.Kind(meta.Kind)) {
			continue
		}
		gv, err := schema.ParseGroupVersion(meta.APIVersion)
		if err != nil {
			f.t.Fatal(err)
		}
		obj, err := f.scheme.New(gv.WithKind(meta.Kind))
		if err != nil {
			f.t.Fatal(err)
		}
		if err := json.Unmarshal(raw, obj); err != nil {
			f.t.Fatal(err)
		}
		objects = append(objects, obj.(store.Object))
	}
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: publicKinds, Now: f.now}, objects, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		f.t.Fatal(err)
	}
	return Options{Base: image.Generation(), Objects: snapshot.Objects, Primary: primary, Keys: keys, Now: f.now}
}
func mustPrepare(t *testing.T, o Options) *Prepared {
	t.Helper()
	p, err := Prepare(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func unavailable(t *testing.T, p *Prepared, name string, now time.Time) {
	t.Helper()
	z := p.Zones(now).Match("www." + name)
	if z == nil || z.Name() != name || (z.AvailableAt(now) && z.Type() != public.ZoneTypeStub) || z.RoutingAvailableAt(now) {
		t.Fatalf("lost unavailable coverage %s: %v", name, z)
	}
}

func TestActualCompositionCommittedPrimaryAndDetachedConfig(t *testing.T) {
	f := newFixture(t)
	z := f.config("primary", "example.", public.ZoneTypePrimary)
	f.apply(&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "committed", Namespace: "apps"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "committed.example.", RecordType: "A", Targets: []string{"192.0.2.1"}}}}})
	f.active(z)
	f.apply(&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "desired", Namespace: "apps"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "desired.example.", RecordType: "A", Targets: []string{"192.0.2.99"}}}}})
	f.config("route", "other.", public.ZoneTypeForward)
	o := f.options(f.image())
	// No private carriers leak into the public objects owner.
	if o.Objects.Count(zk.KindZoneLifecycle) != 0 {
		t.Fatal("private carrier leaked")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	p := mustPrepare(t, o)
	held := p.Zones(f.now)
	model := held.Match("committed.example.")
	if model.Lookup("committed.example.", dns.TypeA).Kind != zone.Success || model.Lookup("desired.example.", dns.TypeA).Kind != zone.NXDomain {
		t.Fatal("desired primary mixed into committed authority")
	}
	if !p.Base().Same(o.Base) || !held.Synced() || held.Gate() != zone.GateOpen || held.Match("x.other.").UpstreamGroup() != "corp" {
		t.Fatal("base/strict forward mismatch")
	}
	for _, obj := range o.Objects.Zones() {
		obj.Spec.Forward = &public.ForwardSpec{UpstreamGroup: "mutated"}
		obj.Spec.ZoneName = "mutated."
	}
	for _, rs := range o.Objects.RecordSets() {
		rs.Spec.Endpoints = nil
	}
	if held.Match("x.other.").UpstreamGroup() != "corp" || held.Match("committed.example.").Lookup("committed.example.", dns.TypeA).Kind != zone.Success {
		t.Fatal("held routing aliases inputs")
	}
}

func TestActualNonprimarySourceMutationMissingWitnessAndPreparing(t *testing.T) {
	f := newFixture(t)
	parent := f.active(f.config("parent", "example.", public.ZoneTypePrimary))
	childConfig := f.config("child", "child.example.", public.ZoneTypeForward)
	child := f.active(childConfig, parent)
	i := f.image()
	o := f.options(i)
	p := mustPrepare(t, o)
	if !o.Primary.Admitted("child.example.", f.now) || p.Zones(f.now).Match("x.child.example.").Type() != public.ZoneTypeForward {
		t.Fatal("admitted forward was not composed")
	}
	// Well-formed complete image with absent obligation must fail closed.
	for n, raw := range i.Payload.Objects {
		var meta metav1.TypeMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind != zk.KindZoneLifecycle {
			continue
		}
		var l zk.ZoneLifecycle
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		if l.Kind == zk.KindZoneLifecycle && l.Name == child.Name {
			l.Spec.Parents = nil
			var err error
			i.Payload.Objects[n], err = json.Marshal(&l)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := replication.Seal(&i); err != nil {
		t.Fatal(err)
	}
	missing := f.options(i)
	if missing.Primary.Admitted("child.example.", f.now) {
		t.Fatal("missing parent witness admitted")
	}
	unavailable(t, mustPrepare(t, missing), "child.example.", f.now)
	changed := childConfig.DeepCopy()
	changed.Spec.Forward.UpstreamGroup = "new"
	changed = f.apply(changed).(*public.DNSZone)
	if changed.UID != childConfig.UID {
		t.Fatal("fixture lost same UID")
	}
	mutated := f.options(f.image())
	if mutated.Primary.Admitted("child.example.", f.now) {
		t.Fatal("stale same-UID source token admitted")
	}
	unavailable(t, mustPrepare(t, mutated), "child.example.", f.now)
	if p.Zones(f.now).Match("x.child.example.").UpstreamGroup() != "corp" {
		t.Fatal("held old cut changed")
	}
	f2 := newFixture(t)
	preparing := f2.config("route", "preparing.", public.ZoneTypeForward)
	f2.begin(preparing)
	unavailable(t, mustPrepare(t, f2.options(f2.image())), "preparing.", f2.now)
}

func TestActualMissingRuntimeAndRPZCanonicalSelection(t *testing.T) {
	f := newFixture(t)
	f.config("secondary", "secondary.", public.ZoneTypeSecondary)
	f.config("stub", "stub.", public.ZoneTypeStub)
	f.config("primary", "primary.", public.ZoneTypePrimary)
	for _, namespace := range []string{"one", "two", "svc"} {
		f.apply(&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "ads", Namespace: namespace}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.", Primaries: []string{"127.0.0.1:5300"}}}}}})
	}
	o := f.options(f.image())
	p := mustPrepare(t, o)
	for _, apex := range []string{"secondary.", "stub.", "primary."} {
		unavailable(t, p, apex, f.now)
	}
	if !p.HasList("one/ads") || !p.HasList("svc/ads") || p.HasList("ads") || p.HasList("service:ads") {
		t.Fatal("canonical names/catalog collision")
	}
	s, err := p.SelectRPZ([]string{"service:ads", "two/ads", "one/ads"})
	if err != nil {
		t.Fatal(err)
	}
	if d := s.Evaluate(rpz.Context{Name: "bad."}, f.now); !d.Unavailable || d.List != "two/ads" {
		t.Fatal("required missing feed/order lost", d)
	}
	for _, names := range [][]string{{"ads"}, {"missing"}, {"svc/missing"}, {"service:"}, {""}, {"one/ads", "one/ads"}} {
		if _, err := p.SelectRPZ(names); err == nil {
			t.Fatal("invalid list selection accepted", names)
		}
	}
	if s, err := p.SelectRPZ([]string{"service:ads"}); err != nil || s.Evaluate(rpz.Context{Name: "bad."}, f.now).Unavailable {
		t.Fatal("reserved service labels became required RPZ", err)
	}
	for _, list := range o.Objects.BlockLists() {
		list.Spec.Sources = nil
		list.Name = "mutated"
	}
	if d := s.Evaluate(rpz.Context{Name: "bad."}, f.now); !d.Unavailable || d.List != "two/ads" {
		t.Fatal("held raw selection mutated", d)
	}
}

func TestPrepareRejectsMismatchedOrUnboundedCut(t *testing.T) {
	f := newFixture(t)
	f.config("one", "one.", public.ZoneTypeForward)
	f.config("two", "two.", public.ZoneTypeForward)
	o := f.options(f.image())
	for _, mutate := range []func(*Options){func(x *Options) { x.Keys = nil }, func(x *Options) { x.Primary = nil }, func(x *Options) { x.Now = time.Time{} }, func(x *Options) { x.Base.Revision++ }, func(x *Options) { x.Objects = zonecache.ObjectSet{} }, func(x *Options) { x.MaxZones = 1 }, func(x *Options) { x.MaxFeeds = 100001 }} {
		bad := o
		mutate(&bad)
		if _, err := Prepare(bad); err == nil {
			t.Fatal("invalid cut accepted")
		}
	}
	// Unsupported configuration cannot retain an earlier usable zone silently.
	o.Objects.Zones()[0].Spec.Type = public.ZoneTypeCatalog
	if _, err := Prepare(o); err == nil {
		t.Fatal("invalid nonprimary builder succeeded")
	}
}

func TestActualAncestorDeadlineClosesNonprimaryAdmission(t *testing.T) {
	f := newFixture(t)
	config := f.config("parent", "example.", public.ZoneTypePrimary)
	deadline := metav1.NewTime(f.now.Add(time.Minute).Truncate(time.Second))
	f.apply(&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "expiring", Namespace: "apps"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: config.Name}, Endpoints: []public.Endpoint{{DNSName: "expiring.example.", RecordType: "A", Targets: []string{"192.0.2.1"}, Kudurru: &public.EndpointExt{ExpiresAt: &deadline}}}}})
	parent := f.active(config)
	f.active(f.config("child", "child.example.", public.ZoneTypeForward), parent)
	o := f.options(f.image())
	p := mustPrepare(t, o)
	before := deadline.Add(-time.Nanosecond)
	if !o.Primary.Admitted("CHILD.EXAMPLE.", before) || !o.Primary.Admitted("example.", before) || p.Zones(before).Match("x.child.example.").Type() != public.ZoneTypeForward {
		t.Fatal("valid admission/ancestor deadline rejected")
	}
	if o.Primary.Admitted("child.example.", deadline.Time) || o.Primary.Admitted("example.", deadline.Time) || o.Primary.Admitted("child.example.", time.Time{}) || o.Primary.Admitted("absent.", f.now) || (*pr.PreparedImage)(nil).Admitted("example.", f.now) {
		t.Fatal("expired or missing readiness admitted")
	}
	unavailable(t, p, "child.example.", deadline.Time)
	unavailable(t, p, "example.", deadline.Time)
}

func TestActualPrimaryTypeReplacementRetainsCoverageUntilNewIncarnation(t *testing.T) {
	f := newFixture(t)
	config := f.config("changing", "example.", public.ZoneTypePrimary)
	old := f.active(config)
	held := mustPrepare(t, f.options(f.image()))
	changed := config.DeepCopy()
	changed.Spec.Type, changed.Spec.Forward = public.ZoneTypeForward, &public.ForwardSpec{UpstreamGroup: "replacement"}
	changed = f.apply(changed).(*public.DNSZone)
	unavailable(t, mustPrepare(t, f.options(f.image())), "example.", f.now)
	token, err := primaryzone.SourceToken(changed)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := f.runtime.RetireSource(t.Context(), old.Spec.Zone, zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest})
	if err != nil {
		t.Fatal(err)
	}
	unavailable(t, mustPrepare(t, f.options(f.image())), "example.", f.now)
	next, err := pr.NewLifecycle(old.Spec.Zone.ClusterID, changed, nil)
	if err != nil {
		t.Fatal(err)
	}
	next.Spec.Incarnation, next.Spec.Sequence = retired.Spec.Incarnation+1, retired.Spec.Sequence+1
	next, err = f.runtime.Transition(t.Context(), retired, next)
	if err != nil {
		t.Fatal(err)
	}
	unavailable(t, mustPrepare(t, f.options(f.image())), "example.", f.now)
	fenced, err := f.runtime.FenceParents(t.Context(), next.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	active := fenced.DeepCopy()
	active.Spec.Phase = zk.PhaseActive
	active.Spec.Sequence++
	if _, err := f.runtime.Transition(t.Context(), fenced, active); err != nil {
		t.Fatal(err)
	}
	o := f.options(f.image())
	p := mustPrepare(t, o)
	if !o.Primary.Admitted("example.", f.now) || p.Zones(f.now).Match("x.example.").UpstreamGroup() != "replacement" {
		t.Fatal("durable replacement not composed")
	}
	if held.Zones(f.now).Match("x.example.").Type() != public.ZoneTypePrimary || !held.Zones(f.now).Match("x.example.").AvailableAt(f.now) {
		t.Fatal("held old primary changed")
	}
	if err := f.store.Delete(t.Context(), store.KeyOf(changed), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	if p := mustPrepare(t, f.options(f.image())); p.Zones(f.now).Match("x.example.") != nil {
		t.Fatal("deleted nonprimary configuration resurrected")
	}
}

func TestActualMovedApexAndRecreatedSourceCannotFallBackToLegacy(t *testing.T) {
	f := newFixture(t)
	config := f.config("moving", "old.example.", public.ZoneTypeForward)
	old := f.active(config)
	o := f.options(f.image())
	held := mustPrepare(t, o)
	if !o.Primary.Coordinated(config.Name) || o.Primary.Coordinated("default/"+config.Name) || o.Primary.Coordinated("missing") || (*pr.PreparedImage)(nil).Coordinated(config.Name) {
		t.Fatal("source-key coordination contract mismatch")
	}
	moved := config.DeepCopy()
	moved.Spec.ZoneName = "new.example."
	// Ordinary desired writes enforce immutable apex. The production move is
	// a real delete/recreate of the same source key while its old ledger remains.
	if _, err := f.store.Apply(t.Context(), moved, store.ApplyOptions{Manager: "api"}); err == nil {
		t.Fatal("ordinary source write changed immutable apex")
	}
	if err := f.store.Delete(t.Context(), store.KeyOf(config), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	moved.ObjectMeta = metav1.ObjectMeta{Name: config.Name}
	moved = f.apply(moved).(*public.DNSZone)
	for _, recreate := range []bool{false, true} {
		if recreate {
			if err := f.store.Delete(t.Context(), store.KeyOf(moved), store.DeleteOptions{Manager: "api"}); err != nil {
				t.Fatal(err)
			}
			moved.UID, moved.ResourceVersion = "", ""
			moved = f.apply(moved).(*public.DNSZone)
			if moved.UID == config.UID {
				t.Fatal("recreate did not allocate new UID")
			}
		}
		candidate := f.options(f.image())
		if !candidate.Primary.Coordinated(config.Name) || candidate.Primary.Admitted("new.example.", f.now) {
			t.Fatal("unresolved old lifecycle lost coordination")
		}
		unavailable(t, mustPrepare(t, candidate), "new.example.", f.now)
	}
	// A retired former UID still prevents source-key legacy fallback.
	token, err := primaryzone.SourceToken(config)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := f.runtime.RetireSource(t.Context(), old.Spec.Zone, zk.LifecycleSource{UID: string(config.UID), ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest, Removed: true})
	if err != nil {
		t.Fatal(err)
	}
	unavailable(t, mustPrepare(t, f.options(f.image())), "new.example.", f.now)
	next, err := pr.NewLifecycle(old.Spec.Zone.ClusterID, moved, nil)
	if err != nil {
		t.Fatal(err)
	}
	next.Spec.Sequence, next.Spec.Incarnation = retired.Spec.Sequence+1, retired.Spec.Incarnation+1
	next, err = f.runtime.Transition(t.Context(), retired, next)
	if err != nil {
		t.Fatal(err)
	}
	unavailable(t, mustPrepare(t, f.options(f.image())), "new.example.", f.now)
	fenced, err := f.runtime.FenceParents(t.Context(), next.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	active := fenced.DeepCopy()
	active.Spec.Sequence++
	active.Spec.Phase = zk.PhaseActive
	if _, err := f.runtime.Transition(t.Context(), fenced, active); err != nil {
		t.Fatal(err)
	}
	if p := mustPrepare(t, f.options(f.image())); p.Zones(f.now).Match("x.new.example.").Type() != public.ZoneTypeForward || p.Zones(f.now).Match("x.old.example.") != nil {
		t.Fatal("replacement did not move source-key membership")
	}
	if held.Zones(f.now).Match("x.old.example.").Type() != public.ZoneTypeForward || held.Zones(f.now).Match("x.new.example.") != nil {
		t.Fatal("held old cut changed")
	}
}
