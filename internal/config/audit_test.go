package config

import (
	"strings"
	"testing"
)

func TestAuditStore(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Audit.Store != AuditStoreAuto || cfg.Audit.Retention != Duration(9600*60*60*1e9) {
		t.Fatalf("defaults %+v", cfg.Audit)
	}
	if got := cfg.AuditStore(); got != AuditStoreLocal {
		t.Fatalf("sqlite auto = %s", got)
	}
	cfg.Store.Driver = "kube"
	if got := cfg.AuditStore(); got != AuditStoreAuditLog {
		t.Fatalf("kube auto = %s", got)
	}
	cfg.Database.DSN = "postgres://db/kudurru"
	if got := cfg.AuditStore(); got != AuditStoreDatabase {
		t.Fatalf("kube with a database: auto = %s", got)
	}
	cfg.Audit.Store = AuditStoreLocal
	if got := cfg.AuditStore(); got != AuditStoreLocal {
		t.Fatalf("kube local = %s", got)
	}
	cfg.Database.DSN, cfg.Audit.Store = "", AuditStoreDatabase
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "audit.store: database needs database.dsn") {
		t.Fatalf("database without dsn: %v", err)
	}
	cfg.Audit.Store = AuditStoreLocal
	cfg.Store.Driver, cfg.Audit.Store = "sqlite", AuditStoreAuditLog
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "audit.store: auditlog needs store.driver: kube") {
		t.Fatalf("auditlog with sqlite: %v", err)
	}
	cfg.Audit.Store, cfg.Audit.Retention = AuditStoreAuto, Duration(3600*1e9)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "audit.retention: at least 24h") {
		t.Fatalf("short retention: %v", err)
	}
}

func TestAuditKeySecretHostOnly(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil || cfg.Audit.KeySecret != "kudurru-audit-keys" {
		t.Fatal("independent ring default")
	}
	for _, name := range []string{"", "Wrong", "bad/name"} {
		cfg.Audit.KeySecret = name
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "audit.keySecret") {
			t.Fatal("invalid Secret accepted")
		}
	}
	for _, name := range []string{"audit-trust", "release.audit-keys"} {
		cfg.Audit.KeySecret = name
		if err := cfg.Validate(); err != nil {
			t.Fatal("valid Secret rejected")
		}
	}
	found := false
	for _, field := range Schema {
		if field.Path == "audit.keySecret" {
			found = true
			if !field.HostOnly || !field.RestartRequired {
				t.Fatal("audit trust location is remotely mutable")
			}
		}
	}
	if !found {
		t.Fatal("missing key schema")
	}
}
