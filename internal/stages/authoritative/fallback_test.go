package authoritative

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	cacheStage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func fallbackZone(group string) *v1alpha1.DNSZone {
	return dnsZone("example.org.", func(z *v1alpha1.DNSZone) {
		z.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{UpstreamGroup: group}
	})
}

func fallbackSource(t *testing.T, zs ...*zone.Zone) *zone.Holder {
	t.Helper()
	snap, err := zone.NewSnapshot(1, true, zs...)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap)
	return h
}

func TestFallbackNamesAndAliases(t *testing.T) {
	z := build(t, fallbackZone("public"),
		ep("auth.example.org", "A", "10.0.0.10"),
		ep("alias.example.org", "CNAME", "www.example.org."),
		ep("alias-local.example.org", "CNAME", "auth.example.org."),
		ep("alias-child.example.org", "CNAME", "missing.child.example.org."),
		ep("alias-shadow.example.org", "CNAME", "shadow.child.example.org."),
		ep("shadow.child.example.org", "A", "10.0.0.50"),
		ep("alias-child-forward.example.org", "CNAME", "app.corp.example.org."),
		ep("alias-apex.example.org", "CNAME", "example.org."),
		ep("*.wild.example.org", "A", "10.0.0.99"),
		ep("host.ent.example.org", "A", "10.0.0.20"),
		ep("dn.example.org", "DNAME", "public.example.org."),
	)
	child := build(t, dnsZone("child.example.org."))
	corp := build(t, dnsZone("corp.example.org.", func(z *v1alpha1.DNSZone) {
		z.Spec.Type = v1alpha1.ZoneTypeForward
		z.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "corp"}
	}))
	st := New(fallbackSource(t, z, child, corp))
	childSOA := child.SOA().String()
	run(t, st, []Case{
		{Name: "local-address", Qname: "auth.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"auth.example.org. 300 IN A 10.0.0.10"}},
		{Name: "local-name-missing-aaaa", Qname: "auth.example.org.", Qtype: dns.TypeAAAA, AA: true, Status: pipeline.StatusAuthoritative, Ns: []string{soaOrg}},
		{Name: "public-name", Qname: "www.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Next: "www.example.org.", Group: "public",
			Answer: []string{"www.example.org. 60 IN A " + upstreamA}},
		{Name: "public-apex", Qname: "example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Next: "example.org.", Group: "public",
			Answer: []string{"example.org. 60 IN A " + upstreamA}},
		{Name: "local-apex-soa", Qname: "example.org.", Qtype: dns.TypeSOA, AA: true, Status: pipeline.StatusAuthoritative, Answer: []string{soaOrg}},
		{Name: "local-apex-ns", Qname: "example.org.", Qtype: dns.TypeNS, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"example.org. 300 IN NS ns1.example.org."}},
		{Name: "wildcard-address", Qname: "web.wild.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"web.wild.example.org. 300 IN A 10.0.0.99"}},
		{Name: "wildcard-missing-type", Qname: "web.wild.example.org.", Qtype: dns.TypeAAAA, AA: true, Status: pipeline.StatusAuthoritative, Ns: []string{soaOrg}},
		{Name: "empty-nonterminal", Qname: "ent.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative, Ns: []string{soaOrg}},
		{Name: "alias-public-target", Qname: "alias.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Next: "www.example.org.", Group: "public",
			Answer: []string{"alias.example.org. 300 IN CNAME www.example.org.", "www.example.org. 60 IN A " + upstreamA}},
		{Name: "alias-apex-target", Qname: "alias-apex.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Next: "example.org.", Group: "public",
			Answer: []string{"alias-apex.example.org. 300 IN CNAME example.org.", "example.org. 60 IN A " + upstreamA}},
		{Name: "alias-local-missing-type", Qname: "alias-local.example.org.", Qtype: dns.TypeAAAA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"alias-local.example.org. 300 IN CNAME auth.example.org."}, Ns: []string{soaOrg}},
		{Name: "specific-primary", Qname: "missing.child.example.org.", Qtype: dns.TypeA, AA: true, Rcode: dns.RcodeNameError, Status: pipeline.StatusAuthoritative, Ns: []string{childSOA}},
		{Name: "alias-specific-primary", Qname: "alias-child.example.org.", Qtype: dns.TypeA, AA: true, Rcode: dns.RcodeNameError, Status: pipeline.StatusAuthoritative,
			Answer: []string{"alias-child.example.org. 300 IN CNAME missing.child.example.org."}, Ns: []string{childSOA}},
		{Name: "alias-cannot-use-parent-data-below-child-zone", Qname: "alias-shadow.example.org.", Qtype: dns.TypeA, AA: true, Rcode: dns.RcodeNameError, Status: pipeline.StatusAuthoritative,
			Answer: []string{"alias-shadow.example.org. 300 IN CNAME shadow.child.example.org."}, Ns: []string{childSOA}},
		{Name: "specific-forward", Qname: "app.corp.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Next: "app.corp.example.org.", Group: "corp",
			Answer: []string{"app.corp.example.org. 60 IN A " + upstreamA}},
		{Name: "alias-specific-forward", Qname: "alias-child-forward.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Next: "app.corp.example.org.", Group: "corp",
			Answer: []string{"alias-child-forward.example.org. 300 IN CNAME app.corp.example.org.", "app.corp.example.org. 60 IN A " + upstreamA}},
		{Name: "dname-public-target", Qname: "web.dn.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Next: "web.public.example.org.", Group: "public",
			Answer: []string{"dn.example.org. 300 IN DNAME public.example.org.", "web.dn.example.org. 300 IN CNAME web.public.example.org.", "web.public.example.org. 60 IN A " + upstreamA}},
		{Name: "no-recursion", Qname: "www.example.org.", Qtype: dns.TypeA, NoRD: true, Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL},
	})
}

