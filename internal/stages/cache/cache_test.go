package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeUpstream is the rest of the chain: it answers like the upstream stage
// would and counts calls.
type fakeUpstream struct {
	calls   atomic.Int64
	mu      sync.Mutex
	respond func(req *pipeline.Request) (*pipeline.Response, error)
}

func (f *fakeUpstream) next(req *pipeline.Request) (*pipeline.Response, error) {
	f.calls.Add(1)
	f.mu.Lock()
	fn := f.respond
	f.mu.Unlock()
	return fn(req)
}

func (f *fakeUpstream) set(fn func(req *pipeline.Request) (*pipeline.Response, error)) {
	f.mu.Lock()
	f.respond = fn
	f.mu.Unlock()
}

// forwarded answers A ip with ttl, as the upstream stage would for the
// default group (Cacheable, upstream EDNS adapted to the client).
func forwarded(ip string, ttl uint32) func(req *pipeline.Request) (*pipeline.Response, error) {
	return func(req *pipeline.Request) (*pipeline.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		q := req.Msg.Question[0]
		m.Answer = []dns.RR{
			&dns.CNAME{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: ttl * 10}, Target: "target.example."},
			&dns.A{Hdr: dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: net.ParseIP(ip)},
		}
		m.SetEdns0(1232, false)
		upstream.FinishReply(req.Msg, m, -1)
		return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Upstream: "default/test", Cacheable: true}, nil
	}
}

func failing(status pipeline.Status) func(req *pipeline.Request) (*pipeline.Response, error) {
	return func(req *pipeline.Request) (*pipeline.Response, error) {
		r := pipeline.Reply(req, dns.RcodeServerFailure, status)
		return r, nil
	}
}

type harness struct {
	t     *testing.T
	clk   *clock
	m     *cache.Metrics
	stage *Stage
	up    *fakeUpstream
}

func newHarness(t *testing.T, mutate func(*config.Cache)) *harness {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cc := cfg.Cache
	cc.ServeStale.MaxWait = config.Duration(100 * time.Millisecond)
	if mutate != nil {
		mutate(&cc)
	}
	clk := &clock{now: time.Unix(1_800_000_000, 0)}
	m := cache.NewMetrics(nil)
	opts := cache.OptionsFrom(cc, m)
	opts.Now = clk.Now
	h := &harness{t: t, clk: clk, m: m, up: &fakeUpstream{}}
	h.stage = New(cache.New(opts), SettingsFrom(cc), nil)
	h.up.set(forwarded("192.0.2.1", 300))
	return h
}

func request(name string, qtype uint16, id uint16) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.Id = id
	return &pipeline.Request{Ctx: context.Background(), Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.200:4000")}}
}

