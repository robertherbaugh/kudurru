package conformance

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func testCreateOnly(t *testing.T, s store.Store) {
	ctx := context.Background()
	start := make(chan struct{})
	type result struct {
		obj store.Object
		err error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			obj, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
			results <- result{obj, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var winner store.Object
	for result := range results {
		if result.err == nil {
			if winner != nil {
				t.Fatal("multiple create-only writers won")
			}
			winner = result.obj
		} else if !errors.Is(result.err, store.ErrConflict) {
			t.Fatal(result.err)
		}
	}
	if winner == nil {
		t.Fatal("no create-only writer won")
	}
	changed := zone("home", "changed.example.")
	if _, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerSystem, Force: true, CreateOnly: true}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("Force bypassed create-only precondition")
	}
	var stored v1alpha1.DNSZone
	if err := s.Get(ctx, store.KeyOf(winner), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.UID != winner.GetUID() || stored.ResourceVersion != winner.GetResourceVersion() || stored.Spec.ZoneName != "home.example." || store.ManagedBy(&stored) != store.ManagerAPI {
		t.Fatal("losing writer changed the existing object")
	}
	if _, err := s.Apply(ctx, zone("missing", "missing.example."), store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: "1", CreateOnly: true}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("contradictory preconditions created an object")
	}
}
