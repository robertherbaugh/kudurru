package secondary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

type managerClock struct{ ns atomic.Int64 }

func (c *managerClock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *managerClock) advance(d time.Duration) { c.ns.Add(int64(d)) }
func newManagerClock() *managerClock {
	c := new(managerClock)
	c.ns.Store(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

type managerReceiver struct {
	mu                  sync.Mutex
	serial              uint32
	err                 error
	records             []dns.RR
	soaCalls, transfers int
	soaHook             func(context.Context, string) error
}

func (r *managerReceiver) SOA(ctx context.Context, _ *v1alpha1.DNSZone, primary string) (*dns.SOA, error) {
	r.mu.Lock()
	r.soaCalls++
	hook, err, serial := r.soaHook, r.err, r.serial
	r.mu.Unlock()
	if hook != nil {
		if e := hook(ctx, primary); e != nil {
			return nil, e
		}
	}
	if err != nil {
		return nil, err
	}
	return managerRecords(serial)[0].(*dns.SOA), nil
}
func (r *managerReceiver) AXFR(context.Context, *v1alpha1.DNSZone, string) ([]dns.RR, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transfers++
	if r.err != nil {
		return nil, r.err
	}
	if r.records != nil {
		return r.records, nil
	}
	return managerRecords(r.serial), nil
}
func (r *managerReceiver) set(serial uint32, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serial, r.err = serial, err
}
func (r *managerReceiver) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.transfers }
func managerRecords(serial uint32) []dns.RR {
	lines := []string{fmt.Sprintf("example. 600 IN SOA ns.example. hostmaster.example. %d 10 3 30 120", serial), "example. 600 IN NS ns.example.", "ns.example. 777 IN A 192.0.2.53", "www.example. 987 IN A 192.0.2.80", "raw.example. 432 IN TYPE65280 \\# 3 010203"}
	var out []dns.RR
	for _, line := range lines {
		rr, err := dns.NewRR(line)
		if err != nil {
			panic(err)
		}
		out = append(out, rr)
	}
	return out
}
func managerZone() *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "stable-zone"}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypeSecondary, ZoneName: "example.", Secondary: &v1alpha1.SecondarySpec{Primaries: []string{"primary.example:5300"}}}}
}
func managerDB(t *testing.T) *localdb.SnapshotStore {
	t.Helper()
	db, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func managerFor(t *testing.T, clock *managerClock, receiver Receiver, blobs localdb.BlobStore) (*Manager, *memory.Store, *v1alpha1.DNSZone) {
	t.Helper()
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	obj, err := st.Apply(t.Context(), managerZone(), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	dz := obj.(*v1alpha1.DNSZone)
	cache, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Cache: cache, Blobs: blobs, ClusterID: "cluster-a", Receiver: receiver, Now: clock.now, PollInterval: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	return m, st, dz
}
func runManager(t *testing.T, m *Manager) func() {
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
func awaitManager(t *testing.T, m *Manager, dz *v1alpha1.DNSZone, check func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := m.StatusFor(dz); ok && check(st) {
			return st
		}
		time.Sleep(time.Millisecond)
	}
	st, _ := m.StatusFor(dz)
	t.Fatalf("secondary did not reach expected state: %+v", st)
	return Status{}
}

func TestManagerRefreshRetryExpiryAndRecovery(t *testing.T) {
	clock := newManagerClock()
	receiver := &managerReceiver{serial: 0}
	m, _, dz := managerFor(t, clock, receiver, managerDB(t))
	runManager(t, m)
	initial := awaitManager(t, m, dz, func(st Status) bool { return st.Available })
	if initial.Serial != 0 || initial.RecordCount != 4 || !initial.ExpiresAt.Equal(clock.now().Add(30*time.Second)) {
		t.Fatalf("initial %+v", initial)
	}
	old := m.Snapshot().Lookup(zone.SecondaryIdentity(dz))
	clock.advance(10 * time.Second)
	m.Wake()
	renewed := awaitManager(t, m, dz, func(st Status) bool { return st.ConfirmedAt.After(initial.ConfirmedAt) })
	if receiver.count() != 1 {
		t.Fatal("same serial must renew through SOA without AXFR")
	}
	if !old.NextExpiry().Equal(initial.ExpiresAt) {
		t.Fatal("refresh mutated an earlier query snapshot")
	}
	receiver.set(0, errors.New("primary offline"))
	clock.advance(10 * time.Second)
	m.Wake()
	failed := awaitManager(t, m, dz, func(st Status) bool { return st.Error != "" })
	if !failed.Available || !failed.ExpiresAt.Equal(renewed.ExpiresAt) || !failed.NextRefresh.Equal(clock.now().Add(3*time.Second)) {
		t.Fatalf("retry changed last-good state: %+v", failed)
	}
	clock.advance(20 * time.Second)
	st, _ := m.StatusFor(dz)
	if st.Available || m.Snapshot().Lookup(zone.SecondaryIdentity(dz)).AvailableAt(clock.now()) {
		t.Fatal("data served at absolute expiry")
	}
	receiver.set(1, nil)
	m.Wake()
	recovered := awaitManager(t, m, dz, func(st Status) bool { return st.Available && st.Serial == 1 && st.Error == "" })
	if recovered.RecordCount != 4 || receiver.count() != 2 {
		t.Fatalf("recovery %+v, transfers %d", recovered, receiver.count())
	}
}

func TestManagerRestartNeverRenewsExpiry(t *testing.T) {
	clock := newManagerClock()
	receiver := &managerReceiver{serial: 42}
	db := managerDB(t)
	m, _, dz := managerFor(t, clock, receiver, db)
	stop := runManager(t, m)
	initial := awaitManager(t, m, dz, func(st Status) bool { return st.Available })
	stop()
	receiver.set(42, errors.New("offline"))
	clock.advance(20 * time.Second)
	second, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	stop2 := runManager(t, second)
	restored := awaitManager(t, second, dz, func(st Status) bool { return st.Error != "" })
	if !restored.Available || restored.Serial != 42 || !restored.ConfirmedAt.Equal(initial.ConfirmedAt) || !restored.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("restart renewed or lost last-good data: %+v", restored)
	}
	stop2()
	clock.advance(10 * time.Second)
	third, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	runManager(t, third)
	expired := awaitManager(t, third, dz, func(st Status) bool { return !st.ConfirmedAt.IsZero() })
	if expired.Available || !expired.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("expired restart %+v", expired)
	}
}

