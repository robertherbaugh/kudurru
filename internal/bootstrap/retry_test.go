package bootstrap

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type interruptedSeedStore struct {
	store.Store
	calls int
}

func (s *interruptedSeedStore) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	s.calls++
	if s.calls == 2 {
		return nil, syscall.ECONNREFUSED
	}
	return s.Store.Apply(ctx, obj, opts)
}

func TestContinueInterruptedFirstBoot(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	interrupted := &interruptedSeedStore{Store: st}
	res, err := Seed(ctx, interrupted, Options{Validator: testValidator(t)})
	if !errors.Is(err, syscall.ECONNREFUSED) || !res.FirstBoot || len(res.Created) != 1 {
		t.Fatalf("expected interrupted first boot: %+v %v", res, err)
	}
	// Preserve an operator edit made during the outage; only complete the
	// missing keys, even though the initial seed made the store nonempty.
	var group v1alpha1.ClientGroup
	if err := st.Get(ctx, groupKey, &group); err != nil {
		t.Fatal(err)
	}
	group.Spec.Clients[0].Name = "operator edit"
	if _, err := st.Apply(ctx, &group, store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	res, err = Seed(ctx, interrupted, Options{Validator: testValidator(t), ContinueFirstBoot: true})
	if err != nil || len(res.Created) != 2 || len(res.Present) != 1 {
		t.Fatalf("resume: %+v %v", res, err)
	}
	if err := st.Get(ctx, groupKey, &group); err != nil || group.Spec.Clients[0].Name != "operator edit" || store.ManagedBy(&group) != store.ManagerAPI {
		t.Fatalf("operator edit overwritten: %+v %v", group, err)
	}
	// A later normal startup must respect deliberate deletions.
	if err := st.Delete(ctx, policyKey, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err = Seed(ctx, st, Options{Validator: testValidator(t)})
	if err != nil || res.FirstBoot {
		t.Fatalf("normal restart must not resume: %+v %v", res, err)
	}
	var pol v1alpha1.Policy
	if err := st.Get(ctx, policyKey, &pol); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted policy recreated: %v", err)
	}
}
