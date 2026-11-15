package providercoordinator

import (
	"context"
	"testing"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestBrowserSelectorRetainsExactResourceHostAndRelease(t *testing.T) {
	server := newProviderServer(t)
	f := newFixture(t, server)
	accept(t, f, candidates(t, f, 1))
	ready(t, f, 1)
	old := f.capture(t)
	defer old.Release()
	ref := old.ProviderResources[0].Ref
	// A verifier with no explicit redirect is a complete denying browser cut.
	if _, err := old.Browser(ref, "wrong-resource"); err == nil {
		t.Fatal("resource mismatch")
	}
	if err := f.resources.WithReplacement(t.Context(), func(r *providerresources.Replacement) error {
		err := f.store.Command(t.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var source publicv1.IdentityProvider
			if err := tx.Get(ctx, store.Key{Kind: "IdentityProvider", Name: "corp0"}, &source); err != nil {
				return err
			}
			source.Spec.RedirectURL = "https://console.example/api/v1/auth/oidc/corp0/callback"
			_, err := tx.Apply(ctx, &source, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		r.Retire()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	accept(t, f, candidates(t, f, 1))
	ready(t, f, 1)
	capture := f.capture(t)
	defer capture.Release()
	info, err := f.view(t).ProviderInfo("corp0")
	if err != nil {
		t.Fatal(err)
	}
	b, err := capture.Browser(info.Descriptor.Ref, info.ResourceIdentity)
	if err != nil || !b.Current() {
		t.Fatal("same retained cut not selected", err)
	}
	if _, err := old.Browser(info.Descriptor.Ref, info.ResourceIdentity); err == nil {
		t.Fatal("old capture adopted new source")
	}
	before, _ := f.store.ReadGeneration(t.Context())
	wrong := info.Descriptor.Ref
	wrong.UID = "wrong-uid"
	if _, err := capture.Browser(wrong, info.ResourceIdentity); err == nil {
		t.Fatal("new UID adopted")
	}
	after, _ := f.store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("pure browser selection wrote state")
	}
	f.host.install(&hostGeneration{id: "replacement", allowed: true, secret: "test-only"})
	if b.Current() {
		t.Fatal("captured host replacement not bound")
	}
	if _, err := capture.Browser(info.Descriptor.Ref, info.ResourceIdentity); err == nil {
		t.Fatal("selector recaptured successor host")
	}
	capture.Release()
	if b.Current() {
		t.Fatal("release retained borrowed browser")
	}
}
