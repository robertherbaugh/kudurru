package conformance

import (
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func testCreateThenUpdate(t *testing.T, s store.Store) {
	ctx := t.Context()
	first, err := s.Apply(ctx, zone("created", "created.example."), store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	next := first.DeepCopyObject().(*v1alpha1.DNSZone)
	next.Spec.DefaultTTL = 720
	second, err := s.Apply(ctx, next, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
	if err != nil {
		t.Fatal("same owner cannot update its atomic creation", err)
	}
	if second.GetUID() != first.GetUID() || second.GetResourceVersion() == first.GetResourceVersion() || second.(*v1alpha1.DNSZone).Spec.DefaultTTL != 720 {
		t.Fatal("update did not preserve identity and advance contents")
	}
}

func testDeleteOriginalIdentity(t *testing.T, s store.Store) {
	ctx := t.Context()
	first, err := s.Apply(ctx, zone("original", "original.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	key := store.KeyOf(first)
	guard := store.DeleteOptions{Manager: store.ManagerAPI, ExpectedUID: string(first.GetUID()), ExpectedResourceVersion: first.GetResourceVersion()}
	changed := first.DeepCopyObject().(*v1alpha1.DNSZone)
	changed.Spec.DefaultTTL = 900
	second, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		guard.Force = force
		if err := s.Delete(ctx, key, guard); !errors.Is(err, store.ErrConflict) {
			t.Fatal("stale delete was rescued", err)
		}
	}
	var saved v1alpha1.DNSZone
	if err := s.Get(ctx, key, &saved); err != nil || saved.ResourceVersion != second.GetResourceVersion() || saved.Spec.DefaultTTL != 900 {
		t.Fatal("refusal changed newer object", err)
	}
	guard.ExpectedResourceVersion = second.GetResourceVersion()
	if err := s.Delete(ctx, key, guard); err != nil {
		t.Fatal("exact original delete failed", err)
	}
	replacement, err := s.Apply(ctx, zone("original", "original.example."), store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	guard.ExpectedResourceVersion = replacement.GetResourceVersion()
	if err := s.Delete(ctx, key, guard); !errors.Is(err, store.ErrConflict) {
		t.Fatal("old UID deleted replacement", err)
	}
	if err := s.Get(ctx, key, &saved); err != nil || saved.UID != replacement.GetUID() {
		t.Fatal("replacement lost", err)
	}
	if md, ok := s.(store.MultiDeleter); ok {
		other, err := s.Apply(ctx, zone("other", "other.example."), store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			t.Fatal(err)
		}
		if err := md.DeleteMany(ctx, []store.DeleteRequest{{Key: store.KeyOf(other), Options: store.DeleteOptions{Manager: store.ManagerAPI}}, {Key: key, Options: guard}}); !errors.Is(err, store.ErrConflict) {
			t.Fatal("batch lost original identity", err)
		}
		if err := s.Get(ctx, store.KeyOf(other), &saved); err != nil {
			t.Fatal("failed batch partially deleted", err)
		}
	}
}
