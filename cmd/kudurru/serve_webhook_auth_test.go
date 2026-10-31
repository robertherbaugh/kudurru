package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
)

// Exercise the production license/enable gates, config mapping and constructor;
// invalid configured credentials must stop startup before the listener runs.
func TestRunServeRejectsInvalidWebhookTokenFile(t *testing.T) {
	originalLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	sign := trustTestKey(t)
	key := sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}})
	for _, mode := range []string{"missing", "malformed", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _ := mountedTestConfig(t)
			cfg.Auth.IdentityProviders = nil
			cfg.ExternalDNS.Webhook.Enabled = true
			cfg.ExternalDNS.Webhook.Listen = "127.0.0.1:0"
			path := filepath.Join(t.TempDir(), "webhook-token")
			cfg.ExternalDNS.Webhook.TokenFile = path
			if mode != "missing" {
				perm := os.FileMode(0o600)
				data := "invalid credential with spaces"
				if mode == "unreadable" {
					perm = 0
					data = "synthetic-test-credential"
				}
				if err := os.WriteFile(path, []byte(data), perm); err != nil {
					t.Fatal("could not prepare invalid startup credential")
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := runServe(ctx, cfg, key)
			if err == nil || !strings.HasPrefix(err.Error(), "externaldns: invalid externalDNS.webhook.tokenFile:") {
				t.Fatal("production serve did not reject the configured invalid webhook token file")
			}
			if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "synthetic-test-credential") || strings.Contains(err.Error(), "invalid credential with spaces") {
				t.Fatal("startup credential error disclosed private input")
			}
		})
	}
}
