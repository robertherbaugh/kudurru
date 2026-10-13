package main

import (
	"bytes"
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
		if c.ExpiresAt == 0 {
			c.IssuedAt, c.ExpiresAt = time.Now().Add(-time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix()
		}
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

// The supervisors sit in the data path: a lapse must never become an outage.
// Only a start with no key that ever covered the edge is refused.
func TestRequireFeatureGatesTheSupervisors(t *testing.T) {
	sign := trustTestKey(t)
	t.Setenv(license.EnvKeyFile, "")
	day := 24 * time.Hour
	expired := func(ago time.Duration, features ...license.Feature) string {
		return sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: features,
			IssuedAt: time.Now().Add(-ago - 365*day).Unix(), ExpiresAt: time.Now().Add(-ago).Unix()})
	}
	for _, tc := range []struct {
		name    string
		key     string
		wantErr string // a substring; "" means the supervisor starts
		warn    string // a substring of the warning; "" means none
	}{
		{"no key", "", "needs a kudurru license key", ""},
		{"a key that cannot be verified", "not-a-key", "needs a kudurru license key", ""},
		{"a key without the edge", sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureOIDC}}), "needs a kudurru license key", ""},
		{"a valid key", sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3}), "", ""},
		{"a key in its grace period", expired(10 * day), "", "keeps working until"},
		{"a key that expired past the grace period", expired(60 * day), "", "keeps running because it was licensed"},
		{"an expired key that covered the edge by name", expired(60*day, license.FeatureEdge), "", "keeps running because it was licensed"},
		{"an expired key that never covered the edge", expired(60*day, license.FeatureOIDC), "needs a kudurru license key", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(license.EnvKey, tc.key)
			var logs bytes.Buffer
			err := requireFeature(slog.New(slog.NewTextHandler(&logs, nil)), license.FeatureEdge)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("the supervisor was refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if tc.warn == "" && strings.Contains(logs.String(), "level=WARN msg=\"the license key") {
				t.Errorf("unexpected license warning: %s", logs.String())
			}
			if tc.warn != "" && !strings.Contains(logs.String(), tc.warn) {
				t.Errorf("log lacks %q: %s", tc.warn, logs.String())
			}
		})
	}
}
