package central_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
)

func TestDomainHourMigrationPreservesInputGates(t *testing.T) {
	p := pool(t, centraltest.DSN(t))
	ctx := t.Context()
	if _, err := p.Exec(ctx, `CREATE TABLE kudurru_meta(key text PRIMARY KEY,value text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"0001_init.sql", "0002_audit.sql", "0003_querylog.sql", "0004_dictionary_retention.sql", "0005_source_rollups.sql"} {
		//nolint:gosec // fixed repository migration filenames
		b, err := os.ReadFile("migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Exec(ctx, `INSERT INTO kudurru_meta VALUES('schema_version','5'),('compat_version','5');
 INSERT INTO source(uuid,kind,node,version) VALUES('retained','querylog','old','old');
 INSERT INTO dict_node(name) VALUES('old'); INSERT INTO dict_client(ip) VALUES('10.0.0.1'); INSERT INTO dict_domain(name) VALUES('old.example');
 CREATE TABLE query_old PARTITION OF query FOR VALUES FROM(0) TO(86400000000);
 INSERT INTO query(ts,node,source,source_id,proto,client,domain,qtype,status,rcode,reply_type) VALUES(1000000,1,1,1,1,1,1,1,1,0,1),(2000000,1,1,2,1,1,1,1,16,0,1);
 UPDATE rollup_input_hour SET input_complete=false;
 INSERT INTO rollup_1h(ts,node,total,blocked,cached,forwarded,authoritative,dropped) VALUES(0,1,7,0,0,7,0,0)`); err != nil {
		t.Fatal(err)
	}

	old, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	if _, err := central.Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}

	// The connection predates the migration and deliberately bypasses its
	// startup compatibility check. Native pre-v6 destructive statements
	// must refuse instead of leaving counts for facts that no longer exist.
	for _, q := range []string{`TRUNCATE query_old`, `DROP TABLE query_old`} {
		if _, err := old.Exec(ctx, q); err == nil {
			t.Fatalf("old connected destructive statement accepted: %s", q)
		}
	}
	if _, err := p.Exec(ctx, `CREATE TABLE query_future PARTITION OF query FOR VALUES FROM(86400000000) TO(172800000000)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`TRUNCATE query_future`, `DROP TABLE query_future`} {
		if _, err := old.Exec(ctx, q); err == nil {
			t.Fatalf("future partition unprotected: %s", q)
		}
	}
	var total, blocked, first, last, opaque int64
	var complete bool
	if err := p.QueryRow(ctx, `SELECT total,blocked,first_ts,last_ts,(SELECT input_complete FROM rollup_input_hour),(SELECT total FROM rollup_1h WHERE source IS NULL) FROM query_domain_hour`).Scan(&total, &blocked, &first, &last, &complete, &opaque); err != nil {
		t.Fatal(err)
	}
	if total != 2 || blocked != 1 || first != 1000000 || last != 2000000 || complete || opaque != 7 {
		t.Fatal("migration changed inputs/ownership/gates")
	}
	if _, err := p.Exec(ctx, `DELETE FROM query WHERE ts=1000000`); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := p.QueryRow(ctx, `SELECT total FROM query_domain_hour`).Scan(&n); err != nil || n != 1 {
		t.Fatal("delete", n, err)
	}
	// The migration requires the new janitor/purge implementation; old readers
	// must refuse this schema rather than leave a stale retained-fact index.
	var compat string
	if err := p.QueryRow(ctx, `SELECT value FROM kudurru_meta WHERE key='compat_version'`).Scan(&compat); err != nil || compat != "6" {
		t.Fatal("compatibility fence", err)
	}
}

func TestDomainHourUpgradeExcludesWritesBeforeSeed(t *testing.T) {
	p := pool(t, centraltest.DSN(t))
	ctx := t.Context()
	if _, err := p.Exec(ctx, `CREATE TABLE kudurru_meta(key text PRIMARY KEY,value text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"0001_init.sql", "0002_audit.sql", "0003_querylog.sql", "0004_dictionary_retention.sql", "0005_source_rollups.sql"} {
		//nolint:gosec // fixed repository migration filenames
		b, err := os.ReadFile("migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Exec(ctx, `INSERT INTO source(uuid,kind,node,version) VALUES('upgrade','querylog','old','old');
 INSERT INTO dict_node(name) VALUES('old'); INSERT INTO dict_client(ip) VALUES('10.0.0.1'); INSERT INTO dict_domain(name) VALUES('old.example');
 CREATE TABLE query_old PARTITION OF query FOR VALUES FROM(0) TO(86400000000)`); err != nil {
		t.Fatal(err)
	}
	// Hold the real migration just before its seed. An already-connected
	// writer must wait here, not commit an unindexed fact in the install gap.
	b, err := os.ReadFile("migrations/0006_domain_hour.sql")
	if err != nil {
		t.Fatal(err)
	}
	prefix, suffix, ok := strings.Cut(string(b), "INSERT INTO query_domain_hour")
	if !ok {
		t.Fatal("no seed")
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, prefix); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO query(ts,node,source,source_id,proto,client,domain,qtype,status,rcode,reply_type) VALUES(1000000,1,1,1,1,1,1,1,1,0,1)`
	blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = p.Exec(blocked, insert)
	cancel()
	if err == nil {
		t.Fatal("fact committed between seed and trigger installation")
	}
	if _, err := tx.Exec(ctx, "INSERT INTO query_domain_hour"+suffix); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, insert); err != nil {
		t.Fatal(err)
	}
	var facts, indexed int64
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM query),(SELECT sum(total) FROM query_domain_hour)`).Scan(&facts, &indexed); err != nil || facts != 1 || indexed != facts {
		t.Fatalf("connected writer after upgrade: facts=%d index=%d err=%v", facts, indexed, err)
	}
}