func (h *harness) serve(req *pipeline.Request) *pipeline.Response {
	h.t.Helper()
	resp, err := h.stage.Serve(req, h.up.next)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func ttls(m *dns.Msg) []uint32 {
	var out []uint32
	for _, sec := range [][]dns.RR{m.Answer, m.Ns} {
		for _, rr := range sec {
			out = append(out, rr.Header().Ttl)
		}
	}
	return out
}

func TestMissThenHitWithCountdown(t *testing.T) {
	h := newHarness(t, nil)
	r1 := h.serve(request("www.example.com", dns.TypeA, 1))
	if r1.Status != pipeline.StatusForwarded || h.up.calls.Load() != 1 || r1.Msg.Id != 1 || r1.Cacheable {
		t.Fatalf("miss: %v calls %d cacheable %v", r1.Status, h.up.calls.Load(), r1.Cacheable)
	}
	h.clk.Advance(100 * time.Second)
	req := request("WWW.Example.COM", dns.TypeA, 2)
	req.Msg.SetEdns0(4096, false)
	r2 := h.serve(req)
	if r2.Status != pipeline.StatusCached || h.up.calls.Load() != 1 {
		t.Fatalf("hit: %v calls %d", r2.Status, h.up.calls.Load())
	}
	if got := ttls(r2.Msg); fmt.Sprint(got) != "[2900 200]" {
		t.Fatalf("TTL countdown: %v", got)
	}
	if r2.Msg.Id != 2 || r2.Msg.Question[0].Name != "WWW.Example.COM." || r2.Msg.Authoritative || !r2.Msg.RecursionAvailable || !r2.Msg.Response {
		t.Fatalf("reply header: %+v %v", r2.Msg.MsgHdr, r2.Msg.Question)
	}
	if opt := r2.Msg.IsEdns0(); opt == nil || opt.UDPSize() != upstream.EDNSBufferSize {
		t.Fatal("EDNS client must get an OPT")
	}
	if r2.Cacheable || r2.Upstream != "" {
		t.Fatal("cache answers are not re-stored and name no upstream")
	}
	if _, err := r2.Msg.Pack(); err != nil {
		t.Fatal(err)
	}
	// The stored wire has no OPT and a zero ID.
	l, _ := h.stage.Cache().Peek(cache.MakeKey("www.example.com.", dns.TypeA, dns.ClassINET, false, false, netip.Prefix{}))
	stored := new(dns.Msg)
	if err := stored.Unpack(l.Wire); err != nil || stored.IsEdns0() != nil || stored.Id != 0 {
		t.Fatalf("stored message: %v %v", err, stored)
	}
	if hits := testutil.ToFloat64(h.m.Hits.WithLabelValues("positive", "false")); hits != 1 {
		t.Fatalf("hits = %v", hits)
	}
}

func TestKeysSeparateDOAndECS(t *testing.T) {
	h := newHarness(t, nil)
	h.stage.ecs = subnetFunc(func(_ *dns.Msg, client netip.Addr) netip.Prefix {
		if client.Is4() && client.As4()[0] == 203 {
			return netip.PrefixFrom(client, 24).Masked()
		}
		return netip.Prefix{}
	})
	plain := request("x.example", dns.TypeA, 1)
	do := request("x.example", dns.TypeA, 1)
	do.Msg.SetEdns0(1232, true)
	cd := request("x.example", dns.TypeA, 1)
	cd.Msg.CheckingDisabled = true
	ecsA := request("x.example", dns.TypeA, 1)
	ecsA.Client.Addr = netip.MustParseAddrPort("203.0.113.5:1")
	ecsB := request("x.example", dns.TypeA, 1)
	ecsB.Client.Addr = netip.MustParseAddrPort("203.0.114.5:1")
	ecsA2 := request("x.example", dns.TypeA, 1)
	ecsA2.Client.Addr = netip.MustParseAddrPort("203.0.113.99:1")
	aaaa := request("x.example", dns.TypeAAAA, 1)
	for _, r := range []*pipeline.Request{plain, do, cd, ecsA, ecsB, aaaa} {
		h.serve(r)
	}
	if h.up.calls.Load() != 6 {
		t.Fatalf("each variant is its own entry: %d upstream calls", h.up.calls.Load())
	}
	if r := h.serve(ecsA2); r.Status != pipeline.StatusCached {
		t.Fatal("same /24 shares the entry")
	}
}

type subnetFunc func(m *dns.Msg, client netip.Addr) netip.Prefix

func (f subnetFunc) ECSSubnet(m *dns.Msg, client netip.Addr) netip.Prefix { return f(m, client) }

func TestWhatIsNotCached(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		name    string
		respond func(*pipeline.Request) (*pipeline.Response, error)
	}{
		{"conditional", func(req *pipeline.Request) (*pipeline.Response, error) {
			r, _ := forwarded("10.0.0.1", 300)(req)
			r.Cacheable = false
			return r, nil
		}},
		{"servfail", failing(pipeline.StatusServfailUpstream)},
		{"refused", func(req *pipeline.Request) (*pipeline.Response, error) {
			r := pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusForwarded)
			r.Cacheable = true
			return r, nil
		}},
		{"truncated", func(req *pipeline.Request) (*pipeline.Response, error) {
			r, _ := forwarded("192.0.2.1", 300)(req)
			r.Msg.Truncated = true
			return r, nil
		}},
		{"zero ttl without minTTL", func(req *pipeline.Request) (*pipeline.Response, error) {
			return forwarded("192.0.2.1", 0)(req)
		}},
		{"error", func(*pipeline.Request) (*pipeline.Response, error) { return nil, errors.New("boom") }},
	}
	set := *h.stage.settings()
	set.MinTTL = 0
	h.stage.SetSettings(set)
	for _, tc := range cases {
		h.up.set(tc.respond)
		before := h.up.calls.Load()
		_, _ = h.stage.Serve(request(tc.name+".example", dns.TypeA, 1), h.up.next) // the error case returns one
		_, _ = h.stage.Serve(request(tc.name+".example", dns.TypeA, 2), h.up.next) // the error case returns one
		if h.up.calls.Load()-before != 2 {
			t.Errorf("%s was cached", tc.name)
		}
	}
	// Queries the cache never handles go straight through.
	h.up.set(forwarded("192.0.2.1", 300))
	for _, req := range []*pipeline.Request{
		request("any.example", dns.TypeANY, 1),
		func() *pipeline.Request {
			r := request("chaos.example", dns.TypeTXT, 1)
			r.Msg.Question[0].Qclass = dns.ClassCHAOS
			return r
		}(),
		func() *pipeline.Request {
			r := request("notify.example", dns.TypeSOA, 1)
			r.Msg.Opcode = dns.OpcodeNotify
			return r
		}(),
	} {
		before := h.up.calls.Load()
		h.serve(req)
		h.serve(req)
		if h.up.calls.Load()-before != 2 {
			t.Errorf("%v must bypass the cache", req.Msg.Question[0])
		}
	}
}

