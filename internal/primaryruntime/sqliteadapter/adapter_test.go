package sqliteadapter

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Explicit test issuer/guard; production uses root's private VerifiedTSIG proof
// and immutable keyring publication guard. These tests never label raw names
// as production verification capabilities.
type issued struct{ p pr.ProofIdentity }
type issuer struct{}

func (issuer) Verify(_ context.Context, v any, wire []byte) (pr.ProofIdentity, error) {
	p, ok := v.(*issued)
	if !ok || p == nil {
		return pr.ProofIdentity{}, pr.ErrInvalid
	}
	d, err := pr.SemanticDigest(wire)
	if err != nil || d != p.p.SemanticDigest {
		return pr.ProofIdentity{}, pr.ErrInvalid
	}
	return p.p, nil
}

type currentKeys struct {
	mu      sync.RWMutex
	current CurrentCredential
	calls   atomic.Int64
	entered func()
}

func (k *currentKeys) WithCurrent(_ context.Context, _ pr.ProofIdentity, fn func(CurrentCredential) error) error {
	k.mu.RLock()
	defer k.mu.RUnlock()
	k.calls.Add(1)
	if k.entered != nil {
		k.entered()
	}
	return fn(k.current)
}
func (k *currentKeys) rotate(v CurrentCredential) { k.mu.Lock(); defer k.mu.Unlock(); k.current = v }

