package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

func TestBlockyCLIPreviewAndSelection(t *testing.T) {
	h := newHarness(t)
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
	defer target.Close()
	h.env[cli.EnvServer] = target.URL
	h.env[cli.EnvToken] = "destination-token"
	h.stdin = "upstreams: {groups: {default: [192.0.2.53]}}\nblocking: {blockType: nxDomain}\nkeyFile: /secret-key-marker"
	for _, flags := range [][]string{nil, {"--dry-run"}, {"--skip-settings"}, {"--select-setting", "upstreams"}, {"--select-object", "Policy/default/default", "--select-setting", "upstreams"}} {
		out, stderr, err := h.run(append([]string{"import", "blocky", "-f", "-", "-n", "default", "-o", "json"}, flags...)...)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 || strings.Contains(out+stderr, "secret-") {
			t.Fatal("preview contacted destination or leaked a secret/path")
		}
		var plan importers.Plan
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Source.Type != "blocky" || plan.Source.URL != "" {
			t.Fatal("source identity")
		}
	}
	for _, flags := range [][]string{{"--wait"}, {"--apply", "--dry-run"}, {"--apply", "--select-object", "Policy/missing"}, {"--skip-settings", "--select-setting", "upstreams"}} {
		_, _, err := h.run(append([]string{"import", "blocky", "-f", "-"}, flags...)...)
		if err == nil || calls != 0 {
			t.Fatal("invalid flags/selection reached destination")
		}
	}
}
