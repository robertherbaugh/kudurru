package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProductionCrashBetweenQueueAndCursorIsOneRow(t *testing.T) {
	h, j, st, now := deliveryFixture(t)
	failCursor := true

	r, err := ledger.New(ledger.Options{Reader: st, Now: func() time.Time { return *now }, Write: func(ctx context.Context, g ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		if _, ok := o.(*kinds.IntegrationEventCursor); ok && failCursor {
			return nil, errors.New("cut before the cursor commit")
		}
		var out store.Object
		err := h.WithLease(ctx, g, func(ctx context.Context) error { var err error; out, err = st.Apply(ctx, o, a); return err })
		return out, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.Ledger = r
	j.provider = &fakeHook{handle: func(context.Context, string, integration.Event) error { return nil }}
	var calls []map[string]string
	h.opts.Produce = func(_ context.Context, _ ExecutionGuard, positions map[string]string, _ []string, _ int) (EventBatch, error) {
		calls = append(calls, positions)
		if positions == nil {
			return EventBatch{Positions: map[string]string{"feed": "10"}}, nil
		}
		if positions["feed"] == "10" {
			return EventBatch{Events: []ProducedEvent{{Topic: "zone.changed", Subject: "zone-uid", Version: "7", Payload: json.RawMessage(`{"zone":"example.com","serial":7}`)}}, Positions: map[string]string{"feed": "11"}}, nil
		}
		return EventBatch{Positions: positions}, nil
	}
	// Baseline: no events, a cursor; the first save is cut.
	if err := h.produceEvents(context.Background(), j); err == nil {
		t.Fatal("cut cursor save reported success")
	}
	failCursor = false
	if err := h.produceEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	failCursor = true
	*now = now.Add(time.Second)
	// The event is queued, the cursor save is cut, the next tick repeats it.
	if err := h.produceEvents(context.Background(), j); err == nil {
		t.Fatal("cut cursor save reported success")
	}
	failCursor = false
	*now = now.Add(2 * time.Second)
	if err := h.produceEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	rows, err := st.List(context.Background(), kinds.KindIntegrationDelivery, store.ListOptions{})
	if err != nil || len(rows.Items) != 1 {
		t.Fatal("rows", len(rows.Items), err)
	}
	d := rows.Items[0].(*kinds.IntegrationDelivery)
	if d.Spec.DeliveryID != DeliveryID(j.guard.Owner, ProducedEvent{Topic: "zone.changed", Subject: "zone-uid", Version: "7"}) {
		t.Fatal("unstable delivery ID")
	}
	c, err := r.ReadEventCursor(context.Background(), j.guard.Owner)
	if err != nil || c.Spec.Positions["feed"] != "11" {
		t.Fatal("cursor", c, err)
	}
	// Idle ticks do not write the cursor again.
	rv := c.ResourceVersion
	if err := h.produceEvents(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.ReadEventCursor(context.Background(), j.guard.Owner); c.ResourceVersion != rv {
		t.Fatal("idle production wrote the cursor")
	}
	if len(calls) != 5 || calls[0] != nil {
		t.Fatal("producer calls", calls)
	}
}

func TestProductionWaitsAtThePendingCap(t *testing.T) {
	h, j, st, now := deliveryFixture(t)
	j.provider = &fakeHook{handle: func(context.Context, string, integration.Event) error { return nil }}
	for i := range maxPendingDeliveries {
		id := kinds.Digest(fmt.Appendf(nil, "pending-%d", i))
		row := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(j.guard.Owner, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: kinds.FormatVersion, Owner: j.guard.Owner, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: *now, Deadline: now.Add(24 * time.Hour), NextAttempt: *now}}
		if _, err := st.Apply(context.Background(), row, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	h.opts.Produce = func(context.Context, ExecutionGuard, map[string]string, []string, int) (EventBatch, error) {
		called = true
		return EventBatch{}, nil
	}
	if err := h.produceEvents(context.Background(), j); err != nil || called {
		t.Fatal("produced past the pending cap", err, called)
	}
	// A producer that returns more than it was asked for is refused.
	h2, j2, _, _ := deliveryFixture(t)
	j2.provider = j.provider
	h2.opts.Produce = func(_ context.Context, _ ExecutionGuard, _ map[string]string, _ []string, limit int) (EventBatch, error) {
		out := EventBatch{Positions: map[string]string{}}
		for i := range limit + 1 {
			out.Events = append(out.Events, ProducedEvent{Topic: "zone.changed", Subject: "s", Version: fmt.Sprint(i), Payload: json.RawMessage(`{}`)})
		}
		return out, nil
	}
	if err := h2.produceEvents(context.Background(), j2); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded batch accepted", err)
	}
}

// A peer that acknowledges every event promptly gets no back-pressure from
// the pending cap, so retention must bound the rows by count: the instance
// never holds more than maxInstanceDeliveries rows, its oldest terminal
// rows are evicted above maxTerminalDeliveries, and production keeps going
// (D379).
func TestProductionRowsStayBoundedByCount(t *testing.T) {
	h, j, st, now := deliveryFixture(t)
	delivered := 0
	j.provider = &fakeHook{handle: func(context.Context, string, integration.Event) error { delivered++; return nil }}
	next := 0
	h.opts.Produce = func(_ context.Context, _ ExecutionGuard, positions map[string]string, _ []string, limit int) (EventBatch, error) {
		if positions == nil {
			return EventBatch{Positions: map[string]string{"feed": "0"}}, nil
		}
		var out EventBatch
		for range limit {
			next++
			out.Events = append(out.Events, ProducedEvent{Topic: "zone.changed", Subject: fmt.Sprintf("zone-%d", next), Version: "1", Payload: json.RawMessage(`{}`)})
		}
		out.Positions = map[string]string{"feed": fmt.Sprint(next)}
		return out, nil
	}
	rows := func() (all, terminal int) {
		page, err := st.List(context.Background(), kinds.KindIntegrationDelivery, store.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if d := item.(*kinds.IntegrationDelivery); d.Spec.Acknowledged || d.Spec.DeadLetter {
				terminal++
			}
		}
		return len(page.Items), terminal
	}
	for tick := range 40 {
		*now = now.Add(2 * time.Second)
		if err := h.produceEvents(context.Background(), j); err != nil {
			t.Fatal(tick, err)
		}
		if all, _ := rows(); all > maxInstanceDeliveries {
			t.Fatalf("tick %d: %d rows after production", tick, all)
		}
		if err := h.drainEvents(context.Background(), j); err != nil {
			t.Fatal(tick, err)
		}
		// Retention runs before this tick's attempts, so the rows those
		// attempts acknowledged wait for the next tick.
		if _, terminal := rows(); terminal > maxTerminalDeliveries+maxProducePerTick {
			t.Fatalf("tick %d: %d terminal rows after retention", tick, terminal)
		}
	}
	// Forty ticks of 32 events: production was never held back for long.
	if delivered < 35*maxProducePerTick {
		t.Fatal("production stalled at the row budget", delivered)
	}
	// The newest terminal rows are the ones kept.
	page, err := st.List(context.Background(), kinds.KindIntegrationDelivery, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if d := item.(*kinds.IntegrationDelivery); d.Spec.CreatedAt.Before(now.Add(-time.Duration(maxTerminalDeliveries/maxProducePerTick+2) * 2 * time.Second)) {
			t.Fatal("an old terminal row was kept over a newer one", d.Spec.CreatedAt, *now)
		}
	}
}

// Count eviction takes superseded rows first (D379): a subject's older
// versions go before the newest row of any subject, because a status write
// reproduces only a subject's current version. So the newest row of a
// subject that changed long ago still dedups its reproduction after many
// newer versions of another subject.
func TestEvictionPrefersSupersededRows(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	row := func(i int, uid string) *kinds.IntegrationDelivery {
		created := start.Add(time.Duration(i) * time.Second)
		return &kinds.IntegrationDelivery{Spec: kinds.IntegrationDeliverySpec{DeliveryID: fmt.Sprint(i), Topic: "record.changed", Payload: json.RawMessage(fmt.Sprintf(`{"uid":%q}`, uid)), CreatedAt: created, Deadline: created.Add(24 * time.Hour), Acknowledged: true}}
	}
	// The oldest row is the only version of "quiet"; then many versions of
	// "busy", which exceed the terminal budget.
	own := []*kinds.IntegrationDelivery{row(0, "quiet")}
	for i := 1; i <= maxTerminalDeliveries+10; i++ {
		own = append(own, row(i, "busy"))
	}
	evicted := evictable(start.Add(time.Hour), own)
	if len(evicted) != 11 {
		t.Fatal("evicted", len(evicted))
	}
	for i, d := range evicted {
		if d.Spec.DeliveryID != fmt.Sprint(i+1) {
			t.Fatal("evicted other than the oldest superseded rows", i, d.Spec.DeliveryID)
		}
	}
	// Past their retention every terminal row goes, superseded or not, and
	// undelivered rows never do.
	pending := row(500, "pending")
	pending.Spec.Acknowledged = false
	if evicted := evictable(start.Add(26*time.Hour), []*kinds.IntegrationDelivery{row(0, "quiet"), pending}); len(evicted) != 1 || evicted[0].Spec.DeliveryID != "0" {
		t.Fatal("expiry eviction", evicted)
	}
}