func TestUpstreamGroupHintBypassesCache(t *testing.T) {
	h := newHarness(t, nil)
	h.serve(request("app.corp.example", dns.TypeA, 1)) // cached via the default group
	hinted := request("app.corp.example", dns.TypeA, 2)
	hinted.UpstreamGroup = "corp"
	h.up.set(func(req *pipeline.Request) (*pipeline.Response, error) {
		if req.UpstreamGroup != "corp" {
			t.Errorf("hint lost: %q", req.UpstreamGroup)
		}
		r, err := forwarded("10.0.0.1", 300)(req)
		r.Cacheable = false
		return r, err
	})
	for range 2 {
		if r := h.serve(hinted); r.Status != pipeline.StatusForwarded {
			t.Fatalf("hinted query answered from the cache: %v", r.Status)
		}
	}
	if h.up.calls.Load() != 3 {
		t.Fatalf("upstream calls %d", h.up.calls.Load())
	}
}

func TestTTLClamps(t *testing.T) {
	soa := func(ttl, minttl uint32) dns.RR {
		return &dns.SOA{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl}, Ns: "ns.example.", Mbox: "h.example.", Serial: 1, Refresh: 1, Retry: 1, Expire: 1, Minttl: minttl}
	}
	for _, tc := range []struct {
		name     string
		rcode    int
		answer   uint32 // 0: no answer
		ns       dns.RR
		wantTTL  time.Duration
		wantKind cache.Kind
	}{
		{"min clamp", dns.RcodeSuccess, 1, nil, 5 * time.Second, cache.Positive},
		{"max clamp", dns.RcodeSuccess, 200000, nil, 24 * time.Hour, cache.Positive},
		{"in range", dns.RcodeSuccess, 600, nil, 600 * time.Second, cache.Positive},
		{"nxdomain soa minimum", dns.RcodeNameError, 0, soa(3600, 60), 60 * time.Second, cache.Negative},
		{"nxdomain soa ttl", dns.RcodeNameError, 0, soa(30, 600), 30 * time.Second, cache.Negative},
		{"nxdomain negative cap", dns.RcodeNameError, 0, soa(86400, 86400), 5 * time.Minute, cache.Negative},
		{"nodata", dns.RcodeSuccess, 0, soa(120, 120), 120 * time.Second, cache.Negative},
		{"nxdomain without soa", dns.RcodeNameError, 0, nil, 5 * time.Second, cache.Negative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.up.set(func(req *pipeline.Request) (*pipeline.Response, error) {
				m := new(dns.Msg)
				m.SetRcode(req.Msg, tc.rcode)
				if tc.answer > 0 {
					m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "t.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: tc.answer}, A: net.IPv4(192, 0, 2, 1)}}
				}
				if tc.ns != nil {
					m.Ns = []dns.RR{dns.Copy(tc.ns)}
				}
				return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Cacheable: true}, nil
			})
			h.serve(request("t.example", dns.TypeA, 1))
			l, ok := h.stage.Cache().Peek(cache.MakeKey("t.example.", dns.TypeA, dns.ClassINET, false, false, netip.Prefix{}))
			if !ok || l.TTL != tc.wantTTL || l.Kind != tc.wantKind {
				t.Fatalf("stored ttl %v kind %v, want %v %v", l.TTL, l.Kind, tc.wantTTL, tc.wantKind)
			}
			r := h.serve(request("t.example", dns.TypeA, 2))
			for _, ttl := range ttls(r.Msg) {
				if time.Duration(ttl)*time.Second > tc.wantTTL {
					t.Fatalf("served TTL %d exceeds the entry lifetime %v", ttl, tc.wantTTL)
				}
			}
			if r.Msg.Rcode != tc.rcode {
				t.Fatalf("rcode %d", r.Msg.Rcode)
			}
		})
	}
}

