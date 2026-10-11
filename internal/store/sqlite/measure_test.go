package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestTenThousandObjects seeds 10,000 record sets through Apply and
// measures the numbers the report quotes: Apply and Get latency, a full
// List, paginated listing, database size per object and open time. Timing
// bounds are generous and only enforced without the race detector.
func TestTenThousandObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 10,000 objects")
	}
	const n = 10_000
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	// Without the race detector the seed is sequential so the Apply
	// latencies are uncontended. Under it validation dominates and the
	// numbers mean nothing, so parallel writers keep the run short (they
	// only share the write lock).
	workers := 1
	if raceEnabled {
		workers = 4
	}
	lat := make([][]time.Duration, workers)
	var wg sync.WaitGroup
	seedErr := make(chan error, workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < n; i += workers {
				rs := recordSet(fmt.Sprintf("ns%02d", i%10), fmt.Sprintf("host-%05d", i), map[string]string{"team": fmt.Sprintf("t%d", i%4)})
				t0 := time.Now()
				if _, err := s.Apply(ctx, rs, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					seedErr <- err
					return
				}
				lat[w] = append(lat[w], time.Since(t0))
			}
		}(w)
	}
	wg.Wait()
	close(seedErr)
	if err := <-seedErr; err != nil {
		t.Fatal(err)
	}
	seed := time.Since(start)
	var applyLat []time.Duration
	for _, l := range lat {
		applyLat = append(applyLat, l...)
	}

	t0 := time.Now()
	all, err := s.List(ctx, "DNSRecordSet", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	listAll := time.Since(t0)
	if len(all.Items) != n {
		t.Fatalf("listed %d, want %d", len(all.Items), n)
	}

	t0 = time.Now()
	pages, seen, cont := 0, 0, ""
	for {
		page, err := s.List(ctx, "DNSRecordSet", store.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		seen += len(page.Items)
		if cont = page.Continue; cont == "" {
			break
		}
	}
	listPaged := time.Since(t0)
	if seen != n || pages != n/500 {
		t.Fatalf("paged listing saw %d objects in %d pages", seen, pages)
	}

	t0 = time.Now()
	sel, err := s.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: "ns03", LabelSelector: map[string]string{"team": "t3"}})
	if err != nil {
		t.Fatal(err)
	}
	listSelector := time.Since(t0)
	if len(sel.Items) != n/20 {
		t.Fatalf("selector matched %d, want %d", len(sel.Items), n/20)
	}

	getLat := make([]time.Duration, 0, 1000)
	var got v1alpha1.DNSRecordSet
	for i := 0; i < 1000; i++ {
		j := (i * 7919) % n
		t0 := time.Now()
		if err := s.Get(ctx, store.Key{Kind: "DNSRecordSet", Namespace: fmt.Sprintf("ns%02d", j%10), Name: fmt.Sprintf("host-%05d", j)}, &got); err != nil {
			t.Fatal(err)
		}
		getLat = append(getLat, time.Since(t0))
	}

	updLat := make([]time.Duration, 0, 1000)
	for i := 0; i < 1000; i++ {
		rs := recordSet(fmt.Sprintf("ns%02d", i%10), fmt.Sprintf("host-%05d", i), map[string]string{"team": fmt.Sprintf("t%d", i%4)})
		rs.Spec.Endpoints[0].Targets = []string{"10.0.0.2"}
		t0 := time.Now()
		if _, err := s.Apply(ctx, rs, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		updLat = append(updLat, time.Since(t0))
	}

	// Per-table bytes (tables plus their indexes), when SQLite was built
	// with the dbstat virtual table.
	if rows, err := s.r.QueryContext(ctx, `SELECT coalesce(m.tbl_name, d.name), sum(d.pgsize)
		FROM dbstat AS d LEFT JOIN sqlite_schema AS m ON m.name = d.name
		GROUP BY 1 ORDER BY 2 DESC`); err == nil {
		for rows.Next() {
			var name string
			var size int64
			if rows.Scan(&name, &size) == nil {
				t.Logf("  %-16s %9d bytes", name, size)
			}
		}
		_ = rows.Close()
	} else {
		t.Logf("dbstat unavailable: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 = time.Now()
	s2, err := Open(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	openTime := time.Since(t0)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	// The same open with Options.Validator unset: validator.New() compiles
	// the CEL rules of every embedded CRD. Skipped under the race detector,
	// where it only measures the detector.
	var openFresh time.Duration
	if !raceEnabled {
		t0 = time.Now()
		s3, err := Open(path, Options{Identity: "test-node"})
		if err != nil {
			t.Fatal(err)
		}
		openFresh = time.Since(t0)
		if err := s3.Close(); err != nil {
			t.Fatal(err)
		}
	}

	t.Logf("seed %d objects: %v total", n, seed)
	t.Logf("Apply create: p50 %v p99 %v", pct(applyLat, 50), pct(applyLat, 99))
	t.Logf("Apply update (spec change): p50 %v p99 %v", pct(updLat, 50), pct(updLat, 99))
	t.Logf("Get: p50 %v p99 %v", pct(getLat, 50), pct(getLat, 99))
	t.Logf("List all 10k: %v; 20 pages of 500: %v; namespace+label selector (500 hits): %v", listAll, listPaged, listSelector)
	t.Logf("state.db: %d bytes after checkpoint, %d bytes/object (incl. change feed and audit)", fi.Size(), fi.Size()/(n+1000))
	t.Logf("open with 10k objects: %v with a shared validator", openTime)
	if !raceEnabled {
		t.Logf("open with 10k objects: %v building validator.New() too", openFresh)
	}

	if !raceEnabled {
		if listAll > 3*time.Second {
			t.Errorf("listing 10k objects took %v", listAll)
		}
		if openTime > time.Second {
			t.Errorf("opening a 10k-object state.db took %v", openTime)
		}
	}
}

func pct(d []time.Duration, p int) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)*p/100]
}

