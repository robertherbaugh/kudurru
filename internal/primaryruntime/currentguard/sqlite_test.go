package currentguard

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"k8s.io/apimachinery/pkg/runtime"
)

// Explicit TEST-ONLY dependency replacement gate. Actual root publisher/holder
// exclusion and native MAC verification are root integration responsibilities.
type testHolder struct {
	gate    chan struct{}
	current Pinned // protected by gate; all Keys are actual immutable providers
	writer  pr.PublicationFence
	entered chan struct{}
}

func (h *testHolder) pin(ctx context.Context, f pr.PublicationFence, fn func(Pinned) error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.gate:
	}
	defer func() { h.gate <- struct{}{} }()
	if !f.Term.Same(h.writer.Term) || f.Incarnation != h.writer.Incarnation {
		return pr.ErrUnavailable
	}
	if h.entered != nil {
		select {
		case h.entered <- struct{}{}:
		default:
		}
	}
	return fn(h.current)
}

func (h *testHolder) rotate(keys *dnsserver.StaticKeyring) {
	<-h.gate
	h.current.Keys = keys
	h.current.Identity += "+rotation"
	h.gate <- struct{}{}
}

type testIssued struct{ identity pr.ProofIdentity }
type testIssuer struct{}

func (testIssuer) Verify(ctx context.Context, opaque any, wire []byte) (pr.ProofIdentity, error) {
	issued, ok := opaque.(*testIssued)
	if !ok || issued == nil || ctx.Err() != nil {
		return pr.ProofIdentity{}, pr.ErrUnavailable
	}
	digest, err := pr.SemanticDigest(wire)
	if err != nil || digest != issued.identity.SemanticDigest {
		return pr.ProofIdentity{}, pr.ErrUnavailable
	}
	return issued.identity, nil
}

type sqliteFixture struct {
	t     *testing.T
	s     *sqlite.Store
	h     *testHolder
	g     *Owner
	a     *sqliteadapter.Adapter
	r     *pr.Runtime
	repo  *zonestate.Repository
	fence pr.PublicationFence
	extra sqliteadapter.PolicyGuard // explicit TEST-ONLY optional admission fault
}

