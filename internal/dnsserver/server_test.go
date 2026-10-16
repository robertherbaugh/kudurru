package dnsserver

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
)

// fakeUpstream answers every A query with 192.0.2.1.
func fakeUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.1")}}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

func startServer(t *testing.T, chain *pipeline.Chain) *Server {
	t.Helper()
	s := New("127.0.0.1:0", []string{"udp", "tcp"}, chain, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start: %v", err)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addrs(); a["udp"] != "" && a["tcp"] != "" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not bind")
	return nil
}

func TestEndToEndForward(t *testing.T) {
	up := fakeUpstream(t)
	fwd, err := upstream.New("default", []string{up}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	chain := pipeline.MustNew(&ingress.Stage{NodeID: "test-node", Version: "test"}, fwd)
	s := startServer(t, chain)

	for _, netw := range []string{"udp", "tcp"} {
		q := new(dns.Msg)
		q.SetQuestion("example.com.", dns.TypeA)
		c := &dns.Client{Net: netw, Timeout: 2 * time.Second}
		in, _, err := c.Exchange(q, s.Addrs()[netw])
		if err != nil {
			t.Fatalf("%s: %v", netw, err)
		}
		if in.Rcode != dns.RcodeSuccess || len(in.Answer) != 1 {
			t.Fatalf("%s: unexpected answer %v", netw, in)
		}
		if a, ok := in.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.1" {
			t.Fatalf("%s: wrong record %v", netw, in.Answer[0])
		}
	}
}

func TestHealthAndChaosAnsweredLocally(t *testing.T) {
	// No upstream at all: the ingress stage must answer by itself.
	chain := pipeline.MustNew(&ingress.Stage{NodeID: "node-a", Version: "1.2.3"})
	s := startServer(t, chain)
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}

	q := new(dns.Msg)
	q.SetQuestion(ingress.HealthName, dns.TypeTXT)
	in, _, err := c.Exchange(q, s.Addrs()["udp"])
	if err != nil {
		t.Fatal(err)
	}
	if in.Rcode != dns.RcodeSuccess || len(in.Answer) != 1 || in.Answer[0].(*dns.TXT).Txt[0] != "node-a 0" {
		t.Fatalf("health answer wrong: %v", in)
	}

	q = new(dns.Msg)
	q.SetQuestion("version.bind.", dns.TypeTXT)
	q.Question[0].Qclass = dns.ClassCHAOS
	in, _, err = c.Exchange(q, s.Addrs()["udp"])
	if err != nil {
		t.Fatal(err)
	}
	if in.Answer[0].(*dns.TXT).Txt[0] != "kudurru 1.2.3" {
		t.Fatalf("version.bind wrong: %v", in)
	}

	// A normal query with nothing to answer it is SERVFAIL, never NXDOMAIN.
	q = new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	in, _, err = c.Exchange(q, s.Addrs()["udp"])
	if err != nil {
		t.Fatal(err)
	}
	if in.Rcode != dns.RcodeServerFailure {
		t.Fatalf("expected SERVFAIL, got %s", dns.RcodeToString[in.Rcode])
	}
}
