package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
)

var quiet = slog.New(slog.DiscardHandler)

func newSet(t testing.TB, cfg Config, opts Options) *Set {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = quiet
	}
	s, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func group(strategy Strategy, timeout time.Duration, servers ...*fake) Config {
	addrs := make([]string, len(servers))
	for i, f := range servers {
		addrs[i] = f.addr
	}
	return Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: addrs, Strategy: strategy, Timeout: timeout}}}
}

func exchange(t testing.TB, s *Set, name string) (*Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := s.PrepareQuery(query(name, dns.TypeA), netip.Addr{})
	return s.Exchange(ctx, DefaultGroup, q)
}

func mustExchange(t testing.TB, s *Set, name string) *Result {
	t.Helper()
	r, err := exchange(t, s, name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStrategiesAnswer(t *testing.T) {
	for _, st := range []Strategy{"", ParallelBest, Strict, Random, RTTWeighted} {
		t.Run(string(st)+"_", func(t *testing.T) {
			a, b := startFake(t), startFake(t)
			s := newSet(t, group(st, time.Second, a, b), Options{})
			for range 10 {
				r := mustExchange(t, s, "example.com")
				ip := answerIP(r.Msg)
				if !ip.Equal(a.ip) && !ip.Equal(b.ip) {
					t.Fatalf("answer %v from neither server", ip)
				}
				want := map[string]bool{"default/" + a.addr: true, "default/" + b.addr: true}
				if !want[r.Upstream] || r.Group != DefaultGroup {
					t.Fatalf("result names %q / %q", r.Group, r.Upstream)
				}
			}
		})
	}
}

func TestStrictUsesFirstServer(t *testing.T) {
	a, b := startFake(t), startFake(t)
	s := newSet(t, group(Strict, time.Second, a, b), Options{})
	for range 5 {
		r := mustExchange(t, s, "example.com")
		if !answerIP(r.Msg).Equal(a.ip) {
			t.Fatalf("strict answered from %v", answerIP(r.Msg))
		}
	}
	if b.calls.Load() != 0 {
		t.Fatalf("strict asked the second server %d times while the first was healthy", b.calls.Load())
	}
}

func TestFailover(t *testing.T) {
	cases := []struct {
		name     string
		strategy Strategy
		mode     behaviour
	}{
		{"strict/timeout", Strict, drop},
		{"strict/servfail", Strict, servfail},
		{"strict/refused", Strict, refused},
		{"random/timeout", Random, drop},
		{"rtt/servfail", RTTWeighted, servfail},
		{"parallel/timeout", ParallelBest, drop},
		{"parallel/refused", ParallelBest, refused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad, good := startFake(t), startFake(t)
			bad.set(tc.mode, 0)
			s := newSet(t, group(tc.strategy, 150*time.Millisecond, bad, good), Options{})
			for range 4 {
				r := mustExchange(t, s, "example.com")
				if !answerIP(r.Msg).Equal(good.ip) {
					t.Fatalf("answer from %v, want the healthy server %v", answerIP(r.Msg), good.ip)
				}
			}
		})
	}
}

func TestParallelBestRacesTwo(t *testing.T) {
	fs := []*fake{startFake(t), startFake(t), startFake(t), startFake(t)}
	s := newSet(t, group(ParallelBest, time.Second, fs...), Options{})
	const n = 40
	for range n {
		mustExchange(t, s, "example.com")
	}
	// Losers may still be counted after the winner returned; wait briefly.
	time.Sleep(50 * time.Millisecond)
	var total int64
	used := 0
	for _, f := range fs {
		c := f.calls.Load()
		total += c
		if c > 0 {
			used++
		}
	}
	if total != 2*n {
		t.Fatalf("parallel_best sent %d queries for %d lookups, want exactly 2 per lookup", total, n)
	}
	if used < 3 {
		t.Fatalf("parallel_best used only %d of 4 servers over %d lookups", used, n)
	}
}

func TestParallelBestTakesFastest(t *testing.T) {
	slow, fast := startFake(t), startFake(t)
	slow.set(answer, 300*time.Millisecond)
	s := newSet(t, group(ParallelBest, time.Second, slow, fast), Options{})
	for range 5 {
		start := time.Now()
		r := mustExchange(t, s, "example.com")
		if el := time.Since(start); el > 200*time.Millisecond {
			t.Fatalf("parallel_best waited %v for the slow server", el)
		}
		if !answerIP(r.Msg).Equal(fast.ip) {
			t.Fatalf("answer from %v, want the fast server", answerIP(r.Msg))
		}
	}
}

func TestParallelBestFallsBackToRest(t *testing.T) {
	fs := []*fake{startFake(t), startFake(t), startFake(t)}
	fs[0].set(drop, 0)
	fs[1].set(drop, 0)
	// Pin the plan: with fs[2] healthy the race may pick it first; either
	// way the answer must come from it.
	s := newSet(t, group(ParallelBest, 100*time.Millisecond, fs...), Options{})
	for range 3 {
		r := mustExchange(t, s, "example.com")
		if !answerIP(r.Msg).Equal(fs[2].ip) {
			t.Fatalf("answer from %v", answerIP(r.Msg))
		}
	}
}

func TestRandomSpreads(t *testing.T) {
	a, b := startFake(t), startFake(t)
	s := newSet(t, group(Random, time.Second, a, b), Options{})
	for range 60 {
		mustExchange(t, s, "example.com")
	}
	if a.calls.Load() < 10 || b.calls.Load() < 10 {
		t.Fatalf("random strategy is lopsided: %d / %d", a.calls.Load(), b.calls.Load())
	}
	if a.calls.Load()+b.calls.Load() != 60 {
		t.Fatalf("random strategy sent %d queries for 60 lookups", a.calls.Load()+b.calls.Load())
	}
}

func TestRTTWeightedPrefersFast(t *testing.T) {
	slow, fast := startFake(t), startFake(t)
	slow.set(answer, 40*time.Millisecond)
	s := newSet(t, group(RTTWeighted, time.Second, slow, fast), Options{})
	for range 5 { // warm up: both get measured
		mustExchange(t, s, "example.com")
	}
	if slow.calls.Load() == 0 {
		// The unmeasured server must be probed during warm-up at least once
		// in expectation; force a sample to keep the test deterministic.
		g, _ := s.Group(DefaultGroup)
		g.servers[0].record(true, "", 40*time.Millisecond)
	}
	slow.calls.Store(0)
	fast.calls.Store(0)
	for range 100 {
		mustExchange(t, s, "example.com")
	}
	if f := fast.calls.Load(); f < 80 {
		t.Fatalf("rtt_weighted sent only %d of 100 queries to the fast server (slow: %d)", f, slow.calls.Load())
	}
}

func TestEveryServerFails(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		a, b := startFake(t), startFake(t)
		a.set(drop, 0)
		b.set(drop, 0)
		s := newSet(t, group(ParallelBest, 100*time.Millisecond, a, b), Options{})
		_, err := exchange(t, s, "example.com")
		var ue *Error
		if !errors.As(err, &ue) || !ue.Timeout() || !IsTimeout(err) || ue.Msg != nil {
			t.Fatalf("want a timeout *Error, got %v", err)
		}
		if len(ue.Errs) != 2 || !strings.Contains(err.Error(), a.addr) {
			t.Fatalf("error should list both servers: %v", err)
		}
	})
	t.Run("servfail", func(t *testing.T) {
		a, b := startFake(t), startFake(t)
		a.set(servfail, 0)
		b.set(servfail, 0)
		s := newSet(t, group(Strict, time.Second, a, b), Options{})
		_, err := exchange(t, s, "example.com")
		var ue *Error
		if !errors.As(err, &ue) || ue.Timeout() || IsTimeout(err) {
			t.Fatalf("want a non-timeout *Error, got %v", err)
		}
		if ue.Msg == nil || ue.Msg.Rcode != dns.RcodeServerFailure || ue.Upstream != "default/"+b.addr {
			t.Fatalf("the last SERVFAIL response should be kept: %+v", ue)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		a := startFake(t)
		a.set(drop, 0)
		s := newSet(t, group(Strict, time.Second, a), Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := s.Exchange(ctx, DefaultGroup, s.PrepareQuery(query("example.com", dns.TypeA), netip.Addr{}))
		if !IsTimeout(err) || time.Since(start) > 500*time.Millisecond {
			t.Fatalf("the caller's deadline must bound the exchange: %v after %v", err, time.Since(start))
		}
		g, _ := s.Group(DefaultGroup)
		if g.servers[0].fails != 0 {
			t.Fatal("a caller deadline must not count against the server")
		}
	})
	t.Run("unknown group", func(t *testing.T) {
		s := newSet(t, group(Strict, time.Second, startFake(t)), Options{})
		if _, err := s.Exchange(context.Background(), "nope", query("x.", dns.TypeA)); !errors.Is(err, ErrUnknownGroup) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestHealthBackoffAndRecovery(t *testing.T) {
	clock := newFakeClock()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	a, b := startFake(t), startFake(t)
	a.set(refused, 0)
	cfg := group(Strict, time.Second, a, b)
	cfg.Health = HealthConfig{Failures: 3, BackoffMin: 10 * time.Second, BackoffMax: 40 * time.Second}
	s := newSet(t, cfg, Options{Now: clock.Now, Metrics: metrics})
	labelA := "default/" + a.addr

	for range 3 {
		mustExchange(t, s, "example.com")
	}
	if got := testutil.ToFloat64(metrics.Healthy.WithLabelValues(labelA)); got != 0 {
		t.Fatalf("healthy gauge after 3 failures = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.Errors.WithLabelValues(labelA, reasonRefused)); got != 3 {
		t.Fatalf("errors_total{reason=refused} = %v", got)
	}
	// In backoff: strict goes to b first and does not touch a.
	before := a.calls.Load()
	for range 5 {
		r := mustExchange(t, s, "example.com")
		if !answerIP(r.Msg).Equal(b.ip) {
			t.Fatal("answer must come from the healthy server")
		}
	}
	if a.calls.Load() != before {
		t.Fatalf("a server in backoff was asked %d times", a.calls.Load()-before)
	}
	st := s.Status()
	if st[0].Healthy || st[0].ConsecutiveFailures != 3 || !st[0].BackoffUntil.Equal(clock.Now().Add(10*time.Second)) || !st[1].Healthy {
		t.Fatalf("status: %+v", st)
	}

	// Backoff over, still failing: one probe, backoff doubles.
	clock.Advance(11 * time.Second)
	mustExchange(t, s, "example.com")
	if a.calls.Load() != before+1 {
		t.Fatalf("expected exactly one probe, got %d", a.calls.Load()-before)
	}
	if st := s.Status(); !st[0].BackoffUntil.Equal(clock.Now().Add(20 * time.Second)) {
		t.Fatalf("backoff should double: %+v", st[0])
	}

	// Recovered: the next probe succeeds and a is first again.
	a.set(answer, 0)
	clock.Advance(21 * time.Second)
	r := mustExchange(t, s, "example.com")
	if !answerIP(r.Msg).Equal(a.ip) {
		t.Fatal("the recovered server should answer the probe")
	}
	if got := testutil.ToFloat64(metrics.Healthy.WithLabelValues(labelA)); got != 1 {
		t.Fatalf("healthy gauge after recovery = %v", got)
	}
	if got := testutil.ToFloat64(metrics.Requests.WithLabelValues("default/" + b.addr)); got < 6 {
		t.Fatalf("requests_total for b = %v", got)
	}
}

func TestAllServersInBackoffAreStillTried(t *testing.T) {
	clock := newFakeClock()
	a := startFake(t)
	a.set(refused, 0)
	cfg := group(Strict, time.Second, a)
	cfg.Health = HealthConfig{Failures: 1, BackoffMin: time.Hour}
	s := newSet(t, cfg, Options{Now: clock.Now})
	if _, err := exchange(t, s, "example.com"); err == nil {
		t.Fatal("refused must fail")
	}
	a.set(answer, 0)
	if _, err := exchange(t, s, "example.com"); err != nil {
		t.Fatalf("a group whose servers are all in backoff must still try them: %v", err)
	}
}

func TestTruncatedFallsBackToTCP(t *testing.T) {
	a := startFake(t)
	a.set(truncate, 0)
	s := newSet(t, group(Strict, time.Second, a), Options{})
	r := mustExchange(t, s, "example.com")
	if r.Msg.Truncated || !answerIP(r.Msg).Equal(a.ip) || a.tcp.Load() != 1 {
		t.Fatalf("TC answer must be retried over TCP: tc=%v answer=%v tcp=%d", r.Msg.Truncated, answerIP(r.Msg), a.tcp.Load())
	}
	// A large answer through a real UDP size limit also arrives complete.
	a.set(answer, 0)
	r = mustExchange(t, s, "big.test")
	if len(r.Msg.Answer) != 61 || r.Msg.Truncated {
		t.Fatalf("big answer: %d records, tc=%v", len(r.Msg.Answer), r.Msg.Truncated)
	}
}

func TestTCPAndDoT(t *testing.T) {
	plain := startFake(t)
	dot, pool := startDoT(t)
	cfg := Config{Groups: map[string]GroupConfig{
		DefaultGroup: {Servers: []string{"tls://" + dot.addr}, Strategy: Strict},
		"tcp":        {Servers: []string{"tcp://" + plain.addr}},
	}}
	s := newSet(t, cfg, Options{RootCAs: pool})
	r := mustExchange(t, s, "example.com")
	if !answerIP(r.Msg).Equal(dot.ip) || r.Upstream != "default/tls://"+dot.addr || dot.tcp.Load() == 0 {
		t.Fatalf("DoT answer %v via %q", answerIP(r.Msg), r.Upstream)
	}
	tr, err := s.Exchange(context.Background(), "tcp", s.PrepareQuery(query("example.com", dns.TypeA), netip.Addr{}))
	if err != nil || !answerIP(tr.Msg).Equal(plain.ip) || plain.tcp.Load() != 1 {
		t.Fatalf("tcp:// exchange: %v %v", err, tr)
	}

	// Without the test CA the certificate is rejected: a network failure.
	bad := newSet(t, Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"tls://" + dot.addr}}}}, Options{})
	if _, err := exchange(t, bad, "example.com"); err == nil || IsTimeout(err) {
		t.Fatalf("untrusted DoT certificate must fail fast: %v", err)
	}
}

func TestBootstrapResolvesHostnames(t *testing.T) {
	boot := startFake(t) // the bootstrap resolver: dns.test. is 127.0.0.1
	boot.mu.Lock()
	boot.answerIP = net.IPv4(127, 0, 0, 1)
	boot.mu.Unlock()
	target := startFake(t)
	_, port, _ := strings.Cut(target.addr, ":")
	cfg := Config{
		Groups:    map[string]GroupConfig{DefaultGroup: {Servers: []string{"dns.test:" + port}}},
		Bootstrap: []string{boot.addr},
	}
	s := newSet(t, cfg, Options{})
	r := mustExchange(t, s, "example.com")
	if !answerIP(r.Msg).Equal(target.ip) || r.Upstream != "default/dns.test:"+port {
		t.Fatalf("bootstrap: answer %v via %q", answerIP(r.Msg), r.Upstream)
	}
	if boot.calls.Load() == 0 {
		t.Fatal("bootstrap resolver was not used")
	}
	if _, err := New(Config{Groups: cfg.Groups, Bootstrap: []string{"tls://dns.example"}}, Options{}); err == nil {
		t.Fatal("a bootstrap server given by name must be rejected")
	}
}

func TestServerNamesAndLabels(t *testing.T) {
	a := startFake(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	cfg := Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"lab=" + a.addr}}}}
	s := newSet(t, cfg, Options{Metrics: m})
	r := mustExchange(t, s, "example.com")
	if r.Server != "lab" || r.Upstream != "default/lab" {
		t.Fatalf("named server: %q %q", r.Server, r.Upstream)
	}
	if testutil.ToFloat64(m.Requests.WithLabelValues("default/lab")) != 1 {
		t.Fatal("metrics must use the display name")
	}
	if c := testutil.CollectAndCount(m.Duration); c != 1 {
		t.Fatalf("duration series: %d", c)
	}

	// A reload that drops the server deletes its series; one that keeps it
	// does not.
	next := newSet(t, cfg, Options{Metrics: m})
	if err := s.CloseReplacedBy(next); err != nil {
		t.Fatal(err)
	}
	if testutil.CollectAndCount(m.Healthy) != 1 {
		t.Fatal("kept server lost its series")
	}
	other := newSet(t, group(Strict, time.Second, startFake(t)), Options{Metrics: m})
	if err := next.CloseReplacedBy(other); err != nil {
		t.Fatal(err)
	}
	if testutil.CollectAndCount(m.Requests) != 1 { // only the new server
		t.Fatalf("removed server kept its series: %d", testutil.CollectAndCount(m.Requests))
	}

	for _, tc := range []struct{ entry, name, addr string }{
		{"cf=tls://1.1.1.1", "cf", "tls://1.1.1.1"},
		{"tls://1.1.1.1", "", "tls://1.1.1.1"},
		{"https://dns.example/dns-query?x=1", "", "https://dns.example/dns-query?x=1"},
		{"[::1]:53", "", "[::1]:53"},
	} {
		n, ad := splitName(tc.entry)
		if n != tc.name || ad != tc.addr {
			t.Errorf("splitName(%q) = %q, %q", tc.entry, n, ad)
		}
	}
}

