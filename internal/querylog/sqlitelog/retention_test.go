package sqlitelog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// seed writes n events spread evenly over [from, from+span) in batches of
// 1 000.
func seed(t *testing.T, w *Writer, from time.Time, span time.Duration, n int) {
	t.Helper()
	batch := make([]*querylog.Event, 0, 1000)
	for i := range n {
		ts := from.Add(time.Duration(int64(span) / int64(n) * int64(i)))
		batch = append(batch, event(ts, fmt.Sprintf("10.0.%d.%d", i%7, i%200+1), fmt.Sprintf("d%d.example", i%3000)))
		if len(batch) == cap(batch) {
			mustWrite(t, w, batch...)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		mustWrite(t, w, batch...)
	}
}

// scale shrinks row counts and the delete batch under the race detector,
// where the pure-Go SQLite is ~20x slower; the batching logic is the same.
func scale(w *Writer, n int) int {
	if raceEnabled {
		w.deleteBatch = DeleteBatch / 10
		return n / 10
	}
	return n
}

func TestRetentionRawAndVacuum(t *testing.T) {
	w, _ := newWriter(t, t0)
	ctx := context.Background()
	old, fresh := scale(w, 25000), scale(w, 5000)
	seed(t, w, t0.Add(-40*24*time.Hour), 9*24*time.Hour, old) // all older than 30 d
	seed(t, w, t0.Add(-24*time.Hour), 24*time.Hour, fresh)
	before := queryInt(t, w.w, `PRAGMA page_count`)
	rep, err := w.Retain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RawDeleted != int64(old) {
		t.Errorf("raw deleted = %d, want %d", rep.RawDeleted, old)
	}
	if got := queryInt(t, w.w, `SELECT count(*) FROM query`); got != int64(fresh) || w.rows.Load() != int64(fresh) {
		t.Errorf("rows left = %d (counter %d)", got, w.rows.Load())
	}
	if oldest := queryInt(t, w.w, `SELECT min(ts) FROM query`); oldest < t0.Add(-30*24*time.Hour).UnixMicro() {
		t.Errorf("row older than raw retention survived: %d", oldest)
	}
	if free := queryInt(t, w.w, `PRAGMA freelist_count`); free != 0 {
		t.Errorf("freelist_count = %d after incremental_vacuum", free)
	}
	after := queryInt(t, w.w, `PRAGMA page_count`)
	if after >= before {
		t.Errorf("page_count %d → %d: incremental_vacuum returned nothing", before, after)
	}
	if rep.DBBytesAfter >= rep.DBBytesBefore {
		t.Errorf("db bytes %d → %d", rep.DBBytesBefore, rep.DBBytesAfter)
	}
	t.Logf("retention: %+v, pages %d → %d", rep, before, after)
	if got := testutil.ToFloat64(w.metrics.Rows); got != float64(fresh) {
		t.Errorf("kudurru_querylog_rows = %v", got)
	}
	if got := testutil.ToFloat64(w.metrics.OldestTS); got < float64(t0.Add(-24*time.Hour).Unix()) {
		t.Errorf("oldest_timestamp_seconds = %v", got)
	}
}

func TestRetentionSizeCap(t *testing.T) {
	w, _ := newWriter(t, t0, func(o *Options) { o.RawRetention = 0 })
	ctx := context.Background()
	seed(t, w, t0.Add(-2*time.Hour), 2*time.Hour, scale(w, 40000))
	size, err := w.DBBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w.opts.MaxDBBytes = size / 2
	deleted, err := w.enforceSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := w.DBBytes(ctx)
	if after > w.opts.MaxDBBytes || deleted == 0 {
		t.Errorf("size cap: %d → %d bytes (cap %d), %d rows deleted", size, after, w.opts.MaxDBBytes, deleted)
	}
	if lo := queryInt(t, w.w, `SELECT min(id) FROM query`); lo != deleted+1 {
		t.Errorf("oldest rows must go first: min id %d after deleting %d", lo, deleted)
	}
	if deleted%int64(w.deleteBatch) != 0 {
		t.Errorf("deleted %d, not a multiple of the %d-row batch", deleted, w.deleteBatch)
	}
	t.Logf("size cap %d B: %d → %d B, %d rows deleted", w.opts.MaxDBBytes, size, after, deleted)

	// Maintain enforces the cap on its own schedule. (The cap stays above
	// the dictionaries and rollups, which the size cap never deletes.)
	w.opts.MaxDBBytes = after / 5 * 4
	w.lastSizeCheck = time.Time{}
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if now, _ := w.DBBytes(ctx); now > w.opts.MaxDBBytes {
		t.Errorf("Maintain left %d bytes over a %d cap", now, w.opts.MaxDBBytes)
	}
}

func TestRollupExpiry(t *testing.T) {
	w, _ := newWriter(t, t0)
	ctx := context.Background()
	day := int64(86400)
	now := t0.Unix()
	for _, ts := range []int64{now - 36*day, now - 34*day, now - 401*day, now - 399*day} {
		for _, table := range []string{"rollup_1m", "rollup_1h", "rollup_1d"} {
			if _, err := w.w.Exec(`INSERT INTO `+table+`(ts,node,total,blocked,cached,forwarded,authoritative,dropped) VALUES(?,1,1,0,0,0,0,0)`, floorTo(ts, 60)); err != nil { //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
				t.Fatal(err)
			}
		}
		if _, err := w.w.Exec(`INSERT INTO rollup_domain_1h(ts,node,domain,total,blocked) VALUES(?,1,1,1,0)`, floorTo(ts, 3600)); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := w.Retain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"rollup_1m": 1, "rollup_1h": 3, "rollup_domain_1h": 3, "rollup_1d": 4} {
		if got := queryInt(t, w.w, `SELECT count(*) FROM `+table); got != want {
			t.Errorf("%s keeps %d rows, want %d", table, got, want)
		}
	}
	if rep.RollupsDeleted != 3+1+1 {
		t.Errorf("rollups deleted = %d", rep.RollupsDeleted)
	}
}

func TestDictionaryPruning(t *testing.T) {
	w, _ := newWriter(t, t0)
	ctx := context.Background()
	mustWrite(t, w, event(t0.Add(-time.Hour), "10.0.0.1", "fresh.example"))
	old := t0.Add(-500 * 24 * time.Hour).Unix()
	if _, err := w.w.Exec(`INSERT INTO dict_domain(name, first_seen, last_seen) VALUES('stale.example', ?1, ?1)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := w.w.Exec(`INSERT INTO dict_client(ip, name, first_seen, last_seen) VALUES('10.9.9.9', '', ?1, ?1)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := w.w.Exec(`INSERT INTO rollup_domain_1d(ts,node,domain,total,blocked) SELECT ?, 1, id, 5, 0 FROM dict_domain WHERE name='stale.example'`, floorTo(old, 86400)); err != nil {
		t.Fatal(err)
	}
	rep, err := w.Retain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DictPruned != 2 {
		t.Errorf("pruned %d dictionary rows, want 2", rep.DictPruned)
	}
	if n := queryInt(t, w.w, `SELECT count(*) FROM dict_domain WHERE name='fresh.example'`); n != 1 {
		t.Error("fresh domain pruned")
	}
	if n := queryInt(t, w.w, `SELECT count(*) FROM rollup_domain_1d`); n != 0 {
		t.Errorf("daily rows of a pruned key survive: %d", n)
	}
	// The cache was purged: writing the fresh name again still resolves.
	mustWrite(t, w, event(t0, "10.0.0.1", "fresh.example"))
}

func TestCheckpointWaitsForReaders(t *testing.T) {
	w, _ := newWriter(t, t0)
	ctx := context.Background()
	seed(t, w, t0.Add(-time.Hour), time.Hour, 3000)
	if w.WALBytes() == 0 {
		t.Fatal("expected a WAL after writes")
	}
	w.readers.Add(1)
	if ok, err := w.Checkpoint(ctx); ok || err != nil {
		t.Errorf("checkpoint with an active reader: ran=%v err=%v", ok, err)
	}
	w.readers.Add(-1)
	if ok, err := w.Checkpoint(ctx); !ok || err != nil {
		t.Errorf("idle checkpoint: ran=%v err=%v", ok, err)
	}
	if b := w.WALBytes(); b != 0 {
		t.Errorf("WAL is %d bytes after wal_checkpoint(TRUNCATE)", b)
	}
}

func TestRetentionSchedule(t *testing.T) {
	loc := time.FixedZone("test", 2*3600)
	for _, tc := range []struct {
		now  time.Time
		min  int
		want time.Time
	}{
		{time.Date(2026, 10, 11, 1, 0, 0, 0, loc), 17, time.Date(2026, 10, 11, 3, 17, 0, 0, loc)},
		{time.Date(2026, 10, 11, 3, 17, 0, 0, loc), 17, time.Date(2026, 10, 12, 3, 17, 0, 0, loc)},
		{time.Date(2026, 10, 11, 12, 0, 0, 0, loc), 50, time.Date(2026, 10, 12, 3, 50, 0, 0, loc)},
	} {
		if got := nextRetentionTime(tc.now, tc.min); !got.Equal(tc.want) {
			t.Errorf("nextRetentionTime(%s, %d) = %s, want %s", tc.now, tc.min, got, tc.want)
		}
	}
	for range 20 {
		w, _ := newWriter(t, t0)
		nr := w.nextRetention
		if nr.Hour() != 3 || nr.Minute() < 10 || nr.Minute() > 50 || !nr.After(t0) || nr.Sub(t0) > 24*time.Hour {
			t.Fatalf("retention scheduled at %s", nr)
		}
	}

	// Maintain runs the job when the time comes and schedules the next day.
	w, fc := newWriter(t, t0)
	seed(t, w, t0.Add(-31*24*time.Hour), time.Hour, 100)
	first := w.nextRetention
	fc.SetTime(first)
	if err := w.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, w.w, `SELECT count(*) FROM query`); n != 0 {
		t.Errorf("scheduled retention left %d expired rows", n)
	}
	if !w.nextRetention.Equal(first.AddDate(0, 0, 1)) {
		t.Errorf("next run %s, want %s", w.nextRetention, first.AddDate(0, 0, 1))
	}
	if v := queryInt(t, w.w, `SELECT value FROM meta WHERE key='last_retention'`); v != first.Unix() {
		t.Errorf("meta last_retention = %d", v)
	}
}

func TestWeeklyAnalyze(t *testing.T) {
	w, fc := newWriter(t, t0)
	seed(t, w, t0.Add(-time.Hour), time.Hour, 2000)
	fc.SetTime(t0.Add(7*24*time.Hour + time.Minute))
	w.nextRetention = t0.Add(30 * 24 * time.Hour) // keep retention out of the way
	if err := w.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, w.w, `SELECT count(*) FROM sqlite_master WHERE name='sqlite_stat1'`); n != 1 {
		t.Error("ANALYZE did not run after a week")
	}
	if v := queryInt(t, w.w, `SELECT value FROM meta WHERE key='last_analyze'`); v != fc.Now().Unix() {
		t.Errorf("last_analyze = %d", v)
	}
}
