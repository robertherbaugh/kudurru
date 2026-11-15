package secondary

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

func ixfrSOA(serial uint32) *dns.SOA {
	s := transferSOA()
	s.Serial = serial
	return s
}

func ixfrPrimary(t *testing.T, ring dnsserver.TSIGKeyring, frames [][]dns.RR) string {
	t.Helper()
	return transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		if q.Question[0].Qtype != dns.TypeIXFR || len(q.Ns) != 1 || q.Ns[0].(*dns.SOA).Serial != transferSOA().Serial {
			t.Errorf("invalid IXFR request: %v", q)
			return
		}
		if q.IsTsig() != nil && w.TsigStatus() != nil {
			t.Errorf("invalid signed IXFR request: %v", w.TsigStatus())
			return
		}
		for i, frame := range frames {
			m := transferReply(q, frame...)
			if i > 0 {
				m.Question = nil
			}
			if ts := q.IsTsig(); ts != nil {
				m.SetTsig(ts.Hdr.Name, ts.Algorithm, 300, time.Now().Unix())
			}
			if err := w.WriteMsg(m); err != nil {
				return
			}
			w.TsigTimersOnly(true)
		}
	})
}

func rrStrings(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, rr.String())
	}
	slices.Sort(out)
	return out
}

func TestIXFRUpdatesAndFallback(t *testing.T) {
	old := transferRR(t, "WWW.Example.org. 37 IN CNAME Old.Example.org.")
	removed := transferRR(t, "www.example.org. 37 IN CNAME old.example.org.")
	kept := transferRR(t, "txt.example.org. 91 IN TXT \"MiXeD Data\"")
	added := transferRR(t, "www.example.org. 53 IN TYPE65400 \\# 4 12345678")
	final := transferRR(t, "www.example.org. 123 IN A 192.0.2.123")
	for _, algorithm := range []string{"", "hmac-sha256", "hmac-sha512"} {
		t.Run(algorithm, func(t *testing.T) {
			var ring dnsserver.TSIGKeyring
			if algorithm != "" {
				ring = transferRing(t, transferKey, algorithm, transferSecret)
			}
			base := []dns.RR{transferSOA(), old, kept}
			before := rrStrings(base)
			for _, test := range []struct {
				name   string
				frames [][]dns.RR
				want   []dns.RR
			}{
				{"same serial", [][]dns.RR{{transferSOA()}}, base},
				{"wrap and multiple deltas", [][]dns.RR{{ixfrSOA(1)}, {transferSOA(), removed}, {ixfrSOA(0), added}, {ixfrSOA(0), added}, {ixfrSOA(1), final}, {ixfrSOA(1)}}, []dns.RR{ixfrSOA(1), kept, final}},
				{"AXFR stream fallback", [][]dns.RR{{ixfrSOA(1)}, {kept, final}, {ixfrSOA(1)}}, []dns.RR{ixfrSOA(1), kept, final}},
				{"empty AXFR stream fallback", [][]dns.RR{{ixfrSOA(1), ixfrSOA(1)}}, []dns.RR{ixfrSOA(1)}},
			} {
				t.Run(test.name, func(t *testing.T) {
					address := ixfrPrimary(t, ring, test.frames)
					got, err := (&TransferClient{Keyring: ring}).IXFR(t.Context(), transferZone(algorithm != ""), address, base)
					if err != nil || !reflect.DeepEqual(rrStrings(got), rrStrings(test.want)) {
						t.Fatalf("IXFR got %v, %v; want %v", got, err, test.want)
					}
					got[0].(*dns.SOA).Serial = 123 // Returned records must not alias base.
					if !reflect.DeepEqual(before, rrStrings(base)) {
						t.Fatal("IXFR mutated the base")
					}
				})
			}
		})
	}
}

func TestIXFRNotImplementedFallback(t *testing.T) {
	ring := transferRing(t, transferKey, "hmac-sha256", transferSecret)
	var requests atomic.Int32
	address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		requests.Add(1)
		m := transferReply(q)
		if q.Question[0].Qtype == dns.TypeIXFR {
			m.Rcode, m.Authoritative = dns.RcodeNotImplemented, false
		} else {
			m.Answer = []dns.RR{ixfrSOA(1), ixfrSOA(1)}
		}
		m.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
		_ = w.WriteMsg(m)
	})
	client := &TransferClient{Keyring: ring}
	got, err := client.IXFR(t.Context(), transferZone(true), address, []dns.RR{transferSOA()})
	if err != nil || len(got) != 1 || got[0].(*dns.SOA).Serial != 1 || requests.Load() != 2 {
		t.Fatalf("NOTIMP fallback: %v, %v, requests=%d", got, err, requests.Load())
	}
	// The first NOTIMP response consumes part of the same byte budget.
	client.MaxBytes = int64(dns.Len(ixfrSOA(1))*2 + 120)
	got, err = client.IXFR(t.Context(), transferZone(true), address, []dns.RR{transferSOA()})
	if err == nil || got != nil {
		t.Fatalf("fallback bypassed shared byte budget: %v, %v", got, err)
	}
}

