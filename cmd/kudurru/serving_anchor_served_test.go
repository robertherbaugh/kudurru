package main

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// An ordinary answer from local, authoritative or mixed data is bound to the
// publication that is served, not to the latest commit (D378). While a
// canonical commit is not yet published, a lease of the served publication
// still answers, captured before or after the commit; it never continues a
// transfer, and a transfer or a superseded publication stays strict. Once the
// node has published the commit, an answer of the old lease is refused. The
// test holds the publisher's barrier across the commit, so the publication
// cannot be replaced while the first half runs.
func TestRootAnchorServedDNSFollowsThePublication(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	query := func(qtype uint16) *pipeline.Request {
		q := new(dns.Msg)
		q.SetQuestion("served.example.", qtype)
		return &pipeline.Request{Ctx: ctx, Msg: q, Serving: servingdns.Wrap(lease)}
	}
	seal, err := servingdns.Component[*recursiveRootSeal](query(dns.TypeA), recursiveRootComponent)
	if err != nil {
		t.Fatal(err)
	}
	local := func(req *pipeline.Request, origin pipeline.ResponseOrigin, status pipeline.Status) *pipeline.Response {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		m.Authoritative = origin == pipeline.OriginAuthoritative
		return &pipeline.Response{Msg: m, Origin: origin, Status: status}
	}
	emit := func(t *testing.T, _ *pipeline.Request, output *rootAnchorOutput, response *pipeline.Response) bool {
		t.Helper()
		if err := output.BindResponse(response); err != nil {
			t.Fatal(err)
		}
		called := false
		err := output.Emit(func() error { called = true; return nil })
		if called != (err == nil) {
			t.Fatal("output ran its write but reported a refusal, or the reverse", err)
		}
		return called
	}
	capture := func(t *testing.T, req *pipeline.Request) *rootAnchorOutput {
		t.Helper()
		output, err := captureRootAnchorOutput(req)
		if err != nil {
			t.Fatal(err)
		}
		return output.(*rootAnchorOutput)
	}
	cases := []struct {
		name   string
		origin pipeline.ResponseOrigin
		status pipeline.Status
	}{
		{"local", pipeline.OriginLocal, pipeline.StatusLocalRecord},
		{"authoritative", pipeline.OriginAuthoritative, pipeline.StatusAuthoritative},
		{"mixed", pipeline.OriginMixed, pipeline.StatusForwarded},
	}
	before := make([]*rootAnchorOutput, len(cases))
	for i := range cases {
		before[i] = capture(t, query(dns.TypeA))
	}
	beforeLater := capture(t, query(dns.TypeA))

	var release func()
	awaitServingRuntime(t, f.s, func() bool {
		_, held, err := f.s.publisher.Pin(ctx)
		if err != nil {
			return false
		}
		release = held
		return true
	})
	pinned := true
	defer func() {
		if pinned {
			release()
		}
	}()
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.opts.Store.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return stageCommandLabel(ctx, tx, auth.Principal{})
	}); err != nil {
		t.Fatal(err)
	}
	if seal.current() || !seal.servedCurrent() || !f.s.publisher.Serves(recursiveRootComponent, seal) {
		t.Fatal("the commit did not leave an unpublished successor behind the served publication")
	}
	for i, tc := range cases {
		t.Run("unpublished commit "+tc.name, func(t *testing.T) {
			for _, output := range []*rootAnchorOutput{before[i], capture(t, query(dns.TypeA))} {
				req := query(dns.TypeA)
				if !emit(t, req, output, local(req, tc.origin, tc.status)) {
					t.Fatal("an answer of the served publication was withheld after an unpublished commit")
				}
				if err := output.Next(); err == nil {
					t.Fatal("a served answer authorized a transfer continuation")
				}
			}
		})
	}
	t.Run("unpublished commit transfer", func(t *testing.T) {
		req := query(dns.TypeAXFR)
		output := capture(t, req)
		if output.query {
			t.Fatal("a transfer was classified as an ordinary query")
		}
		if emit(t, req, output, local(req, pipeline.OriginAuthoritative, pipeline.StatusAuthoritative)) {
			t.Fatal("a transfer of the served publication was emitted after an unpublished commit")
		}
	})

	pinned = false
	release()
	awaitServingRuntime(t, f.s, func() bool {
		return f.s.publisher.Status().ProtectedReady && !f.s.publisher.Serves(recursiveRootComponent, seal)
	})
	if seal.servedCurrent() || !seal.dnsCurrent() {
		t.Fatal("a superseded publication still counts as served, or lost its DNS dependencies")
	}
	req := query(dns.TypeA)
	if emit(t, req, beforeLater, local(req, pipeline.OriginLocal, pipeline.StatusLocalRecord)) {
		t.Fatal("a local answer of a superseded publication was emitted after the node published its successor")
	}
}

// DNS answers of the served publication do not depend on the provider
// resource set, which only HTTP authentication uses (D378): the node replaces
// that set on its own schedule (its resource worker, provider authority
// admission, a reset), and in a mounted apply of 120 documents every such
// replacement withheld local answers until the next publication, for up to
// seven seconds. The publication's complete dependency check, which HTTP
// requests, acknowledgments and transfers use, still retires at once. A
// replaced DNS keyring, which TSIG on DNS uses, still withholds answers.
func TestRootAnchorServedDNSOutlivesAProviderReplacement(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var release func()
	awaitServingRuntime(t, f.s, func() bool {
		_, held, err := f.s.publisher.Pin(ctx)
		if err != nil {
			return false
		}
		release = held
		return true
	})
	pinned := true
	defer func() {
		if pinned {
			release()
		}
	}()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	query := func() *pipeline.Request {
		q := new(dns.Msg)
		q.SetQuestion("served.example.", dns.TypeA)
		return &pipeline.Request{Ctx: ctx, Msg: q, Serving: servingdns.Wrap(lease)}
	}
	seal, err := servingdns.Component[*recursiveRootSeal](query(), recursiveRootComponent)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := servingdns.Component[*servingbuild.RuntimeOwner](query(), "runtime")
	if err != nil {
		t.Fatal(err)
	}
	answer := func(t *testing.T) bool {
		t.Helper()
		req := query()
		captured, err := captureRootAnchorOutput(req)
		if err != nil {
			return false
		}
		output := captured.(*rootAnchorOutput)
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		if err := output.BindResponse(&pipeline.Response{Msg: m, Origin: pipeline.OriginLocal, Status: pipeline.StatusLocalRecord}); err != nil {
			t.Fatal(err)
		}
		return output.Emit(func() error { return nil }) == nil
	}
	if !answer(t) || !owner.DependenciesCurrent() {
		t.Fatal("the served publication did not answer before any replacement")
	}

	if err := f.s.resources.WithReplacement(ctx, func(r *providerresources.Replacement) error {
		r.Retire()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if owner.DependenciesCurrent() {
		t.Fatal("the fixture replacement did not retire the publication's provider resource set")
	}
	if !owner.DNSDependenciesCurrent() || !seal.dnsCurrent() || !answer(t) {
		t.Fatal("a replaced provider resource set withheld a DNS answer of the served publication")
	}

	if err := f.s.opts.Keyrings.Replace(ctx, f.s.opts.Keyrings.Capture().Keys()); err != nil {
		t.Fatal(err)
	}
	if owner.DNSDependenciesCurrent() || answer(t) {
		t.Fatal("a replaced DNS keyring did not withhold the served publication's answers")
	}
	pinned = false
	release()
}
