package providerentitlement_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pe "github.com/robertherbaugh/kudurru/internal/auth/providerentitlement"
	"github.com/robertherbaugh/kudurru/internal/license"
)

type licenseFixture struct {
	key   ed25519.PrivateKey
	ring  license.Keyring
	clock atomic.Int64
	at    time.Time
}

func newLicenseFixture(t *testing.T) *licenseFixture {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &licenseFixture{key: key, ring: license.Keyring{"test": pub}, at: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}
	f.clock.Store(f.at.UnixNano())
	return f
}

func (f *licenseFixture) now() time.Time { return time.Unix(0, f.clock.Load()).UTC() }

func (f *licenseFixture) claims() license.Claims {
	return license.Claims{Issuer: license.Issuer, ID: "test-policy", Licensee: "licensed test owner", Plan: license.PlanEnterprise, Nodes: 3, Features: []license.Feature{license.FeatureRBAC, license.FeatureOIDC}, IssuedAt: f.at.Add(-time.Minute).Unix(), NotBefore: f.at.Add(-time.Minute).Unix(), ExpiresAt: f.at.Add(time.Hour).Unix()}
}

func (f *licenseFixture) signed(t *testing.T, c license.Claims) string {
	t.Helper()
	raw, err := license.Sign(f.key, "test", c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *licenseFixture) manager(t *testing.T, c license.Claims, source string) *license.Manager {
	t.Helper()
	m, err := license.New(license.Options{Keyring: f.ring, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set(f.signed(t, c), source); err != nil {
		t.Fatal(err)
	}
	return m
}

func profile(t *testing.T, m *license.Manager) string {
	t.Helper()
	value, _, err := pe.Profile(m.Capture())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSignedPolicyEquivalenceAcrossInstallationsActiveAndGrace(t *testing.T) {
	f := newLicenseFixture(t)
	first := f.manager(t, f.claims(), "node A mounted source")
	c := f.claims()
	c.Features = []license.Feature{license.FeatureOIDC, license.FeatureRBAC}
	c.IssuedAt = f.at.Add(-2 * time.Minute).Unix()
	c.NotBefore = f.at.Add(-2 * time.Minute).Unix()
	second := f.manager(t, c, "node B source")
	a, deadline, err := pe.Profile(first.Capture())
	if err != nil || len(a) != 64 || !deadline.Equal(f.at.Add(time.Hour+license.Grace)) {
		t.Fatal("verified policy or original deadline missing", err)
	}
	if a != profile(t, second) || first.Capture().Identity() == second.Capture().Identity() {
		t.Fatal("management equivalence used local source/installation/full signed claims")
	}
	// The fixed v1 encoding is durable: this catches unreviewed profile drift.
	if a != "8705b5d8f167cd94ac11f4ced008b2d05936265cb44b08fa51c4522847a6a3ce" {
		t.Fatal("management profile v1 encoding changed")
	}
	old := first.Capture()
	f.clock.Store(f.at.Add(time.Hour).UnixNano())
	if first.Capture().Status().State != license.StateGrace {
		t.Fatal("did not reach real grace interval")
	}
	grace, sameDeadline, err := pe.Profile(first.Capture())
	if err != nil || grace != a || !sameDeadline.Equal(deadline) || !old.Current() {
		t.Fatal("active/grace changed effective profile or extended deadline", err)
	}
	f.clock.Store(deadline.UnixNano())
	if _, _, err := pe.Profile(first.Capture()); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("license grace expiry granted provider policy", err)
	}
	if _, _, err := pe.Profile(old); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("expired held policy accepted", err)
	}
}

func TestEveryEffectivePolicyFieldChangesProfile(t *testing.T) {
	f := newLicenseFixture(t)
	original := profile(t, f.manager(t, f.claims(), "test"))
	for name, change := range map[string]func(*license.Claims){
		"id":       func(c *license.Claims) { c.ID = "other-id" },
		"subject":  func(c *license.Claims) { c.Licensee = "other subject" },
		"plan":     func(c *license.Claims) { c.Plan = license.PlanBusiness },
		"nodes":    func(c *license.Claims) { c.Nodes++ },
		"features": func(c *license.Claims) { c.Features = []license.Feature{license.FeatureOIDC} },
		"deadline": func(c *license.Claims) { c.ExpiresAt++ },
	} {
		t.Run(name, func(t *testing.T) {
			c := f.claims()
			change(&c)
			if profile(t, f.manager(t, c, "test")) == original {
				t.Fatal("changed effective management policy reused profile")
			}
		})
	}
}

func TestDeniedMalformedAndReplacedSignedLicenseCuts(t *testing.T) {
	f := newLicenseFixture(t)
	m := f.manager(t, f.claims(), "test")
	old := m.Capture()
	before := profile(t, m)
	if err := m.Set("", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pe.Profile(m.Capture()); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("community policy granted entitlement", err)
	}
	if err := m.Set(f.signed(t, f.claims()), "test"); err != nil {
		t.Fatal(err)
	}
	if before != profile(t, m) || old.Identity() == m.Capture().Identity() {
		t.Fatal("same policy did not retain equivalence with new local installation")
	}
	if _, _, err := pe.Profile(old); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("old installation revived after identical signed license return", err)
	}
	for name, change := range map[string]func(*license.Claims){
		"not-yet-valid": func(c *license.Claims) { c.NotBefore = f.at.Add(time.Minute).Unix() },
		"no-oidc":       func(c *license.Claims) { c.Features = []license.Feature{license.FeatureRBAC} },
		"id-cap":        func(c *license.Claims) { c.ID = strings.Repeat("x", 257) },
		"subject-cap":   func(c *license.Claims) { c.Licensee = strings.Repeat("x", 4097) },
	} {
		t.Run(name, func(t *testing.T) {
			c := f.claims()
			change(&c)
			if _, _, err := pe.Profile(f.manager(t, c, "test").Capture()); !errors.Is(err, pe.ErrContinuity) {
				t.Fatal("denied or excessive verified profile accepted", err)
			}
		})
	}
	if err := m.Set("malformed", "test"); err == nil {
		t.Fatal("malformed signature accepted by real verifier")
	}
	untrusted := newLicenseFixture(t)
	if err := m.Set(untrusted.signed(t, untrusted.claims()), "test"); !errors.Is(err, license.ErrBadSignature) {
		t.Fatal("untrusted real signature accepted", err)
	}
	for _, cut := range []license.Cut{m.Capture(), {}, (*license.Manager)(nil).Capture(), new(license.Manager).Capture()} {
		if p, deadline, err := pe.Profile(cut); p != "" || !deadline.IsZero() || !errors.Is(err, pe.ErrContinuity) {
			t.Fatal("missing/invalid verified policy returned authority", err)
		}
	}
	good := f.manager(t, f.claims(), "test").Capture()
	f.clock.Store(f.at.Add(-time.Second).UnixNano())
	if _, _, err := pe.Profile(good); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("clock regression accepted later capture", err)
	}
}
