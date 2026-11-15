package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestCommandDeleteOriginalConflictPoisonsWholeTransaction(t *testing.T) {
	s := openTemp(t)
	original, err := s.Apply(t.Context(), zone("original-delete", "delete.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	changed := original.DeepCopyObject().(*v1alpha1.DNSZone)
	changed.Spec.DefaultTTL = 900
	next, err := s.Apply(t.Context(), changed, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: original.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	rv := s.ResourceVersion()
	err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if _, err := tx.Apply(ctx, zone("rolled-back", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}); err != nil {
			return err
		}
		_ = tx.Delete(ctx, store.KeyOf(original), store.DeleteOptions{Manager: store.ManagerAPI, Force: true, ExpectedUID: string(original.GetUID()), ExpectedResourceVersion: original.GetResourceVersion()})
		return nil
	})
	if !errors.Is(err, store.ErrConflict) || s.ResourceVersion() != rv {
		t.Fatal("ignored original delete conflict did not poison command", err)
	}
	var after v1alpha1.DNSZone
	if err := s.Get(t.Context(), store.KeyOf(original), &after); err != nil || after.ResourceVersion != next.GetResourceVersion() {
		t.Fatal("original successor changed", err)
	}
	if err := s.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "rolled-back"}, &after); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("partial transaction survived", err)
	}
}
