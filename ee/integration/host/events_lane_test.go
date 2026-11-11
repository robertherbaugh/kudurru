package host

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deliveryFixture is engineFixture with a ledger that can delete, as root's
// forget callback does.
func deliveryFixture(t *testing.T) (*Host, *job, *memory.Store, *time.Time) {
	t.Helper()
	h, j, _, st, now := engineFixture(t)
	r, err := ledger.New(ledger.Options{Reader: st, Now: func() time.Time { return *now }, Write: func(ctx context.Context, g ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		var out store.Object
		err := h.WithLease(ctx, g, func(ctx context.Context) error { var err error; out, err = st.Apply(ctx, o, a); return err })
		return out, err
	}, Delete: func(ctx context.Context, g ledger.Guard, requests []store.DeleteRequest) error {
		return h.WithLease(ctx, g, func(ctx context.Context) error { return st.DeleteMany(ctx, requests) })
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.Ledger = r
	return h, j, st, now
}

func queueOne(t *testing.T, h *Host, j *job, now time.Time, name string, p *fakeHook) string {
	t.Helper()
	j.provider = p
	id := kinds.Digest([]byte(name))
	event := integration.Event{Topic: "zone.changed", Time: now, Payload: json.RawMessage(`{"zone":"example.com"}`)}
	// A local refusal of the first attempt is returned after the durable
	// queue; the row itself must exist either way.
	a := h.execute(context.Background(), j, command{kind: "deliver", expected: id, event: &event})
	if _, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id); err != nil {
		t.Fatal(a.err, err)
	}
	if a.err != nil && !localRefusal(context.Background(), a.err) {
		t.Fatal(a.err)
	}
	return id
}

func TestEventAttemptClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		saved bool // the attempt is recorded
		dead  bool
		delay time.Duration // minimum next attempt
	}{
		// A slow peer: the provider maps its own attempt timeout to this.
		{name: "attempt_timeout", err: &Failure{Kind: FailureTemporary}, saved: true, delay: time.Second},
		// An exhausted request budget that reached the host unmapped.
		{name: "budget", err: ledger.ErrBudget, saved: true, delay: 5 * time.Minute},
		{name: "rate_limited", err: &Failure{Kind: FailureRateLimited, RetryAfter: 5 * time.Minute}, saved: true, delay: 5 * time.Minute},
		// Local refusals decide nothing about the event.
		{name: "fenced", err: ErrFenced},
		{name: "unavailable", err: ErrUnavailable},
		{name: "caller_canceled", err: context.Canceled},
		{name: "caller_deadline", err: context.DeadlineExceeded},
		// A 4xx answer drops the event.
		{name: "permanent", err: &Failure{Kind: FailurePermanent}, saved: true, dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, j, _, now := deliveryFixture(t)
			p := &fakeHook{handle: func(context.Context, string, integration.Event) error { return tc.err }}
			id := queueOne(t, h, j, *now, "event", p)
			d, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id)
			if err != nil || p.calls != 1 {
				t.Fatal(d, err, p.calls)
			}
			if !tc.saved {
				if d.Spec.Attempts != 0 || d.Spec.DeadLetter || d.Spec.ErrorClass != "" {
					t.Fatalf("a local refusal changed the row: %+v", d.Spec)
				}
				return
			}
			if d.Spec.Attempts != 1 || d.Spec.DeadLetter != tc.dead || d.Spec.Acknowledged {
				t.Fatalf("row: %+v", d.Spec)
			}
			if !tc.dead && d.Spec.NextAttempt.Before(now.Add(tc.delay)) {
				t.Fatalf("retried too early: %s", d.Spec.NextAttempt.Sub(*now))
			}
			statuses := h.Statuses()
			degraded := len(statuses) == 1 && statuses[0].Phase == "Degraded" && statuses[0].Reason == ReasonEventDeadLettered
			if degraded != tc.dead {
				t.Fatalf("dead letter status: %+v", statuses)
			}
		})
	}
}

