package rpz

import (
	"context"
	"errors"
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
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type runtimeClock struct{ ns atomic.Int64 }

func (c *runtimeClock) now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *runtimeClock) add(d time.Duration) { c.ns.Add(int64(d)) }

type testReceiver struct {
	mu              sync.Mutex
	serial          uint32
	err             error
	soa, axfr, ixfr int
	axfrHook        func(context.Context, uint32) error
}

func (r *testReceiver) SOA(context.Context, *v1alpha1.DNSZone, string) (*dns.SOA, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.soa++
	if r.err != nil {
		return nil, r.err
	}
	return runtimeRecords(r.serial)[0].(*dns.SOA), nil
}
func (r *testReceiver) AXFR(ctx context.Context, _ *v1alpha1.DNSZone, _ string) ([]dns.RR, error) {
	r.mu.Lock()
	r.axfr++
	serial, err, hook := r.serial, r.err, r.axfrHook
	r.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, serial); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	return runtimeRecords(serial), nil
}
func (r *testReceiver) IXFR(_ context.Context, _ *v1alpha1.DNSZone, _ string, base []dns.RR) ([]dns.RR, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ixfr++
	if r.err != nil {
		return nil, r.err
	}
	if len(base) == 0 {
		return nil, errors.New("empty incremental baseline")
	}
	return runtimeRecords(r.serial), nil
}
func (r *testReceiver) set(serial uint32, err error) {
	r.mu.Lock()
	r.serial, r.err = serial, err
	r.mu.Unlock()
}
func (r *testReceiver) counts() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.soa, r.axfr, r.ixfr
}

