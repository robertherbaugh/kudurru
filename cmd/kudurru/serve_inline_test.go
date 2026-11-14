package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
)

func TestRunServeInlineFileReload(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "environment"}[locked], func(t *testing.T) {
			t.Setenv(license.EnvKey, "")
			t.Setenv(license.EnvKeyFile, "")
			cfg, _ := mountedTestConfig(t)
			cfg.Source = filepath.Join(t.TempDir(), "node.yaml")
			objects := func(address string, record bool) []map[string]any {
				rows := []map[string]any{{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "home"}, "spec": map[string]any{"zoneName": "example.", "type": "primary", "policy": map[string]any{"notify": "none"}}}}
				if record {
					rows = append(rows, map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": map[string]any{"name": "inline", "namespace": "default"}, "spec": map[string]any{"zoneRef": map[string]any{"name": "home"}, "endpoints": []any{map[string]any{"dnsName": "inline.example.", "recordType": "A", "targets": []string{address}}}}})
				}
				return rows
			}
			put := func(rows []map[string]any) {
				t.Helper()
				next := *cfg
				next.Objects = rows
				b, err := json.Marshal(&next)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.Source+".next", b, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(cfg.Source+".next", cfg.Source); err != nil {
					t.Fatal(err)
				}
			}
			cfg.Objects = objects("192.0.2.10", true)
			if locked {
				cfg.EnvLocked = append(cfg.EnvLocked, "objects")
				put(objects("192.0.2.99", true))
			} else {
				put(cfg.Objects)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- runServe(ctx, cfg, "") }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(15 * time.Second):
					t.Error("serve failed to stop")
				}
			})
			awaitMountedDNS(t, cfg, "inline.example.", "192.0.2.10")
			if locked {
				return
			} // file cannot override the original environment source
			put(objects("192.0.2.20", true))
			awaitMountedDNS(t, cfg, "inline.example.", "192.0.2.20")
			// A valid empty object list for the record removes it through the same
			// owned-file path and complete DNS publication, without restarting.
			put(objects("", false))
			awaitMountedDNSAbsent(t, cfg, "inline.example.")
			put(objects("192.0.2.30", true))
			awaitMountedDNS(t, cfg, "inline.example.", "192.0.2.30")
		})
	}
}
