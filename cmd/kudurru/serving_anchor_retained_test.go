package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	q := new(dns.Msg)
	q.SetQuestion("retained.example.", dns.TypeA)
	req := &pipeline.Request{Ctx: ctx, Msg: q, Serving: servingdns.Wrap(lease)}
	insertion, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	native, err := captureRootNativeSource(req)
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := native.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.opts.Store.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return stageCommandLabel(ctx, tx, auth.Principal{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mutation.Apply(func() error { t.Fatal("native mutation lost its strict data guard"); return nil }); err == nil {
		t.Fatal("native mutation accepted superseded data")
	}
	// No new publication is captured. Both an original insertion and one
	// captured from the retained lease after commit must leave the cache alone.
	late, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []interface{ Apply(func() error) error }{insertion, late} {
		if err := attempt.Apply(func() error { t.Fatal("old data mutated cache"); return nil }); err != nil {
			t.Fatal("ordinary commit invalidated held DNS work", err)
		}
		if err := attempt.Apply(func() error { t.Fatal("consumed insertion reused"); return nil }); err == nil {
			t.Fatal("skipped mutation did not consume its attempt")
		}
	}
	// Exercise actual ingress replies from the original complete lease. These
	// carry node identity, never canonical authoritative or local zone data.
	for _, name := range []string{ingress.HealthName, "id.server."} {
		t.Run(name, func(t *testing.T) {
			query := new(dns.Msg)
			query.SetQuestion(name, dns.TypeTXT)
			if name == "id.server." {
				query.Question[0].Qclass = dns.ClassCHAOS
			}
			request := &pipeline.Request{Ctx: ctx, Msg: query, Serving: req.Serving}
			output, err := captureRootAnchorOutput(request)
			if err != nil {
				t.Fatal(err)
			}
			response, err := (&ingress.Stage{NodeID: "original-node"}).Serve(request, func(*pipeline.Request) (*pipeline.Response, error) {
				t.Fatal("node identity reached a data-answering stage")
				return nil, nil
			})
			if err != nil || len(response.Msg.Answer) != 1 || response.Msg.Answer[0].Header().Rrtype != dns.TypeTXT {
				t.Fatal("ingress identity reply failed", err)
			}
			o := output.(*rootAnchorOutput)
			if err := o.BindResponse(response); err != nil {
				t.Fatal(err)
			}
			called := false
			if err := o.Emit(func() error { called = true; return nil }); err != nil || !called {
				t.Fatal("ordinary commit suppressed ingress identity", err)
			}
			if err := o.Next(); err == nil {
				t.Fatal("node identity granted transfer continuation")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		origin pipeline.ResponseOrigin
		status pipeline.Status
		rcode  int
		allow  bool
	}{
		{"forwarded", pipeline.OriginForwarded, pipeline.StatusForwarded, dns.RcodeSuccess, true},
		{"cached", pipeline.OriginForwarded, pipeline.StatusCached, dns.RcodeSuccess, true},
		{"blocked alias", pipeline.OriginSynthetic, pipeline.StatusBlockedCNAME, dns.RcodeNameError, true},
		{"authoritative", pipeline.OriginAuthoritative, pipeline.StatusAuthoritative, dns.RcodeSuccess, false},
		{"mixed", pipeline.OriginMixed, pipeline.StatusForwarded, dns.RcodeSuccess, false},
		{"local", pipeline.OriginLocal, pipeline.StatusLocalRecord, dns.RcodeSuccess, false},
		{"unattributed", pipeline.OriginUnknown, pipeline.StatusForwarded, dns.RcodeSuccess, false},
		{"unavailable", pipeline.OriginUnknown, pipeline.StatusServfailUnsynced, dns.RcodeServerFailure, true},
		{"unavailable success", pipeline.OriginUnknown, pipeline.StatusServfailUnsynced, dns.RcodeSuccess, false},
		{"authoritative special status", pipeline.OriginAuthoritative, pipeline.StatusSpecialDomain, dns.RcodeSuccess, false},
		{"upstream failure", pipeline.OriginUnknown, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, true},
		{"upstream timeout", pipeline.OriginUnknown, pipeline.StatusTimeoutUpstream, dns.RcodeServerFailure, true},
		{"failed target", pipeline.OriginFailure, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, true},
		{"upstream failure success", pipeline.OriginUnknown, pipeline.StatusServfailUpstream, dns.RcodeSuccess, false},
		{"authoritative upstream failure", pipeline.OriginAuthoritative, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false},
		{"local upstream failure", pipeline.OriginLocal, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false},
		{"mixed upstream timeout", pipeline.OriginMixed, pipeline.StatusTimeoutUpstream, dns.RcodeServerFailure, false},
		{"synthesized upstream failure", pipeline.OriginSynthetic, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := captureRootAnchorOutput(req)
			if err != nil {
				t.Fatal(err)
			}
			o := output.(*rootAnchorOutput)
			if o.seal.current() || !o.seal.dnsCurrent() {
				t.Fatal("fixture did not retain an old data generation")
			}
			m := new(dns.Msg)
			m.SetReply(q)
			m.Rcode = tc.rcode
			if err := o.BindResponse(&pipeline.Response{Msg: m, Origin: tc.origin, Status: tc.status}); err != nil {
				t.Fatal(err)
			}
			called := false
			err = o.Emit(func() error { called = true; return nil })
			if called != tc.allow || (err == nil) != tc.allow {
				t.Fatal("wrong original output disposition", called, err)
			}
			if err := o.Next(); err == nil {
				t.Fatal("retained DNS output authorized a transfer continuation")
			}
		})
	}
	// The actual upstream stage against a port nothing listens on, with and
	// without EDNS: its empty SERVFAIL is the answer the client would have had
	// before the commit, and the only form the retained check lets through.
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.LocalAddr().String()
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	forward, err := upstreamstage.New("default", []string{dead}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forward.Set().Close() })
	address := &dns.A{Hdr: dns.RR_Header{Name: "upstream-failure.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}
	for _, edns := range []bool{false, true} {
		query := new(dns.Msg)
		query.SetQuestion("upstream-failure.example.", dns.TypeA)
		if edns {
			query.SetEdns0(1232, false)
		}
		failed, err := forward.Serve(&pipeline.Request{Ctx: ctx, Msg: query}, nil)
		if err != nil || failed == nil || failed.Msg == nil || failed.Msg.Rcode != dns.RcodeServerFailure ||
			failed.Status != pipeline.StatusServfailUpstream && failed.Status != pipeline.StatusTimeoutUpstream {
			t.Fatal("upstream stage did not report its failure", err, failed)
		}
		extended := false
		if opt := failed.Msg.IsEdns0(); opt != nil {
			for _, option := range opt.Option {
				_, ok := option.(*dns.EDNS0_EDE)
				extended = extended || ok
			}
		}
		if extended != edns {
			t.Fatal("upstream failure lost or invented its extended error", failed.Msg)
		}
		for _, tc := range []struct {
			name  string
			alter func(*dns.Msg)
			allow bool
		}{
			{"empty", func(*dns.Msg) {}, true},
			{"answer", func(m *dns.Msg) { m.Answer = []dns.RR{address} }, false},
			{"authority", func(m *dns.Msg) { m.Ns = []dns.RR{address} }, false},
			{"additional", func(m *dns.Msg) { m.Extra = append(m.Extra, address) }, false},
			{"authoritative", func(m *dns.Msg) { m.Authoritative = true }, false},
		} {
			name := "actual upstream failure " + tc.name
			if edns {
				name += " edns"
			}
			t.Run(name, func(t *testing.T) {
				response := *failed
				response.Msg = failed.Msg.Copy()
				tc.alter(response.Msg)
				output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: ctx, Msg: query, Serving: req.Serving})
				if err != nil {
					t.Fatal(err)
				}
				o := output.(*rootAnchorOutput)
				if o.seal.current() || !o.seal.dnsCurrent() {
					t.Fatal("fixture did not retain an old data generation")
				}
				if err := o.BindResponse(&response); err != nil {
					t.Fatal(err)
				}
				called := false
				err = o.Emit(func() error { called = true; return nil })
				if called != tc.allow || (err == nil) != tc.allow {
					t.Fatal("wrong upstream failure disposition", called, err)
				}
				if err := o.Next(); err == nil {
					t.Fatal("upstream failure authorized a transfer continuation")
				}
			})
		}
	}
	// Actual first Managed acceptance still retires all old static output and
	// cache attempts; permitting a skipped cache insertion grants no fallback.
	retired, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := captureOutputBootstrap(ctx, t, f)(); err != nil {
		t.Fatal(err)
	}
	if err := retired.Apply(func() error { t.Fatal("retired anchor mutated cache"); return nil }); err == nil {
		t.Fatal("managed acceptance treated as an ordinary data commit")
	}
	if _, err := captureRootAnchorOutput(req); err == nil {
		t.Fatal("old lease adopted managed output authority")
	}
}
