package servingbuild

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Explicit fixture admission; production must pin current credentials/policy
// through the entire command. This guard exists only in this test file.
type publicationTestGuard struct{}

func (publicationTestGuard) WithCurrent(_ context.Context, _ pr.PublicationFence, fn func(sqliteadapter.BackgroundPolicy) error) error {
	return fn(func(context.Context, *sqlite.CommandTx, sqliteadapter.BackgroundCheck) error { return nil })
}

func primaryFixture(t *testing.T, f *fixture) (*pr.Runtime, *PrimaryPublication, context.Context) {
	t.Helper()
	p, err := NewPrimaryPublication(f.pub, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	a, err := sqliteadapter.New(sqliteadapter.Options{Store: f.store, Background: publicationTestGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := f.store.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := pr.New(pr.Options{Store: f.store, Repository: zonestate.NewRepository(backend, zonestate.Limits{}), Reader: primaryzone.NewReader(f.store, primaryzone.ReaderOptions{}), LifecycleCAS: a.LifecycleCAS, Admit: a.Admission, Publish: p.Publish})
	if err != nil {
		t.Fatal(err)
	}
	authority := f.store.Authority()
	ctx := pr.WithPublicationFence(t.Context(), pr.PublicationFence{Term: authority.Term, Incarnation: authority.Incarnation})
	return r, p, ctx
}

func beginPrimary(ctx context.Context, t *testing.T, f *fixture, r *pr.Runtime) (*public.DNSZone, *zk.ZoneLifecycle) {
	t.Helper()
	z := &public.DNSZone{}
	if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Nameservers = []string{"ns." + z.Spec.ZoneName}
	object, err := f.store.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	z = object.(*public.DNSZone)
	l, err := pr.NewLifecycle(f.store.Info().ClusterID, z, nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err = r.Transition(ctx, nil, l)
	if err != nil {
		t.Fatal(err)
	}
	return z, l
}

func TestPrimaryPublicationWaitsForCompleteCommitAndRecovery(t *testing.T) {
	f := newFixture(t)
	r, p, ctx := primaryFixture(t, f)
	_, lifecycle := beginPrimary(ctx, t, f, r)
	f.blocked.Store(true)
	f.epoch.Add(1)
	p.Wake()
	bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	view, err := r.Reconcile(bounded, lifecycle.Spec.Zone)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || view == nil || view.Head == nil {
		t.Fatalf("durable commit must remain publication-pending: view=%v err=%v", view != nil, err)
	}
	member, ok := r.Capture(f.now).Member(lifecycle.Spec.Zone.Apex)
	if !ok || member.Published || member.Phase != zk.PhaseActive {
		t.Fatal("local runtime falsely acknowledged blocked publication", member)
	}
	before := f.store.Authority().Installed.Revision
	f.blocked.Store(false)
	p.Wake()
	writer, _ := pr.PublicationFenceFrom(ctx)
	bounded, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Wait(bounded, writer, member); err != nil {
		t.Fatal(err)
	}
	if f.store.Authority().Installed.Revision != before {
		t.Fatal("publication recovery repeated durable semantics")
	}
	answer := query(t, f.chain, nil, member.Zone.Apex)
	if answer.Msg.Rcode != dns.RcodeSuccess || !answer.Msg.Authoritative {
		t.Fatal("acknowledged primary is not actually served", answer.Msg)
	}
	if err := p.Publish(ctx, pr.Cut{}, member); err != nil {
		t.Fatal("callback must use complete bundle, not local Cut", err)
	}
	if err := p.Publish(t.Context(), r.Capture(f.now), member); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("missing captured writer accepted", err)
	}
	for _, alter := range []func(*pr.Member){
		func(m *pr.Member) { m.Incarnation++ },
		func(m *pr.Member) { m.Sequence++ },
		func(m *pr.Member) { m.Type = public.ZoneTypeForward },
		func(m *pr.Member) { m.Version.CommitSequence++ },
		func(m *pr.Member) { m.Parents = []zk.ParentFence{{Incarnation: 1}} },
	} {
		changed := member
		alter(&changed)
		if err := p.Wait(bounded, writer, changed); !errors.Is(err, serving.ErrSuperseded) {
			t.Fatal("different lifecycle/head acknowledged", err)
		}
	}
	writer.Incarnation++
	if err := p.Wait(bounded, writer, member); !errors.Is(err, serving.ErrSuperseded) {
		t.Fatal("different writer incarnation acknowledged", err)
	}
}

func TestPrimaryPublicationChecksCurrentLeaseAndDesiredReplacement(t *testing.T) {
	f := newFixture(t)
	r, p, ctx := primaryFixture(t, f)
	z, lifecycle := beginPrimary(ctx, t, f, r)
	if _, err := r.Reconcile(ctx, lifecycle.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	writer, _ := pr.PublicationFenceFrom(ctx)
	active, _ := r.Capture(f.now).Member(z.Spec.ZoneName)
	held, err := f.pub.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if err := f.store.Delete(ctx, store.KeyOf(z), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	if err := f.pub.CheckWriterLease(ctx, held, serving.TermActivation{Term: writer.Term, Incarnation: writer.Incarnation}); !errors.Is(err, serving.ErrPending) {
		t.Fatal("held old image remains eligible after a write", err)
	}
	replacement := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: z.Name}, Spec: z.Spec}
	object, err := f.store.Apply(ctx, replacement, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	if object.GetUID() == z.UID {
		t.Fatal("replacement reused UID")
	}
	token, err := primaryzone.SourceToken(z)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := r.RetireSource(ctx, lifecycle.Spec.Zone, zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest, Removed: true})
	if err != nil {
		t.Fatal(err)
	}
	if retired.Spec.Phase != zk.PhaseRetired {
		t.Fatal("retirement incomplete")
	}
	member, _ := r.Capture(f.now).Member(z.Spec.ZoneName)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Wait(bounded, writer, member); err != nil {
		t.Fatal("desired replacement hid durable retirement", err)
	}
	if err := p.Wait(bounded, writer, active); !errors.Is(err, serving.ErrSuperseded) {
		t.Fatal("old active member acknowledged after retirement", err)
	}
	lease, err := f.pub.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	value, _ := lease.Bundle().Component("runtime")
	image := value.(*RuntimeOwner).Primary()
	placeholder, exists := image.Cut(f.now).Member(z.Spec.ZoneName)
	if !exists || placeholder.Incarnation != 0 || placeholder.Zone.UID != string(object.GetUID()) {
		t.Fatal("replacement coverage not installed", placeholder)
	}
	if err := p.Wait(bounded, writer, placeholder); !errors.Is(err, pr.ErrInvalid) {
		t.Fatal("desired-only placeholder acknowledged", err)
	}
	if query(t, f.chain, nil, z.Spec.ZoneName).Msg.Rcode != dns.RcodeServerFailure {
		t.Fatal("retired data leaked through replacement coverage")
	}
}

func TestPrimaryPublicationExpiryAndAuthorityLossCannotAcknowledge(t *testing.T) {
	f := newFixture(t)
	r, p, ctx := primaryFixture(t, f)
	z, lifecycle := beginPrimary(ctx, t, f, r)
	expires := metav1.NewTime(f.now.Add(time.Hour))
	_, err := f.store.Apply(ctx, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "expiring", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "live." + z.Spec.ZoneName, RecordType: "A", Targets: []string{"192.0.2.9"}, Kudurru: &public.EndpointExt{ExpiresAt: &expires}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, lifecycle.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	member, _ := r.Capture(f.now).Member(z.Spec.ZoneName)
	writer, _ := pr.PublicationFenceFrom(ctx)
	p.now = func() time.Time { return expires.Time }
	bounded, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	err = p.Wait(bounded, writer, member)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired exact head acknowledged as serving", err)
	}
	// Keep publication ineligible and prove a writer fence wakes a waiting
	// acknowledgment without waiting for a successful newer preparation.
	f.blocked.Store(true)
	f.epoch.Add(1)
	bounded, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Wait(bounded, writer, member) }()
	higher := writer.Term
	higher.Epoch++
	higher.PrimaryID, higher.PromotionID = "new-primary", "new-promotion"
	if _, err := f.store.ObserveAuthority(ctx, higher); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, serving.ErrSuperseded) {
			t.Fatal("old writer did not stop", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fenced waiter blocked behind owner preparation")
	}
}
