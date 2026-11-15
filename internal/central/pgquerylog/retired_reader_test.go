package pgquerylog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	testclock "k8s.io/utils/clock/testing"
)

// Exercise every public historical read before/after adding an unrelated node,
// then expire raw rows. Both source epochs of the retired name remain visible.
func TestReaderRetiredNodeIsolation(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	ctx := t.Context()
	for epoch := range 2 {
		w := writer(t, "retired", fc)
		for i := range 7 {
			at := t0.Add(-time.Duration(48+epoch*24)*time.Hour + time.Duration(i)*time.Minute)
			e := ev("retired", at, "10.0.0.1", fmt.Sprintf("a-%d.example", i), []pipeline.Status{pipeline.StatusForwarded, pipeline.StatusCached, pipeline.StatusBlockedList, pipeline.StatusAuthoritative, pipeline.StatusBlockedRegex, pipeline.StatusDropped, pipeline.StatusServfailUpstream}[i])
			e.Zone = "a.example."
			e.CNAME = "a-cname.example"
			e.Policy = "a/policy"
			e.List = "a/list"
			e.Upstream = "a/upstream"
			write(t, w, e)
		}
		if err := w.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
		shipAll(t, shipper(t, db, w, "retired", fc))
	}
	cen := centralReader(db, fc)
	r := cen.ForNode("retired")
	win := sqlitelog.Window{From: t0.Add(-4 * 24 * time.Hour), Until: t0}
	calls := []struct {
		name string
		run  func(*sqlitelog.Reader) (any, error)
	}{
		{"summary", func(r *sqlitelog.Reader) (any, error) { return r.Summary(ctx, sqlitelog.SummaryQuery{Window: win}) }},
		{"shard", func(r *sqlitelog.Reader) (any, error) {
			return r.SummaryShard(ctx, sqlitelog.SummaryQuery{Window: win})
		}},
		{"series", func(r *sqlitelog.Reader) (any, error) {
			return r.Timeseries(ctx, sqlitelog.TimeseriesQuery{Window: win})
		}},
		{"daily", func(r *sqlitelog.Reader) (any, error) {
			return r.Timeseries(ctx, sqlitelog.TimeseriesQuery{Window: win, Step: 24 * time.Hour})
		}},
		{"domains", func(r *sqlitelog.Reader) (any, error) {
			return r.TopDomains(ctx, sqlitelog.TopQuery{Window: win, N: 100})
		}},
		{"blocked-domains", func(r *sqlitelog.Reader) (any, error) {
			return r.TopDomains(ctx, sqlitelog.TopQuery{Window: win, N: 100, Blocked: true})
		}},
		{"clients", func(r *sqlitelog.Reader) (any, error) { return r.TopClients(ctx, sqlitelog.TopQuery{Window: win}) }},
		{"lists", func(r *sqlitelog.Reader) (any, error) {
			return r.TopLists(ctx, sqlitelog.TopQuery{Window: win, Blocked: true})
		}},
		{"zones", func(r *sqlitelog.Reader) (any, error) { return r.TopZones(ctx, sqlitelog.TopQuery{Window: win}) }},
		{"upstreams", func(r *sqlitelog.Reader) (any, error) { return r.TopUpstreams(ctx, sqlitelog.TopQuery{Window: win}) }},
		{"client-series", func(r *sqlitelog.Reader) (any, error) {
			return r.ClientTimeseries(ctx, sqlitelog.ClientTimeseriesQuery{Window: win})
		}},
		{"recent", func(r *sqlitelog.Reader) (any, error) { return r.RecentBlocked(ctx, 100) }},
		{"queries", func(r *sqlitelog.Reader) (any, error) { return r.Queries(ctx, sqlitelog.QueryFilter{Limit: 100}) }},
		{"cidr", func(r *sqlitelog.Reader) (any, error) {
			return r.Queries(ctx, sqlitelog.QueryFilter{Client: "10.0.0.0/24", Limit: 100})
		}},
		{"policy", func(r *sqlitelog.Reader) (any, error) { return r.PolicyStats(ctx, "a/policy", win, 100) }},
		{"groups", func(r *sqlitelog.Reader) (any, error) { return r.ClientsSeenByGroup(ctx, win.From) }},
	}
	for _, field := range []string{sqlitelog.FieldDomain, sqlitelog.FieldClient, sqlitelog.FieldUpstream, sqlitelog.FieldList, sqlitelog.FieldZone, sqlitelog.FieldPolicy} {
		calls = append(calls, struct {
			name string
			run  func(*sqlitelog.Reader) (any, error)
		}{"suggest-" + field, func(r *sqlitelog.Reader) (any, error) { return r.Suggestions(ctx, field, "", 100) }})
	}
	baseline := func() []any {
		out := make([]any, len(calls))
		for i, c := range calls {
			v, err := c.run(r)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			out[i] = normal(t, v)
		}
		return out
	}
	check := func(want []any) {
		for i, c := range calls {
			t.Run(c.name, func(t *testing.T) {
				got, err := c.run(r)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(want[i], normal(t, got)) {
					t.Fatalf("unrelated node changed %s", c.name)
				}
			})
		}
	}
	want := baseline()
	w := writer(t, "other", fc)
	for i := range 3 {
		e := ev("other", t0.Add(-time.Minute), fmt.Sprintf("10.0.0.%d", i+1), "b-only.example", pipeline.StatusBlockedList)
		e.Policy = "b/policy"
		e.CNAME = "b-cname.example"
		e.List = "b/list"
		e.Zone = "b.example."
		e.Upstream = "b/upstream"
		write(t, w, e)
	}
	shipAll(t, shipper(t, db, w, "other", fc))
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE source SET last_seen=$1 WHERE node='retired'`, t0.Add(-3*time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check(want)
	// A source's mutable current name cannot erase the node on historical rows.
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE source SET node='renamed-current' WHERE node='retired'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check(want)
	page, err := r.Queries(ctx, sqlitelog.QueryFilter{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if page.RecordsTotal != 14 || page.Scope.Scope != "node" || page.Source != "central" || len(page.Nodes) != 1 || page.Nodes[0].Name != "retired" || page.Partial {
		t.Fatalf("retired scope/count: %+v", page.Scope)
	}
	seen := map[uint64]bool{}
	for {
		for _, row := range page.Queries {
			if row.Node != "retired" || seen[row.ID] {
				t.Fatal("page crossed node or repeated ID")
			}
			seen[row.ID] = true
		}
		if page.CursorNext == "" {
			break
		}
		page, err = r.Queries(ctx, sqlitelog.QueryFilter{Limit: 3, Cursor: page.CursorNext})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 14 {
		t.Fatalf("pagination lost rows: %d", len(seen))
	}
	if _, err := cen.ForNode("unknown").Queries(ctx, sqlitelog.QueryFilter{}); !errors.Is(err, sqlitelog.ErrUnknownNode) {
		t.Fatalf("unknown node: %v", err)
	}
	if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `DELETE FROM query`); return err }); err != nil {
		t.Fatal(err)
	}
	// Dictionary visibility is retained by rollups after fact retention.
	domains, err := r.Suggestions(ctx, sqlitelog.FieldDomain, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains.Values) != 7 {
		t.Fatalf("rollup domain visibility: %v", domains.Values)
	}
	// Repeat every reader after expiration. Unrelated rollups still retain
	// their own dictionaries, but must not reveal their values here.
	for _, call := range calls {
		value, err := call.run(r)
		if err != nil {
			t.Fatalf("expired %s: %v", call.name, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "b-only") || strings.Contains(string(encoded), "b/upstream") || strings.Contains(string(encoded), "b/policy") {
			t.Fatalf("expired %s leaked another node", call.name)
		}
	}
	clients, err := r.TopClients(ctx, sqlitelog.TopQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	if len(clients.Items) != 1 || clients.Items[0].LastSeen == nil || clients.Items[0].LastSeen.After(t0.Add(-47*time.Hour)) {
		t.Fatal("client last_seen leaked the other node")
	}
	page, err = r.Queries(ctx, sqlitelog.QueryFilter{})
	if err != nil || page.RecordsTotal != 0 || page.EarliestTSDisk != nil {
		t.Fatalf("expired rows: %v", err)
	}
	var wg sync.WaitGroup
	for _, node := range []string{"retired", "other"} {
		wg.Go(func() {
			for range 8 {
				out, err := cen.ForNode(node).Summary(context.Background(), sqlitelog.SummaryQuery{Window: win})
				if err != nil {
					t.Error(err)
					return
				}
				if len(out.Nodes) != 1 || out.Nodes[0].Name != node {
					t.Error("concurrent scope contamination")
				}
			}
		})
	}
	wg.Wait()
	// Retained source registration is sufficient even after all data is gone.
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO source(uuid,kind,node,version) VALUES ('empty-epoch','querylog','empty','test')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	empty, err := cen.ForNode("empty").Queries(ctx, sqlitelog.QueryFilter{})
	if err != nil || empty.RecordsTotal != 0 {
		t.Fatalf("known empty source: %v", err)
	}
}
