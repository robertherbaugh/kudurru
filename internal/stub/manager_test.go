package stub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

type testClock struct{ ns atomic.Int64 }

func clock() *testClock                      { c := new(testClock); c.ns.Store(time.Now().UTC().UnixNano()); return c }
func (c *testClock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

type discoverFunc func(context.Context, *v1alpha1.DNSZone) (*zone.StubData, error)

func (f discoverFunc) Discover(ctx context.Context, dz *v1alpha1.DNSZone) (*zone.StubData, error) {
	return f(ctx, dz)
}

type fakeDiscovery struct {
	clock  *testClock
	fail   atomic.Bool
	serial atomic.Uint32
	calls  atomic.Int32
}

func (f *fakeDiscovery) Discover(context.Context, *v1alpha1.DNSZone) (*zone.StubData, error) {
	f.calls.Add(1)
	if f.fail.Load() {
		return nil, errors.New("offline")
	}
	data := delegation(f.clock.now())
	data.Records[0].(*dns.SOA).Serial = f.serial.Load()
	return data, nil
}

func setupManager(t *testing.T, c *testClock, d Discoverer, blobs localdb.BlobStore) (*Manager, *memory.Store, *v1alpha1.DNSZone) {
	t.Helper()
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	obj, err := st.Apply(t.Context(), config("127.0.0.1"), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = cache.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-cache.Initialized():
	case <-time.After(5 * time.Second):
		t.Fatal("cache readiness")
	}
	m, err := New(Options{Cache: cache, Blobs: blobs, ClusterID: "cluster-a", Discoverer: d, Now: c.now, PollInterval: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return m, st, obj.(*v1alpha1.DNSZone)
}
func run(t *testing.T, m *Manager) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("manager shutdown did not join workers")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
func await(t *testing.T, m *Manager, dz *v1alpha1.DNSZone, check func(Status) bool) Status {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if st, ok := m.StatusFor(dz); ok && check(st) {
			return st
		}
		time.Sleep(time.Millisecond)
	}
	st, _ := m.StatusFor(dz)
	t.Fatalf("manager state %+v", st)
	return Status{}
}
func db(t *testing.T, path string) *localdb.SnapshotStore {
	t.Helper()
	s, err := localdb.OpenSnapshotStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestManagerRefreshRetryExpiryAndOfflineRestart(t *testing.T) {
	c := clock()
	d := &fakeDiscovery{clock: c}
	d.serial.Store(4294967295)
	path := filepath.Join(t.TempDir(), "local.db")
	disk := db(t, path)
	m, _, dz := setupManager(t, c, d, disk)
	stop := run(t, m)
	initial := await(t, m, dz, func(s Status) bool { return s.Available })
	old := m.Snapshot().Lookup(zone.StubIdentity(dz))
	if initial.Serial != 4294967295 || !initial.ExpiresAt.Equal(c.now().Add(20*time.Second)) {
		t.Fatalf("initial %+v", initial)
	}
	// Confirmation at the same serial legitimately renews the TTL/expire window.
	c.advance(10 * time.Second)
	m.Wake()
	renewed := await(t, m, dz, func(s Status) bool { return s.ConfirmedAt.After(initial.ConfirmedAt) })
	if renewed.Serial != initial.Serial || old.NextExpiry() != initial.ExpiresAt {
		t.Fatal("renewal mutated captured route")
	}
	d.fail.Store(true)
	c.advance(10 * time.Second)
	m.Wake()
	failed := await(t, m, dz, func(s Status) bool { return s.Error != "" })
	if !failed.Available || failed.ExpiresAt != renewed.ExpiresAt || failed.NextRefresh != c.now().Add(3*time.Second) {
		t.Fatalf("failed refresh %+v", failed)
	}
	stop()
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	restartedDisk := db(t, path)
	m2, _, dz2 := setupManager(t, c, d, restartedDisk)
	run(t, m2)
	restored := await(t, m2, dz2, func(s Status) bool { return s.Available && s.Error != "" })
	if restored.ConfirmedAt != renewed.ConfirmedAt || restored.ExpiresAt != renewed.ExpiresAt {
		t.Fatal("restart renewed absolute validity")
	}
	c.advance(10 * time.Second)
	m2.Wake()
	if status, ok := m2.StatusFor(dz2); !ok || status.Available {
		t.Fatalf("expiry between ticks: %+v", status)
	}
	if m2.Snapshot().Lookup(zone.StubIdentity(dz2)).RoutingAvailableAt(c.now()) {
		t.Fatal("expired route served")
	}
	// Wait for the offline retry to finish at the new clock before changing
	// the fake primary, so an in-flight failure cannot reschedule recovery.
	await(t, m2, dz2, func(s Status) bool { return !s.Available && s.Error != "" && s.NextRefresh.After(c.now()) })
	d.fail.Store(false)
	d.serial.Store(0)
	c.advance(3 * time.Second)
	m2.Wake()
	await(t, m2, dz2, func(s Status) bool { return s.Available && s.Serial == 0 && s.Error == "" })
}

type failingBlobs struct{ localdb.BlobStore }

func (b failingBlobs) PutBlob(context.Context, string, []byte) error { return errors.New("disk full") }
func TestManagerPersistenceFailureNeverPublishes(t *testing.T) {
	c := clock()
	m, _, dz := setupManager(t, c, &fakeDiscovery{clock: c}, failingBlobs{db(t, filepath.Join(t.TempDir(), "local.db"))})
	run(t, m)
	s := await(t, m, dz, func(s Status) bool { return s.Error != "" })
	if s.Available || m.Snapshot().Lookup(zone.StubIdentity(dz)) != nil {
		t.Fatal("published undurable discovery")
	}
}

func TestManagerRestoreRejectsWrongIdentityClusterCorruptionAndExpiry(t *testing.T) {
	c := clock()
	disk := db(t, filepath.Join(t.TempDir(), "local.db"))
	d := &fakeDiscovery{clock: c}
	m, _, dz := setupManager(t, c, d, disk)
	data := delegation(c.now())
	if err := m.persist(t.Context(), zone.StubIdentity(dz), data); err != nil {
		t.Fatal(err)
	}
	blob, err := disk.GetBlob(t.Context(), "stub/"+zone.StubIdentity(dz))
	if err != nil {
		t.Fatal(err)
	}
	var original envelope
	if err := json.Unmarshal(blob.Data, &original); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*envelope)
	}{
		{"cluster", func(e *envelope) { e.ClusterID = "cluster-b"; e.Checksum = e.digest() }},
		{"identity", func(e *envelope) { e.Identity = "different"; e.Checksum = e.digest() }},
		{"checksum", func(e *envelope) { e.Checksum = "bad" }},
		{"future", func(e *envelope) { e.ConfirmedAt = c.now().Add(time.Second); e.Checksum = e.digest() }},
		{"expiry", func(e *envelope) { e.ExpiresAt = e.ConfirmedAt.Add(21 * time.Second); e.Checksum = e.digest() }},
		{"out-of-zone", func(e *envelope) { e.Records = []string{"other. 20 IN A 127.0.0.1"}; e.Checksum = e.digest() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := original
			test.change(&candidate)
			b, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := disk.PutBlob(t.Context(), "stub/"+zone.StubIdentity(dz), b); err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.restore(t.Context(), &entry{id: zone.StubIdentity(dz), dz: dz}); err == nil {
				t.Fatal("invalid disk state restored")
			}
		})
	}
}

func TestManagerDeleteSupersedesDiscoveryAndJoinsCancellation(t *testing.T) {
	c := clock()
	started := make(chan struct{})
	canceled := make(chan struct{})
	d := discoverFunc(func(ctx context.Context, _ *v1alpha1.DNSZone) (*zone.StubData, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return delegation(c.now()), nil
	})
	m, st, dz := setupManager(t, c, d, db(t, filepath.Join(t.TempDir(), "local.db")))
	stop := run(t, m)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("discovery did not start")
	}
	if err := st.Delete(t.Context(), store.Key{Kind: "DNSZone", Name: dz.Name}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("removed discovery not canceled")
	}
	stop()
	if m.Snapshot().Lookup(zone.StubIdentity(dz)) != nil || len(m.Statuses()) != 0 {
		t.Fatal("deleted zone revived")
	}
}

func TestManagerBoundsConcurrentDiscoveryAndShutdown(t *testing.T) {
	c := clock()
	var active, peak atomic.Int32
	started := make(chan struct{}, 16)
	d := discoverFunc(func(ctx context.Context, _ *v1alpha1.DNSZone) (*zone.StubData, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	m, st, _ := setupManager(t, c, d, db(t, filepath.Join(t.TempDir(), "local.db")))
	m.opts.MaxConcurrent = 2
	for i := range 7 {
		dz := config("127.0.0.1")
		dz.Name = fmt.Sprintf("example-%d", i)
		dz.Spec.ZoneName = fmt.Sprintf("zone-%d.example.", i)
		if _, err := st.Apply(t.Context(), dz, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	stop := run(t, m)
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bounded worker not started")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if peak.Load() != 2 {
		t.Fatalf("discovery concurrency=%d", peak.Load())
	}
	stop()
	if active.Load() != 0 {
		t.Fatal("shutdown left discovery workers running")
	}
}
