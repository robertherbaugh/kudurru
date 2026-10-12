package upstream

import (
	"testing"
	"time"

	proxy "github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestProtoLabel(t *testing.T) {
	for _, tt := range []struct{ addr, want string }{
		{"1.1.1.1:53", "udp"},
		{"1.1.1.1", "udp"},
		{"udp://1.1.1.1", "udp"},
		{"tcp://1.1.1.1", "tcp"},
		{"tls://1.1.1.1", "dot"},
		{"https://1.1.1.1/dns-query", "doh"},
		{"h3://1.1.1.1/dns-query", "doh3"},
		{"quic://1.1.1.1", "doq"},
		// AdGuard DNS's published DNSCrypt stamp.
		{"sdns://AQMAAAAAAAAAETk0LjE0MC4xNC4xNDo1NDQzINErR_JS3PLCu_iZEIbq95zkSV2LFsigxDIuUso_OQhzIjIuZG5zY3J5cHQuZGVmYXVsdC5uczEuYWRndWFyZC5jb20", "dnscrypt"},
	} {
		u, err := proxy.AddressToUpstream(tt.addr, &proxy.Options{Timeout: time.Second, Logger: quiet})
		if err != nil {
			t.Errorf("%s: %v", tt.addr, err)
			continue
		}
		if got := protoLabel(tt.addr, u); got != tt.want {
			t.Errorf("protoLabel(%s) = %q (Address %q), want %q", tt.addr, got, u.Address(), tt.want)
		}
		_ = u.Close()
	}
}

func TestRcodeLabel(t *testing.T) {
	if rcodeLabel(0) != "NOERROR" || rcodeLabel(3) != "NXDOMAIN" || rcodeLabel(4000) != "other" {
		t.Fatal("rcode labels")
	}
}

// TestResponsesInflightAndProto: every exchange counts once in
// requests_total{upstream,proto}, every response in
// responses_total{upstream,rcode}, and inflight returns to zero.
func TestResponsesInflightAndProto(t *testing.T) {
	ok := startFake(t)
	bad := startFake(t)
	bad.set(servfail, 0)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	s := newSet(t, group(Strict, time.Second, bad, ok), Options{Metrics: m})
	for range 3 {
		mustExchange(t, s, "example.com")
	}
	okLabel, badLabel := "default/"+ok.addr, "default/"+bad.addr
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"requests ok/udp", testutil.ToFloat64(m.Requests.WithLabelValues(okLabel, "udp")), 3},
		{"requests bad/udp", testutil.ToFloat64(m.Requests.WithLabelValues(badLabel, "udp")), 3},
		{"responses ok NOERROR", testutil.ToFloat64(m.Responses.WithLabelValues(okLabel, "NOERROR")), 3},
		{"responses bad SERVFAIL", testutil.ToFloat64(m.Responses.WithLabelValues(badLabel, "SERVFAIL")), 3},
		{"inflight ok", testutil.ToFloat64(m.Inflight.WithLabelValues(okLabel)), 0},
		{"inflight bad", testutil.ToFloat64(m.Inflight.WithLabelValues(badLabel)), 0},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// A dropped query is a request without a response.
	drops := startFake(t)
	drops.set(drop, 0)
	ds := newSet(t, group(Strict, 100*time.Millisecond, drops), Options{Metrics: m})
	if _, err := exchange(t, ds, "example.com"); err == nil {
		t.Fatal("a dropping server must fail the exchange")
	}
	dl := "default/" + drops.addr
	responses := 0.0
	for _, rc := range []string{"NOERROR", "NXDOMAIN", "SERVFAIL", "REFUSED"} {
		responses += testutil.ToFloat64(m.Responses.WithLabelValues(dl, rc))
	}
	if testutil.ToFloat64(m.Requests.WithLabelValues(dl, "udp")) != 1 || responses != 0 || testutil.ToFloat64(m.Inflight.WithLabelValues(dl)) != 0 {
		t.Fatalf("dropped exchange: requests %v, responses %v, inflight %v",
			testutil.ToFloat64(m.Requests.WithLabelValues(dl, "udp")), responses, testutil.ToFloat64(m.Inflight.WithLabelValues(dl)))
	}

	// Replacing the set deletes every series of the dropped servers.
	if err := s.CloseReplacedBy(nil); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(m.Requests); n != 1 { // only the dropping server's set remains
		t.Fatalf("requests series after the reload: %d", n)
	}
	if n := testutil.CollectAndCount(m.Responses); n != 4 { // the remaining server's pre-resolved rcodes
		t.Fatalf("responses series after the reload: %d", n)
	}
	if n := testutil.CollectAndCount(m.Inflight); n != 1 {
		t.Fatalf("inflight series after the reload: %d", n)
	}
}
