package upstream

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// fakeServer answers A queries with ip over UDP on loopback, or fails as
// told.
type fakeServer struct {
	addr  string
	ip    net.IP
	mode  atomic.Int32 // 0 answer, 1 drop, 2 servfail
	calls atomic.Int64
}

func startFake(t *testing.T, last byte) *fakeServer {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{addr: pc.LocalAddr().String(), ip: net.IPv4(192, 0, 2, last)}
	started := make(chan struct{})
	srv := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		f.calls.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)
		switch f.mode.Load() {
		case 1:
			return
		case 2:
			m.Rcode = dns.RcodeServerFailure
			m.SetEdns0(1232, false)
			m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeDNSBogus})
		default:
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: f.ip}}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return f
}

func request(name string, edns bool) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.Id = 4242
	if edns {
		m.SetEdns0(4096, false)
	}
	return &pipeline.Request{
		Ctx:      context.Background(),
		Msg:      m,
		Client:   pipeline.Client{Addr: netip.MustParseAddrPort("203.0.113.5:5353")},
		Deadline: time.Now().Add(3 * time.Second),
	}
}

func newStage(t *testing.T, cfg upstream.Config) *Stage {
	t.Helper()
	set, err := upstream.New(cfg, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := NewStage(set)
	t.Cleanup(func() { _ = s.Set().Close() })
	return s
}

func TestServeRoutes(t *testing.T) {
	def, ad, lab := startFake(t, 1), startFake(t, 2), startFake(t, 3)
	s := newStage(t, upstream.Config{
		Groups: map[string]upstream.GroupConfig{
			"default": {Servers: []string{def.addr}},
			"ad":      {Servers: []string{"dc1=" + ad.addr}, Strategy: upstream.Strict},
			"lab":     {Servers: []string{lab.addr}},
		},
		Conditional: map[string]string{"corp.example.": "ad"},
	})
	if s.Name() != "upstream" {
		t.Fatal(s.Name())
	}
	for _, tc := range []struct {
		name, hint string
		ip         net.IP
		upstream   string
		cacheable  bool
	}{
		{"www.example.com", "", def.ip, "default/" + def.addr, true},
		{"host.corp.example", "", ad.ip, "ad/dc1", false},
		{"www.example.com", "lab", lab.ip, "lab/" + lab.addr, false},
		{"host.corp.example", "lab", lab.ip, "lab/" + lab.addr, false},
	} {
		req := request(tc.name, true)
		req.UpstreamGroup = tc.hint
		resp, err := s.Serve(req, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != pipeline.StatusForwarded || resp.Upstream != tc.upstream || resp.Cacheable != tc.cacheable {
			t.Errorf("%s via %q: status %v upstream %q cacheable %v", tc.name, tc.hint, resp.Status, resp.Upstream, resp.Cacheable)
		}
		a, ok := resp.Msg.Answer[0].(*dns.A)
		if !ok || !a.A.Equal(tc.ip) {
			t.Errorf("%s: answer %v", tc.name, resp.Msg.Answer)
		}
		if resp.Msg.Id != 4242 || resp.Msg.IsEdns0() == nil || resp.Msg.IsEdns0().UDPSize() != upstream.EDNSBufferSize {
			t.Errorf("%s: reply not adapted to the client: id %d opt %v", tc.name, resp.Msg.Id, resp.Msg.IsEdns0())
		}
	}

	// Without EDNS from the client, no OPT goes back.
	resp, _ := s.Serve(request("www.example.com", false), nil)
	if resp.Msg.IsEdns0() != nil {
		t.Fatal("OPT returned to a client that sent none")
	}

	// An unknown hint never falls back to the public resolvers.
	req := request("secret.corp.example", true)
	req.UpstreamGroup = "missing"
	before := def.calls.Load()
	resp, _ = s.Serve(req, nil)
	if resp.Msg.Rcode != dns.RcodeServerFailure || resp.Status != pipeline.StatusServfailUpstream || resp.Upstream != "missing" || def.calls.Load() != before {
		t.Fatalf("unknown hint: rcode %d status %v upstream %q", resp.Msg.Rcode, resp.Status, resp.Upstream)
	}
}

func TestServeFailures(t *testing.T) {
	f := startFake(t, 9)
	s := newStage(t, upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{f.addr}, Timeout: 100 * time.Millisecond}}})

	f.mode.Store(1)
	resp, err := s.Serve(request("example.com", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != pipeline.StatusTimeoutUpstream || resp.Msg.Rcode != dns.RcodeServerFailure || resp.Cacheable || resp.Msg.Id != 4242 {
		t.Fatalf("timeout: %v rcode %d", resp.Status, resp.Msg.Rcode)
	}
	if ede := edeCode(resp.Msg); ede != dns.ExtendedErrorCodeNoReachableAuthority {
		t.Fatalf("timeout EDE %d", ede)
	}

	f.mode.Store(2)
	resp, _ = s.Serve(request("example.com", true), nil)
	if resp.Status != pipeline.StatusServfailUpstream || resp.Msg.Rcode != dns.RcodeServerFailure || resp.Upstream != "default/"+f.addr {
		t.Fatalf("servfail: %v %q", resp.Status, resp.Upstream)
	}
	if ede := edeCode(resp.Msg); ede != dns.ExtendedErrorCodeDNSBogus {
		t.Fatalf("the upstream's EDE should be relayed, got %d", ede)
	}

	// Unreachable server (nothing listens): network error.
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	dead := pc.LocalAddr().String()
	_ = pc.Close()
	s2 := newStage(t, upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{dead}, Timeout: 200 * time.Millisecond}}})
	resp, _ = s2.Serve(request("example.com", true), nil)
	if resp.Msg.Rcode != dns.RcodeServerFailure || (resp.Status != pipeline.StatusServfailUpstream && resp.Status != pipeline.StatusTimeoutUpstream) {
		t.Fatalf("dead server: %v", resp.Status)
	}
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

func TestSwapAndCompat(t *testing.T) {
	a, b := startFake(t, 21), startFake(t, 22)
	s, err := New("default", []string{a.addr}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Servers) != 1 || s.Group != "default" {
		t.Fatalf("compat fields: %+v", s)
	}
	resp, _ := s.Serve(request("example.com", false), nil)
	if resp.Status != pipeline.StatusForwarded || !resp.Cacheable {
		t.Fatalf("compat: %v", resp.Status)
	}
	next, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{b.addr}}}}, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	prev := s.Swap(next)
	if err := prev.CloseReplacedBy(next); err != nil {
		t.Fatal(err)
	}
	resp, _ = s.Serve(request("example.com", false), nil)
	if resp.Upstream != "default/"+b.addr {
		t.Fatalf("after swap: %q", resp.Upstream)
	}
	_ = next.Close()
	if p := s.ECSSubnet(request("x", false).Msg, netip.MustParseAddr("203.0.113.1")); p.IsValid() {
		t.Fatalf("default ECS mode must strip, got %v", p)
	}

	if _, err := New("other", []string{a.addr}, 0); err == nil {
		t.Fatal("compat constructor only builds the default group")
	}
	if _, err := New("default", nil, 0); err == nil {
		t.Fatal("no servers must fail")
	}
}