func TestFallbackSnapshotAndAccessGates(t *testing.T) {
	for _, gate := range []zone.Gate{zone.GateUnsynced, zone.GateStale} {
		t.Run(gate.String(), func(t *testing.T) {
			h := fallbackSource(t, build(t, fallbackZone("")))
			h.Store(h.Load().WithGate(gate))
			run(t, New(h), []Case{{Qname: "public.example.org.", Qtype: dns.TypeA, Rcode: dns.RcodeServerFailure, Status: pipeline.StatusServfailUnsynced}})
		})
	}
	z := fallbackZone("")
	z.Spec.Policy.QueryAccess = v1alpha1.AccessAllowOnlyPrivate
	run(t, New(fallbackSource(t, build(t, z))), []Case{
		{Qname: "public.example.org.", Qtype: dns.TypeA, Client: "203.0.113.1", Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL},
	})
	st := New(fallbackSource(t, build(t, fallbackZone(""))))
	st.RecursionAvailable = false
	run(t, st, []Case{{Qname: "public.example.org.", Qtype: dns.TypeA, Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL}})
}

func TestFallbackAliasDNSSECAndErrors(t *testing.T) {
	st := New(fallbackSource(t, build(t, fallbackZone(""), ep("alias.example.org", "CNAME", "www.example.org."))))
	for _, alias := range []bool{false, true} {
		for _, bogus := range []bool{false, true} {
			name := "www.example.org."
			if alias {
				name = "alias.example.org."
			}
			req := (&Case{Qname: name, Qtype: dns.TypeA, EDNS: true}).request()
			resp, err := st.Serve(req, func(sub *pipeline.Request) (*pipeline.Response, error) {
				r := pipeline.Reply(sub, dns.RcodeSuccess, pipeline.StatusForwarded)
				r.Msg.AuthenticatedData = true
				r.DNSSEC = pipeline.DNSSECSecure
				if bogus {
					r.Msg.Rcode = dns.RcodeServerFailure
					r.Status, r.DNSSEC = pipeline.StatusDNSSECBogus, pipeline.DNSSECBogus
				}
				r.Msg.SetEdns0(1232, true)
				r.Msg.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeDNSBogus, ExtraText: "test"}}
				return r, nil
			})
			if err != nil || resp.Msg.Authoritative || resp.Msg.Id != req.Msg.Id || resp.Msg.Question[0] != req.Question() {
				t.Fatalf("alias=%v bogus=%v: response=%+v err=%v", alias, bogus, resp, err)
			}
			if alias && resp.Msg.AuthenticatedData {
				t.Fatal("a local alias must not inherit the target's AD bit")
			}
			wantSecurity := pipeline.DNSSECSecure
			if alias {
				wantSecurity = pipeline.DNSSECUnknown
			}
			if bogus {
				wantSecurity = pipeline.DNSSECBogus
			}
			if resp.DNSSEC != wantSecurity || len(resp.Msg.IsEdns0().Option) != 1 {
				t.Fatalf("lost DNSSEC state or extended error: %+v", resp)
			}
		}
	}
}

