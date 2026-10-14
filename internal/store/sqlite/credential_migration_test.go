package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Seed raw schema-v1 rows rather than going through the new token writer.
func legacyCredentialDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate(context.Background(), db, migrations[:1], 1, time.Now); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func insertLegacyCredential(t *testing.T, db *sql.DB, obj store.Object, rv int) {
	t.Helper()
	obj.GetObjectKind().SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(string(store.KindOf(obj))))
	obj.SetResourceVersion(strconv.Itoa(rv))
	obj.SetGeneration(4)
	obj.SetUID(types.UID("legacy-" + obj.GetName()))
	obj.SetAnnotations(map[string]string{v1alpha1.ManagedByAnnotation: "bootstrap:test", "preserved": "yes"})
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(sqlUpsertObject, store.KindOf(obj), obj.GetNamespace(), obj.GetName(), string(obj.GetUID()), obj.GetGeneration(), rv,
		"bootstrap:test", obj.GetCreationTimestamp().UTC().Format(time.RFC3339), 0, specHash(obj), "{}", 0, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value=? WHERE key=?`, strconv.Itoa(rv), metaResourceVersion); err != nil {
		t.Fatal(err)
	}
}

func legacyToken(name string, owner internalv1.TokenOwner, at time.Time) *internalv1.APIToken {
	last := metav1.NewTime(at.Add(time.Minute))
	return &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(at)},
		Spec:   internalv1.APITokenSpec{Owner: owner, SecretHash: strings.Repeat("a", 64), Description: "preserved", Scopes: []string{"viewer"}},
		Status: internalv1.APITokenStatus{LastUsedAt: &last, LastUsedIP: "192.0.2.1"}}
}

func TestCredentialMigrationPreservesIdentityAndRestart(t *testing.T) {
	db, path := legacyCredentialDB(t)
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	sa := &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "dns", CreationTimestamp: metav1.NewTime(at)}, Spec: internalv1.ServiceAccountSpec{BuiltinAdmin: true}}
	u := &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice", CreationTimestamp: metav1.NewTime(at)}}
	insertLegacyCredential(t, db, sa, 1)
	insertLegacyCredential(t, db, u, 2)
	tokens := []*internalv1.APIToken{
		legacyToken("aaaaaaaaaaaa", internalv1.TokenOwner{Kind: "ServiceAccount", Name: "ci", Namespace: "dns", ID: "sa:dns/ci"}, at.Add(time.Second)),
		legacyToken("bbbbbbbbbbbb", internalv1.TokenOwner{Kind: "User", Name: "alice", ID: "local:alice"}, at.Add(time.Second)),
		legacyToken("cccccccccccc", internalv1.TokenOwner{Kind: "ServiceAccount", Name: "gone", Namespace: "dns"}, at.Add(time.Second)),
		legacyToken("dddddddddddd", internalv1.TokenOwner{Kind: "ServiceAccount", Name: "ci", Namespace: "dns"}, at.Add(-time.Second)),
		legacyToken("eeeeeeeeeeee", internalv1.TokenOwner{Kind: "User", Name: "corp:alice", ID: "oidc:corp:alice", Provider: "corp"}, at.Add(time.Second)),
	}
	for i, token := range tokens {
		insertLegacyCredential(t, db, token, i+3)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s := openAt(t, path, internalOptions)
		for i, want := range tokens {
			var got internalv1.APIToken
			if err := s.Get(context.Background(), store.KeyOf(want), &got); err != nil {
				t.Fatal(err)
			}
			uid := ""
			switch i {
			case 0:
				uid = string(sa.UID)
			case 1:
				uid = string(u.UID)
			}
			if got.Spec.Owner.UID != uid {
				t.Fatalf("token %d identity was not safely bound", i)
			}
			got.Spec.Owner.UID = ""
			gotJSON, _ := json.Marshal(&got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("token %d changed fields other than owner UID", i)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	old, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if err := migrate(context.Background(), old, migrations[:2], 2, time.Now); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("older schema accepted upgraded credentials: %v", err)
	}
}

func TestCredentialMigrationTimestampTiesNeedCreationOrder(t *testing.T) {
	for _, tc := range []struct {
		name               string
		accountRV, tokenRV int
		bound              bool
	}{{"legitimate", 1, 2, true}, {"same-second replacement", 3, 2, false}, {"pruned history", 0, 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := legacyCredentialDB(t)
			at := time.Unix(1800000000, 0)
			sa := &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "dns", CreationTimestamp: metav1.NewTime(at)}}
			token := legacyToken("aaaaaaaaaaaa", internalv1.TokenOwner{Kind: "ServiceAccount", Name: "ci", Namespace: "dns"}, at)
			insertLegacyCredential(t, db, sa, 3)
			insertLegacyCredential(t, db, token, 4)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				obj store.Object
				rv  int
			}{{sa, tc.accountRV}, {token, tc.tokenRV}} {
				if item.rv == 0 {
					continue
				}
				e := &AuditEntry{Time: at, Action: strings.ToLower(string(store.KindOf(item.obj))) + ".create", Key: store.KeyOf(item.obj), ResourceVersion: strconv.Itoa(item.rv), Result: AuditSuccess}
				if err := insertAudit(context.Background(), tx, e); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := migrate(context.Background(), db, migrations, store.SchemaVersion, time.Now); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := db.QueryRow(`SELECT data FROM objects WHERE kind='APIToken'`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var got internalv1.APIToken
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if (got.Spec.Owner.UID != "") != tc.bound {
				t.Fatal("unsafe timestamp-tie binding")
			}
		})
	}
}

func TestCredentialMigrationFailureRollsBackEntireUpgrade(t *testing.T) {
	db, _ := legacyCredentialDB(t)
	at := time.Unix(1800000000, 0)
	u := &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice", CreationTimestamp: metav1.NewTime(at)}}
	insertLegacyCredential(t, db, u, 1)
	good := legacyToken("aaaaaaaaaaaa", internalv1.TokenOwner{Kind: "User", Name: "alice", ID: "local:alice"}, at.Add(time.Second))
	insertLegacyCredential(t, db, good, 2)
	bad := legacyToken("zzzzzzzzzzzz", good.Spec.Owner, at.Add(time.Second))
	bad.Spec.SecretHash = "malformed-secret-must-not-appear-in-error"
	insertLegacyCredential(t, db, bad, 3)
	err := migrate(context.Background(), db, migrations, store.SchemaVersion, time.Now)
	if err == nil || strings.Contains(err.Error(), bad.Spec.SecretHash) {
		t.Fatal("expected redacted migration error")
	}
	var version string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='replication_sections'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != "1" || tables != 0 {
		t.Fatal("failed upgrade committed a partial schema")
	}
	var uid string
	if err := db.QueryRow(`SELECT coalesce(json_extract(data,'$.spec.owner.uid'),'') FROM objects WHERE name='aaaaaaaaaaaa'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if uid != "" {
		t.Fatal("failed upgrade changed a credential")
	}
}
