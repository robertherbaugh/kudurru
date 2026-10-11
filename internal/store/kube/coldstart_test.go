package kube

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// TestColdStartFromLocalDB is the §4.4 proof on a dedicated control plane:
//
//  1. a live driver writes its materialised object set to local.db through
//     PersistSnapshots (initial write after sync, then on change);
//  2. the driver is closed and the control plane (apiserver and etcd) is
//     stopped;
//  3. a new driver against the dead apiserver fails WaitForSync within
//     StartupTimeout, the signal to fall back;
//  4. a zonecache-side reader opens local.db with no Kubernetes client and
//     finds every object, the per-kind resourceVersions, the cluster ID
//     and the snapshot age.
func TestColdStartFromLocalDB(t *testing.T) {
	requireEnv(t)
	assets, _ := findEnvtestAssets()
	env, cfg, admin, err := startControlPlane(assets)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = env.Stop()
		}
	}()
	ctx := context.Background()
	var ks corev1.Namespace
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ks); err != nil {
		t.Fatal(err)
	}

	s := newStoreOn(t, cfg, Options{})
	writes := []store.Object{
		zone("home", "home.example."),
		rrset("apps", "nas", map[string]string{"team": "infra"}),
		rrset("dns", "ns1", nil),
		blocklist("apps", "ads"),
		clientgroup("apps", "kids"),
	}
	for _, o := range writes {
		if _, err := s.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if !s.WaitForSync(ctx) {
		t.Fatal("informers did not sync against a live apiserver")
	}
	dbPath := filepath.Join(t.TempDir(), localdb.FileName)
	db, err := localdb.OpenSnapshotStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	pctx, stopPersist := context.WithCancel(ctx)
	persistDone := make(chan error, 1)
	go func() { persistDone <- s.PersistSnapshots(pctx, db, 100*time.Millisecond) }()
	countIs := func(n int) func() bool {
		return func() bool {
			snap, err := db.Load(ctx)
			return err == nil && len(snap.Objects) == n
		}
	}
	eventually(t, 10*time.Second, "the initial snapshot", countIs(len(writes)))
	// A change after the first write reaches local.db on the next tick.
	late := rrset("apps", "web", nil)
	if _, err := s.Apply(ctx, late, store.ApplyOptions{Manager: store.ManagerFile("objects.d/web.yaml")}); err != nil {
		t.Fatal(err)
	}
	writes = append(writes, late)
	eventually(t, 10*time.Second, "the changed snapshot", countIs(len(writes)))
	stopPersist()
	if err := <-persistDone; err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := env.Stop(); err != nil {
		t.Fatal(err)
	}
	stopped = true
	apiserverGone := time.Now()

	// A restarted pod: the apiserver is unreachable, informers cannot sync.
	cold := newStoreOn(t, cfg, Options{StartupTimeout: time.Second})
	start := time.Now()
	if cold.WaitForSync(ctx) {
		t.Fatal("WaitForSync reported success with the apiserver stopped")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("WaitForSync took %s, want about StartupTimeout (1s)", waited)
	}
	if _, err := cold.MaterializedSnapshot(ctx); !errors.Is(err, ErrNotSynced) {
		t.Fatalf("MaterializedSnapshot without sync: want ErrNotSynced, got %v", err)
	}

	// The zonecache side: local.db only, no Kubernetes client.
	reader, err := localdb.OpenSnapshotStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	snap, err := reader.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Objects) != len(writes) {
		t.Fatalf("snapshot has %d objects, want %d", len(snap.Objects), len(writes))
	}
	for _, w := range writes {
		var found store.Object
		for _, o := range snap.Objects {
			if store.KeyOf(o) == store.KeyOf(w) {
				found = o
			}
		}
		if found == nil {
			t.Fatalf("%s missing from the snapshot", store.KeyOf(w))
		}
		if found.GetUID() == "" || found.GetResourceVersion() == "" || store.ManagedBy(found) == "" {
			t.Errorf("%s lost its identity: uid=%q rv=%q managedBy=%q", store.KeyOf(w), found.GetUID(), found.GetResourceVersion(), store.ManagedBy(found))
		}
	}
	z := snap.ByKind("DNSZone")
	if len(z) != 1 || z[0].(*v1alpha1.DNSZone).Spec.ZoneName != "home.example." {
		t.Fatalf("zone not served from the snapshot: %v", z)
	}
	web := snap.ByKind("DNSRecordSet")
	if len(web) != 3 {
		t.Fatalf("record sets: %d", len(web))
	}
	for _, k := range []store.Kind{"BlockList", "ClientGroup", "DNSRecordSet", "DNSZone", "Policy"} {
		if snap.ResourceVersions[k] == "" {
			t.Errorf("no resourceVersion high-water mark for %s", k)
		}
	}
	if snap.HighWaterMark() == "" {
		t.Error("no overall high-water mark")
	}
	if snap.ClusterID != string(ks.UID) {
		t.Errorf("cluster ID %q, want kube-system UID %q", snap.ClusterID, ks.UID)
	}
	age := snap.Age(time.Now())
	if age <= 0 || age > time.Minute || !snap.WrittenAt.Before(apiserverGone) {
		t.Errorf("snapshot age %s (written %s, apiserver stopped %s)", age, snap.WrittenAt, apiserverGone)
	}
	if !snap.Fresh(24*time.Hour, time.Now()) {
		t.Error("a seconds-old snapshot must be fresh against maxAge 24h")
	}
	t.Logf("cold start: %d objects, high-water mark %s, age %s, cluster %s", len(snap.Objects), snap.HighWaterMark(), age.Round(time.Millisecond), snap.ClusterID)
}
