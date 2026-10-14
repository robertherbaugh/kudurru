package blocking

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	rewritestage "github.com/robertherbaugh/kudurru/internal/stages/rewrite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func modifierIndex(t testing.TB, lines ...string) *policy.Index {
	t.Helper()
	snap := compileLists(t, map[string][]string{"default/rules": lines}, nil)
	return policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{cg("default", v1alpha1.ClientMatcher{CIDR: "192.0.2.0/24"})}, Policies: []*v1alpha1.Policy{{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"rules"}, BlockTTL: metav1.Duration{Duration: time.Minute}}}}, BlockLists: []*v1alpha1.BlockList{bl("rules", "block", inlineSource)}, Lists: snap}, policy.Options{})
}
func modifierRequest(ix *policy.Index, name string, typ uint16, client string) *pipeline.Request {
	r := &pipeline.Request{Ctx: context.Background(), Msg: query(name, typ, true), Client: pipeline.Client{Addr: netip.MustParseAddrPort(client + ":5000")}}
	r.Policy = ix.SelectPolicy(r.Client, time.Now())
	return r
}

type modifierTerminal struct {
	calls atomic.Int64
	alias bool
}

func (*modifierTerminal) Name() string { return "upstream" }
func (s *modifierTerminal) Serve(r *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	s.calls.Add(1)
	m := new(dns.Msg)
	m.SetReply(r.Msg)
	q := r.Question()
	target := q.Name
	if s.alias {
		target = "target.example."
		m.Answer = append(m.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeCNAME, Class: 1, Ttl: 60}, Target: target})
	}
	m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: target, Rrtype: dns.TypeA, Class: 1, Ttl: 60}, A: netip.MustParseAddr("192.0.2.77").AsSlice()})
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Origin: pipeline.OriginForwarded, Cacheable: true, Upstream: "default/fixture", Security: pipeline.Security{State: pipeline.ValidationSecure, ValidatedAt: time.Now().Add(-time.Second), ValidUntil: time.Now().Add(time.Hour)}}, nil
}

type responseTransform struct{}

