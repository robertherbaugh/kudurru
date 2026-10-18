package ingress

import (
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newLimiter(t testing.TB, cfg RateLimitConfig) (*RateLimiter, *fakeClock) {
	t.Helper()
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	cfg.Now = clk.Now
	l, err := NewRateLimiter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l, clk
}

func udpReq(client string, name string) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	return &pipeline.Request{Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort(client), Proto: pipeline.ProtoUDP}}
}

// counts runs n queries from client and tallies the verdicts.
func counts(l *RateLimiter, client string, n int) map[Verdict]int {
	out := map[Verdict]int{}
	for range n {
		out[l.Check(udpReq(client, "example.com."))]++
	}
	return out
}

func TestRateLimiterLevels(t *testing.T) {
	tests := []struct {
		name    string
		cfg     RateLimitConfig
		clients []string // queried in order, each perClient times
		per     int
		allowed []int // allowed per client
	}{
		{
			name:    "ipv4 /32 burst is one minute's allowance",
			cfg:     RateLimitConfig{IPv4: map[int]int{32: 6, 24: 100}},
			clients: []string{"192.0.2.1:5000", "192.0.2.2:5000"},
			per:     10, allowed: []int{6, 6},
		},
		{
			name:    "ipv4 /24 aggregates hosts",
			cfg:     RateLimitConfig{IPv4: map[int]int{32: 6, 24: 10}},
			clients: []string{"192.0.2.1:1", "192.0.2.2:1", "192.0.2.3:1", "198.51.100.1:1"},
			per:     5, allowed: []int{5, 5, 0, 5},
		},
		{
			name:    "a limited /32 does not drain its /24",
			cfg:     RateLimitConfig{IPv4: map[int]int{32: 3, 24: 6}},
			clients: []string{"192.0.2.1:1", "192.0.2.2:1"},
			per:     20, allowed: []int{3, 3},
		},
		{
			name:    "ipv6 /128, /64 and /56",
			cfg:     RateLimitConfig{IPv6: map[int]int{128: 4, 64: 6, 56: 7}},
			clients: []string{"[2001:db8:0:1::1]:1", "[2001:db8:0:1::2]:1", "[2001:db8:0:2::1]:1", "[2001:db8:1::1]:1"},
			per:     4, allowed: []int{4, 2, 1, 4},
		},
		{
			name:    "ipv4-mapped clients use the ipv4 levels",
			cfg:     RateLimitConfig{IPv4: map[int]int{32: 2}, IPv6: map[int]int{128: 100}},
			clients: []string{"[::ffff:192.0.2.9]:53"},
			per:     5, allowed: []int{2},
		},
		{
			name:    "a zero QPM disables the level",
			cfg:     RateLimitConfig{IPv4: map[int]int{32: 0, 24: 3}},
			clients: []string{"192.0.2.1:1", "192.0.2.2:1"},
			per:     2, allowed: []int{2, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, _ := newLimiter(t, tt.cfg)
			for i, c := range tt.clients {
				if got := counts(l, c, tt.per)[VerdictAllow]; got != tt.allowed[i] {
					t.Errorf("client %s: %d allowed, want %d", c, got, tt.allowed[i])
				}
			}
		})
	}
}

func TestRateLimiterRefill(t *testing.T) {
	l, clk := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 60}})
	const c = "203.0.113.5:4242"
	if got := counts(l, c, 100)[VerdictAllow]; got != 60 {
		t.Fatalf("burst: %d allowed, want 60", got)
	}
	clk.Advance(5 * time.Second) // 60 QPM = 1 token per second
	if got := counts(l, c, 10)[VerdictAllow]; got != 5 {
		t.Fatalf("after 5s: %d allowed, want 5", got)
	}
	clk.Advance(time.Hour) // refills to the burst, never beyond
	if got := counts(l, c, 100)[VerdictAllow]; got != 60 {
		t.Fatalf("after an hour: %d allowed, want 60", got)
	}
}