// Exercise both actual group modes through the cache, without public network
// dependencies. The same server acts as a forwarder or a private root server.
func TestFallbackResolverGroupsAndCache(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		q := r.Question[0]
		if q.Qtype == dns.TypeA {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(upstreamA)}}
		} else if q.Qtype == dns.TypeAAAA {
			soa, _ := dns.NewRR(soaOrg)
			m.Ns = []dns.RR{soa}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	for _, recursive := range []bool{false, true} {
		for _, group := range []string{"", "public"} {
			t.Run(strings.Join([]string{map[bool]string{false: "forward", true: "recursive"}[recursive], group}, "/"), func(t *testing.T) {
				rc := recursor.DefaultConfig()
				rc.DNSSEC, rc.QNAMEMinimization, rc.IPv6, rc.RootHints = false, false, recursor.IPv6Off, []string{pc.LocalAddr().String()}
				rec, err := recursor.New(rc, recursor.Options{AllowLoopback: true})
				if err != nil {
					t.Fatal(err)
				}
				gc := upstream.GroupConfig{Servers: []string{pc.LocalAddr().String()}}
				if recursive {
					gc = upstream.GroupConfig{Recursive: true}
				}
				set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": gc, "public": gc}}, upstream.Options{Recursor: rec})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = set.Close() })
				h := fallbackSource(t, build(t, fallbackZone(group)))
				c := cache.New(cache.Options{MaxEntries: 128})
				cs := cacheStage.New(c, cacheStage.Settings{NegativeTTL: time.Minute}, nil)
				t.Cleanup(cs.Wait)
				chain := pipeline.MustNew(New(h), cs, upstreamstage.NewStage(set))
				ask := func(name string, typ uint16) *pipeline.Response {
					t.Helper()
					req := (&Case{Qname: name, Qtype: typ}).request()
					req.Ctx = context.Background()
					r, err := chain.Serve(req)
					if err != nil || r.Msg.Rcode != dns.RcodeSuccess {
						t.Fatalf("%s: %+v %v", name, r, err)
					}
					return r
				}
				for i := range 2 {
					r := ask("www.example.org.", dns.TypeA)
					want := pipeline.StatusForwarded
					if i == 1 && group == "" {
						want = pipeline.StatusCached
					}
					if r.Status != want || len(r.Msg.Answer) != 1 || r.Msg.Answer[0].(*dns.A).A.String() != upstreamA || r.Msg.Authoritative {
						t.Fatalf("public answer: %+v", r)
					}
					if recursive && want == pipeline.StatusForwarded && !strings.HasSuffix(r.Upstream, "/recursive") {
						t.Fatalf("did not use native recursion: %s", r.Upstream)
					}
				}
				// Positive and negative public cache entries must not shadow a
				// newly published local name, including its missing AAAA type.
				ask("www.example.org.", dns.TypeAAAA)
				h.Store(h.Load().With(2, build(t, fallbackZone(group), ep("www.example.org", "A", "10.0.0.10"))))
				if r := ask("www.example.org.", dns.TypeA); r.Status != pipeline.StatusAuthoritative || r.Msg.Answer[0].(*dns.A).A.String() != "10.0.0.10" {
					t.Fatalf("public cache shadowed local address: %+v", r)
				}
				if r := ask("www.example.org.", dns.TypeAAAA); r.Status != pipeline.StatusAuthoritative || len(r.Msg.Answer) != 0 || len(r.Msg.Ns) != 1 {
					t.Fatalf("public cache shadowed local NODATA: %+v", r)
				}
			})
		}
	}
}

func TestFallbackTransferRefused(t *testing.T) {
	z := fallbackZone("")
	z.Spec.Policy.ZoneTransfer = v1alpha1.AccessAllow
	z.Spec.Policy.Notify = v1alpha1.NotifyBoth
	built := build(t, z)
	st := New(fallbackSource(t, built))
	run(t, st, []Case{{Qname: "example.org.", Qtype: dns.TypeAXFR, Proto: pipeline.ProtoTCP, Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL}})
	if _, err := st.Transfer("example.org.", 0); !errors.Is(err, ErrNotTransferable) {
		t.Fatalf("partial zone transfer allowed: %v", err)
	}
	if built.NotifyMode() != v1alpha1.NotifyNone {
		t.Fatal("partial zone must not notify ordinary secondaries")
	}
}
