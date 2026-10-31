package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	iam "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auditdocument"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate/zonestatetest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// insertLegacyAudit deliberately uses the pre-attribution SQL and encoding.
// Never use the current insertion path to simulate a historical database.
func insertLegacyAudit(ctx context.Context, tx *sql.Tx, e *AuditEntry) error {
	if err := tx.QueryRowContext(ctx, `SELECT hash FROM audit ORDER BY id DESC LIMIT 1`).Scan(&e.PrevHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	h := sha256.New()
	for _, f := range []string{strconv.FormatInt(e.Time.UnixNano(), 10), e.Actor, e.Manager, e.Action, string(e.Key.Kind), e.Key.Namespace, e.Key.Name, e.ResourceVersion, e.Result, e.Reason, string(e.Before), string(e.After), e.PrevHash} {
		_, _ = h.Write([]byte(strconv.Itoa(len(f)) + ":" + f))
	}
	e.Hash = hex.EncodeToString(h.Sum(nil))
	_, err := tx.ExecContext(ctx, `INSERT INTO audit (ts,actor,manager,action,kind,namespace,name,resource_version,result,reason,before,after,prev_hash,hash) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.Time.UnixNano(), e.Actor, e.Manager, e.Action, string(e.Key.Kind), e.Key.Namespace, e.Key.Name, e.ResourceVersion, e.Result, e.Reason, nullJSON(e.Before), nullJSON(e.After), e.PrevHash, e.Hash)
	return err
}

func TestAuditAtRestDocuments(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	secret := "fixture-credential-never-retain" //nolint:gosec // G101: synthetic audit redaction sentinel, not a usable credential.
	public := json.RawMessage(`{"metadata":{"name":"safe","labels":{"team":"dns"},"annotations":{"note":"` + secret + `"}},"spec":{"serial":18446744073709551615,"endpoints":[{"dnsName":"safe.example.","recordType":"A","targets":["192.0.2.5"]}],"secretRef":{"name":"credential"},"password":"` + secret + `","nested":[{"access_token":"` + secret + `"}]}}`)
	for _, result := range []string{AuditSuccess, AuditDenied, AuditError} {
		e, err := s.RecordAudit(ctx, AuditEntry{Action: "dnsrecordset.update", Key: store.Key{Kind: "DNSRecordSet"}, Result: result, Before: public, After: public})
		if err != nil {
			t.Fatal(err)
		}
		var before, after string
		if err := s.r.QueryRow(`SELECT before,after FROM audit WHERE id=?`, e.ID).Scan(&before, &after); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{before, after} {
			if strings.Contains(raw, secret) || strings.Contains(raw, "password") || strings.Contains(raw, "access_token") {
				t.Fatal("credential retained in raw SQLite audit document")
			}
			for _, evidence := range []string{"safe.example.", "192.0.2.5", "18446744073709551615", "secretRef", "credential", "team"} {
				if !strings.Contains(raw, evidence) {
					t.Fatal("safe record evidence removed")
				}
			}
		}
	}
	for _, kind := range runtimekinds.Private() {
		e, err := s.RecordAudit(ctx, AuditEntry{Action: strings.ToLower(kind) + ".update", Key: store.Key{Kind: store.Kind(kind)}, Before: public, After: public})
		if err != nil {
			t.Fatal(err)
		}
		var before, after sql.NullString
		if err := s.r.QueryRow(`SELECT before,after FROM audit WHERE id=?`, e.ID).Scan(&before, &after); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(before.String, secret) || strings.Contains(after.String, secret) {
			t.Fatalf("private kind %s leaked a credential", kind)
		}
		if kind != "APIToken" && (before.Valid || after.Valid) {
			t.Fatalf("private kind %s retained documents", kind)
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`{"password":"` + secret + `"`), json.RawMessage(`"` + secret + `"`), json.RawMessage(`{"truncated":true,"prefix":"` + secret + `","sha256":"credential-hash","bytes":9000}`)} {
		e, err := s.RecordAudit(ctx, AuditEntry{Action: "runtime.failed", Before: raw, After: raw})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(e.Before, []byte(secret)) || bytes.Contains(e.After, []byte(secret)) || bytes.Contains(e.After, []byte("credential-hash")) {
			t.Fatal("unsafe malformed or historical capped document retained")
		}
	}
	large := json.RawMessage(`{"password":"` + strings.Repeat(secret, 500) + `","targets":["` + strings.Repeat("safe-data-", 1000) + `"]}`)
	e, err := s.RecordAudit(ctx, AuditEntry{Action: "runtime.update", Before: large, After: large})
	if err != nil {
		t.Fatal(err)
	}
	safe := auditdocument.Redact("", large)
	sum := sha256.Sum256(safe)
	var marker struct {
		Truncated bool
		SHA256    string
		Prefix    string
		Bytes     int
	}
	if err := json.Unmarshal(e.After, &marker); err != nil {
		t.Fatal(err)
	}
	if !marker.Truncated || marker.SHA256 != hex.EncodeToString(sum[:]) || marker.Bytes != len(safe) || !bytes.HasPrefix(safe, []byte(marker.Prefix)) || len(e.After) > AuditDiffCap || bytes.Contains(e.After, []byte(secret)) {
		t.Fatal("cap not derived from safe document")
	}
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuditAttributionAndFormatTampering(t *testing.T) {
	columns := []string{"authz", "request_id", "client_ip", "user_agent", "request_method", "request_path", "chain_version"}
	for _, column := range columns {
		t.Run(column, func(t *testing.T) {
			s := openTemp(t)
			authz := "before-authorization"
			ctx := store.WithAuditRequest(context.Background(), store.AuditRequest{ID: "request-1", IP: "192.0.2.5", UA: "fixture-agent", Method: "POST", Path: "/api/v1/dnszones"}, func(context.Context) string { return authz })
			authz = "binding"
			if _, err := s.Apply(ctx, zone("safe", "safe.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			rows, err := s.ReadAudit(ctx, AuditQuery{})
			if err != nil || len(rows) != 1 {
				t.Fatal("missing ordinary write audit")
			}
			e := rows[0]
			if e.ChainVersion != 2 || e.Authz != "binding" || e.RequestID != "request-1" || e.ClientIP != "192.0.2.5" || e.UserAgent != "fixture-agent" || e.RequestMethod != "POST" || e.RequestPath != "/api/v1/dnszones" {
				t.Fatal("ordinary write lost direct attribution")
			}
			if err := s.VerifyAudit(ctx); err != nil {
				t.Fatal(err)
			}
			value := any("modified")
			if column == "chain_version" {
				value = 1
			}
			// #nosec G202 -- column comes only from the fixed attribution-column test list above.
			if _, err := s.w.Exec(`UPDATE audit SET `+column+`=?`, value); err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyAudit(ctx); !errors.Is(err, store.ErrAuditIntegrity) {
				t.Fatal("attribution or format tampering accepted")
			}
			if column == "chain_version" {
				e.ChainVersion = 1
				if _, err := s.w.Exec(`UPDATE audit SET hash=?`, auditHash(&e)); err != nil {
					t.Fatal(err)
				}
				if err := s.VerifyAudit(ctx); !errors.Is(err, store.ErrAuditIntegrity) {
					t.Fatal("recomputed legacy downgrade accepted")
				}
			}
		})
	}
}

func TestAuditLegacyUpgradeRestartPrune(t *testing.T) {
	ctx := context.Background()
	db, path := legacyCredentialDB(t)
	old := AuditEntry{Time: time.Unix(1700000000, 123), Actor: "User:admin", Manager: "api", Action: "localuser.update", Key: store.Key{Kind: "LocalUser", Name: "admin"}, ResourceVersion: "7", Result: AuditSuccess, Reason: "request=historical", Before: json.RawMessage(`{"spec":{"passwordHash":"fixture-old-hash"}}`), After: json.RawMessage(`{"spec":{"passwordHash":"fixture-next-hash"}}`)}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertLegacyAudit(ctx, tx, &old); err != nil {
		t.Fatal(err)
	}
	if old.Hash != "469a08adb1d09e7a9231e3cf3e1505b078c4e7ede728869a8e31d53cf1abfe0a" {
		t.Fatal("historical encoding differs from fixed legacy hash")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	rows, err := s.ReadAudit(ctx, AuditQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatal("legacy row missing")
	}
	if rows[0].Hash != old.Hash || rows[0].PrevHash != old.PrevHash || !bytes.Equal(rows[0].Before, old.Before) || !bytes.Equal(rows[0].After, old.After) || rows[0].ChainVersion != 1 || rows[0].RequestID != "" {
		t.Fatal("upgrade rewrote historical evidence or inferred attribution")
	}
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.RecordAudit(ctx, AuditEntry{Time: old.Time.Add(time.Hour), Action: "runtime.test", RequestID: "new-request"})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.PrevHash != old.Hash || fresh.ChainVersion != 2 {
		t.Fatal("mixed chain transition lost")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneAudit(ctx, old.Time.Add(time.Minute)); err != nil || n != 1 {
		t.Fatal("legacy retention failed")
	}
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAudit(ctx, AuditEntry{Action: "runtime.test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	oldBinary, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oldBinary.Close() }()
	if err := migrate(ctx, oldBinary, migrations[:5], 5, time.Now); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatal("old schema accepted new chain")
	}
}

func TestAuditRejectsInvalidFormatsOnReopen(t *testing.T) {
	for _, change := range []string{
		`UPDATE audit SET chain_version=1`,
		`PRAGMA ignore_check_constraints=ON; UPDATE audit SET chain_version=3`,
		`DELETE FROM meta WHERE key='audit_legacy_max_id'`,
		`UPDATE meta SET value='invalid' WHERE key='audit_legacy_max_id'`,
		`UPDATE meta SET value='-1' WHERE key='audit_legacy_max_id'`,
		`UPDATE meta SET value='1' WHERE key='audit_legacy_max_id'`,
	} {
		t.Run(strconv.Itoa(len(change))+change[:6], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			s := openAt(t, path)
			if _, err := s.RecordAudit(context.Background(), AuditEntry{Action: "runtime.test"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.w.Exec(change); err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyAudit(context.Background()); !errors.Is(err, store.ErrAuditIntegrity) {
				t.Fatal("invalid format accepted by verifier")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, testOptions())
			if err == nil {
				_ = reopened.Close()
				t.Fatal("invalid format accepted on open")
			}
			if !errors.Is(err, store.ErrAuditIntegrity) {
				t.Fatal("invalid format error not explicit")
			}
		})
	}
}

func TestAuditPrivateCurrentMutations(t *testing.T) {
	s := openTemp(t, func(o *Options) {
		scheme, err := runtimekinds.NewScheme()
		if err != nil {
			t.Fatal(err)
		}
		o.Scheme = scheme
		for _, kind := range runtimekinds.Private() {
			o.InternalKinds = append(o.InternalKinds, store.Kind(kind))
		}
	})
	//nolint:gosec // G101: deliberately unusable synthetic hash for the persistence fixture.
	user := &iam.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "admin"}, Spec: iam.LocalUserSpec{PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"}}
	out, err := s.Apply(t.Context(), user, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	user = out.(*iam.LocalUser)
	user.Spec.DisplayName = "safe account label"
	if _, err := s.Apply(t.Context(), user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	repo, _ := zoneStateRepository(t, s)
	_ = zonestatetest.Initialize(t, repo, zoneStateID(s, "at-rest"))
	data := []byte(`{"opaque":"fixture-private-provider-material"}`)
	owner := integration.Owner{ClusterID: s.Authority().Term.ClusterID, Namespace: "default", Name: "provider", UID: "provider-original"}
	digest := integration.Digest(data)
	chunk := &integration.IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: integration.SourceChunkName(owner, 1, 0, digest)}, Spec: integration.IntegrationSourceChunkSpec{FormatVersion: 1, Owner: owner, Sequence: 1, Digest: digest, Data: data}}
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		_, err := tx.Apply(ctx, chunk, store.ApplyOptions{Manager: "integration"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.r.Query(`SELECT kind,before,after FROM audit`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var kind string
		var before, after sql.NullString
		if err := rows.Scan(&kind, &before, &after); err != nil {
			t.Fatal(err)
		}
		if before.Valid || after.Valid {
			t.Fatal("private current mutation retained documents")
		}
		seen[kind] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"LocalUser", "ZoneState", "ZoneChunk", "IntegrationSourceChunk"} {
		if !seen[kind] {
			t.Fatalf("missing private mutation audit for %s", kind)
		}
	}
	if err := s.VerifyAudit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAuditLegacyAttributionCannotBeInjected(t *testing.T) {
	for _, column := range []string{"authz", "request_id", "client_ip", "user_agent", "request_method", "request_path"} {
		t.Run(column, func(t *testing.T) {
			db, path := legacyCredentialDB(t)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			e := &AuditEntry{Time: time.Unix(1700000000, 0), Action: "fixture.legacy", Result: AuditSuccess}
			if err := insertLegacyAudit(t.Context(), tx, e); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, path)
			// #nosec G202 -- column comes only from the fixed attribution-column test list above.
			if _, err := s.w.Exec(`UPDATE audit SET ` + column + `='injected'`); err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyAudit(t.Context()); !errors.Is(err, store.ErrAuditIntegrity) {
				t.Fatal("unauthenticated legacy attribution accepted")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, testOptions())
			if err == nil {
				_ = reopened.Close()
				t.Fatal("legacy attribution accepted on reopen")
			}
			if !errors.Is(err, store.ErrAuditIntegrity) {
				t.Fatal("legacy attribution failure not explicit")
			}
		})
	}
}

func TestAuditFormatBoundaryCancellationIsNotTampering(t *testing.T) {
	s := openTemp(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := auditLegacyBoundary(ctx, s.r); !errors.Is(err, context.Canceled) || errors.Is(err, store.ErrAuditIntegrity) {
		t.Fatal("canceled format read misclassified as tampering")
	}
}
