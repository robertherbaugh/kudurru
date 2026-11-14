package main

import (
	"context"
	"crypto"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

// These are actual numeric sockets, independently of the unmounted updater.
// The function runs once per server; cleanup closes and joins every socket.
func anchorWireServer(t *testing.T, network string, reply func([]byte) []byte) (netip.AddrPort, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	done := make(chan struct{})
	if network == "udp" {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		go func() {
			defer close(done)
			packet := make([]byte, 65535)
			n, address, err := conn.ReadFromUDP(packet)
			if err != nil {
				return
			}
			reads.Add(1)
			if response := reply(packet[:n]); response != nil {
				_, _ = conn.WriteToUDP(response, address)
			}
		}()
		t.Cleanup(func() { _ = conn.Close(); <-done })
		return conn.LocalAddr().(*net.UDPAddr).AddrPort(), &reads
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		packet := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, packet); err != nil {
			return
		}
		reads.Add(1)
		if response := reply(packet); response != nil {
			responseSize := len(response)
			if responseSize > anchorWireLimit {
				t.Error("fixture response exceeds one DNS frame")
				return
			}
			binary.BigEndian.PutUint16(size[:], uint16(responseSize))
			_, _ = conn.Write(append(size[:], response...))
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return listener.Addr().(*net.TCPAddr).AddrPort(), &reads
}

func anchorWireSignedReply(t *testing.T) (trustanchors.Seed, func([]byte) []byte) {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 60}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	inception, expiration := time.Now().Add(-time.Minute).Unix(), time.Now().Add(time.Hour).Unix()
	if inception < 0 || inception > math.MaxUint32 || expiration < 0 || expiration > math.MaxUint32 {
		t.Fatal("fixture clock outside DNS signature time range")
		return trustanchors.Seed{}, nil
	}
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, TypeCovered: dns.TypeDNSKEY, Algorithm: key.Algorithm, Labels: 0, OrigTtl: 60, Inception: uint32(inception), Expiration: uint32(expiration), KeyTag: key.KeyTag(), SignerName: "."}
	if err := sig.Sign(private.(crypto.Signer), []dns.RR{key}); err != nil {
		t.Fatal(err)
	}
	seed := trustanchors.Seed{ClusterID: "wire-cluster", PolicyID: "wire-policy", DS: []string{key.ToDS(dns.SHA256).String()}}
	return seed, func(raw []byte) []byte {
		request := new(dns.Msg)
		if request.Unpack(raw) != nil || request.RecursionDesired || len(request.Question) != 1 || request.Question[0] != (dns.Question{Name: ".", Qtype: dns.TypeDNSKEY, Qclass: dns.ClassINET}) || request.IsEdns0() == nil || !request.IsEdns0().Do() {
			t.Error("wire request changed original root DNSKEY acquisition")
			return nil
		}
		response := new(dns.Msg)
		response.SetReply(request)
		response.Authoritative = true
		response.Answer = []dns.RR{dns.Copy(key), dns.Copy(sig)}
		packet, err := response.Pack()
		if err != nil {
			t.Error(err)
		}
		return packet
	}
}

func TestAnchorWireSignedNumericUDPAndTCP(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			seed, reply := anchorWireSignedReply(t)
			endpoint, reads := anchorWireServer(t, network, reply)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			plan, err := captureAnchorWirePlan(ctx, []netip.AddrPort{endpoint}, network, 0)
			if err != nil {
				t.Fatal(err)
			}
			clock, err := trustanchors.NewSessionClock()
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			packet, received, err := acquireAnchorWire(plan, func() (trustanchors.Sample, error) { calls++; return clock.Sample(), nil })
			if err != nil || calls != 1 || reads.Load() != 1 {
				t.Fatal("original signed socket acquisition failed", calls, reads.Load(), err)
			}
			message := new(dns.Msg)
			if err := message.Unpack(packet); err != nil {
				t.Fatal(err)
			}
			state, observation, err := trustanchors.Bootstrap(seed, message, received)
			if err != nil {
				t.Fatal("actual wire proof did not authenticate at original receipt", err)
			}
			if err := observation.CheckBootstrap(state, clock.Sample()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := acquireAnchorWire(plan, func() (int, error) { calls++; return 1, nil }); err == nil || calls != 1 {
				t.Fatal("acquisition plan replayed or sampled a second receipt")
			}
		})
	}
}

