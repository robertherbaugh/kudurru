package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/ee/auditstore"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestAuditResponsesRedactHistoricalCredentials(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := sqlite.Open(path, sqlite.Options{Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	// Build the exact v5 audit shape before inserting original byte-hashed rows.
	// Fresh current inserts intentionally cannot seed historical credentials.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"chain_version", "authz", "request_id", "client_ip", "user_agent", "request_method", "request_path"} {
		if _, err := legacy.Exec(`ALTER TABLE audit DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.Exec(`UPDATE meta SET value='5' WHERE key='schema_version'; DELETE FROM meta WHERE key='audit_legacy_max_id'`); err != nil {
		t.Fatal(err)
	}
	previous := ""
	token := json.RawMessage(`{"metadata":{"annotations":{"private":"secret-metadata"}},"spec":{"owner":{"kind":"User","name":"admin"},"description":"old deployment check","expiresAt":null,"scopes":["viewer"],"secretHash":"secret-token-hash"}}`)
	user := json.RawMessage(`{"spec":{"passwordHash":"secret-password-hash","totpSecret":"secret-totp-seed"}}`)
	for _, kind := range runtimekinds.Private() {
		doc := user
		if kind == internalkinds.KindAPIToken {
			doc = token
		}
		fields := []string{strconv.FormatInt(time.Unix(1700000000, 0).UnixNano(), 10), "User:admin", "api", strings.ToLower(kind) + ".update", kind, "", "historical", "1", "success", "", string(doc), string(doc), previous}
		hash := sha256.New()
		for _, field := range fields {
			_, _ = hash.Write([]byte(strconv.Itoa(len(field)) + ":" + field))
		}
		next := hex.EncodeToString(hash.Sum(nil))
		if _, err := legacy.Exec(`INSERT INTO audit(ts,actor,manager,action,kind,namespace,name,resource_version,result,reason,before,after,prev_hash,hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, time.Unix(1700000000, 0).UnixNano(), "User:admin", "api", strings.ToLower(kind)+".update", kind, "", "historical", "1", "success", "", string(doc), string(doc), previous, next); err != nil {
			t.Fatal(err)
		}
		previous = next
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = sqlite.Open(path, sqlite.Options{Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

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
	if err != nil || len(rows) != len(runtimekinds.Private()) || !strings.Contains(string(rows[0].Before), "secret-token-hash") {
		t.Fatalf("historical records changed: %v", err)
	}
	source, err := audit.NewStateSource(ctx, st, "legacy-node")
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := source.ReadAfter(ctx, 0, 100)
	if err != nil || len(shipped) != len(rows) {
		t.Fatal("historical source missing")
	}
	for i := range rows {
		if shipped[i].Hash != rows[i].Hash || shipped[i].PrevHash != rows[i].PrevHash || string(shipped[i].Before) != string(rows[i].Before) || string(shipped[i].After) != string(rows[i].After) {
			t.Fatal("shipping rewrote hashed historical source record")
		}
	}
	var result AuditVerification
	h.do(http.MethodGet, "/api/v1/audit/verify", "auditor", nil).want(http.StatusOK).json(&result)
	if !result.Verified {
		t.Fatalf("original chain: %+v", result)
	}
}