func (responseTransform) Name() string { return "rewrite" }
func (responseTransform) Serve(r *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	out, e := next(r)
	if e != nil || out == nil {
		return out, e
	}
	transformed := *out
	transformed.Msg = out.Msg.Copy()
	transformed.Modified = true
	transformed.Origin = pipeline.OriginMixed
	transformed.Msg.AuthenticatedData = false
	for _, rr := range transformed.Msg.Answer {
		rr.Header().Name = "target.example."
	}
	transformed.Msg.Answer = append([]dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: r.Question().Name, Rrtype: dns.TypeCNAME, Class: 1, Ttl: 60}, Target: "target.example."}}, transformed.Msg.Answer...)
	return &transformed, nil
}
func TestModifierPolicyIsolationOnRawCacheHitsAndReload(t *testing.T) {
	for _, transform := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw alias", true: "configured synthesis"}[transform], func(t *testing.T) {
			ix := modifierIndex(t, "||target.example^$client=192.0.2.1,dnstype=A")
			st, e := New(Options{})
			if e != nil {
				t.Fatal(e)
			}
			cs := cachestage.New(cache.New(cache.Options{MaxEntries: 100}), cachestage.Settings{}, nil)
			terminal := &modifierTerminal{alias: !transform}
			stages := []pipeline.Stage{st}
			if transform {
				stages = append(stages, responseTransform{})
			}
			stages = append(stages, cs, terminal)
			chain := pipeline.MustNew(stages...)
			closed := modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.1")
			r, e := chain.Serve(closed)
			if e != nil || r.Status != pipeline.StatusBlockedCNAME || r.Msg.AuthenticatedData || r.Origin != pipeline.OriginSynthetic || !r.Modified {
				t.Fatalf("closed client: %+v %v", r, e)
			}
			open := modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.2")
			r, e = chain.Serve(open)
			if e != nil || r.Status != pipeline.StatusCached || r.Msg.Answer[len(r.Msg.Answer)-1].(*dns.A).A.To4()[3] != 77 || terminal.calls.Load() != 1 {
				t.Fatalf("raw hit contaminated: %+v calls=%d %v", r, terminal.calls.Load(), e)
			}
			// The new policy must execute on the same raw cache hit without fetching
			// again, and an already-captured request retains its original decision.
			pinned := modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.1")
			next := modifierIndex(t, "||target.example^$client=192.0.2.2,dnstype=A")
			r, e = chain.Serve(modifierRequest(next, "raw.example.", dns.TypeA, "192.0.2.1"))
			if e != nil || r.Status != pipeline.StatusCached {
				t.Fatal("retired policy shadowed new allow")
			}
			r, e = chain.Serve(modifierRequest(next, "raw.example.", dns.TypeA, "192.0.2.2"))
			if e != nil || r.Status != pipeline.StatusBlockedCNAME {
				t.Fatal("new deny did not wrap raw hit")
			}
			r, e = chain.Serve(pinned)
			if e != nil || r.Status != pipeline.StatusBlockedCNAME {
				t.Fatal("in-flight policy was not pinned")
			}
			if terminal.calls.Load() != 1 {
				t.Fatal("policy accidentally changed shared raw cache identity")
			}
		})
	}
}
func TestStageContextAdapterAndTypedRewrites(t *testing.T) {
	ix := modifierIndex(t, "||rewrite.example^$ctag=device_pc,dnsrewrite=192.0.2.9", "||type.example^$dnstype=AAAA", "||bad.example^$important", "@@||bad.example^")
	st, e := New(Options{Context: func(r *pipeline.Request) engine.Context {
		tags := []string(nil)
		if r.Client.Addr.Addr() == netip.MustParseAddr("192.0.2.1") {
			tags = []string{"device_pc"}
		}
		return engine.Context{Tags: tags, Name: "forged", ClientIP: netip.MustParseAddr("192.0.2.99")}
	}})
	if e != nil {
		t.Fatal(e)
	}
	terminal := &modifierTerminal{}
	r, e := st.Serve(modifierRequest(ix, "rewrite.example.", dns.TypeA, "192.0.2.1"), func(r *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(r, nil) })
	if e != nil || r.Origin != pipeline.OriginSynthetic || !r.Modified || r.Cacheable || r.Msg.AuthenticatedData || r.Msg.Answer[0].(*dns.A).A.To4()[3] != 9 || terminal.calls.Load() != 0 {
		t.Fatalf("typed rewrite %+v %v", r, e)
	}
	r, e = st.Serve(modifierRequest(ix, "rewrite.example.", dns.TypeA, "192.0.2.2"), func(r *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(r, nil) })
	if e != nil || r.Status != pipeline.StatusForwarded {
		t.Fatal("tagless client rewrite leaked")
	}
	if _, err := st.Toggle().Disable(time.Minute); err != nil {
		t.Fatal(err)
	}
	r, e = st.Serve(modifierRequest(ix, "rewrite.example.", dns.TypeA, "192.0.2.1"), func(r *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(r, nil) })
	if e != nil || r.Status != pipeline.StatusForwarded {
		t.Fatal("adblock rewrite ignored disable")
	}
}
func TestCNAMERewriteLoopsAndAuthorityTargetFailure(t *testing.T) {
	ix := modifierIndex(t, "||first.example^$dnsrewrite=second.example", "||second.example^$dnsrewrite=first.example")
	st, _ := New(Options{})
	terminal := &modifierTerminal{}
	r, e := st.Serve(modifierRequest(ix, "first.example.", dns.TypeA, "192.0.2.1"), func(r *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(r, nil) })
	if e != nil || r.Msg.Rcode != dns.RcodeServerFailure || len(r.Msg.Answer) != 0 || terminal.calls.Load() != 0 {
		t.Fatalf("rewrite loop: %+v %v", r, e)
	}
	ix = modifierIndex(t, "||alias.example^$dnsrewrite=expired.secondary.example")
	st, _ = New(Options{ResolveTarget: func(sub *pipeline.Request) (*pipeline.Response, error) {
		if sub.Question().Name != "expired.secondary.example." {
			t.Fatal("wrong target")
		}
		return pipeline.Reply(sub, dns.RcodeServerFailure, pipeline.StatusServfailUnsynced), nil
	}})
	r, e = st.Serve(modifierRequest(ix, "alias.example.", dns.TypeA, "192.0.2.1"), func(r *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(r, nil) })
	if e != nil || r.Msg.Rcode != dns.RcodeServerFailure || len(r.Msg.Answer) != 0 || r.Status != pipeline.StatusServfailUnsynced || terminal.calls.Load() != 0 {
		t.Fatal("failed authority target became positive alias")
	}
}
func TestResponseInspectionBudgetFailsClosed(t *testing.T) {
	ix := modifierIndex(t, "||never.example^$client=192.0.2.1")
	st, _ := New(Options{})
	r, e := st.Serve(modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.1"), func(req *pipeline.Request) (*pipeline.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		for i := 0; i < maxInspect+1; i++ {
			m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: "raw.example.", Rrtype: dns.TypeA, Class: 1, Ttl: 60}, A: netip.MustParseAddr("192.0.2.77").AsSlice()})
		}
		return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded}, nil
	})
	if e != nil || r.Status != pipeline.StatusBlockedCNAME || r.Rule != "response inspection limit" {
		t.Fatal("inspection silently skipped overflow")
	}
}

