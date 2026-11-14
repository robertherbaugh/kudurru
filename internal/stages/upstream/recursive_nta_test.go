package upstream_test

import (
	"bytes"
	"crypto"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	dnssecstage "github.com/robertherbaugh/kudurru/internal/stages/dnssec"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func recursiveNTARoot(t *testing.T, hold func(*dns.Msg), outsideExpiry time.Time) (string, string, *atomic.Int64) {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 300}, Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	var signer crypto.Signer
	for key.KeyTag() == 0 || signer == nil {
		private, err := key.Generate(256)
		if err != nil {
			t.Fatal("fixture key generation failed")
		}
		signer = private.(crypto.Signer)
	}
	root := signedRoot{key: key, signer: signer, expires: time.Now().Add(time.Hour)}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture root socket failed")
	}
	queries := &atomic.Int64{}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		if q.Question[0].Qtype == dns.TypeA {
			queries.Add(1)
			if hold != nil {
				hold(q)
			}
		}
		source := root
		if strings.EqualFold(q.Question[0].Name, "good.outside.example.") && !outsideExpiry.IsZero() {
			source.expires = outsideExpiry
		}
		m, err := source.Exchange(t.Context(), q, netip.AddrPort{}, false)
		if err != nil {
			return
		}
		if q.Question[0].Qtype == dns.TypeA && !strings.HasPrefix(strings.ToLower(q.Question[0].Name), "good.") {
			m.Answer[0].(*dns.A).A = net.IPv4(192, 0, 2, 99) // tamper after signing
			if strings.Contains(strings.ToLower(q.Question[0].Name), ".broken.example.") {
				for _, rr := range m.Answer {
					if sig, ok := rr.(*dns.RRSIG); ok {
						sig.Expiration = 1
					} // expired proof, retained raw
				}
			}
		}
		if q.Question[0].Qtype == dns.TypeA && strings.EqualFold(q.Question[0].Name, "alias.broken.example.") {
			name := strings.ToLower(q.Question[0].Name)
			m.Answer = []dns.RR{
				&dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60}, Target: "good.outside.example."},
				&dns.RRSIG{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, TypeCovered: dns.TypeCNAME, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: ".", Expiration: 1, Signature: "AA=="},
			}
		}
		_ = w.WriteMsg(m)
	})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String(), key.ToDS(dns.SHA256).String(), queries
}

