package kube

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// applyCounter counts the Apply calls a file loader makes on the driver.
type applyCounter struct {
	store.Store
	applies int
}

func (c *applyCounter) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	c.applies++
	return c.Store.Apply(ctx, obj, opts)
}

// TestFileDeclaredDeleteIntegrationIsNotRewrittenWithoutAdmission runs the
// file reconciler over this driver. The reconciler applies the unchanged
// document of a live Integration whose policy is Delete while the object
// lacks the cleanup finalizer, so that a driver with admission (sqlite) arms
// it (D367). This driver has no admission and never arms it, which makes
// that state the normal one here until a host arms the object. The extra
// apply must then be a no-op at the apiserver, be reported as unchanged, and
// happen once per agreement, not on every pass.
func TestFileDeclaredDeleteIntegrationIsNotRewrittenWithoutAdmission(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	counted := &applyCounter{Store: s}
	dir := filepath.Join(t.TempDir(), "objects.d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	doc := "apiVersion: kudurru.io/v1alpha1\nkind: Integration\nmetadata:\n  name: file-declared\n  namespace: apps\nspec:\n  type: unifi\n" +
		"  credentials:\n    apiKey:\n      secretRef:\n        name: unifi\n        key: token\n  unifi:\n    endpoint: https://network.example\n  deletionPolicy: Delete\n"
	if err := os.WriteFile(filepath.Join(dir, "integration.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := func() *objects.Loader {
		t.Helper()
		l, err := objects.New(objects.Options{Store: counted, Dir: dir, PollInterval: -1, Logger: testLogger()})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	load := func(l *objects.Loader, what string) objects.Report {
		t.Helper()
		rep := l.Load(t.Context())
		if rep.Count(objects.SeverityError) != 0 || rep.Count(objects.SeverityWarning) != 0 {
			t.Fatalf("%s: %v", what, rep.Diagnoses)
		}
		return rep
	}
	key := store.Key{Kind: "Integration", Namespace: "apps", Name: "file-declared"}
	stored := func() *v1alpha1.Integration {
		t.Helper()
		var got v1alpha1.Integration
		if err := s.Get(t.Context(), key, &got); err != nil {
			t.Fatal(err)
		}
		return &got
	}

	if rep := load(loader(), "create"); rep.Created != 1 {
		t.Fatalf("create: created=%d", rep.Created)
	}
	created := stored()
	if len(created.Finalizers) != 0 || !store.ReservedFinalizerUnarmed(created) {
		t.Fatalf("this driver must not arm the finalizer on its own: %v", created.Finalizers)
	}
	// A process start: no remembered agreement, so the reconciler compares
	// and applies the unchanged document once.
	restarted := loader()
	for pass := 1; pass <= 3; pass++ {
		if rep := load(restarted, "pass over the unarmed object"); rep.Unchanged != 1 || rep.Updated != 0 || rep.Created != 0 {
			t.Fatalf("pass %d: created=%d updated=%d unchanged=%d, want unchanged", pass, rep.Created, rep.Updated, rep.Unchanged)
		}
	}
	if got := stored(); got.ResourceVersion != created.ResourceVersion || len(got.Finalizers) != 0 {
		t.Fatalf("the unchanged document rewrote the object: rv %s then %s, finalizers %v", created.ResourceVersion, got.ResourceVersion, got.Finalizers)
	}
	if counted.applies != 2 {
		t.Fatalf("%d applies (one create, then one for three passes over the unarmed object), want 2", counted.applies)
	}
	// Once a host has armed the object the document needs no apply at all,
	// and the host's finalizer stays.
	armed, err := s.UpdateFinalizer(t.Context(), key, store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, ExpectedUID: string(created.UID), ExpectedResourceVersion: created.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	if rep := load(restarted, "pass over the armed object"); rep.Unchanged != 1 || rep.Updated != 0 {
		t.Fatalf("pass over the armed object: updated=%d unchanged=%d", rep.Updated, rep.Unchanged)
	}
	if got := stored(); got.ResourceVersion != armed.GetResourceVersion() || !store.HasFinalizer(got, store.IntegrationCleanupFinalizer) || counted.applies != 2 {
		t.Fatalf("the armed object was applied or changed: applies=%d finalizers=%v", counted.applies, got.Finalizers)
	}
}
