package secondary

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

var transferSecret = []byte("secondary-transfer-test-secret")

const transferKey = "transfer-key.example."

func transferRing(t *testing.T, name, alg string, secret []byte) *dnsserver.StaticKeyring {
	t.Helper()
	k, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: name, Algorithm: alg, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func transferZone(signed bool) *v1alpha1.DNSZone {
	dz := &v1alpha1.DNSZone{Spec: v1alpha1.DNSZoneSpec{
		ZoneName: "example.org.", Type: v1alpha1.ZoneTypeSecondary,
		Secondary: &v1alpha1.SecondarySpec{TransferProtocol: "tcp"},
	}}
	if signed {
		dz.Spec.Secondary.TSIGKeyRef = "Transfer-Key.Example"
	}
	return dz
}

func transferRR(t *testing.T, text string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(text)
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

func transferSOA() *dns.SOA {
	return &dns.SOA{
		Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 537},
		Ns:  "Ns1.Example.org.", Mbox: "Hostmaster.Example.org.",
		Serial: 4294967294, Refresh: 61, Retry: 13, Expire: 301, Minttl: 47,
	}
}

func transferReply(q *dns.Msg, records ...dns.RR) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	m.Answer = records
	m.Compress = true
	return m
}

func transferPrimary(t *testing.T, ring dnsserver.TSIGKeyring, handler dns.HandlerFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return transferPrimaryListener(t, ring, ln, handler)
}

func transferPrimaryListener(t *testing.T, ring dnsserver.TSIGKeyring, ln net.Listener, handler dns.HandlerFunc) string {
	t.Helper()
	started, done := make(chan struct{}), make(chan error, 1)
	srv := &dns.Server{
		Listener: ln, Handler: handler, TsigProvider: dnsserver.TSIGProvider(ring),
		NotifyStartedFunc: func() { close(started) },
	}
	go func() { done <- srv.ActivateAndServe() }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("start primary: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("primary did not start")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.ShutdownContext(ctx); err != nil {
			t.Errorf("shutdown primary: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("primary: %v", err)
		}
	})
	return ln.Addr().String()
}

func runTransfer(t *testing.T, c *TransferClient, dz *v1alpha1.DNSZone, address string, soa bool) ([]dns.RR, error) {
	t.Helper()
	if soa {
		rr, err := c.SOA(context.Background(), dz, address)
		if err != nil {
			if rr != nil {
				t.Fatal("SOA returned data on error")
			}
			return nil, err
		}
		return []dns.RR{rr}, nil
	}
	rrs, err := c.AXFR(context.Background(), dz, address)
	if err != nil && rrs != nil {
		t.Fatal("AXFR returned partial data on error")
	}
	return rrs, err
}