func TestCoalescing(t *testing.T) {
	h := newHarness(t, nil)
	release := make(chan struct{})
	inner := forwarded("192.0.2.7", 300)
	h.up.set(func(req *pipeline.Request) (*pipeline.Response, error) {
		<-release
		return inner(req)
	})
	const n = 50
	var wg sync.WaitGroup
	results := make([]*pipeline.Response, n)
	for i := range n {
		wg.Go(func() {
			req := request("burst.example", dns.TypeA, uint16(1000+i)) //nolint:gosec // small
			if i%2 == 0 {
				req.Msg.SetEdns0(4096, false)
			}
			resp, err := h.stage.Serve(req, h.up.next)
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = resp
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if c := h.up.calls.Load(); c != 1 {
		t.Fatalf("%d concurrent misses made %d upstream calls", n, c)
	}
	for i, r := range results {
		if r == nil || r.Msg.Id != uint16(1000+i) || (i%2 == 0) != (r.Msg.IsEdns0() != nil) { //nolint:gosec // small
			t.Fatalf("result %d not personalised: %+v", i, r)
		}
	}
	// Followers got copies: changing one does not affect another.
	results[0].Msg.Answer[0].Header().Ttl = 1
	if results[1].Msg.Answer[0].Header().Ttl == 1 {
		t.Fatal("coalesced results share a message")
	}
}

func TestClientGivesUp(t *testing.T) {
	h := newHarness(t, nil)
	release := make(chan struct{})
	inner := forwarded("192.0.2.8", 300)
	h.up.set(func(req *pipeline.Request) (*pipeline.Response, error) {
		<-release
		return inner(req)
	})
	req := request("slow.example", dns.TypeA, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req.Ctx = ctx
	if _, err := h.stage.Serve(req, h.up.next); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	close(release)
	// The detached fetch still completes and fills the cache.
	deadline := time.Now().Add(2 * time.Second)
	for h.stage.Cache().Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r := h.serve(request("slow.example", dns.TypeA, 2)); r.Status != pipeline.StatusCached {
		t.Fatalf("after the detached fetch: %v", r.Status)
	}
}

func TestPrefetch(t *testing.T) {
	h := newHarness(t, func(c *config.Cache) {
		c.Prefetch.Threshold = 3
		c.Prefetch.Window = config.Duration(time.Hour)
	})
	h.up.set(forwarded("192.0.2.1", 100))
	h.serve(request("hot.example", dns.TypeA, 1))
	for range 3 {
		if r := h.serve(request("hot.example", dns.TypeA, 2)); r.Status != pipeline.StatusCached {
			t.Fatalf("hit: %v", r.Status)
		}
	}
	h.up.set(forwarded("192.0.2.2", 100))
	h.clk.Advance(95 * time.Second)
	r := h.serve(request("hot.example", dns.TypeA, 3))
	if r.Status != pipeline.StatusPrefetchRefresh {
		t.Fatalf("trigger hit: %v", r.Status)
	}
	if a := r.Msg.Answer[1].(*dns.A); !a.A.Equal(net.IPv4(192, 0, 2, 1)) || a.Hdr.Ttl != 5 {
		t.Fatalf("the trigger hit is served from the old entry: %v", r.Msg.Answer[1])
	}
	h.stage.Wait()
	if h.up.calls.Load() != 2 {
		t.Fatalf("upstream calls %d", h.up.calls.Load())
	}
	r = h.serve(request("hot.example", dns.TypeA, 4))
	if a := r.Msg.Answer[1].(*dns.A); r.Status != pipeline.StatusCached || !a.A.Equal(net.IPv4(192, 0, 2, 2)) || a.Hdr.Ttl != 100 {
		t.Fatalf("after prefetch: %v %v", r.Status, r.Msg.Answer[1])
	}
	if got := testutil.ToFloat64(h.m.Prefetches); got != 1 {
		t.Fatalf("prefetches = %v", got)
	}

	// A failing prefetch keeps the old entry and releases the claim.
	h.up.set(failing(pipeline.StatusServfailUpstream))
	h.clk.Advance(95 * time.Second)
	if r := h.serve(request("hot.example", dns.TypeA, 5)); r.Status != pipeline.StatusPrefetchRefresh {
		t.Fatalf("second trigger: %v", r.Status)
	}
	h.stage.Wait()
	if r := h.serve(request("hot.example", dns.TypeA, 6)); r.Status != pipeline.StatusPrefetchRefresh {
		t.Fatalf("after a failed prefetch the claim is free again: %v", r.Status)
	}
	h.stage.Wait()
}

func TestServeStale(t *testing.T) {
	h := newHarness(t, nil)
	h.up.set(forwarded("192.0.2.1", 60))
	h.serve(request("stale.example", dns.TypeA, 1))
	h.clk.Advance(2 * time.Hour)

	// Every upstream fails: stale answer, TTL 30, EDE 3.
	h.up.set(failing(pipeline.StatusTimeoutUpstream))
	req := request("stale.example", dns.TypeA, 2)
	req.Msg.SetEdns0(4096, false)
	r := h.serve(req)
	if r.Status != pipeline.StatusCachedStale || r.Msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("stale: %v", r.Status)
	}
	for _, ttl := range ttls(r.Msg) {
		if ttl != 30 {
			t.Fatalf("stale TTLs %v", ttls(r.Msg))
		}
	}
	if ede := edeCode(r.Msg); ede != dns.ExtendedErrorCodeStaleAnswer {
		t.Fatalf("EDE %d", ede)
	}
	calls := h.up.calls.Load()

	// Within the failure-recheck hold: stale at once, upstream untouched.
	if r := h.serve(request("stale.example", dns.TypeA, 3)); r.Status != pipeline.StatusCachedStale || h.up.calls.Load() != calls {
		t.Fatalf("hold: %v, calls %d", r.Status, h.up.calls.Load()-calls)
	}

	// After the hold, a slow upstream: stale after maxWait, the refresh
	// lands in the cache afterwards.
	h.clk.Advance(31 * time.Second)
	release := make(chan struct{})
	h.up.set(func(req *pipeline.Request) (*pipeline.Response, error) {
		<-release
		return forwarded("192.0.2.9", 60)(req)
	})
	start := time.Now()
	r = h.serve(request("stale.example", dns.TypeA, 4))
	if r.Status != pipeline.StatusCachedStale || time.Since(start) < 90*time.Millisecond || time.Since(start) > time.Second {
		t.Fatalf("maxWait: %v after %v", r.Status, time.Since(start))
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if l, _ := h.stage.Cache().Peek(cache.MakeKey("stale.example.", dns.TypeA, dns.ClassINET, false, false, netip.Prefix{})); l.State == cache.Fresh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late refresh never stored")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r = h.serve(request("stale.example", dns.TypeA, 5))
	if a := r.Msg.Answer[1].(*dns.A); r.Status != pipeline.StatusCached || !a.A.Equal(net.IPv4(192, 0, 2, 9)) {
		t.Fatalf("after late refresh: %v %v", r.Status, r.Msg.Answer)
	}

	// Expired with a working upstream: the fresh answer wins.
	h.clk.Advance(2 * time.Minute)
	h.up.set(forwarded("192.0.2.10", 60))
	if r := h.serve(request("stale.example", dns.TypeA, 6)); r.Status != pipeline.StatusForwarded {
		t.Fatalf("refresh: %v", r.Status)
	}

	if got := testutil.ToFloat64(h.m.Hits.WithLabelValues("positive", "true")); got != 3 {
		t.Fatalf("stale hits = %v", got)
	}

	// Past the 3-day stale window the entry is gone: SERVFAIL passes through.
	h.clk.Advance(73 * time.Hour)
	h.up.set(failing(pipeline.StatusServfailUpstream))
	if r := h.serve(request("stale.example", dns.TypeA, 7)); r.Status != pipeline.StatusServfailUpstream {
		t.Fatalf("beyond the stale window: %v", r.Status)
	}
}

func TestServeStaleDisabled(t *testing.T) {
	h := newHarness(t, func(c *config.Cache) { c.ServeStale.TTL = 0 })
	h.up.set(forwarded("192.0.2.1", 60))
	h.serve(request("x.example", dns.TypeA, 1))
	h.clk.Advance(61 * time.Second)
	h.up.set(failing(pipeline.StatusServfailUpstream))
	if r := h.serve(request("x.example", dns.TypeA, 2)); r.Status != pipeline.StatusServfailUpstream {
		t.Fatalf("serve-stale off: %v", r.Status)
	}
}

func TestCorruptEntryIsDropped(t *testing.T) {
	h := newHarness(t, nil)
	k := cache.MakeKey("bad.example.", dns.TypeA, dns.ClassINET, false, false, netip.Prefix{})
	h.stage.Cache().Set(k, []byte{1, 2, 3}, cache.Positive, time.Minute)
	if r := h.serve(request("bad.example", dns.TypeA, 1)); r.Status != pipeline.StatusForwarded || h.up.calls.Load() != 1 {
		t.Fatalf("corrupt entry: %v", r.Status)
	}
	if r := h.serve(request("bad.example", dns.TypeA, 2)); r.Status != pipeline.StatusCached {
		t.Fatalf("replaced entry: %v", r.Status)
	}
}

func TestChainOrder(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := pipeline.New(h.stage, &passthrough{"upstream"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.New(&passthrough{"upstream"}, h.stage); err == nil {
		t.Fatal("cache must come before upstream")
	}
	if h.stage.Name() != "cache" {
		t.Fatal(h.stage.Name())
	}
}

type passthrough struct{ name string }

func (p *passthrough) Name() string { return p.name }
func (p *passthrough) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	return next(req)
}

func TestConcurrentMix(t *testing.T) {
	h := newHarness(t, func(c *config.Cache) {
		c.MaxEntries = 64
		c.Prefetch.Threshold = 2
	})
	h.up.set(forwarded("192.0.2.1", 5))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 300 {
				req := request(fmt.Sprintf("n%d.example", (i*3+g)%100), dns.TypeA, uint16(i)) //nolint:gosec // small
				if _, err := h.stage.Serve(req, h.up.next); err != nil {
					t.Error(err)
					return
				}
				if i%50 == 0 {
					h.clk.Advance(3 * time.Second)
				}
			}
		})
	}
	wg.Wait()
	h.stage.Wait()
}

func edeCode(m *dns.Msg) uint16 {
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if e, ok := o.(*dns.EDNS0_EDE); ok {
				return e.InfoCode
			}
		}
	}
	return 0xffff
}

