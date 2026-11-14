package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalDNSCRDHostConfiguration(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil || cfg.ExternalDNS.CRD.Enabled {
		t.Fatal("source must be opt-in", err)
	}
	for _, key := range []string{"externalDNS.crd.enabled", "externalDNS.crd.namespace", "externalDNS.crd.zoneSelector"} {
		meta, ok := Lookup(key)
		if !ok || !meta.HostOnly || !meta.RestartRequired || PublicClusterKey(meta) {
			t.Fatal("source authority is not host-owned", key)
		}
	}
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte("store: {driver: kube}\nexternalDNS:\n  crd:\n    enabled: true\n    namespace: apps\n    zoneSelector: {external-dns: enabled}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path, nil)
	if err != nil || !cfg.ExternalDNS.CRD.Enabled || cfg.ExternalDNS.CRD.Namespace != "apps" || cfg.ExternalDNS.CRD.ZoneSelector["external-dns"] != "enabled" {
		t.Fatal("source config did not load", err)
	}
	if _, _, err := ApplyClusterDocument(cfg, map[string]any{"externalDNS": map[string]any{"crd": map[string]any{"enabled": true}}}); err == nil {
		t.Fatal("API delegated source authority")
	}
	cfg.Store.Driver = "sqlite"
	if cfg.Validate() == nil {
		t.Fatal("unsupported driver admitted source before startup")
	}
	cfg.Store.Driver = "kube"
	cfg.ExternalDNS.CRD.Namespace = "not/a/namespace"
	if cfg.Validate() == nil {
		t.Fatal("invalid source namespace accepted")
	}
}
