package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// testStatusOptimistic: UpdateStatus with a resourceVersion writes only on
// top of it (ErrConflict otherwise); without one it always writes. The
// controller relies on this to never write twice from a stale cache.
func testStatusOptimistic(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	key := store.Key{Kind: "DNSZone", Name: "home"}
	var stale v1alpha1.DNSZone
	if err := s.Get(ctx, key, &stale); err != nil {
		t.Fatal(err)
	}
	first := stale.DeepCopy()
	first.Status.Serial = 1
	if err := s.UpdateStatus(ctx, first); err != nil {
		t.Fatalf("write on the current resourceVersion: %v", err)
	}
	second := stale.DeepCopy()
	second.Status.Serial = 2
	if err := s.UpdateStatus(ctx, second); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("write on a stale resourceVersion: %v, want ErrConflict", err)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Serial != 1 {
		t.Fatalf("a conflicting write changed the status: serial %d", got.Status.Serial)
	}
	unconditional := got.DeepCopy()
	unconditional.ResourceVersion = ""
	unconditional.Status.Serial = 3
	if err := s.UpdateStatus(ctx, unconditional); err != nil {
		t.Fatalf("write without a resourceVersion: %v", err)
	}
}