func TestKeepsDNSSECState(t *testing.T) {
	h := newHarness(t, nil)
	fwd := forwarded("192.0.2.1", 300)
	h.up.set(func(req *pipeline.Request) (*pipeline.Response, error) {
		r, err := fwd(req)
		r.DNSSEC = pipeline.DNSSECSecure
		return r, err
	})
	if r := h.serve(request("secure.example", dns.TypeA, 1)); r.DNSSEC != pipeline.DNSSECSecure {
		t.Fatalf("miss: %d", r.DNSSEC)
	}
	r := h.serve(request("secure.example", dns.TypeA, 2))
	if r.Status != pipeline.StatusCached || r.DNSSEC != pipeline.DNSSECSecure {
		t.Fatalf("hit: %v %d", r.Status, r.DNSSEC)
	}
	for _, rr := range r.Msg.Extra {
		if opt, ok := rr.(*dns.OPT); ok {
			for _, o := range opt.Option {
				if o.Option() == dnssecOption {
					t.Fatal("the private option must not reach clients")
				}
			}
		}
	}
	// Answers kudurru did not validate stay unknown.
	h.up.set(forwarded("192.0.2.2", 300))
	h.serve(request("plain.example", dns.TypeA, 3))
	if r := h.serve(request("plain.example", dns.TypeA, 4)); r.DNSSEC != pipeline.DNSSECUnknown {
		t.Fatalf("unvalidated: %d", r.DNSSEC)
	}
}
