package sqlitelog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

func TestDomainHourExactEdgesAndFuture(t *testing.T) {
	w, fc := newWriter(t, t0.Add(30*time.Second))
	for i, sec := range []int{-3601, -3600, -3599, -31, -30, -1, 0, 1, 29, 30, 31, 3599, 3600} {
		mustWrite(t, w, event(t0.Add(time.Duration(sec)*time.Second), "10.0.0.1", fmt.Sprintf("d%d.example", i%3), withStatus([]pipeline.Status{pipeline.StatusForwarded, pipeline.StatusBlockedList}[i%2])))
	}
	// Same domain crossing each edge exercises indexed fact lookups, including
	// the first minute after the hour and future-dated rows already committed.
	for _, end := range []int{-3599, -30, 0, 1, 30, 31, 60, 3600, 3601} {
		for _, start := range []int{-3602, -3600, -30, 0, 30} {
			if start >= end {
				continue
			}
			from, until := t0.Add(time.Duration(start)*time.Second), t0.Add(time.Duration(end)*time.Second)
			err := w.Reader().read(t.Context(), true, func(tx *rtx, s *snapshot) error {
				src := w.Reader().domainSource(s)
				got, _, a, err := w.Reader().keyed(t.Context(), tx, s, src, from.Unix(), until.Unix(), 0, "", "k", 0, false)
				if err != nil {
					return err
				}
				src.domainFacts = false
				want, _, b, err := w.Reader().keyed(t.Context(), tx, s, src, from.Unix(), until.Unix(), 0, "", "k", 0, false)
				if err == nil && (!reflect.DeepEqual(got, want) || a != b) {
					t.Errorf("[%d,%d): %v want %v", start, end, got, want)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	// Deleting extrema leaves conservative timestamp bounds, still exact.
	if _, err := w.w.Exec(`DELETE FROM query WHERE ts<? OR ts>=?`, t0.UnixMicro(), t0.Add(31*time.Second).UnixMicro()); err != nil {
		t.Fatal(err)
	}
	top, err := w.Reader().TopDomains(t.Context(), TopQuery{Window: Window{From: t0, Until: fc.Now()}, N: 100})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, it := range top.Items {
		total += it.Total
	}
	if total != 2 {
		t.Fatalf("delete/boundary count=%d want 2", total)
	}
	if _, err := w.w.Exec(`DELETE FROM query`); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, w.w, `SELECT count(*) FROM query_domain_hour`); n != 0 {
		t.Fatalf("empty facts retain %d index cells", n)
	}
}

func TestDomainHourMigrationRollbackPrivacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "querylog.db")
	w, _ := newWriter(t, t0, func(o *Options) { o.Path = path })
	mustWrite(t, w, event(t0.Add(time.Second), "10.0.0.1", "before.example"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TRIGGER query_domain_insert`, `DROP TRIGGER query_domain_delete`, `DROP TABLE query_domain_hour`, `UPDATE meta SET value=2 WHERE key='schema_version'`} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	w2, _ := newWriter(t, t0, func(o *Options) { o.Path = path })
	if n := queryInt(t, w2.w, `SELECT sum(total) FROM query_domain_hour`); n != 1 {
		t.Fatalf("migration index=%d", n)
	}
	e := event(t0.Add(2*time.Second), "10.0.0.1", "rollback.example")
	e.ID = 1
	if err := w2.Write([]*querylog.Event{event(t0, "10.0.0.1", "first-in-failed-batch.example"), e}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if n := queryInt(t, w2.w, `SELECT sum(total) FROM query_domain_hour`); n != 1 {
		t.Fatalf("rollback index=%d", n)
	}
	w2.SetPrivacy(querylog.PrivacyNothing)
	mustWrite(t, w2, event(t0, "10.0.0.1", "private.example"))
	if n := queryInt(t, w2.w, `SELECT sum(total) FROM query_domain_hour`); n != 1 {
		t.Fatalf("privacy index=%d", n)
	}
}

func domainHourWorkload(t *testing.T, n, distinct int) (*Writer, Window, time.Duration) {
	t.Helper()
	w, fc := newWriter(t, t0)
	start := time.Now()
	for offset := 0; offset < n; offset += 1000 {
		batch := make([]*querylog.Event, 0, 1000)
		for i := offset; i < min(offset+1000, n); i++ {
			d := i % distinct
			if i >= distinct && i%20 == 0 {
				d = 0
			}
			batch = append(batch, event(t0.Add(time.Duration(i)*500*time.Microsecond), "10.0.0.1", fmt.Sprintf("d%d.example", d)))
		}
		mustWrite(t, w, batch...)
	}
	elapsed := time.Since(start)
	fc.SetTime(t0.Add(time.Duration(n)*500*time.Microsecond + time.Second))
	return w, Window{From: t0, Until: fc.Now()}, elapsed
}

func TestDomainHourBeyondOldCap(t *testing.T) {
	w, win, _ := domainHourWorkload(t, 110001, 100001)
	sum, err := w.Reader().Summary(t.Context(), SummaryQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Queries.Total != 110001 || sum.Queries.UniqueDomains != 100001 || sum.Approximate {
		t.Fatalf("summary total=%d unique=%d approximate=%v", sum.Queries.Total, sum.Queries.UniqueDomains, sum.Approximate)
	}
	top, err := w.Reader().TopDomains(t.Context(), TopQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Items) == 0 || top.Items[0].Key != "d0.example" || top.Approximate {
		t.Fatalf("top: %v", top)
	}
	for _, h := range w.acc.hours {
		if len(h.domains) != 0 {
			t.Fatal("live domains retained in writer memory")
		}
	}
	// The normal current-hour edge relation must be empty, making raw reads
	// independent of fact count. Pin the indexed boundary lookup plan as well.
	err = w.Reader().read(t.Context(), true, func(tx *rtx, _ *snapshot) error {
		args := []any{}
		parts := domainFactParts(tx, segment{from: win.From.Unix(), until: win.Until.Unix()}, 0, &args)
		rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+strings.Join(parts, " UNION ALL "), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var plan strings.Builder
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				return err
			}
			plan.WriteString(detail)
		}
		if !strings.Contains(plan.String(), "SEARCH q USING INDEX query_domain_ts") {
			t.Errorf("unindexed edge: %s", plan.String())
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDomainHourMillion reruns the original F16 scale without imposing a
// machine-dependent latency threshold on shared CI or race builds.
func TestDomainHourMillion(t *testing.T) {
	if os.Getenv("KUDURRU_MEASURE") != "1" {
		t.Skip("set KUDURRU_MEASURE=1 for 1M-row acceptance")
	}
	const n, distinct = 1000000, 252000
	w, win, ingest := domainHourWorkload(t, n, distinct)
	r := w.Reader()
	for _, countOnly := range []bool{true, false} {
		var timings [2]time.Duration
		for i, live := range []bool{false, true} {
			start := time.Now()
			err := r.read(t.Context(), true, func(tx *rtx, s *snapshot) error {
				src := r.domainSource(s)
				src.domainFacts = live
				rows, count, approx, err := r.keyed(t.Context(), tx, s, src, win.From.Unix(), win.Until.Unix(), 0, "", "sum(a) DESC,k", 10, countOnly)
				if err == nil && (approx || countOnly && count != distinct || !countOnly && (len(rows) != 10 || rows[0].a < 37000)) {
					t.Errorf("million countOnly=%v index=%v count=%d approximate=%v", countOnly, live, count, approx)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			timings[i] = time.Since(start)
		}
		t.Logf("F16 1M/252k countOnly=%v raw=%s index=%s", countOnly, timings[0], timings[1])
	}
	start := time.Now()
	sum, err := r.Summary(context.Background(), SummaryQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	summaryTime := time.Since(start)
	if sum.Queries.Total != n || sum.Queries.UniqueDomains != distinct || sum.Approximate {
		t.Fatal("million summary is not exact")
	}
	start = time.Now()
	_, err = r.TopDomains(t.Context(), TopQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	topTime := time.Since(start)

	// A narrow second-aligned interval cuts many repeated domain cells.
	// Exact edge reads must not include their later occurrences.
	if err := r.read(t.Context(), true, func(tx *rtx, s *snapshot) error {
		_, count, approx, err := r.keyed(t.Context(), tx, s, r.domainSource(s), t0.Add(30*time.Second).Unix(), t0.Add(60*time.Second).Unix(), 0, "", "", 0, true)
		if err == nil && (count != 60000 || approx) {
			t.Errorf("million ragged boundary unique=%d approximate=%v", count, approx)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var indexBytes int64
	if err := w.w.QueryRow(`SELECT sum(pgsize) FROM dbstat WHERE name='query_domain_hour'`).Scan(&indexBytes); err != nil {
		t.Fatal(err)
	}
	t.Logf("F16 1M/252k ingest=%s rowsPerSecond=%.0f summary=%s top=%s indexBytes=%d", ingest, float64(n)/ingest.Seconds(), summaryTime, topTime, indexBytes)
}

// A write and flush after the state copy must not advance the SQL snapshot.
// This catches mixed old base/client counts and newer domain-index counts.
func TestDomainHourReadSnapshotAcrossWrite(t *testing.T) {
	w, fc := newWriter(t, t0)
	mustWrite(t, w, event(t0, "10.0.0.1", "before.example"))
	fc.SetTime(t0.Add(time.Minute))
	err := w.Reader().read(t.Context(), true, func(tx *rtx, s *snapshot) error {
		done := make(chan error, 1)
		go func() {
			if err := w.Write([]*querylog.Event{event(t0, "10.0.0.2", "after.example")}); err != nil {
				done <- err
				return
			}
			done <- w.Flush(t.Context())
		}()
		if err := <-done; err != nil {
			return err
		}
		base, _, err := w.Reader().baseSeries(t.Context(), tx, s, t0.Unix(), fc.Now().Unix(), 0)
		if err != nil {
			return err
		}
		_, domains, _, err := w.Reader().keyed(t.Context(), tx, s, w.Reader().domainSource(s), t0.Unix(), fc.Now().Unix(), 0, "", "", 0, true)
		if err == nil && (base[0][cTotal] != 1 || domains != 1) {
			t.Errorf("mixed snapshot total=%d domains=%d", base[0][cTotal], domains)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := w.Reader().Summary(t.Context(), SummaryQuery{Window: Window{From: t0, Until: fc.Now()}})
	if err != nil || sum.Queries.Total != 2 || sum.Queries.UniqueDomains != 2 {
		t.Fatalf("next snapshot: %+v: %v", sum, err)
	}
}
