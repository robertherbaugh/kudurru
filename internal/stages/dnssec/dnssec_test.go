package dnssec_test

import (
	"bytes"
	"context"
	"crypto"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/cache"
	validator "github.com/robertherbaugh/kudurru/internal/dnssec"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	dnssecstage "github.com/robertherbaugh/kudurru/internal/stages/dnssec"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// This fixture exchanges real UDP messages and locally generated signed RRsets.
// Its clock is independent of network deadlines and safe for detached workers.
type fixture struct {
	t             *testing.T
	nanos         atomic.Int64
	key           *dns.DNSKEY
	signer        crypto.Signer
	mu            sync.Mutex
	replies       map[string]*dns.Msg
	queries       []*dns.Msg
	mode          int // 1=upstream SERVFAIL, 2=corrupt A data
	hold, entered chan struct{}
	held          bool
	addr          string
	fwd           *upstreamstage.Stage
	cs            *cachestage.Stage
	chain         *pipeline.Chain
}

func newFixture(t *testing.T, enabled, cached bool) *fixture {
	t.Helper()
	f := &fixture{t: t, replies: map[string]*dns.Msg{}}
	f.nanos.Store(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC).UnixNano())
	f.key = &dns.DNSKEY{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	priv, err := f.key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	f.signer = priv.(crypto.Signer)
	f.replies[queryKey("example.", dns.TypeDNSKEY)] = f.message("example.", dns.TypeDNSKEY, 0, f.sign(f.key), nil)
	f.replies[queryKey("www.example.", dns.TypeA)] = f.message("www.example.", dns.TypeA, 0, f.sign(f.rr("www.example. 2 IN A 192.0.2.1")), nil)
	soa := f.sign(f.rr("example. 300 IN SOA ns.example. hostmaster.example. 1 3600 600 86400 300"))
	f.replies[queryKey("missing.example.", dns.TypeA)] = f.message("missing.example.", dns.TypeA, dns.RcodeNameError, nil, append(soa, f.sign(f.rr("example. 300 IN NSEC z.example. NS SOA RRSIG NSEC DNSKEY"))...))
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = pc.LocalAddr().String()
	started := make(chan struct{})
	server := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		f.mu.Lock()
		f.queries = append(f.queries, q.Copy())
		m := f.replies[queryKey(q.Question[0].Name, q.Question[0].Qtype)]
		if m != nil {
			m = m.Copy()
		}
		mode := f.mode
		var hold chan struct{}
		if q.Question[0].Qtype == dns.TypeA && f.hold != nil && !f.held {
			f.held = true
			hold = f.hold
			close(f.entered)
		}
		f.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if m == nil || mode == 1 {
			m = new(dns.Msg)
			m.SetReply(q)
			m.Rcode = dns.RcodeServerFailure
		}
		if mode == 2 && len(m.Answer) > 0 {
			if a, ok := m.Answer[0].(*dns.A); ok {
				a.A[3] = 9
			}
		}
		m.Id = q.Id
		m.Question = append([]dns.Question(nil), q.Question...)
		m.AuthenticatedData = true // upstream AD is deliberately untrusted
		m.SetEdns0(1232, true)
		_ = w.WriteMsg(m)
	})}
	go func() { _ = server.ActivateAndServe() }()
	<-started
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{f.addr}, Timeout: time.Second}}, ECS: upstream.ECSConfig{Mode: upstream.ECSAdd}}, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.fwd = upstreamstage.NewStage(set)
	v := f.validator(nil)
	if err = f.fwd.SetValidation(v, enabled); err != nil {
		t.Fatal(err)
	}
	f.cs = cachestage.New(cache.New(cache.Options{MaxEntries: 1024, StaleTTL: time.Hour, Now: f.now}), cachestage.Settings{MinTTL: time.Minute, StaleMaxWait: 50 * time.Millisecond, FetchTimeout: time.Second}, f.fwd)
	if !cached {
		f.cs.SetDisabled(true)
	}
	f.chain = pipeline.MustNew(f.cs, dnssecstage.New(f.fwd), f.fwd)
	t.Cleanup(func() { f.cs.Wait(); _ = set.Close(); _ = server.Shutdown() })
	return f
}
func queryKey(name string, typ uint16) string {
	return fmt.Sprintf("%s/%d", dns.CanonicalName(name), typ)
}
func (f *fixture) now() time.Time          { return time.Unix(0, f.nanos.Load()) }
func (f *fixture) advance(d time.Duration) { f.nanos.Add(int64(d)) }
func (f *fixture) rr(text string) dns.RR {
	f.t.Helper()
	r, e := dns.NewRR(text)
	if e != nil {
		f.t.Fatal(e)
	}
	return r
}
func (f *fixture) sign(rrs ...dns.RR) []dns.RR {
	f.t.Helper()
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: rrs[0].Header().Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: rrs[0].Header().Ttl}, Algorithm: f.key.Algorithm, KeyTag: f.key.KeyTag(), SignerName: f.key.Hdr.Name, Inception: uint32(f.now().Add(-time.Hour).Unix()), Expiration: uint32(f.now().Add(2 * time.Hour).Unix())}
	if e := sig.Sign(f.signer, rrs); e != nil {
		f.t.Fatal(e)
	}
	return append(append([]dns.RR(nil), rrs...), sig)
}
func (f *fixture) message(name string, typ uint16, rcode int, answer, ns []dns.RR) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(name, typ)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Rcode = rcode
	m.Answer = answer
	m.Ns = ns
	return m
}
func (f *fixture) validator(ntas []validator.NTA) *validator.Validator {
	f.t.Helper()
	v, e := validator.New(validator.Options{Anchors: []dns.RR{f.key}, NTAs: ntas, Now: f.now})
	if e != nil {
		f.t.Fatal(e)
	}
	return v
}
func (f *fixture) request(name string, typ uint16) *pipeline.Request {
	q := new(dns.Msg)
	q.SetQuestion(name, typ)
	q.SetEdns0(1232, true)
	return &pipeline.Request{Ctx: context.Background(), Msg: q, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.200:4000")}, Deadline: time.Now().Add(2 * time.Second)}
}
func (f *fixture) serve(r *pipeline.Request) *pipeline.Response {
	f.t.Helper()
	resp, e := f.chain.Serve(r)
	if e != nil {
		f.t.Fatal(e)
	}
	if resp == nil || resp.Msg == nil {
		f.t.Fatal("nil response")
	}
	return resp
}
func (f *fixture) queriesFor(typ uint16) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if q.Question[0].Qtype == typ {
			n++
		}
	}
	return n
}
func ede(m *dns.Msg, code uint16) bool {
	if o := m.IsEdns0(); o != nil {
		for _, x := range o.Option {
			if e, ok := x.(*dns.EDNS0_EDE); ok && e.InfoCode == code {
				return true
			}
		}
	}
	return false
}
func countType(m *dns.Msg, typ uint16) int {
	n := 0
	for _, rr := range append(append([]dns.RR(nil), m.Answer...), m.Ns...) {
		if rr.Header().Rrtype == typ {
			n++
		}
	}
	return n
}