func TestTransferClientPrimary(t *testing.T) {
	for _, algorithm := range []string{"", "hmac-sha256", "hmac-sha512"} {
		t.Run(algorithm, func(t *testing.T) {
			signed := algorithm != ""
			var ring dnsserver.TSIGKeyring
			if signed {
				ring = transferRing(t, transferKey, algorithm, transferSecret)
			}
			soa := transferSOA()
			want := []dns.RR{
				soa, transferRR(t, "example.org. 721 IN NS Ns1.Example.org."),
				transferRR(t, "Ns1.Example.org. 87 IN A 192.0.2.53"),
				transferRR(t, "example.org. 89 IN TYPE65400 \\# 4 01020304"),
				transferRR(t, "example.org. 90 IN DNSKEY 256 3 13 AQID"),
				transferRR(t, "example.org. 91 IN RRSIG DNSKEY 13 2 90 20300101000000 20200101000000 42 example.org. AQID"),
				transferRR(t, "example.org. 92 IN NSEC ns1.example.org. NS SOA RRSIG NSEC DNSKEY"),
				&dns.NULL{Hdr: dns.RR_Header{Name: "empty.example.org.", Rrtype: dns.TypeNULL, Class: dns.ClassINET, Ttl: 94}},
				&dns.APL{Hdr: dns.RR_Header{Name: "empty.example.org.", Rrtype: dns.TypeAPL, Class: dns.ClassINET, Ttl: 95}},
				&dns.RFC3597{Hdr: dns.RR_Header{Name: "empty.example.org.", Rrtype: 65401, Class: dns.ClassINET, Ttl: 96}},
			}
			address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				if signed && w.TsigStatus() != nil {
					t.Errorf("query TSIG: %v", w.TsigStatus())
					return
				}
				chunks := [][]dns.RR{{soa}}
				if q.Question[0].Qtype == dns.TypeAXFR {
					chunks = append(chunks, want[1:], []dns.RR{soa})
				}
				for i, chunk := range chunks {
					m := transferReply(q, chunk...)
					if i != 0 {
						m.Question = nil // RFC 5936 permits omitted later questions.
					}
					if sig := q.IsTsig(); sig != nil {
						m.SetTsig(sig.Hdr.Name, sig.Algorithm, 300, time.Now().Unix())
					}
					if err := w.WriteMsg(m); err != nil {
						t.Errorf("write primary: %v", err)
						return
					}
					w.TsigTimersOnly(true)
				}
			})
			client := &TransferClient{Keyring: ring}
			for _, poll := range []bool{true, false} {
				got, err := runTransfer(t, client, transferZone(signed), address, poll)
				if err != nil {
					t.Fatal(err)
				}
				expect := want
				if poll {
					expect = want[:1]
				}
				if len(got) != len(expect) {
					t.Fatalf("records = %d, want %d", len(got), len(expect))
				}
				for i := range got {
					if got[i].String() != expect[i].String() {
						t.Errorf("record %d = %s, want %s", i, got[i], expect[i])
					}
				}
			}
		})
	}
}

func TestTransferClientTSIGRejections(t *testing.T) {
	ring := transferRing(t, transferKey, "hmac-sha256", transferSecret)
	for _, test := range []struct {
		name                                                   string
		key                                                    string
		alg                                                    string
		age                                                    int64
		code                                                   uint16
		badMAC, wrongSecret, missing, missingFinal, wrongChain bool
	}{
		{name: "missing", missing: true},
		{name: "wrong MAC", badMAC: true},
		{name: "wrong secret", wrongSecret: true},
		{name: "wrong key", key: "other-key.example."},
		{name: "wrong algorithm", alg: dns.HmacSHA512},
		{name: "expired signature", age: -3600},
		{name: "future signature", age: 3600},
		{name: "BADTIME", code: dns.RcodeBadTime},
		{name: "BADSIG", code: dns.RcodeBadSig},
		{name: "BADKEY", code: dns.RcodeBadKey},
		{name: "missing final", missingFinal: true},
		{name: "unchained final", wrongChain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, alg := test.key, test.alg
			if key == "" {
				key = transferKey
			}
			if alg == "" {
				alg = dns.HmacSHA256
			}
			secret := transferSecret
			if test.wrongSecret {
				secret = []byte("different-transfer-test-secret")
			}
			serverRing := transferRing(t, key, alg, secret)
			address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				chunks := [][]dns.RR{{transferSOA()}}
				if q.Question[0].Qtype == dns.TypeAXFR {
					chunks = append(chunks, []dns.RR{transferSOA()})
				}
				previous := q.IsTsig().MAC
				for i, chunk := range chunks {
					m := transferReply(q, chunk...)
					var wire []byte
					if test.missing || (test.missingFinal && i > 0) {
						wire, _ = m.Pack()
					} else {
						m.SetTsig(key, alg, 300, time.Now().Unix()+test.age)
						m.IsTsig().Error = test.code
						if test.wrongChain && i > 0 {
							previous = ""
						}
						var err error
						wire, previous, err = dns.TsigGenerateWithProvider(m, dnsserver.TSIGProvider(serverRing), previous, i > 0)
						if err != nil {
							t.Errorf("sign: %v", err)
							return
						}
						if test.badMAC {
							wire[len(wire)-7] ^= 1 // Last MAC byte, before the six trailing TSIG bytes.
						}
					}
					if _, err := w.Write(wire); err != nil {
						return
					}
				}
			})
			for _, poll := range []bool{true, false} {
				_, err := runTransfer(t, &TransferClient{Keyring: ring}, transferZone(true), address, poll)
				wantFailure := !poll || (!test.missingFinal && !test.wrongChain)
				if (err != nil) != wantFailure {
					t.Fatalf("SOA=%v error=%v, want failure=%v", poll, err, wantFailure)
				}
				if test.code == dns.RcodeBadTime && !errors.Is(err, dns.ErrTime) {
					t.Fatalf("BADTIME error: %v", err)
				}
				if test.badMAC && !errors.Is(err, dns.ErrSig) {
					t.Fatalf("bad MAC error: %v", err)
				}
			}
		})
	}
}

