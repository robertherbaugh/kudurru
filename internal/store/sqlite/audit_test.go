package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestAuditRowsPerMutation(t *testing.T) {
	ctx := WithActor(context.Background(), "user:alice")
	s := openTemp(t)

	z := mustApply(t, s, zone("home", "home.example."), store.ManagerAPI).(*v1alpha1.DNSZone)
	z.Status.Serial = 7
	if err := s.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	upd := zone("home", "home.example.")
	upd.Spec.DefaultTTL = 60
	if _, err := s.Apply(ctx, upd, store.ApplyOptions{Manager: store.ManagerFile("z.yaml"), Force: true}); err != nil {
		t.Fatal(err)
	}
	// An identical re-apply writes nothing, so it is not audited.
	if _, err := s.Apply(ctx, upd, store.ApplyOptions{Manager: store.ManagerFile("z.yaml")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerFile("z.yaml")}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ReadAudit(context.Background(), AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ actor, manager, action, result, reason string }
	want := []row{
		{"api", "api", "dnszone.create", AuditSuccess, ""},
		{"user:alice", "api", "dnszone.status", AuditSuccess, ""},
		{"user:alice", "file:z.yaml", "dnszone.update", AuditSuccess, `force: took over from "api"`},
		{"user:alice", "file:z.yaml", "dnszone.delete", AuditSuccess, ""},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d audit rows, want %d: %+v", len(entries), len(want), entries)
	}
	for i, e := range entries {
		got := row{e.Actor, e.Manager, e.Action, e.Result, e.Reason}
		if got != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got, want[i])
		}
		if e.Key != (store.Key{Kind: "DNSZone", Name: "home"}) || e.ResourceVersion == "" || e.Time.IsZero() {
			t.Errorf("row %d identity: %+v", i, e)
		}
	}
	if entries[0].Before != nil || entries[0].After == nil {
		t.Error("create has after only")
	}
	if entries[3].Before == nil || entries[3].After != nil {
		t.Error("delete has before only")
	}
	var after v1alpha1.DNSZone
	if err := json.Unmarshal(entries[1].After, &after); err != nil || after.Status.Serial != 7 {
		t.Fatalf("status row must hold the new document: %v %+v", err, after.Status)
	}
	if entries[0].ResourceVersion != z.ResourceVersion {
		t.Errorf("audit resourceVersion %s, object %s", entries[0].ResourceVersion, z.ResourceVersion)
	}
	if err := s.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Filters and the cursor.
	if got, _ := s.ReadAudit(ctx, AuditQuery{AfterID: entries[1].ID, Limit: 1}); len(got) != 1 || got[0].ID != entries[2].ID {
		t.Fatalf("cursor: %+v", got)
	}
	if got, _ := s.ReadAudit(ctx, AuditQuery{Desc: true, BeforeID: entries[2].ID, Limit: 1}); len(got) != 1 || got[0].ID != entries[1].ID {
		t.Fatalf("desc cursor: %+v", got)
	}
	if got, _ := s.ReadAudit(ctx, AuditQuery{Kind: "Policy"}); len(got) != 0 {
		t.Fatalf("kind filter: %d rows", len(got))
	}
	if got, _ := s.ReadAudit(ctx, AuditQuery{Since: time.Now().Add(time.Hour)}); len(got) != 0 {
		t.Fatalf("since filter: %d rows", len(got))
	}

	// Tampering with a row breaks the chain.
	if _, err := s.w.Exec(`UPDATE audit SET actor = 'mallory' WHERE id = ?`, entries[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyAudit(context.Background()); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("tampering must be detected, got %v", err)
	}
}

