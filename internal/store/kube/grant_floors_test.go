package kube

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGrantFloorsMergeAcrossReplicasAndRestart(t *testing.T) {
	requireEnv(t)
	const ns = "grant-floors-test"
	if err := shared.admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	a := newTestStore(t, Options{Namespace: ns})
	b := newTestStore(t, Options{Namespace: ns})
	empty, err := a.ReadGrantFloors(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatal("missing document is not empty floors", err, empty)
	}
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	if err := a.MergeGrantFloors(ctx, map[string]time.Time{"operator": t0}); err != nil {
		t.Fatal(err)
	}
	// Concurrent merges from both replicas resolve by commutative maximum.
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 20 {
		for _, s := range []*Store{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.MergeGrantFloors(ctx, map[string]time.Time{"operator": t0.Add(time.Duration(i) * time.Second), "zone-admin@zone:home": t0.Add(time.Duration(i) * time.Minute)})
			}()
		}
	}
	wg.Wait()
	close(errs)
	contended := 0
	for err := range errs {
		if err != nil {
			contended++
		}
	}
	// Bounded retries may be exhausted under this deliberate burst; whatever
	// committed must still be the maximum of committed raises.
	floors, err := b.ReadGrantFloors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if floors["operator"].Before(t0) || floors["zone-admin@zone:home"].IsZero() {
		t.Fatal("concurrent merges lost floors", floors, contended)
	}
	// A final uncontended merge of the maxima always succeeds.
	if err := a.MergeGrantFloors(ctx, map[string]time.Time{"operator": t0.Add(19 * time.Second), "zone-admin@zone:home": t0.Add(19 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// An older raise never lowers a floor.
	if err := b.MergeGrantFloors(ctx, map[string]time.Time{"operator": t0}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newTestStore(t, Options{Namespace: ns})
	floors, err = restarted.ReadGrantFloors(ctx)
	if err != nil || !floors["operator"].Equal(t0.Add(19*time.Second)) || !floors["zone-admin@zone:home"].Equal(t0.Add(19*time.Minute)) {
		t.Fatal("restart lost or lowered durable floors", err, floors)
	}
	// Malformed content refuses instead of resetting floors.
	var saved corev1.Secret
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: grantFloorsName}, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Data = map[string][]byte{"floors": []byte(`{"version":2,"floors":{}}`)}
	if err := shared.admin.Update(ctx, &saved); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ReadGrantFloors(ctx); !errors.Is(err, ErrGrantFloors) {
		t.Fatal("malformed floors accepted", err)
	}
	if err := restarted.MergeGrantFloors(ctx, map[string]time.Time{"operator": t0}); !errors.Is(err, ErrGrantFloors) {
		t.Fatal("merge over malformed floors reset them", err)
	}
}
