package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeliveryCASImmutableTerminalAndUIDFence(t *testing.T) {
	ctx := context.Background()
	g, _ := fixture(t)
	r := repository(t, memory.New(), nil)
	id := kinds.Digest([]byte("delivery"))
	d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(g.Owner, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: 1, Owner: g.Owner, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: testTime, Deadline: testTime.Add(24 * time.Hour), NextAttempt: testTime}}
	saved, err := r.QueueDelivery(ctx, g, d)
	if err != nil {
		t.Fatal(err)
	}
	changed := d.DeepCopy()
	changed.Spec.Payload = json.RawMessage(`{"changed":true}`)
	if _, err := r.QueueDelivery(ctx, g, changed); !errors.Is(err, ErrInvalid) {
		t.Fatal("queue payload changed", err)
	}
	next := saved.DeepCopy()
	next.Spec.Attempts = 1
	next.Spec.NextAttempt = testTime.Add(time.Second)
	progress, err := r.SaveDelivery(ctx, g, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveDelivery(ctx, g, next); !errors.Is(err, store.ErrConflict) {
		t.Fatal("fresh RV substituted", err)
	}
	changed = progress.DeepCopy()
	changed.Spec.Attempts = 0
	if _, err := r.SaveDelivery(ctx, g, changed); !errors.Is(err, ErrInvalid) {
		t.Fatal("attempt regressed", err)
	}
	changed = progress.DeepCopy()
	changed.Spec.Attempts = 3
	if _, err := r.SaveDelivery(ctx, g, changed); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded attempt advance", err)
	}
	progress.Spec.Acknowledged = true
	acked, err := r.SaveDelivery(ctx, g, progress)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := r.QueueDelivery(ctx, g, d)
	if err != nil || !replay.Spec.Acknowledged {
		t.Fatal("ack lost on replay", err)
	}
	changed = acked.DeepCopy()
	changed.Spec.Acknowledged = false
	if _, err := r.SaveDelivery(ctx, g, changed); !errors.Is(err, ErrInvalid) {
		t.Fatal("terminal outcome resurrected", err)
	}
	other := g
	other.Owner.UID = "other-uid"
	if _, err := r.SaveDelivery(ctx, other, acked); !errors.Is(err, ErrInvalid) {
		t.Fatal("UID recreation wrote old delivery", err)
	}
}

func TestPruneDeliveriesExactVersionAndRetention(t *testing.T) {
	ctx := context.Background()
	g, _ := fixture(t)
	st := memory.New()
	now := testTime
	base := repository(t, st, nil)
	r, err := New(Options{Reader: st, Write: base.write, Now: func() time.Time { return now }, Delete: func(ctx context.Context, _ Guard, requests []store.DeleteRequest) error {
		return st.DeleteMany(ctx, requests)
	}})
	if err != nil {
		t.Fatal(err)
	}
	queue := func(owner kinds.Owner, name string) *kinds.IntegrationDelivery {
		id := kinds.Digest([]byte(name))
		d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(owner, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: 1, Owner: owner, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: testTime, Deadline: testTime.Add(24 * time.Hour), NextAttempt: testTime}}
		og := g
		og.Owner = owner
		saved, err := r.QueueDelivery(ctx, og, d)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	pending := queue(g.Owner, "pending")
	acked := queue(g.Owner, "acked")
	next := acked.DeepCopy()
	next.Spec.Attempts, next.Spec.Acknowledged = 1, true
	if acked, err = r.SaveDelivery(ctx, g, next); err != nil {
		t.Fatal(err)
	}
	if err := r.PruneDeliveries(ctx, g, []*kinds.IntegrationDelivery{acked}); !errors.Is(err, ErrInvalid) {
		t.Fatal("pruned before deadline plus retention", err)
	}
	now = testTime.Add(24*time.Hour + DeliveryRetention)
	if err := r.PruneDeliveries(ctx, g, []*kinds.IntegrationDelivery{pending}); !errors.Is(err, ErrInvalid) {
		t.Fatal("pruned an own row that is not terminal", err)
	}
	stale := acked.DeepCopy()
	stale.ResourceVersion = "stale"
	if err := r.PruneDeliveries(ctx, g, []*kinds.IntegrationDelivery{stale}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("pruned with a stale version", err)
	}
	// An undelivered foreign row past its retention is pruned whether or
	// not its Integration exists (TestPruneDeliveriesRowsOfLiveOwner).
	other := kinds.Owner{ClusterID: g.Owner.ClusterID, Namespace: "dns", Name: "other", UID: "uid-other"}
	foreign := queue(other, "foreign")
	if err := r.PruneDeliveries(ctx, g, []*kinds.IntegrationDelivery{acked, foreign}); err != nil {
		t.Fatal(err)
	}
	for owner, id := range map[kinds.Owner]string{g.Owner: acked.Spec.DeliveryID, other: foreign.Spec.DeliveryID} {
		if _, err := r.ReadDelivery(ctx, owner, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("row kept", err)
		}
	}
	if _, err := r.ReadDelivery(ctx, g.Owner, pending.Spec.DeliveryID); err != nil {
		t.Fatal("pending row removed", err)
	}
}

