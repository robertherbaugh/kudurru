// Package conformance is the test suite every Store driver must pass. Run it
// from the driver's own test package with a factory that returns a fresh,
// empty store.
package conformance

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Run executes the suite.
func Run(t *testing.T, factory func(t *testing.T) store.Store) {
	t.Helper()
	t.Run("ApplyCreatesWithManagedBy", func(t *testing.T) { testApplyCreate(t, factory(t)) })
	t.Run("ApplyRejectsOtherManager", func(t *testing.T) { testManagedBy(t, factory(t)) })
	t.Run("ApplyOptimisticConcurrency", func(t *testing.T) { testOptimistic(t, factory(t)) })
	t.Run("DeleteAndNotFound", func(t *testing.T) { testDelete(t, factory(t)) })
	t.Run("DeleteAllCascade", func(t *testing.T) { testDeleteAll(t, factory(t)) })
	t.Run("ListFiltersByKindNamespaceLabels", func(t *testing.T) { testList(t, factory(t)) })
	t.Run("WatchDeliversInitialAndLive", func(t *testing.T) { testWatch(t, factory(t)) })
	t.Run("UpdateStatusKeepsSpecAndManager", func(t *testing.T) { testStatus(t, factory(t)) })
	t.Run("UpdateStatusOptimisticConcurrency", func(t *testing.T) { testStatusOptimistic(t, factory(t)) })
	t.Run("ApplyIgnoresStatusAndStatusIgnoresSpec", func(t *testing.T) { testStatusSeparation(t, factory(t)) })
	t.Run("SlowWatcherGetsClosedOrResynced", func(t *testing.T) { testSlowWatcher(t, factory(t)) })
}

func zone(name, zoneName string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: zoneName, Type: v1alpha1.ZoneTypePrimary},
	}
}

func testApplyCreate(t *testing.T, s store.Store) {
	ctx := context.Background()
	out, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	if store.ManagedBy(out) != store.ManagerAPI {
		t.Fatalf("managedBy not recorded: %q", store.ManagedBy(out))
	}
	if out.GetResourceVersion() == "" || out.GetUID() == "" || out.GetGeneration() != 1 {
		t.Fatalf("identity not stamped: rv=%q uid=%q gen=%d", out.GetResourceVersion(), out.GetUID(), out.GetGeneration())
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.ZoneName != "home.example." {
		t.Fatalf("round trip lost spec: %+v", got.Spec)
	}
}

func testManagedBy(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerFile("objects.d/zones.yaml")}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	var mb *store.ErrManagedBy
	if !errors.As(err, &mb) || mb.Owner != "file:objects.d/zones.yaml" {
		t.Fatalf("expected ErrManagedBy from the file owner, got %v", err)
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatalf("Force must override managedBy: %v", err)
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("after Force the new manager owns the object: %v", err)
	}
	if _, err := s.Apply(ctx, zone("x", "x."), store.ApplyOptions{}); err == nil {
		t.Fatal("empty Manager must be rejected")
	}
}

func testOptimistic(t *testing.T, s store.Store) {
	ctx := context.Background()
	first, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	changed := zone("home", "home.example.")
	changed.Spec.DefaultTTL = 600
	second, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if second.GetResourceVersion() == first.GetResourceVersion() {
		t.Fatal("resourceVersion must change when the spec changes")
	}
	if second.GetGeneration() != 2 {
		t.Fatalf("generation should be 2 after a spec change, got %d", second.GetGeneration())
	}
	// An identical re-apply is a no-op for the apiserver: generation must not
	// move (resourceVersion may or may not).
	same, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	if same.GetGeneration() != 2 {
		t.Fatalf("identical re-apply must not bump generation, got %d", same.GetGeneration())
	}
	_, err = s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale resourceVersion must be ErrConflict, got %v", err)
	}
}