func TestRateLimiterSlip(t *testing.T) {
	for _, tt := range []struct {
		slip       int
		slips, drs int
	}{
		{0, 0, 100},
		{50, 50, 50},
		{10, 10, 90},
		{100, 100, 0},
	} {
		t.Run(fmt.Sprintf("slip=%d", tt.slip), func(t *testing.T) {
			l, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 1}, Slip: tt.slip})
			var hooked [3]atomic.Int64
			l.SetHook(func(v Verdict) { hooked[v].Add(1) })
			got := counts(l, "192.0.2.1:1", 101)
			if got[VerdictAllow] != 1 || got[VerdictSlip] != tt.slips || got[VerdictDrop] != tt.drs {
				t.Fatalf("verdicts %v, want 1 allow, %d slip, %d drop", got, tt.slips, tt.drs)
			}
			if hooked[VerdictAllow].Load() != 0 || hooked[VerdictSlip].Load() != int64(tt.slips) || hooked[VerdictDrop].Load() != int64(tt.drs) {
				t.Fatalf("hook saw allow=%d slip=%d drop=%d", hooked[0].Load(), hooked[1].Load(), hooked[2].Load())
			}
		})
	}
}

func TestRateLimiterExemptions(t *testing.T) {
	l, _ := newLimiter(t, RateLimitConfig{
		IPv4:   map[int]int{32: 1},
		IPv6:   map[int]int{128: 1},
		Exempt: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	for _, c := range []string{"127.0.0.1:1", "[::1]:1", "10.1.2.3:1", "[::ffff:127.0.0.2]:1"} {
		if got := counts(l, c, 50)[VerdictAllow]; got != 50 {
			t.Errorf("%s must be exempt: %d of 50 allowed", c, got)
		}
	}
	health := 0
	for range 50 {
		if l.Check(udpReq("192.0.2.1:1", "HEALTH.kudurru.internal.")) == VerdictAllow {
			health++
		}
	}
	if health != 50 {
		t.Errorf("the health name must be exempt: %d of 50 allowed", health)
	}
	for _, proto := range []pipeline.Proto{pipeline.ProtoTCP, pipeline.ProtoDoT, pipeline.ProtoDoH, pipeline.ProtoDoQ, pipeline.ProtoDoH3} {
		n := 0
		for range 20 {
			r := udpReq("192.0.2.2:1", "example.com.")
			r.Client.Proto = proto
			if l.Check(r) == VerdictAllow {
				n++
			}
		}
		if n != 20 {
			t.Errorf("%s must never be limited: %d of 20 allowed", proto, n)
		}
	}
	if got := counts(l, "192.0.2.2:1", 3)[VerdictAllow]; got != 1 {
		t.Errorf("TCP queries must not consume UDP tokens: %d allowed, want 1", got)
	}
	if l.Buckets() != 1 {
		t.Errorf("exempt traffic created buckets: %d", l.Buckets())
	}
}

// TestRateLimiterProxiedClient: the PROXYv2-attributed client is charged,
// not the dnsdist address that carried the query.
func TestRateLimiterProxiedClient(t *testing.T) {
	l, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 2}})
	allowed := 0
	for i := range 10 {
		r := udpReq(fmt.Sprintf("198.51.100.%d:53", i+1), "example.com.")
		r.Client.ProxiedFrom = netip.MustParseAddr("10.0.0.2")
		if l.Check(r) == VerdictAllow {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("ten proxied clients behind one dnsdist: %d allowed, want 10", allowed)
	}
}

func TestRateLimiterLRUBound(t *testing.T) {
	l, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 1, 24: 1000}, MaxBuckets: 128})
	for i := range 5000 {
		l.Check(udpReq(fmt.Sprintf("10.%d.%d.%d:1", i/65536+1, (i/256)%256, i%256), "x."))
		l.Check(udpReq(fmt.Sprintf("100.64.%d.%d:1", (i/256)%256, i%256), "x."))
	}
	if b := l.Buckets(); b <= 0 || b > 128 {
		t.Fatalf("buckets %d, want 1-128", b)
	}
	if got := testutil.ToFloat64(l.Collector()); int(got) != l.Buckets() {
		t.Fatalf("kudurru_ratelimit_buckets %v, want %d", got, l.Buckets())
	}
}

func TestNewRateLimiterRejects(t *testing.T) {
	for _, cfg := range []RateLimitConfig{
		{Slip: -1},
		{Slip: 101},
		{IPv4: map[int]int{33: 1}},
		{IPv6: map[int]int{129: 1}},
		{IPv4: map[int]int{24: -1}},
	} {
		if _, err := NewRateLimiter(cfg); err == nil {
			t.Errorf("NewRateLimiter(%+v) must fail", cfg)
		}
	}
}