func TestConfiguredPolicyRewriteFinalInspectionAndToggle(t *testing.T) {
	ix := modifierIndex(t, "||192.0.2.66^$client=192.0.2.1")
	rules, err := responsepolicy.CompileRewrites([]responsepolicy.RewriteSpec{{Name: "configured.example", Targets: []string{"192.0.2.66"}}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := New(Options{})
	terminal := &modifierTerminal{}
	chain := pipeline.MustNew(st, &rewritestage.Stage{}, cachestage.New(cache.New(cache.Options{MaxEntries: 100}), cachestage.Settings{}, nil), terminal)
	request := func(client string, configured bool) *pipeline.Request {
		r := modifierRequest(ix, "configured.example.", dns.TypeA, client)
		if configured {
			d := *policy.FromRequest(r)
			d.Rewrites = rules
			r.Policy = &d
		}
		return r
	}
	// Install a raw cache entry first. Explicit policy rewrites take effect
	// immediately even when a former policy left the name in the shared cache.
	if _, err := chain.Serve(request("192.0.2.2", false)); err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{"192.0.2.1", "192.0.2.2"} {
		r, err := chain.Serve(request(client, true))
		if err != nil {
			t.Fatal(err)
		}
		if client == "192.0.2.1" {
			if r.Status != pipeline.StatusBlockedCNAME {
				t.Fatalf("configured rewrite escaped final filter: %+v", r)
			}
		} else if r.Origin != pipeline.OriginSynthetic || !r.Modified || r.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.66" {
			t.Fatalf("configured rewrite leaked policy: %+v", r)
		}
	}
	if _, err := st.Toggle().Disable(time.Minute); err != nil {
		t.Fatal(err)
	}
	r, err := chain.Serve(request("192.0.2.1", true))
	if err != nil || !r.Modified || r.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.66" {
		t.Fatal("temporary blocking disable removed configured rewrite")
	}
	r, err = chain.Serve(request("192.0.2.2", false))
	if err != nil || r.Status != pipeline.StatusCached || r.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.77" || terminal.calls.Load() != 1 {
		t.Fatal("configured rewrite contaminated raw cache")
	}
}

type negativeTerminal struct{ calls atomic.Int64 }

func (*negativeTerminal) Name() string { return "upstream" }
func (s *negativeTerminal) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	s.calls.Add(1)
	r := pipeline.Reply(req, dns.RcodeNameError, pipeline.StatusForwarded)
	r.Origin, r.Cacheable = pipeline.OriginForwarded, true
	r.Msg.Ns = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: 1, Ttl: 60}, Ns: "ns.example.", Mbox: "hostmaster.example.", Minttl: 60}}
	return r, nil
}

