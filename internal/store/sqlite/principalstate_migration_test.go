package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPrincipalStateMigrationPreservesPrivateAuthority(t *testing.T) {
	db, path := legacyCredentialDB(t)
	at := time.Unix(1800000000, 0).UTC()
	sa := &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "dns", CreationTimestamp: metav1.NewTime(at)}}
	insertLegacyCredential(t, db, sa, 1)
	if _, err := db.Exec(`UPDATE objects SET data=json_set(data,'$.spec.builtinAdmin',json('true')) WHERE kind='ServiceAccount'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path, internalOptions)
	owner := internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: sa.Name, Namespace: sa.Namespace, UID: string(sa.UID), ID: "sa:dns/bootstrap", Provider: "kudurru"}
	var state internalv1.PrincipalState
	if err := s.Get(context.Background(), store.Key{Kind: internalv1.KindPrincipalState, Name: internalv1.PrincipalStateName(owner)}, &state); err != nil {
		t.Fatal(err)
	}
	if state.Spec.Owner != owner || !state.Spec.BuiltinAdmin || state.UID == "" || state.Validate() != nil {
		t.Fatal("legacy bootstrap authority lost its private UID binding")
	}
	var raw string
	if err := s.w.QueryRow(`SELECT data FROM objects WHERE kind='ServiceAccount'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "builtinAdmin") {
		t.Fatal("public account still carries privileged field")
	}
	var got internalv1.ServiceAccount
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != sa.UID || got.ResourceVersion != sa.ResourceVersion || got.Generation != sa.Generation || store.ManagedBy(&got) != store.ManagedBy(sa) {
		t.Fatal("service account migration changed identity or ownership")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path, internalOptions)
	var restarted internalv1.PrincipalState
	if err := s.Get(context.Background(), store.KeyOf(&state), &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.UID != state.UID || restarted.Spec != state.Spec {
		t.Fatal("restart recreated private authority")
	}
}

func TestPrincipalStateMigrationConflictRollsBack(t *testing.T) {
	db, _ := legacyCredentialDB(t)
	at := time.Unix(1800000000, 0).UTC()
	sa := &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "dns", CreationTimestamp: metav1.NewTime(at)}}
	insertLegacyCredential(t, db, sa, 1)
	if _, err := db.Exec(`UPDATE objects SET data=json_set(data,'$.spec.builtinAdmin',json('true')) WHERE kind='ServiceAccount'`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), db, migrations[:3], 3, time.Now); err != nil {
		t.Fatal(err)
	}
	owner := internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: sa.Name, Namespace: sa.Namespace, UID: string(sa.UID), ID: "sa:dns/bootstrap", Provider: "kudurru"}
	state := &internalv1.PrincipalState{ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(owner), CreationTimestamp: metav1.NewTime(at)}, Spec: internalv1.PrincipalStateSpec{Owner: owner}}
	insertLegacyCredential(t, db, state, 2)
	if err := migrate(context.Background(), db, migrations, store.SchemaVersion, time.Now); err == nil {
		t.Fatal("conflicting private trust was overwritten")
	}
	var version, rv string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='resource_version'`).Scan(&rv); err != nil {
		t.Fatal(err)
	}
	if version != "3" || rv != "2" {
		t.Fatal("failed private authority migration committed metadata")
	}
	var privileged bool
	if err := db.QueryRow(`SELECT json_extract(data,'$.spec.builtinAdmin') FROM objects WHERE kind='ServiceAccount'`).Scan(&privileged); err != nil {
		t.Fatal(err)
	}
	if !privileged {
		t.Fatal("failed migration removed old private authority")
	}
}
