package primaryruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

type boundNotifyKey struct{}
type bindingNotify struct {
	fail                 error
	noContext, noRelease bool
	released, sent       int
}

func (n *bindingNotify) Bind(ctx context.Context, _ Member, _ *zone.Zone) (context.Context, func(), error) {
	release := func() { n.released++ }
	if n.noRelease {
		release = nil
	}
	if n.noContext {
		return nil, release, n.fail
	}
	return context.WithValue(ctx, boundNotifyKey{}, n), release, n.fail
}
func (n *bindingNotify) Send(ctx context.Context, _ Member, _ *zone.Zone) error {
	if ctx.Value(boundNotifyKey{}) != n {
		return ErrUnavailable
	}
	n.sent++
	return nil
}

func TestNotifyBinderPreservesCapabilityThroughAdmissionSendAndAck(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := t.Context()
	l := h.begin(h.zone("primary", "example."))
	_, err := h.r.Reconcile(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	h.r.o.Proofs = testIssuer{}
	if out := h.r.Apply(ctx, h.request(l, "notify-binding", "dynamic.example. 60 IN A 192.0.2.1", false)); out.Err != nil {
		t.Fatal(out)
	}
	n := &bindingNotify{}
	h.r.o.Notify = n
	admit := h.r.o.Admit
	var reasons []string
	h.r.o.Admit = func(ctx context.Context, c Command) (*zonestate.View, error) {
		if c.Reason == "notify-delivery" || c.Reason == "notify-ack" {
			if ctx.Value(boundNotifyKey{}) != n {
				return nil, ErrUnavailable
			}
			reasons = append(reasons, c.Reason)
		}
		return admit(ctx, c)
	}
	if err := h.r.NotifyOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n.sent != 1 || n.released != 1 || len(reasons) != 2 || reasons[0] != "notify-delivery" || reasons[1] != "notify-ack" {
		t.Fatal(n, reasons)
	}
}

func TestNotifyBinderFailureReleasesAndNeverAdmitsDelivery(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		fail                 error
		noContext, noRelease bool
	}{
		{"capture-error", ErrUnavailable, false, false}, {"nil-context", nil, true, false}, {"nil-release", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "sqlite")
			ctx := t.Context()
			l := h.begin(h.zone("primary", "example."))
			_, err := h.r.Reconcile(ctx, l.Spec.Zone)
			if err != nil {
				t.Fatal(err)
			}
			h.r.o.Proofs = testIssuer{}
			if out := h.r.Apply(ctx, h.request(l, "binding-fail", "dynamic.example. 60 IN A 192.0.2.1", false)); out.Err != nil {
				t.Fatal(out)
			}
			n := &bindingNotify{fail: tc.fail, noContext: tc.noContext, noRelease: tc.noRelease}
			h.r.o.Notify = n
			before := h.admissions.Load()
			if err := h.r.NotifyOnce(ctx); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			if n.sent != 0 || h.admissions.Load() != before || (!tc.noRelease && n.released != 1) {
				t.Fatal("binding failure admitted or leaked", n)
			}
		})
	}
}
