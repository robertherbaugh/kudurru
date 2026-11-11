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