// A row of a live Integration that no longer runs a hook lease, terminal or
// undelivered, is pruned by any other lease once its retention ran out: an
// owner that runs any lease dead-letters its own rows at the deadline, so
// one still undelivered an hour later has no lease to do it (D379, D380).
func TestPruneDeliveriesRowsOfLiveOwner(t *testing.T) {
	ctx := context.Background()
	g, _ := fixture(t)
	st := memory.New()
	now := testTime
	base := repository(t, st, nil)
	r, err := New(Options{Reader: st, Write: base.write, Now: func() time.Time { return now }, Delete: func(ctx context.Context, _ Guard, requests []store.DeleteRequest) error {
		return st.DeleteMany(ctx, requests)
	}})
	if err != nil {
		t.Fatal(err)
	}
	other := kinds.Owner{ClusterID: g.Owner.ClusterID, Namespace: "dns", Name: "other", UID: "uid-other"}
	og := g
	og.Owner = other
	queue := func(name string) *kinds.IntegrationDelivery {
		id := kinds.Digest([]byte(name))
		d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(other, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: 1, Owner: other, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: testTime, Deadline: testTime.Add(24 * time.Hour), NextAttempt: testTime}}
		saved, err := r.QueueDelivery(ctx, og, d)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	pending := queue("pending")
	acked := queue("acked")
	next := acked.DeepCopy()
	next.Spec.Attempts, next.Spec.Acknowledged = 1, true
	if acked, err = r.SaveDelivery(ctx, og, next); err != nil {
		t.Fatal(err)
	}
	live := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "dns", UID: "uid-other"}, Spec: public.IntegrationSpec{Type: "webhook"}}
	if _, err := st.Apply(ctx, live, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.PruneDeliveries(ctx, g, []*kinds.IntegrationDelivery{acked}); !errors.Is(err, ErrInvalid) {
		t.Fatal("pruned a foreign row before its retention ran out", err)
	}
	now = testTime.Add(24*time.Hour + DeliveryRetention)
	if err := r.PruneDeliveries(ctx, g, []*kinds.IntegrationDelivery{acked, pending}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadDelivery(ctx, other, acked.Spec.DeliveryID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a terminal row of a live owner was kept", err)
	}
	if _, err := r.ReadDelivery(ctx, other, pending.Spec.DeliveryID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("an expired undelivered row of a live owner was kept", err)
	}
	// The guard's own undelivered row is never pruned: its lease
	// dead-letters it.
	own := pending.DeepCopy()
	own.Spec.Owner, own.Name = g.Owner, kinds.DeliveryName(g.Owner, own.Spec.DeliveryID)
	own.ResourceVersion, own.UID = "", ""
	saved, err := r.QueueDelivery(ctx, g, own)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PruneDeliveries(ctx, g, []*kinds.IntegrationDelivery{saved}); !errors.Is(err, ErrInvalid) {
		t.Fatal("pruned the guard's own undelivered row", err)
	}
}

// Eviction deletes only the guard's own terminal rows, by exact version,
// before their retention ran out (D379).
func TestEvictDeliveriesOwnTerminalRowsOnly(t *testing.T) {
	ctx := context.Background()
	g, _ := fixture(t)
	st := memory.New()
	base := repository(t, st, nil)
	r, err := New(Options{Reader: st, Write: base.write, Now: func() time.Time { return testTime }, Delete: func(ctx context.Context, _ Guard, requests []store.DeleteRequest) error {
		return st.DeleteMany(ctx, requests)
	}})
	if err != nil {
		t.Fatal(err)
	}
	queue := func(owner kinds.Owner, name string, ack bool) *kinds.IntegrationDelivery {
		id := kinds.Digest([]byte(name))
		og := g
		og.Owner = owner
		d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(owner, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: 1, Owner: owner, DeliveryID: id, Topic: "zone.changed", Payload: json.RawMessage(`{}`), CreatedAt: testTime, Deadline: testTime.Add(24 * time.Hour), NextAttempt: testTime}}
		saved, err := r.QueueDelivery(ctx, og, d)
		if err != nil {
			t.Fatal(err)
		}
		if !ack {
			return saved
		}
		next := saved.DeepCopy()
		next.Spec.Attempts, next.Spec.Acknowledged = 1, true
		if saved, err = r.SaveDelivery(ctx, og, next); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	acked := queue(g.Owner, "acked", true)
	pending := queue(g.Owner, "pending", false)
	other := kinds.Owner{ClusterID: g.Owner.ClusterID, Namespace: "dns", Name: "other", UID: "uid-other"}
	foreign := queue(other, "foreign", true)
	if err := r.EvictDeliveries(ctx, g, []*kinds.IntegrationDelivery{acked, pending}); !errors.Is(err, ErrInvalid) {
		t.Fatal("evicted an undelivered row", err)
	}
	if err := r.EvictDeliveries(ctx, g, []*kinds.IntegrationDelivery{foreign}); !errors.Is(err, ErrInvalid) {
		t.Fatal("evicted another instance's row", err)
	}
	stale := acked.DeepCopy()
	stale.ResourceVersion = "stale"
	if err := r.EvictDeliveries(ctx, g, []*kinds.IntegrationDelivery{stale}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("evicted with a stale version", err)
	}
	if err := r.EvictDeliveries(ctx, g, []*kinds.IntegrationDelivery{acked}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadDelivery(ctx, g.Owner, acked.Spec.DeliveryID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("terminal row kept", err)
	}
	for owner, id := range map[kinds.Owner]string{g.Owner: pending.Spec.DeliveryID, other: foreign.Spec.DeliveryID} {
		if _, err := r.ReadDelivery(ctx, owner, id); err != nil {
			t.Fatal("row removed", err)
		}
	}
}

func TestPruneEventCursorsOnlyOrphans(t *testing.T) {
	ctx := context.Background()
	g, _ := fixture(t)
	st := memory.New()
	base := repository(t, st, nil)
	r, err := New(Options{Reader: st, Write: base.write, Now: func() time.Time { return testTime }, Delete: func(ctx context.Context, _ Guard, requests []store.DeleteRequest) error {
		return st.DeleteMany(ctx, requests)
	}})
	if err != nil {
		t.Fatal(err)
	}
	save := func(owner kinds.Owner) *kinds.IntegrationEventCursor {
		og := g
		og.Owner = owner
		c, err := r.SaveEventCursor(ctx, og, &kinds.IntegrationEventCursor{ObjectMeta: metav1.ObjectMeta{Name: kinds.EventCursorName(owner)}, Spec: kinds.IntegrationEventCursorSpec{FormatVersion: 1, Owner: owner, Positions: map[string]string{"feed": "1"}, UpdatedAt: testTime}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	own := save(g.Owner)
	gone := save(kinds.Owner{ClusterID: g.Owner.ClusterID, Namespace: "dns", Name: "gone", UID: "uid-gone"})
	liveOwner := kinds.Owner{ClusterID: g.Owner.ClusterID, Namespace: "dns", Name: "live", UID: "uid-live"}
	live := save(liveOwner)
	if _, err := st.Apply(ctx, &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "dns", UID: "uid-live"}, Spec: public.IntegrationSpec{Type: "webhook"}}, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.PruneEventCursors(ctx, g, []*kinds.IntegrationEventCursor{own}); !errors.Is(err, ErrInvalid) {
		t.Fatal("pruned the guard's own cursor", err)
	}
	stale := gone.DeepCopy()
	stale.ResourceVersion = "stale"
	if err := r.PruneEventCursors(ctx, g, []*kinds.IntegrationEventCursor{stale}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("pruned with a stale version", err)
	}
	if err := r.PruneEventCursors(ctx, g, []*kinds.IntegrationEventCursor{gone, live}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadEventCursor(ctx, gone.Spec.Owner); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("orphaned cursor kept", err)
	}
	if _, err := r.ReadEventCursor(ctx, liveOwner); err != nil {
		t.Fatal("a live owner's cursor was pruned", err)
	}
}