func TestIXFRInvalidDelta(t *testing.T) {
	a := transferRR(t, "www.example.org. 123 IN A 192.0.2.1")
	b := transferRR(t, "www.example.org. 124 IN A 192.0.2.2")
	outside := transferRR(t, "evil.test. 123 IN A 192.0.2.1")
	mismatch := ixfrSOA(1)
	mismatch.Expire++
	for _, test := range []struct {
		name string
		rrs  []dns.RR
	}{
		{"no opening SOA", []dns.RR{a}},
		{"older serial", []dns.RR{ixfrSOA(4294967293)}},
		{"ambiguous serial", []dns.RR{ixfrSOA(2147483646)}},
		{"wrong base", []dns.RR{ixfrSOA(1), ixfrSOA(4294967293)}},
		{"missing delete", []dns.RR{ixfrSOA(1), transferSOA(), b, ixfrSOA(1), ixfrSOA(1)}},
		{"wrong delete TTL", []dns.RR{ixfrSOA(1), transferSOA(), transferRR(t, "www.example.org. 124 IN A 192.0.2.1"), ixfrSOA(1), ixfrSOA(1)}},
		{"duplicate addition", []dns.RR{ixfrSOA(1), transferSOA(), ixfrSOA(1), a, ixfrSOA(1)}},
		{"out of zone deletion", []dns.RR{ixfrSOA(1), transferSOA(), outside, ixfrSOA(1), ixfrSOA(1)}},
		{"out of zone addition", []dns.RR{ixfrSOA(1), transferSOA(), ixfrSOA(1), outside, ixfrSOA(1)}},
		{"out of zone fallback", []dns.RR{ixfrSOA(1), outside, ixfrSOA(1)}},
		{"wrong class", []dns.RR{ixfrSOA(1), transferSOA(), ixfrSOA(1), transferRR(t, "www.example.org. 123 CH TXT \"evil\""), ixfrSOA(1)}},
		{"non apex SOA", []dns.RR{ixfrSOA(1), transferRR(t, "child.example.org. 537 IN SOA ns.example.org. hostmaster.example.org. 4294967294 61 13 301 47")}},
		{"unchanged delta serial", []dns.RR{ixfrSOA(1), transferSOA(), transferSOA()}},
		{"past target serial", []dns.RR{ixfrSOA(1), transferSOA(), ixfrSOA(2)}},
		{"broken chain", []dns.RR{ixfrSOA(1), transferSOA(), ixfrSOA(0), ixfrSOA(4294967295)}},
		{"mismatched final SOA", []dns.RR{ixfrSOA(1), transferSOA(), mismatch, mismatch}},
		{"records after closing", []dns.RR{ixfrSOA(1), transferSOA(), ixfrSOA(1), ixfrSOA(1), b}},
		{"same serial with data", []dns.RR{transferSOA(), a, transferSOA()}},
		{"truncated additions", []dns.RR{ixfrSOA(1), transferSOA(), a, ixfrSOA(1), b}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := []dns.RR{transferSOA(), a}
			before := rrStrings(base)
			address := ixfrPrimary(t, nil, [][]dns.RR{test.rrs})
			got, err := (&TransferClient{}).IXFR(t.Context(), transferZone(false), address, base)
			if err == nil || got != nil || !reflect.DeepEqual(before, rrStrings(base)) {
				t.Fatalf("invalid IXFR accepted or changed base: %v, %v", got, err)
			}
		})
	}
}

