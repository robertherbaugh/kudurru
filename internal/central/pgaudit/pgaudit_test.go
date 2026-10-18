package pgaudit_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/robertherbaugh/kudurru/ee/auditstore"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/audittest"
	"github.com/robertherbaugh/kudurru/internal/audit/spool"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/central/pgaudit"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func openSpool(t *testing.T, node string) *spool.Spool {
	t.Helper()
	s, err := spool.Open(context.Background(), spool.Options{Path: filepath.Join(t.TempDir(), spool.FileName), Node: node, Max: 100000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func ship(t *testing.T, tgt audit.Target, src audit.Source) {
	t.Helper()
	ctx := context.Background()
	after, err := tgt.Cursor(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	for {
		evs, err := src.ReadAfter(ctx, after, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			return
		}
		if err := tgt.Append(ctx, src, evs); err != nil {
			t.Fatal(err)
		}
		after = evs[len(evs)-1].ID
	}
}

type store struct {
	t   *testing.T
	sp  *spool.Spool
	tgt *pgaudit.Target
	rd  *pgaudit.Reader
}

func (s *store) Record(ctx context.Context, ev audit.Event) error { return s.sp.Record(ctx, ev) }
func (s *store) Read(ctx context.Context, q audit.Query) ([]audit.Event, error) {
	ship(s.t, s.tgt, s.sp)
	return s.rd.Read(ctx, q)
}

func TestConformance(t *testing.T) {
	audittest.Run(t, func(t *testing.T) audittest.Store {
		db := centraltest.New(t)
		return &store{t: t, sp: openSpool(t, "node-a"), tgt: &pgaudit.Target{DB: db}, rd: &pgaudit.Reader{DB: db}}
	})
}

func event(i int, node string) audit.Event {
	return audit.Event{Time: time.Now().Add(time.Duration(i) * time.Millisecond), Node: node,
		Actor:  audit.Actor{Kind: "User", Name: "alice", Groups: []string{"admins", "ops"}, Auth: "cookie"},
		Action: "dnszone.update", Resource: audit.Resource{Kind: "DNSZone", Name: fmt.Sprintf("z%d", i)}, Result: audit.ResultSuccess,
		Reason: "because", Authz: "binding", Manager: "api", ResourceVersion: "42",
		Before: []byte(`{"b":1,"a":2}`), After: []byte(`{"z":true}`), Request: audit.Request{ID: "r", IP: "10.0.0.9", UA: "curl", Method: "PUT", Path: "/x"}}
}

// TestChainAcrossNodes: eight nodes shipping at once form one gapless
// chain that verifies; every event is stored once with its documents
// byte for byte.
func TestChainAcrossNodes(t *testing.T) {
	db := centraltest.New(t)
	tgt := &pgaudit.Target{DB: db, Version: "test"}
	const nodes, each = 8, 250
	var wg sync.WaitGroup
	for n := range nodes {
		sp := openSpool(t, fmt.Sprintf("node-%d", n))
		for i := range each {
			if err := sp.Record(context.Background(), event(i, "")); err != nil {
				t.Fatal(err)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ship(t, tgt, sp)
			ship(t, tgt, sp) // again: nothing new
		}()
	}
	wg.Wait()
	ctx := context.Background()
	if err := pgaudit.Verify(ctx, db); err != nil {
		t.Fatal(err)
	}
	rd := &pgaudit.Reader{DB: db}
	var all []audit.Event
	q := audit.Query{Limit: 1000}
	for {
		page, err := rd.Read(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < q.Limit {
			break
		}
		q.After(page[len(page)-1])
	}
	if len(all) != nodes*each {
		t.Fatalf("%d events, want %d", len(all), nodes*each)
	}
	for i, ev := range all {
		if ev.ID != int64(i+1) {
			t.Fatalf("event %d has id %d", i, ev.ID)
		}
	}
	ev := all[0]
	if string(ev.Before) != `{"b":1,"a":2}` || ev.Actor.Groups[1] != "ops" || ev.Request.UA != "curl" || ev.Node == "" || !strings.HasPrefix(ev.Node, "node-") {
		t.Fatalf("event %+v", ev)
	}
}

func TestVerifyDetectsChanges(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ change, want string }{
		{``, ""},
		{`UPDATE audit SET reason = 'nothing to see' WHERE id = 4`, "event 4 was changed"},
		{`UPDATE audit SET before = '{"a":2,"b":1}' WHERE id = 2`, "event 2 was changed"},
		{`DELETE FROM audit WHERE id = 6`, "events 6 to 6 are missing"},
		{`DELETE FROM audit WHERE id = 10`, "the chain ends at event 9"},
	} {
		db := centraltest.New(t)
		sp := openSpool(t, "node-a")
		for i := range 10 {
			if err := sp.Record(ctx, event(i, "")); err != nil {
				t.Fatal(err)
			}
		}
		ship(t, &pgaudit.Target{DB: db}, sp)
		pool, _ := db.Pool()
		if tc.change != "" {
			if _, err := pool.Exec(ctx, tc.change); err != nil {
				t.Fatal(err)
			}
		}
		err := (&pgaudit.Reader{DB: db}).Verify(ctx)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%q: %v", tc.change, err)
		}
		if tc.want != "" && !errors.Is(err, audit.ErrTampered) {
			t.Fatalf("integrity failure must be distinguishable from storage errors: %v", err)
		}
	}
}

func TestPruneKeepsChainVerifiable(t *testing.T) {
	db := centraltest.New(t)
	sp := openSpool(t, "node-a")
	ctx := context.Background()
	old := time.Now().Add(-500 * 24 * time.Hour)
	for i := range 5 {
		ev := event(i, "")
		ev.Time = old.Add(time.Duration(i) * time.Second)
		if err := sp.Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// A node with a slow clock recorded an old-looking event after a
	// recent one: the prune stops before the recent one.
	for i, ts := range []time.Time{time.Now(), old.Add(time.Hour), time.Now()} {
		ev := event(10+i, "")
		ev.Time = ts
		if err := sp.Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	ship(t, &pgaudit.Target{DB: db}, sp)
	job := pgaudit.PruneJob(400 * 24 * time.Hour)
	var n int64
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		n, err = job.Run(ctx, tx, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("pruned %d, want the 5 events before the first recent one", n)
	}
	if err := pgaudit.Verify(ctx, db); err != nil {
		t.Fatalf("verify after prune: %v", err)
	}
	evs, err := (&pgaudit.Reader{DB: db}).Read(ctx, audit.Query{})
	if err != nil || len(evs) != 3 || evs[0].ID != 6 {
		t.Fatalf("after prune %+v: %v", evs, err)
	}
}

// TestStateSource: the sqlite driver's state.db rows ship with their own
// chain kept as origin hashes.
func TestStateSource(t *testing.T) {
	db := centraltest.New(t)
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	rec := auditstore.SQLite{Store: st}
	for i := range 3 {
		if err := rec.Record(ctx, event(i, "")); err != nil {
			t.Fatal(err)
		}
	}
	src, err := audit.NewStateSource(ctx, st, "docker-1")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := audit.NewStateSource(ctx, st, "docker-1")
	if src.SourceID() == "" || again.SourceID() != src.SourceID() {
		t.Fatalf("source id %q then %q", src.SourceID(), again.SourceID())
	}
	ship(t, &pgaudit.Target{DB: db}, src)
	evs, err := (&pgaudit.Reader{DB: db}).Read(ctx, audit.Query{Actor: "User:alice"})
	if err != nil || len(evs) != 3 || evs[0].Node != "docker-1" {
		t.Fatalf("%+v: %v", evs, err)
	}
	if err := pgaudit.Verify(ctx, db); err != nil {
		t.Fatal(err)
	}
}

func TestUnavailable(t *testing.T) {
	db, err := central.Open(central.Options{DSN: "postgres://x@127.0.0.1:1/x"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := (&pgaudit.Reader{DB: db}).Read(context.Background(), audit.Query{}); err != central.ErrUnavailable {
		t.Fatalf("read: %v", err)
	}
	if _, err := (&pgaudit.Reader{DB: db}).Read(context.Background(), audit.Query{Cursor: "c1.x"}); err != audit.ErrBadCursor {
		t.Fatalf("opaque cursor: %v", err)
	}
}

// rawSource hands out events as recorded, unsanitized (a state.db row
// written before the API sanitized its requests).
type rawSource struct{ evs []audit.Event }

func (r *rawSource) SourceID() string { return "raw-source" }
func (r *rawSource) Node() string     { return "node-a" }
func (r *rawSource) ReadAfter(_ context.Context, after int64, n int) ([]audit.Event, error) {
	var out []audit.Event
	for _, ev := range r.evs {
		if ev.ID > after && len(out) < n {
			out = append(out, ev)
		}
	}
	return out, nil
}

// TestDirtyEventsShip: a NUL in Request.Path and invalid UTF-8 in
// Request.UA and a group do not stop shipping (the review's poison pill);
// they are stored sanitized and the chain verifies.
func TestDirtyEventsShip(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	src := &rawSource{}
	for i := range 3 {
		ev := event(i, "")
		ev.ID = int64(i + 1)
		if i == 1 {
			ev.Request.Path, ev.Request.UA, ev.Actor.Groups = "/api/v1/cache/flush\x00", "evil\xff\xfe", []string{"g\x00"}
			ev.Reason = "bad \xc3"
		}
		src.evs = append(src.evs, ev)
	}
	tgt := &pgaudit.Target{DB: db}
	ship(t, tgt, src)
	if c, err := tgt.Cursor(ctx, src); err != nil || c != 3 {
		t.Fatalf("cursor %d: %v", c, err)
	}
	evs, err := (&pgaudit.Reader{DB: db}).Read(ctx, audit.Query{})
	if err != nil || len(evs) != 3 {
		t.Fatalf("%d events: %v", len(evs), err)
	}
	if evs[1].Request.UA != "evil�" || evs[1].Request.Path != "/api/v1/cache/flush�" || evs[1].Actor.Groups[0] != "g�" {
		t.Fatalf("stored %+v", evs[1])
	}
	if err := pgaudit.Verify(ctx, db); err != nil {
		t.Fatal(err)
	}
	// The same through a node's spool.
	sp := openSpool(t, "node-b")
	if err := sp.Record(ctx, audit.Event{Action: "x.y", Resource: audit.Resource{Kind: "x"}, Result: audit.ResultDenied, Request: audit.Request{UA: "\xff"}}); err != nil {
		t.Fatal(err)
	}
	ship(t, tgt, sp)
	if err := pgaudit.Verify(ctx, db); err != nil {
		t.Fatal(err)
	}
}

// TestDataErrorStoresWithoutDetails: a batch the database still refuses
// for its data (SQLSTATE class 22) is stored once more without the
// events' free text instead of blocking the source forever. A trigger
// stands in for the refusal.
func TestDataErrorStoresWithoutDetails(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	pool, _ := db.Pool()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_poison() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.req_path LIKE '%poison%' THEN RAISE EXCEPTION 'refused' USING ERRCODE = '22023'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER refuse_poison BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION refuse_poison()`); err != nil {
		t.Fatal(err)
	}
	src := &rawSource{}
	for i := range 2 {
		ev := event(i, "")
		ev.ID = int64(i + 1)
		if i == 1 {
			ev.Request.Path = "/poison"
		}
		src.evs = append(src.evs, ev)
	}
	ship(t, &pgaudit.Target{DB: db}, src)
	evs, err := (&pgaudit.Reader{DB: db}).Read(ctx, audit.Query{})
	if err != nil || len(evs) != 2 {
		t.Fatalf("%d events: %v", len(evs), err)
	}
	for _, ev := range evs {
		if ev.Request.Path != "" || ev.Before != nil || !strings.HasPrefix(ev.Reason, "stored without details") || ev.Action != "dnszone.update" {
			t.Fatalf("event %+v", ev)
		}
	}
	if err := pgaudit.Verify(ctx, db); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyWhileAppending: appends committed while Verify pages through
// the chain are not reported as a truncated end or a gap.
func TestVerifyWhileAppending(t *testing.T) {
	db := centraltest.New(t)
	ctx := context.Background()
	src := &rawSource{}
	tgt := &pgaudit.Target{DB: db}
	add := func(n int) []audit.Event {
		var out []audit.Event
		for range n {
			ev := event(len(src.evs), "")
			ev.ID = int64(len(src.evs) + 1)
			src.evs = append(src.evs, ev)
			out = append(out, ev)
		}
		return out
	}
	if err := tgt.Append(ctx, src, add(2500)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 40 {
			if err := tgt.Append(ctx, src, add(25)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		if err := pgaudit.Verify(ctx, db); err != nil {
			t.Fatalf("verify while appending: %v", err)
		}
		select {
		case <-done:
			if err := pgaudit.Verify(ctx, db); err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
	}
}
