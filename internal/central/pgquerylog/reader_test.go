package pgquerylog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
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

// fixture writes a varied day of queries to w: statuses, clients with and
// without names, query types, protocols, upstreams, zones, lists,
// policies and latencies, from 09:02:30 to 12:01:45 (the clock reads
// 12:03 when the readers run: three complete hours and part of the
// current one, nothing in the future).
func fixture(t *testing.T, w *sqlitelog.Writer, node string, now time.Time, seed int) {
	t.Helper()
	statuses := []pipeline.Status{pipeline.StatusForwarded, pipeline.StatusCached, pipeline.StatusBlockedList, pipeline.StatusAuthoritative,
		pipeline.StatusBlockedRegex, pipeline.StatusForwarded, pipeline.StatusServfailUpstream}
	protos := []pipeline.Proto{pipeline.ProtoUDP, pipeline.ProtoTCP, pipeline.ProtoDoT, pipeline.ProtoDoH}
	var evs []*querylog.Event
	for i := range 240 {
		k := i*7 + seed
		e := &querylog.Event{
			ID: uint64(i + 1), Time: now.Add(-3*time.Hour + 150*time.Second + time.Duration(i)*45*time.Second), Latency: time.Duration(k%40) * time.Millisecond,
			Node: node, Proto: protos[k%len(protos)], Client: netip.AddrFrom4([4]byte{10, 0, 0, byte(1 + k%6)}), //nolint:gosec // 1-6
			Name: fmt.Sprintf("d%d.example%d.", k%9, k%2), QType: []uint16{1, 28, 65, 15}[k%4], QClass: 1,
			Status: statuses[k%len(statuses)], Rcode: []int{0, 3, 2}[k%3], ReplyType: querylog.ReplyIP, Answers: 1,
			Policy: []string{"default/default", "kids/evening"}[k%2],
		}
		if k%6 < 3 {
			e.ClientName, e.ClientGroup = fmt.Sprintf("host-%d", k%6), "default/default"
		}
		switch e.Status {
		case pipeline.StatusBlockedList, pipeline.StatusBlockedRegex:
			e.List, e.Rule = []string{"default/ads", "default/trackers"}[k%2], "||x^"
		case pipeline.StatusAuthoritative:
			e.Zone = "home.example."
		case pipeline.StatusCached:
			e.Upstream = "cache"
		default:
			e.Upstream = []string{"default", "secure"}[k%2]
		}
		evs = append(evs, e)
	}
	if err := w.Write(evs); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// normal is a reader result as JSON with what may differ between a node's
// answer and the central one removed: scope, timings and row ids.
func normal(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var strip func(any)
	strip = func(x any) {
		switch y := x.(type) {
		case map[string]any:
			for _, k := range []string{"scope", "nodes", "partial", "source", "took_ms", "id", "cursor_next", "records_capped"} {
				delete(y, k)
			}
			for _, c := range y {
				strip(c)
			}
		case []any:
			for _, c := range y {
				strip(c)
			}
		}
	}
	strip(m)
	return untie(m)
}

// untie removes what depends on dictionary ids: both readers break ties
// between equal counts by id, and the central ids are assigned in another
// order. Lists of named items are sorted by name (the tests ask for every
// item, so no tie straddles a cut-off), and the per-client series are
// keyed by client.
func untie(m any) any {
	switch y := m.(type) {
	case map[string]any:
		if clients, ok := y["clients"].([]any); ok {
			if points, ok := y["points"].([]any); ok {
				series := map[string][]any{}
				for i, c := range clients {
					key := c.(map[string]any)["key"].(string)
					for _, p := range points {
						series[key] = append(series[key], p.(map[string]any)["counts"].([]any)[i])
					}
				}
				y["clients"], y["points"] = series, len(points)
			}
		}
		for k, c := range y {
			y[k] = untie(c)
		}
	case []any:
		for i, c := range y {
			y[i] = untie(c)
		}
		sort.SliceStable(y, func(i, j int) bool { return name(y[i]) < name(y[j]) })
	}
	return m
}

func name(v any) string {
	o, ok := v.(map[string]any)
	if !ok || o["ts"] != nil {
		return "" // query rows keep their order: newest first
	}
	for _, k := range []string{"key", "addr", "domain"} {
		if s, ok := o[k].(string); ok {
			return s
		}
	}
	return ""
}

func centralReader(db *central.DB, fc *testclock.FakeClock) *sqlitelog.Reader {
	return sqlitelog.NewCentralReader(db.SQL, sqlitelog.CentralOptions{Now: fc.Now, RawRetention: 30 * 24 * time.Hour})
}

// TestReaderConformance: the central reader gives a node's answers once
// the node has shipped, for every question the API asks.
func TestReaderConformance(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	w := writer(t, "node-a", fc)
	fixture(t, w, "node-a", t0, 0)
	// Complete the earlier hours on the node, then ship everything.
	fc.SetTime(t0.Add(3 * time.Minute))
	if err := w.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	shipAll(t, shipper(t, db, w, "node-a", fc))
	local, cen := w.Reader(), centralReader(db, fc)
	ctx := context.Background()

	win := sqlitelog.Window{From: t0.Add(-4 * time.Hour), Until: fc.Now()}
	short := sqlitelog.Window{From: t0.Add(-90 * time.Minute), Until: fc.Now()}
	type call struct {
		name string
		run  func(r *sqlitelog.Reader) (any, error)
	}
	calls := []call{
		{"Summary", func(r *sqlitelog.Reader) (any, error) { return r.Summary(ctx, sqlitelog.SummaryQuery{Window: win}) }},
		{"SummaryShort", func(r *sqlitelog.Reader) (any, error) { return r.Summary(ctx, sqlitelog.SummaryQuery{Window: short}) }},
		{"Timeseries", func(r *sqlitelog.Reader) (any, error) {
			return r.Timeseries(ctx, sqlitelog.TimeseriesQuery{Window: win})
		}},
		{"TimeseriesHourly", func(r *sqlitelog.Reader) (any, error) {
			return r.Timeseries(ctx, sqlitelog.TimeseriesQuery{Window: win, Step: time.Hour})
		}},
		{"TopDomains", func(r *sqlitelog.Reader) (any, error) {
			return r.TopDomains(ctx, sqlitelog.TopQuery{Window: win, N: 100})
		}},
		{"TopBlockedDomains", func(r *sqlitelog.Reader) (any, error) {
			return r.TopDomains(ctx, sqlitelog.TopQuery{Window: win, N: 100, Blocked: true})
		}},
		{"TopClients", func(r *sqlitelog.Reader) (any, error) { return r.TopClients(ctx, sqlitelog.TopQuery{Window: win}) }},
		{"TopBlockedLists", func(r *sqlitelog.Reader) (any, error) {
			return r.TopLists(ctx, sqlitelog.TopQuery{Window: win, Blocked: true})
		}},
		{"TopZones", func(r *sqlitelog.Reader) (any, error) { return r.TopZones(ctx, sqlitelog.TopQuery{Window: win}) }},
		{"TopUpstreams", func(r *sqlitelog.Reader) (any, error) { return r.TopUpstreams(ctx, sqlitelog.TopQuery{Window: win}) }},
		{"ClientTimeseries", func(r *sqlitelog.Reader) (any, error) {
			return r.ClientTimeseries(ctx, sqlitelog.ClientTimeseriesQuery{Window: win, N: 10})
		}},
		{"RecentBlocked", func(r *sqlitelog.Reader) (any, error) { return r.RecentBlocked(ctx, 7) }},
		{"Queries", func(r *sqlitelog.Reader) (any, error) { return r.Queries(ctx, sqlitelog.QueryFilter{Limit: 25}) }},
		{"QueriesFiltered", func(r *sqlitelog.Reader) (any, error) {
			return r.Queries(ctx, sqlitelog.QueryFilter{Client: "10.0.0.0/30", StatusClass: pipeline.ClassBlocked, Limit: 25})
		}},
		{"QueriesDomainWildcard", func(r *sqlitelog.Reader) (any, error) {
			return r.Queries(ctx, sqlitelog.QueryFilter{Domain: "d3.*", Limit: 25})
		}},
		{"QueriesClientName", func(r *sqlitelog.Reader) (any, error) {
			return r.Queries(ctx, sqlitelog.QueryFilter{Client: "host-*", QType: 28, Limit: 25})
		}},
		{"QueriesByPolicyAndList", func(r *sqlitelog.Reader) (any, error) {
			return r.Queries(ctx, sqlitelog.QueryFilter{Policy: "kids/evening", List: "default/ads", Limit: 25})
		}},
		{"SuggestDomain", func(r *sqlitelog.Reader) (any, error) { return r.Suggestions(ctx, sqlitelog.FieldDomain, "d1", 10) }},
		{"SuggestClient", func(r *sqlitelog.Reader) (any, error) { return r.Suggestions(ctx, sqlitelog.FieldClient, "", 20) }},
		{"PolicyStats", func(r *sqlitelog.Reader) (any, error) { return r.PolicyStats(ctx, "kids/evening", win, 100) }},
		{"ClientsSeenByGroup", func(r *sqlitelog.Reader) (any, error) { return r.ClientsSeenByGroup(ctx, t0.Add(-24*time.Hour)) }},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			want, err := c.run(local)
			if err != nil {
				t.Fatalf("local: %v", err)
			}
			got, err := c.run(cen)
			if err != nil {
				t.Fatalf("central: %v", err)
			}
			if w, g := normal(t, want), normal(t, got); !reflect.DeepEqual(w, g) {
				wb, _ := json.MarshalIndent(w, "", " ")
				gb, _ := json.MarshalIndent(g, "", " ")
				t.Fatalf("central answer differs\nlocal:   %s\ncentral: %s", wb, gb)
			}
		})
	}
	// The central query log pages with its own ids.
	page, err := cen.Queries(ctx, sqlitelog.QueryFilter{Limit: 100})
	if err != nil || page.CursorNext == "" || page.Scope.Scope != sqlitelog.ScopeCluster || page.Source != sqlitelog.SourceCentral {
		t.Fatalf("page: cursor %q scope %+v err %v", page.CursorNext, page.Scope, err)
	}
	next, err := cen.Queries(ctx, sqlitelog.QueryFilter{Limit: 100, Cursor: page.CursorNext})
	if err != nil || len(next.Queries) != 100 || next.Queries[0].TS.After(page.Queries[99].TS) {
		t.Fatalf("second page: %d rows: %v", len(next.Queries), err)
	}
}

