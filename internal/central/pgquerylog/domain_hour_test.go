package pgquerylog

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	testclock "k8s.io/utils/clock/testing"
)

func TestDomainHourSourceOwnershipEdgesAndPurge(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(30 * time.Second))
	for _, node := range []string{"same", "same", "other"} {
		w := writer(t, node, fc)
		write(t, w, ev(node, t0.Add(-time.Second), "10.0.0.1", "shared.example", pipeline.StatusForwarded), ev(node, t0.Add(time.Second), "10.0.0.1", "shared.example", pipeline.StatusForwarded), ev(node, t0.Add(29*time.Second), "10.0.0.1", "blocked.example", pipeline.StatusBlockedList), ev(node, t0.Add(30*time.Second), "10.0.0.1", "shared.example", pipeline.StatusForwarded), ev(node, t0.Add(time.Hour), "10.0.0.1", node+"-future.example", pipeline.StatusForwarded))
		s := shipper(t, db, w, node, fc)
		shipAll(t, s)
		shipAll(t, s)
	}
	if got := scalar(t, db, `SELECT sum(total) FROM query_domain_hour`); got != 15 {
		t.Fatalf("index/replay=%d", got)
	}
	if got := scalar(t, db, `SELECT count(DISTINCT source) FROM query_domain_hour WHERE node=(SELECT id FROM dict_node WHERE name='same')`); got != 2 {
		t.Fatalf("source owners=%d", got)
	}
	for _, tc := range []struct {
		node           string
		total, blocked int64
	}{{"same", 2, 2}, {"other", 1, 1}, {"", 3, 3}} {
		r := centralReader(db, fc)
		if tc.node != "" {
			r = r.ForNode(tc.node)
		}
		win := sqlitelog.Window{From: t0, Until: fc.Now()}
		top, err := r.TopDomains(t.Context(), sqlitelog.TopQuery{Window: win})
		if err != nil || len(top.Items) != 1 || top.Items[0].Count != tc.total || top.Approximate {
			t.Fatalf("node=%s top=%+v err=%v", tc.node, top, err)
		}
		top, err = r.TopDomains(t.Context(), sqlitelog.TopQuery{Window: win, Blocked: true})
		if err != nil || len(top.Items) != 1 || top.Items[0].Count != tc.blocked {
			t.Fatalf("blocked node=%s top=%+v err=%v", tc.node, top, err)
		}
		sum, err := r.Summary(t.Context(), sqlitelog.SummaryQuery{Window: win})
		if err != nil || sum.Queries.UniqueDomains != 2 || sum.Approximate {
			t.Fatalf("summary node=%s %+v: %v", tc.node, sum, err)
		}
	}
	// Fact transaction rollback cannot leave an index delta. Reusing an
	// existing row's mandatory fields avoids changing input identity.
	rollback := errors.New("rollback")
	err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO query(ts,node,source,source_id,proto,client,domain,qtype,qclass,status,rcode,reply_type,dnssec,flags) SELECT ts,node,source,9999,proto,client,domain,qtype,qclass,status,rcode,reply_type,dnssec,flags FROM query LIMIT 1`); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || scalar(t, db, `SELECT sum(total) FROM query_domain_hour`) != 15 {
		t.Fatal("rollback changed domain index", err)
	}
	if _, err := (Purger{DB: db}).Purge(t.Context(), t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, `SELECT sum(total) FROM query_domain_hour`); got != 9 {
		t.Fatalf("partial purge=%d", got)
	}
	// Whole-day TRUNCATE needs explicit index cleanup; row triggers cannot
	// observe it. The production purge must remove all cells atomically.
	if _, err := (Purger{DB: db}).Purge(t.Context(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, `SELECT count(*) FROM query_domain_hour`); got != 0 {
		t.Fatalf("whole-day purge retains %d cells", got)
	}
}

func TestDomainHourPartitionRetentionAndLateInput(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-48 * time.Hour))
	w := writer(t, "old", fc)
	write(t, w, ev("old", fc.Now().Add(time.Second), "10.0.0.1", "old.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "old", fc)
	shipAll(t, s)
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now())
	fc.SetTime(t0.Add(72 * time.Hour))
	for range 4 {
		backfillPass(t, db, fc.Now())
	}
	for _, job := range Jobs(24 * time.Hour) {
		if job.Name == "querylog-raw-retention" {
			if err := db.Tx(t.Context(), func(tx pgx.Tx) error { _, err := job.Run(t.Context(), tx, fc.Now()); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if scalar(t, db, `SELECT count(*) FROM query`) != 0 || scalar(t, db, `SELECT count(*) FROM query_domain_hour`) != 0 {
		t.Fatal("partition retained index rows")
	}
	// Late original facts recreate a source-owned cell; the F71 retention
	// input gate remains visible and is not replaced by index completeness.
	write(t, w, ev("old", t0.Add(-48*time.Hour+2*time.Second), "10.0.0.1", "late.example", pipeline.StatusForwarded))
	shipAll(t, s)
	if scalar(t, db, `SELECT sum(total) FROM query_domain_hour`) != 1 {
		t.Fatal("late input missing")
	}
	if scalar(t, db, `SELECT count(*) FROM rollup_input_hour WHERE NOT input_complete`) != 1 {
		t.Fatal("late index erased input gate")
	}
}

// The same original 1M/252k acceptance scale is exercised centrally without
// asking a shipper to materialize a million Go objects. Source/input triggers
// and the production central reader remain enabled throughout ingest.
func TestDomainHourMillionCentral(t *testing.T) {
	if os.Getenv("KUDURRU_MEASURE") != "1" {
		t.Skip("set KUDURRU_MEASURE=1 for central 1M-row acceptance")
	}
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(501 * time.Second))
	if err := EnsurePartition(t.Context(), db, floorDay(t0.UnixMicro())); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		if err := central.NoStatementTimeout(t.Context(), tx); err != nil {
			return err
		}
		if _, err := central.EnsureSource(t.Context(), tx, "million", "querylog", "million", ""); err != nil {
			return err
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO dict_node(name) VALUES('million'); INSERT INTO dict_client(ip) VALUES('10.0.0.1'); INSERT INTO dict_domain(name) SELECT 'd'||i||'.example' FROM generate_series(0,251999) i`); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < 1000000; offset += 1000 {
		if err := db.Tx(t.Context(), func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), `INSERT INTO query(ts,node,source,source_id,proto,client,domain,qtype,status,rcode,reply_type)
             SELECT $1+i*500,1,1,i+1,1,1,(CASE WHEN i>=252000 AND i%20=0 THEN 0 ELSE i%252000 END)+1,1,1,0,1 FROM generate_series($2::bigint,$3::bigint) i`, t0.UnixMicro(), int64(offset), int64(offset+999))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO rollup_1m(ts,node,source,total,blocked,cached,forwarded,authoritative,dropped) SELECT (ts/60000000)*60,node,source,count(*),0,0,count(*),0,0 FROM query GROUP BY 1,2,3`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ingest := time.Since(start)
	r := centralReader(db, fc).ForNode("million")
	win := sqlitelog.Window{From: t0, Until: fc.Now()}
	start = time.Now()
	sum, err := r.Summary(t.Context(), sqlitelog.SummaryQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	summaryTime := time.Since(start)
	if sum.Queries.Total != 1000000 || sum.Queries.UniqueDomains != 252000 || sum.Approximate {
		t.Fatalf("central million exactness total=%d unique=%d approximate=%v", sum.Queries.Total, sum.Queries.UniqueDomains, sum.Approximate)
	}
	start = time.Now()
	top, err := r.TopDomains(t.Context(), sqlitelog.TopQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	topTime := time.Since(start)
	if len(top.Items) != 10 || top.Items[0].Count < 37000 || top.Approximate {
		t.Fatal("central million ranking")
	}
	t.Logf("F16 central 1M/252k ingest=%s rowsPerSecond=%.0f summary=%s top=%s indexBytes=%d", ingest, 1000000/ingest.Seconds(), summaryTime, topTime, scalar(t, db, `SELECT pg_total_relation_size('query_domain_hour')`))
}