// This signer computes the stream digest independently with crypto/hmac; it
// deliberately includes the exact unsigned wire bytes, not repacked records.
type transferStreamSigner struct {
	previous string
	unsigned []byte
}

func (p transferStreamSigner) Generate(buf []byte, _ *dns.TSIG) ([]byte, error) {
	mac, err := hex.DecodeString(p.previous)
	if err != nil {
		return nil, err
	}
	macSize := len(mac)
	if macSize > 65535 {
		return nil, fmt.Errorf("fixture MAC exceeds uint16 wire length")
	}
	h := hmac.New(sha256.New, transferSecret)
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(macSize))
	h.Write(size[:])
	h.Write(mac)
	h.Write(p.unsigned)
	h.Write(buf[2+len(mac):]) // Current unsigned message and TSIG timers.
	return h.Sum(nil), nil
}

func (transferStreamSigner) Verify([]byte, *dns.TSIG) error { return dns.ErrSig }

func TestTransferClientUnsignedIntermediates(t *testing.T) {
	ring := transferRing(t, transferKey, "hmac-sha256", transferSecret)
	for _, test := range []struct {
		name                              string
		count                             int
		tamper, skipDigest, unsignedFinal bool
		wantFailure                       bool
	}{
		{name: "one unsigned", count: 1},
		{name: "99 unsigned", count: 99},
		{name: "100 unsigned", count: 100, wantFailure: true},
		{name: "tampered unsigned record", count: 1, tamper: true, wantFailure: true},
		{name: "unsigned omitted from digest", count: 1, skipDigest: true, wantFailure: true},
		{name: "unsigned closing SOA", count: 1, unsignedFinal: true, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				first := transferReply(q, transferSOA())
				first.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
				wire, previous, err := dns.TsigGenerateWithProvider(first, dnsserver.TSIGProvider(ring), q.IsTsig().MAC, false)
				if err != nil {
					t.Errorf("sign first: %v", err)
					return
				}
				if _, err := w.Write(wire); err != nil {
					return
				}
				var unsigned []byte
				for i := range test.count {
					m := transferReply(q, &dns.A{Hdr: dns.RR_Header{Name: fmt.Sprintf("ns%d.example.org.", i), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 1)})
					m.Question = nil
					wire, _ = m.Pack()
					unsigned = append(unsigned, wire...)
					if test.tamper {
						wire[len(wire)-1] = 2
					}
					if _, err := w.Write(wire); err != nil {
						return
					}
				}
				last := transferReply(q, transferSOA())
				if test.unsignedFinal {
					wire, _ = last.Pack()
				} else {
					if test.skipDigest {
						unsigned = nil
					}
					last.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
					wire, _, err = dns.TsigGenerateWithProvider(last, transferStreamSigner{previous, unsigned}, previous, true)
					if err != nil {
						t.Errorf("sign last: %v", err)
						return
					}
				}
				_, _ = w.Write(wire)
			})
			got, err := runTransfer(t, &TransferClient{Keyring: ring}, transferZone(true), address, false)
			if (err != nil) != test.wantFailure {
				t.Fatalf("error=%v, want failure=%v", err, test.wantFailure)
			}
			if !test.wantFailure && len(got) != test.count+1 {
				t.Fatalf("records = %d, want %d", len(got), test.count+1)
			}
		})
	}
}