func newSQLiteFixture(t *testing.T) *sqliteFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zk.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "current.db"), sqlite.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &sqliteFixture{t: t, s: s}
	f.fence = pr.PublicationFence{Term: s.Authority().Term, Incarnation: s.Authority().Incarnation}
	f.h = &testHolder{gate: make(chan struct{}, 1), current: Pinned{Identity: "test-host-v1", Keys: testRing(t, "first")}, writer: f.fence}
	f.h.gate <- struct{}{}
	f.g, err = New(Options{Pin: f.h.pin})
	if err != nil {
		t.Fatal(err)
	}
	f.a, err = sqliteadapter.New(sqliteadapter.Options{Store: s, Background: f.g.Background(), Credentials: f.g.Credentials(), Policy: func(ctx context.Context, tx *sqlite.CommandTx, z *public.DNSZone, p zonestate.Policy) error {
		if f.extra != nil {
			return f.extra(ctx, tx, z, p)
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
	f.r, err = pr.New(pr.Options{Store: s, Repository: f.repo, Reader: primaryzone.NewReader(s, primaryzone.ReaderOptions{PageSize: 1, Policy: f.g.CapturePolicy}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Proofs: testIssuer{}, Publish: func(ctx context.Context, _ pr.Cut, _ pr.Member) error {
		// The real publisher MUST be called after the pin is released. A
		// recursive pin here would deadlock the actual root publication gate.
		ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		select {
		case <-f.h.gate:
			f.h.gate <- struct{}{}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *sqliteFixture) ctx() context.Context { return pr.WithPublicationFence(f.t.Context(), f.fence) }

func (f *sqliteFixture) source(name, apex string) *public.DNSZone {
	f.t.Helper()
	z := testSource()
	z.Name, z.Spec.ZoneName = name, apex
	z.Spec.Nameservers = []string{"ns." + apex}
	z.Spec.Policy.UpdatePolicies[0].Name = "*." + apex
	z.UID, z.ResourceVersion = "", ""
	saved, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		f.t.Fatal(err)
	}
	return saved.(*public.DNSZone)
}

func (f *sqliteFixture) begin(z *public.DNSZone, parents ...*zk.ZoneLifecycle) *zk.ZoneLifecycle {
	f.t.Helper()
	l, err := pr.NewLifecycle(f.fence.Term.ClusterID, z, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, parent := range parents {
		l.Spec.Parents = append(l.Spec.Parents, pr.ParentObligation(parent, l))
	}
	l, err = f.r.Transition(f.ctx(), nil, l)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *sqliteFixture) active(name, apex string) *zk.ZoneLifecycle {
	f.t.Helper()
	l := f.begin(f.source(name, apex))
	if _, err := f.r.Reconcile(f.ctx(), l.Spec.Zone); err != nil {
		f.t.Fatal(err)
	}
	l, err := f.r.ReadLifecycle(f.ctx(), l.Spec.Zone)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *sqliteFixture) request(l *zk.ZoneLifecycle, retry, record string) pr.VerifiedUpdate {
	f.t.Helper()
	m := new(dns.Msg)
	m.SetUpdate(l.Spec.Zone.Apex)
	rr, err := dns.NewRR(record)
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
	<-f.h.gate
	id, ok := f.h.current.Keys.Identity("key.")
	f.h.gate <- struct{}{}
	if !ok {
		f.t.Fatal("test proof issuer has no credential")
	}
	p := pr.ProofIdentity{Principal: dynamicupdate.Principal{Key: id.Key, Version: id.CredentialVersion}, ProviderVersion: id.ProviderVersion, Algorithm: id.Algorithm, Transport: "tcp", Domain: "dns-update:" + zk.HeadName(l.Spec.Zone), Subject: id.Key, RetryID: retry, SemanticDigest: digest, Term: f.fence.Term, AuthorityIncarnation: f.fence.Incarnation, NotAfter: time.Now().Add(time.Minute)}
	return pr.VerifiedUpdate{Zone: l.Spec.Zone, Wire: wire, Proof: &testIssued{identity: p}}
}

func (f *sqliteFixture) view(l *zk.ZoneLifecycle) *zonestate.View {
	f.t.Helper()
	v, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *sqliteFixture) image() []byte {
	f.t.Helper()
	var b bytes.Buffer
	if err := f.s.Snapshot(f.ctx(), &b, store.SnapshotOptions{}); err != nil {
		f.t.Fatal(err)
	}
	return b.Bytes()
}

func TestActualRotationReconcilesAuthEpochWithoutSerialAndRechecksEveryResponse(t *testing.T) {
	f := newSQLiteFixture(t)
	l := f.active("primary", "example.")
	request := f.request(l, "one", "dynamic.example. 60 IN A 192.0.2.1")
	if out := f.r.Apply(f.ctx(), request); out.Rcode != dns.RcodeSuccess || !out.Committed || !out.Published {
		t.Fatal(out)
	}
	if out := f.r.Apply(f.ctx(), request); out.Rcode != dns.RcodeSuccess || !out.Deduplicated {
		t.Fatal(out)
	}
	before := f.image()
	v := f.view(l)
	f.h.rotate(testRing(t, "second"))
	for _, req := range []pr.VerifiedUpdate{request, f.request(l, "noop", "dynamic.example. 60 IN A 192.0.2.1")} {
		if out := f.r.Apply(f.ctx(), req); out.Rcode == dns.RcodeSuccess || !bytes.Equal(before, f.image()) {
			t.Fatal("stale committed or verification credential authorized response", out)
		}
	}
	if _, err := f.r.Reconcile(f.ctx(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	after := f.view(l)
	if after.Head.Spec.AuthEpoch != v.Head.Spec.AuthEpoch+1 || after.Head.Spec.Serial != v.Head.Spec.Serial || after.Aggregate.Policy.Credentials[0].Version == v.Aggregate.Policy.Credentials[0].Version {
		t.Fatal("rotation did not preserve serial/advance policy", after.Head.Spec)
	}
	if out := f.r.Apply(f.ctx(), f.request(l, "two", "next.example. 60 IN A 192.0.2.2")); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	before = f.image()
	if out := f.r.Apply(f.ctx(), request); out.Rcode == dns.RcodeSuccess || !bytes.Equal(before, f.image()) {
		t.Fatal("retained receipt bypassed verification-time credential", out)
	}
	empty, _ := dnsserver.NewStaticKeyring()
	f.h.rotate(empty)
	if _, err := f.r.Reconcile(f.ctx(), l.Spec.Zone); err != nil {
		t.Fatal("removed key prevented durable revocation", err)
	}
	if policy := f.view(l).Aggregate.Policy; len(policy.Credentials) != 0 || len(policy.Grants) != 0 {
		t.Fatal("removed key retained grants", policy)
	}
}

func TestActualGuardPinsThroughBlockedWriterRotationAndSignatureDeadline(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotation", true: "signature-expiry"}[expire], func(t *testing.T) {
			f := newSQLiteFixture(t)
			l := f.active("primary", "example.")
			request := f.request(l, "blocked", "dynamic.example. 60 IN A 192.0.2.1")
			before := f.image()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unlock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unlock)
			writer := make(chan error, 1)
			go func() {
				writer <- f.s.Command(f.ctx(), f.fence.Term, func(context.Context, *sqlite.CommandTx) error { close(entered); <-release; return nil })
			}()
			<-entered
			f.h.entered = make(chan struct{}, 1)
			if expire {
				request.Proof.(*testIssued).identity.NotAfter = time.Now().Add(150 * time.Millisecond)
			}
			result := make(chan pr.UpdateOutcome, 1)
			go func() { result <- f.r.Apply(f.ctx(), request) }()
			<-f.h.entered
			rotated := make(chan struct{})
			go func() { f.h.rotate(testRing(t, "second")); close(rotated) }()
			select {
			case <-rotated:
				t.Fatal("replacement crossed blocked Command's outer pin")
			case <-time.After(25 * time.Millisecond):
			}
			if expire {
				timer := time.NewTimer(time.Until(request.Proof.(*testIssued).identity.NotAfter))
				<-timer.C
			}
			unlock()
			if err := <-writer; err != nil {
				t.Fatal(err)
			}
			out := <-result
			<-rotated
			if expire {
				if out.Committed || out.Rcode == dns.RcodeSuccess || !bytes.Equal(before, f.image()) {
					t.Fatal("signature expired in writer wait but mutated head", out)
				}
			} else if out.Rcode != dns.RcodeSuccess || !out.Committed {
				t.Fatal(out)
			}
		})
	}
}

func TestActualBoundaryRepairAndRetirementDoNotRequireRevokedUpdatePolicy(t *testing.T) {
	f := newSQLiteFixture(t)
	parent := f.active("parent", "example.")
	initial := f.view(parent)
	empty, _ := dnsserver.NewStaticKeyring()
	f.h.rotate(empty)
	child := f.begin(f.source("child", "child.example."), parent)
	if _, err := f.r.Reconcile(f.ctx(), child.Spec.Zone); err != nil {
		t.Fatal("revoked parent policy blocked boundary repair", err)
	}
	fenced := f.view(parent)
	if !reflect.DeepEqual(initial.Aggregate.Policy, fenced.Aggregate.Policy) || len(fenced.Aggregate.Reservations) == 0 {
		t.Fatal("boundary repair altered retained policy or lacked reservation")
	}
	if err := f.s.Delete(f.ctx(), store.Key{Kind: "DNSZone", Name: "parent"}, store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	removed := parent.Spec.Source
	removed.Removed = true
	parent, err := f.r.RetireSource(f.ctx(), parent.Spec.Zone, removed)
	if err != nil {
		t.Fatal("revoked old policy prevented source retirement", err)
	}
	historical := f.view(parent)
	if historical.Head.Spec.Lifecycle != zk.LifecycleTombstoned || !reflect.DeepEqual(historical.Aggregate.Policy, initial.Aggregate.Policy) {
		t.Fatal("retirement changed historical credentials")
	}
	if _, err := f.r.Retire(f.ctx(), child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.ReleaseParents(f.ctx(), child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(historical, f.view(parent)) {
		t.Fatal("terminal parent release rewrote historical head")
	}
}

func TestActualNotifyRequiresExplicitPinnedCapabilityAndRetainedPolicyClosuresExpire(t *testing.T) {
	f := newSQLiteFixture(t)
	l := f.active("primary", "example.")
	v := f.view(l)
	before := f.image()
	for _, reason := range []string{"notify-delivery", "notify-ack"} {
		var prepared *zonestate.Prepared
		if reason == "notify-ack" {
			var err error
			prepared, err = zonestate.Prepare(v.Aggregate, v.Head, zonestate.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			prepared.Head.Spec.NotifySequence, prepared.Head.Spec.NotifySerial = 0, 0
		}
		if _, err := f.a.Admission(f.ctx(), pr.Command{Lifecycle: l, Expected: v.Expected, Prepared: prepared, Reason: reason}); !errors.Is(err, pr.ErrUnavailable) || !bytes.Equal(before, f.image()) {
			t.Fatal("nil NOTIFY capability admitted delivery/ack", err)
		}
	}
	called := false
	f.h.current.Notify = func(ctx context.Context, tx *sqlite.CommandTx, c sqliteadapter.BackgroundCheck) error {
		called = tx != nil && c.Source.Name == "primary" && c.Reason == "notify-delivery"
		return ctx.Err()
	}
	if _, err := f.a.Admission(f.ctx(), pr.Command{Lifecycle: l, Expected: v.Expected, Reason: "notify-delivery"}); err != nil || !called || !bytes.Equal(before, f.image()) {
		t.Fatal("explicit same-pin NOTIFY validator not used", err)
	}
	request := f.request(l, "capture", "dynamic.example. 60 IN A 192.0.2.1")
	var retained sqliteadapter.PolicyGuard
	if err := f.g.Credentials().WithCurrent(f.ctx(), request.Proof.(*testIssued).identity, func(c sqliteadapter.CurrentCredential) error { retained = c.Policy; return nil }); err != nil {
		t.Fatal(err)
	}
	var source public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "primary"}, &source); err != nil {
		t.Fatal(err)
	}
	err := f.s.Command(f.ctx(), f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return retained(ctx, tx, &source, v.Aggregate.Policy)
	})
	if !errors.Is(err, pr.ErrUnavailable) || !bytes.Equal(before, f.image()) {
		t.Fatal("retained validator authorized after pin release", err)
	}
}

func TestActualPreparedPolicyCannotBorrowNewGenerationAndDesiredGrantChangeIsCurrent(t *testing.T) {
	f := newSQLiteFixture(t)
	l := f.active("primary", "example.")
	in, err := primaryzone.NewReader(f.s, primaryzone.ReaderOptions{Policy: f.g.CapturePolicy}).ReadDesired(f.ctx(), l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	v := f.view(l)
	compilation, err := primaryzone.NewCompiler(primaryzone.CompilerOptions{}).Project(f.ctx(), v, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := zonestate.Prepare(compilation.Aggregate, v.Head, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	before := f.image()
	f.h.rotate(testRing(t, "second"))
	if _, err := f.a.Admission(f.ctx(), pr.Command{Lifecycle: l, Expected: v.Expected, Prepared: prepared, Reason: "source-reconcile"}); !errors.Is(err, pr.ErrUnavailable) || !bytes.Equal(before, f.image()) {
		t.Fatal("old compilation was relabeled with current generation", err)
	}
	if _, err := f.r.Reconcile(f.ctx(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	request := f.request(l, "before-grant-change", "dynamic.example. 60 IN A 192.0.2.1")
	if out := f.r.Apply(f.ctx(), request); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	v = f.view(l)
	var source public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "primary"}, &source); err != nil {
		t.Fatal(err)
	}
	source.Spec.Policy.UpdatePolicies = []public.UpdatePolicy{{TSIGKeyRef: "key.", Name: "allowed.example.", Types: []string{"A"}}}
	if _, err := f.s.Apply(f.ctx(), &source, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: source.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	before = f.image()
	if out := f.r.Apply(f.ctx(), request); out.Rcode == dns.RcodeSuccess || !bytes.Equal(before, f.image()) {
		t.Fatal("receipt ignored current desired source change", out)
	}
	if _, err := f.r.Reconcile(f.ctx(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	next := f.view(l)
	if next.Head.Spec.AuthEpoch != v.Head.Spec.AuthEpoch+1 || next.Head.Spec.Serial != v.Head.Spec.Serial {
		t.Fatal("desired authorization change did not preserve serial/advance epoch")
	}
	before = f.image()
	if out := f.r.Apply(f.ctx(), request); out.Rcode != dns.RcodeRefused || !bytes.Equal(before, f.image()) {
		t.Fatal("receipt ignored new committed authorization epoch", out)
	}
	if out := f.r.Apply(f.ctx(), f.request(l, "new-grant", "allowed.example. 60 IN A 192.0.2.2")); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
}

func TestActualSamePinCoversFinalCommitCheckAndOuterCancellation(t *testing.T) {
	for _, cancelOuter := range []bool{false, true} {
		t.Run(map[bool]string{false: "final-commit", true: "outer-cancellation"}[cancelOuter], func(t *testing.T) {
			f := newSQLiteFixture(t)
			l := f.active("primary", "example.")
			request := f.request(l, "final-check", "dynamic.example. 60 IN A 192.0.2.1")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			t.Cleanup(finish)
			// Explicit test-only extra conjunct stalls the actual final hook,
			// after all chunk/head/receipt mutations are staged in CommandTx.
			f.extra = func(_ context.Context, tx *sqlite.CommandTx, _ *public.DNSZone, _ zonestate.Policy) error {
				return tx.RequireCommitCheck(func(context.Context) error { close(entered); <-release; return nil })
			}
			v := f.view(l)
			// Exercise through Runtime so actual UPDATE semantics/head/chunks
			// are built and committed by the production adapter, not this test.
			before := f.image()
			ctx, cancel := context.WithCancel(f.ctx())
			defer cancel()
			result := make(chan pr.UpdateOutcome, 1)
			go func() { result <- f.r.Apply(ctx, request) }()
			<-entered
			rotated := make(chan struct{})
			go func() { f.h.rotate(testRing(t, "second")); close(rotated) }()
			select {
			case <-rotated:
				t.Fatal("replacement crossed actual final commit hook")
			case <-time.After(25 * time.Millisecond):
			}
			if cancelOuter {
				cancel()
			}
			finish()
			out := <-result
			<-rotated
			if cancelOuter {
				if out.Committed || out.Rcode == dns.RcodeSuccess || !bytes.Equal(before, f.image()) {
					t.Fatal("ended outer source context committed staged data", out)
				}
			} else if out.Rcode != dns.RcodeSuccess || !out.Committed || f.view(l).Head.Spec.CommitSequence != v.Head.Spec.CommitSequence+1 {
				t.Fatal(out)
			}
		})
	}
}
