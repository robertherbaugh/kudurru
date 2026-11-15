package sqlite

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPublicIAMMigrationPreservesIdentityAndRestart(t *testing.T) {
	db, path := legacyCredentialDB(t)
	at := metav1.NewTime(time.Unix(1800000000, 0).UTC())
	idp := &publicv1.IdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "corp", CreationTimestamp: at},
		Spec:       publicv1.IdentityProviderSpec{Issuer: "https://sso.example", ClientID: "test", ClientSecretRef: &publicv1.CredentialReference{File: "/host/secret"}},
		Status:     publicv1.IdentityProviderStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Offline", Message: "waiting", LastTransitionTime: at}}},
	}
	insertLegacyCredential(t, db, idp, 1)
	if _, err := db.Exec(`UPDATE objects SET data=json_set(json_remove(data,'$.spec.clientSecretRef'),'$.spec.clientSecretFile','/host/secret') WHERE kind='IdentityProvider'`); err != nil {
		t.Fatal(err)
	}
	binding := &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "editors", Namespace: "dns", CreationTimestamp: at}, Spec: publicv1.RoleBindingSpec{Role: "operator", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "staff"}}}}
	insertLegacyCredential(t, db, binding, 2)
	if _, err := db.Exec(`UPDATE objects SET data=json_set(data,'$.spec.scope.kind','') WHERE kind='RoleBinding'`); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path, internalOptions)
	var got publicv1.IdentityProvider
	if err := s.Get(t.Context(), store.Key{Kind: "IdentityProvider", Name: "corp"}, &got); err != nil {
		t.Fatal(err)
	}
	if !sameIAMJSON(idp, &got) {
		t.Fatal("IAM conversion changed configuration, status or identity")
	}
	var rb publicv1.RoleBinding
	if err := s.Get(t.Context(), store.Key{Kind: "RoleBinding", Namespace: "dns", Name: "editors"}, &rb); err != nil {
		t.Fatal(err)
	}
	if !sameIAMJSON(binding, &rb) {
		t.Fatal("IAM conversion changed default binding scope or ownership")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path, internalOptions)
	var restarted publicv1.IdentityProvider
	if err := s.Get(t.Context(), store.KeyOf(&got), &restarted); err != nil {
		t.Fatal(err)
	}
	if !sameIAMJSON(&got, &restarted) {
		t.Fatal("restart changed converted identity")
	}
	var raw string
	if err := db.QueryRow(`SELECT data FROM objects WHERE kind='IdentityProvider'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "clientSecretFile") {
		t.Fatal("legacy file capability remains in stored public spec")
	}
	if err := migrate(t.Context(), db, migrations[:4], 4, time.Now); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatal("older binary accepted public IAM schema", err)
	}
}

func sameIAMJSON(a, b any) bool {
	x, xe := json.Marshal(a)
	y, ye := json.Marshal(b)
	return xe == nil && ye == nil && string(x) == string(y)
}

func TestPublicIAMMigrationRollsBackInvalidObjects(t *testing.T) {
	db, _ := legacyCredentialDB(t)
	idp := &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp", CreationTimestamp: metav1.NewTime(time.Now())}, Spec: publicv1.IdentityProviderSpec{Issuer: "https://sso.example", ClientID: "test", ClientSecretRef: &publicv1.CredentialReference{File: "/host/secret"}}}
	insertLegacyCredential(t, db, idp, 1)
	if _, err := db.Exec(`UPDATE objects SET data=json_set(json_remove(data,'$.spec.clientSecretRef'),'$.spec.clientSecretFile','/host/secret') WHERE kind='IdentityProvider'`); err != nil {
		t.Fatal(err)
	}
	invalid := &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "dns", CreationTimestamp: metav1.NewTime(time.Now())}, Spec: publicv1.RoleBindingSpec{Role: "operator"}}
	insertLegacyCredential(t, db, invalid, 2)
	if err := migrate(t.Context(), db, migrations[:4], 4, time.Now); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.QueryRow(`SELECT data FROM objects WHERE kind='IdentityProvider'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := migrate(t.Context(), db, migrations, store.SchemaVersion, time.Now); err == nil {
		t.Fatal("invalid public IAM migration committed")
	}
	var version, after string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT data FROM objects WHERE kind='IdentityProvider'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if version != "4" || before != after {
		t.Fatal("failed conversion partially changed objects or schema version")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(after), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["spec"].(map[string]any)["clientSecretFile"] != "/host/secret" {
		t.Fatal("rollback lost old reference")
	}
}
