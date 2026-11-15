package stub

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rr(line string) dns.RR {
	r, err := dns.NewRR(line)
	if err != nil {
		panic(err)
	}
	return r
}
func config(endpoint string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "stable"}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypeStub, ZoneName: "example.", Stub: &v1alpha1.StubSpec{Nameservers: []string{endpoint}}}}
}
func delegation(now time.Time) *zone.StubData {
	return &zone.StubData{Records: []dns.RR{rr("example. 300 IN SOA ns.example. admin.example. 4294967295 10 3 30 10"), rr("example. 30 IN NS ns.example."), rr("ns.example. 20 IN A 127.0.0.1")}, Servers: []zone.StubServer{{Name: "ns.example.", Address: netip.MustParseAddrPort("127.0.0.1:53"), Protocol: "udp"}}, ConfirmedAt: now, ExpiresAt: now.Add(20 * time.Second)}
}

// Both genuine DNS sockets share a port, matching the real authority model.
func primary(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	for _, server := range []*dns.Server{{Listener: tcp, Net: "tcp", Handler: handler}, {PacketConn: udp, Net: "udp", Handler: handler}} {
		ready := make(chan struct{})
		done := make(chan error, 1)
		server.NotifyStartedFunc = func() { close(ready) }
		go func() { done <- server.ActivateAndServe() }()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("primary start: %v", err)
		}
		t.Cleanup(func() {
			_ = server.Shutdown()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	return tcp.Addr().String()
}

func responder(w dns.ResponseWriter, q *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	switch q.Question[0].Qtype {
	case dns.TypeSOA:
		m.Answer = []dns.RR{rr("example. 300 IN SOA ns.example. admin.example. 4294967295 10 3 30 10")}
	case dns.TypeNS:
		m.Answer = []dns.RR{rr("example. 30 IN NS ns.example.")}
		m.Extra = []dns.RR{rr("ns.example. 20 IN A 127.0.0.1")}
	default:
		if q.Question[0].Name == "www.example." {
			m.Answer = []dns.RR{rr("www.example. 42 IN A 192.0.2.80"), rr("www.example. 42 IN RRSIG A 13 2 42 20270101000000 20260101000000 1234 example. AQID")}
			m.AuthenticatedData = true
		} else {
			m.Ns = []dns.RR{rr("child.example. 60 IN NS ns.child.example.")}
			m.Extra = []dns.RR{rr("ns.child.example. 60 IN A 192.0.2.44")}
		}
	}
	_ = w.WriteMsg(m)
}

func TestDiscoveryAndCapturedExchangeOverRealUDPAndTCP(t *testing.T) {
	var udp, tcp atomic.Int32
	addr := primary(t, func(w dns.ResponseWriter, q *dns.Msg) {
		if q.RecursionDesired {
			t.Error("authority received RD=1")
		}
		if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
			udp.Add(1)
		} else {
			tcp.Add(1)
		}
		responder(w, q)
	})
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, protocol := range []string{"udp", "tcp"} {
		t.Run(protocol, func(t *testing.T) {
			c := &Client{Now: func() time.Time { return now }}
			dz := config(protocol + "://" + addr)
			data, err := c.Discover(t.Context(), dz)
			if err != nil {
				t.Fatal(err)
			}
			if len(data.Records) != 3 || len(data.Servers) != 1 || data.Servers[0].Address.String() != addr || data.Servers[0].Protocol != protocol || !data.ExpiresAt.Equal(now.Add(20*time.Second)) {
				t.Fatalf("discovery %+v", data)
			}
			z, err := zone.BuildStub(dz, data)
			if err != nil {
				t.Fatal(err)
			}
			// A query may never use the resolver, even for advertised hostnames.
			c.Resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				t.Fatal("query resolved hostname")
				return nil, nil
			})
			q := new(dns.Msg)
			q.SetQuestion("www.example.", dns.TypeA)
			q.SetEdns0(1232, true)
			answer, err := c.Exchange(t.Context(), z, q)
			if err != nil || answer.AuthenticatedData || len(answer.Answer) != 2 {
				t.Fatalf("raw answer %+v %v", answer, err)
			}
			q.SetQuestion("www.child.example.", dns.TypeA)
			referral, err := c.Exchange(t.Context(), z, q)
			if err != nil || len(referral.Ns) != 1 || len(referral.Extra) != 1 {
				t.Fatalf("child referral %+v %v", referral, err)
			}
		})
	}
	if udp.Load() != 5 || tcp.Load() != 5 {
		t.Fatalf("unexpected discovery/query count udp=%d tcp=%d", udp.Load(), tcp.Load())
	}
}

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func TestDiscoveryIgnoresExternalGlueAndBoundsHostResolution(t *testing.T) {
	addr := primary(t, func(w dns.ResponseWriter, q *dns.Msg) {
		if q.Question[0].Qtype != dns.TypeNS {
			responder(w, q)
			return
		}
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		m.Answer = []dns.RR{rr("example. 600 IN NS ns.external.")}
		m.Extra = []dns.RR{rr("ns.external. 600 IN A 192.0.2.99")}
		_ = w.WriteMsg(m)
	})
	now := time.Now()
	var calls atomic.Int32
	c := &Client{Now: func() time.Time { return now }, Resolver: resolverFunc(func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host != "ns.external" {
			t.Errorf("host %s", host)
		}
		calls.Add(1)
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})}
	data, err := c.Discover(t.Context(), config(addr))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(data.Records) != 2 || data.Servers[0].Address.Addr() != netip.MustParseAddr("127.0.0.1") || !data.ExpiresAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("external glue trusted: %+v", data)
	}
	c.Resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return make([]netip.Addr, 33), nil })
	if _, err := c.Discover(t.Context(), config(addr)); err == nil {
		t.Fatal("unbounded host resolution")
	}
}

