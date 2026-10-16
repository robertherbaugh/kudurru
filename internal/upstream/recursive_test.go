package upstream

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/recursor"
)

// startRoot runs a "root server" on loopback that is authoritative for
// every name: enough for a recursive group to resolve in one step.
func startRoot(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		q := r.Question[0]
		if q.Qtype == dns.TypeA && strings.EqualFold(q.Name, "www.example.com.") {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 7)}}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

func TestRecursiveGroup(t *testing.T) {
	root := startRoot(t)
	rc := recursor.DefaultConfig()
	rc.DNSSEC, rc.IPv6, rc.RootHints = false, recursor.IPv6Off, []string{root}
	rec, err := recursor.New(rc, recursor.Options{AllowLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	s := newSet(t, Config{Groups: map[string]GroupConfig{DefaultGroup: {Recursive: true}}}, Options{Recursor: rec})
	if s.Recursor() != rec {
		t.Fatal("the Set must use the caller's resolver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := s.Exchange(ctx, DefaultGroup, s.PrepareQuery(query("www.example.com", dns.TypeA), netip.Addr{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Upstream != "default/recursive" || len(res.Msg.Answer) != 1 || !res.Msg.RecursionAvailable || res.DNSSEC != recursor.SecUnknown {
		t.Fatalf("%q %v sec %s", res.Upstream, res.Msg, res.DNSSEC)
	}
	st := s.Status()
	if len(st) != 1 || st[0].Server != RecursiveServer || !st[0].Healthy {
		t.Fatalf("status %+v", st)
	}
	// Closing the Set leaves the caller's resolver alone.
	_ = s.Close()
	if _, err := rec.Exchange(ctx, query("www.example.com", dns.TypeA)); err != nil {
		t.Fatal(err)
	}
}

func TestRecursiveGroupValidation(t *testing.T) {
	for _, tc := range []struct {
		cfg Config
		ok  bool
	}{
		{Config{Groups: map[string]GroupConfig{DefaultGroup: {Recursive: true}}}, true},
		{Config{Groups: map[string]GroupConfig{DefaultGroup: {Recursive: true, Servers: []string{"1.1.1.1"}}}}, false},
		{Config{Groups: map[string]GroupConfig{DefaultGroup: {}}}, false},
		{Config{Groups: map[string]GroupConfig{DefaultGroup: {Recursive: true}}, Recursion: recursor.Config{IPv6: "sometimes"}}, false},
		{Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"1.1.1.1"}}}, Recursion: recursor.Config{IPv6: "sometimes"}}, true}, // unused
	} {
		if err := tc.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.cfg, err)
		}
	}
	// Without a caller's resolver the Set builds and owns one.
	s := newSet(t, Config{Groups: map[string]GroupConfig{DefaultGroup: {Recursive: true}}, Recursion: recursor.DefaultConfig()}, Options{})
	if s.Recursor() == nil || !s.ownRec {
		t.Fatal("no resolver built")
	}
	if g, _ := s.Group(DefaultGroup); g.timeout != DefaultRecursiveTimeout {
		t.Fatalf("timeout %s", g.timeout)
	}
}

func TestConfigFromRecursion(t *testing.T) {
	c := ConfigFrom(config.Upstreams{
		Groups:    map[string]config.UpstreamGroup{"default": {Mode: "recursive"}, "lan": {Servers: []string{"192.168.1.1"}}},
		Recursion: config.Recursion{DNSSEC: true, QNAMEMinimization: false, IPv6: "off", TrustAnchors: []string{"x"}},
	})
	if !c.Groups["default"].Recursive || c.Groups["lan"].Recursive {
		t.Fatalf("%+v", c.Groups)
	}
	r := c.Recursion
	if !r.DNSSEC || r.QNAMEMinimization || r.IPv6 != recursor.IPv6Off || len(r.TrustAnchors) != 1 || !r.CaseRandomization {
		t.Fatalf("%+v", r)
	}
}
