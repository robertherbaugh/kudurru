package main

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// A TSIG refusal the transport answers before any pipeline work binds its
// own message (dnsserver bindRefusal) and then follows the served-publication
// policy of every node-produced refusal (D378): sent while a canonical commit
// is not yet published. An unbound output keeps full data currency and is
// withheld in the same state, which is how a client of the mounted node saw
// no answer at all to a refused query sent right after a commit. A bound
// refusal of an UPDATE, a NOTIFY or a zone transfer is not in the served
// class and keeps full data currency: it is withheld in the same state.
func TestRootAnchorTSIGRefusalFollowsTheServedPublication(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	query := func(q *dns.Msg) { q.SetQuestion("served.example.", dns.TypeA) }
	request := func(build func(*dns.Msg)) *pipeline.Request {
		q := new(dns.Msg)
		build(q)
		q.SetTsig("refused.", dns.HmacSHA256, 300, time.Now().Unix())
		return &pipeline.Request{Ctx: ctx, Msg: q, Serving: servingdns.Wrap(lease)}
	}
	capture := func(build func(*dns.Msg)) (*pipeline.Request, *rootAnchorOutput) {
		t.Helper()
		req := request(build)
		output, err := captureRootAnchorOutput(req)
		if err != nil {
			t.Fatal(err)
		}
		return req, output.(*rootAnchorOutput)
	}
	refusal := func(req *pipeline.Request) *dns.Msg {
		m := new(dns.Msg)
		m.SetRcode(req.Msg, dns.RcodeNotAuth)
		m.Extra = append(m.Extra, &dns.TSIG{Hdr: dns.RR_Header{Name: "refused.", Rrtype: dns.TypeTSIG, Class: dns.ClassANY}, Algorithm: dns.HmacSHA256, Fudge: 300, Error: dns.RcodeBadSig})
		return m
	}
	emitted := func(output *rootAnchorOutput) bool {
		t.Helper()
		called := false
		err := output.Emit(func() error { called = true; return nil })
		if called != (err == nil) {
			t.Fatal("output ran its write but reported a refusal, or the reverse", err)
		}
		return called
	}
	boundReq, bound := capture(query)
	_, unbound := capture(query)
	strict := []struct {
		name  string
		build func(*dns.Msg)
	}{
		{"UPDATE", func(q *dns.Msg) { q.SetUpdate("served.example.") }},
		{"NOTIFY", func(q *dns.Msg) { q.SetNotify("served.example.") }},
		{"AXFR", func(q *dns.Msg) { q.SetQuestion("served.example.", dns.TypeAXFR) }},
		{"IXFR", func(q *dns.Msg) { q.SetIxfr("served.example.", 1, "ns.served.example.", "hostmaster.served.example.") }},
	}
	strictReqs := make([]*pipeline.Request, len(strict))
	strictOutputs := make([]*rootAnchorOutput, len(strict))
	for i, c := range strict {
		strictReqs[i], strictOutputs[i] = capture(c.build)
	}
	seal, err := servingdns.Component[*recursiveRootSeal](boundReq, recursiveRootComponent)
	if err != nil {
		t.Fatal(err)
	}
	var release func()
	awaitServingRuntime(t, f.s, func() bool {
		_, held, err := f.s.publisher.Pin(ctx)
		if err != nil {
			return false
		}
		release = held
		return true
	})
	defer release()
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.opts.Store.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return stageCommandLabel(ctx, tx, auth.Principal{})
	}); err != nil {
		t.Fatal(err)
	}
	if seal.current() || !seal.servedCurrent() {
		t.Fatal("the commit did not leave an unpublished successor behind the served publication")
	}
	if err := bound.BindResponse(&pipeline.Response{Msg: refusal(boundReq)}); err != nil {
		t.Fatal(err)
	}
	if !bound.servedDNS || bound.retainedDNS {
		t.Fatal("a bound TSIG refusal is not in the served class", bound.servedDNS, bound.retainedDNS)
	}
	if !emitted(bound) {
		t.Fatal("a bound TSIG refusal of the served publication was withheld after an unpublished commit")
	}
	if emitted(unbound) {
		t.Fatal("an unbound output was admitted without full data currency")
	}
	for i, c := range strict {
		output := strictOutputs[i]
		if err := output.BindResponse(&pipeline.Response{Msg: refusal(strictReqs[i])}); err != nil {
			t.Fatal(c.name, err)
		}
		if output.servedDNS || output.retainedDNS {
			t.Fatal("a bound TSIG refusal of", c.name, "left full data currency", output.servedDNS, output.retainedDNS)
		}
		if emitted(output) {
			t.Fatal("a bound TSIG refusal of", c.name, "was sent after an unpublished commit")
		}
	}
}
