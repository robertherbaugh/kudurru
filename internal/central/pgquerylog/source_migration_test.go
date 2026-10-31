package pgquerylog

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	testclock "k8s.io/utils/clock/testing"
)

func TestSourceMigrationPreservesOpaqueHistoryAndRefusesOldWriter(t *testing.T) {
	ctx := t.Context()
	dsn := centraltest.DSN(t)
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Exec(ctx, `CREATE TABLE kudurru_meta(key text PRIMARY KEY,value text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"0001_init.sql", "0002_audit.sql", "0003_querylog.sql", "0004_dictionary_retention.sql"} {
		//nolint:gosec // fixed repository migration names above
		b, err := os.ReadFile("../migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	hour := t0.Add(-time.Hour).Unix()
	if _, err := p.Exec(ctx, `INSERT INTO kudurru_meta VALUES('schema_version','4'),('compat_version','4');
 INSERT INTO source(uuid,kind,node,version) VALUES('legacy-a','querylog','legacy-node','old'),('legacy-b','querylog','legacy-node','old');
 INSERT INTO dict_node(name) VALUES('legacy-node');
 INSERT INTO dict_client(ip) VALUES('192.0.2.1'); INSERT INTO dict_domain(name) VALUES('legacy.example')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO rollup_1h(ts,node,total,blocked,cached,forwarded,authoritative,dropped) VALUES($1,1,5,0,0,5,0,0)`, hour); err != nil {
		t.Fatal(err)
	}
	// Keep an already-connected old writer, rather than merely testing startup.
	old, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	if _, err := central.Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(ctx, `INSERT INTO rollup_1h(ts,node,total,blocked,cached,forwarded,authoritative,dropped) VALUES($1,1,7,0,0,7,0,0) ON CONFLICT(ts,node) DO UPDATE SET total=excluded.total`, hour); err == nil {
		t.Fatal("connected schema-4 writer accepted")
	}
	db := centraltest.Open(t, dsn)
	if scalar(t, db, `SELECT total FROM rollup_1h WHERE source IS NULL`) != 5 {
		t.Fatal("migration changed original value")
	}
	fc := testclock.NewFakeClock(time.Now().UTC().Add(-30 * time.Minute))
	// A new file with the same name must inherit the upgrade gate too.
	w := writer(t, "legacy-node", fc)
	write(t, w, ev("legacy-node", time.Unix(hour+1, 0), "192.0.2.1", "legacy.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "legacy-node", fc)
	shipAll(t, s)
	if scalar(t, db, `SELECT count(*) FROM rollup_source_state WHERE legacy_before>0`) != 3 {
		t.Fatal("new file escaped opaque history gate")
	}
	fc.SetTime(time.Now().UTC().Add(3 * time.Hour))
	pruneExec(t, db, `UPDATE source SET last_seen=$1`, fc.Now().Add(-3*time.Hour))
	for range 4 {
		backfillPass(t, db, fc.Now())
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 5 {
		t.Fatal("overlap changed original aggregate")
	}
	rec := Recovery{DB: db, Now: fc.Now}
	e, err := rec.Evidence(ctx, s.source, "legacy-node", hour)
	if err != nil || e.BlockedReason != sqlitelog.ErrRollupLegacy.Error() {
		t.Fatalf("missing visible gate: %v", err)
	}
	if _, err := rec.Repair(ctx, *e, func(context.Context) error { return nil }); !errors.Is(err, sqlitelog.ErrRollupLegacy) {
		t.Fatal("opaque provenance guessed", err)
	}
	r := sqlitelog.NewCentralReader(db.SQL, sqlitelog.CentralOptions{Now: fc.Now}).ForNode("legacy-node")
	status, err := r.Status(ctx)
	if err != nil || !status.Partial || status.Backfill.LegacyCells != 1 || len(status.Backfill.Details) == 0 {
		t.Fatalf("invisible upgrade gate: %+v: %v", status, err)
	}
	// Retention and dictionary pruning preserve the original cells and their inputs.
	for _, job := range Jobs(time.Hour) {
		if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := job.Run(ctx, tx, fc.Now().Add(800*24*time.Hour)); return err }); err != nil {
			t.Fatal(err)
		}
	}
	if scalar(t, db, `SELECT total FROM rollup_1h WHERE source IS NULL`) != 5 || scalar(t, db, `SELECT count(*) FROM query`) != 1 {
		t.Fatal("retention removed gated evidence")
	}
}
