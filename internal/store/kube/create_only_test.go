package kube

import (
	"context"
	"errors"
	"sync"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnvtestInternalCreateOnly(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{Scheme: internalScheme(t), InternalKinds: internalKindList()})
	ctx := context.Background()
	key := store.Key{Kind: internalv1.KindServiceAccount, Namespace: "dns", Name: "create-only"}
	t.Cleanup(func() { _ = s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}) })
	start := make(chan struct{})
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			_, err := s.Apply(ctx, &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, store.ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("create-only winners = %d", winners)
	}
	var original internalv1.ServiceAccount
	if err := s.Get(ctx, key, &original); err != nil {
		t.Fatal(err)
	}
	changed := original.DeepCopy()
	changed.Spec.Disabled = true
	if _, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerSystem, Force: true, CreateOnly: true}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("private object creation modified an existing identity")
	}
	var got internalv1.ServiceAccount
	if err := s.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != original.UID || got.ResourceVersion != original.ResourceVersion || got.Spec.Disabled {
		t.Fatal("losing private writer changed identity or spec")
	}
}
