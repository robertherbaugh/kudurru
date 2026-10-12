package objects

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestApplyFile(t *testing.T) {
	st := memoryStore(t)
	path := filepath.Join(t.TempDir(), "objects.yaml")
	if err := os.WriteFile(path, []byte(zonesYAML+listsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := ApplyFile(ctx(t), st, path, WithValidator(testValidator(t)))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 3 || len(rep.Diagnoses) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if got := owner(t, st, zoneKey("home")); got != store.ManagerAPI {
		t.Fatalf("ApplyFile must apply as %q, got %q", store.ManagerAPI, got)
	}

	// Re-applying is a no-op; removing a document deletes nothing.
	if err := os.WriteFile(path, []byte(listsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = ApplyFile(ctx(t), st, path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unchanged != 1 || rep.Deleted != 0 || zone(t, st, "home") == nil {
		t.Fatalf("report = %+v", rep)
	}
}

func TestApplyFileConflictsAndOptions(t *testing.T) {
	st := memoryStore(t)
	// A file-managed zone belongs to the loader: ApplyFile reports a conflict.
	if _, err := ApplyBytes(ctx(t), st, "objects.d/zones.yaml", []byte(zonesYAML), WithManager(Manager("objects.d/zones.yaml"))); err != nil {
		t.Fatal(err)
	}
	rep, err := ApplyBytes(ctx(t), st, "cli.yaml", []byte(`
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home }
spec: { zoneName: home.example., type: forward }
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: { name: kids }
spec: { clientGroupRefs: [kids] }
`), WithDefaultNamespace("dns"), WithLogger(quiet))
	if err != nil {
		t.Fatal(err)
	}
	d := findDiag(t, rep.Diagnoses, ReasonManagedByOther)
	if d.Owner != "file:objects.d/zones.yaml" || d.Document != 1 || d.Hint != "the object belongs to objects.d/zones.yaml; remove one of the declarations" {
		t.Fatalf("conflict diagnosis = %+v", d)
	}
	if owner(t, st, nsKey("Policy", "dns", "kids")) != store.ManagerAPI {
		t.Fatal("the non-conflicting document must be applied in the default namespace")
	}
	if z := zone(t, st, "home"); z.Spec.Type != v1alpha1.ZoneTypePrimary {
		t.Fatal("file-managed zone changed by ApplyFile")
	}

	if _, err := ApplyFile(ctx(t), st, filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file must be an error")
	}
	if _, err := ApplyFile(ctx(t), st, t.TempDir()); err == nil {
		t.Fatal("a directory must be an error")
	}
	if _, err := ApplyBytes(ctx(t), nil, "x", nil); err == nil {
		t.Fatal("nil store must be an error")
	}
	if _, err := ApplyBytes(ctx(t), st, "x", nil, WithManager("")); err == nil {
		t.Fatal("empty manager must be an error")
	}
}
