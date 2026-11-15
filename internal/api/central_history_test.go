package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/central/pgquerylog"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	testclock "k8s.io/utils/clock/testing"
)

type noHistoryPeers struct{ t *testing.T }

func (n noHistoryPeers) Leader(context.Context) (cluster.Peer, error) {
	return cluster.Peer{Name: "node-a", Self: true}, nil
}

func (n noHistoryPeers) Peers(context.Context) ([]cluster.Peer, error) {
	n.t.Error("central history called membership")
	return nil, errors.New("must not query peers")
}

// The actual public router reads a retired node without membership, peers or a
// local log. IDs from that response select central domains on row actions.
func TestCentralHistoricalNodeMounted(t *testing.T) {
	db := centraltest.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	now := time.Now().UTC().Truncate(time.Minute)
	clock := testclock.NewFakeClock(now)
	done := []chan struct{}{}
	for _, node := range []string{"retired", "another"} {
		w, err := sqlitelog.Open(ctx, sqlitelog.Options{Path: filepath.Join(t.TempDir(), sqlitelog.FileName), Node: node, Privacy: querylog.PrivacyFull, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		e := qev(1, now.Add(-10*time.Minute), "10.0.0.1", node+".example", pipeline.StatusBlockedList, "default/ads")
		e.Node = node
		if err := w.Write([]*querylog.Event{e}); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		ship := &pgquerylog.Shipper{DB: db, Local: w, Node: node, Facts: true, Interval: 10 * time.Millisecond, RollupInterval: 10 * time.Millisecond, Now: clock.Now}
		d := make(chan struct{})
		done = append(done, d)
		go func() { defer close(d); _ = ship.Run(ctx) }()
	}
	// Stop the shippers before changing inactivity; all tests read committed data.
	deadline := time.Now().Add(10 * time.Second)
	pool, err := db.Pool()
	if err != nil {
		t.Fatal(err)
	}
	for {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM query`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not ship")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	for _, d := range done {
		<-d
	}
	if err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE source SET last_seen=$1 WHERE node='retired'`, now.Add(-3*time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	central := sqlitelog.NewCentralReader(db.SQL, sqlitelog.CentralOptions{Now: clock.Now})
	h := newHarness(t, func(o *Options) {
		o.Querylog = nil
		o.CentralQuerylog = central
		o.Membership = noHistoryPeers{t}
		o.Now = clock.Now
		o.License = licenseManager(t, 0)
	})
	selector := "scope=node&node=retired"
	paths := []string{"/queries", "/queries/export", "/stats/summary", "/stats/timeseries", "/stats/top?dim=domains", "/stats/top?dim=clients", "/stats/top?dim=lists&blocked=true", "/stats/top?dim=zones", "/stats/upstreams", "/stats/clients/timeseries", "/stats/recent_blocked", "/clients", "/queries/suggestions?field=domain", "/querylog/status"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			resp := h.do(http.MethodGet, "/api/v1"+path+sep+selector, "admin", nil).want(200)
			var scope sqlitelog.Scope
			resp.json(&scope)
			if scope.Scope != "node" || scope.Source != "central" || len(scope.Nodes) != 1 || scope.Nodes[0].Name != "retired" {
				t.Fatalf("incorrect historical scope: %+v", scope)
			}
		})
	}
	var page QueriesBody
	h.do("GET", "/api/v1/queries?"+selector, "operator", nil).want(200).json(&page)
	if len(page.Queries) != 1 || page.Queries[0].Domain != "retired.example" || page.RecordsTotal != 1 {
		t.Fatal("selected history crossed node")
	}
	id := page.Queries[0].ID
	h.do("POST", fmt.Sprintf("/api/v1/queries/%d/actions?%s", id, selector), "admin", map[string]any{"action": "deny"}).want(200)
	var domains List[DomainEntry]
	h.do("GET", "/api/v1/domains?blocked=true", "viewer", nil).want(200).json(&domains)
	if len(domains.Items) != 1 || domains.Items[0].Value != "retired.example" {
		t.Fatal("row action reinterpreted central ID")
	}
	h.do("GET", "/api/v1/queries?scope=node&node=unknown", "operator", nil).want(404)
	h.do("GET", "/api/v1/queries?"+selector, "viewer", nil).want(403)
	h.do("GET", "/api/v1/queries?"+selector, "", nil).want(401)
	h.do("GET", "/api/v1/stats/summary?"+selector+"&window=48h", "viewer", nil).want(402)
	h.do("GET", "/api/v1/queries?scope=cluster&node=retired", "admin", nil).want(400)
	h.do("GET", "/api/v1/queries?scope=invalid&node=retired", "admin", nil).want(400)
	h.do("GET", "/api/v1/queries?scope=node", "admin", nil).want(503)
	away := sqlitelog.NewCentralReader(func() (*sql.DB, error) { return nil, errors.New("private connection failure") }, sqlitelog.CentralOptions{})
	failed := newHarness(t, func(o *Options) { o.Querylog = nil; o.CentralQuerylog = away; o.Membership = noHistoryPeers{t} })
	for _, path := range []string{"/queries", "/stats/summary", "/querylog/status"} {
		r := failed.do("GET", "/api/v1"+path+"?"+selector, "admin", nil).want(503)
		if strings.Contains(r.Body.String(), "private connection failure") {
			t.Fatal("database error leaked")
		}
	}
}
