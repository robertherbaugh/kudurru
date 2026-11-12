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
	// Rows of other owners: an undelivered one whose Integration is gone,
	// and an acknowledged and an undelivered one of a live Integration
	// that runs no hook lease (D379).
	gone := kinds.Owner{ClusterID: j.guard.Owner.ClusterID, Namespace: "dns", Name: "gone", UID: "uid-gone"}
	liveOwner := kinds.Owner{ClusterID: j.guard.Owner.ClusterID, Namespace: "dns", Name: "live", UID: "uid-live"}
	for _, row := range []struct {
		owner kinds.Owner
		name  string
		acked bool
	}{{gone, "gone", false}, {liveOwner, "live", true}, {liveOwner, "live-pending", false}} {
		id := kinds.Digest([]byte(row.name))
		d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(row.owner, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: kinds.FormatVersion, Owner: row.owner, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: start, Deadline: start.Add(24 * time.Hour), NextAttempt: start, Acknowledged: row.acked}}
		if _, err := st.Apply(context.Background(), d, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
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
	// "live" exists in the store; that no longer keeps its expired rows.
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
	if err := h.drainEvents(context.Background(), j); err != nil || count() != 6 {
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
	if _, err := h.opts.Ledger.ReadDelivery(context.Background(), liveOwner, kinds.Digest([]byte("live"))); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a live owner's expired terminal row was kept", err)
	}
	// An owner that runs any lease dead-letters its own rows at the
	// deadline, so an undelivered row an hour past it belongs to an owner
	// that runs none; another lease prunes it (D380).
	if _, err := h.opts.Ledger.ReadDelivery(context.Background(), liveOwner, kinds.Digest([]byte("live-pending"))); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a live owner's expired undelivered row was kept", err)
	}
}

// The event cursor of an instance that no longer exists (deleted under the
// Retain policy) is pruned by another hook lease; the cursor of a live
// Integration and the lease's own cursor stay (D379).
func TestEventRetentionPrunesOrphanedCursors(t *testing.T) {
	h, j, st, now := deliveryFixture(t)
	j.provider = &fakeHook{handle: func(context.Context, string, integration.Event) error { return nil }}
	gone := kinds.Owner{ClusterID: j.guard.Owner.ClusterID, Namespace: "dns", Name: "gone", UID: "uid-gone"}
	liveOwner := kinds.Owner{ClusterID: j.guard.Owner.ClusterID, Namespace: "dns", Name: "live", UID: "uid-live"}
	for _, owner := range []kinds.Owner{gone, liveOwner, j.guard.Owner} {
		c := &kinds.IntegrationEventCursor{ObjectMeta: metav1.ObjectMeta{Name: kinds.EventCursorName(owner)}, Spec: kinds.IntegrationEventCursorSpec{FormatVersion: kinds.FormatVersion, Owner: owner, Positions: map[string]string{"feed": "1"}, UpdatedAt: *now}}
		if _, err := st.Apply(context.Background(), c, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	live := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "dns", UID: "uid-live"}, Spec: public.IntegrationSpec{Type: "webhook", Webhook: &public.WebhookIntegration{Endpoint: "https://live.example"}}}
	if _, err := st.Apply(context.Background(), live, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.drainEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if _, err := h.opts.Ledger.ReadEventCursor(context.Background(), gone); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("orphaned cursor kept", err)
	}
	for _, owner := range []kinds.Owner{liveOwner, j.guard.Owner} {
		if _, err := h.opts.Ledger.ReadEventCursor(context.Background(), owner); err != nil {
			t.Fatal("a live cursor was pruned", owner.Name, err)
		}
	}
}

// A lease without the hook role, here an instance whose peer stopped
// advertising hooks, still runs retention (D380): it prunes another
// instance's expired rows and dead-letters its own undelivered rows once
// their day ran out, without any delivery attempt, and raises Degraded.
func TestRetentionWithoutHookRole(t *testing.T) {
	h, j, st, now := deliveryFixture(t)
	start := *now
	attempts := 0
	hook := &fakeHook{handle: func(context.Context, string, integration.Event) error {
		attempts++
		return &Failure{Kind: FailureTemporary}
	}}
	own := queueOne(t, h, j, start, "own", hook)
	other := kinds.Owner{ClusterID: j.guard.Owner.ClusterID, Namespace: "dns", Name: "other", UID: "uid-other"}
	id := kinds.Digest([]byte("other"))
	d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(other, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: kinds.FormatVersion, Owner: other, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: start, Deadline: start.Add(24 * time.Hour), NextAttempt: start}}
	if _, err := st.Apply(context.Background(), d, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	attempts = 0
	// The provider no longer has the hook role.
	j.provider = &fakePublisher{}
	*now = start.Add(23 * time.Hour)
	if err := h.expireEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if row, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, own); err != nil || row.Spec.DeadLetter {
		t.Fatal("dead-lettered before the deadline", row, err)
	}
	*now = start.Add(25 * time.Hour)
	if err := h.expireEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	row, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, own)
	if err != nil || !row.Spec.DeadLetter || row.Spec.ErrorClass != "expired" || attempts != 0 {
		t.Fatal("own expired row not dead-lettered without an attempt", row, err, attempts)
	}
	if _, err := h.opts.Ledger.ReadDelivery(context.Background(), other, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("another instance's expired row was kept", err)
	}
	if status := h.Statuses(); len(status) == 0 || status[0].Reason != ReasonEventDeadLettered {
		t.Fatal("no Degraded for the dropped event", status)
	}
}
