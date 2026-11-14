package pgquerylog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	testclock "k8s.io/utils/clock/testing"
)

func backfillPass(t *testing.T, db *central.DB, now time.Time) int64 {
	t.Helper()
	var n int64
	if err := db.Tx(t.Context(), func(tx pgx.Tx) error { var err error; n, err = backfillStopped(t.Context(), tx, now); return err }); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoppedSourceHourDayLateReturn(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-45 * time.Minute))
	ctx := t.Context()
	hour := t0.Add(-time.Hour).Unix()
	a := writer(t, "same", fc)
	fixture(t, a, "same", fc.Now(), 0)
	fc.Step(3 * time.Minute)
	// The varied fixture includes three completed earlier hours and a partial
	// final hour, with every metric family. Local compaction remains stopped.
	if err := a.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	sa := shipper(t, db, a, "same", fc)
	shipAll(t, sa)
	b := writer(t, "same", fc)
	write(t, b, ev("same", fc.Now().Add(-time.Minute), "10.1.0.1", "second-source.example", pipeline.StatusBlockedList))
	sb := shipper(t, db, b, "same", fc)
	shipAll(t, sb)
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	boundary := fc.Now().Add(2 * time.Hour)
	if n := backfillPass(t, db, boundary.Add(-time.Nanosecond)); n != 0 {
		t.Fatal("source eligible before exact two hours")
	}
	for range 4 {
		backfillPass(t, db, boundary)
	}
	if got := scalar(t, db, `SELECT sum(total) FROM rollup_1h WHERE ts=$1`, hour); got == 0 {
		t.Fatal("partial hour not summarized")
	}
	total := scalar(t, db, `SELECT sum(total) FROM rollup_1h`)
	if total != 241 {
		t.Fatalf("all retained hours total=%d", total)
	}
	if n := backfillPass(t, db, boundary); n != 0 {
		t.Fatal("repeat backfill changed complete inputs")
	}
	// Daily work must run when the day closes, even without another fact/hour.
	nextDay := time.Unix(hour/86400*86400+86400, 0).UTC()
	for range 3 {
		backfillPass(t, db, nextDay.Add(-time.Nanosecond))
	}
	if scalar(t, db, `SELECT count(*) FROM rollup_1d`) != 0 {
		t.Fatal("open UTC day summarized")
	}
	for range 4 {
		backfillPass(t, db, nextDay)
	}
	if got := scalar(t, db, `SELECT sum(total) FROM rollup_1d`); got != 241 {
		t.Fatalf("daily total=%d", got)
	}
	beforeOther := scalar(t, db, `SELECT sum(total) FROM rollup_1h WHERE source=(SELECT id FROM source WHERE uuid=$1)`, sb.source)
	// Returning authoritative hour/day shipments replace only A, including its
	// synthesized rows. B's distinct file shares the node but keeps its counts.
	fc.SetTime(nextDay)
	if err := a.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	shipAll(t, sa)
	if got := scalar(t, db, `SELECT sum(total) FROM rollup_1h`); got != 241 {
		t.Fatalf("official hour double counted/lost source: %d", got)
	}
	if got := scalar(t, db, `SELECT sum(total) FROM rollup_1d`); got != 241 {
		t.Fatalf("official day double counted/lost source: %d", got)
	}
	if got := scalar(t, db, `SELECT sum(total) FROM rollup_1h WHERE source=(SELECT id FROM source WHERE uuid=$1)`, sb.source); got != beforeOther {
		t.Fatal("return overwrote other source")
	}
	r := centralReader(db, fc).ForNode("same")
	sum, err := r.Summary(ctx, sqlitelog.SummaryQuery{Window: sqlitelog.Window{From: t0.Add(-24 * time.Hour), Until: nextDay}})
	if err != nil || sum.Queries.Total != 241 {
		t.Fatalf("reader lost completed backfill: %v", err)
	}
}

