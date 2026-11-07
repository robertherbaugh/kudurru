package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/central/pgquerylog"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	testclock "k8s.io/utils/clock/testing"
)

func TestRollupRecoveryMountedPostgres(t *testing.T) {
	db := centraltest.New(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	fc := testclock.NewFakeClock(now)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w, err := sqlitelog.Open(ctx, sqlitelog.Options{Path: filepath.Join(t.TempDir(), sqlitelog.FileName), Node: "retired", Privacy: querylog.PrivacyFull, Clock: fc})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	event := qev(1, now.Add(-2*time.Hour), "192.0.2.1", "private-history.example", pipeline.StatusForwarded, "")
	if err := w.Write([]*querylog.Event{event}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	source, err := w.SourceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ship := &pgquerylog.Shipper{DB: db, Local: w, Node: "retired", Facts: true, Interval: time.Millisecond, RollupInterval: time.Millisecond, Now: fc.Now}
	done := make(chan struct{})
	go func() { defer close(done); _ = ship.Run(ctx) }()
	pool, err := db.Pool()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM query`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not ship")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	hour := event.Time.Unix() / 3600 * 3600
	rec := pgquerylog.Recovery{DB: db, Now: fc.Now}
	reader := sqlitelog.NewCentralReader(db.SQL, sqlitelog.CentralOptions{Now: fc.Now, Recovery: rec})
	h := newHarness(t, func(o *Options) {
		o.Querylog = nil
		o.CentralQuerylog = reader
		o.Now = fc.Now
		o.Membership = noHistoryPeers{t}
	})
	path := fmt.Sprintf("/api/v1/querylog/rollup-evidence?scope=node&node=retired&source=%s&hour=%d", source, hour)
	var evidence sqlitelog.RollupEvidence
	response := h.do("GET", path, "operator", nil).want(200)
	response.json(&evidence)
	if evidence.SourceID != source || evidence.Source != sqlitelog.SourceCentral || evidence.Scope.Scope != sqlitelog.ScopeNode || evidence.Node != "retired" || len(evidence.Digest) != 64 || strings.Contains(response.Body.String(), "private-history.example") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("receipt scope/privacy/cache contract violated")
	}
	verify := "/api/v1/querylog/rollup-evidence/verify?scope=node&node=retired"
	h.do("POST", verify, "operator", evidence).want(200)
	h.do("GET", path, "viewer", nil).want(403)
	h.do("GET", path, "", nil).want(401)
	h.do("POST", verify, "viewer", evidence).want(403)
	h.do("GET", strings.Replace(path, "node=retired", "node=unknown", 1), "admin", nil).want(404)
	h.do("POST", strings.Replace(verify, "node=retired", "node=unknown", 1), "admin", evidence).want(409)
	h.do("GET", strings.Replace(path, "scope=node", "scope=cluster", 1), "admin", nil).want(400)
	h.do("GET", strings.Replace(path, fmt.Sprintf("hour=%d", hour), fmt.Sprintf("hour=%d", hour+1), 1), "admin", nil).want(400)
	// Unknown legacy attribution remains an explicit read/status/verify gate.
	if err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE rollup_source_state SET legacy_before=$1`, hour+3600)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.do("GET", path, "admin", nil).want(200).json(&evidence)
	if evidence.BlockedReason != "legacy_provenance_required" || !evidence.Partial {
		t.Fatal("legacy gate hidden")
	}
	h.do("POST", verify, "admin", evidence).want(409)
	var status sqlitelog.Status
	h.do("GET", "/api/v1/querylog/status?scope=node&node=retired", "operator", nil).want(200).json(&status)
	if !status.Partial || status.Backfill == nil || len(status.Backfill.Details) == 0 || status.Backfill.Details[0].State != "legacy_provenance_required" {
		t.Fatal("missing scoped operational status")
	}
	// Existing long-term licensing applies before acquiring recovery evidence.
	licensed := newHarness(t, func(o *Options) { o.CentralQuerylog = reader; o.Now = fc.Now; o.License = licenseManager(t, 0) })
	old := strings.Replace(path, fmt.Sprintf("hour=%d", hour), fmt.Sprintf("hour=%d", hour-48*3600), 1)
	licensed.do("GET", old, "admin", nil).want(402)
	// Central outage is redacted and never falls through to a local log/peer.
	unavailable := sqlitelog.NewCentralReader(func() (*sql.DB, error) { return nil, errors.New("sensitive database failure") }, sqlitelog.CentralOptions{Recovery: pgquerylog.Recovery{DB: db, Now: fc.Now}})
	down := newHarness(t, func(o *Options) { o.CentralQuerylog = unavailable; o.Now = fc.Now; o.Membership = noHistoryPeers{t} })
	down.do("GET", "/api/v1/querylog/status?scope=node&node=retired", "admin", nil).want(503)
	missing := newHarness(t, func(o *Options) { o.Now = fc.Now; o.Membership = noHistoryPeers{t} })
	missing.do("GET", path, "admin", nil).want(503)
}
