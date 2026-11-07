package main

import (
	"context"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServingHTTPTokenOwnerScopeIntersection(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	wire, id := tokens.Generate()
	now := time.Now().UTC()
	created := metav1.NewTime(now.Add(-time.Minute))
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		user, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "scoped-user", CreationTimestamp: created}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(now)}, Spec: internalv1.APITokenSpec{Owner: internalv1.TokenOwner{Kind: "User", Name: "scoped-user", ID: "local:scoped-user", Provider: "local", UID: string(user.GetUID())}, SecretHash: tokens.Hash(wire), Scopes: []string{"viewer"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "owner-zone", Namespace: "default"}, Spec: publicv1.RoleBindingSpec{Role: "zone-admin", Scope: publicv1.BindingScope{Kind: "zone", Name: "home"}, Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "scoped-user"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	cut, err := servingauth.CaptureImage(t.Context(), image, servingauth.Schema{Scheme: scheme, Kinds: image.KindManifest, SchemaVersion: store.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	// A pure compiler test pins explicit management policy; runtime/host license
	// admission is separately exercised by the root composed serving tests.
	view, err := servingauth.Prepare(t.Context(), cut, servingauth.Dependencies{Identity: "scope-test", ExpectedIdentity: "scope-test", License: servingauth.LicensePolicy{RBAC: true}, Permissions: f.h.permissions, CurrentKey: bytesOfOne(32), ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}})
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	p, err := view.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: wire}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := view.Authorize(p, servingHTTPCluster("tokens", "own"), now); err != nil || d.Allowed {
		t.Fatal("scope fixture unexpectedly has cluster authority", err)
	}
	attrs, err := servingHTTPTokenScope(view, p, nil, now)
	if err != nil || attrs.Scope != (rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}) {
		t.Fatal("owner intersection lost tokens.own", err)
	}
	// A candidate scope cannot become authority: removing the only live binding
	// in a new actual SQLite image must deny, even with the old raw token.
	err = f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "RoleBinding", Namespace: "default", Name: "owner-zone"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	image, err = f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cut, err = servingauth.CaptureImage(t.Context(), image, servingauth.Schema{Scheme: scheme, Kinds: image.KindManifest, SchemaVersion: store.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	newView, err := servingauth.Prepare(t.Context(), cut, servingauth.Dependencies{Identity: "scope-test", ExpectedIdentity: "scope-test", License: servingauth.LicensePolicy{RBAC: true}, Permissions: f.h.permissions, CurrentKey: bytesOfOne(32), ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}})
	if err != nil {
		t.Fatal(err)
	}
	defer newView.Release()
	p, err = newView.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: wire}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = servingHTTPTokenScope(newView, p, nil, now); err == nil {
		t.Fatal("missing current owner binding granted tokens.own")
	}
}