func TestNewRejectsBadServers(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no default", Config{Groups: map[string]GroupConfig{"x": {Servers: []string{"1.1.1.1"}}}}, `group "default" is required`},
		{"bad scheme", Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"gopher://1.1.1.1"}}}}, "unsupported url scheme"},
		{"bad port", Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"1.1.1.1:99999"}}}}, "invalid port"},
		{"duplicate", Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"1.1.1.1", "1.1.1.1"}}}}, "twice"},
		{"strategy", Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"1.1.1.1"}, Strategy: "fastest"}}}, "unknown strategy"},
		{"empty", Config{Groups: map[string]GroupConfig{DefaultGroup: {}}}, "no servers"},
		{"conditional group", Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"1.1.1.1"}}}, Conditional: map[string]string{"corp.": "ad"}}, "unknown group"},
		{"ecs", Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"1.1.1.1"}}}, ECS: ECSConfig{Mode: "leak"}}, "unknown ECS mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	// Every transport scheme parses without dialing.
	all := []string{"1.1.1.1", "1.1.1.1:53", "udp://1.1.1.1", "tcp://1.1.1.1", "tls://1.1.1.1", "https://1.1.1.1/dns-query",
		"quic://1.1.1.1", "h3://1.1.1.1/dns-query", "tls://dns.example",
		"sdns://AgcAAAAAAAAABzEuMC4wLjEAEmRucy5jbG91ZGZsYXJlLmNvbQovZG5zLXF1ZXJ5"}
	s, err := New(Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: all}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}

