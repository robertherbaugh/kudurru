package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/auditstore"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestAuditResponsesRedactHistoricalCredentials(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	token := json.RawMessage(`{"metadata":{"annotations":{"private":"secret-metadata"}},"spec":{"owner":{"kind":"User","name":"admin"},"description":"old deployment check","expiresAt":null,"scopes":["viewer"],"secretHash":"secret-token-hash"}}`)
	user := json.RawMessage(`{"spec":{"passwordHash":"secret-password-hash","totpSecret":"secret-totp-seed"}}`)
	for _, kind := range internalkinds.Kinds {
		doc := user
		if kind == internalkinds.KindAPIToken {
			doc = token
		}
		if _, err := st.RecordAudit(ctx, sqlite.AuditEntry{Action: strings.ToLower(kind) + ".update", Key: store.Key{Kind: store.Kind(kind), Name: "historical"}, Before: doc, After: doc}); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, func(o *Options) { o.AuditReader = auditstore.SQLite{Store: st} })
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		user := "auditor"
		if path == "/api/v1/audit/export" {
			user = "admin"
		}
		res := h.do(http.MethodGet, path, user, nil).want(http.StatusOK)
		for _, secret := range []string{"secret-token-hash", "secret-password-hash", "secret-totp-seed", "secret-metadata", "secretHash", "passwordHash", "totpSecret"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatalf("%s exposed %s", path, secret)
			}
		}
		if !strings.Contains(res.Body.String(), "old deployment check") || !strings.Contains(res.Body.String(), "viewer") {
			t.Fatalf("%s lost safe token details", path)
		}
	}
	// Response redaction never rewrites retained records or invalidates the chain.
	rows, err := st.ReadAudit(ctx, sqlite.AuditQuery{})
	if err != nil || len(rows) != len(internalkinds.Kinds) || !strings.Contains(string(rows[0].Before), "secret-token-hash") {
		t.Fatalf("historical records changed: %v", err)
	}
	var result AuditVerification
	h.do(http.MethodGet, "/api/v1/audit/verify", "auditor", nil).want(http.StatusOK).json(&result)
	if !result.Verified {
		t.Fatalf("original chain: %+v", result)
	}
}