func TestFlagsAndRawCacheAcrossMissesAndHits(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%v", cached), func(t *testing.T) {
			f := newFixture(t, true, cached)
			for _, edns := range []bool{false, true} {
				for _, do := range []bool{false, true} {
					if do && !edns {
						continue
					}
					for _, ad := range []bool{false, true} {
						for _, cd := range []bool{false, true} {
							r := f.request("www.example.", dns.TypeA)
							r.Msg.Extra = nil
							if edns {
								r.Msg.SetEdns0(4096, do)
							}
							r.Msg.AuthenticatedData = ad
							r.Msg.CheckingDisabled = cd
							for i := 0; i < 2; i++ {
								q := *r
								q.Msg = r.Msg.Copy()
								q.Msg.Id = uint16(100 + i)
								q.Resolution = nil
								out := f.serve(&q)
								wantAD := !cd && (do || ad)
								want := pipeline.ValidationSecure
								if cd {
									want = pipeline.ValidationUnknown
								}
								if out.Msg.Rcode != dns.RcodeSuccess || out.Msg.Id != q.Msg.Id || out.Msg.AuthenticatedData != wantAD || out.Msg.CheckingDisabled != cd || out.Security.State != want {
									t.Fatalf("edns=%v do=%v ad=%v cd=%v: %+v sec=%+v", edns, do, ad, cd, out.Msg.MsgHdr, out.Security)
								}
								if (out.Msg.IsEdns0() != nil) != edns || do != (countType(out.Msg, dns.TypeRRSIG) > 0) {
									t.Fatalf("proof representation edns=%v do=%v: %s", edns, do, out.Msg)
								}
							}
						}
					}
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			proofs := 0
			for _, q := range f.queries {
				if q.Question[0].Qtype == dns.TypeDNSKEY {
					proofs++
					if !q.CheckingDisabled || q.IsEdns0() == nil || !q.IsEdns0().Do() {
						t.Fatal("proof query missing DO/CD")
					}
					for _, x := range q.IsEdns0().Option {
						if _, ok := x.(*dns.EDNS0_SUBNET); ok {
							t.Fatal("client ECS leaked into proof lookup")
						}
					}
				}
			}
			if proofs == 0 {
				t.Fatal("no DNSKEY lookup")
			}
		})
	}
}

