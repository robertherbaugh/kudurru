package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// The measurements recorded in dev-docs/spikes/store-kube.md. They create
// thousands of objects and run the production election timings, so they
// only run with KUDURRU_MEASURE=1:
//
//	KUDURRU_MEASURE=1 go test -run 'TestMeasure' -v -count=1 ./internal/store/kube/

const measureObjects = 1000

func requireMeasure(t *testing.T) {
	t.Helper()
	requireEnv(t)
	if os.Getenv("KUDURRU_MEASURE") == "" {
		t.Skip("set KUDURRU_MEASURE=1 to run the spike measurements")
	}
}

// heapInUse returns the live heap in bytes after forcing collection.
func heapInUse() int64 {
	var ms runtime.MemStats
	for range 3 {
		runtime.GC()
	}
	debug.FreeOSMemory()
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc) //nolint:gosec // a test heap is far below 2^63 bytes
}

func durations(ds []time.Duration) string {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return fmt.Sprintf("min %s, median %s, max %s (n=%d)",
		s[0].Round(10*time.Microsecond), s[len(s)/2].Round(10*time.Microsecond), s[len(s)-1].Round(10*time.Microsecond), len(s))
}

func mib(b int64) string { return fmt.Sprintf("%.2f MiB", float64(b)/(1<<20)) }

