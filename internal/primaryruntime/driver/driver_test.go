package driver

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Explicit TEST-ONLY immutable policy guard. Never exported as production proof.
type testGuard struct {
	mu      sync.RWMutex
	revoked bool
}

func (g *testGuard) WithCurrent(ctx context.Context, _ pr.PublicationFence, fn func(sqliteadapter.BackgroundPolicy) error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.revoked {
		return pr.ErrUnavailable
	}
	return fn(func(_ context.Context, _ *sqlite.CommandTx, c sqliteadapter.BackgroundCheck) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.Policy != nil && c.Policy.Enabled {
			return pr.ErrUnavailable
		}
		return nil
	})
}

// This test waiter checks one actual complete SQLite image; root owns real
// protected serving Lease/routing acknowledgment in servingbuild.
type testPublication struct {
	f       *fixture
	held    atomic.Bool
	mu      sync.Mutex
	changed chan struct{}
}

func (p *testPublication) Wake() {}
func (p *testPublication) Subscribe() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}
func (p *testPublication) release() {
	p.held.Store(false)
	p.mu.Lock()
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}
func (p *testPublication) Wait(ctx context.Context, w Writer, m pr.Member) error {
	if p.held.Load() {
		return pr.ErrUnavailable
	}
	a := p.f.s.Authority()
	if !a.Writable() || !w.Term.Same(a.Term) || w.Incarnation != a.Incarnation {
		return pr.ErrUnavailable
	}
	image, _, err := p.f.s.ReadServingCandidate(ctx)
	if err != nil {
		return err
	}
	opts := p.f.image
	opts.Now = time.Now()
	prepared, err := pr.PrepareImage(ctx, *image, opts)
	if err != nil {
		return err
	}
	actual, ok := prepared.DurableMember(m.Zone)
	m.Published = false
	if !ok || !reflect.DeepEqual(actual, m) {
		return pr.ErrConflict
	}
	if m.Phase == zk.PhaseActive && !prepared.Admitted(m.Zone.Apex, time.Now()) {
		return pr.ErrUnavailable
	}
	return nil
}
func (p *testPublication) publish(ctx context.Context, _ pr.Cut, m pr.Member) error {
	w, ok := pr.PublicationFenceFrom(ctx)
	if !ok {
		return pr.ErrUnavailable
	}
	return p.Wait(ctx, w, m)
}

type sender struct {
	calls atomic.Int64
	fail  atomic.Bool
}

func (n *sender) Send(ctx context.Context, _ pr.Member, _ *zone.Zone) error {
	n.calls.Add(1)
	if n.fail.Load() {
		return pr.ErrUnavailable
	}
	return ctx.Err()
}

type fixture struct {
	t      *testing.T
	path   string
	s      *sqlite.Store
	r      *pr.Runtime
	a      *sqliteadapter.Adapter
	repo   *zonestate.Repository
	image  pr.ImageOptions
	writer Writer
	pub    *testPublication
	guard  *testGuard
	notify *sender
}

