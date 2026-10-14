package primaryruntime

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// These issuers exist ONLY in tests. Root supplies the production context-bound
// VerifiedTSIG capability; raw strings cannot enter through this verifier.
type issuedProof struct{ identity ProofIdentity }
type testIssuer struct{}

func (testIssuer) Verify(_ context.Context, opaque any, wire []byte) (ProofIdentity, error) {
	p, ok := opaque.(*issuedProof)
	if !ok || p == nil {
		return ProofIdentity{}, ErrInvalid
	}
	digest, err := SemanticDigest(wire)
	if err != nil || digest != p.identity.SemanticDigest {
		return ProofIdentity{}, ErrInvalid
	}
	return p.identity, nil
}
func (h *harness) request(l *zk.ZoneLifecycle, retry, text string, prereq bool) VerifiedUpdate {
	h.t.Helper()
	m := new(dns.Msg)
	m.SetUpdate(l.Spec.Zone.Apex)
	rr, err := dns.NewRR(text)
	if err != nil {
		h.t.Fatal(err)
	}
	m.Ns = []dns.RR{rr}
	if prereq {
		m.Answer = []dns.RR{&dns.RR_Header{Name: rr.Header().Name, Rrtype: rr.Header().Rrtype, Class: dns.ClassNONE}}
	}
	wire, err := m.Pack()
	if err != nil {
		h.t.Fatal(err)
	}
	digest, err := SemanticDigest(wire)
	if err != nil {
		h.t.Fatal(err)
	}
	term := replication.Term{ClusterID: l.Spec.Zone.ClusterID, Epoch: 1, PrimaryID: "test-primary", PromotionID: "test-term"}
	incarnation := uint64(1)
	if sql, ok := h.st.(*sqlite.Store); ok {
		term = sql.Authority().Term
		incarnation = sql.Authority().Incarnation
	}
	p := ProofIdentity{Principal: dynamicupdate.Principal{Key: "key.", Version: "v1"}, ProviderVersion: "immutable-provider-v1", Algorithm: dns.HmacSHA256, Transport: "udp", Domain: "test-zone-update", Subject: "test-key", RetryID: retry, SemanticDigest: digest, Term: term, AuthorityIncarnation: incarnation}
	return VerifiedUpdate{Zone: l.Spec.Zone, Wire: wire, Proof: &issuedProof{identity: p}}
}
func TestActualUpdatePublicationFailureReplayAndAuth(t *testing.T) {
	for _, kind := range []string{"sqlite", "kube"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			ctx := context.Background()
			l := h.begin(h.zone("primary", "example."))
			if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			h.r.o.Proofs = testIssuer{}
			request := h.request(l, "retry-1", "dynamic.example. 70 IN TYPE65200 \\# 3 ABCDEF", true)
			h.publishFail = true
			result := h.r.Apply(ctx, request)
			if result.Rcode != dns.RcodeServerFailure || !result.Committed || result.Published {
				t.Fatalf("commit/publication outcome %+v", result)
			}
			committed, err := h.repo.Read(ctx, l.Spec.HeadZone())
			if err != nil {
				t.Fatal(err)
			}
			if len(committed.Aggregate.Receipts) != 1 {
				t.Fatal("no durable receipt")
			}
			// A cold owner recovers solely from durable ledger/head/receipt and still
			// requires current admission + complete publication before NOERROR.
			cold, err := New(h.r.o)
			if err != nil {
				t.Fatal(err)
			}
			h.r = cold
			h.publishFail = false
			before := h.admissions.Load()
			again := h.r.Apply(ctx, request)
			if again.Rcode != dns.RcodeSuccess || !again.Committed || !again.Published || !again.Deduplicated || h.admissions.Load() <= before {
				t.Fatalf("cold dedup %+v", again)
			}
			after, err := h.repo.Read(ctx, l.Spec.HeadZone())
			if err != nil || after.Head.Spec != committed.Head.Spec {
				t.Fatal("replay repeated semantics/serial", err)
			}
			if got := h.r.Capture(time.Now()).Match("dynamic.example.").Lookup("dynamic.example.", 65200); got.Answer[0].(*dns.RFC3597).Rdata != "abcdef" {
				t.Fatal("raw dynamic data lost")
			}
			changed := h.request(l, "retry-1", "different.example. 60 IN A 192.0.2.9", false)
			if out := h.r.Apply(ctx, changed); out.Rcode != dns.RcodeRefused {
				t.Fatalf("retry ID reused %+v", out)
			}
			h.authFail = true
			if out := h.r.Apply(ctx, request); out.Rcode != dns.RcodeServerFailure || out.Published {
				t.Fatalf("revoked dedup %+v", out)
			}
			h.authFail = false
			request.Proof = "key."
			if out := h.r.Apply(ctx, request); out.Rcode != dns.RcodeServerFailure {
				t.Fatal("raw key string accepted")
			}
		})
	}
}

