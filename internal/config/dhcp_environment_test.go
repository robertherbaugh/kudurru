package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func writeDHCPEnvironmentConfig(t *testing.T, d DHCP) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"dhcp": d})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDHCPManagementEnvironmentOwnership(t *testing.T) {
	base, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"rangeStart", "rangeEnd", "router", "dnsServers", "domainName", "leaseDuration", "registerDNS", "dnsTTL", "reservations"} {
		t.Run(field, func(t *testing.T) {
			cfg := *base
			cfg.DHCP = testDHCP()
			cfg.DHCP.Management = "api"
			cfg.EnvLocked = []string{"dhcp." + field}
			// Provenance alone rejects the lock, including equal defaults and
			// collection locks that the environment parser already refuses.
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "dhcp."+field) || !strings.Contains(err.Error(), "remove the environment variable") || !strings.Contains(err.Error(), "management=file") {
				t.Fatalf("API-managed environment override: %v", err)
			}
			cfg.DHCP.Enabled = false
			if err := cfg.Validate(); err != nil {
				t.Fatalf("disabled scope: %v", err)
			}
			cfg.DHCP.Enabled = true
			cfg.DHCP.Management = "file"
			if err := cfg.Validate(); err != nil {
				t.Fatalf("file-managed environment override: %v", err)
			}
		})
	}

	first, second := testDHCPScope(), testDHCPScope()
	second.Name, second.Interface = "other", "eth1"
	second.ServerIP, second.Subnet = "192.168.2.2", "192.168.2.0/24"
	second.RangeStart, second.RangeEnd, second.Router = "192.168.2.100", "192.168.2.200", "192.168.2.1"
	second.Management = "api"
	cfg := *base
	cfg.DHCP.Enabled = true
	cfg.DHCP.Scopes = []DHCPScope{first, second}
	cfg.EnvLocked = []string{"dhcp.scopes"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "dhcp.scopes: environment overrides") {
		t.Fatalf("named API scope environment lock: %v", err)
	}
	cfg.DHCP.Scopes[1].Management = "file"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("named file scope environment lock: %v", err)
	}
}

func TestDHCPManagementLoadEnvironment(t *testing.T) {
	d := testDHCP()
	d.Management = "api"
	path := writeDHCPEnvironmentConfig(t, d)
	for _, tc := range []struct{ field, value string }{
		{"rangeStart", d.RangeStart}, {"rangeEnd", d.RangeEnd}, {"router", d.Router},
		{"dnsServers", strings.Join(d.DNSServers, ",")}, {"domainName", d.DomainName},
		{"leaseDuration", "12h"}, {"registerDNS", "false"}, {"dnsTTL", "30s"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			env := "KUDURRU_DHCP__" + strings.ToUpper(tc.field) + "=" + tc.value
			if _, err := Load(path, []string{env}); err == nil || !strings.Contains(err.Error(), "dhcp."+tc.field+": environment overrides") {
				t.Fatalf("equal-value override accepted: %v", err)
			}
			cfg, err := Load(path, []string{env, "KUDURRU_DHCP__MANAGEMENT=file"})
			if err != nil || cfg.DHCP.Management != "file" {
				t.Fatalf("file ownership override: %v", err)
			}
			if _, err := Load(path, []string{env, "KUDURRU_DHCP__ENABLED=false"}); err != nil {
				t.Fatalf("disabled API scope: %v", err)
			}
		})
	}
	valueFile := filepath.Join(t.TempDir(), "lease-duration")
	if err := os.WriteFile(valueFile, []byte("12h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, []string{"KUDURRU_DHCP__LEASEDURATION_FILE=" + valueFile}); err == nil || !strings.Contains(err.Error(), "dhcp.leaseDuration: environment overrides") {
		t.Fatalf("file-backed environment override accepted: %v", err)
	}
	// Host topology and ownership remain valid environment overrides.
	if _, err := Load(path, []string{
		"KUDURRU_DHCP__ENABLED=true", "KUDURRU_DHCP__MANAGEMENT=api",
		"KUDURRU_DHCP__INTERFACE=eth1", "KUDURRU_DHCP__SERVERIP=192.168.1.3",
		"KUDURRU_DHCP__SUBNET=192.168.1.0/24", "KUDURRU_DHCP__RELAYONLY=false",
	}); err != nil {
		t.Fatalf("topology environment overrides: %v", err)
	}
}

func TestDHCPManagementEnvironmentAndPersistentRestart(t *testing.T) {
	path := writeDHCPEnvironmentConfig(t, testDHCP())
	env := []string{"KUDURRU_DHCP__MANAGEMENT=api"}
	loadRuntime := func(environ []string) dhcp.Config {
		t.Helper()
		cfg, err := Load(path, environ)
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := cfg.DHCP.Config()
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	seed := loadRuntime(env)
	db := filepath.Join(t.TempDir(), "dhcp.db")
	store, err := dhcp.OpenLeases(db, seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opts := dhcp.PoolOptions{RangeStart: seed.RangeStart, RangeEnd: seed.RangeEnd, Router: seed.Router,
		DNSServers: seed.DNSServers, DomainName: "managed.example", LeaseDuration: seed.LeaseDuration,
		RegisterDNS: seed.RegisterDNS, DNSTTL: seed.DNSTTL}
	if err := store.UpdateOptions(context.Background(), store.Configuration().Revision, opts); err != nil {
		t.Fatal(err)
	}
	revision := store.Configuration().Revision
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Even an override equal to the persisted value must fail validation
	// before a caller opens the lease database.
	if _, err := Load(path, append(env, "KUDURRU_DHCP__DOMAINNAME=managed.example")); err == nil || !strings.Contains(err.Error(), "dhcp.domainName: environment overrides") {
		t.Fatalf("persistent scope accepted environment ownership: %v", err)
	}
	reopened, err := dhcp.OpenLeases(db, loadRuntime(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := reopened.Configuration(); got.Config.DomainName != opts.DomainName || got.Revision != revision {
		t.Fatalf("API options lost on restart: %+v", got)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	fileOwned, err := dhcp.OpenLeases(db, loadRuntime([]string{"KUDURRU_DHCP__MANAGEMENT=file", "KUDURRU_DHCP__DOMAINNAME=environment.example"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fileOwned.Close() })
	if got := fileOwned.Configuration().Config.DomainName; got != "environment.example" {
		t.Fatalf("file mode lost environment precedence: %q", got)
	}
}