func TestBogusCDBypassOverridesAndExplicitTypes(t *testing.T) {
	f := newFixture(t, true, true)
	f.mu.Lock()
	f.mode = 2
	f.mu.Unlock()
	cd := f.request("www.example.", dns.TypeA)
	cd.Msg.CheckingDisabled = true
	if r := f.serve(cd); r.Msg.Rcode != 0 || r.Msg.AuthenticatedData || r.Security.Reason != "cd_bypass" {
		t.Fatal("CD bypass rejected or authenticated")
	}
	bad := f.serve(f.request("www.example.", dns.TypeA))
	if bad.Msg.Rcode != dns.RcodeServerFailure || len(bad.Msg.Answer) != 0 || bad.Status != pipeline.StatusDNSSECBogus || !ede(bad.Msg, dns.ExtendedErrorCodeDNSBogus) {
		t.Fatalf("bogus answer: %+v %s", bad, bad.Msg)
	}
	// An explicit override uses separate cache semantics and cannot reuse CD or bogus data.
	off := false
	r := f.request("www.example.", dns.TypeA)
	r.DNSSECValidation = &off
	if out := f.serve(r); out.Msg.Rcode != 0 || out.Msg.AuthenticatedData || out.Security.Reason != "validation_disabled" {
		t.Fatal("explicit disable ignored")
	}
	f.mu.Lock()
	f.mode = 0
	f.mu.Unlock()
	if e := f.fwd.SetValidation(f.validator(nil), false); e != nil {
		t.Fatal(e)
	}
	r = f.request("www.example.", dns.TypeA)
	if out := f.serve(r); out.Msg.AuthenticatedData || out.Security.State != pipeline.ValidationUnknown {
		t.Fatal("default validation should be disabled")
	}
	on := true
	r = f.request("www.example.", dns.TypeA)
	r.DNSSECValidation = &on
	if out := f.serve(r); !out.Msg.AuthenticatedData || out.Security.State != pipeline.ValidationSecure {
		t.Fatal("per-zone opt-in ignored")
	}
	for _, typ := range []uint16{dns.TypeDNSKEY, dns.TypeRRSIG} {
		if typ == dns.TypeRRSIG {
			f.mu.Lock()
			sig := dns.Copy(f.replies[queryKey("www.example.", dns.TypeA)].Answer[1])
			f.replies[queryKey("www.example.", typ)] = f.message("www.example.", typ, 0, []dns.RR{sig}, nil)
			f.mu.Unlock()
		}
		name := "example."
		if typ == dns.TypeRRSIG {
			name = "www.example."
		}
		q := f.request(name, typ)
		q.Msg.Extra = nil
		q.Msg.AuthenticatedData = true
		q.DNSSECValidation = &on
		out := f.serve(q)
		if countType(out.Msg, typ) != 1 || out.Msg.AuthenticatedData != (typ == dns.TypeDNSKEY) {
			t.Fatalf("explicit type %s: %s", dns.TypeToString[typ], out.Msg)
		}
	}
}

func TestNegativeRawProofsAndSecureStaleLifetime(t *testing.T) {
	f := newFixture(t, true, true)
	neg := f.serve(f.request("missing.example.", dns.TypeA))
	if neg.Msg.Rcode != dns.RcodeNameError || !neg.Msg.AuthenticatedData || countType(neg.Msg, dns.TypeNSEC) != 1 {
		t.Fatalf("secure negative: %s", neg.Msg)
	}
	plain := f.request("missing.example.", dns.TypeA)
	plain.Msg.SetEdns0(1232, false)
	plain.Msg.AuthenticatedData = true
	neg = f.serve(plain)
	if neg.Msg.Rcode != dns.RcodeNameError || !neg.Msg.AuthenticatedData || countType(neg.Msg, dns.TypeNSEC) != 0 {
		t.Fatalf("DO0 negative: %s", neg.Msg)
	}
	r := f.serve(f.request("www.example.", dns.TypeA))
	if r.Msg.Answer[0].Header().Ttl != 2 {
		t.Fatal("MinTTL inflated authenticated TTL")
	}
	f.advance(3 * time.Second)
	f.mu.Lock()
	f.mode = 1
	f.mu.Unlock()
	r = f.serve(f.request("www.example.", dns.TypeA))
	if r.Status != pipeline.StatusCachedStale || !r.Msg.AuthenticatedData || !ede(r.Msg, dns.ExtendedErrorCodeStaleAnswer) {
		t.Fatalf("valid stale: %v %s", r.Status, r.Msg)
	}
	until := r.Security.ValidUntil
	f.nanos.Store(until.Add(-500 * time.Millisecond).UnixNano())
	r = f.serve(f.request("www.example.", dns.TypeA))
	if r.Status != pipeline.StatusCachedStale || r.Msg.Answer[0].Header().Ttl != 0 {
		t.Fatalf("stale TTL exceeded security deadline: %s", r.Msg)
	}
	f.nanos.Store(until.UnixNano())
	r = f.serve(f.request("www.example.", dns.TypeA))
	if r.Status == pipeline.StatusCachedStale || r.Msg.AuthenticatedData || len(r.Msg.Answer) > 0 {
		t.Fatal("exact security expiry served old data")
	}
}

