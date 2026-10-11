package kube

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// TestColdStartFromLocalDB is the driver's half of the §4.4 proof on a
// dedicated control plane (zonecache's TestKubeColdStart is the other
// half: since D79 zonecache writes local.db from its own watches, and
// the driver's MaterializedSnapshot/PersistSnapshots are gone):
//
//  1. a live driver syncs, reports Info().LastContact and the kube-system
//     UID as cluster ID, and its objects are written to local.db;
//  2. the driver is closed and the control plane (apiserver and etcd) is
//     stopped;
//  3. a new driver against the dead apiserver fails WaitForSync within
//     StartupTimeout, the signal to fall back, and reports no contact and
//     no cluster ID;
//  4. a reader opens local.db with no Kubernetes client and finds every
//     object with its identity, the cluster ID and the snapshot age.
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
	eventually(t, 10*time.Second, "the cluster ID", func() bool { return s.Info().ClusterID == string(ks.UID) })
	if lc := s.Info().LastContact; lc.IsZero() || time.Since(lc) > 10*time.Second {
		t.Fatalf("LastContact %s against a live apiserver", lc)
	}

	// local.db as zonecache writes it: every object plus the cluster ID.
	snap := &localdb.Snapshot{ClusterID: s.Info().ClusterID, ResourceVersions: map[store.Kind]string{}}
	for _, k := range s.kinds.kinds {
		page, err := s.List(ctx, k, store.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snap.Objects = append(snap.Objects, page.Items...)
	}
	dbPath := filepath.Join(t.TempDir(), localdb.FileName)
	db, err := localdb.OpenSnapshotStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The apiserver's graceful shutdown waits for open watches, so the
	// driver goes first (TestLastContactDuringPartition covers an outage
	// under a running driver).
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
	if info := cold.Info(); !info.LastContact.IsZero() || info.ClusterID != "" {
		t.Fatalf("a driver that never reached the apiserver reports contact %s, cluster %q", info.LastContact, info.ClusterID)
	}

	// The zonecache side: local.db only, no Kubernetes client.
	reader, err := localdb.OpenSnapshotStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Objects) != len(writes) {
		t.Fatalf("snapshot has %d objects, want %d", len(got.Objects), len(writes))
	}
	for _, w := range writes {
		var found store.Object
		for _, o := range got.Objects {
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
	z := got.ByKind("DNSZone")
	if len(z) != 1 || z[0].(*v1alpha1.DNSZone).Spec.ZoneName != "home.example." {
		t.Fatalf("zone not served from the snapshot: %v", z)
	}
	if got.ClusterID != string(ks.UID) {
		t.Errorf("cluster ID %q, want kube-system UID %q", got.ClusterID, ks.UID)
	}
	age := got.Age(time.Now())
	if age <= 0 || age > time.Minute || !got.WrittenAt.Before(apiserverGone) {
		t.Errorf("snapshot age %s (written %s, apiserver stopped %s)", age, got.WrittenAt, apiserverGone)
	}
	if !got.Fresh(24*time.Hour, time.Now()) {
		t.Error("a seconds-old snapshot must be fresh against maxAge 24h")
	}
	t.Logf("cold start: %d objects, age %s, cluster %s", len(got.Objects), age.Round(time.Millisecond), got.ClusterID)
}