func TestConfiguredNegativeSynthesisInspectedOnActualRawHits(t *testing.T) {
	ix := modifierIndex(t, "||192.0.2.66^$client=192.0.2.1")
	settings, err := responsepolicy.Compile(responsepolicy.Options{NXDomainIPv4: []string{"192.0.2.66"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := New(Options{})
	terminal := &negativeTerminal{}
	chain := pipeline.MustNew(st, &rewritestage.Stage{}, cachestage.New(cache.New(cache.Options{MaxEntries: 100}), cachestage.Settings{}, nil), terminal)
	for _, client := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.1"} {
		q := modifierRequest(ix, "missing.example.", dns.TypeA, client)
		q.ResponseSettings = settings
		r, err := chain.Serve(q)
		if err != nil {
			t.Fatal(err)
		}
		if client == "192.0.2.1" && r.Status != pipeline.StatusBlockedCNAME {
			t.Fatalf("cached synthesis escaped final filter: %+v", r)
		}
		if client == "192.0.2.2" && (!r.Modified || r.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.66") {
			t.Fatal("open client's synthesized response contaminated")
		}
	}
	if terminal.calls.Load() != 1 {
		t.Fatalf("negative raw answer not shared, calls=%d", terminal.calls.Load())
	}
	r, err := chain.Serve(modifierRequest(ix, "missing.example.", dns.TypeA, "192.0.2.2"))
	if err != nil || r.Msg.Rcode != dns.RcodeNameError || r.Modified || len(r.Msg.Answer) != 0 {
		t.Fatal("raw negative was overwritten by synthesis")
	}
}

func TestRewriteSignedDependencyCopiesAndPinnedContext(t *testing.T) {
	ix := modifierIndex(t, "||alias.example^$dnsrewrite=target.example")
	var original, dependency *dns.Msg
	adapterCalls := 0
	st, _ := New(Options{Context: func(*pipeline.Request) engine.Context { adapterCalls++; return engine.Context{} }, ResolveTarget: func(sub *pipeline.Request) (*pipeline.Response, error) {
		if sub.Zone != nil || sub.DNSSECValidation != nil || sub.UpstreamGroup != "" {
			t.Fatal("target retained name-specific authority selection")
		}
		m := new(dns.Msg)
		m.SetReply(sub.Msg)
		m.AuthenticatedData = true
		for _, text := range []string{"target.example. 60 IN A 192.0.2.77", "target.example. 60 IN RRSIG A 15 2 60 20300101000000 20200101000000 1 example. AAAA"} {
			rr, err := dns.NewRR(text)
			if err != nil {
				t.Fatal(err)
			}
			m.Answer = append(m.Answer, rr)
		}
		nsec, _ := dns.NewRR("target.example. 60 IN NSEC z.example. A RRSIG NSEC")
		m.Ns, m.Extra = []dns.RR{nsec}, []dns.RR{dns.Copy(m.Answer[1])}
		original, dependency = m.Copy(), m
		return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Origin: pipeline.OriginForwarded, Security: pipeline.Security{State: pipeline.ValidationSecure}}, nil
	}})
	q := modifierRequest(ix, "alias.example.", dns.TypeA, "192.0.2.1")
	q.Zone, q.UpstreamGroup = "old forward", "old group"
	enabled := false
	q.DNSSECValidation = &enabled
	r, err := st.Serve(q, neverNext(t))
	if err != nil || r.Origin != pipeline.OriginMixed || !r.Modified || r.Cacheable || r.Msg.AuthenticatedData || r.Security.State != pipeline.ValidationSecure || len(r.Msg.Answer) != 2 || len(r.Msg.Ns) != 0 || adapterCalls != 1 {
		t.Fatalf("signed dependency composition: %+v %v calls=%d", r, err, adapterCalls)
	}
	for _, rr := range r.Msg.Extra {
		if rr.Header().Rrtype == dns.TypeRRSIG {
			t.Fatal("obsolete additional signature")
		}
	}
	if dependency.String() != original.String() {
		t.Fatal("target message mutated")
	}
	if q.Zone == nil || q.UpstreamGroup != "old group" || q.DNSSECValidation == nil {
		t.Fatal("parent request mutated")
	}
}

func neverNext(t *testing.T) pipeline.Next {
	return func(*pipeline.Request) (*pipeline.Response, error) { t.Fatal("unexpected forwarding"); return nil, nil }
}

func TestModifiersDoNotTransformProtocolOrOtherClasses(t *testing.T) {
	ix := modifierIndex(t, "||rewrite.example^$dnsrewrite=192.0.2.9")
	st, _ := New(Options{})
	for _, typ := range []uint16{dns.TypeAXFR, dns.TypeIXFR, dns.TypeA} {
		q := modifierRequest(ix, "rewrite.example.", typ, "192.0.2.1")
		if typ == dns.TypeA {
			q.Msg.Question[0].Qclass = dns.ClassCHAOS
		}
		calls := 0
		_, err := st.Serve(q, func(req *pipeline.Request) (*pipeline.Response, error) {
			calls++
			return pipeline.Reply(req, dns.RcodeNotImplemented, pipeline.StatusForwarded), nil
		})
		if err != nil || calls != 1 {
			t.Fatal("modifier intercepted nonordinary query")
		}
	}
}
