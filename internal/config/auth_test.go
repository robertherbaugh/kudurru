package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadYAML(t *testing.T, yaml string, environ ...string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, environ)
}

func TestAuthDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a.LocalLogin != "enabled" || a.Session.IdleTimeout.Std() != 30*time.Minute || a.Session.Lifetime.Std() != 12*time.Hour ||
		a.Session.Secure != "auto" || a.Tokens.MaxLifetime != 0 || a.Kubernetes.Mode != "sar" ||
		a.AdminSocket.Path != "/run/kudurru/kudurru.sock" || a.AdminSocket.Group != -1 || len(a.IdentityProviders) != 0 || len(cfg.RBAC.Bindings) != 0 {
		t.Fatalf("auth defaults %+v rbac %+v", a, cfg.RBAC)
	}
}

func TestAuthFile(t *testing.T) {
	cfg, err := loadYAML(t, `
auth:
  localLogin: admins-only
  bootstrap: { adminPasswordFile: /run/secrets/admin_password }
  session: { idleTimeout: 15m, lifetime: 8h, secure: always }
  tokens: { maxLifetime: 720h }
  identityProviders:
  - id: corp-sso
    issuer: https://auth.example.com/application/o/kudurru/
    clientID: kudurru
    clientSecretFile: /run/secrets/oidc_client_secret
    cliClientID: kudurru-cli
    scopes: [openid, profile, email, groups]
    claimMappings: { username: { claim: preferred_username }, groups: { claim: groups, prefix: "corp:" } }
    provisioning: { autoProvision: mapped-only, syncGroups: true }
rbac:
  bindings:
  - { role: admin, scope: { kind: cluster }, subjects: [{ kind: Group, name: "corp:/dns-admins" }] }
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.LocalLogin != "admins-only" || cfg.Auth.Session.Lifetime.Std() != 8*time.Hour || cfg.Auth.Tokens.MaxLifetime.Std() != 720*time.Hour {
		t.Fatalf("auth %+v", cfg.Auth)
	}
	if len(cfg.Auth.IdentityProviders) != 1 || cfg.Auth.IdentityProviders[0]["id"] != "corp-sso" {
		t.Fatalf("identity providers %#v", cfg.Auth.IdentityProviders)
	}
	cm, _ := cfg.Auth.IdentityProviders[0]["claimMappings"].(map[string]any)
	if g, _ := cm["groups"].(map[string]any); g["prefix"] != "corp:" {
		t.Fatalf("nested claim mappings lost: %#v", cfg.Auth.IdentityProviders[0])
	}
	if len(cfg.RBAC.Bindings) != 1 || cfg.RBAC.Bindings[0]["role"] != "admin" {
		t.Fatalf("bindings %#v", cfg.RBAC.Bindings)
	}
}

func TestAuthValidation(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"auth: { localLogin: sometimes }", "auth.localLogin"},
		{"auth: { session: { secure: maybe } }", "auth.session.secure"},
		{"auth: { session: { idleTimeout: 10s } }", "auth.session.idleTimeout"},
		{"auth: { session: { idleTimeout: 2h, lifetime: 1h } }", "auth.session.lifetime"},
		{"auth: { kubernetes: { mode: impersonate } }", "impersonate is planned for v0.2"},
		{"auth: { kubernetes: { mode: magic } }", "auth.kubernetes.mode"},
		{"auth: { identityProviders: [ { issuer: x } ] }", "identityProviders[0].id"},
		{"auth: { identityProviders: [ { id: a }, { id: a } ] }", "defined twice"},
		{"rbac: { bindings: [ { subjects: [ { kind: User, name: x } ] } ] }", "rbac.bindings[0].role"},
		{"rbac: { bindings: [ { role: admin } ] }", "rbac.bindings[0].subjects"},
	} {
		_, err := loadYAML(t, tc.yaml)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}
