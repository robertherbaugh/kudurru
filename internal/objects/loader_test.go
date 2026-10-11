package objects

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const zonesYAML = `
# zones
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home }
spec: { zoneName: home.example., type: primary }
---
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: { name: kids, namespace: dns }
spec: { clients: [{ cidr: 10.0.20.0/24, name: Kids VLAN }] }
---
`

const listsYAML = `
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: { name: ads }
spec: { mode: block, sources: [{ url: "https://lists.example/ads.txt" }] }
`

var inlinePolicy = []map[string]any{{
	"apiVersion": "kudurru.io/v1alpha1",
	"kind":       "Policy",
	"metadata":   map[string]any{"name": "default"},
	"spec":       map[string]any{"clientGroupRefs": []any{"default"}, "blockListRefs": []any{"ads"}, "priority": 1},
}}

func TestLoadAppliesInlineAndFiles(t *testing.T) {
	f := newFixture(t, inlinePolicy)
	f.write(t, "10-zones.yaml", zonesYAML)
	f.write(t, "20-lists.yaml", listsYAML)
	// Ignored: other extensions, dot files, sub-directories (even *.yaml).
	f.write(t, "notes.txt", "not yaml at all: [")
	f.write(t, ".hidden.yaml", "kind: [")
	f.write(t, "30-other.yml", "kind: [")
	if err := os.Mkdir(filepath.Join(f.dir, "sub.yaml"), 0o750); err != nil {
		t.Fatal(err)
	}

	rep := f.l.Load(ctx(t))
	if len(rep.Diagnoses) != 0 {
		t.Fatalf("unexpected diagnoses: %v", rep.Diagnoses)
	}
	wantSources := []string{"kudurru.yaml", "objects.d/10-zones.yaml", "objects.d/20-lists.yaml"}
	if !slices.Equal(rep.Sources, wantSources) {
		t.Fatalf("sources = %v, want %v", rep.Sources, wantSources)
	}
	if rep.Objects != 4 || rep.Created != 4 || rep.Updated+rep.Deleted+rep.Skipped != 0 {
		t.Fatalf("report = %+v", rep)
	}

	z := zone(t, f.st, "home")
	if z == nil || store.ManagedBy(z) != "file:objects.d/10-zones.yaml" {
		t.Fatalf("zone not applied with the file manager: %+v", z)
	}
	// spec.policy is absent from the document; its defaults still apply,
	// as they do when a driver validates the typed write.
	if z.Spec.DefaultTTL != 300 || z.Spec.Policy.QueryAccess != v1alpha1.AccessAllow {
		t.Errorf("schema defaults not applied: ttl=%d queryAccess=%q", z.Spec.DefaultTTL, z.Spec.Policy.QueryAccess)
	}
	if got := owner(t, f.st, nsKey("ClientGroup", "dns", "kids")); got != "file:objects.d/10-zones.yaml" {
		t.Errorf("ClientGroup owner = %q", got)
	}
	// Namespaced objects without a namespace land in the default namespace.
	if got := owner(t, f.st, nsKey("BlockList", "default", "ads")); got != "file:objects.d/20-lists.yaml" {
		t.Errorf("BlockList owner = %q", got)
	}
	pol := get(t, f.st, nsKey("Policy", "default", "default"), &v1alpha1.Policy{})
	if pol == nil || store.ManagedBy(pol) != "file:kudurru.yaml" || pol.Spec.Priority != 1 {
		t.Fatalf("inline policy wrong: %+v", pol)
	}
	if pol.Spec.BlockTTL.Duration.String() != "10m0s" {
		t.Errorf("policy blockTTL default not applied: %v", pol.Spec.BlockTTL)
	}

	// A second load changes nothing: no writes, resourceVersions stay.
	rv := z.ResourceVersion
	rep = f.l.Load(ctx(t))
	if rep.Unchanged != 4 || rep.Created+rep.Updated+rep.Deleted != 0 || len(rep.Diagnoses) != 0 {
		t.Fatalf("re-load report = %+v", rep)
	}
	if got := zone(t, f.st, "home").ResourceVersion; got != rv {
		t.Errorf("identical re-load wrote the zone: rv %s -> %s", rv, got)
	}
}

func TestLoadUpdatesAndDeletes(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "zones.yaml", zonesYAML)
	f.write(t, "lists.yaml", listsYAML)
	f.l.Load(ctx(t))

	// Change one document, drop the other from the same file.
	f.write(t, "zones.yaml", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home, labels: { site: berlin } }
