package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsOwnershipCommands(t *testing.T) {
	h := loggedIn(t)
	out := h.mustRun("settings", "ownership", "-o", "json")
	if !strings.Contains(out, "gitops") || !strings.Contains(out, "reviewed") {
		t.Fatalf("ownership: %s", out)
	}
	if _, _, err := h.run("settings", "adopt", "upstreams"); err == nil {
		t.Fatal("adoption did not require reviewed revision")
	}
	if out := h.mustRun("settings", "adopt", "upstreams", "--revision", "reviewed"); !strings.Contains(out, "ui-managed") {
		t.Fatal(out)
	}
	if got := string(h.api.lastBody("POST", "/api/v1/settings/sections/upstreams/ownership/adopt")); got != `{"revision":"reviewed"}` {
		t.Fatal(got)
	}
	if _, _, err := h.run("settings", "release", "upstreams", "--revision", "stale"); err == nil {
		t.Fatal("stale release succeeded")
	}
	if out := h.mustRun("settings", "release", "upstreams", "--revision", "reviewed"); !strings.Contains(out, "gitops-managed") {
		t.Fatal(out)
	}
	file := filepath.Join(t.TempDir(), "proposed.json")
	if err := os.WriteFile(file, []byte(`{"groups":{"default":{"mode":"recursive"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out = h.mustRun("settings", "yaml", "upstreams", "--proposed-file", file)
	if !strings.HasPrefix(out, "config:\n") {
		t.Fatalf("YAML: %s", out)
	}
	var body map[string]any
	if err := json.Unmarshal(h.api.lastBody("POST", "/api/v1/settings/sections/upstreams/yaml"), &body); err != nil {
		t.Fatal(err)
	}
	if body["proposed"] == nil {
		t.Fatal("proposed section was not sent")
	}
}

func TestSettingsSetReviewedRevision(t *testing.T) {
	h := loggedIn(t)
	h.mustRun("settings", "set", "upstreams.bootstrap=192.0.2.1:53", "log.level=debug", "--revision", "reviewed")
	if got := h.api.lastHeader("PATCH", "/api/v1/settings").Get("If-Match"); got != "reviewed" {
		t.Fatalf("upstream revision header: %q", got)
	}
	if got := h.api.lastHeader("PATCH", "/api/v1/node/config").Get("If-Match"); got != "" {
		t.Fatalf("upstream revision sent to node configuration: %q", got)
	}
	if _, _, err := h.run("settings", "set", "upstreams.bootstrap=192.0.2.2:53", "--revision", "stale"); err == nil || !strings.Contains(err.Error(), "settings changed") {
		t.Fatalf("stale upstream revision: %v", err)
	}
	h.mustRun("settings", "set", "cache.maxEntries=42", "--revision", "stale")
	if got := h.api.lastHeader("PATCH", "/api/v1/settings").Get("If-Match"); got != "" {
		t.Fatalf("upstream revision sent to unrelated cache edit: %q", got)
	}
}