func setup(t *testing.T) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zk.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, s: st, path: path, guard: &testGuard{}, notify: &sender{}}
	f.writer = Writer{Term: st.Authority().Term, Incarnation: st.Authority().Incarnation}
	kinds := []replication.Kind{}
	known := scheme.KnownTypes(public.GroupVersion)
	for name, typ := range known {
		if _, ok := known[name+"List"]; !ok {
			continue
		}
		if _, ok := reflect.New(typ).Interface().(store.Object); ok {
			kinds = append(kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
		}
	}
	slices.SortFunc(kinds, func(a, b replication.Kind) int { return strings.Compare(a.Kind, b.Kind) })
	f.image = pr.ImageOptions{ClusterID: f.writer.Term.ClusterID, Scheme: scheme, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: kinds}, Now: time.Now()}
	f.pub = &testPublication{f: f, changed: make(chan struct{})}
	f.a, err = sqliteadapter.New(sqliteadapter.Options{Store: st, Background: f.guard})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := st.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	f.repo = zonestate.NewRepository(backend, zonestate.Limits{})
	f.r, err = pr.New(pr.Options{Store: st, Repository: f.repo, Reader: primaryzone.NewReader(st, primaryzone.ReaderOptions{PageSize: 1}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Publish: f.pub.publish, Notify: f.notify, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) ctx() context.Context {
	return pr.WithPublicationFence(context.Background(), f.writer)
}
func (f *fixture) zone(name, apex string) *public.DNSZone {
	f.t.Helper()
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: apex, Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + apex}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	obj, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		f.t.Fatal(err)
	}
	return obj.(*public.DNSZone)
}
func (f *fixture) start() (*Driver, func()) {
	f.t.Helper()
	d, err := New(Options{Store: f.s, Runtime: f.r, Image: f.image, Publication: f.pub, PollInterval: 10 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, f.writer) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) && !errors.Is(err, pr.ErrUnavailable) {
				f.t.Error(err)
			}
		case <-time.After(2 * time.Second):
			f.t.Error("unbounded shutdown")
		}
	}
	return d, stop
}
func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not converge")
}
func (f *fixture) active(apex string, inc uint64) bool {
	m, ok := f.r.Capture(time.Now()).Member(apex)
	return ok && m.Phase == zk.PhaseActive && m.Incarnation == inc && m.Published
}
func (f *fixture) ledger(name string) *zk.ZoneLifecycle {
	var l zk.ZoneLifecycle
	if err := f.s.Get(f.ctx(), store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(f.writer.Term.ClusterID, name)}, &l); err != nil {
		return nil
	}
	return &l
}

func TestActualNewLateParentAndParentRemovalSurvivingChild(t *testing.T) {
	f := setup(t)
	f.zone("child", "child.example.")
	d, stop := f.start()
	defer stop()
	await(t, func() bool { return f.active("child.example.", 1) })
	f.zone("parent", "example.")
	d.Wake()
	await(t, func() bool { return f.active("example.", 1) && f.active("child.example.", 1) })
	child := f.ledger("child")
	if len(child.Spec.Parents) != 1 || child.Spec.Parents[0].CommitSequence == 0 {
		t.Fatal(child.Spec.Parents)
	}
	parent := f.ledger("parent")
	if err := f.s.Delete(f.ctx(), store.Key{Kind: "DNSZone", Name: "parent"}, store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	d.Wake()
	await(t, func() bool { return f.active("child.example.", 2) })
	terminal, err := f.repo.Read(f.ctx(), parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
		t.Fatal("parent head not terminal")
	}
	reserved := false
	for _, p := range terminal.Aggregate.Reservations {
		if p.Name == "child.example." && p.Source == "system" {
			reserved = true
		}
	}
	if !reserved {
		t.Fatal("terminal release mutated old reservation")
	}
	if child := f.ledger("child"); len(child.Spec.Parents) != 0 {
		t.Fatal(child.Spec.Parents)
	}
	status := d.Status()
	if !status.Ready || len(status.Zones) != 2 {
		t.Fatal(status)
	}
	status.Zones[0].Pending = "mutated"
	if d.Status().Zones[0].Pending == "mutated" {
		t.Fatal("status aliased")
	}
}

func TestActualConcurrentCreationTypeReplacementAndMovedApex(t *testing.T) {
	f := setup(t)
	z := f.zone("child", "child.example.")
	f.zone("parent", "example.")
	d, stop := f.start()
	defer stop()
	await(t, func() bool { return f.active("example.", 1) && f.active("child.example.", 1) })
	z.Spec.Type = public.ZoneTypeForward
	z.Spec.Forward = &public.ForwardSpec{UpstreamGroup: "default"}
	obj, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: z.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	z = obj.(*public.DNSZone)
	d.Wake()
	await(t, func() bool { return f.active("child.example.", 2) })
	l := f.ledger("child")
	if l.Spec.Type != public.ZoneTypeForward {
		t.Fatal(l.Spec.Type)
	}
	z.Spec.ZoneName = "moved.other."
	if _, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: z.ResourceVersion}); err == nil {
		t.Fatal("ordinary mutation changed immutable apex")
	}
	if err := f.s.Delete(f.ctx(), store.KeyOf(z), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	z.ObjectMeta = metav1.ObjectMeta{Name: z.Name}
	obj, err = f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		t.Fatal(err)
	}
	z = obj.(*public.DNSZone)
	d.Wake()
	await(t, func() bool { return f.active(z.Spec.ZoneName, 3) })
	if _, ok := f.r.Capture(time.Now()).Member("child.example."); ok {
		t.Fatal("stale moved membership")
	}
	if err := f.s.Delete(f.ctx(), store.KeyOf(z), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { l := f.ledger("child"); return l != nil && l.Spec.Phase == zk.PhaseRetired })
	replacement := f.zone("child", "moved.other.")
	d.Wake()
	await(t, func() bool { return f.active(replacement.Spec.ZoneName, 4) })
	if f.ledger("child").Spec.Zone.UID == l.Spec.Zone.UID {
		t.Fatal("delete/recreate reused UID")
	}
}