func TestStoppedSourceRollbackAndSourceLockIsolation(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	for _, node := range []string{"locked", "free"} {
		w := writer(t, node, fc)
		write(t, w, ev(node, fc.Now(), "10.0.0.1", node+".example", pipeline.StatusForwarded))
		shipAll(t, shipper(t, db, w, node, fc))
	}
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	rollback := errors.New("test rollback")
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := backfillStopped(ctx, tx, fc.Now().Add(2*time.Hour)); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || scalar(t, db, `SELECT count(*) FROM rollup_1h`) != 0 {
		t.Fatal("rollback retained summary")
	}
	lock, err := prunePool(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(ctx, `SELECT id FROM source WHERE node='locked' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	backfillPass(t, db, fc.Now().Add(2*time.Hour))
	if got := scalar(t, db, `SELECT sum(total) FROM rollup_1h WHERE node=(SELECT id FROM dict_node WHERE name='free')`); got != 1 {
		t.Fatal("other source blocked")
	}
	if got := scalar(t, db, `SELECT count(*) FROM rollup_1h WHERE node=(SELECT id FROM dict_node WHERE name='locked')`); got != 0 {
		t.Fatal("ignored source lock")
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		backfillPass(t, db, fc.Now().Add(2*time.Hour))
	}
	if got := scalar(t, db, `SELECT sum(total) FROM rollup_1h`); got != 2 {
		t.Fatal("restart/wrap lost blocked source")
	}
}

// Compare every aggregate column with local compaction, including latency
// histograms, all statuses/transports and all seven hourly/three daily families.
func TestStoppedSourceMetricsAndLateFactRevision(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	w := writer(t, "metrics", fc)
	statuses := []pipeline.Status{pipeline.StatusForwarded, pipeline.StatusCached, pipeline.StatusCachedStale, pipeline.StatusAuthoritative, pipeline.StatusLocalRecord, pipeline.StatusPrefetchRefresh, pipeline.StatusSpecialDomain, pipeline.StatusRetried, pipeline.StatusBlockedList, pipeline.StatusBlockedRegex, pipeline.StatusBlockedExact, pipeline.StatusBlockedCNAME, pipeline.StatusBlockedUpstream, pipeline.StatusBlockedRPZ, pipeline.StatusBlockedSafeSearch, pipeline.StatusBlockedRateLimit, pipeline.StatusRefusedACL, pipeline.StatusDropped, pipeline.StatusServfailUpstream, pipeline.StatusTimeoutUpstream, pipeline.StatusDNSSECBogus, pipeline.StatusServfailUnsynced}
	protos := []pipeline.Proto{pipeline.ProtoUDP, pipeline.ProtoTCP, pipeline.ProtoDoT, pipeline.ProtoDoH, pipeline.ProtoDoQ, pipeline.ProtoDoH3}
	var events []*querylog.Event
	for i := range 240 {
		e := ev("metrics", fc.Now().Add(-3*time.Hour+time.Duration(i)*40*time.Second), fmt.Sprintf("192.0.2.%d", 1+i%5), fmt.Sprintf("metric%d.example", i%11), statuses[i%len(statuses)])
		e.Proto = protos[i%len(protos)]
		e.Latency = time.Duration([]int64{0, 1, 999, 1000, 4999, 5000, 9999, 10000, 49999, 50000, 99999, 100000, 499999, 500000, 1000000}[i%15]) * time.Microsecond
		e.Rcode = []int{0, 3, 2, 5, 1}[i%5]
		e.QType = []uint16{1, 28, 65, 15}[i%4]
		if e.Status == pipeline.StatusAuthoritative || e.Status == pipeline.StatusLocalRecord {
			e.Zone = "metric.example."
			e.Upstream = ""
		}
		if i%13 == 0 || e.Status == pipeline.StatusDropped {
			e.ReplyType = querylog.ReplyNone
		}
		events = append(events, e)
	}
	write(t, w, events...)
	s := shipper(t, db, w, "metrics", fc)
	shipAll(t, s)
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	fc.Step(3 * time.Hour)
	for range 5 {
		backfillPass(t, db, fc.Now())
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 240 {
		t.Fatal("initial facts lost")
	}
	// The same hour receives a late fact after synthesis. It invalidates both
	// coverage levels in the fact transaction before any background work runs.
	lateHour := t0.Add(-time.Hour).Unix()
	write(t, w, ev("metrics", time.Unix(lateHour+5, 0), "192.0.2.99", "late-metric.example", pipeline.StatusForwarded))
	if _, err := s.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db, `SELECT count(*) FROM rollup_bucket WHERE resolution=3600 AND ts=$1 AND origin='pending'`, lateHour) != 1 {
		t.Fatal("late input did not invalidate committed summary")
	}
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now().Add(-3*time.Hour))
	fc.SetTime(time.Unix(t0.Unix()/86400*86400+86400, 0).UTC().Add(3 * time.Hour))
	for range 8 {
		backfillPass(t, db, fc.Now())
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 241 || scalar(t, db, `SELECT sum(total) FROM rollup_1d`) != 241 {
		t.Fatal("late input not reconciled exactly once")
	}
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range sqlitelog.RollupTables {
		if table.Res == 60 {
			continue
		}
		t.Run(table.Name, func(t *testing.T) {
			rows, err := w.ExportRollup(ctx, table, 0, fc.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			want := make([]int64, len(table.Cols))
			for _, row := range rows {
				for i, v := range row.Values {
					want[i] += v
				}
			}
			for i, col := range table.Cols {
				got := scalar(t, db, `SELECT coalesce(sum(`+col+`),0) FROM `+table.Name)
				if got != want[i] {
					t.Fatalf("%s=%d local=%d", col, got, want[i])
				}
			}
			if scalar(t, db, `SELECT count(*) FROM `+table.Name) != int64(len(rows)) {
				t.Fatal("dimension keys missing or duplicated")
			}
		})
	}
}

func TestStoppedSourceBoundsRetainFailedInputAndProgress(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	w := writer(t, "bounded", fc)
	write(t, w, ev("bounded", fc.Now(), "192.0.2.1", "oversize.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "bounded", fc)
	shipAll(t, s)
	// A single known source/hour exceeds the explicit 10,000-fact bound. Each
	// insert has the normal revision trigger; a later small hour must still run.
	pruneExec(t, db, `INSERT INTO query(ts,node,source,source_id,proto,client,domain,qtype,qclass,status,rcode,reply_type,dnssec,flags)
 SELECT q.ts,q.node,q.source,g+1,q.proto,q.client,q.domain,q.qtype,q.qclass,q.status,q.rcode,q.reply_type,q.dnssec,q.flags FROM query q CROSS JOIN generate_series(1,10000) g WHERE q.source_id=1`)
	hour := fc.Now().Unix() / 3600 * 3600
	pruneExec(t, db, `INSERT INTO query(ts,node,source,source_id,proto,client,domain,qtype,qclass,status,rcode,reply_type,dnssec,flags)
 SELECT $1,node,source,20001,proto,client,domain,qtype,qclass,status,rcode,reply_type,dnssec,flags FROM query WHERE source_id=1`, (hour+3601)*1000000)
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	fc.Step(3 * time.Hour)
	backfillPass(t, db, fc.Now())
	if scalar(t, db, `SELECT count(*) FROM rollup_bucket WHERE error_key='source_bucket_too_large'`) != 1 {
		t.Fatal("oversize failure hidden")
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h WHERE ts=$1`, hour+3600) != 1 {
		t.Fatal("failed first bucket starved later work")
	}
	// Even far beyond normal retention, failed raw input remains available.
	for _, job := range Jobs(time.Hour) {
		if job.Name == "querylog-raw-retention" || job.Name == "querylog-rollup-retention" {
			if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := job.Run(ctx, tx, fc.Now().Add(800*24*time.Hour)); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if scalar(t, db, `SELECT count(*) FROM query`) != 10002 {
		t.Fatal("retention removed failed input")
	}
}

func TestStoppedSourceFactsDisabledKeepsKnownDimensions(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	w := writer(t, "statistics-only", fc)
	write(t, w, ev("statistics-only", fc.Now(), "192.0.2.1", "hidden.example", pipeline.StatusBlockedList))
	s := shipper(t, db, w, "statistics-only", fc)
	s.Facts = false
	shipAll(t, s)
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	fc.Step(3 * time.Hour)
	for range 3 {
		backfillPass(t, db, fc.Now())
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 1 || scalar(t, db, `SELECT count(*) FROM query`) != 0 || scalar(t, db, `SELECT count(*) FROM rollup_domain_1h`) != 0 {
		t.Fatal("missing facts reconstructed or known stats lost")
	}
	if scalar(t, db, `SELECT count(*) FROM rollup_bucket WHERE error_key='facts_unavailable'`) != 1 {
		t.Fatal("unavailable dimensions hidden")
	}
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	shipAll(t, s)
	if scalar(t, db, `SELECT sum(total) FROM rollup_domain_1h`) != 1 || scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 1 {
		t.Fatal("official return did not replace partial contribution")
	}
}

func TestSourceExportStaleAndEmptyReplacement(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	w := writer(t, "empty", fc)
	write(t, w, ev("empty", fc.Now(), "192.0.2.1", "empty.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "empty", fc)
	shipAll(t, s)
	hour := fc.Now().Unix() / 3600 * 3600
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	fc.Step(3 * time.Hour)
	backfillPass(t, db, fc.Now())
	cur, err := central.Cursor(ctx, prunePool(t, db), s.source, streamHour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.shipRollupRange(ctx, 3600, streamHour, hour, hour+3600, cur+1); !errors.Is(err, errStaleRollup) {
		t.Fatal("stale export accepted", err)
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 1 {
		t.Fatal("stale export mutated contribution")
	}
	// Local hour has not been compacted, so its authoritative export is empty.
	// The whole contribution is replaced; no synthetic key survives.
	if _, _, err := s.shipRollupRange(ctx, 3600, streamHour, hour, hour+3600, cur); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db, `SELECT count(*) FROM rollup_1h`) != 0 || scalar(t, db, `SELECT count(*) FROM rollup_domain_1h`) != 0 {
		t.Fatal("empty authoritative replacement kept synthetic keys")
	}
}

func TestSourceLockKeepsOtherFileShipping(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	ctx := t.Context()
	wa := writer(t, "source-lock-a", fc)
	sa := shipper(t, db, wa, "source-lock-a", fc)
	write(t, wa, ev("source-lock-a", fc.Now(), "192.0.2.1", "a.example", pipeline.StatusForwarded))
	shipAll(t, sa)
	wb := writer(t, "source-lock-b", fc)
	sb := shipper(t, db, wb, "source-lock-b", fc)
	write(t, wb, ev("source-lock-b", fc.Now(), "192.0.2.2", "b.example", pipeline.StatusForwarded))
	shipAll(t, sb)
	// Each file's next collector ID is two (test factories are per-file).
	ea := ev("source-lock-a", fc.Now(), "192.0.2.3", "next-a.example", pipeline.StatusForwarded)
	ea.ID = 2
	eb := ev("source-lock-b", fc.Now(), "192.0.2.4", "next-b.example", pipeline.StatusForwarded)
	eb.ID = 2
	write(t, wa, ea)
	write(t, wb, eb)
	lock, err := prunePool(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(ctx, `SELECT id FROM source WHERE uuid=$1 FOR UPDATE`, sa.source); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { _, err := sa.shipFacts(ctx); blocked <- err }()
	if _, err := sb.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db, `SELECT count(*) FROM query WHERE source=(SELECT id FROM source WHERE uuid=$1)`, sb.source) != 2 {
		t.Fatal("other file did not commit")
	}
	select {
	case err := <-blocked:
		_ = err
		t.Fatal("same source passed lock")
	case <-time.After(25 * time.Millisecond):
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("same file did not resume")
	}
}

func TestBackfillReadSnapshot(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	w := writer(t, "snapshot", fc)
	write(t, w, ev("snapshot", fc.Now(), "192.0.2.1", "snapshot.example", pipeline.StatusForwarded))
	shipAll(t, shipper(t, db, w, "snapshot", fc))
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	sqlDB, err := db.SQL()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := sqlDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var before, after int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rollup_1h`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	backfillPass(t, db, fc.Now().Add(2*time.Hour))
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rollup_1h`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != 0 || after != 0 || scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 1 {
		t.Fatal("mixed read snapshots")
	}
}

func TestLateFactAfterRawRetentionPreservesBaselineExactlyOnce(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	w := writer(t, "retention-return", fc)
	write(t, w, ev("retention-return", fc.Now(), "192.0.2.1", "original.example", pipeline.StatusBlockedList))
	s := shipper(t, db, w, "retention-return", fc)
	shipAll(t, s)
	hour := fc.Now().Unix() / 3600 * 3600
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	fc.Step(48 * time.Hour)
	for range 4 {
		backfillPass(t, db, fc.Now())
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 1 || scalar(t, db, `SELECT sum(total) FROM rollup_1d`) != 1 {
		t.Fatal("baseline missing")
	}
	for _, job := range Jobs(time.Hour) {
		if job.Name == "querylog-raw-retention" {
			if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := job.Run(ctx, tx, fc.Now()); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if scalar(t, db, `SELECT count(*) FROM query`) != 0 || scalar(t, db, `SELECT count(*) FROM rollup_input_hour WHERE NOT input_complete`) != 1 {
		t.Fatal("raw expiration not recorded")
	}
	// Reuse the same shipper, including its cached partition name. A returning
	// fact must recreate the expired partition and add only IDs above the head.
	write(t, w, ev("retention-return", time.Unix(hour+5, 0), "192.0.2.2", "late.example", pipeline.StatusForwarded))
	if _, err := s.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now().Add(-3*time.Hour))
	for range 4 {
		backfillPass(t, db, fc.Now())
	}
	for _, table := range []string{"rollup_1h", "rollup_client_1h", "rollup_qtype_1h", "rollup_domain_1h", "rollup_1d", "rollup_client_1d", "rollup_domain_1d"} {
		if scalar(t, db, `SELECT sum(total) FROM `+table) != 2 {
			t.Fatalf("%s lost or duplicated original contribution", table)
		}
	}
	if scalar(t, db, `SELECT sum(blocked) FROM rollup_1h`) != 1 || scalar(t, db, `SELECT sum(hits) FROM rollup_list_1h`) != 1 {
		t.Fatal("retained dimensions overwritten")
	}
	if err := db.Tx(ctx, func(tx pgx.Tx) error { return summarizeHour(ctx, tx, 1, hour) }); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 2 {
		t.Fatal("delta replay double counted")
	}
	// Once the hourly baseline itself expires, surviving tail input cannot
	// recreate it. Keep the existing daily value and visibly retain the tail.
	for _, job := range Jobs(0) {
		if job.Name == "querylog-rollup-retention" {
			if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := job.Run(ctx, tx, fc.Now().Add(500*24*time.Hour)); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if scalar(t, db, `SELECT count(*) FROM rollup_bucket WHERE resolution=3600 AND NOT retained`) != 1 {
		t.Fatal("hour expiration not recorded")
	}
	write(t, w, ev("retention-return", time.Unix(hour+6, 0), "192.0.2.3", "later.example", pipeline.StatusForwarded))
	if _, err := s.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now().Add(-3*time.Hour))
	for range 4 {
		backfillPass(t, db, fc.Now())
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1d`) != 2 || scalar(t, db, `SELECT count(*) FROM rollup_bucket WHERE error_key='source_input_incomplete'`) != 1 {
		t.Fatal("absent hourly baseline guessed or hidden")
	}
}