// TestConcurrentWriters runs writers on distinct and shared objects at
// once: every write gets its own resourceVersion, change-feed row and audit
// row, and strict writers racing on one version see exactly one winner.
func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	const writers, perWriter = 8, 40
	mustApply(t, s, zone("shared", "shared.example."), store.ManagerAPI)

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter*2)
	for w := 0; w < writers; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := s.Apply(ctx, recordSet("apps", fmt.Sprintf("w%d-%d", w, i), nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					errs <- err
				}
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				z := zone("shared", "shared.example.")
				z.Spec.DefaultTTL = uint32(1000 + w*perWriter + i) // #nosec G115 -- small constants; distinct, so every write changes the spec
				if _, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var z v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "shared"}, &z); err != nil {
		t.Fatal(err)
	}
	if z.Generation != 1+writers*perWriter {
		t.Fatalf("shared generation = %d, want %d (a lost update)", z.Generation, 1+writers*perWriter)
	}
	total := 1 + 2*writers*perWriter
	if got := countRows(t, s, "changes"); got != total {
		t.Fatalf("change feed rows = %d, want %d", got, total)
	}
	if got := countRows(t, s, "audit"); got != total {
		t.Fatalf("audit rows = %d, want %d", got, total)
	}
	var distinct int
	if err := s.r.QueryRow(`SELECT count(DISTINCT resource_version) FROM changes`).Scan(&distinct); err != nil || distinct != total {
		t.Fatalf("resourceVersions are not unique: %d of %d (%v)", distinct, total, err)
	}
	if s.ResourceVersion() != fmt.Sprint(total) {
		t.Fatalf("head = %s, want %d", s.ResourceVersion(), total)
	}
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}

	// Strict writers: one version, many contenders, one winner.
	rv := z.ResourceVersion
	var ok, conflicts int
	var mu sync.Mutex
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := zone("shared", "shared.example.")
			c.Spec.DefaultTTL = uint32(10_000 + w) // #nosec G115 -- small constants
			_, err := s.Apply(ctx, c, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: rv})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrConflict):
				conflicts++
			default:
				t.Error(err)
			}
		}(w)
	}
	wg.Wait()
	if ok != 1 || conflicts != writers-1 {
		t.Fatalf("strict writers: %d succeeded, %d conflicted", ok, conflicts)
	}
}

func BenchmarkApplyCreate(b *testing.B) {
	s := openTemp(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Apply(ctx, recordSet("apps", fmt.Sprintf("r%d", i), nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplyUpdate(b *testing.B) {
	s := openTemp(b)
	ctx := context.Background()
	mustApply(b, s, recordSet("apps", "r", nil), store.ManagerAPI)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rs := recordSet("apps", "r", nil)
		rs.Spec.Endpoints[0].Targets = []string{fmt.Sprintf("10.0.%d.%d", (i/250)%250, i%250+1)}
		if _, err := s.Apply(ctx, rs, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplyNoop(b *testing.B) {
	s := openTemp(b)
	ctx := context.Background()
	mustApply(b, s, recordSet("apps", "r", nil), store.ManagerAPI)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Apply(ctx, recordSet("apps", "r", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	s := openTemp(b)
	ctx := context.Background()
	mustApply(b, s, recordSet("apps", "r", nil), store.ManagerAPI)
	key := store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "r"}
	var rs v1alpha1.DNSRecordSet
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Get(ctx, key, &rs); err != nil {
			b.Fatal(err)
		}
	}
}