// testStatusSeparation pins the apiserver's split: Apply writes the main
// resource and ignores status; UpdateStatus writes the status subresource
// and ignores spec (docs/architecture.md §4.3, decision D70).
func testStatusSeparation(t *testing.T, s store.Store) {
	ctx := context.Background()
	z := zone("home", "home.example.")
	z.Status.Serial = 99
	out, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	if out.(*v1alpha1.DNSZone).Status.Serial != 0 {
		t.Fatalf("Apply must ignore status, got serial %d", out.(*v1alpha1.DNSZone).Status.Serial)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Serial != 0 {
		t.Fatalf("stored status must be empty after Apply, got serial %d", got.Status.Serial)
	}
	storedTTL := got.Spec.DefaultTTL // whatever the driver stored (a real apiserver defaults it)
	got.Status.Serial = 7
	got.Spec.DefaultTTL = storedTTL + 1234 // must be ignored
	if err := s.UpdateStatus(ctx, &got); err != nil {
		t.Fatal(err)
	}
	var after v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Serial != 7 || after.Spec.DefaultTTL != storedTTL {
		t.Fatalf("UpdateStatus must write status only: serial=%d defaultTTL=%d (stored %d)", after.Status.Serial, after.Spec.DefaultTTL, storedTTL)
	}
	if after.GetGeneration() != 1 {
		t.Fatalf("status writes must not bump generation: %d", after.GetGeneration())
	}
}

func testDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := store.Key{Kind: "DNSZone", Name: "home"}
	if err := s.Delete(ctx, key, store.DeleteOptions{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete of a missing object must be ErrNotFound, got %v", err)
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerIntegration("unifi")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI}); !store.IsManagedBy(err) {
		t.Fatalf("delete by another manager must be ErrManagedBy, got %v", err)
	}
	if err := s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerIntegration("unifi")}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, key, &got); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

// testDeleteAll checks store.DeleteAll, the zone cascade: a driver with
// DeleteMany deletes all or nothing; any other driver deletes in order and
// stops at the first failure, reporting how many went.
func testDeleteAll(t *testing.T, s store.Store) {
	ctx := context.Background()
	rs := func(ns, name, manager string) store.Key {
		t.Helper()
		o := &v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"}, Endpoints: []v1alpha1.Endpoint{{DNSName: name + ".home.example", RecordType: "A", Targets: []string{"10.0.0.1"}}}},
		}
		if _, err := s.Apply(ctx, o, store.ApplyOptions{Manager: manager}); err != nil {
			t.Fatal(err)
		}
		return store.KeyOf(o)
	}
	exists := func(key store.Key) bool {
		t.Helper()
		var into store.Object = &v1alpha1.DNSRecordSet{}
		if key.Kind == "DNSZone" {
			into = &v1alpha1.DNSZone{}
		}
		err := s.Get(ctx, key, into)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	api := store.DeleteOptions{Manager: store.ManagerAPI}

	// Success: every object goes; a missing one is skipped.
	a, b := rs("apps", "a", store.ManagerAPI), rs("dns", "b", store.ManagerAPI)
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	z := store.Key{Kind: "DNSZone", Name: "home"}
	missing := store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "gone"}
	reqs := []store.DeleteRequest{{Key: a, Options: api}, {Key: missing, Options: api}, {Key: b, Options: api}, {Key: z, Options: api}}
	n, _, err := store.DeleteAll(ctx, s, reqs)
	if err != nil || n != len(reqs) {
		t.Fatalf("DeleteAll = %d, %v; want %d, nil", n, err, len(reqs))
	}
	for _, k := range []store.Key{a, b, z} {
		if exists(k) {
			t.Fatalf("%s survived DeleteAll", k)
		}
	}

	// Failure: the second request is managed by another writer.
	c, d, e := rs("apps", "c", store.ManagerAPI), rs("apps", "d", store.ManagerFile("d.yaml")), rs("apps", "e", store.ManagerAPI)
	n, atomic, err := store.DeleteAll(ctx, s, []store.DeleteRequest{{Key: c, Options: api}, {Key: d, Options: api}, {Key: e, Options: api}})
	if !store.IsManagedBy(err) {
		t.Fatalf("DeleteAll over a file-managed object: %v, want ErrManagedBy", err)
	}
	if !exists(d) || !exists(e) {
		t.Fatal("DeleteAll deleted past the failing request")
	}
	switch {
	case atomic && (n != 0 || !exists(c)):
		t.Fatalf("atomic DeleteAll failed but deleted %d (c exists: %v)", n, exists(c))
	case !atomic && (n != 1 || exists(c)):
		t.Fatalf("sequential DeleteAll reported %d deleted (c exists: %v); want 1 and c gone", n, exists(c))
	}
}

