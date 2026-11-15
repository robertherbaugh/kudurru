package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/auditstore"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestSQLiteAPIAuditAttributionAndSafeEvidence(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	recorder := auditstore.SQLite{Store: st}
	h := newHarness(t, func(o *Options) { o.Store = st; o.Audit = recorder; o.AuditReader = recorder })
	body := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "audited", "annotations": map[string]string{"private": "fixture-credential"}}, "spec": map[string]any{"zoneName": "audit.example.", "type": "primary"}}
	path := "/api/v1/objects/dnszones/audited"
	h.do(http.MethodPut, path, "admin", body, "User-Agent", "attribution-fixture").want(http.StatusCreated)
	rows, err := st.ReadAudit(context.Background(), sqlite.AuditQuery{Name: "audited"})
	if err != nil || len(rows) != 1 {
		t.Fatal("mounted router did not audit ordinary store write")
	}
	e := rows[0]
	if e.Authz != "binding" || e.RequestID == "" || e.ClientIP != "192.0.2.10" || e.UserAgent != "attribution-fixture" || e.RequestMethod != http.MethodPut || e.RequestPath != path || e.Actor != "User:admin" {
		t.Fatal("mounted API attribution missing or incorrect")
	}
	if strings.Contains(string(e.After), "fixture-credential") || !strings.Contains(string(e.After), "audit.example.") {
		t.Fatal("public evidence redaction incorrect")
	}
	body["spec"].(map[string]any)["defaultTTL"] = 60
	h.do(http.MethodPut, path, "admin", body).want(http.StatusOK)
	h.do(http.MethodDelete, path, "viewer", nil, "User-Agent", "denied-fixture").want(http.StatusForbidden)
	rows, err = st.ReadAudit(context.Background(), sqlite.AuditQuery{})
	if err != nil || len(rows) != 3 {
		t.Fatal("expected create, update and denied attempt")
	}
	if len(rows[1].Before) == 0 || !strings.Contains(string(rows[1].After), `"defaultTTL":60`) {
		t.Fatal("safe before/after evidence lost")
	}
	denied := rows[2]
	if denied.Result != sqlite.AuditDenied || denied.RequestMethod != http.MethodDelete || denied.RequestPath != path || denied.RequestID == "" || denied.UserAgent != "denied-fixture" || denied.Actor != "User:viewer" || denied.Authz == "" {
		t.Fatal("direct denial attribution lost")
	}
	if err := st.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	var page AuditPage
	h.do(http.MethodGet, "/api/v1/audit", "auditor", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 3 {
		t.Fatal("audit response missing stored rows")
	}
	found := false
	for _, ev := range page.Items {
		if ev.ID == e.ID {
			found = ev.Authz == e.Authz && ev.Request.ID == e.RequestID && ev.Request.Path == e.RequestPath && ev.ResourceVersion == e.ResourceVersion
		}
	}
	if !found {
		t.Fatal("attribution lost in public audit conversion")
	}
}