func TestTransferClientInvalidAXFR(t *testing.T) {
	soa := transferSOA()
	a := transferRR(t, "ns1.example.org. 300 IN A 192.0.2.53")
	for _, test := range []struct {
		name   string
		change func(*dns.Msg, *dns.Msg)
	}{
		{"wrong ID", func(first, _ *dns.Msg) { first.Id++ }},
		{"not response", func(first, _ *dns.Msg) { first.Response = false }},
		{"wrong opcode", func(first, _ *dns.Msg) { first.Opcode = dns.OpcodeNotify }},
		{"not authoritative", func(first, _ *dns.Msg) { first.Authoritative = false }},
		{"truncated bit", func(first, _ *dns.Msg) { first.Truncated = true }},
		{"reserved bit", func(first, _ *dns.Msg) { first.Zero = true }},
		{"first question missing", func(first, _ *dns.Msg) { first.Question = nil }},
		{"wrong question", func(first, _ *dns.Msg) { first.Question[0].Name = "other.org." }},
		{"wrong query type", func(first, _ *dns.Msg) { first.Question[0].Qtype = dns.TypeIXFR }},
		{"wrong query class", func(first, _ *dns.Msg) { first.Question[0].Qclass = dns.ClassCHAOS }},
		{"later error", func(_, last *dns.Msg) { last.Rcode = dns.RcodeServerFailure }},
		{"later wrong question", func(_, last *dns.Msg) { last.Question[0].Name = "other.org." }},
		{"nonempty authority", func(first, _ *dns.Msg) { first.Ns = []dns.RR{soa} }},
		{"unexpected additional", func(first, _ *dns.Msg) { first.Extra = []dns.RR{a} }},
		{"missing initial SOA", func(first, _ *dns.Msg) { first.Answer = []dns.RR{a} }},
		{"no answers", func(first, _ *dns.Msg) { first.Answer = nil }},
		{"no closing SOA", func(_, last *dns.Msg) { last.Answer = []dns.RR{a} }},
		{"SOA followed by data", func(_, last *dns.Msg) { last.Answer = []dns.RR{soa, a} }},
		{"out of zone", func(first, _ *dns.Msg) {
			first.Answer = append(first.Answer, transferRR(t, "other.org. 300 IN A 192.0.2.1"))
		}},
		{"wrong class", func(first, _ *dns.Msg) {
			rr := dns.Copy(a)
			rr.Header().Class = dns.ClassCHAOS
			first.Answer = append(first.Answer, rr)
		}},
		{"child SOA", func(_, last *dns.Msg) {
			rr := dns.Copy(soa)
			rr.Header().Name = "child.example.org."
			last.Answer = []dns.RR{rr}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				first, last := transferReply(q, soa), transferReply(q, soa)
				test.change(first, last)
				_ = w.WriteMsg(first)
				_ = w.WriteMsg(last)
			})
			if _, err := runTransfer(t, &TransferClient{}, transferZone(false), address, false); err == nil {
				t.Fatal("accepted malformed AXFR")
			}
		})
	}
	for _, field := range []string{"Serial", "Refresh", "Retry", "Expire", "Minttl", "Ns", "Mbox", "Ttl"} {
		t.Run("closing SOA "+field, func(t *testing.T) {
			closing := dns.Copy(soa).(*dns.SOA)
			value := reflect.ValueOf(closing).Elem().FieldByName(field)
			if field == "Ttl" {
				value = reflect.ValueOf(&closing.Hdr).Elem().FieldByName(field)
			}
			if value.Kind() == reflect.String {
				value.SetString("changed.example.org.")
			} else {
				value.SetUint(value.Uint() + 1)
			}
			address := transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				_ = w.WriteMsg(transferReply(q, soa, closing))
			})
			if _, err := runTransfer(t, &TransferClient{}, transferZone(false), address, false); err == nil {
				t.Fatal("accepted mismatched SOA")
			}
		})
	}
}

