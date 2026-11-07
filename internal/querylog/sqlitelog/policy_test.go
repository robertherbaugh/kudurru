package sqlitelog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

func withPolicy(p string) evOpt { return func(e *querylog.Event) { e.Policy = p } }

func withGroup(g string) evOpt { return func(e *querylog.Event) { e.ClientGroup = g } }

// TestPolicyColumn: the deciding policy is stored per row, read back on
// GET /queries rows, filtered by policy= and suggested.
func TestPolicyColumn(t *testing.T) {
	w, _ := newWriter(t, t0)
	mustWrite(t, w,
		event(t0, "10.0.0.1", "a.example", withPolicy("home/kids")),
		event(t0.Add(time.Second), "10.0.0.2", "b.example", withPolicy("default/default")),
		event(t0.Add(2*time.Second), "10.0.0.3", "c.example"),
	)
	r := w.Reader()
	ctx := context.Background()
	page, err := r.Queries(ctx, QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, q := range page.Queries {
		got[q.Domain] = q.Policy
	}
	if got["a.example"] != "home/kids" || got["b.example"] != "default/default" || got["c.example"] != "" {
		t.Errorf("row policies = %v", got)
	}
	page, err = r.Queries(ctx, QueryFilter{Policy: "home/kids"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Queries) != 1 || page.Queries[0].Domain != "a.example" || page.RecordsFiltered != 1 {
		t.Errorf("policy filter: %+v", page.Queries)
	}
	if page, _ = r.Queries(ctx, QueryFilter{Policy: "home/none"}); len(page.Queries) != 0 {
		t.Errorf("unknown policy matched %d rows", len(page.Queries))
	}
	s, err := r.Suggestions(ctx, FieldPolicy, "home/", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Values) != 1 || s.Values[0] != "home/kids" {
		t.Errorf("suggestions = %v", s.Values)
	}
}

// TestPolicyStats: counts, top blocked domains and top clients of one
// policy in a window; other policies' rows are left out.
func TestPolicyStats(t *testing.T) {
	w, _ := newWriter(t, t0)
	kids := withPolicy("home/kids")
	blocked := withStatus(pipeline.StatusBlockedList)
	mustWrite(t, w,
		event(t0.Add(-time.Hour), "10.0.0.1", "games.example", kids, blocked, withList("home/games", "||games.example^")),
		event(t0.Add(-time.Hour), "10.0.0.1", "games.example", kids, blocked, withList("home/games", "||games.example^")),
		event(t0.Add(-time.Hour), "10.0.0.2", "ads.example", kids, blocked, withList("home/ads", "||ads.example^")),
		event(t0.Add(-time.Hour), "10.0.0.1", "school.example", kids),
		event(t0.Add(-time.Hour), "10.0.0.9", "games.example", withPolicy("default/default"), blocked),
		event(t0.Add(-48*time.Hour), "10.0.0.1", "old.example", kids, blocked),
	)
	st, err := w.Reader().PolicyStats(context.Background(), "home/kids", Window{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.Queries != 4 || st.Blocked != 3 {
		t.Errorf("queries %d blocked %d, want 4 and 3", st.Queries, st.Blocked)
	}
	if len(st.TopBlocked) != 2 || st.TopBlocked[0] != (PolicyDomain{Domain: "games.example", Count: 2}) {
		t.Errorf("top blocked = %+v", st.TopBlocked)
	}
	if len(st.TopClients) != 2 || st.TopClients[0] != (PolicyClient{Key: "10.0.0.1", Count: 3}) {
		t.Errorf("top clients = %+v", st.TopClients)
	}
	none, err := w.Reader().PolicyStats(context.Background(), "home/absent", Window{}, 10)
	if err != nil || none.Queries != 0 || len(none.TopBlocked) != 0 || none.TopClients == nil {
		t.Errorf("absent policy: %+v %v", none, err)
	}
}

func TestClientsSeenByGroup(t *testing.T) {
	w, _ := newWriter(t, t0)
	mustWrite(t, w,
		event(t0.Add(-time.Hour), "10.0.0.1", "a.example", withGroup("home/kids")),
		event(t0.Add(-time.Hour), "10.0.0.1", "b.example", withGroup("home/kids"), withClientName("tablet")),
		event(t0.Add(-time.Hour), "10.0.0.2", "a.example", withGroup("home/kids")),
		event(t0.Add(-time.Hour), "10.0.0.3", "a.example", withGroup("default/default")),
		event(t0.Add(-72*time.Hour), "10.0.0.4", "a.example", withGroup("home/kids")),
	)
	got, err := w.Reader().ClientsSeenByGroup(context.Background(), t0.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got["home/kids"] != 2 || got["default/default"] != 1 || len(got) != 2 {
		t.Errorf("seen = %v", got)
	}
}

// TestUpgradeFromSchema1: a querylog.db written by schema version 1 (no
// policy column) is upgraded in place on open; its rows survive with no
// policy and new rows carry one.
func TestUpgradeFromSchema1(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	opts := func(o *Options) { o.Path = path }
	w, _ := newWriter(t, t0, opts)
	mustWrite(t, w, event(t0, "10.0.0.1", "old.example"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Turn the file back into schema 1.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`DROP TRIGGER query_domain_insert`, `DROP TRIGGER query_domain_delete`, `DROP TABLE query_domain_hour`, `ALTER TABLE query DROP COLUMN policy`, `DROP TABLE dict_policy`, `UPDATE meta SET value=1 WHERE key='schema_version'`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = db.Close()

	w2, _ := newWriter(t, t0.Add(time.Minute), opts)
	meta, err := readMeta(context.Background(), w2.w)
	if err != nil || meta[metaSchemaVersion] != "3" {
		t.Fatalf("schema version after upgrade %q, %v", meta[metaSchemaVersion], err)
	}
	mustWrite(t, w2, event(t0.Add(time.Minute), "10.0.0.1", "new.example", withPolicy("home/kids")))
	page, err := w2.Reader().Queries(context.Background(), QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Queries) != 2 || page.Queries[0].Policy != "home/kids" || page.Queries[1].Domain != "old.example" || page.Queries[1].Policy != "" {
		t.Errorf("rows after upgrade: %+v", page.Queries)
	}
}
