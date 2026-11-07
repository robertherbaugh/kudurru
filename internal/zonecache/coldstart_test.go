package zonecache

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

func openDB(t testing.TB) (*localdb.SnapshotStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), localdb.FileName)
	db, err := localdb.OpenSnapshotStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func load(t testing.TB, db *localdb.SnapshotStore) *localdb.Snapshot {
	t.Helper()
	snap, err := db.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func loadCount(db *localdb.SnapshotStore) int {
	snap, err := db.Load(context.Background())
	if err != nil {
		return -1
	}
	return len(snap.Objects)
}

func TestPersistThenColdStartThenSwap(t *testing.T) {
	db, _ := openDB(t)

	// A live replica writes local.db after its sync and on change.
	live := newFakeStore()
	live.put(zone("home", "home.example."), true)
	live.put(rrset("apps", "nas", "home", map[string]string{"team": "infra"}), true)
	live.put(rrset("dns", "ns1", "home", nil), true)
	live.put(policy("apps", "kids", "ads"), true)
	a := newCache(t, live, Options{LocalDB: db, PersistInterval: 50 * time.Millisecond, Debounce: time.Millisecond})
	if err := a.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	stopA := startCache(t, a)
	waitState(t, a, Live)
	eventually(t, 5*time.Second, "the post-sync snapshot", func() bool { return loadCount(db) == 4 })
	live.put(rrset("apps", "web", "home", nil), true)
	eventually(t, 5*time.Second, "the changed snapshot", func() bool { return loadCount(db) == 5 })
	if testutil.ToFloat64(a.m.snapshotWrites.WithLabelValues("ok")) < 2 || testutil.ToFloat64(a.m.lastWrite) == 0 {
		t.Error("snapshot write metrics not updated")
	}
	persisted := load(t, db)
	if persisted.ClusterID != "fake-cluster" {
		t.Errorf("cluster ID %q", persisted.ClusterID)
	}
	for _, k := range DefaultKinds() {
		if persisted.ResourceVersions[k] == "" {
			t.Errorf("no resourceVersion recorded for %s", k)
		}
	}
	wantView := zoneSummary{"home": {"apps/nas", "apps/web", "dns/ns1"}}
	if zs, _ := View[zoneSummary](a.Snapshot(), "zones"); !reflect.DeepEqual(zs, wantView) {
		t.Fatalf("live view %v", zs)
	}
	stopA()

	// A restarted replica whose store never syncs serves local.db.
	cold := newFakeStore()
	cold.holdBookmarks()
	cold.put(zone("home", "home.example."), true)
	cold.put(rrset("apps", "moved", "home", nil), true)
	reg := prometheus.NewRegistry()
	b := newCache(t, cold, Options{LocalDB: db, Registerer: reg, StartupTimeout: 100 * time.Millisecond,
		PersistInterval: 20 * time.Millisecond, Debounce: time.Millisecond})
	if err := b.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	startCache(t, b)
	select {
	case <-b.Initialized():
	case <-time.After(5 * time.Second):
		t.Fatal("not initialized from local.db")
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Errorf("fell back after %s, before StartupTimeout", waited)
	}
	snap := waitState(t, b, FromSnapshot)
	if zs, ok := View[zoneSummary](snap, "zones"); !ok || !reflect.DeepEqual(zs, wantView) {
		t.Fatalf("view from local.db %v, want %v", zs, wantView)
	}
	if !snap.Authoritative() || snap.Stale || !snap.SnapshotWrittenAt.Equal(persisted.WrittenAt) {
		t.Fatalf("authoritative=%v stale=%v writtenAt=%s", snap.Authoritative(), snap.Stale, snap.SnapshotWrittenAt)
	}
	if snap.Objects.ResourceVersion(KindDNSRecordSet) != persisted.ResourceVersions[KindDNSRecordSet] {
		t.Error("the snapshot's resourceVersions are not carried")
	}
	ok, msgs := b.Ready()
	if !ok || len(msgs) != 1 || !strings.Contains(msgs[0], ReasonServingFromSnapshot) {
		t.Fatalf("Ready() from a fresh snapshot = %v %v", ok, msgs)
	}
	if conds := snap.Degraded(); len(conds) != 1 || conds[0].Reason != ReasonServingFromSnapshot {
		t.Fatalf("degraded %v", conds)
	}
	if testutil.ToFloat64(b.m.serving) != 1 {
		t.Error("kudurru_store_snapshot_serving != 1")
	}
	if age := b.snapshotAgeSeconds(); age <= 0 || age > 60 {
		t.Errorf("kudurru_store_snapshot_age_seconds = %v", age)
	}
	if n, err := testutil.GatherAndCount(reg, "kudurru_store_snapshot_age_seconds"); err != nil || n != 1 {
		t.Errorf("age gauge not registered: %d %v", n, err)
	}
	// Nothing is written back while serving the snapshot.
	time.Sleep(100 * time.Millisecond)
	if again := load(t, db); !again.WrittenAt.Equal(persisted.WrittenAt) {
		t.Fatal("local.db rewritten while serving from it")
	}

	// The store syncs: the snapshot is swapped out for the live set.
	cold.releaseBookmarks()
	snap = waitState(t, b, Live)
	if zs, _ := View[zoneSummary](snap, "zones"); !reflect.DeepEqual(zs, zoneSummary{"home": {"apps/moved"}}) {
		t.Fatalf("live view after the swap %v", zs)
	}
	if !snap.Objects.Changed(KindPolicy) {
		t.Error("the source switch must report every kind changed")
	}
	if testutil.ToFloat64(b.m.serving) != 0 || b.snapshotAgeSeconds() != 0 || len(b.Degraded()) != 0 {
		t.Errorf("after the swap: serving=%v age=%v degraded=%v", testutil.ToFloat64(b.m.serving), b.snapshotAgeSeconds(), b.Degraded())
	}
	if ok, msgs := b.Ready(); !ok || len(msgs) != 0 {
		t.Fatalf("Ready() live = %v %v", ok, msgs)
	}
	eventually(t, 5*time.Second, "the live set in local.db", func() bool { return loadCount(db) == 2 })
}

func TestColdStartStaleSnapshot(t *testing.T) {
	db, _ := openDB(t)
	written := time.Now().Add(-25 * time.Hour)
	if err := db.Write(context.Background(), &localdb.Snapshot{
		Objects:          []store.Object{zone("home", "home.example.")},
		ResourceVersions: map[store.Kind]string{KindDNSZone: "7"},
		WrittenAt:        written,
	}); err != nil {
		t.Fatal(err)
	}
	st := newFakeStore()
	st.holdBookmarks()
	c := newCache(t, st, Options{LocalDB: db, StartupTimeout: 20 * time.Millisecond, Debounce: time.Millisecond})
	if err := c.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	startCache(t, c)
	snap := waitState(t, c, FromSnapshot)
	if !snap.Stale || snap.Authoritative() {
		t.Fatalf("stale=%v authoritative=%v for a 25 h old snapshot", snap.Stale, snap.Authoritative())
	}
	// Forwarding-side views still build from it.
	if zs, ok := View[zoneSummary](snap, "zones"); !ok || len(zs) != 1 {
		t.Fatalf("view %v %v", zs, ok)
	}
	ok, msgs := c.Ready()
	if ok || len(msgs) != 1 || !strings.Contains(msgs[0], "older than maxAge") {
		t.Fatalf("Ready() = %v %v", ok, msgs)
	}
	reasons := []string{}
	for _, cond := range c.Degraded() {
		reasons = append(reasons, cond.Reason)
	}
	if !reflect.DeepEqual(reasons, []string{ReasonServingFromSnapshot, ReasonSnapshotStale}) {
		t.Fatalf("conditions %v", reasons)
	}
	if age := c.snapshotAgeSeconds(); age < 25*3600 {
		t.Errorf("age %v", age)
	}
}

func TestSnapshotCrossesMaxAgeWhileServing(t *testing.T) {
	db, _ := openDB(t)
	const maxAge = time.Hour
	if err := db.Write(context.Background(), &localdb.Snapshot{
		Objects:   []store.Object{zone("home", "home.example.")},
		WrittenAt: time.Now().Add(-maxAge + 400*time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	st := newFakeStore()
	st.holdBookmarks()
	c := newCache(t, st, Options{LocalDB: db, StartupTimeout: 10 * time.Millisecond, MaxAge: maxAge, Debounce: time.Millisecond})
	startCache(t, c)
	snap := waitState(t, c, FromSnapshot)
	if snap.Stale || !snap.Authoritative() {
		t.Fatal("stale before maxAge")
	}
	if ok, _ := c.Ready(); !ok {
		t.Fatal("not ready before maxAge")
	}
	eventually(t, 5*time.Second, "a republished stale snapshot", func() bool { return c.Snapshot().Stale })
	if c.Snapshot().Authoritative() {
		t.Fatal("authoritative after maxAge")
	}
	if ok, _ := c.Ready(); ok {
		t.Fatal("ready after maxAge")
	}
}

func TestUnsyncedWithoutSnapshot(t *testing.T) {
	emptyDB, _ := openDB(t)
	for _, tc := range []struct {
		name string
		db   *localdb.SnapshotStore
		why  string
	}{
		{"empty local.db", emptyDB, "local.db holds no snapshot yet"},
		{"no local.db", nil, "no local.db configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.holdBookmarks()
			st.put(zone("home", "home.example."), true)
			c := newCache(t, st, Options{LocalDB: tc.db, StartupTimeout: 10 * time.Millisecond, Debounce: time.Millisecond})
			startCache(t, c)
			eventually(t, 5*time.Second, "the fallback decision", func() bool {
				_, msgs := c.Ready()
				return len(msgs) == 2
			})
			ok, msgs := c.Ready()
			if ok || !strings.Contains(msgs[0], "waiting for the initial sync of BlockList, ClientGroup, DNSRecordSet, DNSZone, Integration, Kudurru, Policy") || msgs[1] != tc.why {
				t.Fatalf("Ready() = %v %q", ok, msgs)
			}
			snap := c.Snapshot()
			if snap.State != Unsynced || snap.Authoritative() || c.State() != Unsynced {
				t.Fatalf("state %s authoritative %v", snap.State, snap.Authoritative())
			}
			select {
			case <-c.Initialized():
				t.Fatal("initialized without sync or snapshot")
			default:
			}
			st.releaseBookmarks()
			waitState(t, c, Live)
			if ok, _ := c.Ready(); !ok {
				t.Fatal("not ready after sync")
			}
		})
	}
}

func TestRefreshKeepsSnapshotAgeHonest(t *testing.T) {
	db, _ := openDB(t)
	st := newFakeStore()
	st.put(zone("home", "home.example."), true)
	c := newCache(t, st, Options{LocalDB: db, PersistInterval: 20 * time.Millisecond, RefreshInterval: 100 * time.Millisecond, Debounce: time.Millisecond})
	startCache(t, c)
	waitState(t, c, Live)
	eventually(t, 5*time.Second, "the first write", func() bool { return loadCount(db) == 1 })
	first := load(t, db).WrittenAt
	eventually(t, 5*time.Second, "a refreshed timestamp without changes", func() bool {
		return load(t, db).WrittenAt.After(first.Add(90 * time.Millisecond))
	})
	if loadCount(db) != 1 {
		t.Fatal("refresh changed the objects")
	}
}

// contactStore reports a settable Info().LastContact, as the kube driver
// does.
type contactStore struct {
	*fakeStore
	last atomic.Int64 // UnixNano
}

func (c *contactStore) Info() store.Info {
	info := c.fakeStore.Info()
	info.LastContact = time.Unix(0, c.last.Load())
	return info
}

// TestRefreshUsesLastContact: with a driver that reports LastContact, the
// local.db timestamp follows it rather than the wall clock, and stays put
// while the contact is frozen (an apiserver outage).
func TestRefreshUsesLastContact(t *testing.T) {
	db, _ := openDB(t)
	st := &contactStore{fakeStore: newFakeStore()}
	contacted := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	st.last.Store(contacted.UnixNano())
	st.put(zone("home", "home.example."), true)
	c := newCache(t, st, Options{LocalDB: db, PersistInterval: 10 * time.Millisecond, RefreshInterval: 30 * time.Millisecond, Debounce: time.Millisecond})
	startCache(t, c)
	waitState(t, c, Live)
	eventually(t, 5*time.Second, "the first write", func() bool { return loadCount(db) == 1 })
	if got := load(t, db).WrittenAt; !got.Equal(contacted) {
		t.Fatalf("first write stamped %s, want the last contact %s", got, contacted)
	}

	later := contacted.Add(20 * time.Second)
	st.last.Store(later.UnixNano())
	eventually(t, 5*time.Second, "a refresh to the new contact", func() bool { return load(t, db).WrittenAt.Equal(later) })

	// Contact frozen: several refresh intervals pass without a touch.
	time.Sleep(10 * 30 * time.Millisecond)
	if got := load(t, db).WrittenAt; !got.Equal(later) {
		t.Fatalf("timestamp moved to %s while the contact was frozen at %s", got, later)
	}
}

func TestFinalWriteOnShutdown(t *testing.T) {
	db, _ := openDB(t)
	st := newFakeStore()
	st.put(zone("home", "home.example."), true)
	c := newCache(t, st, Options{LocalDB: db, PersistInterval: time.Hour, Debounce: time.Millisecond})
	stop := startCache(t, c)
	waitState(t, c, Live)
	eventually(t, 5*time.Second, "the post-sync write", func() bool { return loadCount(db) == 1 })
	st.put(zone("lab", "lab.example."), true)
	eventually(t, 5*time.Second, "the change", func() bool { return c.Snapshot().Objects.Count(KindDNSZone) == 2 })
	stop()
	if n := loadCount(db); n != 2 {
		t.Fatalf("local.db has %d objects after shutdown, want 2", n)
	}
}

func TestSnapshotWriteFailure(t *testing.T) {
	db, _ := openDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st := newFakeStore()
	st.put(zone("home", "home.example."), true)
	c := newCache(t, st, Options{LocalDB: db, PersistInterval: 20 * time.Millisecond, Debounce: time.Millisecond})
	startCache(t, c)
	waitState(t, c, Live)
	eventually(t, 5*time.Second, "a failed write", func() bool { return testutil.ToFloat64(c.m.snapshotWrites.WithLabelValues("error")) >= 2 })
	conds := c.Degraded()
	if len(conds) != 1 || conds[0].Reason != ReasonSnapshotWriteFailed {
		t.Fatalf("conditions %v", conds)
	}
	if ok, msgs := c.Ready(); !ok || len(msgs) != 1 {
		t.Fatalf("a failing local.db must not cost readiness: %v %v", ok, msgs)
	}
}
