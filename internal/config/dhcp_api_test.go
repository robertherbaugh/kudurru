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
// that a DHCP schema change cannot accidentally open an API write path.
func TestDHCPAPILocks(t *testing.T) {
	cfg, err := config.Load("", []string{"KUDURRU_DHCP__LEASEDURATION=24h"})
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
	if dns := byPath["dhcp.dnsServers"].Items; dns == nil || dns.Type != "string" || dns.Format != "ipv4" {
		t.Fatalf("API schema lost DNS address shape: %+v", dns)
	}
	reservations := byPath["dhcp.reservations"].Items
	if reservations == nil || reservations.Type != "object" || len(reservations.Fields) != 3 {
		t.Fatalf("API schema lost reservation shape: %+v", reservations)
	}
	for _, f := range reservations.Fields {
		if f.Help == "" || f.Required != (f.Name != "hostname") || f.Name == "ip" && f.Format != "ipv4" {
			t.Errorf("API reservation field constraints: %+v", f)
		}
	}
	for _, key := range config.Schema {
		if !strings.HasPrefix(key.Path, "dhcp.") {
			continue
		}
		k, ok := byPath[key.Path]
		if !ok || !k.HostOnly || !k.RestartRequired || k.Scope != "node" || k.Group != config.GroupDHCP || k.Help == "" {
			t.Errorf("API schema lost DHCP restrictions: %+v", k)
		}
		if k.EnvLocked != (key.Path == "dhcp.leaseDuration") {
			t.Errorf("API environment lock for %s: %v", key.Path, k.EnvLocked)
		}
		for _, route := range []string{"/api/v1/settings", "/api/v1/node/config"} {
			// Even resetting a value to its default must pass the host-only
			// gate before decoding or attempting persistence.
			r := request(http.MethodPatch, route, map[string]any{
				"dhcp": map[string]any{strings.TrimPrefix(key.Path, "dhcp."): nil},
			})
			var body struct {
				Error api.ErrorBody `json:"error"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if r.Code != http.StatusBadRequest || body.Error.Key != api.KeyHostOnly || !strings.Contains(body.Error.Message, key.Path) {
				t.Errorf("PATCH %s %s: %d %s", route, key.Path, r.Code, r.Body.String())
			}
		}
	}
	for _, route := range []string{"/api/v1/settings", "/api/v1/node/config"} {
		r := request(http.MethodPatch, route, map[string]any{"dhcp": map[string]any{"enabled": true}})
		var body struct {
			Error api.ErrorBody `json:"error"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if r.Code != http.StatusBadRequest || body.Error.Key != api.KeyHostOnly {
			t.Errorf("enabling DHCP through %s: %d %s", route, r.Code, r.Body.String())
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
		d, ok := body.Settings["dhcp"].(map[string]any)
		if route == "/api/v1/node/config" {
			if !ok || d["enabled"] != false || d["leaseDuration"] != "24h0m0s" {
				t.Errorf("node config DHCP values changed: %v", body.Settings["dhcp"])
			}
		} else if ok {
			t.Error("node-scoped DHCP keys leaked into cluster settings")
		}
	}
}
