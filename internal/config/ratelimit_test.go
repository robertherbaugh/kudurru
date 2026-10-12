package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRateLimitAndSocketsDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := RateLimit{
		Enabled:    true,
		IPv4:       map[string]int{"32": 600, "24": 6000},
		IPv6:       map[string]int{"128": 600, "64": 6000, "56": 12000},
		Slip:       50,
		MaxBuckets: 65536,
		Exempt:     []string{},
	}
	if !reflect.DeepEqual(cfg.RateLimit, want) {
		t.Errorf("ratelimit defaults:\n got %+v\nwant %+v", cfg.RateLimit, want)
	}
	if cfg.Listeners.Sockets != 0 {
		t.Errorf("listeners.sockets default %d, want 0 (GOMAXPROCS)", cfg.Listeners.Sockets)
	}

	env, err := Load("", []string{
		"KUDURRU_RATELIMIT__ENABLED=false",
		"KUDURRU_RATELIMIT__SLIP=10",
		"KUDURRU_RATELIMIT__EXEMPT=10.0.0.0/8, 192.0.2.7",
		"KUDURRU_LISTENERS__SOCKETS=2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.RateLimit.Enabled || env.RateLimit.Slip != 10 || !reflect.DeepEqual(env.RateLimit.Exempt, []string{"10.0.0.0/8", "192.0.2.7"}) || env.Listeners.Sockets != 2 {
		t.Errorf("env overrides lost: %+v sockets=%d", env.RateLimit, env.Listeners.Sockets)
	}
}

func TestRateLimitFileMergesLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
listeners: { sockets: 1 }
ratelimit:
  ipv4: { 24: 0, 16: 60000 }
  ipv6: { 48: 24000 }
  maxBuckets: 1000
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"32": 600, "24": 0, "16": 60000}; !reflect.DeepEqual(cfg.RateLimit.IPv4, want) {
		t.Errorf("ipv4 levels %v, want %v", cfg.RateLimit.IPv4, want)
	}
	if want := map[string]int{"128": 600, "64": 6000, "56": 12000, "48": 24000}; !reflect.DeepEqual(cfg.RateLimit.IPv6, want) {
		t.Errorf("ipv6 levels %v, want %v", cfg.RateLimit.IPv6, want)
	}
	if cfg.RateLimit.MaxBuckets != 1000 || cfg.Listeners.Sockets != 1 {
		t.Errorf("file values lost: %+v sockets=%d", cfg.RateLimit, cfg.Listeners.Sockets)
	}
}

func TestRateLimitValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte(`
listeners: { sockets: -1 }
ratelimit:
  slip: 101
  maxBuckets: -5
  ipv4: { 33: 10, abc: 5, 8: -1 }
  ipv6: { 129: 10 }
  exempt: [ "10.0.0.0/8", "not-an-address" ]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("invalid ratelimit config must be rejected")
	}
	for _, want := range []string{
		"listeners.sockets: -1 is negative",
		"ratelimit.slip: 101 is outside 0-100",
		"ratelimit.maxBuckets: -5 is negative",
		`ratelimit.ipv4: key "33" is not a number in 0-32`,
		`ratelimit.ipv4: key "abc" is not a number in 0-32`,
		"ratelimit.ipv4[8]: -1 is negative",
		`ratelimit.ipv6: key "129" is not a number in 0-128`,
		`ratelimit.exempt[1]: "not-an-address" is neither a CIDR nor an address`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "10.0.0.0/8") {
		t.Errorf("a valid exempt CIDR was reported: %v", err)
	}
}
