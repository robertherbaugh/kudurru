package authoritative

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func stubStageZone(t *testing.T, now time.Time, addr string, mutate ...func(*v1alpha1.DNSZone)) *zone.Zone {
	t.Helper()
	dz := dnsZone("child.example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeStub
		dz.Spec.Stub = &v1alpha1.StubSpec{Nameservers: []string{addr}}
	})
	for _, fn := range mutate {
		fn(dz)
	}
	z, err := zone.BuildStub(dz, &zone.StubData{Records: []dns.RR{mustRR("child.example.org. 300 IN SOA ns.child.example.org. admin.child.example.org. 99 10 3 30 10"), mustRR("child.example.org. 30 IN NS ns.child.example.org."), mustRR("ns.child.example.org. 20 IN A 127.0.0.1"), mustRR("child.example.org. 30 IN RRSIG NS 13 3 30 20270101000000 20260101000000 1234 child.example.org. AQID")}, Servers: []zone.StubServer{{Name: "ns.child.example.org.", Address: netip.MustParseAddrPort(addr), Protocol: "udp"}}, ConfirmedAt: now, ExpiresAt: now.Add(20 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func stageAuthority(t *testing.T, h dns.HandlerFunc) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p, err := net.ListenPacket("udp", l.Addr().String())
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	for _, s := range []*dns.Server{{Listener: l, Net: "tcp", Handler: h}, {PacketConn: p, Net: "udp", Handler: h}} {
		ready := make(chan struct{})
		done := make(chan error, 1)
		s.NotifyStartedFunc = func() { close(ready) }
		go func() { done <- s.ActivateAndServe() }()
		<-ready
		t.Cleanup(func() {
			_ = s.Shutdown()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	return l.Addr().String()
}

func TestStubProductionRouteReferralForwardingAndExpiry(t *testing.T) {
	var calls atomic.Int32
	addr := stageAuthority(t, func(w dns.ResponseWriter, q *dns.Msg) {
		calls.Add(1)
		if q.RecursionDesired {
			t.Error("authority received RD=1")
		}
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		m.AuthenticatedData = true
		if q.Question[0].Name == "www.child.example.org." {
			m.Answer = []dns.RR{mustRR("www.child.example.org. 42 IN A 192.0.2.80"), mustRR("www.child.example.org. 42 IN RRSIG A 13 4 42 20270101000000 20260101000000 1234 child.example.org. AQID")}
		} else {
			m.Ns = []dns.RR{mustRR("deleg.child.example.org. 60 IN NS ns.deleg.child.example.org.")}
			m.Extra = []dns.RR{mustRR("ns.deleg.child.example.org. 60 IN A 192.0.2.44")}
		}
		_ = w.WriteMsg(m)
	})
	now := time.Now()
	z := stubStageZone(t, now, addr)
	s := secondaryStage(t, z, now)
	client := &stub.Client{Now: func() time.Time { return now }}
	s.StubQuery = client.Exchange
	// RD=0 asks for local routing information, without any authority traffic.
	req := stubCaseRequest(Case{Qname: "www.child.example.org.", Qtype: dns.TypeA, NoRD: true})
	req.Msg.SetEdns0(1232, true)
	rec := &recorder{}
	resp, err := s.Serve(req, rec.next)
	if err != nil || resp.Msg.Authoritative || len(resp.Msg.Ns) != 2 || calls.Load() != 0 || len(rec.calls) != 0 || resp.Origin != pipeline.OriginLocal {
		t.Fatalf("RD0 %+v %v", resp, err)
	}
	for _, qname := range []string{"www.child.example.org.", "www.deleg.child.example.org.", "alias.example.net."} {
		req := stubCaseRequest(Case{Qname: qname, Qtype: dns.TypeA})
		req.Msg.SetEdns0(1232, true)
		resp, err := s.Serve(req, rec.next)
		if err != nil || resp.Msg.Rcode != dns.RcodeSuccess || resp.Msg.Authoritative || resp.Msg.AuthenticatedData || resp.Cacheable || resp.Security.State != pipeline.ValidationUnknown || len(rec.calls) != 0 {
			t.Fatalf("RD1 %s %+v %v", qname, resp, err)
		}
		if qname == "www.deleg.child.example.org." {
			if len(resp.Msg.Ns) != 1 || len(resp.Msg.Extra) != 2 {
				t.Fatal("child referral/glue lost")
			}
		} else if len(resp.Msg.Answer) < 2 {
			t.Fatal("raw answer/signature lost")
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("chased child referral or repeated discovery: %d", calls.Load())
	}
	s.Now = func() time.Time { return now.Add(20 * time.Second) }
	for _, qname := range []string{"www.child.example.org.", "alias.example.net.", "inside.example.org."} {
		resp, err := s.Serve(stubCaseRequest(Case{Qname: qname, Qtype: dns.TypeA}), rec.next)
		if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || resp.Upstream != "stub:unavailable:child.example.org." || len(rec.calls) != 0 || calls.Load() != 3 {
			t.Fatalf("expiry %s %+v", qname, resp)
		}
	}
}

func TestStubCNAMEUsesCapturedLocalBoundariesAndHonestMixedProvenance(t *testing.T) {
	now := time.Now()
	z := stubStageZone(t, now, "127.0.0.1:5300")
	s := secondaryStage(t, z, now)
	// A configured child overrides the network's invented target answer.
	configured := build(t, dnsZone("private.child.example.org."), ep("www.private.child.example.org", "A", "198.51.100.7"))
	snap, err := zone.NewSnapshot(2, true, z, configured)
	if err != nil {
		t.Fatal(err)
	}
	var captures atomic.Int32
	s.Capture = func(*pipeline.Request) zone.Set { captures.Add(1); return snap }
	s.StubQuery = func(_ context.Context, _ *zone.Zone, q *dns.Msg) (*dns.Msg, error) {
		// Publish a different source while this query is underway. All subsequent
		// matching must still use the captured set, with no second source read.
		next, err := zone.NewSnapshot(3, true, z)
		if err != nil {
			t.Fatal(err)
		}
		s.Source.(*zone.Holder).Store(next)
		m := new(dns.Msg)
		m.SetReply(q)
		m.AuthenticatedData = true
		m.Authoritative = true
		m.Answer = []dns.RR{mustRR("alias.child.example.org. 42 IN CNAME www.private.child.example.org."), mustRR("www.private.child.example.org. 42 IN A 192.0.2.99")}
		return m, nil
	}
	rec := &recorder{}
	resp, err := s.Serve(stubCaseRequest(Case{Qname: "alias.child.example.org.", Qtype: dns.TypeA}), rec.next)
	if err != nil || len(resp.Msg.Answer) != 2 || resp.Msg.Answer[1].(*dns.A).A.String() != "198.51.100.7" || resp.Msg.AuthenticatedData || resp.Msg.Authoritative || resp.Origin != pipeline.OriginMixed || !resp.Modified || resp.Cacheable || captures.Load() != 1 || len(rec.calls) != 0 {
		t.Fatalf("captured CNAME %+v %v", resp, err)
	}
}

func TestStubUnavailableChildACLStaleAndTransferFailClosed(t *testing.T) {
	now := time.Now()
	z := stubStageZone(t, now, "127.0.0.1:5300")
	// Stub CNAME target is below a configured unavailable secondary boundary.
	dz := dnsZone("private.child.example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeSecondary })
	unavailable, err := zone.BuildSecondaryPlaceholder(dz)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := zone.NewSnapshot(1, true, z, unavailable)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap)
	s := New(h)
	s.Now = func() time.Time { return now }
	var calls atomic.Int32
	s.StubQuery = func(_ context.Context, _ *zone.Zone, q *dns.Msg) (*dns.Msg, error) {
		calls.Add(1)
		m := new(dns.Msg)
		m.SetReply(q)
		m.Answer = []dns.RR{mustRR("alias.child.example.org. 42 IN CNAME www.private.child.example.org.")}
		return m, nil
	}
	rec := &recorder{}
	resp, err := s.Serve(stubCaseRequest(Case{Qname: "alias.child.example.org.", Qtype: dns.TypeA}), rec.next)
	if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || len(resp.Msg.Answer) != 0 || len(rec.calls) != 0 {
		t.Fatalf("unavailable target %+v %v", resp, err)
	}
	for _, typ := range []uint16{dns.TypeAXFR, dns.TypeIXFR} {
		resp, err := s.Serve(stubCaseRequest(Case{Qname: z.Name(), Qtype: typ, Proto: pipeline.ProtoTCP}), rec.next)
		if err != nil || resp.Msg.Rcode != dns.RcodeRefused || calls.Load() != 1 {
			t.Fatal("stub transfer allowed")
		}
	}
	s.Capture = func(*pipeline.Request) zone.Set { return snap.WithGate(zone.GateStale) }
	resp, _ = s.Serve(stubCaseRequest(Case{Qname: "www.child.example.org.", Qtype: dns.TypeA}), rec.next)
	if resp.Msg.Rcode != dns.RcodeServerFailure || calls.Load() != 1 || len(rec.calls) != 0 {
		t.Fatal("stale stub fell through")
	}
}

func TestForwardValidationOverrideDirectAndCNAME(t *testing.T) {
	for _, value := range []*bool{nil, new(true), new(false)} {
		forward := build(t, dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) {
			dz.Spec.Type = v1alpha1.ZoneTypeForward
			dz.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "corp", DNSSECValidation: value}
		}))
		alias := build(t, dnsZone("example.org."), ep("alias.example.org", "CNAME", "www.corp.example"))
		snap, err := zone.NewSnapshot(1, true, forward, alias)
		if err != nil {
			t.Fatal(err)
		}
		h := new(zone.Holder)
		h.Store(snap)
		s := New(h)
		for _, name := range []string{"www.corp.example.", "alias.example.org."} {
			rec := &recorder{}
			_, err := s.Serve(stubCaseRequest(Case{Qname: name, Qtype: dns.TypeA}), rec.next)
			if err != nil || len(rec.calls) != 1 {
				t.Fatal(err)
			}
			got := rec.calls[0].DNSSECValidation
			if value == nil {
				if got != nil {
					t.Fatal("inheritance lost")
				}
			} else if got == nil || *got != *value {
				t.Fatal("override lost")
			}
		}
	}
}

func stubCaseRequest(c Case) *pipeline.Request { return c.request() }

func TestStubDeniedQueriesAndCNAMEBudgetDoNotReachRoot(t *testing.T) {
	now := time.Now()
	denied := stubStageZone(t, now, "127.0.0.1:5300", func(dz *v1alpha1.DNSZone) { dz.Spec.Policy.QueryAccess = v1alpha1.AccessDeny })
	s := secondaryStage(t, denied, now)
	s.StubQuery = func(context.Context, *zone.Zone, *dns.Msg) (*dns.Msg, error) {
		t.Fatal("denied stub queried authority")
		return nil, nil
	}
	rec := &recorder{}
	for _, rd := range []bool{false, true} {
		resp, err := s.Serve(stubCaseRequest(Case{Qname: "www.child.example.org.", Qtype: dns.TypeA, NoRD: !rd}), rec.next)
		if err != nil || resp.Msg.Rcode != dns.RcodeRefused || len(rec.calls) != 0 {
			t.Fatalf("ACL bypass %+v %v", resp, err)
		}
	}
	live := stubStageZone(t, now, "127.0.0.1:5300")
	s = secondaryStage(t, live, now)
	s.StubQuery = func(_ context.Context, _ *zone.Zone, q *dns.Msg) (*dns.Msg, error) {
		m := new(dns.Msg)
		m.SetReply(q)
		m.Answer = []dns.RR{mustRR("a.child.example.org. 42 IN CNAME b.child.example.org."), mustRR("b.child.example.org. 42 IN CNAME a.child.example.org.")}
		return m, nil
	}
	resp, err := s.Serve(stubCaseRequest(Case{Qname: "a.child.example.org.", Qtype: dns.TypeA}), rec.next)
	if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || len(rec.calls) != 0 {
		t.Fatalf("CNAME loop %+v %v", resp, err)
	}
}