func TestRateLimitFromConfig(t *testing.T) {
	if c, err := RateLimitFromConfig(config.RateLimit{}); c != nil || err != nil {
		t.Fatalf("disabled: %+v %v", c, err)
	}
	c, err := RateLimitFromConfig(config.RateLimit{
		Enabled: true, IPv4: map[string]int{"32": 600, "24": 6000}, IPv6: map[string]int{"64": 6000},
		Slip: 25, MaxBuckets: 10, Exempt: []string{"10.0.0.0/8", "192.0.2.7", "::ffff:198.51.100.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.IPv4[32] != 600 || c.IPv4[24] != 6000 || c.IPv6[64] != 6000 || c.Slip != 25 || c.MaxBuckets != 10 {
		t.Fatalf("converted %+v", c)
	}
	wantExempt := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.7/32"), netip.MustParsePrefix("198.51.100.1/32")}
	if fmt.Sprint(c.Exempt) != fmt.Sprint(wantExempt) {
		t.Fatalf("exempt %v, want %v", c.Exempt, wantExempt)
	}
	if _, err := RateLimitFromConfig(config.RateLimit{Enabled: true, IPv4: map[string]int{"x": 1}, Exempt: []string{"nope"}}); err == nil {
		t.Fatal("bad keys must fail")
	}
}

// TestStageRateLimit: the stage turns verdicts into a TC=1 reply, a drop
// (nil message) or the normal path.
func TestStageRateLimit(t *testing.T) {
	l, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 1}, Slip: 50})
	s := &Stage{NodeID: "n1", Limiter: l}
	passed := 0
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		passed++
		return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	var statuses []string
	for range 3 {
		resp, err := s.Serve(udpReq("192.0.2.1:1", "example.com."), next)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case resp.Msg == nil:
			statuses = append(statuses, "drop:"+resp.Status.String())
		case resp.Msg.Truncated:
			if len(resp.Msg.Answer) != 0 || len(resp.Msg.Question) != 1 || resp.Msg.Rcode != dns.RcodeSuccess {
				t.Fatalf("slip reply %v", resp.Msg)
			}
			statuses = append(statuses, "tc:"+resp.Status.String())
		default:
			statuses = append(statuses, "ok:"+resp.Status.String())
		}
	}
	want := []string{"ok:forwarded", "drop:blocked_rate_limit", "tc:blocked_rate_limit"}
	if fmt.Sprint(statuses) != fmt.Sprint(want) {
		t.Fatalf("statuses %v, want %v", statuses, want)
	}
	if passed != 1 {
		t.Fatalf("next called %d times, want 1", passed)
	}
	// The health name still answers locally while the client is limited.
	hreq := udpReq("192.0.2.1:1", HealthName)
	hreq.Msg.Question[0].Qtype = dns.TypeTXT
	resp, err := s.Serve(hreq, next)
	if err != nil || resp.Msg == nil || resp.Status != pipeline.StatusSpecialDomain {
		t.Fatalf("health while limited: %+v %v", resp, err)
	}
}

func TestStageWithoutLimiter(t *testing.T) {
	s := &Stage{NodeID: "n1"}
	n := 0
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		n++
		return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	for range 1000 {
		if _, err := s.Serve(udpReq("192.0.2.1:1", "example.com."), next); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1000 {
		t.Fatalf("%d of 1000 passed without a limiter", n)
	}
}

// BenchmarkRateLimiterCheck is the per-query cost on the UDP path: two
// IPv4 levels, clients spread over 4096 addresses, all under the limit.
func BenchmarkRateLimiterCheck(b *testing.B) {
	l, err := NewRateLimiter(RateLimitConfig{IPv4: DefaultIPv4Limits, IPv6: DefaultIPv6Limits, Slip: DefaultSlip})
	if err != nil {
		b.Fatal(err)
	}
	reqs := make([]*pipeline.Request, 4096)
	for i := range reqs {
		reqs[i] = udpReq(fmt.Sprintf("10.%d.%d.%d:1", i/65536, (i/256)%256, i%256), "example.com.")
		reqs[i].Received = time.Unix(1_800_000_000, 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var ctr atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.Check(reqs[ctr.Add(1)%uint64(len(reqs))])
		}
	})
}
