package dnsserver

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// endlessTransfer is a "local"-position stage that answers every AXFR with
// a stream of ~15 KB messages that never ends; done is closed when the
// server stops pulling from it.
type endlessTransfer struct{ done chan struct{} }

func (endlessTransfer) Name() string { return "local" }

func (s endlessTransfer) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	if req.Question().Qtype != dns.TypeAXFR {
		return next(req)
	}
	r := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusLocalRecord)
	txt := strings.Repeat("x", 250)
	msg := func() *dns.Msg {
		m := r.Msg.Copy()
		for range 60 {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: req.Question().Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{txt}})
		}
		return m
	}
	r.Transfer = func(yield func(*dns.Msg) bool) {
		defer close(s.done)
		for yield(msg()) {
		}
	}
	return r, nil
}

// TestTransferWriteDeadline: Limits.StreamWriteTimeout bounds each message
// of a zone transfer (miekg/dns sets no write deadline itself), on TCP and
// DoT. A client that keeps reading keeps the stream for longer than the
// timeout; once it stops reading, the server gives up the stream.
func TestTransferWriteDeadline(t *testing.T) {
	const timeout = 200 * time.Millisecond
	for _, p := range []Protocol{ProtoTCP, ProtoDoT} {
		t.Run(string(p), func(t *testing.T) {
			stage := endlessTransfer{done: make(chan struct{})}
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: p}}, stages: []pipeline.Stage{stage}, limits: Limits{StreamWriteTimeout: timeout}})
			raw, err := net.Dial("tcp", f.addr(p))
			if err != nil {
				t.Fatal(err)
			}
			conn := raw
			if p == ProtoDoT {
				tc := tls.Client(raw, &tls.Config{RootCAs: f.cert.Pool, ServerName: "localhost", NextProtos: []string{"dot"}, MinVersion: tls.VersionTLS12})
				if err := tc.Handshake(); err != nil {
					t.Fatal(err)
				}
				conn = tc
			}
			defer func() { _ = conn.Close() }()
			co := &dns.Conn{Conn: conn}
			m := new(dns.Msg)
			m.SetAxfr("example.org.")
			if err := co.WriteMsg(m); err != nil {
				t.Fatal(err)
			}

			// A reader: the deadline is per message, not per stream.
			start := time.Now()
			for time.Since(start) < 4*timeout {
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err := co.ReadMsg(); err != nil {
					t.Fatalf("steady reader cut off after %s: %v", time.Since(start), err)
				}
			}
			select {
			case <-stage.done:
				t.Fatal("the stream ended while the client was reading")
			default:
			}

			// The client stops reading: the server must give up.
			select {
			case <-stage.done:
			case <-time.After(10 * time.Second):
				t.Fatal("the transfer is still being written 10 s after the client stopped reading")
			}
		})
	}
}
