package providerhost

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func installBrandingLicense(t *testing.T, x *fixture, plan license.Plan, features []license.Feature) {
	t.Helper()
	claims := x.f.claims
	claims.Plan, claims.Features = plan, features
	raw, err := license.Sign(x.f.key, "test", claims)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.license.Set(raw, "branding-test"); err != nil {
		t.Fatal(err)
	}
	if err = x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestBrandingHostEnterpriseAndRestrictedFeatures(t *testing.T) {
	x := newFixture(t, staticInput(t))
	defer x.stop()
	for _, plan := range []license.Plan{license.PlanPersonal, license.PlanBusiness, license.PlanTrial, license.PlanEnterprise} {
		for _, list := range []struct {
			name     string
			features []license.Feature
			granted  bool
		}{
			{"all", nil, true},
			{"explicit", []license.Feature{license.FeatureBranding}, true},
			{"restricted", []license.Feature{license.FeatureOIDC}, false},
		} {
			t.Run(string(plan)+"/"+list.name, func(t *testing.T) {
				installBrandingLicense(t, x, plan, list.features)
				h := capture(t, x)
				defer h.Release()
				p := h.Auth.License
				want := plan == license.PlanEnterprise && list.granted
				if p.Branding != want {
					t.Fatal("actual host projection bypassed branding plan/feature gate")
				}
				if want {
					if !p.BrandingFrom.Equal(x.f.now()) || !p.BrandingUntil.Equal(x.f.claims.Expires().Add(license.Grace)) {
						t.Fatal("host did not preserve original finite license validity")
					}
				} else if !p.BrandingFrom.IsZero() || !p.BrandingUntil.IsZero() {
					t.Fatal("denied projection retained branding validity")
				}
				requests := (license.Claims{Plan: plan, Features: list.features}).Allows(license.FeatureRequests)
				if p.Requests != requests {
					t.Fatal("branding restriction changed unblock-workflow entitlement")
				}
				if requests {
					if !p.RequestsFrom.Equal(x.f.now()) || !p.RequestsUntil.Equal(x.f.claims.Expires().Add(license.Grace)) {
						t.Fatal("request preview lacks original finite license validity")
					}
				} else if !p.RequestsFrom.IsZero() || !p.RequestsUntil.IsZero() {
					t.Fatal("denied request preview retained validity")
				}
			})
		}
	}
}

func TestCapturedRequestsHostGraceAndExpiry(t *testing.T) {
	x := newFixture(t, staticInput(t))
	defer x.stop()
	installBrandingLicense(t, x, license.PlanBusiness, []license.Feature{license.FeatureRequests})
	first := capture(t, x)
	defer first.Release()
	p := first.Auth.License
	if !p.Requests || p.Branding {
		t.Fatal("Business requests policy borrowed the Enterprise branding restriction")
	}
	x.f.clock.Store(x.f.claims.Expires().UnixNano())
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	grace := capture(t, x)
	defer grace.Release()
	if grace.Auth.License != p {
		t.Fatal("grace renewed the captured request preview window")
	}
	x.f.clock.Store(p.RequestsUntil.UnixNano())
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	expired := capture(t, x)
	defer expired.Release()
	if expired.Auth.License.Requests {
		t.Fatal("exact grace deadline retained request preview entitlement")
	}
}

func TestBrandingHostStableWindowGraceAndOriginalCommand(t *testing.T) {
	x := newFixture(t, staticInput(t))
	defer x.stop()
	installBrandingLicense(t, x, license.PlanEnterprise, []license.Feature{license.FeatureBranding})
	first := capture(t, x)
	defer first.Release()
	p := first.Auth.License
	x.f.clock.Store(x.f.now().Add(time.Second).UnixNano())
	next := capture(t, x)
	defer next.Release()
	if first.Identity != next.Identity || p != next.Auth.License {
		t.Fatal("repeated host capture moved the original window or identity")
	}
	x.f.clock.Store(x.f.claims.Expires().UnixNano())
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	grace := capture(t, x)
	defer grace.Release()
	if !grace.Current() || grace.Auth.License != p || grace.Identity != first.Identity {
		t.Fatal("active-to-grace changed the original branding window")
	}
	// Actual admitted SQLite command sees the same pinned host policy, even
	// without OIDC/provider activation. No successor Manager lookup is needed.
	await(t, func() bool {
		lease, err := x.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		lease.Release()
		return true
	})
	ctx, target := writer(t, x.s)
	var received servingauth.LicensePolicy
	if err := x.coordinator.WithCommand(ctx, target, func(_ context.Context, _ *sqlite.CommandTx, deps servingauth.Dependencies) error {
		received = deps.License
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if received != p {
		t.Fatal("current command did not receive the original pinned branding policy")
	}
	// Replacement denies future commands but cannot rewrite the detached policy
	// already carried by an original read. View applies its original deadline.
	if err := x.license.Set("", ""); err != nil {
		t.Fatal(err)
	}
	if first.Current() || first.Auth.License != p {
		t.Fatal("removal retained current authority or rewrote original policy")
	}
	called := false
	if err := x.host.PinHostCurrent(t.Context(), func(func() bool) error { called = true; return nil }); err == nil || called {
		t.Fatal("replacement admitted an old host command")
	}
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	removed := capture(t, x)
	defer removed.Release()
	if removed.Auth.License.Branding {
		t.Fatal("removed license retained branding")
	}
	installBrandingLicense(t, x, license.PlanEnterprise, []license.Feature{license.FeatureBranding})
	returned := capture(t, x)
	defer returned.Release()
	x.f.clock.Store(p.BrandingUntil.UnixNano())
	called = false
	if returned.Current() {
		t.Fatal("held command authority survived exact grace end")
	}
	if err := x.host.PinHostCurrent(t.Context(), func(func() bool) error { called = true; return nil }); err == nil || called {
		t.Fatal("exact grace end admitted old host command")
	}
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	expired := capture(t, x)
	defer expired.Release()
	if expired.Auth.License.Branding || !expired.Current() {
		t.Fatal("expiry did not install a complete default-branding policy")
	}
}
