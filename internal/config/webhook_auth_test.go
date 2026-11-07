package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalDNSWebhookTokenFileConfig(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExternalDNS.Webhook.TokenFile != "" {
		t.Fatal("webhook authentication must default to disabled")
	}
	key, ok := Lookup("externalDNS.webhook.tokenFile")
	if !ok || !key.HostOnly || !key.RestartRequired || key.Scope != ScopeNode || key.Type != TypeString || key.Default != "" || PublicClusterKey(key) {
		t.Fatal("webhook credential path must remain a host-only startup setting")
	}
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte("externalDNS:\n  webhook:\n    enabled: true\n    tokenFile: /run/secrets/webhook-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path, nil)
	if err != nil || cfg.ExternalDNS.Webhook.TokenFile != "/run/secrets/webhook-token" {
		t.Fatal("webhook token file did not load from host YAML", err)
	}
	cfg, err = Load(path, []string{"KUDURRU_EXTERNALDNS__WEBHOOK__TOKENFILE=/run/secrets/another-token"})
	if err != nil || cfg.ExternalDNS.Webhook.TokenFile != "/run/secrets/another-token" {
		t.Fatal("webhook token file did not load from explicit host environment", err)
	}
	if _, _, err := ApplyClusterDocument(cfg, map[string]any{"externalDNS": map[string]any{"webhook": map[string]any{"tokenFile": "/run/secrets/injected"}}}); err == nil {
		t.Fatal("cluster settings delegated webhook credentials")
	}
	next, _, err := ApplyClusterDocument(cfg, map[string]any{"response": map[string]any{"filterAAAA": true}})
	if err != nil || next.ExternalDNS.Webhook.TokenFile != cfg.ExternalDNS.Webhook.TokenFile {
		t.Fatal("cluster replacement lost the host webhook credential path", err)
	}
}