func TestHeldPublicationRecoveryNoRecommitAndCompleteRestart(t *testing.T) {
	f := setup(t)
	f.zone("one", "one.example.")
	f.zone("two", "two.example.")
	f.pub.held.Store(true)
	d, stop := f.start()
	await(t, func() bool {
		a, b := f.ledger("one"), f.ledger("two")
		return a != nil && b != nil && a.Spec.Phase == zk.PhaseFencing && b.Spec.Phase == zk.PhaseFencing && a.Spec.Sequence > 1 && b.Spec.Sequence > 1
	})
	l := f.ledger("one")
	await(t, func() bool { v, err := f.repo.Read(f.ctx(), l.Spec.HeadZone()); return err == nil && v.Head != nil })
	v, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	f.pub.release()
	d.Wake()
	await(t, func() bool { return f.active("one.example.", 1) && f.active("two.example.", 1) })
	after, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if version := v.Head.Spec.CommitSequence; after.Head.Spec.CommitSequence != version {
		t.Fatal("publication retry recommitted semantics")
	}
	stop()
	// Fresh runtime has no live captured parts. One complete image restores both.
	r, err := pr.New(pr.Options{Store: f.s, Repository: f.repo, Reader: primaryzone.NewReader(f.s, primaryzone.ReaderOptions{}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Publish: f.pub.publish, Notify: f.notify})
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	_, stop2 := f.start()
	defer stop2()
	await(t, func() bool { return f.active("one.example.", 1) && f.active("two.example.", 1) })
}

func TestInvalidCompleteReadNeverPrunesAndNilGuardCannotInitialize(t *testing.T) {
	f := setup(t)
	f.zone("one", "one.example.")
	d, stop := f.start()
	await(t, func() bool { return f.active("one.example.", 1) })
	stop()
	bad := d.o
	bad.Image.Validation.Kinds = bad.Image.Validation.Kinds[1:]
	blocked, err := New(bad)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- blocked.Run(ctx, f.writer) }()
	await(t, func() bool { return blocked.Status().Pending != "" })
	if !f.active("one.example.", 1) {
		t.Fatal("read failure pruned previous roster")
	}
	cancel()
	<-done
	f2 := setup(t)
	f2.a, err = sqliteadapter.New(sqliteadapter.Options{Store: f2.s})
	if err != nil {
		t.Fatal(err)
	}
	f2.r, err = pr.New(pr.Options{Store: f2.s, Repository: f2.repo, Reader: primaryzone.NewReader(f2.s, primaryzone.ReaderOptions{}), LifecycleCAS: f2.a.LifecycleCAS, Admit: f2.a.Admission, Publish: f2.pub.publish})
	if err != nil {
		t.Fatal(err)
	}
	f2.zone("denied", "denied.example.")
	d2, stop2 := f2.start()
	defer stop2()
	await(t, func() bool { return len(d2.Status().Zones) > 0 && d2.Status().Zones[0].Pending == "unavailable" })
	if f2.ledger("denied") != nil {
		t.Fatal("nil guard wrote lifecycle")
	}
}