func TestManagerSerialArithmeticAndInvalidData(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		old, offered, transferred uint32
		bad                       bool
	}{
		{"wrap", ^uint32(0) - 1, 1, 1, false}, {"rollback", 100, 99, 99, true}, {"ambiguous", 0, 1 << 31, 1 << 31, true},
		{"poll then rollback", 100, 101, 99, true}, {"poll then advance", 100, 101, 102, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newManagerClock()
			recv := &managerReceiver{serial: tc.old}
			m, _, dz := managerFor(t, clock, recv, managerDB(t))
			e := &entry{dz: dz, id: zone.SecondaryIdentity(dz), credential: "unsigned"}
			r := m.refresh(t.Context(), e)
			if r.err != nil {
				t.Fatal(r.err)
			}
			e.z, e.data = r.z, r.data
			recv.serial, recv.records = tc.offered, managerRecords(tc.transferred)
			r = m.refresh(t.Context(), e)
			if (r.err != nil) != tc.bad {
				t.Fatalf("refresh error %v, want error %v", r.err, tc.bad)
			}
			if !tc.bad && r.z.Serial() != tc.transferred {
				t.Fatalf("serial %d", r.z.Serial())
			}
		})
	}
	t.Run("structural and signed", func(t *testing.T) {
		clock := newManagerClock()
		recv := &managerReceiver{serial: 1}
		m, _, dz := managerFor(t, clock, recv, managerDB(t))
		e := &entry{dz: dz, id: zone.SecondaryIdentity(dz), credential: "unsigned"}
		for _, line := range []string{"outside.test. 60 IN A 192.0.2.1", "www.example. 60 IN CNAME target.example.", "example. 60 IN RRSIG SOA 13 1 60 20270101000000 20260101000000 1 example. AQID"} {
			rr, err := dns.NewRR(line)
			if err != nil {
				t.Fatal(err)
			}
			recv.records = append(managerRecords(1), rr)
			if r := m.refresh(t.Context(), e); r.err == nil || r.z != nil {
				t.Fatalf("invalid data accepted: %s", line)
			}
		}
	})
}

