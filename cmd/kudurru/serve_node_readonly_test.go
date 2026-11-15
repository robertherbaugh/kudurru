package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

func TestServeLegacyNodeSettingsReadOnly(t *testing.T) {
	ts, _ := authAPI(t, nil)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := ts.URL + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "admin-password-1"}, http.StatusOK, &login)
	assertMountedNodeReadOnly(t, client, base, login.CSRF, "")
}

// Used by both the actual legacy cookie/CSRF route and the real Kubernetes
// runServe/envtest mount. No mutation retries or raw credential diagnostics.
func assertMountedNodeReadOnly(t *testing.T, client *http.Client, base, csrf, bearer string) {
	t.Helper()
	request := func(method, path string, body any, want int, out any) {
		t.Helper()
		var data []byte
		var err error
		if body != nil {
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal("node read-only request encoding failed")
			}
		}
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal("node read-only request construction failed")
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if csrf != "" {
			req.Header.Set("X-Kudurru-CSRF", csrf)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal("node read-only HTTP transport failed")
		}
		defer func() { _ = res.Body.Close() }()
		data, err = io.ReadAll(io.LimitReader(res.Body, 2<<20))
		if err != nil || res.StatusCode != want {
			t.Fatalf("node read-only %s %s status=%d want=%d read_ok=%t", method, path, res.StatusCode, want, err == nil)
		}
		if out != nil && json.Unmarshal(data, out) != nil {
			t.Fatal("node read-only HTTP decoding failed")
		}
	}
	var before, after api.SettingsBody
	request(http.MethodGet, "/node/config", nil, http.StatusOK, &before)
	originalLevel := logLevel.Level()
	for _, path := range []string{"/node/config", "/settings?scope=node"} {
		for _, values := range []map[string]any{{"level": "debug"}, {"format": "text"}, {"level": nil, "format": nil}} {
			var refusal api.Error
			request(http.MethodPatch, path, map[string]any{"log": values}, http.StatusNotImplemented, &refusal)
			if refusal.Body.Key != api.KeyReadOnly {
				t.Fatal("missing explicit node read-only refusal")
			}
		}
	}
	request(http.MethodGet, "/node/config", nil, http.StatusOK, &after)
	if !reflect.DeepEqual(before.Settings, after.Settings) || logLevel.Level() != originalLevel {
		t.Fatal("unsupported node write changed document/logger")
	}
	var schema struct {
		Items []api.SettingKey `json:"items"`
	}
	request(http.MethodGet, "/settings/_schema", nil, http.StatusOK, &schema)
	seen := false
	for _, key := range schema.Items {
		if key.Scope == "node" && !key.ReadOnly {
			t.Fatal("actual legacy schema advertised node writes", key.Path)
		}
		if key.Path == "log.level" {
			seen = true
		}
		if key.Scope == "cluster" && key.ReadOnly {
			t.Fatal("node capability locked cluster setting", key.Path)
		}
	}
	if !seen {
		t.Fatal("actual legacy node schema disappeared")
	}
}