type nativeNotify struct {
	target    netip.AddrPort
	attempts  atomic.Int64
	afterSend func() error
}

func (n *nativeNotify) Send(ctx context.Context, m Member, z *zone.Zone) error {
	n.attempts.Add(1)
	err := zone.SendNotify(ctx, zone.Notify{Zone: m.Zone.Apex, Serial: z.Serial(), SOA: z.SOA(), Target: n.target, Timeout: time.Second})
	if err != nil {
		return err
	}
	if n.afterSend != nil {
		return n.afterSend()
	}
	return nil
}
func nativeNotifyPeer(t *testing.T) *nativeNotify {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	server := &dns.Server{PacketConn: packet, NotifyStartedFunc: func() { close(ready) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(m)
		_ = w.WriteMsg(reply)
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	<-ready
	t.Cleanup(func() {
		_ = server.Shutdown()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return &nativeNotify{target: netip.MustParseAddrPort(packet.LocalAddr().String())}
}
func TestNotifyDurableIntentExactAckAndNativeDelivery(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	l := h.begin(h.zone("primary", "example."))
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	h.r.o.Proofs = testIssuer{}
	sender := nativeNotifyPeer(t)
	h.r.o.Notify = sender
	if out := h.r.Apply(ctx, h.request(l, "notify-update-1", "first.example. 60 IN A 192.0.2.1", false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	sender.afterSend = func() error {
		out := h.r.Apply(ctx, h.request(l, "notify-update-2", "second.example. 60 IN A 192.0.2.2", false))
		return out.Err
	}
	if err := h.r.NotifyOnce(ctx); !conflicts(err) {
		t.Fatalf("stale notification ack %v", err)
	}
	v, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil || v.Head.Spec.NotifySequence == 0 {
		t.Fatal("newer intent lost", err)
	}
	serial := v.Head.Spec.Serial
	sender.afterSend = nil
	if err := h.r.NotifyOnce(ctx); err != nil {
		t.Fatal(err)
	}
	v, err = h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil || v.Head.Spec.NotifySequence != 0 || v.Head.Spec.Serial != serial || sender.attempts.Load() != 2 {
		t.Fatal("delivery/ack/serial", err)
	}
	if out := h.r.Apply(ctx, h.request(l, "notify-update-3", "third.example. 60 IN A 192.0.2.3", false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	if _, err := h.r.Retire(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if err := h.r.NotifyOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.attempts.Load() != 2 {
		t.Fatal("retired UID sent NOTIFY")
	}
}

func TestLateAncestorAndCurrentRetirementToken(t *testing.T) {
	for _, kind := range []string{"sqlite", "kube"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			ctx := context.Background()
			childSource := h.zone("child", "child.example.")
			child := h.begin(childSource)
			if _, err := h.r.Reconcile(ctx, child.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			held := h.r.Capture(time.Now())
			parent := h.begin(h.zone("parent", "example."))
			if _, err := h.r.Prepare(ctx, parent.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			if _, err := h.r.Activate(ctx, parent.Spec.Zone); !conflicts(err) {
				t.Fatalf("late parent activated without child boundary: %v", err)
			}
			if _, err := h.r.AdmitAncestor(ctx, parent.Spec.Zone, child.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			if _, err := h.r.Activate(ctx, parent.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			cut := h.r.Capture(time.Now())
			if m, _ := cut.Member(child.Spec.Zone.Apex); !m.Published || len(m.Parents) != 1 {
				t.Fatal("late obligation/publication")
			}
			if !held.Match("x.child.example.").AvailableAt(time.Now()) {
				t.Fatal("old cut changed")
			}
			// Same-UID type change carries the current spec/RV; retirement does not
			// demand the original primary's accepted RV after desired replacement.
			childSource.Spec.Type = public.ZoneTypeForward
			saved, err := h.st.Apply(ctx, childSource, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: childSource.ResourceVersion})
			if err != nil {
				t.Fatal(err)
			}
			token, err := primaryzone.SourceToken(saved.(*public.DNSZone))
			if err != nil {
				t.Fatal(err)
			}
			retired, err := h.r.RetireSource(ctx, child.Spec.Zone, zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest})
			if err != nil {
				t.Fatal(err)
			}
			if retired.Spec.Source.ResourceVersion != token.ResourceVersion {
				t.Fatal("retirement lost replacement token")
			}
			if _, err := h.r.ReleaseParents(ctx, child.Spec.Zone); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSameUIDTypeIncarnationsAndDesiredDeletion(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	source := h.zone("primary", "example.")
	l := h.begin(source)
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	oldID := l.Spec.HeadZone()
	replace := func(typ public.ZoneType) *zk.ZoneLifecycle {
		t.Helper()
		source.Spec.Type = typ
		saved, err := h.st.Apply(ctx, source, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: source.ResourceVersion})
		if err != nil {
			t.Fatal(err)
		}
		source = saved.(*public.DNSZone)
		token, err := primaryzone.SourceToken(source)
		if err != nil {
			t.Fatal(err)
		}
		retired, err := h.r.RetireSource(ctx, l.Spec.Zone, zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest})
		if err != nil {
			t.Fatal(err)
		}
		next, err := NewLifecycle(l.Spec.Zone.ClusterID, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		next.Spec.Sequence = retired.Spec.Sequence + 1
		next.Spec.Incarnation = retired.Spec.Incarnation + 1
		next, err = h.r.Transition(ctx, retired, next)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	l = replace(public.ZoneTypeForward)
	if l.Spec.Incarnation != 2 {
		t.Fatal("replacement incarnation")
	}
	for _, phase := range []string{zk.PhaseFencing, zk.PhaseActive} {
		next, err := nextPhase(l, phase)
		if err != nil {
			t.Fatal(err)
		}
		l, err = h.r.Transition(ctx, l, next)
		if err != nil {
			t.Fatal(err)
		}
	}
	model, errs := zone.Build(source, nil, zone.BuildOptions{})
	if !errs.Empty() {
		t.Fatal(errs)
	}
	if err := h.r.InstallReplacement(ctx, l, model); err != nil {
		t.Fatal(err)
	}
	if h.r.Capture(time.Now()).Match("x.example.").Type() != public.ZoneTypeForward {
		t.Fatal("forward membership")
	}
	l = replace(public.ZoneTypePrimary)
	if h.r.Capture(time.Now()).Match("x.example.").AvailableAt(time.Now()) {
		t.Fatal("nonprimary retirement left active membership")
	}
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	fresh, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil || fresh.Head.Spec.Zone == oldID {
		t.Fatal("tombstone authority reused", err)
	}
	old, err := h.repo.Read(ctx, oldID)
	if err != nil || old.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
		t.Fatal("old authority resurrected", err)
	}
	token, err := primaryzone.SourceToken(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Delete(ctx, store.KeyOf(source), store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	retired, err := h.r.RetireSource(ctx, l.Spec.Zone, zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest, Removed: true})
	if err != nil {
		t.Fatal(err)
	}
	if !retired.Spec.Source.Removed || h.r.Capture(time.Now()).Match("x.example.").AvailableAt(time.Now()) {
		t.Fatal("desired deletion stayed active")
	}
}

func TestNoopDedupMismatchAndCapacityRequireAdmission(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	l := h.begin(h.zone("primary", "example."))
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	h.r.o.Proofs = testIssuer{}
	h.r.o.Limits.MaxReceipts = 1
	req := h.request(l, "one", "dynamic.example. 60 IN A 192.0.2.1", false)
	if out := h.r.Apply(ctx, req); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	before := h.admissions.Load()
	mismatch := h.request(l, "one", "other.example. 60 IN A 192.0.2.2", false)
	if out := h.r.Apply(ctx, mismatch); out.Rcode != dns.RcodeRefused || h.admissions.Load() <= before {
		t.Fatal("mismatched receipt bypassed admission", out)
	}
	before = h.admissions.Load()
	if out := h.r.Apply(ctx, h.request(l, "two", "dynamic.example. 60 IN A 192.0.2.1", false)); out.Rcode != dns.RcodeServerFailure || h.admissions.Load() <= before {
		t.Fatal("capacity bypassed admission", out)
	}
	h.authFail = true
	if out := h.r.Apply(ctx, mismatch); out.Rcode != dns.RcodeServerFailure {
		t.Fatal("rejected replay bypassed revocation", out)
	}
	h.authFail = false
	h.r.o.Limits.MaxReceipts = 3
	beforeView, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if out := h.r.Apply(ctx, h.request(l, "noop", "dynamic.example. 60 IN A 192.0.2.1", false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	after, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil || after.Head.Spec.Serial != beforeView.Head.Spec.Serial || len(after.Aggregate.Receipts) != 2 {
		t.Fatal("noop receipt/serial", err)
	}
}