spec: { zoneName: home.example., type: primary, defaultTTL: 60 }
`)
	rep := f.l.Load(ctx(t))
	if rep.Updated != 1 || rep.Deleted != 1 || rep.Unchanged != 1 {
		t.Fatalf("report = %+v", rep)
	}
	z := zone(t, f.st, "home")
	if z.Spec.DefaultTTL != 60 || z.Labels["site"] != "berlin" || z.Generation != 2 {
		t.Fatalf("zone not updated: ttl=%d labels=%v gen=%d", z.Spec.DefaultTTL, z.Labels, z.Generation)
	}
	if owner(t, f.st, nsKey("ClientGroup", "dns", "kids")) != "" {
		t.Fatal("ClientGroup removed from its file must be deleted")
	}

	// Deleting a file deletes its objects.
	f.remove(t, "lists.yaml")
	rep = f.l.Load(ctx(t))
	if rep.Deleted != 1 || owner(t, f.st, nsKey("BlockList", "default", "ads")) != "" {
		t.Fatalf("objects of a deleted file must be deleted: %+v", rep)
	}

	// Emptying a file (comments only) deletes its objects too.
	f.write(t, "zones.yaml", "# nothing here\n---\n")
	rep = f.l.Load(ctx(t))
	if rep.Deleted != 1 || zone(t, f.st, "home") != nil {
		t.Fatalf("emptied file must delete its objects: %+v", rep)
	}
}

func TestLoadMoveBetweenFiles(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "a.yaml", listsYAML)
	f.l.Load(ctx(t))
	uid := get(t, f.st, nsKey("BlockList", "default", "ads"), &v1alpha1.BlockList{}).UID

	f.remove(t, "a.yaml")
	f.write(t, "b.yaml", listsYAML)
	rep := f.l.Load(ctx(t))
	d := findDiag(t, rep.Diagnoses, ReasonMoved)
	if d.Severity != SeverityInfo || d.Owner != "file:objects.d/a.yaml" || d.Source != "objects.d/b.yaml" {
		t.Fatalf("move diagnosis = %+v", d)
	}
	bl := get(t, f.st, nsKey("BlockList", "default", "ads"), &v1alpha1.BlockList{})
	if bl == nil || store.ManagedBy(bl) != "file:objects.d/b.yaml" || bl.UID != uid || rep.Deleted != 0 {
		t.Fatalf("moved object must change owner in place: %+v rep=%+v", bl, rep)
	}
}

func TestLoadDuplicateAcrossFiles(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "a.yaml", zonesYAML)
	f.write(t, "b.yaml", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home }
spec: { zoneName: home.example., type: primary, defaultTTL: 30 }
`)
	rep := f.l.Load(ctx(t))
	d := findDiag(t, rep.Diagnoses, ReasonDuplicate)
	if d.Severity != SeverityError || d.Source != "objects.d/b.yaml" || d.Document != 1 || d.Object != "DNSZone/home" || d.Message != "already declared in objects.d/a.yaml" {
		t.Fatalf("duplicate diagnosis = %+v", d)
	}
	z := zone(t, f.st, "home")
	if store.ManagedBy(z) != "file:objects.d/a.yaml" || z.Spec.DefaultTTL != 300 {
		t.Fatalf("first declaration must win: %s ttl=%d", store.ManagedBy(z), z.Spec.DefaultTTL)
	}
	if rep.Created != 2 || rep.Skipped != 1 {
		t.Fatalf("report = %+v", rep)
	}

	// Duplicates inside one file are reported the same way.
	f.write(t, "b.yaml", listsYAML+"---\n"+listsYAML)
	rep = f.l.Load(ctx(t))
	d = findDiag(t, rep.Diagnoses, ReasonDuplicate)
	if d.Source != "objects.d/b.yaml" || d.Document != 2 || d.Message != "already declared in objects.d/b.yaml" {
		t.Fatalf("in-file duplicate diagnosis = %+v", d)
	}
}