func TestEventExpiryRaisesDegraded(t *testing.T) {
	h, j, _, now := deliveryFixture(t)
	p := &fakeHook{handle: func(context.Context, string, integration.Event) error { return &Failure{Kind: FailureTemporary} }}
	id := queueOne(t, h, j, *now, "event", p)
	*now = now.Add(24 * time.Hour)
	if err := h.drainEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	d, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id)
	if err != nil || !d.Spec.DeadLetter || d.Spec.ErrorClass != "expired" || p.calls != 1 {
		t.Fatal(d, err)
	}
	if s := h.Statuses(); len(s) != 1 || s[0].Reason != ReasonEventDeadLettered {
		t.Fatalf("expiry status: %+v", s)
	}
}

func TestEventRetentionPrunesOnlyDeadRows(t *testing.T) {
	h, j, st, now := deliveryFixture(t)
	start := *now
	ok := &fakeHook{handle: func(context.Context, string, integration.Event) error { return nil }}
	acked := queueOne(t, h, j, start, "acked", ok)
	dead := queueOne(t, h, j, start, "dead", &fakeHook{handle: func(context.Context, string, integration.Event) error { return &Failure{Kind: FailurePermanent} }})
	// Rows of other owners: one whose Integration is gone, one still live.
	gone := kinds.Owner{ClusterID: j.guard.Owner.ClusterID, Namespace: "dns", Name: "gone", UID: "uid-gone"}
	liveOwner := kinds.Owner{ClusterID: j.guard.Owner.ClusterID, Namespace: "dns", Name: "live", UID: "uid-live"}
	for _, owner := range []kinds.Owner{gone, liveOwner} {
		id := kinds.Digest([]byte(owner.Name))
		row := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(owner, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: kinds.FormatVersion, Owner: owner, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: start, Deadline: start.Add(24 * time.Hour), NextAttempt: start, Acknowledged: true}}
		if _, err := st.Apply(context.Background(), row, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	live := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "dns", UID: "uid-live"}, Spec: public.IntegrationSpec{Type: "webhook", Webhook: &public.WebhookIntegration{Endpoint: "https://live.example"}}}
	if _, err := st.Apply(context.Background(), live, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	var stored public.Integration
	if err := st.Get(context.Background(), store.KeyOf(live), &stored); err != nil {
		t.Fatal(err)
	}
	// The capture of this fixture does not contain "live", so the host
	// takes its row for an orphan; the ledger's own read of the Integration
	// keeps it.
	if string(stored.UID) != liveOwner.UID {
		t.Fatal("fixture UID changed", stored.UID)
	}
	// A pending row of this instance, created later, is never pruned.
	*now = start.Add(time.Hour)
	pending := queueOne(t, h, j, *now, "pending", &fakeHook{handle: func(context.Context, string, integration.Event) error { return &Failure{Kind: FailureTemporary} }})
	count := func() int {
		page, err := st.List(context.Background(), kinds.KindIntegrationDelivery, store.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Items)
	}
	// Inside the retention window nothing goes.
	*now = start.Add(24*time.Hour + 30*time.Minute)
	j.provider = ok
	if err := h.drainEvents(context.Background(), j); err != nil || count() != 5 {
		t.Fatal("pruned inside the retention window", err, count())
	}
	*now = start.Add(25 * time.Hour)
	if err := h.drainEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{acked, dead} {
		if _, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("terminal row kept past retention", err)
		}
	}
	if d, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, pending); err != nil || !d.Spec.Acknowledged {
		t.Fatal("pending row pruned or not delivered", d, err)
	}
	if _, err := h.opts.Ledger.ReadDelivery(context.Background(), gone, kinds.Digest([]byte("gone"))); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("orphan row kept", err)
	}
	if _, err := h.opts.Ledger.ReadDelivery(context.Background(), liveOwner, kinds.Digest([]byte("live"))); err != nil {
		t.Fatal("a live owner's row was pruned by another instance", err)
	}
}