func TestAuditAtomicWithMutation(t *testing.T) {
	s := openTemp(t)
	mustApply(t, s, zone("a", "a."), store.ManagerAPI)
	// Make the audit insert fail: the mutation must roll back with it.
	if _, err := s.w.Exec(`CREATE TRIGGER no_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	rv := s.ResourceVersion()
	if _, err := s.Apply(context.Background(), zone("b", "b."), store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Fatal("expected the write to fail with the audit insert")
	}
	var z v1alpha1.DNSZone
	if err := s.Get(context.Background(), store.Key{Kind: "DNSZone", Name: "b"}, &z); err == nil {
		t.Fatal("the object must not exist without its audit row")
	}
	if n := countRows(t, s, "changes"); n != 1 {
		t.Fatalf("change feed has %d rows, want 1", n)
	}
	if s.ResourceVersion() != rv {
		t.Fatal("resourceVersion moved for a rolled-back write")
	}
}

func TestAuditDiffCap(t *testing.T) {
	small := json.RawMessage(`{"a":1}`)
	if string(capDiff(small)) != string(small) {
		t.Fatal("small documents are stored verbatim")
	}
	big := json.RawMessage(`{"x":"` + strings.Repeat("é\"", 5000) + `"}`)
	got := capDiff(big)
	if len(got) > AuditDiffCap {
		t.Fatalf("capped document is %d bytes", len(got))
	}
	var sum struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		SHA256    string `json:"sha256"`
		Prefix    string `json:"prefix"`
	}
	if err := json.Unmarshal(got, &sum); err != nil || !sum.Truncated || sum.Bytes != len(big) || len(sum.SHA256) != 64 || !strings.HasPrefix(string(big), sum.Prefix) || sum.Prefix == "" {
		t.Fatalf("summary: %v %+v", err, sum)
	}

	// A big object through the store.
	s := openTemp(t)
	rs := recordSet("apps", "big", nil)
	for i := 0; i < 600; i++ {
		rs.Spec.Endpoints[0].Targets = append(rs.Spec.Endpoints[0].Targets, "10.0.1."+strings.Repeat("1", 1+i%3))
	}
	rs.Annotations = map[string]string{"note": strings.Repeat("n", 5000)}
	mustApply(t, s, rs, store.ManagerAPI)
	entries, err := s.ReadAudit(context.Background(), AuditQuery{Name: "big"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %d", err, len(entries))
	}
	if len(entries[0].After) > AuditDiffCap || !strings.Contains(string(entries[0].After), `"truncated":true`) {
		t.Fatalf("store did not cap the audit document: %d bytes", len(entries[0].After))
	}
}

func TestRecordAudit(t *testing.T) {
	s := openTemp(t)
	mustApply(t, s, zone("a", "a."), store.ManagerAPI)
	e, err := s.RecordAudit(WithActor(context.Background(), "user:bob"), AuditEntry{Action: "auth.login", Result: AuditDenied, Reason: "bad password"})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == 0 || e.Actor != "user:bob" || e.PrevHash == "" || e.Hash == "" {
		t.Fatalf("recorded: %+v", e)
	}
	if _, err := s.RecordAudit(context.Background(), AuditEntry{}); err == nil {
		t.Fatal("an entry without Action must be rejected")
	}
	if err := s.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPruneAuditKeepsTheChainVerifiable(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	s := openTemp(t, func(o *Options) { o.Now = clock })

	mustApply(t, s, zone("old1", "old1."), store.ManagerAPI)
	mustApply(t, s, zone("old2", "old2."), store.ManagerAPI)
	advance(401 * 24 * time.Hour)
	cutoff := clock().Add(-400 * 24 * time.Hour)
	mustApply(t, s, zone("new1", "new1."), store.ManagerAPI)
	mustApply(t, s, zone("new2", "new2."), store.ManagerAPI)

	n, err := s.PruneAudit(ctx, cutoff)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d rows (%v), want 2", n, err)
	}
	entries, err := s.ReadAudit(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Key.Name != "new1" || entries[0].PrevHash == "" {
		t.Fatalf("after prune: %+v", entries)
	}
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatalf("a pruned chain must still verify: %v", err)
	}
	// The objects themselves are untouched.
	if list, err := s.List(ctx, "DNSZone", store.ListOptions{}); err != nil || len(list.Items) != 4 {
		t.Fatalf("prune touched objects: %d %v", len(list.Items), err)
	}
}
