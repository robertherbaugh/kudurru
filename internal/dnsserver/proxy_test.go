package dnsserver

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

var (
	proxySecret = []byte(strings.Repeat("fedcba9876543210", 4))
	realClient  = netip.MustParseAddrPort("203.0.113.7:5555")
)

// ppHeader builds the header dnsdist would send for realClient.
func ppHeader(t *testing.T, transport proxyproto.Transport, dst string, tlvs []byte) []byte {
	t.Helper()
	h := proxyproto.Header{
		Command: proxyproto.CommandProxy, Family: proxyproto.FamilyINET, Transport: transport,
		Source: realClient, Destination: netip.MustParseAddrPort(dst), RawTLVs: tlvs,
	}
	b, err := h.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func secretTLV(v []byte) []byte { return proxyproto.AppendTLV(nil, proxyproto.TLVTypeSecret, v) }

func mustPolicy(t *testing.T, from []string, secret []byte) *proxyproto.Policy {
	t.Helper()
	p, err := proxyproto.NewPolicy(from, secret)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// udpFrom sends payload to server from a socket bound to src (an address
// on lo) and waits up to wait for a reply.
func udpFrom(t *testing.T, src, server string, payload []byte, wait time.Duration) (*dns.Msg, error) {
	t.Helper()
	laddr, _ := net.ResolveUDPAddr("udp", src+":0")
	raddr, _ := net.ResolveUDPAddr("udp", server)
	c, err := net.DialUDP("udp", laddr, raddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	m := new(dns.Msg)
	return m, m.Unpack(buf[:n])
}

// tcpFrom opens a TCP connection from src, writes prefix (a PROXY header or
// nothing) and one length-prefixed query, and reads one response.
func tcpFrom(t *testing.T, src, server string, prefix, wire []byte, wrapTLS *tls.Config) (*dns.Msg, error) {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 2 * time.Second}
	raw, err := d.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := raw.Write(prefix); err != nil {
		return nil, err
	}
	c := raw
	if wrapTLS != nil {
		tc := tls.Client(raw, wrapTLS)
		if err := tc.Handshake(); err != nil {
			return nil, err
		}
		c = tc
	}
	if _, err := c.Write(append(lenPrefix(wire), wire...)); err != nil {
		return nil, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	m := new(dns.Msg)
	return m, m.Unpack(buf)
}

// TestProxyProtocolEndToEnd runs the forgery matrix against real UDP, TCP
// and DoT sockets: 127.0.0.2 plays a non-loopback dnsdist (it is not one of
// the two exempt loopback addresses), 127.0.0.3 a neighbour that is not in
// proxyProtocolFrom.
func TestProxyProtocolEndToEnd(t *testing.T) {
	policy := mustPolicy(t, []string{"127.0.0.2/32"}, proxySecret)
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoUDP}, {Protocol: ProtoTCP}, {Protocol: ProtoDoT}}, proxy: policy})
	if f.reg.Proxy() != policy {
		t.Fatal("Registry.Proxy must expose the policy for metrics")
	}
	udpAddr, tcpAddr, dotAddr := f.addr(ProtoUDP), f.addr(ProtoTCP), f.addr(ProtoDoT)
	wire, _ := query("example.com").Pack()
	wrong := bytes.Clone(proxySecret)
	wrong[0] ^= 0xFF
	malformed := ppHeader(t, proxyproto.TransportDatagram, udpAddr, secretTLV(proxySecret))
	malformed[13] = 0x52 // address family 5

	type tc struct {
		name   string
		src    string
		header []byte
		drop   proxyproto.Reason // "" = honoured
	}
	cases := func(transport proxyproto.Transport, dst string) []tc {
		return []tc{
			{"valid header from allowed peer", "127.0.0.2", ppHeader(t, transport, dst, secretTLV(proxySecret)), ""},
			{"wrong TLV", "127.0.0.2", ppHeader(t, transport, dst, secretTLV(wrong)), proxyproto.ReasonUnauthenticated},
			{"missing TLV", "127.0.0.2", ppHeader(t, transport, dst, nil), proxyproto.ReasonUnauthenticated},
			{"disallowed peer", "127.0.0.3", ppHeader(t, transport, dst, secretTLV(proxySecret)), proxyproto.ReasonUntrustedPeer},
			{"unlisted 127.0.0.1", "127.0.0.1", ppHeader(t, transport, dst, nil), proxyproto.ReasonUntrustedPeer},
			{"malformed header", "127.0.0.2", malformed, proxyproto.ReasonMalformed},
		}
	}
	check := func(t *testing.T, c tc, resp *dns.Msg, err error, before uint64) {
		t.Helper()
		if c.drop == "" {
			if err != nil {
				t.Fatalf("honoured header got no answer: %v", err)
			}
			checkA(t, resp)
			client, _ := f.rec.last(t)
			if client.Addr != realClient || client.ProxiedFrom != netip.MustParseAddr(c.src) {
				t.Fatalf("pipeline saw client %s via %s, want %s via %s", client.Addr, client.ProxiedFrom, realClient, c.src)
			}
			return
		}
		if err == nil {
			t.Fatalf("forged header was answered: %v", resp)
		}
		if got := policy.Dropped(c.drop); got != before+1 {
			t.Fatalf("Dropped(%s) = %d, want %d", c.drop, got, before+1)
		}
	}

	for _, c := range cases(proxyproto.TransportDatagram, udpAddr) {
		t.Run("udp/"+c.name, func(t *testing.T) {
			before := policy.Dropped(c.drop)
			wait := 2 * time.Second
			if c.drop != "" {
				wait = 300 * time.Millisecond
			}
			resp, err := udpFrom(t, c.src, udpAddr, append(bytes.Clone(c.header), wire...), wait)
			check(t, c, resp, err, before)
		})
	}
	for _, c := range cases(proxyproto.TransportStream, tcpAddr) {
		t.Run("tcp/"+c.name, func(t *testing.T) {
			before := policy.Dropped(c.drop)
			resp, err := tcpFrom(t, c.src, tcpAddr, c.header, wire, nil)
			check(t, c, resp, err, before)
		})
	}
	for _, c := range cases(proxyproto.TransportStream, dotAddr) {
		t.Run("dot/"+c.name, func(t *testing.T) {
			before := policy.Dropped(c.drop)
			resp, err := tcpFrom(t, c.src, dotAddr, c.header, wire, (&tlsPool{f.cert}).client("dot"))
			check(t, c, resp, err, before)
		})
	}

	t.Run("queries without a header keep the transport peer", func(t *testing.T) {
		for _, src := range []string{"127.0.0.2", "127.0.0.3"} {
			resp, err := udpFrom(t, src, udpAddr, wire, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			checkA(t, resp)
			client, _ := f.rec.last(t)
			if client.Addr.Addr() != netip.MustParseAddr(src) || client.ProxiedFrom.IsValid() {
				t.Fatalf("plain query attributed to %s via %s", client.Addr, client.ProxiedFrom)
			}
			resp, err = tcpFrom(t, src, tcpAddr, nil, wire, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkA(t, resp)
		}
	})
	if policy.DroppedTotal() != 15 {
		t.Fatalf("DroppedTotal = %d, want 15 (5 forgeries x 3 transports)", policy.DroppedTotal())
	}
}

// TestProxyProtocolLoopbackWithoutSecret is the compose single-edge profile:
// dnsdist shares the network namespace, proxyProtocolFrom is loopback only
// and no secret is configured, so headers from 127.0.0.1 are honoured
// without the TLV (D56).
func TestProxyProtocolLoopbackWithoutSecret(t *testing.T) {
	policy := mustPolicy(t, []string{"127.0.0.1/32", "::1/128"}, nil)
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoUDP}, {Protocol: ProtoTCP}}, proxy: policy})
	wire, _ := query("example.com").Pack()

	resp, err := udpFrom(t, "127.0.0.1", f.addr(ProtoUDP), append(ppHeader(t, proxyproto.TransportDatagram, f.addr(ProtoUDP), nil), wire...), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	checkA(t, resp)
	client, _ := f.rec.last(t)
	if client.Addr != realClient || client.ProxiedFrom != netip.MustParseAddr("127.0.0.1") || client.Proto != pipeline.ProtoUDP {
		t.Fatalf("pipeline saw %+v", client)
	}
	resp, err = tcpFrom(t, "127.0.0.1", f.addr(ProtoTCP), ppHeader(t, proxyproto.TransportStream, f.addr(ProtoTCP), nil), wire, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkA(t, resp)
	if client, _ = f.rec.last(t); client.Addr != realClient || client.Proto != pipeline.ProtoTCP {
		t.Fatalf("pipeline saw %+v", client)
	}

	// 127.0.0.2 is on lo but is neither listed nor exempt.
	if _, err := udpFrom(t, "127.0.0.2", f.addr(ProtoUDP), append(ppHeader(t, proxyproto.TransportDatagram, f.addr(ProtoUDP), nil), wire...), 300*time.Millisecond); err == nil {
		t.Fatal("header from an unlisted loopback alias must be dropped")
	}
	if policy.Dropped(proxyproto.ReasonUntrustedPeer) != 1 {
		t.Fatal("drop not counted")
	}
}

func TestProxyProtocolDisabledDoesNotInspect(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoUDP}}})
	wire, _ := query("example.com").Pack()
	// With no policy the header bytes reach miekg as a garbage message
	// (opcode 1, 13 questions): miekg refuses it before the pipeline and
	// the forged address is never used.
	resp, err := udpFrom(t, "127.0.0.1", f.addr(ProtoUDP), append(ppHeader(t, proxyproto.TransportDatagram, f.addr(ProtoUDP), nil), wire...), 300*time.Millisecond)
	var ne net.Error
	switch {
	case err == nil && (resp.Rcode == dns.RcodeSuccess || len(resp.Answer) > 0):
		t.Fatalf("a PROXY header on a non-PROXY listener was answered: %v", resp)
	case err != nil && !errors.As(err, &ne):
		t.Fatalf("unexpected error: %v", err)
	}
	if f.rec.count() != 0 {
		t.Fatal("garbage reached the pipeline")
	}
}