func TestTransferClientMalformedWire(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"short header", func(wire []byte) []byte { return wire[:11] }},
		{"trailing bytes", func(wire []byte) []byte { return append(wire, 0) }},
		{"missing answer", func(wire []byte) []byte { binary.BigEndian.PutUint16(wire[6:], 3); return wire }},
		{"missing additional", func(wire []byte) []byte { binary.BigEndian.PutUint16(wire[10:], 1); return wire }},
		{"truncated RDATA", func(wire []byte) []byte { return wire[:len(wire)-1] }},
		{"multiple questions", func(wire []byte) []byte { binary.BigEndian.PutUint16(wire[4:], 2); return wire }},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				wire, _ := transferReply(q, transferSOA(), transferSOA()).Pack()
				_, _ = w.Write(test.change(wire))
			})
			if _, err := runTransfer(t, &TransferClient{}, transferZone(false), address, false); err == nil {
				t.Fatal("accepted malformed wire")
			}
		})
	}
}

func TestTransferClientInvalidSOA(t *testing.T) {
	for _, signed := range []bool{false, true} {
		for _, test := range []struct {
			name   string
			change func(*dns.Msg)
		}{
			{"absent SOA", func(m *dns.Msg) { m.Answer = nil }},
			{"multiple SOAs", func(m *dns.Msg) { m.Answer = append(m.Answer, transferSOA()) }},
			{"wrong owner", func(m *dns.Msg) { m.Answer[0].Header().Name = "other.org." }},
			{"child owner", func(m *dns.Msg) { m.Answer[0].Header().Name = "child.example.org." }},
			{"wrong class", func(m *dns.Msg) { m.Answer[0].Header().Class = dns.ClassCHAOS }},
			{"not SOA", func(m *dns.Msg) { m.Answer = []dns.RR{transferRR(t, "example.org. 300 IN A 192.0.2.1")} }},
			{"not authoritative", func(m *dns.Msg) { m.Authoritative = false }},
			{"wrong ID", func(m *dns.Msg) { m.Id++ }},
			{"truncated", func(m *dns.Msg) { m.Truncated = true }},
			{"wrong question", func(m *dns.Msg) { m.Question[0].Name = "other.org." }},
		} {
			t.Run(fmt.Sprintf("signed=%v/%s", signed, test.name), func(t *testing.T) {
				var ring dnsserver.TSIGKeyring
				if signed {
					ring = transferRing(t, transferKey, "hmac-sha256", transferSecret)
				}
				address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
					defer func() { _ = w.Close() }()
					m := transferReply(q, transferSOA())
					test.change(m)
					if signed {
						m.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
					}
					_ = w.WriteMsg(m)
				})
				if _, err := runTransfer(t, &TransferClient{Keyring: ring}, transferZone(signed), address, true); err == nil {
					t.Fatal("accepted invalid SOA")
				}
			})
		}
	}
}

func TestTransferClientEmptyRDATA(t *testing.T) {
	address := transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		m := transferReply(q, transferSOA(), &dns.RFC3597{Hdr: dns.RR_Header{
			Name: "example.org.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300,
		}}, transferSOA())
		_ = w.WriteMsg(m)
	})
	if _, err := runTransfer(t, &TransferClient{}, transferZone(false), address, false); err == nil {
		t.Fatal("accepted A record with zero RDLENGTH")
	}
}