func TestUDPTruncationRetriesTCPOnlyWithMatchingQuestion(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			var tcp atomic.Int32
			addr := primary(t, func(w dns.ResponseWriter, q *dns.Msg) {
				if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
					m := new(dns.Msg)
					m.SetReply(q)
					m.Truncated = true
					if wrong {
						m.Question[0].Qtype = dns.TypeTXT
					}
					_ = w.WriteMsg(m)
				} else {
					tcp.Add(1)
					responder(w, q)
				}
			})
			c := &Client{}
			_, err := c.Discover(t.Context(), config(addr))
			if wrong {
				if err == nil || tcp.Load() != 0 {
					t.Fatalf("wrong question triggered TCP: %v %d", err, tcp.Load())
				}
			} else if err != nil || tcp.Load() != 3 {
				t.Fatalf("TC fallback: %v %d", err, tcp.Load())
			}
		})
	}
}

func TestDiscoveryRejectsIncoherentSOAAndNonAuthoritativeNS(t *testing.T) {
	for _, bad := range []string{"serial", "authority", "out-of-zone", "no-ns", "ttl-zero"} {
		t.Run(bad, func(t *testing.T) {
			var soa atomic.Int32
			addr := primary(t, func(w dns.ResponseWriter, q *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(q)
				m.Authoritative = true
				if q.Question[0].Qtype == dns.TypeSOA {
					serial := uint32(1)
					if bad == "serial" && soa.Add(1) > 1 {
						serial = 2
					}
					m.Answer = []dns.RR{rr(fmt.Sprintf("example. 300 IN SOA ns.example. admin.example. %d 10 3 30 10", serial))}
				} else {
					m.Answer = []dns.RR{rr("example. 30 IN NS ns.example.")}
					m.Extra = []dns.RR{rr("ns.example. 20 IN A 127.0.0.1")}
					switch bad {
					case "authority":
						m.Authoritative = false
					case "out-of-zone":
						m.Answer[0].Header().Name = "other."
					case "no-ns":
						m.Answer = nil
					case "ttl-zero":
						m.Extra[0].Header().Ttl = 0
					}
				}
				_ = w.WriteMsg(m)
			})
			if _, err := (&Client{}).Discover(t.Context(), config(addr)); err == nil {
				t.Fatal("accepted incoherent discovery")
			}
		})
	}
}

func TestStrictReplyFramingAndBounds(t *testing.T) {
	q := discoveryQuestion("example.", dns.TypeNS)
	valid := new(dns.Msg)
	valid.SetReply(q)
	valid.Answer = []dns.RR{rr("example. 30 IN NS ns.example.")}
	base, err := valid.Pack()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"trailing", func(b []byte) []byte { return append(b, 0) }},
		{"short", func(b []byte) []byte { return b[:len(b)-1] }},
		{"question-count", func(b []byte) []byte { binary.BigEndian.PutUint16(b[4:], 0); return b }},
		{"rr-count", func(b []byte) []byte { binary.BigEndian.PutUint16(b[6:], 257); return b }},
		{"id", func(b []byte) []byte { b[0] ^= 1; return b }},
		{"qr", func(b []byte) []byte { b[2] &= 0x7f; return b }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := unpackReply(test.change(append([]byte(nil), base...)), q, false); err == nil {
				t.Fatal("accepted malformed wire")
			}
		})
	}
	// HIP carries a list of domain names. A malicious compressed reply
	// expands well beyond the advertised byte budget while using a tiny frame.
	longName := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + "."
	largeQ := discoveryQuestion(longName, dns.TypeHIP)
	huge := new(dns.Msg)
	huge.SetReply(largeQ)
	wire, err := huge.Pack()
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(wire[6:], 32)
	for range 32 {
		// Owner pointer, HIP type/class, TTL, RDLENGTH, empty HIT/key, then
		// 40 compressed rendezvous names pointing at the original question.
		record := []byte{0xc0, 12, 0, 55, 0, 1, 0, 0, 0, 1, 0, 84, 0, 0, 0, 0}
		for range 40 {
			record = append(record, 0xc0, 12)
		}
		wire = append(wire, record...)
	}
	if _, err := unpackReply(wire, largeQ, false); err == nil || !strings.Contains(err.Error(), "expanded byte bound") {
		t.Fatalf("expanded memory bypass: %v", err)
	}

}