func TestIXFRStreamingAuthentication(t *testing.T) {
	ring := transferRing(t, transferKey, "hmac-sha256", transferSecret)
	a := transferRR(t, "www.example.org. 123 IN A 192.0.2.1")
	b := transferRR(t, "www.example.org. 124 IN A 192.0.2.2")
	for _, mode := range []string{"valid unsigned intermediate", "tampered intermediate", "missing first", "missing final", "wrong MAC", "BADTIME", "unsigned NOTIMP"} {
		t.Run(mode, func(t *testing.T) {
			var count atomic.Int32
			address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				count.Add(1)
				first := transferReply(q, ixfrSOA(1))
				if mode == "unsigned NOTIMP" {
					first.Answer, first.Rcode = nil, dns.RcodeNotImplemented
					wire, _ := first.Pack()
					_, _ = w.Write(wire)
					return
				}
				first.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
				if mode == "BADTIME" {
					first.IsTsig().Error = dns.RcodeBadTime
				}
				wire, previous, _ := dns.TsigGenerateWithProvider(first, dnsserver.TSIGProvider(ring), q.IsTsig().MAC, false)
				if mode == "missing first" {
					first.Extra = nil
					wire, _ = first.Pack()
				}
				if _, err := w.Write(wire); err != nil {
					return
				}
				middle := transferReply(q, transferSOA(), a, ixfrSOA(1), b)
				middle.Question = nil
				unsigned, _ := middle.Pack()
				wire = slices.Clone(unsigned)
				if mode == "tampered intermediate" {
					wire[len(wire)-1] ^= 1
				}
				if _, err := w.Write(wire); err != nil {
					return
				}
				last := transferReply(q, ixfrSOA(1))
				last.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
				wire, _, _ = dns.TsigGenerateWithProvider(last, transferStreamSigner{previous, unsigned}, previous, true)
				switch mode {
				case "missing final":
					last.Extra = nil
					wire, _ = last.Pack()
				case "wrong MAC":
					wire[len(wire)-7] ^= 1
				}
				_, _ = w.Write(wire)
			})
			got, err := (&TransferClient{Keyring: ring}).IXFR(t.Context(), transferZone(true), address, []dns.RR{transferSOA(), a})
			if mode == "valid unsigned intermediate" {
				if err != nil || len(got) != 2 || !dns.IsDuplicate(got[1], b) {
					t.Fatalf("valid IXFR rejected: %v, %v", got, err)
				}
			} else if err == nil || got != nil || count.Load() != 1 {
				t.Fatalf("invalid stream accepted or triggered fallback: %v, %v, requests=%d", got, err, count.Load())
			}
			if mode == "BADTIME" && !errors.Is(err, dns.ErrTime) {
				t.Fatalf("BADTIME lost cause: %v", err)
			}
		})
	}
}

func TestIXFRBoundsAndBase(t *testing.T) {
	a := transferRR(t, "www.example.org. 123 IN A 192.0.2.1")
	b := transferRR(t, "www.example.org. 124 IN A 192.0.2.2")
	base := []dns.RR{transferSOA(), a}
	frames := [][]dns.RR{{ixfrSOA(1), transferSOA(), a, ixfrSOA(0), b, ixfrSOA(0), b, ixfrSOA(1), a, ixfrSOA(1)}}
	address := ixfrPrimary(t, nil, frames)
	for _, client := range []*TransferClient{{MaxRecords: 1}, {MaxRecords: 2}, {MaxBytes: 200}} {
		got, err := client.IXFR(t.Context(), transferZone(false), address, base)
		if err == nil || got != nil {
			t.Fatalf("IXFR bypassed bounds: %v, %v", got, err)
		}
	}
	for _, invalid := range [][]dns.RR{{a}, {transferSOA(), nil}, {transferSOA(), (*dns.A)(nil)}, {transferSOA(), transferSOA()}, {transferSOA(), a, a}} {
		got, err := (&TransferClient{}).IXFR(t.Context(), transferZone(false), address, invalid)
		if err == nil || got != nil {
			t.Fatalf("accepted invalid base: %v, %v", got, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := (&TransferClient{}).IXFR(ctx, transferZone(false), address, base)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled base validation: %v, %v", got, err)
	}
	address = transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		_ = w.WriteMsg(transferReply(q, ixfrSOA(1)))
		// Return without completing IXFR; server then closes the connection.
	})
	got, err = (&TransferClient{Timeout: 20 * time.Millisecond}).IXFR(context.Background(), transferZone(false), address, base)
	if err == nil || got != nil || (!errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "incomplete IXFR")) {
		t.Fatalf("truncated IXFR: %v, %v", got, err)
	}
}

