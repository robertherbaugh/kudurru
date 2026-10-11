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
	want := Blocking{Enabled: true, DefaultResponse: "zeroIP", BlockTTL: Duration(10 * time.Minute), AllowDegraded: true}
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
		`blocking.defaultResponse: unknown response "sinkhole"`,
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
