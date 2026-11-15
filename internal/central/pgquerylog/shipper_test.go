package pgquerylog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	testclock "k8s.io/utils/clock/testing"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// t0 is the fake now: a Sunday noon, UTC.
var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writer(t *testing.T, node string, fc *testclock.FakeClock) *sqlitelog.Writer {
	t.Helper()
	w, err := sqlitelog.Open(context.Background(), sqlitelog.Options{
		Path: filepath.Join(t.TempDir(), sqlitelog.FileName), Node: node, Privacy: querylog.PrivacyFull,
		RawRetention: 30 * 24 * time.Hour, Clock: fc, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	nextID = 0 // event ids start over with every file
	return w
}

// nextID stamps test events (the collector's job), per file.
var nextID uint64

func ev(node string, ts time.Time, client, name string, st pipeline.Status) *querylog.Event {
	nextID++
	e := &querylog.Event{ID: nextID, Time: ts, Latency: 3 * time.Millisecond, Node: node, Proto: pipeline.ProtoUDP,
		Client: netip.MustParseAddr(client), ClientName: "host-" + client, ClientGroup: "default/default",
		Name: name + ".", QType: 1, QClass: 1, Status: st, ReplyType: querylog.ReplyIP, Answers: 1, Upstream: "default", Policy: "default/default"}
	if st >= 16 && st <= 23 {
		e.Upstream, e.List, e.Rule = "", "default/ads", "||"+name+"^"
	}
	return e
}

func write(t *testing.T, w *sqlitelog.Writer, evs ...*querylog.Event) {
	t.Helper()
	if err := w.Write(evs); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func shipper(t *testing.T, db *central.DB, w *sqlitelog.Writer, node string, fc *testclock.FakeClock) *Shipper {
	t.Helper()
	s := &Shipper{DB: db, Local: w, Node: node, Facts: true, Batch: 7, Now: fc.Now, Logger: quiet()}
	s.defaults()
	src, err := w.SourceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.source = src
	w.SetShippedThrough(s.ShippedThrough)
	return s
}

// shipAll runs rounds until no full batch is left; rollups ship every
// round.
func shipAll(t *testing.T, s *Shipper) {
	t.Helper()
	for range 1000 {
		s.lastRollup = time.Time{}
		err := s.round(context.Background())
		if err == nil {
			return
		}
		if !errors.Is(err, errMore) {
			t.Fatal(err)
		}
	}
	t.Fatal("shipping did not finish")
}

func scalar(t *testing.T, db *central.DB, q string, args ...any) int64 {
	t.Helper()
	pool, err := db.Pool()
	if err != nil {
		t.Fatal(err)
	}
	var n *int64
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if n == nil {
		return 0
	}
	return *n
}

func TestShipFactsAndRollups(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "node-a", fc)
	ctx := context.Background()
	// 09:30 to 10:55: every event falls in an hour the node completes.
	var evs []*querylog.Event
	for i := range 20 {
		st := pipeline.StatusForwarded
		if i%4 == 0 {
			st = pipeline.StatusBlockedList
		}
		evs = append(evs, ev("node-a", t0.Add(-150*time.Minute+time.Duration(i)*5*time.Minute), "10.0.0."+string(rune('1'+i%3)), "d"+string(rune('a'+i%5))+".example", st))
	}
	write(t, w, evs...)
	fc.SetTime(t0.Add(5 * time.Minute))
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	hourDone, _ := w.RollupProgress()
	if hourDone != t0.Unix() {
		t.Fatalf("local hours done through %s", time.Unix(hourDone, 0).UTC())
	}
	s := shipper(t, db, w, "node-a", fc)
	shipAll(t, s)

	if n := scalar(t, db, `SELECT count(*) FROM query`); n != 20 {
		t.Fatalf("central facts %d", n)
	}
	if n := scalar(t, db, `SELECT count(*) FROM query q JOIN dict_list l ON l.id = q.list WHERE l.name = 'default/ads' AND q.status = 16`); n != 5 {
		t.Fatalf("blocked rows with their list %d", n)
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_client WHERE grp = 'default/default' AND name LIKE 'host-%'`); n != 3 {
		t.Fatalf("clients %d", n)
	}
	if n := scalar(t, db, `SELECT sum(total) FROM rollup_1m`); n != 20 {
		t.Fatalf("minute totals %d", n)
	}
	if n := scalar(t, db, `SELECT sum(blocked) FROM rollup_1h`); n != 5 {
		t.Fatalf("hour blocked %d", n)
	}
	if n := scalar(t, db, `SELECT sum(total) FROM rollup_domain_1h`); n != 20 {
		t.Fatalf("domain hours %d", n)
	}
	if n := scalar(t, db, `SELECT c.last_id FROM ship_cursor c WHERE c.stream = 'source_rollup_1h'`); n != t0.Unix() {
		t.Fatalf("hour cursor %d", n)
	}
	if s.ShippedThrough() != 20 {
		t.Fatalf("shipped through %d", s.ShippedThrough())
	}

	// Again: nothing new, nothing doubled.
	shipAll(t, s)
	if n := scalar(t, db, `SELECT count(*) FROM query`); n != 20 {
		t.Fatalf("after a second round %d facts", n)
	}
	if n := scalar(t, db, `SELECT sum(total) FROM rollup_1m`); n != 20 {
		t.Fatalf("after a second round %d minute totals", n)
	}

	// A late event in a minute already flushed and shipped: the minute is
	// sent again whole.
	// 10:55 held one event.
	late := ev("node-a", t0.Add(-65*time.Minute+time.Second), "10.0.0.9", "late.example", pipeline.StatusCached)
	write(t, w, late, ev("node-a", t0.Add(-65*time.Minute+2*time.Second), "10.0.0.9", "late.example", pipeline.StatusCached))
	shipAll(t, s)
	if n := scalar(t, db, `SELECT sum(total) FROM rollup_1m`); n != 22 {
		t.Fatalf("after the late events %d minute totals", n)
	}
	if n := scalar(t, db, `SELECT total FROM rollup_1m WHERE ts = $1`, t0.Add(-65*time.Minute).Unix()); n != 3 {
		t.Fatalf("the late minute holds %d", n)
	}
}

func TestTwoNodesSum(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	for _, node := range []string{"node-a", "node-b"} {
		w := writer(t, node, fc)
		write(t, w, ev(node, t0.Add(-3*time.Minute), "10.0.0.1", "shared.example", pipeline.StatusForwarded),
			ev(node, t0.Add(-2*time.Minute), "10.0.0.2", node+".example", pipeline.StatusForwarded))
		shipAll(t, shipper(t, db, w, node, fc))
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_domain WHERE name = 'shared.example'`); n != 1 {
		t.Fatalf("shared domain entries %d", n)
	}
	if n := scalar(t, db, `SELECT count(DISTINCT node) FROM rollup_1m`); n != 2 {
		t.Fatalf("nodes in the minute rollups %d", n)
	}
	if n := scalar(t, db, `SELECT sum(total) FROM rollup_1m WHERE ts = $1`, t0.Add(-3*time.Minute).Unix()); n != 2 {
		t.Fatalf("cluster total of the shared minute %d", n)
	}
	if n := scalar(t, db, `SELECT count(*) FROM source WHERE kind = 'querylog'`); n != 2 {
		t.Fatalf("sources %d", n)
	}
}

// TestRetentionGuard: the node keeps the rows it has not shipped.
func TestRetentionGuard(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "node-a", fc)
	write(t, w, ev("node-a", t0.Add(-40*24*time.Hour), "10.0.0.1", "old.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "node-a", fc)
	// Nothing shipped yet: the 40-day-old row survives local retention.
	if rep, err := w.Retain(context.Background()); err != nil || rep.RawDeleted != 0 {
		t.Fatalf("retain before shipping: %+v %v", rep, err)
	}
	shipAll(t, s)
	if rep, err := w.Retain(context.Background()); err != nil || rep.RawDeleted != 1 {
		t.Fatalf("retain after shipping: %+v %v", rep, err)
	}
}

// TestCentralRetention: the central retention skips old rows on the way
// in and the janitor drops whole days.
func TestCentralRetention(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "node-a", fc)
	write(t, w, ev("node-a", t0.Add(-10*24*time.Hour), "10.0.0.1", "old.example", pipeline.StatusForwarded),
		ev("node-a", t0.Add(-3*24*time.Hour), "10.0.0.1", "mid.example", pipeline.StatusForwarded),
		ev("node-a", t0.Add(-time.Minute), "10.0.0.1", "new.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "node-a", fc)
	s.Retention = 7 * 24 * time.Hour
	shipAll(t, s)
	if n := scalar(t, db, `SELECT count(*) FROM query`); n != 2 {
		t.Fatalf("facts within the central retention %d", n)
	}
	ctx := context.Background()
	// Retention waits for durable source/hour coverage.
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	shipAll(t, s)
	for _, job := range Jobs(2 * 24 * time.Hour) {
		if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := job.Run(ctx, tx, t0); return err }); err != nil {
			t.Fatalf("%s: %v", job.Name, err)
		}
	}
	if n := scalar(t, db, `SELECT count(*) FROM query`); n != 1 {
		t.Fatalf("facts after dropping expired days %d", n)
	}
	if n := scalar(t, db, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'query'::regclass`); n < 5 {
		t.Fatalf("partitions ahead %d", n)
	}
}

func TestRollupsOnly(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "node-a", fc)
	write(t, w, ev("node-a", t0.Add(-time.Minute), "10.0.0.1", "a.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "node-a", fc)
	s.Facts = false
	shipAll(t, s)
	if n := scalar(t, db, `SELECT count(*) FROM query`); n != 0 {
		t.Fatalf("facts shipped with facts off: %d", n)
	}
	if n := scalar(t, db, `SELECT sum(total) FROM rollup_1m`); n != 1 {
		t.Fatalf("minute totals %d", n)
	}
	// No fact rows are shipped, so retention needs no guard.
	if s.ShippedThrough() < 1<<40 {
		t.Fatalf("guard %d", s.ShippedThrough())
	}
}

// TestPurgeByPartition: whole days are truncated, the cutoff's day is
// deleted row by row, and the counts add up.
func TestPurgeByPartition(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "node-a", fc)
	write(t, w,
		ev("node-a", t0.Add(-50*time.Hour), "10.0.0.1", "two-days.example", pipeline.StatusForwarded),
		ev("node-a", t0.Add(-26*time.Hour), "10.0.0.1", "yesterday.example", pipeline.StatusForwarded),
		ev("node-a", t0.Add(-11*time.Hour), "10.0.0.1", "early.example", pipeline.StatusForwarded), // 01:00 today
		ev("node-a", t0.Add(-time.Hour), "10.0.0.1", "late.example", pipeline.StatusForwarded))     // 11:00 today
	shipAll(t, shipper(t, db, w, "node-a", fc))
	ctx := context.Background()
	// Before 06:00 today: two whole days and one row of today.
	n, err := Purger{DB: db}.Purge(ctx, t0.Add(-6*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("purged %d: %v", n, err)
	}
	if left := scalar(t, db, `SELECT count(*) FROM query`); left != 1 {
		t.Fatalf("%d rows left", left)
	}
	if n, err = (Purger{DB: db}).Purge(ctx, time.Time{}); err != nil || n != 1 {
		t.Fatalf("purge all: %d: %v", n, err)
	}
}

// TestJanitorLiftsStatementTimeout: housekeeping is not cut off by the
// pool's 15 s read timeout.
func TestJanitorLiftsStatementTimeout(t *testing.T) {
	db := centraltest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	var setting string
	j := &central.Janitor{DB: db, Logger: quiet(), Jobs: []central.Job{{Name: "probe", Run: func(ctx context.Context, tx pgx.Tx, _ time.Time) (int64, error) {
		defer cancel()
		return 0, tx.QueryRow(ctx, `SELECT current_setting('statement_timeout')`).Scan(&setting)
	}}}}
	_ = j.Run(ctx)
	if setting != "0" {
		t.Fatalf("statement_timeout in a janitor job: %q", setting)
	}
	pool, _ := db.Pool()
	if err := pool.QueryRow(context.Background(), `SELECT current_setting('statement_timeout')`).Scan(&setting); err != nil || setting != "15s" {
		t.Fatalf("pool statement_timeout %q: %v", setting, err)
	}
}
