package sqlite

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func internalOptions(o *Options) {
	s, err := internalkinds.NewScheme()
	if err != nil {
		panic(err)
	}
	o.Scheme = s
	o.InternalKinds = make([]store.Kind, 0, len(internalkinds.Kinds))
	for _, k := range internalkinds.Kinds {
		o.InternalKinds = append(o.InternalKinds, store.Kind(k))
	}
}

func TestInternalKindsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, internalOptions)

	u := &internalkinds.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "admin"}, Spec: internalkinds.LocalUserSpec{ //nolint:gosec // G101: a dummy hash
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA", MustChangePassword: true, BuiltinAdmin: true,
	}}
	got, err := s.Apply(ctx, u, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetResourceVersion() == "" || got.GetUID() == "" {
		t.Fatalf("apply did not stamp metadata: %+v", got)
	}

	// Status writes skip the CRD validator too.
	st := got.DeepCopyObject().(*internalkinds.LocalUser)
	now := metav1.NewTime(time.Unix(1_800_000_000, 0))
	st.Status.SessionsValidAfter = &now
	if err := s.UpdateStatus(ctx, st); err != nil {
		t.Fatal(err)
	}
	var back internalkinds.LocalUser
	if err := s.Get(ctx, store.Key{Kind: internalkinds.KindLocalUser, Name: "admin"}, &back); err != nil {
		t.Fatal(err)
	}
	if back.Status.SessionsValidAfter == nil || !back.Status.SessionsValidAfter.Equal(&now) || !back.Spec.BuiltinAdmin {
		t.Fatalf("round trip lost fields: %+v", back)
	}

	rb := &internalkinds.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: "dns"}, Spec: internalkinds.RoleBindingSpec{
		Role: "operator", Subjects: []internalkinds.BindingSubject{{Kind: "Group", Name: "corp:ops"}},
	}}
	if _, err := s.Apply(ctx, rb, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, internalkinds.KindRoleBinding, store.ListOptions{Namespace: "dns"})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %v, %v", list.Items, err)
	}

	// A safe snapshot omits internal kinds; a full one keeps them.
	var safe, full bytes.Buffer
	if err := s.Snapshot(ctx, &safe, store.SnapshotOptions{Safe: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(ctx, &full, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(safe.String(), "argon2id") || strings.Contains(safe.String(), "LocalUser") {
		t.Fatalf("safe snapshot leaks internal kinds:\n%s", safe.String())
	}
	if !strings.Contains(safe.String(), "RoleBinding") {
		t.Fatal("safe snapshot omitted public IAM configuration")
	}
	if !strings.Contains(full.String(), "LocalUser") || !strings.Contains(full.String(), "RoleBinding") {
		t.Fatalf("full snapshot misses internal kinds:\n%s", full.String())
	}
}

func TestInternalKindsValidation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, internalOptions)
	cases := []struct {
		name  string
		obj   store.Object
		field string
	}{
		{"bad hash", &internalkinds.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "bob"}, Spec: internalkinds.LocalUserSpec{PasswordHash: "plain"}}, "spec.passwordHash"},
		{"namespaced user", &internalkinds.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "dns"}}, "metadata.namespace"},
		{"binding without subjects", &internalkinds.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "dns"}, Spec: internalkinds.RoleBindingSpec{Role: "viewer"}}, "spec.subjects"},
		{"token id", &internalkinds.APIToken{ObjectMeta: metav1.ObjectMeta{Name: "Short"}, Spec: internalkinds.APITokenSpec{SecretHash: strings.Repeat("a", 64), Owner: internalkinds.TokenOwner{Kind: "User", Name: "bob"}}}, "metadata.name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Apply(ctx, tc.obj, store.ApplyOptions{Manager: store.ManagerAPI})
			var ve *validator.Error
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *validator.Error", err)
			}
			found := false
			for _, c := range ve.Causes {
				found = found || c.Field == tc.field
			}
			if !found || ve.HTTPStatus() != 400 {
				t.Fatalf("causes %+v lack %s", ve.Causes, tc.field)
			}
		})
	}
}
