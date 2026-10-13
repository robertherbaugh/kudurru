package oidc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func TestProviderConfigDefaults(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Scopes, DefaultScopes) {
		t.Errorf("scopes = %v", cfg.Scopes)
	}
	if cfg.DisplayName != "corp" || cfg.ClaimMappings.Groups.Claim != "groups" ||
		cfg.ClaimMappings.DisplayName.Claim != "name" || cfg.ClaimMappings.Email.Claim != "email" ||
		cfg.Provisioning.AutoProvision != AutoProvisionMappedOnly {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	cfg = validConfig()
	cfg.Scopes = []string{"profile"}
	_ = cfg.Validate(nil)
	if !reflect.DeepEqual(cfg.Scopes, []string{"openid", "profile"}) {
		t.Errorf("openid not added: %v", cfg.Scopes)
	}
}

func TestProviderConfigValidation(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*ProviderConfig)
		errSub string
	}{
		{"bad id", func(c *ProviderConfig) { c.ID = "Corp SSO" }, "lowercase DNS label"},
		{"http issuer", func(c *ProviderConfig) { c.Issuer = "http://idp.example" }, "must use https"},
		{"relative redirect", func(c *ProviderConfig) { c.RedirectURL = "/callback" }, "absolute"},
		{"no client id", func(c *ProviderConfig) { c.ClientID = "" }, "clientID"},
		{"no secret", func(c *ProviderConfig) { c.ClientSecret = "" }, "confidential"},
		{"rule without claim", func(c *ProviderConfig) { c.ClaimValidationRules = []ClaimValidationRule{{RequiredValue: "x"}} }, "claim is required"},
		{"bad autoProvision", func(c *ProviderConfig) { c.Provisioning.AutoProvision = "sometimes" }, "autoProvision"},
		{"defaultRole admin", func(c *ProviderConfig) {
			c.Provisioning = Provisioning{AutoProvision: AutoProvisionAlways, DefaultRole: "admin"}
		}, "may not be admin"},
		{"defaultRole unknown", func(c *ProviderConfig) {
			c.Provisioning = Provisioning{AutoProvision: AutoProvisionAlways, DefaultRole: "superuser"}
		}, "not a known role"},
		{"defaultRole scoped", func(c *ProviderConfig) {
			c.Provisioning = Provisioning{AutoProvision: AutoProvisionAlways, DefaultRole: "zone-admin"}
		}, "scoped"},
		{"defaultRole with mapped-only", func(c *ProviderConfig) { c.Provisioning = Provisioning{DefaultRole: "viewer"} }, "contradicts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			c.mutate(&cfg)
			err := cfg.Validate(perms)
			if err == nil || !strings.Contains(err.Error(), c.errSub) {
				t.Fatalf("err = %v, want it to mention %q", err, c.errSub)
			}
		})
	}
	ok := validConfig()
	ok.Issuer = "http://127.0.0.1:5556/dex"
	ok.Provisioning = Provisioning{AutoProvision: AutoProvisionAlways, DefaultRole: "viewer"}
	if err := ok.Validate(perms); err != nil {
		t.Fatalf("loopback http issuer + viewer default role: %v", err)
	}
}