func TestBogusRefreshCannotFallbackToSecureStale(t *testing.T) {
	f := newFixture(t, true, true)
	f.serve(f.request("www.example.", dns.TypeA))
	f.advance(3 * time.Second)
	f.mu.Lock()
	f.mode = 2
	f.mu.Unlock()
	r := f.serve(f.request("www.example.", dns.TypeA))
	if r.Status != pipeline.StatusDNSSECBogus || len(r.Msg.Answer) > 0 || r.Msg.AuthenticatedData {
		t.Fatalf("bogus refresh used stale: %+v", r)
	}
	if f.cs.Cache().Len() != 0 {
		t.Fatal("bogus refresh retained old stale copy")
	}
}

func TestNTAExpiryAndCachePersistenceRetainTrustScope(t *testing.T) {
	f := newFixture(t, true, true)
	r := f.serve(f.request("www.example.", dns.TypeA))
	var dump bytes.Buffer
	if e := f.cs.Cache().Dump(&dump); e != nil {
		t.Fatal(e)
	}
	restored := cache.New(cache.Options{MaxEntries: 1024, StaleTTL: time.Hour, Now: f.now})
	if n, e := restored.Load(bytes.NewReader(dump.Bytes())); e != nil || n != 1 {
		t.Fatalf("restore: %d %v", n, e)
	}
	if e := f.fwd.SetValidation(f.validator(nil), true); e != nil {
		t.Fatal(e)
	}
	cs := cachestage.New(restored, cachestage.Settings{}, f.fwd)
	chain := pipeline.MustNew(cs, dnssecstage.New(f.fwd), f.fwd)
	q := f.request("www.example.", dns.TypeA)
	r, e := chain.Serve(q)
	if e != nil || r.Status != pipeline.StatusCached || !r.Msg.AuthenticatedData {
		t.Fatalf("same trust restart: %v %+v", e, r)
	}
	cs.Wait()
	expires := f.now().Add(time.Minute)
	if e := f.fwd.SetValidation(f.validator([]validator.NTA{{Domain: "example.", Reason: "repair", ExpiresAt: expires}}), true); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.mode = 2
	f.mu.Unlock()
	r = f.serve(f.request("www.example.", dns.TypeA))
	if r.Security.State != pipeline.ValidationInsecure || r.Msg.AuthenticatedData || !r.Security.ValidUntil.Equal(expires) {
		t.Fatalf("NTA bypass: %+v", r.Security)
	}
	f.nanos.Store(expires.UnixNano())
	r = f.serve(f.request("www.example.", dns.TypeA))
	if r.Status != pipeline.StatusDNSSECBogus || len(r.Msg.Answer) > 0 {
		t.Fatal("NTA expired entry reused")
	}
}

func TestDetachedFetchFencedByFlushAndABAReplacement(t *testing.T) {
	for _, operation := range []string{"flush", "replace-aba"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t, true, true)
			f.mu.Lock()
			f.hold = make(chan struct{})
			f.entered = make(chan struct{})
			hold, entered := f.hold, f.entered
			f.mu.Unlock()
			defer func() {
				f.mu.Lock()
				if !f.held {
					close(hold)
				}
				f.mu.Unlock()
			}()
			done := make(chan *pipeline.Response, 1)
			go func() { r, _ := f.chain.Serve(f.request("www.example.", dns.TypeA)); done <- r }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream did not block")
			}
			switch operation {
			case "flush":
				f.cs.Cache().Flush()
			case "replace-aba":
				old := f.fwd.Set()
				v := f.validator(nil)
				middle, e := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{f.addr}}}}, upstream.Options{})
				if e != nil {
					t.Fatal(e)
				}
				defer middle.Close()
				if _, e = f.fwd.Replace(middle, v, true); e != nil {
					t.Fatal(e)
				}
				if _, e = f.fwd.Replace(old, v, true); e != nil {
					t.Fatal(e)
				}
			}
			close(hold)
			select {
			case r := <-done:
				if r == nil || r.Msg.Rcode != 0 {
					t.Fatal("captured client query failed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("query stuck")
			}
			if f.cs.Cache().Len() != 0 {
				t.Fatal("detached old fetch published after invalidation")
			}
			before := f.queriesFor(dns.TypeA)
			if r := f.serve(f.request("www.example.", dns.TypeA)); r.Status != pipeline.StatusForwarded || f.queriesFor(dns.TypeA) != before+1 {
				t.Fatal("new request joined/reused retired fetch")
			}
		})
	}
}

