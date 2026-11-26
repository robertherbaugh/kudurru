package main

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A zone transfer answer, or its refusal, of the served publication is sent
// across a commit that changes nothing it is computed from: here the status
// projection of the primary zone, the write that, with the NOTIFY
// acknowledgment and the receipt realignment, follows every transfer and
// NOTIFY exchange (D419, #236). Before, such a commit withheld every transfer
// answer, a REFUSED or NOTAUTH included, until the node had published again,
// and the secondary saw the connection close or time out. Any other commit
// still withholds it at once, as does a superseded dependency; other strict
// outputs (here a NOTIFY response) keep exact data currency. The test holds
// the publisher's barrier, so the publication cannot be replaced meanwhile.
func TestRootAnchorTransferAcrossTransferNeutralCommits(t *testing.T) {
	f := newServingCommandFixture(t, func(s *servingRuntime) {
		if err := s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "xfr"}, Spec: public.DNSZoneSpec{ZoneName: "xfr.example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.xfr.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}}, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	st := f.s.opts.Store
	writer, err := st.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(t *testing.T, fn func(context.Context, *sqlite.CommandTx) error) {
		t.Helper()
		if err := st.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), fn); err != nil {
			t.Fatal(err)
		}
	}
	request := func(lease *serving.Lease, qtype uint16, opcode int) *pipeline.Request {
		q := new(dns.Msg)
		q.SetQuestion("xfr.example.", qtype)
		q.Opcode = opcode
		return &pipeline.Request{Ctx: ctx, Msg: q, Serving: servingdns.Wrap(lease)}
	}
	capture := func(t *testing.T, req *pipeline.Request) *rootAnchorOutput {
		t.Helper()
		output, err := captureRootAnchorOutput(req)
		if err != nil {
			t.Fatal(err)
		}
		return output.(*rootAnchorOutput)
	}
	// write is one message's transport handoff: the first message of an
	// answer after its binding, or a continuation message after Next.
	write := func(t *testing.T, output *rootAnchorOutput) bool {
		t.Helper()
		called := false
		err := output.Emit(func() error { called = true; return nil })
		if called != (err == nil) {
			t.Fatal("output ran its write but reported a refusal, or the reverse", err)
		}
		return called
	}
	emit := func(t *testing.T, req *pipeline.Request, output *rootAnchorOutput) bool {
		t.Helper()
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		m.Authoritative = true
		if err := output.BindResponse(&pipeline.Response{Msg: m, Origin: pipeline.OriginAuthoritative, Status: pipeline.StatusAuthoritative}); err != nil {
			t.Fatal(err)
		}
		return write(t, output)
	}

	// Wait until the zone's head is published, then hold the barrier on a
	// publication that is still the latest commit.
	var (
		lease   *serving.Lease
		seal    *recursiveRootSeal
		release func()
	)
	awaitServingRuntime(t, f.s, func() bool {
		published := false
		for _, z := range f.s.primary.driver.Status().Zones {
			published = published || (z.ObjectName == "xfr" && z.Published)
		}
		if !published {
			return false
		}
		l, err := f.s.publisher.CaptureHTTP(ctx)
		if err != nil {
			return false
		}
		s, err := servingdns.Component[*recursiveRootSeal](request(l, dns.TypeA, dns.OpcodeQuery), recursiveRootComponent)
		if err == nil && s.current() {
			if _, held, err := f.s.publisher.Pin(ctx); err == nil {
				if s.current() {
					lease, seal, release = l, s, held
					return true
				}
				held()
			}
		}
		l.Release()
		return false
	})
	defer lease.Release()
	defer release()

	axfr := capture(t, request(lease, dns.TypeAXFR, dns.OpcodeQuery))
	ixfr := capture(t, request(lease, dns.TypeIXFR, dns.OpcodeQuery))
	later := capture(t, request(lease, dns.TypeAXFR, dns.OpcodeQuery))
	if !axfr.transfer || !ixfr.transfer || axfr.query {
		t.Fatal("a transfer was not classified as one")
	}
	commit(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var z public.DNSZone
		if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: "xfr"}, &z); err != nil {
			return err
		}
		z.Status.RecordCount += 100
		return tx.UpdateStatus(ctx, &z)
	})
	if seal.current() || !seal.transferCurrent() {
		t.Fatal("the status projection did not leave a transfer-current publication behind the latest commit")
	}
	for name, output := range map[string]*rootAnchorOutput{"AXFR": axfr, "IXFR": ixfr, "AXFR captured after": capture(t, request(lease, dns.TypeAXFR, dns.OpcodeQuery))} {
		t.Run("status projection "+name, func(t *testing.T) {
			req := request(lease, dns.TypeAXFR, dns.OpcodeQuery)
			if !emit(t, req, output) {
				t.Fatal("a transfer answer was withheld after a transfer-neutral commit")
			}
			if err := output.Next(); err != nil {
				t.Fatal("a transfer continuation was refused after a transfer-neutral commit", err)
			}
			if !write(t, output) {
				t.Fatal("a transfer continuation message was withheld after a transfer-neutral commit")
			}
		})
	}
	notify := request(lease, dns.TypeSOA, dns.OpcodeNotify)
	if emit(t, notify, capture(t, notify)) {
		t.Fatal("a strict output other than a transfer was emitted after an unpublished commit")
	}

	// A commit that is not transfer-neutral retires the transfer answers.
	commit(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return stageCommandLabel(ctx, tx, auth.Principal{})
	})
	if seal.transferCurrent() {
		t.Fatal("a transfer stayed current across a commit that is not transfer-neutral")
	}
	req := request(lease, dns.TypeAXFR, dns.OpcodeQuery)
	if emit(t, req, later) || emit(t, req, capture(t, req)) {
		t.Fatal("a transfer answer was emitted after a commit that is not transfer-neutral")
	}
	if err := axfr.Next(); err == nil {
		t.Fatal("a transfer continued after a commit that is not transfer-neutral")
	}
}