func TestTransferClientTSIGPlacement(t *testing.T) {
	ring := transferRing(t, transferKey, "hmac-sha256", transferSecret)
	for _, mode := range []string{"duplicate", "not last", "wrong class", "wrong TTL", "wrong original ID", "zero timestamp", "unexpected"} {
		t.Run(mode, func(t *testing.T) {
			address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				m := transferReply(q, transferSOA(), transferSOA())
				if q.Question[0].Qtype == dns.TypeSOA {
					m.Answer = m.Answer[:1]
				}
				m.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
				sig := m.IsTsig()
				switch mode {
				case "duplicate":
					m.Extra = append(m.Extra, dns.Copy(sig))
				case "not last":
					m.Extra = append(m.Extra, &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Class: 1232}})
				case "wrong class":
					sig.Hdr.Class = dns.ClassINET
				case "wrong TTL":
					sig.Hdr.Ttl = 300
				case "wrong original ID":
					sig.OrigId++
				}
				var wire []byte
				if mode == "duplicate" || mode == "not last" {
					wire, _ = m.Pack()
				} else {
					previous := ""
					if q.IsTsig() != nil {
						previous = q.IsTsig().MAC
					}
					var mac string
					wire, mac, _ = dns.TsigGenerateWithProvider(m, dnsserver.TSIGProvider(ring), previous, false)
					if mode == "zero timestamp" {
						sig.TimeSigned, sig.MAC = 0, mac
						sig.MACSize = uint16(len(mac) / 2) //nolint:gosec // G115: HMAC-SHA256 fixture MAC is 32 bytes.
						m.Extra = append(m.Extra, sig)
						wire, _ = m.Pack()
					}
				}
				_, _ = w.Write(wire)
			})
			for _, poll := range []bool{true, false} {
				_, err := runTransfer(t, &TransferClient{Keyring: ring}, transferZone(mode != "unexpected"), address, poll)
				if err == nil {
					t.Fatal("accepted invalid TSIG framing")
				}
				if mode == "zero timestamp" && !errors.Is(err, dns.ErrTime) {
					t.Fatalf("zero wire timestamp was defaulted to now: %v", err)
				}
			}
		})
	}
}

func TestTransferClientPartialTCPFrame(t *testing.T) {
	for _, payload := range []bool{false, true} {
		for _, cancelRead := range []bool{false, true} {
			t.Run(fmt.Sprintf("payload=%v/cancel=%v", payload, cancelRead), func(t *testing.T) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = ln.Close() }()
				written, primaryDone := make(chan struct{}), make(chan error, 1)
				// dns.Conn reads the real query; raw net.Conn writes deliberately
				// incomplete TCP framing that ResponseWriter cannot produce.
				go func() {
					conn, err := ln.Accept()
					if err != nil {
						primaryDone <- err
						return
					}
					defer func() { _ = conn.Close() }()
					_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
					if _, err = (&dns.Conn{Conn: conn}).ReadMsg(); err != nil {
						primaryDone <- err
						return
					}
					part := []byte{0}
					if payload {
						part = []byte{0, 50, 1, 2, 3}
					}
					_, err = conn.Write(part)
					close(written)
					if cancelRead && err == nil {
						var buf [1]byte
						_, err = conn.Read(buf[:])
						if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, syscall.ECONNRESET) {
							primaryDone <- fmt.Errorf("client did not close canceled connection: %v", err)
							return
						}
						err = nil
					}
					primaryDone <- err
				}()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					_, err := (&TransferClient{Timeout: time.Second}).AXFR(ctx, transferZone(false), ln.Addr().String())
					result <- err
				}()
				select {
				case <-written:
				case <-time.After(time.Second):
					t.Fatal("primary did not write partial frame")
				}
				if cancelRead {
					cancel()
				}
				select {
				case err := <-result:
					if err == nil || cancelRead && !errors.Is(err, context.Canceled) {
						t.Fatalf("partial TCP frame error: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("transfer did not stop")
				}
				if err := <-primaryDone; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestTransferClientBounds(t *testing.T) {
	soa := transferSOA()
	rr := transferRR(t, strings.Repeat("a", 60)+"."+strings.Repeat("b", 60)+".example.org. 300 IN NS "+strings.Repeat("c", 60)+".example.org.")
	address := transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		_ = w.WriteMsg(transferReply(q, soa))
		_ = w.WriteMsg(transferReply(q, rr, rr, rr, rr, rr, rr, rr, rr, rr, rr))
		_ = w.WriteMsg(transferReply(q, soa))
	})
	for _, test := range []struct {
		name   string
		client TransferClient
		want   string
	}{
		{"records", TransferClient{MaxRecords: 10}, "record limit"},
		{"compressed expansion", TransferClient{MaxBytes: 1000}, "expanded byte limit"},
		{"wire bytes", TransferClient{MaxBytes: 15}, "wire byte limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runTransfer(t, &test.client, transferZone(false), address, false); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
		})
	}
	got, err := runTransfer(t, &TransferClient{MaxRecords: 11}, transferZone(false), address, false)
	if err != nil || len(got) != 11 {
		t.Fatalf("exact record bound: len=%d error=%v", len(got), err)
	}
	// A transfer may contain both bookends in a single DNS message.
	address = transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		_ = w.WriteMsg(transferReply(q, soa, soa))
	})
	got, err = runTransfer(t, &TransferClient{MaxRecords: 1}, transferZone(false), address, false)
	if err != nil || len(got) != 1 {
		t.Fatalf("SOA-only zone: len=%d error=%v", len(got), err)
	}
}

