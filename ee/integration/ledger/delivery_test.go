package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
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
