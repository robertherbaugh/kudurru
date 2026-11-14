package config_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Exercise the real settings endpoints from the configuration package so
// that a supervisor schema change cannot accidentally open an API write path.
func TestSupervisorAPILocks(t *testing.T) {
	cfg, err := config.Load("", []string{"KUDURRU_EDGE__THREADS=3"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	srv, err := api.New(api.Options{
		Config: cfg, Store: st, Validator: v,
		Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.Grant("admin", "admin")),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", apitest.Bearer("admin"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	schemaResponse := request(http.MethodGet, "/api/v1/settings/_schema", nil)
	if schemaResponse.Code != http.StatusOK {
		t.Fatalf("schema status %d: %s", schemaResponse.Code, schemaResponse.Body.String())
	}
	var schema struct {
		Items []api.SettingKey `json:"items"`
	}
	if err := json.Unmarshal(schemaResponse.Body.Bytes(), &schema); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]api.SettingKey{}
	for _, k := range schema.Items {
		byPath[k.Path] = k
	}
	if shape := byPath["vip.vips"].Items; shape == nil || shape.Type != "object" || len(shape.Fields) != 4 {
		t.Fatal("API lost typed VIP shape")
	}
	for _, key := range config.Schema {
		if !strings.HasPrefix(key.Path, "edge.") && !strings.HasPrefix(key.Path, "vip.") {
			continue
		}
		k, ok := byPath[key.Path]
		if !ok || !k.HostOnly || !k.RestartRequired || k.Scope != "node" || k.Group != config.GroupEdge && k.Group != config.GroupVIP || k.Help == "" {
			t.Errorf("API schema lost supervisor restrictions: %+v", k)
		}
		if k.EnvLocked != (key.Path == "edge.threads") {
			t.Errorf("API environment lock for %s: %v", key.Path, k.EnvLocked)
		}
		section, leaf, _ := strings.Cut(key.Path, ".")
		for _, route := range []string{"/api/v1/settings", "/api/v1/node/config"} {
			for _, value := range []any{key.Default, nil} {
				r := request(http.MethodPatch, route, map[string]any{section: map[string]any{leaf: value}})
				var body struct {
					Error api.ErrorBody `json:"error"`
				}
				if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if r.Code != http.StatusBadRequest || body.Error.Key != api.KeyHostOnly || !strings.Contains(body.Error.Message, key.Path) {
					t.Errorf("PATCH %s %s: host-only refusal lost (status=%d)", route, key.Path, r.Code)
				}
			}
		}
	}

	// Resetting an entire host section, rather than a leaf, must not reopen a
	// write path either. Unknown group resets can be rejected as invalid.
	for _, route := range []string{"/api/v1/settings", "/api/v1/node/config"} {
		for _, section := range []string{"edge", "vip"} {
			r := request(http.MethodPatch, route, map[string]any{section: nil})
			if r.Code != http.StatusBadRequest {
				t.Errorf("host section reset accepted: %s %s", route, section)
			}
		}
	}

	for _, route := range []string{"/api/v1/settings", "/api/v1/node/config"} {
		r := request(http.MethodGet, route, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", route, r.Code, r.Body.String())
		}
		var body api.SettingsBody
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, section := range []string{"edge", "vip"} {
			values, ok := body.Settings[section].(map[string]any)
			if route == "/api/v1/node/config" {
				if !ok || values["logLevel"] != "info" || section == "edge" && values["threads"] != float64(3) {
					t.Errorf("node supervisor values missing for %s", section)
				}
			} else if ok {
				t.Errorf("node-scoped %s leaked into cluster settings", section)
			}
		}
	}
}