// TestMeasureStore records List pagination latency, Apply throughput,
// informer-cache memory, Watch sync time and local.db snapshot cost for
// 1,000 DNSRecordSets.
func TestMeasureStore(t *testing.T) {
	requireMeasure(t)
	purge(t, shared.admin)
	ctx := context.Background()

	// Fixed cost of a driver with synced, empty informers.
	base := heapInUse()
	empty := newTestStore(t, Options{})
	if !empty.WaitForSync(ctx) {
		t.Fatal("empty store did not sync")
	}
	emptyCost := heapInUse() - base
	t.Logf("driver + 5 synced empty informers: heap +%s", mib(emptyCost))
	_ = empty.Close()

	writer := newTestStore(t, Options{})
	if _, err := writer.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	sample := rrset("measure", "host-0000", map[string]string{"team": "infra", "app": "host-0000"})
	body, _ := json.Marshal(sample)
	t.Logf("DNSRecordSet JSON size: %d bytes (2 endpoints, 3 targets, 2 labels)", len(body))

	// Apply throughput: 1,000 creates through SSA, 8 workers.
	start := time.Now()
	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, measureObjects)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= measureObjects {
					return
				}
				name := fmt.Sprintf("host-%04d", i)
				team := []string{"infra", "web", "iot", "media"}[i%4]
				if _, err := writer.Apply(ctx, rrset("measure", name, map[string]string{"team": team, "app": name}), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	applyTotal := time.Since(start)
	t.Logf("Apply (SSA create) x%d, 8 workers: %s total, %.0f objects/s", measureObjects, applyTotal.Round(time.Millisecond), float64(measureObjects)/applyTotal.Seconds())

	// Sequential updates (precheck GET + SSA PATCH per call).
	var upd []time.Duration
	for i := range 50 {
		o := rrset("measure", fmt.Sprintf("host-%04d", i), map[string]string{"team": "updated", "app": fmt.Sprintf("host-%04d", i)})
		t0 := time.Now()
		if _, err := writer.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		upd = append(upd, time.Since(t0))
	}
	t.Logf("Apply (SSA update, sequential): %s", durations(upd))

	// List with pagination.
	for _, limit := range []int{0, 100, 250, 500} {
		var runs []time.Duration
		for range 7 {
			t0 := time.Now()
			n, pages, cont := 0, 0, ""
			for {
				page, err := writer.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: "measure", Limit: limit, Continue: cont})
				if err != nil {
					t.Fatal(err)
				}
				n += len(page.Items)
				pages++
				if cont = page.Continue; cont == "" {
					break
				}
			}
			runs = append(runs, time.Since(t0))
			if n != measureObjects {
				t.Fatalf("listed %d objects, want %d", n, measureObjects)
			}
			_ = pages
		}
		t.Logf("List %d DNSRecordSets, limit %d (%d pages): %s", measureObjects, limit, pagesFor(limit), durations(runs))
	}
	var sel []time.Duration
	for range 7 {
		t0 := time.Now()
		page, err := writer.List(ctx, "DNSRecordSet", store.ListOptions{LabelSelector: map[string]string{"team": "web"}, Limit: 100})
		if err != nil || len(page.Items) == 0 {
			t.Fatalf("selector list: %d (%v)", len(page.Items), err)
		}
		sel = append(sel, time.Since(t0))
	}
	t.Logf("List first page, label selector team=web (250 matches), limit 100: %s", durations(sel))

	// Informer cache memory and Watch sync for 1,000 objects.
	before := heapInUse()
	reader := newTestStore(t, Options{})
	t0 := time.Now()
	if !reader.WaitForSync(ctx) {
		t.Fatal("reader did not sync")
	}
	syncTime := time.Since(t0)
	after := heapInUse()
	cost := after - before
	t.Logf("driver + informers with %d DNSRecordSets (managedFields stripped): heap +%s, %.0f bytes/object above the empty driver; sync %s",
		measureObjects, mib(cost), float64(cost-emptyCost)/measureObjects, syncTime.Round(time.Millisecond))

	t0 = time.Now()
	w, err := reader.Watch(ctx, "DNSRecordSet", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	added := 0
	for ev := range w.ResultChan() {
		if ev.Type == store.Bookmark {
			break
		}
		added++
	}
	t.Logf("Watch on a synced informer (incl. catch-up list): %d Added then Bookmark in %s", added, time.Since(t0).Round(time.Microsecond))
	w.Stop()

	// The same informer without the managedFields transform, for scale.
	before = heapInUse()
	raw, err := cache.New(shared.cfg, cache.Options{Scheme: testScheme()})
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(ctx)
	go func() { _ = raw.Start(rctx) }()
	if _, err := raw.GetInformer(ctx, &v1alpha1.DNSRecordSet{}); err != nil {
		t.Fatal(err)
	}
	raw.WaitForCacheSync(ctx)
	rawCost := heapInUse() - before
	t.Logf("one DNSRecordSet informer without stripping managedFields: heap +%s", mib(rawCost))
	rcancel()
	runtime.KeepAlive(raw)

	// local.db: list every kind (zonecache writes the set it holds; here
	// the paged apiserver list stands in for it), write, load.
	dbPath := filepath.Join(t.TempDir(), localdb.FileName)
	db, err := localdb.OpenSnapshotStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	listAll := func() (*localdb.Snapshot, error) {
		snap := &localdb.Snapshot{ResourceVersions: map[store.Kind]string{}}
		for _, k := range reader.kinds.kinds {
			cont := ""
			for {
				page, err := reader.List(ctx, k, store.ListOptions{Limit: snapshotPageSize, Continue: cont})
				if err != nil {
					return nil, err
				}
				snap.Objects = append(snap.Objects, page.Items...)
				if cont = page.Continue; cont == "" {
					break
				}
			}
		}
		return snap, nil
	}
	var mat, wr, ld []time.Duration
	for range 5 {
		t0 := time.Now()
		snap, err := listAll()
		if err != nil {
			t.Fatal(err)
		}
		mat = append(mat, time.Since(t0))
		t0 = time.Now()
		if err := db.Write(ctx, snap); err != nil {
			t.Fatal(err)
		}
		wr = append(wr, time.Since(t0))
		t0 = time.Now()
		got, err := db.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ld = append(ld, time.Since(t0))
		if len(got.Objects) != measureObjects+1 {
			t.Fatalf("loaded %d objects", len(got.Objects))
		}
	}
	sizes := map[string]int64{}
	for _, suffix := range []string{"", "-wal"} {
		if st, err := os.Stat(dbPath + suffix); err == nil {
			sizes[suffix] = st.Size()
		}
	}
	t.Logf("paged List of every kind (%d objects): %s", measureObjects+1, durations(mat))
	t.Logf("local.db Write: %s; Load: %s; main file %d KiB, WAL %d KiB (after 5 rewrites)", durations(wr), durations(ld), sizes[""]/1024, sizes["-wal"]/1024)

	var buf bytes.Buffer
	t0 = time.Now()
	if err := reader.Snapshot(ctx, &buf, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Logf("Snapshot (kind: List YAML, paged from the apiserver): %s, %d KiB", time.Since(t0).Round(time.Millisecond), buf.Len()/1024)
}

func pagesFor(limit int) int {
	if limit == 0 {
		return 1
	}
	return (measureObjects + limit - 1) / limit
}

// TestMeasureFirstRequests separates the one-off costs a fresh process and
// a fresh apiserver pay: discovery, the first SSA of a CRD kind.
func TestMeasureFirstRequests(t *testing.T) {
	requireMeasure(t)
	assets, _ := findEnvtestAssets()
	t0 := time.Now()
	env, cfg, _, err := startControlPlane(assets)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	t.Logf("envtest control plane start + CRD install: %s", time.Since(t0).Round(time.Millisecond))
	s := newStoreOn(t, cfg, Options{})
	ctx := context.Background()
	step := func(name string, f func() error) {
		t.Helper()
		t0 := time.Now()
		if err := f(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %s", name, time.Since(t0).Round(time.Millisecond))
	}
	step("first List (RESTMapper discovery + list)", func() error {
		_, err := s.List(ctx, "DNSZone", store.ListOptions{})
		return err
	})
	step("first Apply of DNSZone (fresh apiserver)", func() error {
		_, err := s.Apply(ctx, zone("a", "a."), store.ApplyOptions{Manager: store.ManagerAPI})
		return err
	})
	step("second Apply of DNSZone", func() error {
		_, err := s.Apply(ctx, zone("b", "b."), store.ApplyOptions{Manager: store.ManagerAPI})
		return err
	})
	step("first Apply of DNSRecordSet", func() error {
		_, err := s.Apply(ctx, rrset("apps", "x", nil), store.ApplyOptions{Manager: store.ManagerAPI})
		return err
	})
	step("WaitForSync (5 informers)", func() error {
		if !s.WaitForSync(ctx) {
			return fmt.Errorf("no sync")
		}
		return nil
	})
}

// TestMeasureElectorProductionTimings runs the D44 failover with the
// shipped 15 s / 10 s / 2 s timings.
func TestMeasureElectorProductionTimings(t *testing.T) {
	requireMeasure(t)
	lease := uniqueLease()
	var a arena
	ca := newCandidate(t, lease, "pod-a", DefaultLeaseDuration, DefaultRenewDeadline, DefaultRetryPeriod)
	cb := newCandidate(t, lease, "pod-b", DefaultLeaseDuration, DefaultRenewDeadline, DefaultRetryPeriod)
	t0 := time.Now()
	ca.run(t, &a, nil)
	waitLeader(t, 30*time.Second, ca)
	t.Logf("first acquisition: %s", time.Since(t0).Round(time.Millisecond))
	cb.run(t, &a, nil)
	time.Sleep(3 * DefaultRetryPeriod)
	ca.down.Store(true)
	cut := time.Now()
	eventually(t, 30*time.Second, "pod-a onStopped", func() bool { return ca.stopped.Load() == 1 })
	stopped := time.Since(cut)
	eventually(t, 40*time.Second, "pod-b onStarted", func() bool { return cb.started.Load() == 1 })
	took := time.Since(cut)
	t.Logf("production timings: partition → old leader onStopped %s (RenewDeadline 10s + ≤ RetryPeriod), → new leader onStarted %s (LeaseDuration 15s after last renew); old leader Run still running: %v; max concurrent terms %d",
		stopped.Round(time.Millisecond), took.Round(time.Millisecond), ca.running(), a.maxActive.Load())
	if !ca.running() || a.maxActive.Load() != 1 {
		t.Fatal("gate violated")
	}
	ca.down.Store(false)
	cb.cancel()
	t0 = time.Now()
	eventually(t, 20*time.Second, "pod-a to take the released lease", func() bool { return ca.started.Load() == 2 })
	t.Logf("graceful shutdown of the leader (lease released) → successor onStarted: %s", time.Since(t0).Round(time.Millisecond))
}