func TestLoadAPIOwnedConflict(t *testing.T) {
	f := newFixture(t, nil)
	api := &v1alpha1.DNSZone{Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypeForward}}
	api.Name = "home"
	if _, err := f.st.Apply(ctx(t), api, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	f.write(t, "zones.yaml", zonesYAML)
	rep := f.l.Load(ctx(t))
	d := findDiag(t, rep.Diagnoses, ReasonManagedByOther)
	if d.Severity != SeverityError || d.Owner != "api" || d.Object != "DNSZone/home" || d.Source != "objects.d/zones.yaml" || d.Hint == "" {
		t.Fatalf("conflict diagnosis = %+v", d)
	}
	if z := zone(t, f.st, "home"); store.ManagedBy(z) != "api" || z.Spec.Type != v1alpha1.ZoneTypeForward {
		t.Fatalf("api-owned zone must be left alone: %+v", z)
	}
	// The rest of the file is applied.
	if owner(t, f.st, nsKey("ClientGroup", "dns", "kids")) == "" || rep.Created != 1 || rep.Skipped != 1 {
		t.Fatalf("the other document must still be applied: %+v", rep)
	}
	// Removing the document never deletes the api-owned object.
	f.remove(t, "zones.yaml")
	f.l.Load(ctx(t))
	if owner(t, f.st, zoneKey("home")) != "api" {
		t.Fatal("api-owned object deleted by the loader")
	}
}

func TestLoadTakesOverBootstrapAndAdoptsUnowned(t *testing.T) {
	f := newFixture(t, inlinePolicy)
	seed := &v1alpha1.Policy{Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}}
	seed.Name, seed.Namespace = "default", "default"
	if _, err := f.st.Apply(ctx(t), seed, store.ApplyOptions{Manager: store.ManagerBootstrap}); err != nil {
		t.Fatal(err)
	}
	rep := f.l.Load(ctx(t))
	d := findDiag(t, rep.Diagnoses, ReasonTakenOver)
	if d.Severity != SeverityInfo || d.Owner != store.ManagerBootstrap {
		t.Fatalf("takeover diagnosis = %+v", d)
	}
	pol := get(t, f.st, nsKey("Policy", "default", "default"), &v1alpha1.Policy{})
	if store.ManagedBy(pol) != "file:kudurru.yaml" || len(pol.Spec.BlockListRefs) != 1 {
		t.Fatalf("bootstrap policy not replaced: %+v", pol)
	}
}

func TestLoadDriftIsRepaired(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "zones.yaml", zonesYAML)
	f.l.Load(ctx(t))
	if err := f.st.Delete(ctx(t), zoneKey("home"), store.DeleteOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	if rep := f.l.Load(ctx(t)); rep.Created != 1 || zone(t, f.st, "home") == nil {
		t.Fatalf("deleted file-managed object must be recreated: %+v", rep)
	}

	// A write under the file's own manager (or a status update) moves the
	// resourceVersion: the fast path must not hide the drift.
	z := zone(t, f.st, "home")
	z.Spec.DefaultTTL = 5
	if _, err := f.st.Apply(ctx(t), z, store.ApplyOptions{Manager: "file:objects.d/zones.yaml"}); err != nil {
		t.Fatal(err)
	}
	if rep := f.l.Load(ctx(t)); rep.Updated != 1 || zone(t, f.st, "home").Spec.DefaultTTL != 300 {
		t.Fatalf("modified file-managed object must be restored: %+v", rep)
	}
	z = zone(t, f.st, "home")
	z.Status.RecordCount = 3
	if err := f.st.UpdateStatus(ctx(t), z); err != nil {
		t.Fatal(err)
	}
	if rep := f.l.Load(ctx(t)); rep.Unchanged != 2 || rep.Updated != 0 {
		t.Fatalf("a status update is not drift: %+v", rep)
	}
}

func TestLoadKindList(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "backup.yaml", `
apiVersion: v1
kind: List
items:
- apiVersion: kudurru.io/v1alpha1
  kind: DNSZone
  metadata: { name: home, uid: x-1, resourceVersion: "42", generation: 7, creationTimestamp: "2026-01-01T00:00:00Z",
              annotations: { kudurru.io/managed-by: api, note: kept } }
  spec: { zoneName: home.example., type: primary }
  status: { serial: 99, recordCount: 3 }
- apiVersion: kudurru.io/v1alpha1
  kind: BlockList
  metadata: { name: ads, namespace: dns }
  spec: { sources: [{ url: "https://lists.example/" }] }
`)
	rep := f.l.Load(ctx(t))
	if rep.Created != 2 || len(rep.Diagnoses) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	z := zone(t, f.st, "home")
	if store.ManagedBy(z) != "file:objects.d/backup.yaml" || z.Annotations["note"] != "kept" || z.Status.Serial != 0 || z.UID == "x-1" {
		t.Fatalf("snapshot metadata must be stripped, managedBy replaced: %+v", z.ObjectMeta)
	}
}