// TestReaderTwoNodes: the central answer counts both nodes.
func TestReaderTwoNodes(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0)
	ctx := context.Background()
	nodes := []string{"node-a", "node-b"}
	var writers []*sqlitelog.Writer
	for i, node := range nodes {
		w := writer(t, node, fc)
		fixture(t, w, node, t0, i*3)
		writers = append(writers, w)
	}
	fc.SetTime(t0.Add(3 * time.Minute))
	var total, blocked int64
	win := sqlitelog.Window{From: t0.Add(-4 * time.Hour), Until: fc.Now()}
	for i, w := range writers {
		if err := w.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
		shipAll(t, shipper(t, db, w, nodes[i], fc))
		sum, err := w.Reader().Summary(ctx, sqlitelog.SummaryQuery{Window: win})
		if err != nil {
			t.Fatal(err)
		}
		total += sum.Queries.Total
		blocked += sum.Queries.Blocked
	}
	cen := centralReader(db, fc)
	sum, err := cen.Summary(ctx, sqlitelog.SummaryQuery{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Queries.Total != total || sum.Queries.Blocked != blocked || total != 480 {
		t.Fatalf("central total %d blocked %d; nodes %d %d", sum.Queries.Total, sum.Queries.Blocked, total, blocked)
	}
	if len(sum.Nodes) != 2 || sum.Nodes[0].Name != "node-a" || !sum.Nodes[1].OK || sum.Partial {
		t.Fatalf("scope %+v", sum.Scope)
	}
	page, err := cen.Queries(ctx, sqlitelog.QueryFilter{Limit: 1000})
	if err != nil || page.RecordsTotal != 480 || len(page.Queries) != 480 {
		t.Fatalf("query log: total %d rows %d: %v", page.RecordsTotal, len(page.Queries), err)
	}
	seen := map[string]bool{}
	for _, q := range page.Queries {
		seen[q.Node] = true
	}
	if !seen["node-a"] || !seen["node-b"] {
		t.Fatalf("nodes in the query log %v", seen)
	}
	// A node whose statistics stopped arriving ten minutes ago makes the
	// answer partial (cursor times are the database's; the clock is fake).
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ship_cursor SET updated_at = $1 WHERE stream = 'rollup_1m'
			AND source IN (SELECT id FROM source WHERE node = 'node-a')`, fc.Now().Add(-10*time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if sum, err = cen.Summary(ctx, sqlitelog.SummaryQuery{Window: win}); err != nil || !sum.Partial || sum.Nodes[0].OK || sum.Nodes[0].Error == "" {
		t.Fatalf("lagging node: %+v: %v", sum.Scope, err)
	}
}

func TestReaderUnavailable(t *testing.T) {
	db, err := central.Open(central.Options{DSN: "postgres://x@127.0.0.1:1/x"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := sqlitelog.NewCentralReader(db.SQL, sqlitelog.CentralOptions{})
	if _, err := r.Summary(context.Background(), sqlitelog.SummaryQuery{}); !errors.Is(err, central.ErrUnavailable) {
		t.Fatalf("summary: %v", err)
	}
}