func TestTransferClientCancellation(t *testing.T) {
	for _, poll := range []bool{true, false} {
		for _, deadline := range []bool{true, false} {
			t.Run(fmt.Sprintf("SOA=%v/deadline=%v", poll, deadline), func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				address := transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
					defer func() { _ = w.Close() }()
					if !poll {
						_ = w.WriteMsg(transferReply(q, transferSOA()))
					}
					close(entered)
					<-release
				})
				defer close(release)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				client := &TransferClient{Timeout: 2 * time.Second}
				if deadline {
					client.Timeout = 100 * time.Millisecond
				}
				result := make(chan error, 1)
				go func() {
					var err error
					if poll {
						_, err = client.SOA(ctx, transferZone(false), address)
					} else {
						_, err = client.AXFR(ctx, transferZone(false), address)
					}
					result <- err
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("query did not reach primary")
				}
				want := context.DeadlineExceeded
				if !deadline {
					want = context.Canceled
					cancel()
				}
				select {
				case err := <-result:
					if !errors.Is(err, want) {
						t.Fatalf("error = %v, want %v", err, want)
					}
				case <-time.After(time.Second):
					t.Fatal("cancellation did not stop transfer")
				}
			})
		}
	}
}

func TestTransferAddressAndConfiguration(t *testing.T) {
	for input, want := range map[string]string{
		"primary.example": "primary.example:53", "127.0.0.1": "127.0.0.1:53",
		"primary.example:5353": "primary.example:5353", "2001:db8::1": "[2001:db8::1]:53",
		"[2001:db8::1]": "[2001:db8::1]:53", "[2001:db8::1]:5353": "[2001:db8::1]:5353",
	} {
		got, err := transferAddress(input)
		if err != nil || got != want {
			t.Errorf("address %q = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "tcp://host", "host:bad", "host:0", "host:65536", ":53", "host:", "[invalid]", "host name"} {
		if _, err := transferAddress(input); err == nil {
			t.Errorf("accepted primary address %q", input)
		}
	}
	for _, protocol := range []string{"udp", "https"} {
		dz := transferZone(false)
		dz.Spec.Secondary.TransferProtocol = protocol
		_, err := (&TransferClient{}).AXFR(context.Background(), dz, "127.0.0.1:1")
		if err == nil || !strings.Contains(err.Error(), "unsupported transfer protocol") {
			t.Fatalf("protocol %q: %v", protocol, err)
		}
	}
	_, err := (&TransferClient{}).SOA(context.Background(), transferZone(true), "127.0.0.1:1")
	if !errors.Is(err, dns.ErrSecret) {
		t.Fatalf("missing keyring: %v", err)
	}
}

func TestTransferStreamSignerFixture(t *testing.T) {
	// Guard the independent fixture: without intermediate data its digest
	// must agree with miekg's ordinary timers-only signature.
	ring := transferRing(t, transferKey, "hmac-sha256", transferSecret)
	q := new(dns.Msg)
	q.SetAxfr("example.org.")
	previous := hex.EncodeToString([]byte("previous-MAC"))
	m := transferReply(q, transferSOA())
	m.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
	replyCopy := m.Copy()
	a, _, err := dns.TsigGenerateWithProvider(m, dnsserver.TSIGProvider(ring), previous, true)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := dns.TsigGenerateWithProvider(replyCopy, transferStreamSigner{previous: previous}, previous, true)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("independent signer disagrees with miekg: %v", err)
	}
}