func TestIXFREmptyBaseAndExactLimit(t *testing.T) {
	a := transferRR(t, "www.example.org. 123 IN A 192.0.2.1")
	address := transferPrimary(t, nil, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		if q.Question[0].Qtype != dns.TypeAXFR {
			t.Errorf("empty base must request AXFR: %v", q)
		}
		_ = w.WriteMsg(transferReply(q, transferSOA(), a, transferSOA()))
	})
	got, err := (&TransferClient{MaxRecords: 2}).IXFR(t.Context(), transferZone(false), address, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("initial AXFR through IXFR seam: %v, %v", got, err)
	}
	address = ixfrPrimary(t, nil, [][]dns.RR{{ixfrSOA(0), transferSOA(), a}, {ixfrSOA(0), a, ixfrSOA(0)}})
	got, err = (&TransferClient{MaxRecords: 2}).IXFR(t.Context(), transferZone(false), address, []dns.RR{transferSOA(), a})
	if err != nil || len(got) != 2 || got[0].(*dns.SOA).Serial != 0 {
		t.Fatalf("exact record/edit limits and serial zero: %v, %v", got, err)
	}
}

func TestIXFRContextClosesConnection(t *testing.T) {
	for _, protocol := range []string{"tcp", "tls"} {
		for _, mode := range []string{"cancel incomplete IXFR", "timeout AXFR fallback"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				client := &TransferClient{Timeout: time.Second}
				dz := transferZone(false)
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				if protocol == "tls" {
					pair, pool := transferCertificate(t, []string{"127.0.0.1"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
					client.RootCAs = pool
					dz.Spec.Secondary.TransferProtocol = "tls"
					ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12, NextProtos: []string{"dot"}})
				}
				t.Cleanup(func() { _ = ln.Close() })
				entered, done := make(chan struct{}), make(chan error, 1)
				go func() {
					for attempt := range 2 {
						conn, err := ln.Accept()
						if err != nil {
							done <- err
							return
						}
						_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
						dc := &dns.Conn{Conn: conn}
						q, err := dc.ReadMsg()
						if err != nil {
							_ = conn.Close()
							done <- err
							return
						}
						if mode == "timeout AXFR fallback" && attempt == 0 {
							m := transferReply(q)
							m.Rcode = dns.RcodeNotImplemented
							_ = dc.WriteMsg(m)
							_ = conn.Close()
							continue
						}
						_ = dc.WriteMsg(transferReply(q, ixfrSOA(1)))
						close(entered)
						_, err = dc.ReadMsg() // Observe closure when the client cancels.
						_ = conn.Close()
						done <- err
						return
					}
				}()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "timeout AXFR fallback" {
					client.Timeout = 100 * time.Millisecond
				}
				result := make(chan error, 1)
				go func() {
					rrs, err := client.IXFR(ctx, dz, ln.Addr().String(), []dns.RR{transferSOA()})
					if rrs != nil {
						err = fmt.Errorf("canceled transfer returned data")
					}
					result <- err
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("transfer did not reach primary")
				}
				want := context.DeadlineExceeded
				if mode == "cancel incomplete IXFR" {
					cancel()
					want = context.Canceled
				}
				select {
				case err := <-result:
					if !errors.Is(err, want) {
						t.Fatalf("transfer cancellation cause: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("transfer did not cancel")
				}
				select {
				case err := <-done:
					// Closing while the transfer response still has unread bytes
					// can send TCP RST instead of FIN. Both prove cancellation
					// closed the connection; timeouts and nil do not.
					if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
						t.Fatalf("primary did not observe closed socket: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("cancellation left the primary connection open")
				}
			})
		}
	}
}

func TestIXFRResultGrowthBounds(t *testing.T) {
	a := transferRR(t, "txt.example.org. 123 IN TXT "+strings.Repeat("\""+strings.Repeat("x", 180)+"\" ", 3))
	b := transferRR(t, "www.example.org. 124 IN A 192.0.2.2")
	base := []dns.RR{transferSOA(), a}
	address := ixfrPrimary(t, nil, [][]dns.RR{{ixfrSOA(1), transferSOA(), ixfrSOA(1), b, ixfrSOA(1)}})
	got, err := (&TransferClient{MaxRecords: 2}).IXFR(t.Context(), transferZone(false), address, base)
	if err == nil || got != nil || !strings.Contains(err.Error(), "resulting record limit") {
		t.Fatalf("IXFR grew past record budget: %v, %v", got, err)
	}
	// The small delta fits the wire/expanded budget, but retaining the large
	// base TXT plus the new record must fail the independent live-size budget.
	limit := int64(dns.Len(base[0]) + dns.Len(a) + 1)
	got, err = (&TransferClient{MaxBytes: limit}).IXFR(t.Context(), transferZone(false), address, base)
	if err == nil || got != nil || !strings.Contains(err.Error(), "resulting byte limit") {
		t.Fatalf("IXFR grew past live byte budget: %v, %v", got, err)
	}
}
