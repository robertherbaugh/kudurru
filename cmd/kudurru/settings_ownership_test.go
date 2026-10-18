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
