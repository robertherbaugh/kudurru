package pgquerylog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	testclock "k8s.io/utils/clock/testing"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

func prunePool(t *testing.T, db *central.DB) *pgxpool.Pool {
	t.Helper()
	p, err := db.Pool()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pruneExec(t *testing.T, db *central.DB, sql string, args ...any) {
	t.Helper()
	if _, err := prunePool(t, db).Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func prunePass(t *testing.T, db *central.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		n, err = pruneDictionaries(context.Background(), tx, t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every individual referencing column must protect its own distinct ID, with
// no incidental reference from another row. The FK inventory independently
// guards against omission when a referencing table is added in a migration.
func TestDictionaryPruneEveryReference(t *testing.T) {
	db := centraltest.New(t)
	p := prunePool(t, db)
	ctx := context.Background()
	if err := EnsurePartition(ctx, db, floorDay(t0.UnixMicro())); err != nil {
		t.Fatal(err)
	}
	kept := map[string][]int64{}
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		d := newDicts()
		serial := 0
		intern := func(table, label string) (int64, error) {
			seen := int64(0)
			if label == "recent-orphan" {
				seen = t0.Unix()
			}
			if table == "dict_client" {
				key := clientKey(label, nil)
				out, err := d.clients(ctx, tx, map[string]client{key: {ip: label, lastSeenAt: seen}}, t0.Unix())
				return out[key], err
			}
			col := "name"
			if table == "dict_upstream" {
				col = "addr"
			}
			out, err := d.names(ctx, tx, table, col, map[string]bool{label: true}, nil, seen)
			return out[label], err
		}
		node, err := intern("dict_node", "support-node")
		if err != nil {
			return err
		}
		domain, err := intern("dict_domain", "support.example")
		if err != nil {
			return err
		}
		cl, err := intern("dict_client", "192.0.2.1")
		if err != nil {
			return err
		}
		// A separate source's references must protect shared dictionary entries.
		sid, err := central.EnsureSource(ctx, tx, "other-source", "querylog", "other-node", "")
		if err != nil {
			return err
		}
		for _, spec := range dictionarySpecs() {
			var fkCount int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype='f' AND conparentid=0 AND connamespace=current_schema()::regnamespace AND confrelid=$1::regclass`, spec.table).Scan(&fkCount); err != nil {
				return err
			}
			if fkCount != len(spec.references) {
				return fmt.Errorf("%s FK count=%d references=%d", spec.table, fkCount, len(spec.references))
			}
			for _, ref := range spec.references {
				serial++
				label := fmt.Sprintf("retained-%s-%s-%s", spec.table, ref.table, ref.column)
				id, err := intern(spec.table, label)
				if err != nil {
					return err
				}
				kept[spec.table] = append(kept[spec.table], id)
				cols := []string{"ts", "node"}
				values := []any{int64(serial), node}
				switch ref.table {
				case "query":
					cols = []string{"ts", "node", "source", "source_id", "proto", "client", "domain", "qtype", "status", "rcode", "reply_type"}
					values = []any{t0.UnixMicro() + int64(serial), node, sid, int64(serial), 1, cl, domain, 1, 1, 0, 1}
				case "query_domain_hour":
					cols = []string{"ts", "node", "source", "domain", "total", "blocked", "first_ts", "last_ts"}
					values = []any{int64(serial), node, sid, domain, int64(1), int64(0), t0.UnixMicro(), t0.UnixMicro()}
				default:
					for _, rt := range sqlitelog.RollupTables {
						if rt.Name != ref.table {
							continue
						}
						// Ancient daily, current smaller grains and future facts all protect.
						values[0] = t0.Unix() / rt.Res * rt.Res
						if rt.Res == 86400 {
							values[0] = int64(0)
						}
						if rt.Key != "" {
							cols = append(cols, rt.Key)
							if rt.Key == "qtype" {
								values = append(values, 1)
							} else {
								support, err := intern("dict_"+rt.Key, "support-"+rt.Key)
								if err != nil {
									return err
								}
								values = append(values, support)
							}
						}
						for _, c := range rt.Cols {
							cols = append(cols, c)
							values = append(values, int64(1))
						}
					}
				}
				found := false
				for i, c := range cols {
					if c == ref.column {
						values[i] = id
						found = true
					}
				}
				if !found {
					cols = append(cols, ref.column)
					values = append(values, id)
				}
				quoted := make([]string, len(cols))
				params := make([]string, len(cols))
				for i, c := range cols {
					quoted[i] = pgx.Identifier{c}.Sanitize()
					params[i] = fmt.Sprintf("$%d", i+1)
				}
				_, err = tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{ref.table}.Sanitize()+` (`+strings.Join(quoted, ",")+`) VALUES (`+strings.Join(params, ",")+`)`, values...)
				if err != nil {
					return err
				}
			}
			// Recent unreferenced entries are eligible; first/last_seen are irrelevant.
			if _, err := intern(spec.table, "recent-orphan"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := prunePass(t, db); n != 7 {
		t.Fatalf("removed=%d want seven recent orphans", n)
	}
	for table, ids := range kept {
		var n int
		if err := p.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE id=ANY($1::bigint[])`, ids).Scan(&n); err != nil || n != len(ids) {
			t.Fatalf("%s retained=%d want=%d: %v", table, n, len(ids), err)
		}
	}
	if n := prunePass(t, db); n != 0 {
		t.Fatalf("repeat removed=%d", n)
	}
}

func TestDictionaryPruneBoundedSweepAndRestart(t *testing.T) {
	dsn := centraltest.DSN(t)
	db := centraltest.Open(t, dsn)
	ctx := context.Background()
	pruneExec(t, db, `INSERT INTO dict_domain(name) SELECT 'orphan-'||n FROM generate_series(1,800) n`)
	locked, err := prunePool(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Rollback(ctx) }()
	if _, err := locked.Exec(ctx, `SELECT id FROM dict_domain WHERE id=1 FOR KEY SHARE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if n := prunePass(t, db); n != dictionaryPruneBatch {
		t.Fatalf("first batch=%d", n)
	}
	t.Logf("one bounded seven-dictionary pass: %s (256 removed)", time.Since(start))
	if n := scalar(t, db, `SELECT count(*) FROM dict_domain`); n != 544 {
		t.Fatalf("batch cap left=%d", n)
	}
	// Restart uses durable sweep progress. New high IDs must not postpone wrap.
	db2 := centraltest.Open(t, dsn)
	if err := locked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	pruneExec(t, db, `INSERT INTO dict_domain(name) SELECT 'new-'||n FROM generate_series(1,300) n`)
	for range 4 {
		prunePass(t, db2)
	}
	// The original upper ID was 800. A new sweep revisits skipped ID 1 before
	// the new high IDs, even though ingestion extended the identity sequence.
	if n := scalar(t, db2, `SELECT count(*) FROM dict_domain WHERE id=1`); n != 0 {
		t.Fatal("locked low ID starved after restart/wrap")
	}
	for range 8 {
		prunePass(t, db2)
	}
	if n := scalar(t, db2, `SELECT count(*) FROM dict_domain`); n != 0 {
		t.Fatalf("sweep did not finish: %d", n)
	}
	if n := prunePass(t, db2); n != 0 {
		t.Fatalf("empty repeat=%d", n)
	}
}

func TestDictionaryPruneConcurrentInternAndCachedShipping(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "ship-node", fc)
	write(t, w, ev("ship-node", t0.Add(-time.Minute), "192.0.2.1", "held.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "ship-node", fc)
	exported, err := w.Export(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ensurePartitions(ctx, exported); err != nil {
		t.Fatal(err)
	}
	// Prime committed cache entries without a fact yet, then resolve them in
	// a held shipping transaction. Prune skips the cache-hit KEY SHARE locks.
	if err := s.tx(ctx, func(tx pgx.Tx, _ int32) error {
		_, err := s.dicts.resolveFacts(ctx, tx, exported, t0.Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := prunePool(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := s.dicts.resolveFacts(ctx, tx, exported, t0.Unix()); err != nil {
		t.Fatal(err)
	}
	if n := prunePass(t, db); n != 0 {
		t.Fatalf("pruned in-flight dictionary IDs: %d", n)
	}
	var sid int32
	if err := tx.QueryRow(ctx, `SELECT id FROM source WHERE uuid=$1`, s.source).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if err := s.insertFacts(ctx, tx, sid, exported); err != nil {
		t.Fatal(err)
	}
	if err := central.SetCursor(ctx, tx, sid, streamFacts, exported[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	prunePass(t, db)
	if n := scalar(t, db, `SELECT count(*) FROM query q JOIN dict_domain d ON d.id=q.domain WHERE d.name='held.example'`); n != 1 {
		t.Fatalf("held shipping lost data=%d", n)
	}
	old := scalar(t, db, `SELECT id FROM dict_domain WHERE name='held.example'`)
	// Retention removes the sole reference; cache still contains all old IDs.
	pruneExec(t, db, `DELETE FROM query`)
	for range 2 {
		prunePass(t, db)
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_domain WHERE id=$1`, old); n != 0 {
		t.Fatal("unreferenced cached dictionary survived")
	}
	// Already-running pre-v4 shipper is refused by the FK; its cursor rolls
	// back with the attempted fact, even if advanced before the INSERT.
	err = s.tx(ctx, func(tx pgx.Tx, sid int32) error {
		if err := central.SetCursor(ctx, tx, sid, streamFacts, 999); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO rollup_domain_1d(ts,node,domain,total) VALUES(0,1,$1,1)`, old)
		return err
	})
	if err == nil {
		t.Fatal("legacy dangling cached-ID insert accepted")
	}
	if cur, err := central.Cursor(ctx, prunePool(t, db), s.source, streamFacts); err != nil || cur != exported[0].ID {
		t.Fatalf("failed batch cursor=%d: %v", cur, err)
	}
	// New actual local row reuses the strings; the same live cache must re-intern.
	write(t, w, ev("ship-node", t0, "192.0.2.1", "held.example", pipeline.StatusForwarded))
	if _, err := s.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT count(*) FROM query q JOIN dict_domain d ON d.id=q.domain WHERE d.name='held.example' AND d.id<>$1`, old); n != 1 {
		t.Fatalf("cached re-interned facts=%d", n)
	}
	// Retry after unknown success and a fresh process are both watermark noops.
	if _, err := s.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := shipper(t, db, w, "ship-node", fc)
	if _, err := restarted.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT count(*) FROM query`); n != 1 {
		t.Fatalf("retry duplicated facts=%d", n)
	}
}

func TestDictionaryPruneRolledBackCacheAndClientIdentity(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	d := newDicts()
	empty := ""
	entries := map[string]client{clientKey("192.0.2.1", nil): {ip: "192.0.2.1"}, clientKey("192.0.2.1", &empty): {ip: "192.0.2.1", name: &empty}}
	sentinel := errors.New("abort after interning")
	resolve := func(tx pgx.Tx) error {
		if _, err := d.names(ctx, tx, "dict_domain", "name", map[string]bool{"rollback.example": true}, nil, 0); err != nil {
			return err
		}
		_, err := d.clients(ctx, tx, entries, t0.Unix())
		return err
	}
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		if err := resolve(tx); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, resolve); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_client`); n != 2 {
		t.Fatalf("NULL and empty name merged: %d", n)
	}
	if n := prunePass(t, db); n != 3 {
		t.Fatalf("orphan removal=%d", n)
	}
	if err := db.Tx(ctx, resolve); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_client`); n != 2 {
		t.Fatalf("reintern changed NULL/empty identity: %d", n)
	}
	// A corrupt/mismatched hint must never turn one string into another.
	d.byTable["dict_domain"]["different.example"] = d.byTable["dict_domain"]["rollback.example"]
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := d.names(ctx, tx, "dict_domain", "name", map[string]bool{"different.example": true}, nil, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_domain`); n != 2 {
		t.Fatalf("cache changed string identity: %d", n)
	}
}

func TestDictionaryPruneRollbackAndJanitorFailure(t *testing.T) {
	db := centraltest.New(t)
	pruneExec(t, db, `INSERT INTO dict_node(name) VALUES('orphan'); INSERT INTO dict_client(ip) VALUES('192.0.2.1');
 CREATE FUNCTION reject_prune() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test refusal'; END $$;
 CREATE TRIGGER reject_prune BEFORE DELETE ON dict_client FOR EACH ROW EXECUTE FUNCTION reject_prune()`)
	ctx := context.Background()
	var removed int64
	err := db.Tx(ctx, func(tx pgx.Tx) error { var err error; removed, err = pruneDictionaries(ctx, tx, t0); return err })
	if err == nil || removed != 0 {
		t.Fatalf("failed sweep reports removed=%d err=%v", removed, err)
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_node`); n != 1 {
		t.Fatal("earlier dictionary deletion escaped rollback")
	}
	if n := scalar(t, db, `SELECT count(*) FROM kudurru_meta WHERE key LIKE 'prune_dict_%' AND value<>'{"after":0,"through":0}'`); n != 0 {
		t.Fatal("failed sweep advanced durable cursor")
	}
	pruneExec(t, db, `DROP TRIGGER reject_prune ON dict_client`)
	if n := prunePass(t, db); n != 2 {
		t.Fatalf("retry after rollback=%d", n)
	}
	// A positive tentative count followed by cancellation/rollback must not
	// produce a healthy housekeeping log. Test the actual scheduler branch.
	var logs bytes.Buffer
	run, cancel := context.WithCancel(ctx)
	j := central.Janitor{DB: db, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Jobs: []central.Job{{Name: "rollback-probe", Run: func(context.Context, pgx.Tx, time.Time) (int64, error) { cancel(); return 7, context.Canceled }}}}
	if err := j.Run(run); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "rows=7") {
		t.Fatal("janitor logged rolled-back rows as healthy")
	}
}

func TestDictionaryPruneRetainsReaderSnapshot(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "snapshot-node", fc)
	write(t, w, ev("snapshot-node", t0, "192.0.2.1", "snapshot.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "snapshot-node", fc)
	if _, err := s.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	p := prunePool(t, db)
	read, err := p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = read.Rollback(ctx) }()
	query := `SELECT count(*) FROM query q JOIN dict_domain d ON d.id=q.domain WHERE d.name='snapshot.example'`
	var count int
	if err := read.QueryRow(ctx, query).Scan(&count); err != nil || count != 1 {
		t.Fatalf("original snapshot=%d: %v", count, err)
	}
	pruneExec(t, db, `DELETE FROM query`)
	prunePass(t, db)
	if n := scalar(t, db, `SELECT count(*) FROM dict_domain`); n != 0 {
		t.Fatal("fresh reader still sees removed dictionary")
	}
	if err := read.QueryRow(ctx, query).Scan(&count); err != nil || count != 1 {
		t.Fatalf("held snapshot lost joined dictionary=%d: %v", count, err)
	}
	if err := read.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Exercise the production COPY+watermark path while independent PostgreSQL
// transactions sweep dictionaries. Facts arrive once despite concurrent
// interning, stale hints and successive retention rounds.
func TestDictionaryPruneWhileActualShipperRuns(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "concurrent-node", fc)
	for i := range 24 {
		write(t, w, ev("concurrent-node", t0.Add(time.Duration(i)*time.Microsecond), "192.0.2.1", fmt.Sprintf("concurrent-%d.example", i%4), pipeline.StatusForwarded))
	}
	s := &Shipper{DB: db, Local: w, Node: "concurrent-node", Facts: true, Batch: 1, Interval: time.Millisecond, Now: fc.Now, Logger: quiet()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 24 {
		if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := pruneDictionaries(ctx, tx, t0); return err }); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.ShippedThrough() != 24 {
		if time.Now().After(deadline) {
			t.Fatalf("actual background shipper stopped at %d", s.ShippedThrough())
		}
		time.Sleep(time.Millisecond)
	}
	if n := scalar(t, db, `SELECT count(*) FROM query q JOIN dict_domain d ON q.domain=d.id JOIN dict_client c ON q.client=c.id JOIN dict_node n ON q.node=n.id`); n != 24 {
		t.Fatalf("concurrent shipped facts=%d", n)
	}
	source, err := w.SourceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := central.Cursor(ctx, prunePool(t, db), source, streamFacts); err != nil || cur != 24 {
		t.Fatalf("concurrent watermark=%d: %v", cur, err)
	}
}

func TestDictionaryPruneAfterRollupRetentionBoundaries(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		sid, err := central.EnsureSource(ctx, tx, "retention-owned", "querylog", "boundary", "test")
		if err != nil {
			return err
		}
		d := newDicts()
		node, err := d.names(ctx, tx, "dict_node", "name", map[string]bool{"expired": true, "boundary": true, "daily": true}, nil, 0)
		if err != nil {
			return err
		}
		for _, rt := range sqlitelog.RollupTables {
			if rt.Key != "" {
				continue
			}
			cutoff := t0.Unix()
			switch rt.Res {
			case 60:
				cutoff -= int64(sqlitelog.MinuteRetention / time.Second)
			case 3600:
				cutoff -= int64(sqlitelog.HourRetention / time.Second)
			default:
				cutoff = 0
			}
			for _, label := range []string{"expired", "boundary"} {
				ts := cutoff
				if label == "expired" {
					ts--
				}
				n := node[label]
				if rt.Res == 86400 {
					n = node["daily"]
				}
				_, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{rt.Name}.Sanitize()+`(ts,node,source,total,blocked,cached,forwarded,authoritative,dropped) VALUES($1,$2,$3,1,0,0,1,0,0)`, ts, n, sid)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range Jobs(0) {
		if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := job.Run(ctx, tx, t0); return err }); err != nil {
			t.Fatalf("%s: %v", job.Name, err)
		}
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_node WHERE name='expired'`); n != 0 {
		t.Fatal("expired-only reference retained dictionary")
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_node WHERE name IN ('boundary','daily')`); n != 2 {
		t.Fatal("boundary or ancient daily reference lost")
	}
}

func TestDictionaryPruneLockTimeoutRollsBack(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	pruneExec(t, db, `INSERT INTO dict_node(name) VALUES('blocked-orphan')`)
	blocked, err := prunePool(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocked.Rollback(ctx) }()
	if _, err := blocked.Exec(ctx, `LOCK TABLE query IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var n int64
	err = db.Tx(ctx, func(tx pgx.Tx) error { var err error; n, err = pruneDictionaries(ctx, tx, t0); return err })
	if err == nil || n != 0 {
		t.Fatalf("blocked pass removed=%d err=%v", n, err)
	}
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "55P03" {
		t.Fatalf("expected configured lock timeout, got %v", err)
	}
	if count := scalar(t, db, `SELECT count(*) FROM dict_node`); count != 1 {
		t.Fatal("blocked deletion escaped rollback")
	}
	if err := blocked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if count := prunePass(t, db); count != 1 {
		t.Fatalf("post-contention sweep=%d", count)
	}
}

// Job.Run may succeed yet COMMIT fail. The scheduler must report the failed
// transaction, never the tentative removal count as healthy work.
func TestDictionaryPruneDeferredCommitFailure(t *testing.T) {
	db := centraltest.New(t)
	pruneExec(t, db, `INSERT INTO dict_node(name) VALUES('orphan');
 CREATE FUNCTION reject_prune_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test commit refusal'; END $$;
 CREATE CONSTRAINT TRIGGER reject_prune_commit AFTER DELETE ON dict_node DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_prune_commit()`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	warned := make(chan struct{}, 1)
	handler := &pruneFailureHandler{Handler: slog.NewTextHandler(&logs, nil), warned: warned}
	j := central.Janitor{DB: db, Logger: slog.New(handler), Jobs: []central.Job{{Name: "querylog-dictionary-retention", Run: pruneDictionaries}}}
	done := make(chan error, 1)
	go func() { done <- j.Run(ctx) }()
	select {
	case <-warned:
	case <-time.After(5 * time.Second):
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		t.Fatal("commit refusal was not reported")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "rows=") {
		t.Fatal("scheduler reported uncommitted deletions")
	}
	if n := scalar(t, db, `SELECT count(*) FROM dict_node`); n != 1 {
		t.Fatal("failed commit lost dictionary")
	}
	if n := scalar(t, db, `SELECT count(*) FROM kudurru_meta WHERE key LIKE 'prune_dict_%' AND value<>'{"after":0,"through":0}'`); n != 0 {
		t.Fatal("failed commit advanced sweep cursor")
	}
}

type pruneFailureHandler struct {
	slog.Handler
	warned chan struct{}
}

func (h *pruneFailureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &pruneFailureHandler{Handler: h.Handler.WithAttrs(attrs), warned: h.warned}
}

func (h *pruneFailureHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	if r.Level >= slog.LevelWarn {
		select {
		case h.warned <- struct{}{}:
		default:
		}
	}
	return err
}

func TestDictionaryPruneSchemaIsolation(t *testing.T) {
	orphanDB := centraltest.New(t)
	retainedDB := centraltest.New(t)
	for _, db := range []*central.DB{orphanDB, retainedDB} {
		pruneExec(t, db, `INSERT INTO dict_node(name) VALUES('same-node'); INSERT INTO dict_domain(name) VALUES('same.example')`)
	}
	pruneExec(t, retainedDB, `INSERT INTO rollup_domain_1d(ts,node,domain,total) VALUES(0,1,1,7)`)
	if n := prunePass(t, orphanDB); n != 2 {
		t.Fatalf("foreign schema protected unrelated IDs: %d", n)
	}
	if n := prunePass(t, retainedDB); n != 0 {
		t.Fatalf("cross-schema prune removed retained IDs: %d", n)
	}
	if n := scalar(t, retainedDB, `SELECT total FROM rollup_domain_1d r JOIN dict_domain d ON d.id=r.domain WHERE d.name='same.example'`); n != 7 {
		t.Fatalf("schema isolation lost data: %d", n)
	}
}

func TestDictionaryPruneJanitorCommitDeadline(t *testing.T) {
	db := centraltest.New(t)
	// This function only runs during COMMIT, after Job.Run has returned a
	// tentative positive count. The local statement timeout is deliberately
	// lifted there so the scheduler's whole-transaction context must stop it.
	pruneExec(t, db, `INSERT INTO dict_node(name) VALUES('orphan');
 CREATE FUNCTION delay_prune_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(5); RETURN OLD; END $$;
 CREATE CONSTRAINT TRIGGER delay_prune_commit AFTER DELETE ON dict_node DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION delay_prune_commit()`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	warned := make(chan struct{}, 1)
	staged := false
	j := central.Janitor{DB: db, Logger: slog.New(&pruneFailureHandler{Handler: slog.NewTextHandler(&logs, nil), warned: warned}), Jobs: []central.Job{{Name: "commit-deadline", Timeout: dictionaryPruneTimeout, Run: func(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
		n, err := pruneDictionaries(ctx, tx, now)
		if err != nil {
			return n, err
		}
		if err := central.NoStatementTimeout(ctx, tx); err != nil {
			return 0, err
		}
		staged = n == 1
		return n, nil
	}}}}
	done := make(chan error, 1)
	go func() { done <- j.Run(ctx) }()
	select {
	case <-warned:
	case <-time.After(3 * time.Second):
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		t.Fatal("transaction commit exceeded scheduler budget")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !staged {
		t.Fatal("test did not reach delayed COMMIT with staged deletion")
	}
	if strings.Contains(logs.String(), "rows=") {
		t.Fatal("timed-out commit reported healthy deletion")
	}
	// Cancellation must have reached the PostgreSQL backend, not merely
	// returned to Go while a commit keeps running. Acquire the deleted row
	// with a short lock timeout and verify rollback on the actual server.
	err := db.Tx(context.Background(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(context.Background(), `SELECT name FROM dict_node WHERE id=1 FOR UPDATE`).Scan(&name); err != nil {
			return err
		}
		if name != "orphan" {
			return errors.New("timed-out commit changed dictionary")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