func TestManagerPersistedSnapshotValidation(t *testing.T) {
	clock := newManagerClock()
	db := managerDB(t)
	m, _, dz := managerFor(t, clock, &managerReceiver{serial: 7}, db)
	e := &entry{dz: dz, id: zone.SecondaryIdentity(dz), credential: "unsigned"}
	r := m.refresh(t.Context(), e)
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*envelope)
		checksum bool
	}{
		{"checksum", func(d *envelope) { d.Records[1] = "example. 5 IN NS forged.example." }, false},
		{"cluster", func(d *envelope) { d.ClusterID = "other" }, true}, {"schema", func(d *envelope) { d.Version++ }, true},
		{"config", func(d *envelope) { d.Identity = "old-config" }, true}, {"key rotation", func(d *envelope) { d.Credential = "old-key" }, true},
		{"future confirmation", func(d *envelope) { d.ConfirmedAt = clock.now().Add(time.Hour) }, true},
		{"changed expiry", func(d *envelope) { d.ExpiresAt = d.ExpiresAt.Add(time.Hour) }, true},
		{"bad RR", func(d *envelope) { d.Records[0] = "invalid RR" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := *r.data
			data.Records = append([]string(nil), data.Records...)
			data.Checksum = data.digest()
			tc.mutate(&data)
			if tc.checksum {
				data.Checksum = data.digest()
			}
			wire, _ := json.Marshal(&data)
			if err := db.PutBlob(t.Context(), "secondary/"+e.id, wire); err != nil {
				t.Fatal(err)
			}
			if _, z, err := m.restore(t.Context(), e); err == nil || z != nil {
				t.Fatal("invalid persisted data accepted")
			}
		})
	}
	if err := m.persist(t.Context(), e.id, r.data); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	restored, z, err := m.restore(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	if z.AvailableAt(clock.now()) || !restored.ExpiresAt.Equal(r.data.ExpiresAt) {
		t.Fatal("restoring expired data extended its lifetime")
	}
}

type failingManagerBlobs struct {
	localdb.BlobStore
	fail atomic.Bool
}

func (b *failingManagerBlobs) PutBlob(ctx context.Context, name string, data []byte) error {
	if b.fail.Load() {
		return errors.New("disk full")
	}
	return b.BlobStore.PutBlob(ctx, name, data)
}
func TestManagerPersistenceFailureRetainsPublishedCopy(t *testing.T) {
	clock := newManagerClock()
	recv := &managerReceiver{serial: 7}
	blobs := &failingManagerBlobs{BlobStore: managerDB(t)}
	m, _, dz := managerFor(t, clock, recv, blobs)
	runManager(t, m)
	initial := awaitManager(t, m, dz, func(st Status) bool { return st.Available })
	blobs.fail.Store(true)
	recv.set(8, nil)
	clock.advance(10 * time.Second)
	m.Wake()
	failed := awaitManager(t, m, dz, func(st Status) bool { return strings.Contains(st.Error, "disk full") })
	if failed.Serial != 7 || !failed.ExpiresAt.Equal(initial.ExpiresAt) || m.Snapshot().Lookup(zone.SecondaryIdentity(dz)).Serial() != 7 {
		t.Fatalf("undurable data was published: %+v", failed)
	}
}

func TestManagerDeletionCancelsTransferAndDiscardsLateResult(t *testing.T) {
	clock := newManagerClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	canceled := make(chan struct{})
	recv := &managerReceiver{serial: 9, soaHook: func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil
	}}
	db := managerDB(t)
	m, st, dz := managerFor(t, clock, recv, db)
	runManager(t, m)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no transfer")
	}
	if err := st.Delete(t.Context(), store.Key{Kind: "DNSZone", Name: dz.Name}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("deleted transfer not canceled")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := m.StatusFor(dz); !ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if z := m.Snapshot().Lookup(zone.SecondaryIdentity(dz)); z != nil {
		t.Fatal("deleted data published")
	}
	if _, err := db.GetBlob(t.Context(), "secondary/"+zone.SecondaryIdentity(dz)); !errors.Is(err, localdb.ErrNoBlob) {
		t.Fatalf("deleted data persisted: %v", err)
	}
}

func TestManagerReplicasTransferIndependently(t *testing.T) {
	clock := newManagerClock()
	a, b := &managerReceiver{serial: 10}, &managerReceiver{serial: 11}
	first, _, za := managerFor(t, clock, a, managerDB(t))
	second, _, zb := managerFor(t, clock, b, managerDB(t))
	runManager(t, first)
	runManager(t, second)
	sa := awaitManager(t, first, za, func(st Status) bool { return st.Available })
	sb := awaitManager(t, second, zb, func(st Status) bool { return st.Available })
	if sa.Serial != 10 || sb.Serial != 11 || a.count() != 1 || b.count() != 1 {
		t.Fatalf("replica copies %+v %+v", sa, sb)
	}
	changed := za.DeepCopy()
	changed.Spec.Secondary.Primaries = []string{"replacement.example"}
	if _, ok := first.StatusFor(changed); ok {
		t.Fatal("changed config exposed old runtime state")
	}
	changed = za.DeepCopy()
	changed.UID = "recreated"
	if _, ok := first.StatusFor(changed); ok {
		t.Fatal("recreated object exposed old runtime state")
	}
}

