package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// RunReservedFinalizer checks the cleanup obligation of an Integration with
// deletionPolicy Delete (D367) on a driver that honors finalizers: no
// writer's whole-object apply strips store.IntegrationCleanupFinalizer, a
// delete leaves the object readable, and only the exact-identity
// store.FinalizerWriter change removes it. admits says whether the driver
// arms the finalizer in Apply itself (sqlite) or carries what its host armed
// (kube). The memory reference driver has no deletionTimestamp and does not
// run this case, like RunRejections.
func RunReservedFinalizer(t *testing.T, factory func(t *testing.T) store.Store, admits bool) {
	t.Helper()
	t.Run("ReservedIntegrationFinalizer", func(t *testing.T) { testReservedFinalizer(t, factory(t), admits) })
}

func cleanupIntegration(name string) *v1alpha1.Integration {
	return &v1alpha1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec: v1alpha1.IntegrationSpec{
			Type:           "unifi",
			Credentials:    map[string]v1alpha1.CredentialReference{"apiKey": {SecretRef: &v1alpha1.CredentialSecretRef{Name: "unifi", Key: "token"}}},
			UniFi:          &v1alpha1.UniFiIntegration{Endpoint: "https://network.example"},
			DeletionPolicy: v1alpha1.IntegrationDeletionDelete,
		},
	}
}

func testReservedFinalizer(t *testing.T, s store.Store, admits bool) {
	ctx := t.Context()
	fw, ok := s.(store.FinalizerWriter)
	if !ok {
		t.Fatal("driver honors finalizers but has no metadata-only finalizer write")
	}
	const reserved = store.IntegrationCleanupFinalizer
	file := store.ManagerFile("integrations.yaml")
	key := store.Key{Kind: "Integration", Namespace: "apps", Name: "cleanup"}
	get := func() *v1alpha1.Integration {
		t.Helper()
		var got v1alpha1.Integration
		if err := s.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		return &got
	}
	remove := func(uid, rv string) (store.Object, error) {
		return fw.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: reserved, Remove: true, ExpectedUID: uid, ExpectedResourceVersion: rv})
	}

	created, err := s.Apply(ctx, cleanupIntegration("cleanup"), store.ApplyOptions{Manager: file})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.HasFinalizer(created, reserved); got != admits {
		t.Fatalf("Apply of a Delete policy armed the cleanup finalizer: %v, want %v", got, admits)
	}
	if !admits {
		// No admission: the elected host arms the exact object it observed.
		armed, err := fw.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: reserved, ExpectedUID: string(created.GetUID()), ExpectedResourceVersion: created.GetResourceVersion()})
		if err != nil {
			t.Fatal(err)
		}
		if !store.HasFinalizer(armed, reserved) || armed.GetGeneration() != created.GetGeneration() || store.ManagedBy(armed) != file ||
			armed.(*v1alpha1.Integration).Spec.DeletionPolicy != v1alpha1.IntegrationDeletionDelete {
			t.Fatalf("arming changed more than the finalizer: %+v", armed)
		}
	}
	live := get()
	if !store.HasFinalizer(live, reserved) || live.DeletionTimestamp != nil {
		t.Fatalf("armed object: finalizers=%v deletionTimestamp=%v", live.Finalizers, live.DeletionTimestamp)
	}

	// The same manager's document never lists the finalizer.
	if _, err := s.Apply(ctx, cleanupIntegration("cleanup"), store.ApplyOptions{Manager: file}); err != nil {
		t.Fatal(err)
	}
	if got := get(); !store.HasFinalizer(got, reserved) || got.Generation != live.Generation {
		t.Fatalf("identical re-apply: finalizers=%v generation=%d (was %d)", got.Finalizers, got.Generation, live.Generation)
	}
	edited := cleanupIntegration("cleanup")
	edited.Spec.Suspend = true
	if _, err := s.Apply(ctx, edited, store.ApplyOptions{Manager: file}); err != nil {
		t.Fatal(err)
	}
	if got := get(); !store.HasFinalizer(got, reserved) || !got.Spec.Suspend {
		t.Fatalf("edited re-apply stripped the cleanup finalizer: %v", got.Finalizers)
	}
	// A whole-object takeover replaces every field the new manager omits,
	// except the reserved finalizer.
	if _, err := s.Apply(ctx, cleanupIntegration("cleanup"), store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	taken := get()
	if !store.HasFinalizer(taken, reserved) || store.ManagedBy(taken) != store.ManagerAPI || taken.Spec.Suspend {
		t.Fatalf("Force takeover: finalizers=%v managedBy=%q suspend=%v", taken.Finalizers, store.ManagedBy(taken), taken.Spec.Suspend)
	}

	// Only the exact observed object can be released.
	if _, err := remove(string(taken.UID), live.ResourceVersion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale resourceVersion: got %v, want ErrConflict", err)
	}
	if _, err := remove("another-incarnation", taken.ResourceVersion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong UID: got %v, want ErrConflict", err)
	}
	if _, err := remove("", taken.ResourceVersion); err == nil {
		t.Fatal("a finalizer change without an expected UID was accepted")
	}
	if got := get(); !store.HasFinalizer(got, reserved) || got.ResourceVersion != taken.ResourceVersion {
		t.Fatal("a refused finalizer change wrote")
	}

	if err := s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	deleting := get()
	if deleting.DeletionTimestamp == nil || !store.HasFinalizer(deleting, reserved) || deleting.UID != taken.UID ||
		deleting.Spec.DeletionPolicy != v1alpha1.IntegrationDeletionDelete || len(deleting.Spec.Credentials) != 1 {
		t.Fatalf("a deleted Delete-policy Integration must stay readable while it is cleaned up: %+v", deleting)
	}
	// Re-declaring the object while it is being deleted neither finalizes it
	// nor clears the deletion.
	again := cleanupIntegration("cleanup")
	again.Spec.Suspend = true
	if _, err := s.Apply(ctx, again, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	held := get()
	if held.DeletionTimestamp == nil || !store.HasFinalizer(held, reserved) || !held.Spec.Suspend {
		t.Fatalf("apply during deletion: deletionTimestamp=%v finalizers=%v", held.DeletionTimestamp, held.Finalizers)
	}
	if _, err := fw.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: "kudurru.io/late", ExpectedUID: string(held.UID), ExpectedResourceVersion: held.ResourceVersion}); !errors.Is(err, store.ErrTerminating) {
		t.Fatalf("adding a finalizer to an object being deleted: got %v, want ErrTerminating", err)
	}
	if _, err := remove(string(held.UID), deleting.ResourceVersion); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale release of a deleting object: got %v, want ErrConflict", err)
	}
	removed, err := remove(string(held.UID), held.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Terminated(removed) || removed.GetUID() != held.UID {
		t.Fatalf("the release did not report the removed object: %+v", removed)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var gone v1alpha1.Integration
		err := s.Get(context.WithoutCancel(ctx), key, &gone)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("released object still present: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := remove(string(held.UID), held.ResourceVersion); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("release of a removed object: got %v, want ErrNotFound", err)
	}
}
