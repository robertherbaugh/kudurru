package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// TestRunServeRecursiveGroupFromFileFirstBoot boots a first-boot SQLite node
// whose upstream groups are the documented file configuration
// (docs/guide/admin/07-zones-and-upstreams.md, "You can also set the mode in
// the configuration file"). The recursive default group names no strategy;
// seeding the cluster settings with strategy "" failed the Kudurru CRD's enum
// and runServe returned "initialize runtime owners" before any listener came
// up. Nothing is resolved, so the test needs no network.
func TestRunServeRecursiveGroupFromFileFirstBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
upstreams:
  groups:
    default: { mode: recursive, timeout: 4s }
    router: { servers: ["192.168.1.1:53"] }
  conditional: { "[/lan/1.168.192.in-addr.arpa/]": router }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g := file.Upstreams.Groups["default"]; g.Mode != config.UpstreamModeRecursive || g.Strategy != "" {
		t.Fatalf("documented recursive group loaded as %+v", g)
	}
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Upstreams = file.Upstreams

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("recursive group node stopped with %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("recursive group node failed to stop")
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("runServe with the documented recursive group returned before ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("node with the documented recursive group did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	stopped = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("recursive group node failed to stop")
	}
}