func TestManagerBoundsConcurrentRefreshAndTriesEveryZone(t *testing.T) {
	clock := newManagerClock()
	release := make(chan struct{})
	var active, maximum atomic.Int32
	var seen sync.Map
	recv := &managerReceiver{soaHook: func(ctx context.Context, primary string) error {
		seen.Store(primary, true)
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return errors.New("offline")
		}
	}}
	m, st, _ := managerFor(t, clock, recv, managerDB(t))
	m.opts.MaxConcurrent = 2
	for i := 1; i < 6; i++ {
		dz := managerZone()
		dz.Name = fmt.Sprintf("zone-%d", i)
		dz.UID = ""
		dz.Spec.Secondary.Primaries = []string{fmt.Sprintf("primary-%d", i)}
		if _, err := st.Apply(t.Context(), dz, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	runManager(t, m)
	deadline := time.Now().Add(5 * time.Second)
	for active.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 2 {
		t.Fatalf("active transfers %d", active.Load())
	}
	close(release)
	for time.Now().Before(deadline) {
		n := 0
		seen.Range(func(_, _ any) bool { n++; return true })
		if n == 6 {
			if maximum.Load() != 2 {
				t.Fatalf("concurrency %d", maximum.Load())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("a zone starved behind failed primaries")
}

func TestManagerTriesNextPrimary(t *testing.T) {
	clock := newManagerClock()
	recv := &managerReceiver{serial: 7, soaHook: func(_ context.Context, p string) error {
		if p == "offline" {
			return errors.New("unreachable")
		}
		return nil
	}}
	m, _, dz := managerFor(t, clock, recv, managerDB(t))
	dz.Spec.Secondary.Primaries = []string{"offline", "healthy"}
	e := &entry{dz: dz, id: zone.SecondaryIdentity(dz), credential: "unsigned"}
	r := m.refresh(t.Context(), e)
	if r.err != nil || r.primary != "healthy" || r.z.Serial() != 7 {
		t.Fatalf("failover %+v", r)
	}
}

func TestSecondaryStatusRespectsConfigurationGate(t *testing.T) {
	now := newManagerClock().now()
	st := Status{ConfirmedAt: now, ExpiresAt: now.Add(time.Hour)}
	for _, tc := range []struct {
		name      string
		snapshot  *zonecache.Snapshot
		available bool
	}{
		{name: "unsynced", snapshot: &zonecache.Snapshot{State: zonecache.Unsynced}},
		{name: "stale configuration", snapshot: &zonecache.Snapshot{State: zonecache.FromSnapshot, Stale: true}},
		{name: "fresh persisted configuration", snapshot: &zonecache.Snapshot{State: zonecache.FromSnapshot}, available: true},
		{name: "live", snapshot: &zonecache.Snapshot{State: zonecache.Live}, available: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusAt(st, tc.snapshot, now)
			if got.Available != tc.available || (got.ConfigurationError == "") != tc.available {
				t.Fatalf("status %+v", got)
			}
		})
	}
}

func TestManagerCopySurvivesDatabaseReopen(t *testing.T) {
	clock := newManagerClock()
	path := filepath.Join(t.TempDir(), "local.db")
	db, err := localdb.OpenSnapshotStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	m, _, dz := managerFor(t, clock, &managerReceiver{serial: 19}, db)
	e := &entry{dz: dz, id: zone.SecondaryIdentity(dz), credential: "unsigned"}
	r := m.refresh(t.Context(), e)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if err := m.persist(t.Context(), e.id, r.data); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := localdb.OpenSnapshotStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	m.opts.Blobs = reopened
	clock.advance(20 * time.Second)
	data, z, err := m.restore(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	if z.Serial() != 19 || !z.AvailableAt(clock.now()) || !data.ExpiresAt.Equal(r.data.ExpiresAt) {
		t.Fatalf("reopened data %+v", data)
	}
	records, err := data.records()
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range managerRecords(19) {
		if records[i].String() != want.String() {
			t.Fatalf("record changed across restart: %s != %s", records[i], want)
		}
	}
}

func TestManagerInvalidQueryPolicyFailsBeforeNetwork(t *testing.T) {
	clock := newManagerClock()
	recv := &managerReceiver{serial: 7}
	m, _, dz := managerFor(t, clock, recv, managerDB(t))
	dz.Spec.Policy.QueryAccess = v1alpha1.AccessUseNetworkACL
	dz.Spec.Policy.QueryACL = []string{"invalid"}
	r := m.refresh(t.Context(), &entry{dz: dz, id: zone.SecondaryIdentity(dz)})
	if r.err == nil || !strings.Contains(r.err.Error(), "invalid secondary configuration") || recv.soaCalls != 0 {
		t.Fatalf("invalid configuration attempted network: %+v", r)
	}
}
