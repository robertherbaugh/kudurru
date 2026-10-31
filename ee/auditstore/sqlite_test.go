package auditstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestSQLiteRecorder(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	r := SQLite{Store: st}
	ctx := context.Background()
	ev := audit.Event{
		Time: time.Now(), Actor: audit.Actor{Kind: "User", Name: "alice"}, Action: "blocking.toggle",
		Resource: audit.Resource{Kind: "blocking"}, Result: audit.ResultSuccess, Authz: "binding",
		Before: audit.Diff(map[string]bool{"blocking": true}), After: audit.Diff(map[string]bool{"blocking": false}),
		ResourceVersion: "123", Request: audit.Request{ID: "req-1", IP: "192.0.2.1", UA: "fixture", Method: "POST", Path: "/api/v1/blocking"},
	}
	if err := r.Record(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(ctx, audit.Event{Time: time.Now(), Actor: audit.Actor{Kind: "User", Name: "bob"}, Action: "zones.delete", Resource: audit.Resource{Kind: "DNSZone", Name: "x"}, Result: audit.ResultDenied}); err != nil {
		t.Fatal(err)
	}
	evs, err := r.Read(ctx, audit.Query{})
	if err != nil || len(evs) != 2 {
		t.Fatalf("read %v %d", err, len(evs))
	}
	got := evs[0]
	if got.Actor.Kind != "User" || got.Actor.Name != "alice" || got.Manager != "api" || got.Hash == "" ||
		!strings.Contains(got.Reason, "authz=binding") || !strings.Contains(got.Reason, "request=req-1") || string(got.After) != `{"blocking":false}` {
		t.Fatalf("row %+v", got)
	}
	if got.Authz != ev.Authz || got.Request != ev.Request || got.ResourceVersion != ev.ResourceVersion {
		t.Fatal("dedicated attribution did not round trip")
	}
	if evs[1].PrevHash != got.Hash {
		t.Fatal("rows not chained")
	}
	evs, _ = r.Read(ctx, audit.Query{Actor: "bob"})
	if len(evs) != 1 || evs[0].Result != audit.ResultDenied {
		t.Fatalf("actor filter %+v", evs)
	}
	newest, _ := r.Read(ctx, audit.Query{Desc: true})
	if len(newest) != 2 || newest[0].Actor.Name != "bob" || newest[1].ID != got.ID {
		t.Fatalf("desc %+v", newest)
	}
	// The actor filter pages through the table: one row per round here.
	evs, _ = r.Read(ctx, audit.Query{Desc: true, Actor: "alice", Limit: 1})
	if len(evs) != 1 || evs[0].ID != got.ID {
		t.Fatalf("desc actor filter %+v", evs)
	}
	if evs, _ = r.Read(ctx, audit.Query{Desc: true, BeforeID: got.ID}); len(evs) != 0 {
		t.Fatalf("desc cursor %+v", evs)
	}
	if err := st.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteRecorderNeverInfersAttributionFromReason(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rec := SQLite{Store: st}
	if err := rec.Record(context.Background(), audit.Event{Action: "fixture.failure", Result: audit.ResultError, Reason: "authz=local request=spoofed ip=192.0.2.10"}); err != nil {
		t.Fatal(err)
	}
	rows, err := rec.Read(context.Background(), audit.Query{})
	if err != nil || len(rows) != 1 {
		t.Fatal("external event missing")
	}
	if rows[0].Authz != "" || rows[0].Request != (audit.Request{}) {
		t.Fatal("attribution inferred from untrusted reason")
	}
}