func TestCancellationClosesTCPReadAndDiscoveryResolver(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := (&Client{Timeout: time.Hour}).Discover(ctx, config("tcp://"+listener.Addr().String()))
		done <- err
	}()
	conn := <-accepted
	defer func() { _ = conn.Close() }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock read")
	}
	var buf [4096]byte
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = conn.Read(buf[:])
	if _, err := conn.Read(buf[:]); err == nil {
		t.Fatal("cancel did not close connection")
	}
	c := &Client{Resolver: resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() }), Timeout: 10 * time.Millisecond}
	if _, err := c.Discover(t.Context(), config("bootstrap.example")); err == nil {
		t.Fatal("resolver timeout accepted")
	}
}

func TestStubProtocolsAndDefaultPort(t *testing.T) {
	for _, v := range []string{"tls://127.0.0.1", "https://example", "tcp://127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:abc", "0.0.0.0", "[fe80::1%eth0]:53", "udp://example/path"} {
		if _, err := parseEndpoint(v); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
	for _, v := range []string{"127.0.0.1", "::1", "[::1]", "tcp://example."} {
		e, err := parseEndpoint(v)
		if err != nil || e.port != 53 {
			t.Errorf("default endpoint %s: %+v %v", v, e, err)
		}
	}
}

func TestRealPrimaryRejectsMalformedFramingAndOversizedReplies(t *testing.T) {
	for _, bad := range []string{"trailing", "missing-record", "wrong-id", "record-bound", "wrong-question"} {
		t.Run(bad, func(t *testing.T) {
			addr := primary(t, func(w dns.ResponseWriter, q *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(q)
				m.Authoritative = true
				m.Answer = []dns.RR{rr("example. 300 IN SOA ns.example. admin.example. 1 10 3 30 10")}
				if bad == "wrong-question" {
					m.Question[0].Name = "other."
				}
				wire, err := m.Pack()
				if err != nil {
					t.Error(err)
					return
				}
				switch bad {
				case "trailing":
					wire = append(wire, 0)
				case "missing-record":
					wire = wire[:len(wire)-1]
				case "wrong-id":
					wire[0] ^= 1
				case "record-bound":
					binary.BigEndian.PutUint16(wire[6:], 257)
				}
				_, _ = w.Write(wire)
			})
			for _, protocol := range []string{"udp", "tcp"} {
				if _, err := (&Client{}).Discover(t.Context(), config(protocol+"://"+addr)); err == nil {
					t.Fatalf("accepted %s malformed %s primary reply", protocol, bad)
				}
			}
		})
	}
}

func TestDiscoveryAndQueryFailOverWithinOneDeadline(t *testing.T) {
	silent := primary(t, func(dns.ResponseWriter, *dns.Msg) {})
	live := primary(t, responder)
	c := &Client{Timeout: 100 * time.Millisecond, QueryTimeout: 100 * time.Millisecond}
	dz := config(silent)
	dz.Spec.Stub.Nameservers = append(dz.Spec.Stub.Nameservers, live)
	data, err := c.Discover(t.Context(), dz)
	if err != nil {
		t.Fatalf("silent bootstrap prevented failover: %v", err)
	}
	silentAddress := netip.MustParseAddrPort(silent)
	data.Servers = append([]zone.StubServer{{Name: "ns.example.", Address: silentAddress, Protocol: "udp"}}, data.Servers...)
	z, err := zone.BuildStub(dz, data)
	if err != nil {
		t.Fatal(err)
	}
	q := new(dns.Msg)
	q.SetQuestion("www.example.", dns.TypeA)
	answer, err := c.Exchange(t.Context(), z, q)
	if err != nil || len(answer.Answer) != 2 {
		t.Fatalf("silent authority prevented failover: %v %v", answer, err)
	}
}