func runtimeRecords(serial uint32) []dns.RR {
	target := "."
	if serial%2 == 0 {
		target = "*."
	}
	return []dns.RR{
		&dns.SOA{Hdr: dns.RR_Header{Name: "policy.test.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 600}, Ns: "ns.policy.test.", Mbox: "hostmaster.policy.test.", Serial: serial, Refresh: 10, Retry: 3, Expire: 30, Minttl: 5},
		&dns.NS{Hdr: dns.RR_Header{Name: "policy.test.", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 600}, Ns: "ns.policy.test."},
		&dns.CNAME{Hdr: dns.RR_Header{Name: "bad.test.policy.test.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 123}, Target: target},
	}
}

func runtimeList() *v1alpha1.BlockList {
	return &v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ads", UID: "list-a"}, Spec: v1alpha1.BlockListSpec{Mode: "block", Sources: []v1alpha1.ListSource{{Format: "rpz", RPZ: &v1alpha1.RPZSource{ZoneName: "policy.test.", Primaries: []string{"127.0.0.1:5300"}}}}}}
}

func runtimeDB(t *testing.T) *localdb.SnapshotStore {
	t.Helper()
	db, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type runtimeFixture struct {
	m     *Manager
	cache *zonecache.Cache
	store *memory.Store
	list  *v1alpha1.BlockList
	clock *runtimeClock
}

func newRuntime(t *testing.T, rx Receiver, blobs BlobStore, list *v1alpha1.BlockList) *runtimeFixture {
	t.Helper()
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	obj, err := st.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	clock := &runtimeClock{}
	clock.ns.Store(testNow.UnixNano())
	m, err := New(Options{Cache: cache, Blobs: blobs, ClusterID: "cluster-a", Receiver: rx, Now: clock.now, PollInterval: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
		t.Fatal("cache did not initialize")
	}
	eventually(t, func() bool { return cache.Snapshot().Authoritative() })
	return &runtimeFixture{m, cache, st, obj.(*v1alpha1.BlockList), clock}
}

func startRuntime(t *testing.T, m *Manager) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := m.Run(ctx); err != nil {
			t.Error(err)
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("manager did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func awaitStatus(t *testing.T, fx *runtimeFixture, check func(Status) bool) Status {
	t.Helper()
	var status Status
	eventually(t, func() bool { status, _ = fx.m.StatusFor(fx.list, 0); return check(status) })
	return status
}

func TestRuntimeRefreshIncrementalExpiryAndRestart(t *testing.T) {
	rx := &testReceiver{serial: 4294967295}
	fx := newRuntime(t, rx, runtimeDB(t), runtimeList())
	stop := startRuntime(t, fx.m)
	first := awaitStatus(t, fx, func(s Status) bool { return s.Available })
	old := fx.m.Snapshot()
	fx.clock.add(10 * time.Second)
	fx.m.Wake()
	renewed := awaitStatus(t, fx, func(s Status) bool { return s.ConfirmedAt.After(first.ConfirmedAt) })
	_, axfr, ixfr := rx.counts()
	if axfr != 1 || ixfr != 0 {
		t.Fatalf("same serial transferred again: AXFR=%d IXFR=%d", axfr, ixfr)
	}
	if old.Lookup(SourceIdentity(fx.list, 0)).AvailableAt(first.ExpiresAt) {
		t.Fatal("renewal mutated old query snapshot")
	}
	rx.set(0, nil)
	fx.clock.add(10 * time.Second)
	fx.m.Wake()
	updated := awaitStatus(t, fx, func(s Status) bool { return s.Serial == 0 && s.ConfirmedAt.After(renewed.ConfirmedAt) })
	_, axfr, ixfr = rx.counts()
	if axfr != 1 || ixfr != 1 {
		t.Fatalf("serial wrap did not use IXFR: AXFR=%d IXFR=%d", axfr, ixfr)
	}
	ctx := Context{Name: "bad.test.", Type: dns.TypeA}
	if d := fx.m.Snapshot().Select([]*v1alpha1.BlockList{fx.list}, true).Evaluate(ctx, fx.clock.now()); d.Action != ActionNODATA {
		t.Fatal("feed action did not update")
	}
	rx.set(0, errors.New("offline"))
	fx.clock.add(10 * time.Second)
	fx.m.Wake()
	failed := awaitStatus(t, fx, func(s Status) bool { return s.Error != "" })
	if !failed.Available || failed.ExpiresAt != updated.ExpiresAt {
		t.Fatal("failure changed last-good expiry")
	}
	stop()
	// Restart at exact expiry. Loading old bytes must not renew or revive them.
	fx.clock.add(20 * time.Second)
	restarted, err := New(fx.m.opts)
	if err != nil {
		t.Fatal(err)
	}
	fx.m = restarted
	startRuntime(t, restarted)
	st := awaitStatus(t, fx, func(s Status) bool { return s.Error != "" && !s.ConfirmedAt.IsZero() })
	if st.Available || st.ExpiresAt != updated.ExpiresAt {
		t.Fatal("restart renewed expired durable data")
	}
	if d := restarted.Snapshot().Select([]*v1alpha1.BlockList{fx.list}, true).Evaluate(ctx, fx.clock.now()); !d.Unavailable {
		t.Fatal("expired selected feed failed open")
	}
	rx.set(0, nil)
	fx.clock.add(3 * time.Second)
	restarted.Wake()
	awaitStatus(t, fx, func(s Status) bool { return s.Available && s.Error == "" })
	_, axfr, ixfr = rx.counts()
	if axfr != 2 || ixfr != 1 {
		t.Fatalf("expired serial baseline was reused: AXFR=%d IXFR=%d", axfr, ixfr)
	}
	if d := restarted.Snapshot().Select([]*v1alpha1.BlockList{fx.list}, false).Evaluate(ctx, fx.clock.now()); !d.Unavailable {
		t.Fatal("stale configuration bypassed feed gate")
	}
	if d := restarted.Snapshot().Select(nil, false).Evaluate(ctx, fx.clock.now()); d.Unavailable {
		t.Fatal("unrelated policy failed closed")
	}
}

type failingBlobs struct {
	BlobStore
	fail atomic.Bool
}

func (b *failingBlobs) PutBlob(ctx context.Context, name string, data []byte) error {
	if b.fail.Load() {
		return errors.New("disk failure")
	}
	return b.BlobStore.PutBlob(ctx, name, data)
}

func TestRuntimeDurabilityAndIdentityIsolation(t *testing.T) {
	blobs := &failingBlobs{BlobStore: runtimeDB(t)}
	rx := &testReceiver{serial: 1}
	fx := newRuntime(t, rx, blobs, runtimeList())
	startRuntime(t, fx.m)
	first := awaitStatus(t, fx, func(s Status) bool { return s.Available })
	blobs.fail.Store(true)
	rx.set(2, nil)
	fx.clock.add(10 * time.Second)
	fx.m.Wake()
	failed := awaitStatus(t, fx, func(s Status) bool { return s.Error != "" })
	if failed.Serial != 1 || !failed.Available || failed.ExpiresAt != first.ExpiresAt {
		t.Fatal("non-durable candidate was published")
	}
	newConfig := fx.list.DeepCopy()
	newConfig.Spec.Sources[0].RPZ.Primaries = []string{"127.0.0.2:5300"}
	if SourceIdentity(newConfig, 0) == SourceIdentity(fx.list, 0) {
		t.Fatal("changed source has same identity")
	}
	if d := fx.m.Snapshot().Select([]*v1alpha1.BlockList{newConfig}, true).Evaluate(Context{Name: "bad.test."}, fx.clock.now()); !d.Unavailable {
		t.Fatal("changed source matched old transfer")
	}
	clone := fx.list.DeepCopy()
	clone.Status.Unsupported = 100
	if SourceIdentity(clone, 0) != SourceIdentity(fx.list, 0) {
		t.Fatal("shared status affected source identity")
	}
	// Clock rollback must not assert availability of future confirmation data.
	fx.clock.add(-20 * time.Second)
	if fx.m.Snapshot().Lookup(SourceIdentity(fx.list, 0)).AvailableAt(fx.clock.now()) {
		t.Fatal("clock rollback served future data")
	}
}

func TestRuntimeABACancelsOldWorker(t *testing.T) {
	rx := &testReceiver{serial: 1}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	rx.axfrHook = func(context.Context, uint32) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	}
	fx := newRuntime(t, rx, runtimeDB(t), runtimeList())
	fx.m.opts.MaxConcurrent = 2
	startRuntime(t, fx.m)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer did not start")
	}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	change := fx.list.DeepCopy()
	change.Spec.Sources[0].RPZ.Primaries = []string{"127.0.0.2:5300"}
	_, err := fx.store.Apply(t.Context(), change, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		return fx.cache.Snapshot().Objects.BlockLists()[0].Spec.Sources[0].RPZ.Primaries[0] == "127.0.0.2:5300"
	})
	rx.set(7, nil)
	obj, err := fx.store.Apply(t.Context(), fx.list, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	fx.list = obj.(*v1alpha1.BlockList)
	eventually(t, func() bool {
		return fx.cache.Snapshot().Objects.BlockLists()[0].Spec.Sources[0].RPZ.Primaries[0] == "127.0.0.1:5300"
	})
	awaitStatus(t, fx, func(s Status) bool { return s.Available && s.Serial == 7 })
	once.Do(func() { close(release) })
	eventually(t, func() bool { _, axfr, _ := rx.counts(); return axfr >= 3 })
	if s, _ := fx.m.StatusFor(fx.list, 0); s.Serial != 7 {
		t.Fatal("canceled A worker replaced new A data")
	}
}
