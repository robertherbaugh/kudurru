package primaryruntime

import (
	"context"
	"testing"

	"github.com/miekg/dns"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

type includingCall struct {
	member    Member
	receipt   string
	committed uint64
}

// supersede makes the publication of the first UPDATE commit lose to a later
// one: while it is being published, another UPDATE of the zone commits,
// installs and is published, and the first publication then reports that the
// served member is newer than its own (as the complete publisher's exact
// check does).
func supersede(h *harness, l *zk.ZoneLifecycle, ctx context.Context) {
	var first *Member
	h.r.o.Publish = func(_ context.Context, _ Cut, m Member) error {
		if first == nil && m.Version.CommitSequence > 0 {
			first = &m
			if out := h.r.Apply(ctx, h.request(l, "later", "later.example. 60 IN A 192.0.2.2", false)); out.Rcode != dns.RcodeSuccess {
				h.t.Error("later UPDATE", out)
			}
			return ErrConflict
		}
		return nil
	}
}

// A committed UPDATE whose own publication was superseded by a later commit
// of the zone is answered NOERROR once a later published head carries its
// receipt (D409): publishUpdate asks PublishIncluding with the exact member
// it committed, its receipt ID and its commit sequence. Without that hook,
// or when no later head carries the receipt, the answer stays SERVFAIL with
// the commit marked as a publication failure, as before.
func TestUpdateSupersededByLaterCommitIsAcknowledgedByItsSuccessor(t *testing.T) {
	for _, mode := range []string{"accepted", "refused", "absent"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, "sqlite")
			ctx := context.Background()
			l := h.begin(h.zone("primary", "example."))
			if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			h.r.o.Proofs = testIssuer{}
			var calls []includingCall
			switch mode {
			case "accepted", "refused":
				h.r.o.PublishIncluding = func(_ context.Context, m Member, receipt string, committed uint64) error {
					calls = append(calls, includingCall{m, receipt, committed})
					if mode == "refused" {
						return ErrConflict
					}
					return nil
				}
			}
			supersede(h, l, ctx)
			out := h.r.Apply(ctx, h.request(l, "first", "first.example. 60 IN A 192.0.2.1", false))
			v, err := h.repo.Read(ctx, l.Spec.HeadZone())
			if err != nil {
				t.Fatal(err)
			}
			var mine zonestate.Receipt
			for _, r := range v.Aggregate.Receipts {
				if r.CommitSequence == out.Version.CommitSequence {
					mine = r
				}
			}
			if !out.Committed || mine.ID == "" || v.Head.Spec.CommitSequence != out.Version.CommitSequence+1 {
				t.Fatalf("first UPDATE did not commit before the later one: %+v", out)
			}
			switch mode {
			case "accepted":
				if out.Rcode != dns.RcodeSuccess || !out.Published || out.PublishFailed {
					t.Fatalf("superseded commit not acknowledged by its successor: %+v", out)
				}
			default:
				if out.Rcode != dns.RcodeServerFailure || out.Published || !out.PublishFailed {
					t.Fatalf("superseded commit acknowledged without a successor that carries it: %+v", out)
				}
			}
			if mode == "absent" {
				return
			}
			if len(calls) != 1 || calls[0].receipt != mine.ID || calls[0].committed != out.Version.CommitSequence || calls[0].member.Version != out.Version || calls[0].member.Incarnation != l.Spec.Incarnation {
				t.Fatalf("successor acknowledgment asked for %+v, want receipt %s of commit %d", calls, mine.ID, out.Version.CommitSequence)
			}
		})
	}
}

// The recovery publication of an earlier commit (nothing of this request is
// committed yet) never accepts a successor: only its exact member may.
func TestUpdateRecoveryPublicationNeedsTheExactMember(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	l := h.begin(h.zone("primary", "example."))
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	h.r.o.Proofs = testIssuer{}
	called := false
	h.r.o.PublishIncluding = func(context.Context, Member, string, uint64) error { called = true; return nil }
	v, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	h.publishFail = true
	if out := h.r.publishUpdate(ctx, l, v, nil); out.Rcode != dns.RcodeServerFailure || !out.PublishFailed || called || out.Err == nil {
		t.Fatalf("recovery publication: %+v, successor asked %v", out, called)
	}
}

// An UPDATE that finds the zone's head not yet published runs its recovery
// publication first; when that fails because a concurrent commit moved the
// head meanwhile, nothing of the request is committed and it plans again
// against the new head instead of answering SERVFAIL (D409). When the head
// did not move, the recovery's failure is still the answer.
func TestUpdateRecoveryReplansWhenTheHeadMovedOn(t *testing.T) {
	for _, moved := range []bool{true, false} {
		t.Run(map[bool]string{true: "moved", false: "unchanged"}[moved], func(t *testing.T) {
			h := newHarness(t, "sqlite")
			ctx := context.Background()
			l := h.begin(h.zone("primary", "example."))
			if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			h.r.o.Proofs = testIssuer{}
			h.publishFail = true
			if out := h.r.Apply(ctx, h.request(l, "unpublished", "a.example. 60 IN A 192.0.2.1", false)); !out.Committed || out.Published {
				t.Fatal("first UPDATE", out)
			}
			h.publishFail = false
			recovered := false
			h.r.o.Publish = func(_ context.Context, _ Cut, m Member) error {
				if !recovered {
					recovered = true
					if moved {
						if out := h.r.Apply(ctx, h.request(l, "concurrent", "c.example. 60 IN A 192.0.2.3", false)); out.Rcode != dns.RcodeSuccess {
							t.Error("concurrent UPDATE", out)
						}
					}
					return ErrUnavailable
				}
				return nil
			}
			out := h.r.Apply(ctx, h.request(l, "second", "b.example. 60 IN A 192.0.2.2", false))
			if !recovered {
				t.Fatal("recovery publication did not run")
			}
			if moved && (out.Rcode != dns.RcodeSuccess || !out.Committed || !out.Published) {
				t.Fatalf("UPDATE after an obsolete recovery: %+v", out)
			}
			if !moved && (out.Rcode != dns.RcodeServerFailure || out.Committed) {
				t.Fatalf("failed recovery of the current head: %+v", out)
			}
		})
	}
}