func TestAuthenticatedUnsignedDelegationCachedWithoutAD(t *testing.T) {
	f := newFixture(t, true, true)
	soa := f.rr("plain.example. 300 IN SOA ns.plain.example. hostmaster.plain.example. 1 3600 600 86400 300")
	f.mu.Lock()
	f.replies[queryKey("www.plain.example.", dns.TypeSOA)] = f.message("www.plain.example.", dns.TypeSOA, 0, nil, []dns.RR{soa})
	f.replies[queryKey("plain.example.", dns.TypeSOA)] = f.message("plain.example.", dns.TypeSOA, 0, nil, []dns.RR{soa})
	f.replies[queryKey("plain.example.", dns.TypeDS)] = f.message("plain.example.", dns.TypeDS, 0, nil, append(f.sign(f.rr("example. 300 IN SOA ns.example. hostmaster.example. 1 3600 600 86400 300")), f.sign(f.rr("plain.example. 300 IN NSEC z.example. NS RRSIG NSEC"))...))
	f.replies[queryKey("www.plain.example.", dns.TypeA)] = f.message("www.plain.example.", dns.TypeA, 0, []dns.RR{f.rr("www.plain.example. 60 IN A 192.0.2.1")}, nil)
	f.replies[queryKey("www.plain.example.", dns.TypeAAAA)] = f.message("www.plain.example.", dns.TypeAAAA, 0, nil, []dns.RR{soa})
	f.mu.Unlock()
	for _, typ := range []uint16{dns.TypeA, dns.TypeAAAA} {
		for i := 0; i < 2; i++ {
			out := f.serve(f.request("www.plain.example.", typ))
			if out.Msg.Rcode != 0 || out.Security.State != pipeline.ValidationInsecure || out.Msg.AuthenticatedData {
				t.Fatalf("unsigned type %d: %+v %s", typ, out.Security, out.Msg)
			}
			if i == 1 && out.Status != pipeline.StatusCached {
				t.Fatal("insecure answer did not cache")
			}
		}
	}
}

func TestSignedPrefetchAndConcurrentResponseIsolation(t *testing.T) {
	f := newFixture(t, true, true)
	f.cs.Cache().Reconfigure(cache.Options{StaleTTL: time.Hour, PrefetchThreshold: 1, PrefetchWindow: time.Minute})
	f.serve(f.request("www.example.", dns.TypeA))
	f.advance(time.Second)
	out := f.serve(f.request("www.example.", dns.TypeA))
	if out.Status != pipeline.StatusPrefetchRefresh || !out.Msg.AuthenticatedData {
		t.Fatalf("secure prefetch trigger: %v", out.Status)
	}
	f.cs.Wait()
	if f.queriesFor(dns.TypeA) != 2 {
		t.Fatal("signed prefetch did not refresh")
	}
	// Each hit owns its response. A client can mutate its message without
	// affecting either subsequent clients or the immutable raw cache record.
	out.Msg.Answer[0].(*dns.A).A[3] = 99
	out.Msg.Answer[1].(*dns.RRSIG).Signature = "corrupt"
	var wg sync.WaitGroup
	failures := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id uint16) {
			defer wg.Done()
			q := f.request("WWW.Example.", dns.TypeA)
			q.Msg.Id = id
			r, e := f.chain.Serve(q)
			if e != nil || r == nil || r.Msg.Id != id || !r.Msg.AuthenticatedData || r.Msg.Answer[0].(*dns.A).A[3] != 1 || r.Msg.Answer[1].(*dns.RRSIG).Signature == "corrupt" {
				failures <- fmt.Sprintf("client %d: %v", id, e)
			}
		}(uint16(i + 1))
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	f.cs.Wait()
}
