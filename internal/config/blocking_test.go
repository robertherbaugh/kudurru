package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBlockingDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Blocking{Enabled: true, DefaultResponse: "zeroIP", BlockTTL: Duration(10 * time.Minute), AllowDegraded: true, RefreshInterval: Duration(24 * time.Hour), InitStrategy: "fast",
		Catalogue: BlockingCatalogue{RefreshInterval: Duration(24 * time.Hour)}}
	if cfg.Blocking != want {
		t.Errorf("blocking defaults:\n got %+v\nwant %+v", cfg.Blocking, want)
	}
	env, err := Load("", []string{
		"KUDURRU_BLOCKING__ENABLED=false",
		"KUDURRU_BLOCKING__DEFAULTRESPONSE=customIP",
		"KUDURRU_BLOCKING__CUSTOMIP__V4=10.0.0.53",
		"KUDURRU_BLOCKING__BLOCKTTL=1m",
	})
	if err != nil {
		t.Fatal(err)
	}
	b := env.Blocking
	if b.Enabled || b.DefaultResponse != "customIP" || b.CustomIP.V4 != "10.0.0.53" || b.BlockTTL.Std() != time.Minute {
		t.Errorf("env overrides lost: %+v", b)
	}
}

func TestBlockingValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(`
blocking: { defaultResponse: sinkhole, blockTTL: -1s, customIP: { v4: "::1", v6: "10.0.0.1" } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("invalid blocking config must be rejected")
	}
	for _, want := range []string{
		`blocking.defaultResponse: unknown value "sinkhole" (zeroIP, nxdomain, customIP, refused, blockPage)`,
		"blocking.blockTTL: -1s is negative",
		`blocking.customIP.v4: "::1" is not an IPv4 address`,
		`blocking.customIP.v6: "10.0.0.1" is not an IPv6 address`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	if err := os.WriteFile(path, []byte("blocking: { defaultResponse: customIP }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); err == nil || !strings.Contains(err.Error(), "needs blocking.customIP") {
		t.Errorf("customIP without addresses must be rejected: %v", err)
	}
}

func TestBlockingCatalogueValidation(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{[]string{"KUDURRU_BLOCKING__CATALOGUE__URL=ftp://data.example/catalogue.json"}, "blocking.catalogue.url"},
		{[]string{"KUDURRU_BLOCKING__CATALOGUE__URL=https://"}, "blocking.catalogue.url"},
		{[]string{"KUDURRU_BLOCKING__CATALOGUE__REFRESHINTERVAL=30s"}, "blocking.catalogue.refreshInterval"},
	} {
		if _, err := Load("", tc.env); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v", tc.env, err)
		}
	}
	cfg, err := Load("", []string{"KUDURRU_BLOCKING__CATALOGUE__URL=https://data.example/catalogue.json", "KUDURRU_BLOCKING__CATALOGUE__REFRESHINTERVAL=6h"})
	if err != nil {
		t.Fatal(err)
	}
	if c := cfg.Blocking.Catalogue; c.URL != "https://data.example/catalogue.json" || c.RefreshInterval.Std() != 6*time.Hour {
		t.Errorf("catalogue = %+v", c)
	}
}

func TestBlockingHTTPSOnlyHostPolicy(t *testing.T) {
	defaults, err := Load("", nil)
	if err != nil || defaults.Blocking.HTTPSOnly {
		t.Fatal("HTTPS-only must be opt-in", err)
	}
	cfg, err := Load("", []string{"KUDURRU_BLOCKING__HTTPSONLY=true"})
	if err != nil || !cfg.Blocking.HTTPSOnly {
		t.Fatal("HTTPS-only environment setting not loaded", err)
	}
	found := false
	for _, key := range Schema {
		if key.Path == "blocking.httpsOnly" {
			found = true
			if !key.HostOnly || !key.RestartRequired || key.Scope != ScopeNode {
				t.Fatal("download security policy must remain host-only and restart-required")
			}
		}
	}
	if !found {
		t.Fatal("HTTPS-only policy is missing from settings metadata")
	}
}
