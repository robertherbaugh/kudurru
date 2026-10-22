package main

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func startTestProbe(t *testing.T, p *probe) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() { done <- runServers(ctx, "127.0.0.1:0", p, func(address string) { ready <- address }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("probe did not stop")
		}
	})
	select {
	case address := <-ready:
		return address
	case err := <-done:
		t.Fatalf("probe failed to start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	return ""
}

func tkeyRequest(key string, token []byte) *dns.Msg {
	if len(token) > 65535 {
		panic("oversized test token")
	}
	r := new(dns.Msg)
	r.SetQuestion(key, dns.TypeTKEY)
	r.Extra = []dns.RR{&dns.TKEY{
		Hdr:       dns.RR_Header{Name: key, Rrtype: dns.TypeTKEY, Class: dns.ClassANY},
		Algorithm: gssAlgorithm, Mode: 3, Key: hex.EncodeToString(token), KeySize: uint16(len(token)), //nolint:gosec // G115: token length checked above.
	}}
	return r
}

func TestProbeRefusals(t *testing.T) {
	p := &probe{acceptor: testAcceptor(t), contexts: make(map[string]*securityContext),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	address := startTestProbe(t, p)
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			client := &dns.Client{Net: transport, Timeout: time.Second}
			unsigned := new(dns.Msg)
			unsigned.SetUpdate("probe.example.")
			reply, _, err := client.Exchange(unsigned, address)
			if err != nil || reply.Rcode != dns.RcodeRefused {
				t.Fatalf("unsigned update: %v, %v", reply, err)
			}
			for _, test := range []struct {
				name string
				r    *dns.Msg
				code uint16
			}{
				{"invalid token", tkeyRequest("invalid."+transport+".", []byte{0}), dns.RcodeBadKey},
				{"unsupported mode", func() *dns.Msg {
					r := tkeyRequest("badmode."+transport+".", []byte{0})
					r.Extra[0].(*dns.TKEY).Mode = 5
					return r
				}(), dns.RcodeBadMode},
				{"unsupported algorithm", func() *dns.Msg {
					r := tkeyRequest("badalg."+transport+".", []byte{0})
					r.Extra[0].(*dns.TKEY).Algorithm = dns.HmacSHA256
					return r
				}(), dns.RcodeBadAlg},
			} {
				t.Run(test.name, func(t *testing.T) {
					reply, _, err := client.Exchange(test.r, address)
					if err != nil || len(reply.Answer) != 1 {
						t.Fatalf("TKEY reply: %v, %v", reply, err)
					}
					if got := reply.Answer[0].(*dns.TKEY).Error; got != test.code {
						t.Fatalf("got TKEY error %d, want %d", got, test.code)
					}
				})
			}
		})
	}
}

func TestNoUnsignedContextReplacement(t *testing.T) {
	a := testAcceptor(t)
	p := &probe{acceptor: a, contexts: map[string]*securityContext{"protected.example.": testContext()},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	input, _ := testAPReq(t, a, nil)
	r := tkeyRequest("PROTECTED.example.", input)
	m := new(dns.Msg)
	m.SetReply(r)
	p.negotiate(m, r)
	if m.Answer[0].(*dns.TKEY).Error != dns.RcodeBadName {
		t.Fatal("allowed replacement of another security context")
	}
	if len(p.contexts) != 1 || p.contexts["protected.example."].principal != "" {
		t.Fatal("changed protected context")
	}
}