type fixture struct {
	t      *testing.T
	path   string
	s      *sqlite.Store
	a      *Adapter
	r      *pr.Runtime
	repo   *zonestate.Repository
	keys   *currentKeys
	policy zonestate.Policy
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
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{t: t, path: path, s: s, keys: &currentKeys{current: CurrentCredential{Principal: dynamicupdate.Principal{Key: "key.", Version: "v1"}, ProviderVersion: "provider-v1", Algorithm: dns.HmacSHA256}}, policy: zonestate.Policy{Enabled: true, AuthEpoch: 1, Credentials: []dynamicupdate.Credential{{Key: "key.", Version: "v1"}}, Grants: []dynamicupdate.Grant{{Key: "key.", Owner: "*.example."}}}}
	f.a, err = New(Options{Store: s, Credentials: f.keys, Policy: func(_ context.Context, _ *sqlite.CommandTx, z *public.DNSZone, p zonestate.Policy) error {
		if z == nil || z.Spec.Policy.Update != public.AccessAllow {
			return pr.ErrUnavailable
		}
		expected := f.policy
		expected.AuthEpoch = p.AuthEpoch
		if !reflect.DeepEqual(p, expected) {
			return pr.ErrUnavailable
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := s.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	f.repo = zonestate.NewRepository(backend, zonestate.Limits{})
	f.r, err = pr.New(pr.Options{Store: s, Repository: f.repo, Reader: primaryzone.NewReader(s, primaryzone.ReaderOptions{PageSize: 1, Policy: func(context.Context, *public.DNSZone) (*zonestate.Policy, error) { p := f.policy; return &p, nil }}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Proofs: issuer{}, Publish: func(context.Context, pr.Cut, pr.Member) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) zone(name, apex string) *public.DNSZone {
	f.t.Helper()
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: apex, Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + apex}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow, Update: public.AccessAllow}}}
	saved, err := f.s.Apply(context.Background(), z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		f.t.Fatal(err)
	}
	return saved.(*public.DNSZone)
}
func (f *fixture) intent(z *public.DNSZone, parents ...*zk.ZoneLifecycle) *zk.ZoneLifecycle {
	f.t.Helper()
	l, err := pr.NewLifecycle(f.s.Authority().Term.ClusterID, z, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, p := range parents {
		l.Spec.Parents = append(l.Spec.Parents, pr.ParentObligation(p, l))
	}
	return l
}
func (f *fixture) begin(z *public.DNSZone, parents ...*zk.ZoneLifecycle) *zk.ZoneLifecycle {
	f.t.Helper()
	l, err := f.r.Transition(context.Background(), nil, f.intent(z, parents...))
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}
func (f *fixture) active(name, apex string) *zk.ZoneLifecycle {
	f.t.Helper()
	l := f.begin(f.zone(name, apex))
	if _, err := f.r.Reconcile(context.Background(), l.Spec.Zone); err != nil {
		f.t.Fatal(err)
	}
	l, err := f.r.ReadLifecycle(context.Background(), l.Spec.Zone)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}
func (f *fixture) request(l *zk.ZoneLifecycle, retry, text string) pr.VerifiedUpdate {
	f.t.Helper()
	m := new(dns.Msg)
	m.SetUpdate(l.Spec.Zone.Apex)
	rr, err := dns.NewRR(text)
	if err != nil {
		f.t.Fatal(err)
	}
	m.Ns = []dns.RR{rr}
	wire, err := m.Pack()
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := pr.SemanticDigest(wire)
	if err != nil {
		f.t.Fatal(err)
	}
	p := pr.ProofIdentity{Principal: dynamicupdate.Principal{Key: "key.", Version: "v1"}, ProviderVersion: "provider-v1", Algorithm: dns.HmacSHA256, Transport: "tcp", Domain: "dns-update:" + zk.HeadName(l.Spec.Zone), Subject: "key.", RetryID: retry, SemanticDigest: digest, Term: f.s.Authority().Term, NotAfter: time.Now().Add(time.Minute), AuthorityIncarnation: f.s.Authority().Incarnation}
	return pr.VerifiedUpdate{Zone: l.Spec.Zone, Wire: wire, Proof: &issued{p: p}}
}
func snapshot(t *testing.T, s *sqlite.Store) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := s.Snapshot(context.Background(), &b, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestActualTransactionsUpdateReplayAndCurrentCredential(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	l := f.active("primary", "example.")
	req := f.request(l, "retry", "dynamic.example. 60 IN A 192.0.2.1")
	out := f.r.Apply(ctx, req)
	if out.Rcode != dns.RcodeSuccess || !out.Committed || !out.Published {
		t.Fatal(out)
	}
	before := snapshot(t, f.s)
	calls := f.keys.calls.Load()
	out = f.r.Apply(ctx, req)
	if out.Rcode != dns.RcodeSuccess || !out.Deduplicated || f.keys.calls.Load() <= calls || !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("dedup was not fenced/noop", out)
	}
	f.keys.rotate(CurrentCredential{Principal: dynamicupdate.Principal{Key: "key.", Version: "v2"}, ProviderVersion: "provider-v2", Algorithm: dns.HmacSHA256})
	for _, r := range []pr.VerifiedUpdate{req, f.request(l, "noop", "dynamic.example. 60 IN A 192.0.2.1"), f.request(l, "retry", "other.example. 60 IN A 192.0.2.2")} {
		if got := f.r.Apply(ctx, r); got.Rcode == dns.RcodeSuccess || got.Published {
			t.Fatal("revoked version admitted", got)
		}
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("revocation mutated durable state")
	}
}
func TestExactSourceLifecycleCASAndCompleteReconcile(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	l := f.active("primary", "example.")
	in, err := primaryzone.NewReader(f.s, primaryzone.ReaderOptions{}).ReadDesired(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "late", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "primary"}, Endpoints: []public.Endpoint{{DNSName: "static", RecordType: "A", Targets: []string{"192.0.2.8"}}}}}
	if _, err := f.s.Apply(ctx, rs, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.Admission(ctx, pr.Command{Lifecycle: l, Expected: v.Expected, Reason: "source-reconcile", Desired: in}); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("incomplete source list admitted", err)
	}
	next := l.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Source.ResourceVersion = "substituted"
	if _, err := f.a.LifecycleCAS(ctx, l, next); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("fresh source RV substitution", err)
	}
	wrong := l.DeepCopy()
	wrong.ResourceVersion = "different-opaque"
	next = l.DeepCopy()
	next.Spec.Sequence++
	if _, err := f.a.LifecycleCAS(ctx, wrong, next); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("stale exact lifecycle admitted", err)
	}
	if _, err := f.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
}
func TestLateParentAndConcurrentCreationCannotEscapeRoster(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	child := f.active("child", "child.example.")
	parent := f.begin(f.zone("parent", "example."))
	if _, err := f.r.Prepare(ctx, parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	parent, err := f.r.ReadLifecycle(ctx, parent.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	next := parent.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Phase = zk.PhaseActive
	if _, err := f.a.LifecycleCAS(ctx, parent, next); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("late parent activated unfenced", err)
	}
	if _, err := f.r.AdmitAncestor(ctx, parent.Spec.Zone, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Activate(ctx, parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	parent, err = f.r.ReadLifecycle(ctx, parent.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	// All attempts race through actual CommandTx, never fake preflight guards.
	source := f.zone("new-child", "new.example.")
	bare := f.intent(source)
	results := make(chan error, 2)
	go func() { _, e := f.a.LifecycleCAS(ctx, nil, bare); results <- e }()
	go func() {
		out := f.r.Apply(ctx, f.request(parent, "race", "x.new.example. 60 IN A 192.0.2.1"))
		results <- out.Err
	}()
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			t.Fatal("unfenced concurrent child/parent command admitted")
		}
	}
	bound := f.intent(source, parent)
	newChild, err := f.a.LifecycleCAS(ctx, nil, bound)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, newChild.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if out := f.r.Apply(ctx, f.request(parent, "blocked", "x.new.example. 60 IN A 192.0.2.1")); out.Rcode == dns.RcodeSuccess {
		t.Fatal("parent UPDATE crossed child boundary")
	}
}
func TestSQLiteStatementFailureRollsBackHeadChunksReceiptAndNotify(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	l := f.active("primary", "example.")
	before := snapshot(t, f.s)
	authority := f.s.Authority()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_primary_head BEFORE UPDATE ON objects WHEN NEW.kind='ZoneState' BEGIN SELECT RAISE(ABORT,'injected-primary-head'); END`); err != nil {
		t.Fatal(err)
	}
	if out := f.r.Apply(ctx, f.request(l, "fault", "dynamic.example. 60 IN A 192.0.2.1")); out.Rcode != dns.RcodeServerFailure || out.Committed {
		t.Fatal("fault acknowledged durable commit", out)
	}
	if !bytes.Equal(before, snapshot(t, f.s)) || !reflect.DeepEqual(authority, f.s.Authority()) {
		t.Fatal("failed tx leaked chunks/head/receipt/notify/audit/revision")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_primary_head`); err != nil {
		t.Fatal(err)
	}
	if out := f.r.Apply(ctx, f.request(l, "fault", "dynamic.example. 60 IN A 192.0.2.1")); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
}
func TestCredentialRotationPinsTheWholeCommand(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	l := f.active("primary", "example.")
	entered := make(chan struct{})
	release := make(chan struct{})
	original := f.a.o.Policy
	f.a.o.Policy = func(ctx context.Context, tx *sqlite.CommandTx, z *public.DNSZone, p zonestate.Policy) error {
		close(entered)
		<-release
		return original(ctx, tx, z, p)
	}
	applied := make(chan pr.UpdateOutcome, 1)
	req := f.request(l, "rotation", "dynamic.example. 60 IN A 192.0.2.1")
	go func() { applied <- f.r.Apply(ctx, req) }()
	<-entered
	rotating := make(chan struct{})
	rotated := make(chan struct{})
	go func() {
		close(rotating)
		f.keys.rotate(CurrentCredential{Principal: dynamicupdate.Principal{Key: "key.", Version: "v2"}, ProviderVersion: "provider-v2", Algorithm: dns.HmacSHA256})
		close(rotated)
	}()
	<-rotating
	select {
	case <-rotated:
		t.Fatal("rotation crossed pending command")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	out := <-applied
	if out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	<-rotated
	f.a.o.Policy = original
	if out := f.r.Apply(ctx, req); out.Rcode == dns.RcodeSuccess {
		t.Fatal("post-rotation dedup admitted")
	}
}

func TestConcurrentParentActivationAndChildCreationUseSameRosterTransaction(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	parent := f.begin(f.zone("parent", "example."))
	if _, err := f.r.Prepare(ctx, parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	parent, err := f.r.ReadLifecycle(ctx, parent.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	activation := parent.DeepCopy()
	activation.Spec.Sequence++
	activation.Spec.Phase = zk.PhaseActive
	childSource := f.zone("child", "child.example.")
	// Both candidates were prepared without each other's durable boundary. Exact
	// head/object CAS alone would admit them; the transactional roster must not.
	bare := f.intent(childSource)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; _, err := f.a.LifecycleCAS(ctx, parent, activation); results <- err }()
	go func() { <-start; _, err := f.a.LifecycleCAS(ctx, nil, bare); results <- err }()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; !errors.Is(err, pr.ErrConflict) {
			t.Fatal("unfenced concurrent admission", err)
		}
	}
	child, err := f.a.LifecycleCAS(ctx, nil, f.intent(childSource, parent))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.AdmitAncestor(ctx, parent.Spec.Zone, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Activate(ctx, parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
}
func TestCapturedDomainTermAndIncarnationDenyEveryResponse(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	l := f.active("primary", "example.")
	req := f.request(l, "retry", "dynamic.example. 60 IN A 192.0.2.1")
	if out := f.r.Apply(ctx, req); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	before := snapshot(t, f.s)
	for _, mutate := range []func(*pr.ProofIdentity){
		func(p *pr.ProofIdentity) { p.AuthorityIncarnation++ },
		func(p *pr.ProofIdentity) { p.Domain = "wrong-domain" },
		func(p *pr.ProofIdentity) { p.Subject = "another-key." },
		func(p *pr.ProofIdentity) { p.Term.PromotionID = "same-epoch-other-promotion" },
	} {
		copied := *req.Proof.(*issued)
		mutate(&copied.p)
		r := req
		r.Proof = &copied
		if out := f.r.Apply(ctx, r); out.Rcode == dns.RcodeSuccess || out.Published {
			t.Fatal("bad current authority/domain dedup", out)
		}
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("rejected authority/domain mutated durable state")
	}
	successor := f.s.Authority().Term
	successor.Epoch++
	successor.PrimaryID = "successor"
	successor.PromotionID = "next"
	if _, err := f.s.ObserveAuthority(ctx, successor); err != nil {
		t.Fatal(err)
	}
	if out := f.r.Apply(ctx, req); out.Rcode == dns.RcodeSuccess || out.Published {
		t.Fatal("follower dedup response", out)
	}
}

func TestProofDeadlineExpiresWhileWaitingForActualWriterTransaction(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	l := f.active("primary", "example.")
	before := snapshot(t, f.s)
	req := f.request(l, "deadline", "dynamic.example. 60 IN A 192.0.2.1")
	req.Proof.(*issued).p.NotAfter = time.Now().Add(500 * time.Millisecond)
	guardEntered := make(chan struct{})
	f.keys.entered = func() { close(guardEntered) }
	locked := make(chan struct{})
	release := make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- f.s.Command(ctx, f.s.Authority().Term, func(context.Context, *sqlite.CommandTx) error { close(locked); <-release; return nil })
	}()
	<-locked
	out := make(chan pr.UpdateOutcome, 1)
	go func() { out <- f.r.Apply(ctx, req) }()
	select {
	case <-guardEntered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("admission never reached the current credential guard")
	}
	timer := time.NewTimer(time.Until(req.Proof.(*issued).p.NotAfter) + 15*time.Millisecond)
	<-timer.C
	close(release)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	result := <-out
	f.keys.entered = nil
	if result.Rcode != dns.RcodeServerFailure || result.Committed || result.Published {
		t.Fatal("expired stalled admission committed", result)
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("expired command mutated head/chunks/receipt/notify/audit")
	}
	for _, change := range []func(*pr.ProofIdentity){func(p *pr.ProofIdentity) { p.NotAfter = time.Time{} }, func(p *pr.ProofIdentity) { p.NotAfter = time.Now().Add(-time.Second) }} {
		r := f.request(l, "invalid-deadline", "dynamic.example. 60 IN A 192.0.2.1")
		change(&r.Proof.(*issued).p)
		if result := f.r.Apply(ctx, r); result.Rcode == dns.RcodeSuccess || result.Committed {
			t.Fatal("missing/expired proof deadline admitted", result)
		}
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if result := f.r.Apply(ended, f.request(l, "ended", "dynamic.example. 60 IN A 192.0.2.1")); result.Rcode == dns.RcodeSuccess || result.Committed {
		t.Fatal("ended request context admitted", result)
	}
}

func TestActualRetirementUsesCurrentDeleteObservationAndReleasesBoundary(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	parent := f.active("parent", "example.")
	childSource := f.zone("child", "child.example.")
	child := f.begin(childSource, parent)
	if _, err := f.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	token, err := primaryzone.SourceToken(childSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(ctx, store.KeyOf(childSource), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	retired, err := f.r.RetireSource(ctx, child.Spec.Zone, zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest, Removed: true})
	if err != nil {
		t.Fatal(err)
	}
	if retired.Spec.Phase != zk.PhaseRetired || !retired.Spec.Source.Removed {
		t.Fatal("delete retirement")
	}
	dead, err := f.repo.Read(ctx, child.Spec.HeadZone())
	if err != nil || dead.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
		t.Fatal("durable tombstone", err)
	}
	if _, err := f.r.ReleaseParents(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	p, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil || fence(p, child.Spec.Zone.Apex) {
		t.Fatal("released boundary", err)
	}
	if out := f.r.Apply(ctx, f.request(parent, "released", "below.child.example. 60 IN A 192.0.2.1")); out.Rcode != dns.RcodeSuccess {
		t.Fatal("retired child still reserved", out)
	}
	// Same object key with a fresh UID cannot adopt old retired authority.
	replacement := f.zone("child", "child.example.")
	if string(replacement.UID) == child.Spec.Zone.UID {
		t.Fatal("store did not recreate source UID")
	}
	next := f.intent(replacement, parent)
	next.Spec.Incarnation = 2
	next.Spec.Parents = []zk.ParentFence{pr.ParentObligation(parent, next)}
	current, err := f.r.ReadLifecycle(ctx, child.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	next.Spec.Sequence = current.Spec.Sequence + 1
	next, err = f.r.Transition(ctx, current, next)
	if err != nil {
		t.Fatal(err)
	}
	if next.Spec.HeadZone() == dead.Head.Spec.Zone {
		t.Fatal("delete/recreate reused tombstoned head")
	}
	// Existing parent dynamic data in the new subtree must block the new fence,
	// preserving both old dynamic data and unavailable new child coverage.
	beforeParent, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, next.Spec.Zone); !errors.Is(err, pr.ErrConflict) {
		t.Fatalf("new child over existing dynamic boundary: %v, stored=%+v expected=%+v", err, next.Spec.Parents, pr.ParentObligation(parent, next))
	}
	afterParent, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil || afterParent.Head.Spec != beforeParent.Head.Spec {
		t.Fatal("failed boundary changed parent data/head", err)
	}
	if _, err := f.repo.Read(ctx, next.Spec.HeadZone()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("failed boundary initialized replacement authority", err)
	}
}

func TestUnavailableGuardsAndMalformedPreparedFailClosed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	l := f.active("primary", "example.")
	before := snapshot(t, f.s)
	v, err := f.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.Admission(ctx, pr.Command{Lifecycle: l, Expected: v.Expected, Prepared: &zonestate.Prepared{}, Reason: "tombstone"}); err == nil {
		t.Fatal("zero prepared admitted")
	}
	request := f.request(l, "guard", "dynamic.example. 60 IN A 192.0.2.1")
	guard := f.a.o.Credentials
	f.a.o.Credentials = nil
	if out := f.r.Apply(ctx, request); out.Rcode != dns.RcodeServerFailure || out.Committed {
		t.Fatal("nil current credential guard admitted UPDATE", out)
	}
	f.a.o.Credentials = guard
	policy := f.a.o.Policy
	f.a.o.Policy = nil
	if out := f.r.Apply(ctx, request); out.Rcode != dns.RcodeServerFailure || out.Committed {
		t.Fatal("nil current policy guard admitted UPDATE", out)
	}
	if _, err := f.r.Reconcile(ctx, l.Spec.Zone); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("enabled source policy admitted without guard", err)
	}
	f.a.o.Policy = policy
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("malformed/unavailable guard paths mutated durable state")
	}
}

func TestReconcileFencingConflictRecoversAfterExplicitRootCleanup(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	parent := f.active("parent", "example.")
	if out := f.r.Apply(ctx, f.request(parent, "existing", "below.child.example. 60 IN A 192.0.2.1")); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	child := f.begin(f.zone("child", "child.example."), parent)
	before, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := f.r.Reconcile(ctx, child.Spec.Zone); !errors.Is(err, pr.ErrConflict) {
			t.Fatal("semantic conflict changed on fencing retry", err)
		}
		current, err := f.r.ReadLifecycle(ctx, child.Spec.Zone)
		if err != nil || current.Spec.Phase != zk.PhaseFencing || current.Spec.Parents[0].CommitSequence != 0 {
			t.Fatal("unfenced child activated", err)
		}
		if _, err := f.repo.Read(ctx, child.Spec.HeadZone()); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("semantic conflict initialized child head", err)
		}
		after, err := f.repo.Read(ctx, parent.Spec.HeadZone())
		if err != nil || after.Head.Spec != before.Head.Spec {
			t.Fatal("semantic conflict mutated parent", err)
		}
	}
	// Explicit fixture for root's privileged audited cleanup seam. This is NOT
	// an ordinary network UPDATE bypass or an adapter production capability.
	// A real root cleanup command must authorize this transaction separately.
	m := new(dns.Msg)
	m.SetUpdate("example.")
	m.Ns = []dns.RR{&dns.RR_Header{Name: "below.child.example.", Rrtype: dns.TypeANY, Class: dns.ClassANY}}
	tx, pe := dynamicupdate.Parse(m, dynamicupdate.Limits{})
	if pe != nil {
		t.Fatal(pe)
	}
	state, err := before.UpdateState()
	if err != nil {
		t.Fatal(err)
	}
	plan, pe := dynamicupdate.Plan(state, tx, dynamicupdate.Principal{Key: "key.", Version: "v1"}, time.Now(), dynamicupdate.Limits{})
	if pe != nil {
		t.Fatal(pe)
	}
	aggregate, err := zonestate.ApplyResult(before.Aggregate, plan, dynamicupdate.Principal{Key: "key.", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := zonestate.Prepare(aggregate, before.Head, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Command(ctx, before.Expected.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.CommitZoneState(ctx, before.Expected, prepared, zonestate.Limits{}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal("fencing recovery did not progress", err)
	}
	active, err := f.r.ReadLifecycle(ctx, child.Spec.Zone)
	if err != nil || active.Spec.Phase != zk.PhaseActive || active.Spec.Parents[0].CommitSequence == 0 {
		t.Fatal("recovery lost witness/activation", err)
	}
	p, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil || !fence(p, child.Spec.Zone.Apex) {
		t.Fatal("recovered parent fence missing", err)
	}
}
