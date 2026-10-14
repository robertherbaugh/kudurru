package currentguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestNilMissingGenerationAndPinMisuseFailClosed(t *testing.T) {
	f := newSQLiteFixture(t)
	for _, pin := range []PinFunc{
		nil,
		func(context.Context, pr.PublicationFence, func(Pinned) error) error { return nil },
		func(_ context.Context, _ pr.PublicationFence, fn func(Pinned) error) error {
			return fn(Pinned{Identity: "missing-keys"})
		},
		func(_ context.Context, _ pr.PublicationFence, fn func(Pinned) error) error {
			return fn(Pinned{Keys: testRing(t, "test")})
		},
		func(_ context.Context, _ pr.PublicationFence, fn func(Pinned) error) error {
			p := f.h.current
			p.Current = nil
			return fn(p)
		},
		func(_ context.Context, _ pr.PublicationFence, fn func(Pinned) error) error {
			p := f.h.current
			p.Current = func() bool { return false }
			return fn(p)
		},
		func(_ context.Context, _ pr.PublicationFence, fn func(Pinned) error) error {
			if err := fn(f.h.current); err != nil {
				return err
			}
			_ = fn(f.h.current) // a broken root implementation cannot call twice
			return nil
		},
	} {
		o, _ := New(Options{Pin: pin})
		if _, err := o.CapturePolicy(f.ctx(), testSource()); !errors.Is(err, pr.ErrUnavailable) {
			t.Fatal("missing/invalid pin admitted capture", err)
		}
	}
	if _, err := f.g.CapturePolicy(t.Context(), testSource()); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("missing writer fence admitted capture", err)
	}
	errWant := errors.New("test callback error")
	if err := f.g.Background().WithCurrent(f.ctx(), f.fence, func(sqliteadapter.BackgroundPolicy) error { return errWant }); err != errWant {
		t.Fatal("pin changed callback error", err)
	}
	ctx, cancel := context.WithCancel(f.ctx())
	cancel()
	if _, err := f.g.CapturePolicy(ctx, testSource()); !errors.Is(err, context.Canceled) {
		t.Fatal("inactive source request accepted", err)
	}
}

func TestActualCurrentMetadataDomainAndHARejectionCannotMutateHead(t *testing.T) {
	f := newSQLiteFixture(t)
	l := f.active("primary", "example.")
	before := f.image()
	for _, mutate := range []func(*pr.ProofIdentity){
		func(p *pr.ProofIdentity) { p.ProviderVersion = "changed-provider" },
		func(p *pr.ProofIdentity) { p.Principal.Version = "changed-credential" },
		func(p *pr.ProofIdentity) { p.Principal.Key = "KEY." },
		func(p *pr.ProofIdentity) { p.Algorithm = dns.HmacSHA512 },
		func(p *pr.ProofIdentity) { p.Transport = "doh" },
		func(p *pr.ProofIdentity) { p.Domain = "dns-update:other" },
		func(p *pr.ProofIdentity) { p.Subject = "other." },
		func(p *pr.ProofIdentity) { p.NotAfter = time.Time{} },
		func(p *pr.ProofIdentity) { p.NotAfter = time.Now().Add(-time.Second) },
	} {
		request := f.request(l, "identity", "dynamic.example. 60 IN A 192.0.2.1")
		mutate(&request.Proof.(*testIssued).identity)
		if out := f.r.Apply(f.ctx(), request); out.Rcode == dns.RcodeSuccess || out.Committed || string(before) != string(f.image()) {
			t.Fatal("mismatched verification capture admitted", out)
		}
	}
	// Explicit test-only permissive pin demonstrates the actual tx, rather
	// than our test holder's preflight equality check, is the HA authority.
	f.g.options.Pin = func(_ context.Context, _ pr.PublicationFence, fn func(Pinned) error) error { return fn(f.h.current) }
	for _, mutate := range []func(*pr.ProofIdentity){
		func(p *pr.ProofIdentity) { p.AuthorityIncarnation++ },
		func(p *pr.ProofIdentity) { p.Term.PromotionID = "different-promotion" },
	} {
		request := f.request(l, "authority", "dynamic.example. 60 IN A 192.0.2.1")
		mutate(&request.Proof.(*testIssued).identity)
		if out := f.r.Apply(f.ctx(), request); out.Rcode == dns.RcodeSuccess || out.Committed || string(before) != string(f.image()) {
			t.Fatal("non-current actual transaction writer admitted", out)
		}
	}
	v := f.view(l)
	err := f.g.Background().WithCurrent(f.ctx(), f.fence, func(check sqliteadapter.BackgroundPolicy) error {
		return f.s.Command(f.ctx(), f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return check(ctx, tx, sqliteadapter.BackgroundCheck{Lifecycle: l, Policy: &v.Aggregate.Policy, Reason: "unknown"})
		})
	})
	if !errors.Is(err, pr.ErrUnavailable) || string(before) != string(f.image()) {
		t.Fatal("unknown background capability admitted", err)
	}
}