func TestLoadDirectoryMissingOrUnset(t *testing.T) {
	f := newFixture(t, inlinePolicy, func(o *Options) { o.Dir = filepath.Join(t.TempDir(), "missing") })
	rep := f.l.Load(ctx(t))
	if rep.Created != 1 || len(rep.Diagnoses) != 0 || !slices.Equal(rep.Sources, []string{"kudurru.yaml"}) {
		t.Fatalf("missing directory must be treated as empty: %+v", rep)
	}
	g := newFixture(t, nil, func(o *Options) { o.Dir = "" })
	if rep := g.l.Load(ctx(t)); rep.Objects != 0 || len(rep.Diagnoses) != 0 {
		t.Fatalf("no sources: %+v", rep)
	}
}

func TestLoadUnreadableDirectoryKeepsObjects(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "zones.yaml", zonesYAML)
	f.l.Load(ctx(t))
	// Replace the directory by a file: reading it fails with ENOTDIR, which
	// (unlike a missing directory) must not count as "no objects". A
	// permission change would not do: the test may run with
	// CAP_DAC_OVERRIDE.
	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rep := f.l.Load(ctx(t))
	d := findDiag(t, rep.Diagnoses, ReasonReadFailed)
	if d.Severity != SeverityWarning || d.Source != "objects.d" {
		t.Fatalf("read diagnosis = %+v", d)
	}
	if rep.Deleted != 0 || zone(t, f.st, "home") == nil {
		t.Fatalf("objects of an unreadable directory must be kept: %+v", rep)
	}
}

func TestStatusMetricsAndReady(t *testing.T) {
	f := newFixture(t, inlinePolicy, func(o *Options) { o.Static = true })
	if ok, msgs := f.l.Ready(); ok || len(msgs) != 1 {
		t.Fatalf("ready before the first load: %v %v", ok, msgs)
	}
	f.write(t, "zones.yaml", zonesYAML)
	f.write(t, "bad.yaml", "apiVersion: kudurru.io/v1alpha1\nkind: DNSZone\nmetadata: {name: bad}\nspec: {zoneName: bad, type: primary}\n")
	f.l.Load(ctx(t))

	st := f.l.Status()
	if !st.Loaded || st.Files != 3 || st.Objects != 4 || st.Errors != 1 || !st.APIReadOnly || st.Dir != f.dir {
		t.Fatalf("status = %+v", st)
	}
	ok, msgs := f.l.Ready()
	if !ok || len(msgs) != 1 || msgs[0] == "" {
		t.Fatalf("file problems must not make the node unready, only be reported: %v %v", ok, msgs)
	}
	if err := f.l.CheckAPIWrite(); err != ErrStaticReadOnly || !f.l.APIReadOnly() {
		t.Fatalf("static mode must refuse API writes: %v", err)
	}

	if got := testutil.ToFloat64(f.l.m.files); got != 3 {
		t.Errorf("kudurru_objects_files = %v", got)
	}
	if got := testutil.ToFloat64(f.l.m.applied.WithLabelValues(ResultCreated)); got != 3 {
		t.Errorf("created = %v", got)
	}
	if got := testutil.ToFloat64(f.l.m.applied.WithLabelValues(ResultInvalid)); got != 1 {
		t.Errorf("invalid = %v", got)
	}
	f.l.Load(ctx(t))
	if got := testutil.ToFloat64(f.l.m.applied.WithLabelValues(ResultUnchanged)); got != 3 {
		t.Errorf("unchanged = %v", got)
	}
	n, err := testutil.GatherAndCount(f.reg, "kudurru_objects_files", "kudurru_objects_applied_total", "kudurru_objects_diagnoses", "kudurru_objects_last_load_timestamp_seconds")
	if err != nil || n != 1+len(results)+3+1 {
		t.Fatalf("registered series = %d, %v", n, err)
	}

	// A second loader on the same registry reuses the collectors.
	if _, err := New(Options{Store: f.st, Validator: testValidator(t), Registerer: f.reg}); err != nil {
		t.Fatal(err)
	}
	w, err := New(Options{Store: memoryStore(t), Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	if w.CheckAPIWrite() != nil || w.APIReadOnly() {
		t.Fatal("non-static loaders allow API writes")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a store is required")
	}
	if _, err := New(Options{Store: memoryStore(t), Dir: "/etc/kudurru.yaml"}); err == nil {
		t.Fatal("a directory named like the inline source must be rejected")
	}
	if Manager("objects.d/a.yaml") != "file:objects.d/a.yaml" {
		t.Fatal("Manager")
	}
}