func TestRecursiveNTARawCacheRestoreAndIsolation(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	outsideExpiry := start.Add(10 * time.Second)
	addr, anchor, queries := recursiveNTARoot(t, nil, outsideExpiry)
	var clock atomic.Int64
	// Use the real clock until the explicit expiry jumps. Initial native
	// validation and the pipeline timestamp must not straddle a frozen clock.
	now := func() time.Time {
		if ns := clock.Load(); ns != 0 {
			return time.Unix(0, ns)
		}
		return time.Now()
	}
	expiry := start.Add(30 * time.Second)
	rc := config.Recursion{DNSSEC: true, IPv6: "off", RootHints: []string{addr}, TrustAnchors: []string{anchor}, NegativeTrustAnchors: []config.RecursiveNegativeTrustAnchor{{Domain: "broken.example.", Reason: "signer repair", ExpiresAt: expiry}}}
	uc := config.Upstreams{Groups: map[string]config.UpstreamGroup{"default": {Mode: "recursive"}, "forward": {Servers: []string{addr}}}, Recursion: rc}
	converted := upstream.ConfigFrom(uc)
	validator, err := dnssec.New(dnssec.Options{Anchors: []dns.RR{mustNTAAnchor(t, anchor)}, NTAs: []dnssec.NTA{{Domain: "forwarded.example.", Reason: "forwarded incident", ExpiresAt: start.Add(5 * time.Second)}}, Now: now})
	if err != nil {
		t.Fatal("forwarded validator did not construct")
	}
	build := func(raw *cache.Cache, exceptions bool) (*pipeline.Chain, *upstreamstage.Stage, *cachestage.Stage) {
		t.Helper()
		cfg := converted
		if !exceptions {
			cfg.Recursion.NegativeTrustAnchors = nil
		}
		rec, err := recursor.New(cfg.Recursion, recursor.Options{Now: now, AllowLoopback: true})
		if err != nil {
			t.Fatal("native resolver construction failed")
		}
		set, err := upstream.New(cfg, upstream.Options{Recursor: rec})
		if err != nil {
			t.Fatal("production group construction failed")
		}
		t.Cleanup(func() { _ = set.Close() })
		fwd := upstreamstage.NewStage(set)
		if err := fwd.SetValidation(validator, true); err != nil {
			t.Fatal("validation wiring failed")
		}
		cc := cachestage.New(raw, cachestage.Settings{MinTTL: time.Hour, MaxTTL: time.Hour}, fwd)
		t.Cleanup(cc.Wait)
		return pipeline.MustNew(cc, dnssecstage.New(fwd), fwd), fwd, cc
	}
	newCache := func() *cache.Cache { return cache.New(cache.Options{MaxEntries: 1024, StaleTTL: time.Hour, Now: now}) }
	raw := newCache()
	chain, fwd, _ := build(raw, true)
	request := func(name, group string, do, ad, cd bool) *pipeline.Request {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		q.SetEdns0(1232, do)
		q.AuthenticatedData, q.CheckingDisabled = ad, cd
		return &pipeline.Request{Ctx: t.Context(), Msg: q, UpstreamGroup: group}
	}
	serve := func(c *pipeline.Chain, req *pipeline.Request, rcode int, sec pipeline.ValidationState, cached bool) *pipeline.Response {
		t.Helper()
		r, err := c.Serve(req)
		if err != nil || r == nil {
			t.Fatal("recursive pipeline exchange failed")
		}
		t.Cleanup(r.Close)
		wantState := sec
		if sec == pipeline.ValidationBogus && req.UpstreamGroup == "" && r.Security.State != pipeline.ValidationBogus {
			// Native SERVFAIL is carried by the upstream failure envelope,
			// which reports DNSSEC/status rather than successful provenance.
			// An exact-deadline raw-cache refusal instead marks Security bogus
			// and retains the original native DNSSEC classification.
			wantState = pipeline.ValidationUnknown
		}
		if sec == pipeline.ValidationBogus && (r.Status != pipeline.StatusDNSSECBogus || (req.UpstreamGroup == "" && r.DNSSEC != pipeline.DNSSECBogus && r.Security.State != pipeline.ValidationBogus) || len(r.Msg.Answer) != 0) {
			t.Fatalf("bogus answer lost refusal/status semantics: rcode=%d status=%d dnssec=%d state=%d answers=%d", r.Msg.Rcode, r.Status, r.DNSSEC, r.Security.State, len(r.Msg.Answer))
		}
		if r.Msg.Rcode != rcode || r.Security.State != wantState || (cached && r.Status != pipeline.StatusCached) || r.Msg.AuthenticatedData {
			t.Fatalf("pipeline result mismatch: rcode=%d state=%d cached=%t ad=%t do=%t cd=%t expected_state=%d security_expired=%t deadline_before_clock=%t", r.Msg.Rcode, r.Security.State, r.Status == pipeline.StatusCached, r.Msg.AuthenticatedData, req.Msg.IsEdns0().Do(), req.Msg.CheckingDisabled, wantState, r.Security.Reason == "validation_expired", !r.Security.ValidUntil.IsZero() && !now().Before(r.Security.ValidUntil))
		}
		if !req.Msg.IsEdns0().Do() {
			for _, rr := range r.Msg.Answer {
				if rr.Header().Rrtype == dns.TypeRRSIG {
					t.Fatal("proof leaked to non-DO client")
				}
			}
		}
		if sec == pipeline.ValidationInsecure && req.Msg.IsEdns0().Do() && req.Question().Name == "www.broken.example." {
			retained := false
			for _, rr := range r.Msg.Answer {
				if sig, ok := rr.(*dns.RRSIG); ok && sig.Expiration == 1 {
					retained = true
				}
			}
			if !retained {
				t.Fatal("excepted expired raw signature was discarded")
			}
		}
		return r
	}
	// Client DO/AD interest never authenticates an exception; CD is separate.
	for _, flags := range []struct{ do, ad, cd bool }{{true, false, false}, {false, true, false}, {false, false, false}, {true, true, true}} {
		sec := pipeline.ValidationInsecure
		if flags.cd {
			sec = pipeline.ValidationUnknown
		}
		for n := 0; n < 2; n++ {
			out := serve(chain, request("www.broken.example.", "", flags.do, flags.ad, flags.cd), dns.RcodeSuccess, sec, n == 1)
			if !flags.cd && (out.Security.ValidUntil.IsZero() || out.Security.ValidUntil.After(expiry)) {
				t.Fatal("raw cache lost absolute exception deadline")
			}
		}
	}
	// An excepted expired alias cannot relax its authenticated outside target.
	alias := serve(chain, request("alias.broken.example.", "", true, false, false), dns.RcodeSuccess, pipeline.ValidationInsecure, false)
	if alias.Security.ValidUntil.After(outsideExpiry) {
		t.Fatal("excepted alias extended the outside proof deadline")
	}
	// Each validator's exception applies only on its own route.
	serve(chain, request("www.broken.example.", "forward", true, false, false), dns.RcodeServerFailure, pipeline.ValidationBogus, false)
	serve(chain, request("www.forwarded.example.", "", true, false, false), dns.RcodeServerFailure, pipeline.ValidationBogus, false)
	serve(chain, request("www.forwarded.example.", "forward", true, false, false), dns.RcodeSuccess, pipeline.ValidationInsecure, false)
	held, err := fwd.Bind(request("www.broken.example.", "", true, false, false))
	if err != nil {
		t.Fatal("could not capture recursive context")
	}
	clock.Store(start.Add(5 * time.Second).UnixNano())
	fresh, err := fwd.Bind(request("www.broken.example.", "", true, false, false))
	if err != nil || fresh.Scope().ID != held.Scope().ID || !held.Current() {
		t.Fatal("forwarded NTA expiry changed recursive context")
	}
	// Durable restore under identical settings reuses the raw entry, with the
	// original deadline. The response-cache epoch is deliberately new.
	var dump bytes.Buffer
	if err := raw.Dump(&dump); err != nil {
		t.Fatal("raw cache dump failed")
	}
	restored := newCache()
	if n, err := restored.Load(bytes.NewReader(dump.Bytes())); err != nil || n == 0 {
		t.Fatal("raw cache restore failed")
	}
	resumed, _, _ := build(restored, true)
	before := queries.Load()
	serve(resumed, request("www.broken.example.", "", false, true, false), dns.RcodeSuccess, pipeline.ValidationInsecure, true)
	if queries.Load() != before {
		t.Fatal("matching durable restore did not reuse raw answer")
	}
	// A restored answer cannot inherit authority from a removed exception.
	otherContext := newCache()
	if _, err := otherContext.Load(bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatal("different-context dump load failed")
	}
	unexcepted, _, _ := build(otherContext, false)
	before = queries.Load()
	serve(unexcepted, request("www.broken.example.", "", true, false, false), dns.RcodeServerFailure, pipeline.ValidationBogus, false)
	if queries.Load() <= before {
		t.Fatal("different-context restore reused exception cache data")
	}
	// Exact expiry fences detached native fills and cannot serve fresh/stale
	// entries despite a one-hour minimum TTL and stale window.
	clock.Store(outsideExpiry.UnixNano())
	serve(resumed, request("alias.broken.example.", "", true, false, false), dns.RcodeServerFailure, pipeline.ValidationBogus, false)
	if !held.Current() {
		t.Fatal("outside proof expiry retired unrelated recursive NTA authority")
	}
	clock.Store(outsideExpiry.Add(time.Second).UnixNano())
	if r := serve(resumed, request("alias.broken.example.", "", true, false, false), dns.RcodeServerFailure, pipeline.ValidationBogus, false); r.DNSSEC != pipeline.DNSSECBogus {
		t.Fatal("native resolver accepted expired outside-alias proof")
	}
	clock.Store(expiry.UnixNano())
	after, err := fwd.Bind(request("www.broken.example.", "", true, false, false))
	if err != nil || after.Scope().ID == held.Scope().ID || held.Current() {
		t.Fatal("recursive expiry did not fence captured cache publication")
	}
	serve(resumed, request("www.broken.example.", "", true, false, false), dns.RcodeServerFailure, pipeline.ValidationBogus, false)
	afterExpiry := newCache()
	if _, err := afterExpiry.Load(bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatal("expired dump load failed")
	}
	restarted, _, _ := build(afterExpiry, true)
	serve(restarted, request("www.broken.example.", "", true, false, false), dns.RcodeServerFailure, pipeline.ValidationBogus, false)
	if r := serve(restarted, request("www.broken.example.", "", true, true, true), dns.RcodeSuccess, pipeline.ValidationUnknown, false); r.Status == pipeline.StatusCached || r.Status == pipeline.StatusCachedStale {
		t.Fatal("expired CD context reused old exception cache data")
	}
}