func testList(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(ns, name, label string) *v1alpha1.DNSRecordSet {
		return &v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"team": label}},
			Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"}, Endpoints: []v1alpha1.Endpoint{{DNSName: name + ".home.example", RecordType: "A", Targets: []string{"10.0.0.1"}}}},
		}
	}
	for _, o := range []*v1alpha1.DNSRecordSet{mk("apps", "nas", "infra"), mk("apps", "web", "web"), mk("dns", "ns1", "infra")} {
		if _, err := s.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	all, err := s.List(ctx, "DNSRecordSet", store.ListOptions{})
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("expected 3 record sets, got %d (%v)", len(all.Items), err)
	}
	ns, _ := s.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: "apps"})
	if len(ns.Items) != 2 {
		t.Fatalf("namespace filter: got %d", len(ns.Items))
	}
	lbl, _ := s.List(ctx, "DNSRecordSet", store.ListOptions{LabelSelector: map[string]string{"team": "infra"}})
	if len(lbl.Items) != 2 {
		t.Fatalf("label filter: got %d", len(lbl.Items))
	}
	page, _ := s.List(ctx, "DNSRecordSet", store.ListOptions{Limit: 2})
	if len(page.Items) != 2 || page.Continue == "" {
		t.Fatalf("pagination: got %d items, continue=%q", len(page.Items), page.Continue)
	}
	rest, _ := s.List(ctx, "DNSRecordSet", store.ListOptions{Limit: 2, Continue: page.Continue})
	if len(rest.Items) != 1 || rest.Continue != "" {
		t.Fatalf("second page: got %d items, continue=%q", len(rest.Items), rest.Continue)
	}
}

func testWatch(t *testing.T, s store.Store) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Apply(ctx, zone("a", "a."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	ev := <-w.ResultChan()
	if ev.Type != store.Added || ev.Object.GetName() != "a" {
		t.Fatalf("expected initial Added for a, got %+v", ev)
	}
	for ev = range w.ResultChan() {
		if ev.Type == store.Bookmark {
			break
		}
	}
	if _, err := s.Apply(ctx, zone("b", "b."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	ev = <-w.ResultChan()
	if ev.Type != store.Added || ev.Object.GetName() != "b" {
		t.Fatalf("expected live Added for b, got %+v", ev)
	}
	if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "b"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	ev = <-w.ResultChan()
	if ev.Type != store.Deleted || ev.Object.GetName() != "b" {
		t.Fatalf("expected Deleted for b, got %+v", ev)
	}
}

func testStatus(t *testing.T, s store.Store) {
	ctx := context.Background()
	out, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerFile("z.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	z := out.(*v1alpha1.DNSZone)
	z.Status.Serial = 42
	z.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonServing, LastTransitionTime: metav1.Now()}}
	if err := s.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Serial != 42 || got.Spec.ZoneName != "home.example." || store.ManagedBy(&got) != "file:z.yaml" {
		t.Fatalf("status update changed the wrong things: %+v", got)
	}
	if got.GetGeneration() != 1 {
		t.Fatalf("status writes must not bump generation: %d", got.GetGeneration())
	}
}
