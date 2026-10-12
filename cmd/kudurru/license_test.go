package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
)

// trustTestKey makes the commands trust a throwaway licensor key and returns
// a signer for it.
func trustTestKey(t *testing.T) func(c license.Claims) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := newLicenseManager
	newLicenseManager = func() (*license.Manager, error) {
		return license.New(license.Options{Keyring: license.Keyring{"t": pub}})
	}
	t.Cleanup(func() { newLicenseManager = old })
	return func(c license.Claims) string {
		c.IssuedAt, c.ExpiresAt = time.Now().Add(-time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix()
		c.ID, c.Licensee = "lic_t", "Homelab Hank"
		k, err := license.Sign(priv, "t", c)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
}

func TestLicenseStatusAndVerify(t *testing.T) {
	sign := trustTestKey(t)
	key := sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureOIDC}})
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	t.Setenv("KUDURRU_CONFIG", "")

	out := run(t, "license", "status", "--config", t.TempDir()+"/none.yaml")
	if !strings.Contains(out, "state:") || !strings.Contains(out, "community") || !strings.Contains(out, "nodes:     1") {
		t.Errorf("no key: %q", out)
	}
	out = run(t, "license", "status", "--license-key", key, "--json")
	for _, want := range []string{`"state": "active"`, `"plan": "personal"`, `"nodes": 3`, `"oidc"`} {
		if !strings.Contains(out, want) {
			t.Errorf("status --json missing %s: %s", want, out)
		}
	}
	t.Setenv(license.EnvKey, key)
	if out := run(t, "license", "status"); !strings.Contains(out, "active") || !strings.Contains(out, "env "+license.EnvKey) {
		t.Errorf("status from the environment: %q", out)
	}
	t.Setenv(license.EnvKey, "garbage")
	if out, err := runErr("license", "status"); err == nil || !strings.Contains(out, "invalid") {
		t.Errorf("garbage key: err=%v out=%q", err, out)
	}
	if out := run(t, "license", "verify", key); !strings.Contains(out, "Homelab Hank") {
		t.Errorf("verify: %q", out)
	}
	if _, err := runErr("license", "verify", "nonsense"); err == nil {
		t.Error("verify accepted a garbage key")
	}
}

func TestRequireFeatureGatesTheSupervisors(t *testing.T) {
	sign := trustTestKey(t)
	logger := slog.New(slog.DiscardHandler)
	t.Setenv(license.EnvKeyFile, "")

	t.Setenv(license.EnvKey, "")
	if err := requireFeature(logger, license.FeatureEdge); err == nil || !strings.Contains(err.Error(), "needs a kudurru license key") {
		t.Errorf("no key: err = %v", err)
	}
	t.Setenv(license.EnvKey, sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureOIDC}}))
	if err := requireFeature(logger, license.FeatureEdge); err == nil {
		t.Error("a key without the edge feature started the edge")
	}
	t.Setenv(license.EnvKey, sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3}))
	if err := requireFeature(logger, license.FeatureEdge); err != nil {
		t.Errorf("an all-features key was refused: %v", err)
	}
}