// A changed RRset is read by the real primary reader, not the planning image.
func TestActualRRsetSourceChangeUsesCommittedReader(t *testing.T) {
	f := setup(t)
	z := f.zone("primary", "example.")
	d, stop := f.start()
	defer stop()
	await(t, func() bool { return f.active("example.", 1) })
	rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "record", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "host.example.", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.1"}}}}}
	if _, err := f.s.Apply(f.ctx(), rs, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	d.Wake()
	await(t, func() bool {
		model := f.r.Capture(time.Now()).Match("host.example.")
		return model != nil && model.Lookup("host.example.", dns.TypeA).Kind == zone.Success
	})
}

func TestExactWriterLossStopsRunnerWithoutAdoptingSuccessor(t *testing.T) {
	f := setup(t)
	f.zone("one", "one.example.")
	d, stop := f.start()
	defer stop()
	await(t, func() bool { return f.active("one.example.", 1) })
	successor := f.writer.Term
	successor.Epoch++
	successor.PrimaryID = "other"
	successor.PromotionID = "successor"
	if _, err := f.s.ObserveAuthority(context.Background(), successor); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return !d.Status().Running })
	if d.Status().Writer != f.writer {
		t.Fatal("runner adopted successor")
	}
	if err := d.Run(context.Background(), f.writer); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("stale writer resumed", err)
	}
}

func TestPendingNotifySendSuccessAckFaultAndCompleteRestartDuplicatesSafely(t *testing.T) {
	f := setup(t)
	f.notify.fail.Store(true)
	z := f.zone("primary", "example.")
	d, stop := f.start()
	await(t, func() bool { return f.active("example.", 1) })
	rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "change", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "host.example.", RecordType: "A", Targets: []string{"192.0.2.1"}}}}}
	if _, err := f.s.Apply(f.ctx(), rs, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	d.Wake()
	l := f.ledger("primary")
	await(t, func() bool {
		v, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
		return err == nil && v.Head.Spec.NotifySequence != 0 && f.active("example.", 1)
	})
	stop()
	f.notify.fail.Store(false)
	pending, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_notify_ack BEFORE UPDATE ON objects WHEN NEW.kind='ZoneState' BEGIN SELECT RAISE(ABORT,'injected-notify-ack'); END`); err != nil {
		t.Fatal(err)
	}
	// Cancellation can interrupt a same-head local reinstall after its durable
	// no-op admission but before publication. Recovery must explicitly republish
	// before NOTIFY may send; local pending intent alone does not grant delivery.
	if err := f.r.Republish(f.ctx(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	before := f.notify.calls.Load()
	if err := f.r.NotifyOnce(f.ctx()); err == nil {
		t.Fatal("actual send/ack-fault unexpectedly succeeded")
	}
	unchanged, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil || !reflect.DeepEqual(pending, unchanged) {
		t.Fatal("failed ack mutated durable head", err)
	}
	if f.notify.calls.Load() != before+1 {
		t.Fatal("fault test did not actually send")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_notify_ack`); err != nil {
		t.Fatal(err)
	}
	f.r, err = pr.New(pr.Options{Store: f.s, Repository: f.repo, Reader: primaryzone.NewReader(f.s, primaryzone.ReaderOptions{}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Publish: f.pub.publish, Notify: f.notify})
	if err != nil {
		t.Fatal(err)
	}
	_, stop2 := f.start()
	defer stop2()
	await(t, func() bool {
		v, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
		return err == nil && v.Head.Spec.NotifySequence == 0 && f.active("example.", 1)
	})
	recovered, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if f.notify.calls.Load() < before+2 || recovered.Head.Spec.ContentHash != pending.Head.Spec.ContentHash || recovered.Head.Spec.Serial != pending.Head.Spec.Serial {
		t.Fatal("recovery repeated RR semantics or lost at-least-once intent")
	}
}

func TestRunnerBoundsAndWrongCapturedIncarnation(t *testing.T) {
	f := setup(t)
	options := Options{Store: f.s, Runtime: f.r, Image: f.image, Publication: f.pub, MaxWork: 129}
	if _, err := New(options); !errors.Is(err, pr.ErrCapacity) {
		t.Fatal(err)
	}
	options.MaxWork = 1
	options.MaxZones = 1
	d, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	wrong := f.writer
	wrong.Incarnation++
	if err := d.Run(context.Background(), wrong); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("wrong incarnation accepted", err)
	}
	f.zone("one", "one.example.")
	f.zone("two", "two.example.")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, f.writer) }()
	await(t, func() bool { return d.Status().Pending == "capacity" })
	cancel()
	<-done
	if f.ledger("one") != nil || f.ledger("two") != nil {
		t.Fatal("partial oversized roster initialized")
	}
}