func TestAnchorWireInvalidReceiptIsStampedAndTerminal(t *testing.T) {
	for _, kind := range []string{"id", "question", "truncated", "empty", "sampler-refusal"} {
		t.Run(kind, func(t *testing.T) {
			_, signed := anchorWireSignedReply(t)
			first, reads := anchorWireServer(t, "tcp", func(raw []byte) []byte {
				packet := signed(raw)
				switch kind {
				case "id":
					packet[0] ^= 1
				case "question":
					binary.BigEndian.PutUint16(packet[13:15], dns.TypeA)
				case "truncated":
					packet[2] |= 2
				case "empty":
					return []byte{}
				}
				return packet
			})
			second, laterReads := anchorWireServer(t, "tcp", signed)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			plan, err := captureAnchorWirePlan(ctx, []netip.AddrPort{first, second}, "tcp", 0)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			sentinel := errors.New("original sampler refused")
			_, _, err = acquireAnchorWire(plan, func() (int, error) {
				calls++
				if kind == "sampler-refusal" {
					return 0, sentinel
				}
				return 1, nil
			})
			if err == nil || calls != 1 || reads.Load() != 1 || laterReads.Load() != 0 {
				t.Fatal("invalid completed receipt was unstamped, replaced or accepted", calls, err)
			}
			if kind == "sampler-refusal" && !errors.Is(err, sentinel) {
				t.Fatal("original sampler error replaced", err)
			}
		})
	}
}

func TestAnchorWireOriginalCancellationJoinsSocket(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			request := make(chan struct{})
			release := make(chan struct{})
			endpoint, _ := anchorWireServer(t, network, func([]byte) []byte { close(request); <-release; return nil })
			defer close(release)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			plan, err := captureAnchorWirePlan(ctx, []netip.AddrPort{endpoint}, network, 0)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			var stamps atomic.Int32
			go func() {
				_, _, err := acquireAnchorWire(plan, func() (int, error) { stamps.Add(1); return 1, nil })
				done <- err
			}()
			select {
			case <-request:
			case <-ctx.Done():
				t.Fatal("original socket request never arrived")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || stamps.Load() != 0 {
					t.Fatal("canceled acquisition produced a receipt", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not close and join original socket")
			}
		})
	}
}

func TestAnchorWireIncompleteTransportUsesOnlyCapturedEndpoints(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		name := "next-selected"
		if exhausted {
			name = "three-exchange-bound"
		}
		t.Run(name, func(t *testing.T) {
			_, reply := anchorWireSignedReply(t)
			var endpoints []netip.AddrPort
			count := 1
			if exhausted {
				count = 3
			}
			for range count {
				endpoint, _ := anchorWireServer(t, "tcp", func([]byte) []byte { return nil })
				endpoints = append(endpoints, endpoint)
			}
			valid, reads := anchorWireServer(t, "tcp", reply)
			endpoints = append(endpoints, valid)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			plan, err := captureAnchorWirePlan(ctx, endpoints, "tcp", 0)
			if err != nil {
				t.Fatal(err)
			}
			stamps := 0
			_, _, err = acquireAnchorWire(plan, func() (int, error) { stamps++; return 1, nil })
			if exhausted {
				if err == nil || stamps != 0 || reads.Load() != 0 {
					t.Fatal("acquisition exceeded the original three-exchange plan", err)
				}
			} else if err != nil || stamps != 1 || reads.Load() != 1 {
				t.Fatal("incomplete acquisition did not use next original endpoint", err)
			}
		})
	}
}

func TestAnchorWirePlanBoundsAndOriginalSelection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	endpoints := recursor.DefaultRootHints()
	plan, err := captureAnchorWirePlan(ctx, endpoints, "tcp", uint64(len(endpoints))-1)
	if err != nil || len(plan.selected) != 3 || plan.selected[0] != endpoints[len(endpoints)-1] || plan.selected[1] != endpoints[0] {
		t.Fatal("captured endpoint rotation/bound changed", err)
	}
	original := plan.selected[1]
	endpoints[0] = netip.AddrPort{}
	if plan.selected[1] != original || plan.original != ctx {
		t.Fatal("plan borrowed endpoint storage or substituted original context")
	}
	valid := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:53")}
	long, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	for _, bad := range []context.Context{nil, context.Background(), long} {
		if _, err := captureAnchorWirePlan(bad, valid, "tcp", 0); err == nil {
			t.Fatal("missing/long original lifetime admitted")
		}
	}
	for _, bad := range [][]netip.AddrPort{nil, make([]netip.AddrPort, 33), {valid[0], valid[0]}, {netip.MustParseAddrPort("0.0.0.0:53")}, {netip.MustParseAddrPort("224.0.0.1:53")}, {netip.MustParseAddrPort("127.0.0.1:0")}, {netip.MustParseAddrPort("[fe80::1%eth0]:53")}} {
		if _, err := captureAnchorWirePlan(ctx, bad, "tcp", 0); err == nil {
			t.Fatal("invalid or unbounded endpoint plan admitted")
		}
	}
	if _, err := captureAnchorWirePlan(ctx, valid, "unix", 0); err == nil {
		t.Fatal("non-DNS transport admitted")
	}
}