func mustNTAAnchor(t *testing.T, text string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(text)
	if err != nil {
		t.Fatal("fixture anchor parse failed")
	}
	return rr
}

func TestRecursiveNTAExpiryFencesDetachedNetworkFill(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	addr, anchor, queries := recursiveNTARoot(t, func(_ *dns.Msg) {
		if held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}, time.Time{})
	start := time.Now()
	expiry := start.Add(time.Minute)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	cfg := recursor.DefaultConfig()
	cfg.RootHints, cfg.TrustAnchors, cfg.IPv6, cfg.QNAMEMinimization = []string{addr}, []string{anchor}, recursor.IPv6Off, false
	cfg.NegativeTrustAnchors = []dnssec.NTA{{Domain: "broken.example.", Reason: "incident", ExpiresAt: expiry}}
	rec, err := recursor.New(cfg, recursor.Options{Now: now, AllowLoopback: true})
	if err != nil {
		t.Fatal("native resolver construction failed")
	}
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Recursive: true}}, Recursion: cfg}, upstream.Options{Recursor: rec})
	if err != nil {
		t.Fatal("recursive group construction failed")
	}
	defer func() { _ = set.Close() }()
	fwd := upstreamstage.NewStage(set)
	raw := cache.New(cache.Options{MaxEntries: 1024, Now: now})
	cc := cachestage.New(raw, cachestage.Settings{}, fwd)
	defer cc.Wait()
	chain := pipeline.MustNew(cc, dnssecstage.New(fwd), fwd)
	q := new(dns.Msg)
	q.SetQuestion("www.broken.example.", dns.TypeA)
	q.SetEdns0(1232, true)
	result := make(chan *pipeline.Response, 1)
	go func() { r, _ := chain.Serve(&pipeline.Request{Ctx: t.Context(), Msg: q}); result <- r }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("detached recursive fetch did not reach authoritative socket")
	}
	clock.Store(expiry.UnixNano())
	close(release)
	select {
	case r := <-result:
		if r == nil {
			t.Fatal("detached recursive fetch returned no result")
		}
		defer r.Close()
		cc.Wait()
		if r.Msg.Rcode != dns.RcodeServerFailure || r.Msg.AuthenticatedData || raw.Len() != 0 || queries.Load() != 1 {
			t.Fatalf("expired detached fetch: rcode=%d ad=%t entries=%d queries=%d", r.Msg.Rcode, r.Msg.AuthenticatedData, raw.Len(), queries.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("detached recursive fetch failed to finish after release")
	}
}
