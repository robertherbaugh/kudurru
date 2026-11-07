package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
)

func TestBrandingPublicEnterpriseGate(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, plan := range []license.Plan{"", license.PlanPersonal, license.PlanBusiness, license.PlanTrial, license.PlanEnterprise} {
		t.Run(string(plan), func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			clock := now
			manager, err := license.New(license.Options{Keyring: license.Keyring{"test": pub}, Now: func() time.Time { return clock }})
			if err != nil {
				t.Fatal(err)
			}
			if plan != "" {
				raw, err := license.Sign(priv, "test", license.Claims{ID: "branding-test", Licensee: "private licensee", Plan: plan, Nodes: 3, Features: []license.Feature{license.FeatureBranding}, IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: now.Add(time.Hour).Unix()})
				if err != nil {
					t.Fatal(err)
				}
				if err := manager.Set(raw, "private path"); err != nil {
					t.Fatal(err)
				}
			}
			h := newHarness(t, func(o *Options) {
				o.License = manager
				o.Now = func() time.Time { return clock }
				o.Config.Branding.Name = "Saved organization"
				o.Config.Branding.AccentColor = "#123456"
			})
			var info BrandingInfo
			r := h.do(http.MethodGet, BasePath+"/branding", "", nil).want(http.StatusOK)
			r.json(&info)
			if r.Header().Get("Cache-Control") != "no-store" || strings.Contains(r.Body.String(), "private") {
				t.Fatal("branding must be uncached and public-only")
			}
			enterprise := plan == license.PlanEnterprise
			if info.Enabled != enterprise || enterprise && (info.Name != "Saved organization" || info.ValidUntil == nil) || !enterprise && (info.Name != "kudurru" || info.AccentColor != "" || info.ValidUntil != nil) {
				t.Fatal("incorrect effective branding")
			}
			patch := map[string]any{"branding": map[string]any{"name": "Changed name"}}
			h.do(http.MethodPatch, BasePath+"/settings", "viewer", patch).want(http.StatusForbidden)
			code := http.StatusPaymentRequired
			if enterprise {
				code = http.StatusOK
			}
			h.do(http.MethodPatch, BasePath+"/settings", "admin", patch).want(code)
			if !enterprise {
				if h.srv.config().Branding.Name != "Saved organization" {
					t.Fatal("denied write changed settings")
				}
				return
			}
			// The ordinary grace period applies. Exactly at its end, default
			// presentation returns while the saved values remain untouched.
			clock = now.Add(time.Hour)
			h.do(http.MethodGet, BasePath+"/branding", "", nil).want(http.StatusOK).json(&info)
			if !info.Enabled {
				t.Fatal("grace unexpectedly disabled branding")
			}
			clock = clock.Add(license.Grace)
			info = BrandingInfo{}
			h.do(http.MethodGet, BasePath+"/branding", "", nil).want(http.StatusOK).json(&info)
			if info.Enabled || info.Name != "kudurru" || h.srv.config().Branding.Name != "Changed name" {
				t.Fatal("expiry must hide, not erase customization")
			}
			h.do(http.MethodPatch, BasePath+"/settings", "admin", patch).want(http.StatusPaymentRequired)
		})
	}
}

func TestBrandingCapturedFailureNeverFallsBack(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Config.Branding.Name = "Live decoy"
		o.License = license.Unrestricted()
		o.BrandingReads = func(context.Context) (BrandingInfo, error) {
			return BrandingInfo{}, errUnavailable("captured owner unavailable")
		}
	})
	r := h.do(http.MethodGet, BasePath+"/branding", "", nil).want(http.StatusServiceUnavailable)
	if strings.Contains(r.Body.String(), "Live decoy") {
		t.Fatal("live presentation leaked")
	}
	h.srv.opts.BrandingReads = func(context.Context) (BrandingInfo, error) { return BrandingInfo{}, errors.New("unavailable") }
	h.do(http.MethodGet, BasePath+"/branding", "", nil).want(http.StatusServiceUnavailable)
}

func TestBrandingNoImplicitUnrestrictedLicense(t *testing.T) {
	h := newHarness(t)
	var info BrandingInfo
	h.do(http.MethodGet, BasePath+"/branding", "", nil).want(http.StatusOK).json(&info)
	if info.Enabled || info.Name != "kudurru" {
		t.Fatal("missing license must default")
	}
	h.do(http.MethodPatch, BasePath+"/settings", "admin", map[string]any{"branding": map[string]any{"name": "New brand"}}).want(http.StatusPaymentRequired)
}
