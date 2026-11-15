package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
)

func TestBlockingCLIPendingReceiptDoesNotClaimServingState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != api.BasePath+"/dns/blocking" {
			t.Error("unexpected CLI request", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(api.BlockingCommandReceipt{Committed: true, SettingsUID: "settings", ResourceVersion: "v2", Publication: "pending"})
	}))
	defer server.Close()
	for _, format := range []string{"table", "json"} {
		o := newCLIOpts()
		o.getenv = func(string) string { return "" }
		o.server, o.token, o.output, o.cliConfig = server.URL, "test-token", format, filepath.Join(t.TempDir(), "contexts.yaml")
		cmd := newBlockingCmd(o)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"enable"})
		if err := cmd.ExecuteContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		if format == "table" {
			if !strings.Contains(out.String(), "publication pending") || strings.Contains(out.String(), "blocking: enabled") || strings.Contains(out.String(), "blocking: disabled") {
				t.Fatal("receipt misreported as serving state", out.String())
			}
		} else {
			var raw map[string]any
			if json.Unmarshal(out.Bytes(), &raw) != nil || raw["committed"] != true || raw["publication"] != "pending" || raw["blocking"] != nil {
				t.Fatal("structured receipt invented serving state")
			}
		}
	}
}