func TestRoute(t *testing.T) {
	s := newSet(t, Config{
		Groups: map[string]GroupConfig{
			DefaultGroup: {Servers: []string{"127.0.0.1:1"}},
			"ad":         {Servers: []string{"127.0.0.1:2"}},
			"lab":        {Servers: []string{"127.0.0.1:3"}},
		},
		Conditional: map[string]string{
			"corp.example.":    "ad",
			"Lab.Corp.Example": "lab",
			"[/10.in-addr.arpa/168.192.in-addr.arpa/]": "ad",
			"public.corp.example":                      DefaultGroup, // dnsmasq "#" exception
		},
	}, Options{})
	for _, tc := range []struct {
		qname, hint, group string
		conditional        bool
	}{
		{"www.example.com.", "", DefaultGroup, false},
		{"corp.example.", "", "ad", true},
		{"HOST.CORP.example.", "", "ad", true},
		{"x.lab.corp.example.", "", "lab", true},
		{"notcorp.example.", "", DefaultGroup, false},
		{"5.0.0.10.in-addr.arpa.", "", "ad", true},
		{"1.1.168.192.in-addr.arpa.", "", "ad", true},
		{"www.example.com.", "lab", "lab", true},
		{"corp.example.", "lab", "lab", true},
		{"corp.example", "", "ad", true},
		{"www.public.corp.example.", "", DefaultGroup, false},
		{"www.public.corp.example.", DefaultGroup, DefaultGroup, true},
	} {
		g, c, err := s.Route(tc.qname, tc.hint)
		if err != nil || g != tc.group || c != tc.conditional {
			t.Errorf("Route(%q, %q) = %q, %v, %v; want %q, %v", tc.qname, tc.hint, g, c, err, tc.group, tc.conditional)
		}
	}
	if _, _, err := s.Route("x.", "missing"); !errors.Is(err, ErrUnknownGroup) {
		t.Fatalf("unknown hint: %v", err)
	}
	root := newSet(t, Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"127.0.0.1:1"}}, "all": {Servers: []string{"127.0.0.1:2"}}}, Conditional: map[string]string{".": "all"}}, Options{})
	if g, _, _ := root.Route("anything.example.", ""); g != "all" {
		t.Fatalf("root suffix: %q", g)
	}
}

func TestConfigFrom(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := ConfigFrom(cfg.Upstreams)
	g := c.Groups[DefaultGroup]
	if g.Strategy != ParallelBest || g.Timeout != 2*time.Second || len(g.Servers) != 2 {
		t.Fatalf("default group: %+v", g)
	}
	if c.ECS.Mode != ECSStrip || c.ECS.IPv4Prefix != 24 || c.ECS.IPv6Prefix != 56 || len(c.Bootstrap) != 1 {
		t.Fatalf("converted config: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentExchanges(t *testing.T) {
	a, b := startFake(t), startFake(t)
	b.set(answer, 2*time.Millisecond)
	for _, st := range []Strategy{ParallelBest, Strict, Random, RTTWeighted} {
		s := newSet(t, group(st, time.Second, a, b), Options{Metrics: NewMetrics(nil)})
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				for range 10 {
					if _, err := exchange(t, s, "example.com"); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		wg.Wait()
		_ = s.Status()
	}
}
