package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

func migrationSource(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer source-secret" {
			t.Error("incorrect source authentication")
		}
		switch r.URL.Path {
		case "/api/zones/list":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"zones":[]}}`)
		case "/api/settings/get":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"enableBlocking":true,"blockingType":"NxDomain","blockingAnswerTtl":30}}`)
		case "/api/blocked/export":
			_, _ = fmt.Fprintln(w, "tracker.example")
		case "/api/allowed/export":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected source path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestMigrationCLIPreviewWithoutDestination(t *testing.T) {
	source := migrationSource(t)
	for _, useFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("file=%t", useFile), func(t *testing.T) {
			h := newHarness(t) // No destination context or credentials.
			args := []string{"import", "technitium", "--source-url", source.URL, "-n", "dns", "-o", "json"}
			if useFile {
				key := filepath.Join(t.TempDir(), "source-key")
				if err := os.WriteFile(key, []byte("source-secret\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				h.env["TECHNITIUM_API_TOKEN"] = "ignored-env-key"
				args = append(args, "--source-key-file", key)
			} else {
				h.env["TECHNITIUM_API_TOKEN"] = "source-secret"
			}
			out, stderr, err := h.run(args...)
			if err != nil {
				t.Fatal(err)
			}
			var p importers.Plan
			if err := json.Unmarshal([]byte(out), &p); err != nil || len(p.Objects) != 2 {
				t.Fatalf("invalid preview: %v", err)
			}
			if p.Objects[0]["metadata"].(map[string]any)["namespace"] != "dns" {
				t.Fatal("namespace was not preserved")
			}
			if strings.Contains(out+stderr, "source-secret") || !strings.Contains(stderr, "warning:") {
				t.Fatal("credential exposed or migration warnings missing")
			}
			if len(h.api.requested("POST")) != 0 {
				t.Fatal("preview wrote to destination")
			}
		})
	}
}

func TestMigrationCLIApplyReportsPartialFailure(t *testing.T) {
	source := migrationSource(t)
	for _, result := range []string{
		`{"items":[{"kind":"Policy","name":"default","result":"updated"}]}`,
		`{"items":[{"kind":"Policy","name":"default","result":"error","error":"read-only"}]}`,
		`{"items":[],"settingsError":"cannot persist settings"}`,
	} {
		t.Run(result, func(t *testing.T) {
			calls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/imports/apply" || r.Header.Get("Authorization") != "Bearer target-secret" {
					t.Error("unexpected destination request")
				}
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), "source-secret") {
					t.Error("source credential sent to destination apply")
				}
				var p importers.Plan
				if json.Unmarshal(b, &p) != nil || len(p.Objects) != 2 {
					t.Error("converted plan missing from apply")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, result)
			}))
			defer target.Close()
			h := newHarness(t)
			h.env["TECHNITIUM_API_TOKEN"] = "source-secret"
			h.env[cli.EnvServer] = target.URL
			h.env[cli.EnvToken] = "target-secret"
			out, _, err := h.run("import", "technitium", "--source-url", source.URL, "--apply", "-o", "json")
			wantError := strings.Contains(result, "error") || strings.Contains(result, "settingsError")
			if (err != nil) != wantError || !strings.Contains(out, `"items"`) || calls != 1 {
				t.Fatalf("apply error=%v, calls=%d, output=%s", err, calls, out)
			}
		})
	}
}

func TestMigrationCLIMissingCredentialAndConflictingFlags(t *testing.T) {
	h := newHarness(t)
	for _, source := range []string{"technitium", "pihole"} {
		if _, _, err := h.run("import", source, "--source-url", "http://dns.local"); err == nil || !strings.Contains(err.Error(), "--source-key-file") {
			t.Fatalf("missing credential error: %v", err)
		}
	}
	if _, _, err := h.run("import", "technitium", "--source-url", "http://dns.local", "--apply", "--dry-run"); err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("conflicting flags: %v", err)
	}
}

func TestMigrationCLICutoverRequiresReviewedFile(t *testing.T) {
	h := newHarness(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/imports/runs/migration-test/decommission/preview":
			if r.Method != http.MethodPost {
				t.Error("preview method")
			}
			var body struct {
				Nameservers []string `json:"nameservers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Nameservers) != 1 || body.Nameservers[0] != "dns.test." {
				t.Errorf("nameservers: %v", body.Nameservers)
			}
			_, _ = fmt.Fprint(w, `{"revision":"42","nameservers":["dns.test."],"zones":[],"warnings":[]}`)
		case "/api/v1/imports/runs/migration-test/decommission":
			var p struct {
				Revision string `json:"revision"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p.Revision != "42" {
				t.Error("reviewed revision not preserved")
			}
			_, _ = fmt.Fprint(w, `{"runID":"migration-test","items":[{"kind":"DNSZone","name":"home","result":"updated"}]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer target.Close()
	h.env[cli.EnvServer] = target.URL
	h.env[cli.EnvToken] = "target-secret"
	out, _, err := h.run("import", "decommission", "migration-test", "--nameserver", "DNS.TEST", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "preview.json")
	if err := os.WriteFile(file, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.run("import", "decommission", "migration-test", "--apply"); err == nil {
		t.Fatal("applied without reviewed preview")
	}
	out, _, err = h.run("import", "decommission", "migration-test", "--apply", "--plan-file", file, "-o", "json")
	if err != nil || !strings.Contains(out, "updated") {
		t.Fatalf("apply reviewed preview: %v %s", err, out)
	}
}
