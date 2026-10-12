package ingress

import (
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestRateLimiterReconfigure(t *testing.T) {
	l, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 2}, Slip: 0})
	if got := counts(l, "192.0.2.1:1", 5); got[VerdictAllow] != 2 || got[VerdictDrop] != 3 {
		t.Fatalf("before: %v", got)
	}
	// A higher limit applies at once and every bucket starts full.
	if err := l.Reconfigure(&RateLimitConfig{IPv4: map[int]int{32: 4}, Slip: 100, Exempt: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}}); err != nil {
		t.Fatal(err)
	}
	if l.Buckets() != 0 {
		t.Fatalf("buckets after Reconfigure: %d", l.Buckets())
	}
	if got := counts(l, "192.0.2.1:1", 6); got[VerdictAllow] != 4 || got[VerdictSlip] != 2 {
		t.Fatalf("after: %v", got)
	}
	if got := counts(l, "198.51.100.7:1", 50); got[VerdictAllow] != 50 {
		t.Fatalf("exempt after Reconfigure: %v", got)
	}
	if err := l.Reconfigure(&RateLimitConfig{Slip: 101}); err == nil {
		t.Fatal("an invalid config must be refused")
	}
	// nil disables limiting; a config enables it again.
	if err := l.Reconfigure(nil); err != nil || l.Enabled() {
		t.Fatalf("disable: %v %v", err, l.Enabled())
	}
	if got := counts(l, "192.0.2.1:1", 100); got[VerdictAllow] != 100 {
		t.Fatalf("disabled: %v", got)
	}
	if err := l.Reconfigure(&RateLimitConfig{IPv4: map[int]int{32: 1}}); err != nil || !l.Enabled() {
		t.Fatalf("enable: %v", err)
	}
	if got := counts(l, "192.0.2.1:1", 3); got[VerdictAllow] != 1 {
		t.Fatalf("re-enabled: %v", got)
	}
}

func TestLameduckHealthServfail(t *testing.T) {
	var lameduck atomic.Bool
	s := &Stage{NodeID: "n1", Lameduck: lameduck.Load}
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	ask := func(qtype uint16) *dns.Msg {
		t.Helper()
		req := udpReq("192.0.2.1:1", HealthName)
		req.Msg.Question[0].Qtype = qtype
		resp, err := s.Serve(req, next)
		if err != nil || resp.Msg == nil {
			t.Fatalf("health: %+v %v", resp, err)
		}
		return resp.Msg
	}
	if m := ask(dns.TypeTXT); m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
		t.Fatalf("serving: %v", m)
	}
	lameduck.Store(true)
	for _, qt := range []uint16{dns.TypeTXT, dns.TypeA} {
		if m := ask(qt); m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
			t.Fatalf("lameduck %s: %v", dns.TypeToString[qt], m)
		}
	}
	// Other names keep resolving while the node drains.
	resp, err := s.Serve(udpReq("192.0.2.1:1", "example.com."), next)
	if err != nil || resp.Status != pipeline.StatusForwarded {
		t.Fatalf("lameduck must keep serving: %+v %v", resp, err)
	}
}
