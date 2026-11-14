package rewrite_test

import (
	"net"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/rewrite"
)

type source struct {
	calls atomic.Int64
	failA bool
	once  func()
}

func (*source) Name() string { return "upstream" }
func (s *source) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	s.calls.Add(1)
	if s.once != nil {
		f := s.once
		s.once = nil
		f()
	}
	r := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded)
	r.Origin, r.Cacheable = pipeline.OriginForwarded, true
	r.Msg.AuthenticatedData = true // Never local security evidence.
	if req.Question().Qtype == dns.TypeA {
		if s.failA {
			r.Msg.Rcode, r.Status = dns.RcodeServerFailure, pipeline.StatusDNSSECBogus
			return r, nil
		}
		r.Msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: req.Question().Name, Class: dns.ClassINET, Rrtype: dns.TypeA, Ttl: 300}, A: net.ParseIP("192.0.2.33")}}
	} else {
		r.Msg.Ns = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "example.", Class: dns.ClassINET, Rrtype: dns.TypeSOA, Ttl: 300}, Ns: "ns.example.", Mbox: "hostmaster.example.", Minttl: 300}}
	}
	return r, nil
}

func request(typ uint16, overrides *responsepolicy.Overrides) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion("www.example.", typ)
	m.SetEdns0(1232, false)
	return &pipeline.Request{Msg: m, Policy: &policy.Decision{Response: overrides}}
}

func TestSynthesisSharedRawCacheAndReload(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	post := &postprocess.Stage{}
	initial, err := responsepolicy.Compile(responsepolicy.Options{DNS64Prefix: "64:ff9b::/96"})
	if err != nil {
		t.Fatal(err)
	}
	post.SetSettings(initial)
	up := &source{}
	raw := cache.New(cache.OptionsFrom(cfg.Cache, nil))
	chain := pipeline.MustNew(post, &rewrite.Stage{}, cachestage.New(raw, cachestage.SettingsFrom(cfg.Cache), nil), up)
	empty := ""
	noSynthesis, _ := responsepolicy.CompileOverrides(&empty, nil)
	serve := func(q *pipeline.Request) *pipeline.Response {
		t.Helper()
		r, err := chain.Serve(q)
		if err != nil {
			t.Fatal(err)
		}
		if r.Msg.AuthenticatedData {
			t.Fatal("wire AD leaked")
		}
		return r
	}
	for range 2 {
		r := serve(request(dns.TypeAAAA, nil))
		if !r.Modified || len(r.Msg.Answer) != 1 || r.Msg.Answer[0].(*dns.AAAA).AAAA.String() != "64:ff9b::c000:221" {
			t.Fatal(r.Msg)
		}
		r = serve(request(dns.TypeAAAA, noSynthesis))
		if r.Modified || len(r.Msg.Answer) != 0 {
			t.Fatal("synthesis escaped client policy", r.Msg)
		}
	}
	if up.calls.Load() != 2 {
		t.Fatalf("raw cache not shared, calls=%d", up.calls.Load())
	}
	changed, _ := responsepolicy.Compile(responsepolicy.Options{DNS64Prefix: "2001:db8:64::/96"})
	post.SetSettings(changed)
	r := serve(request(dns.TypeAAAA, nil))
	if r.Msg.Answer[0].(*dns.AAAA).AAAA.String() != "2001:db8:64::c000:221" || up.calls.Load() != 2 {
		t.Fatal("cache hit did not observe current synthesis settings")
	}
	// A reload while the first dependency is resolving cannot alter this query.
	raw.Flush()
	up.once = func() { post.SetSettings(initial) }
	r = serve(request(dns.TypeAAAA, nil))
	if r.Msg.Answer[0].(*dns.AAAA).AAAA.String() != "2001:db8:64::c000:221" {
		t.Fatal("settings changed within one query")
	}
	// Failed validation of the A dependency must not turn into a successful
	// AAAA NODATA response that hides the failure.
	raw.Flush()
	up.failA = true
	r = serve(request(dns.TypeAAAA, nil))
	if r.Msg.Rcode != dns.RcodeServerFailure || r.Status != pipeline.StatusDNSSECBogus || r.Msg.Question[0].Qtype != dns.TypeAAAA {
		t.Fatal(r.Msg)
	}
}

func TestPolicyRewriteAndInvalidPolicyDoNotReachUpstream(t *testing.T) {
	up := &source{}
	chain := pipeline.MustNew(&postprocess.Stage{}, &rewrite.Stage{}, up)
	rules, err := responsepolicy.CompileRewrites([]responsepolicy.RewriteSpec{{Name: "www.example", Targets: []string{"192.0.2.8", "2001:db8::8"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeTXT} {
		q := request(typ, nil)
		q.Policy = &policy.Decision{Rewrites: rules}
		r, err := chain.Serve(q)
		if err != nil || r.Msg.Rcode != 0 || !r.Modified || r.Msg.AuthenticatedData || r.Cacheable {
			t.Fatalf("%+v %v", r, err)
		}
		if typ == dns.TypeTXT && len(r.Msg.Answer) != 0 || typ != dns.TypeTXT && (len(r.Msg.Answer) != 1 || r.Msg.Answer[0].Header().Rrtype != typ) {
			t.Fatal(r.Msg)
		}
	}
	q := request(dns.TypeA, nil)
	q.Policy = &policy.Decision{ResponseInvalid: true}
	r, err := chain.Serve(q)
	if err != nil || r.Msg.Rcode != dns.RcodeServerFailure || r.Rule != "invalid_response_policy" || up.calls.Load() != 0 {
		t.Fatalf("invalid policy fell through: %+v %v", r, err)
	}
}
