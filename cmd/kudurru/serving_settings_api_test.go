package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestServingCanonicalSettingsActualHTTPAndSQLite(t *testing.T) {
	f := newServingCommandFixture(t)
	legacy := *f.s.opts.Config
	legacy.Cache.MaxEntries = 888
	legacy.SettingsFile = filepath.Join(t.TempDir(), "settings.yaml")
	contents := []byte("cache:\n  maxEntries: 999\n")
	if err := os.WriteFile(legacy.SettingsFile, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	reload := config.NewReloadable(&legacy)
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: &legacy, Reload: reload, CanonicalSettings: f.s, Authenticate: f.http.Middleware, Authorizer: f.http, Logger: slog.New(slog.DiscardHandler), Settings: api.SettingsFunc(func(context.Context, *config.Config, []string) error {
		t.Error("canonical route invoked legacy settings writer")
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://kudurru.test"+path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+f.token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	read := func(w *httptest.ResponseRecorder) api.SettingsBody {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("settings request failed: %d %s", w.Code, w.Body)
		}
		var result api.SettingsBody
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := read(request(http.MethodGet, "/api/v1/settings", ""))
	if before.Settings["cache"].(map[string]any)["maxEntries"] == float64(legacy.Cache.MaxEntries) {
		t.Fatal("GET used independent legacy configuration")
	}
	for _, patch := range []string{
		`{"cache":{"maxEntries":42},"upstreams":{"conditional":{"corp.example.":"default"}}}`,
		`{"upstreams":{"conditional":{"lab.example.":"default"}}}`,
	} {
		body := read(request(http.MethodPatch, "/api/v1/settings", patch))
		if body.Persisted == nil || !*body.Persisted {
			t.Fatal("canonical write was not acknowledged durable")
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	}
	body := read(request(http.MethodGet, "/api/v1/settings", ""))
	if body.Settings["cache"].(map[string]any)["maxEntries"] != float64(42) || len(body.Settings["upstreams"].(map[string]any)["conditional"].(map[string]any)) != 2 {
		t.Fatal("published canonical settings lost merge", body.Settings)
	}
	var stored public.Kudurru
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &stored); err != nil || !bytes.Contains(stored.Spec.Settings["cache"].Raw, []byte(`"maxEntries":42`)) {
		t.Fatal("actual SQLite singleton missing settings", err)
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	read(request(http.MethodPatch, "/api/v1/settings", `{"cache":{"maxEntries":42}}`))
	same, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !image.Generation().Same(same.Generation()) {
		t.Fatal("identical settings write changed generation", err)
	}
	for _, patch := range []string{`{"listeners":{"api":{"address":":1"}}}`, `{"cache":{"maxEntries":-1}}`, `{"unknown":1}`} {
		if w := request(http.MethodPatch, "/api/v1/settings", patch); w.Code != http.StatusBadRequest {
			t.Fatal("invalid setting reached canonical command", w.Code, w.Body)
		}
	}
	read(request(http.MethodPatch, "/api/v1/settings", `{"cache":{"maxEntries":null},"upstreams":{"conditional":{"corp.example.":null}}}`))
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	body = read(request(http.MethodGet, "/api/v1/settings", ""))
	if body.Settings["cache"].(map[string]any)["maxEntries"] != before.Settings["cache"].(map[string]any)["maxEntries"] || len(body.Settings["upstreams"].(map[string]any)["conditional"].(map[string]any)) != 1 {
		t.Fatal("null reset/map deletion failed", body.Settings)
	}
	if reload.Current() != &legacy || reload.Current().Cache.MaxEntries != 888 {
		t.Fatal("canonical command replaced independent live configuration")
	}
	file, err := os.ReadFile(legacy.SettingsFile)
	if err != nil || !bytes.Equal(file, contents) {
		t.Fatal("canonical command wrote legacy file", err)
	}
	schema := request(http.MethodGet, "/api/v1/settings/_schema", "")
	if schema.Code != http.StatusOK || bytes.Contains(schema.Body.Bytes(), []byte(`"value":888`)) {
		t.Fatal("schema did not use captured canonical configuration", schema.Code)
	}
}

func TestServingCanonicalSettingsHeldReadCannotWriteSuccessor(t *testing.T) {
	f := newServingCommandFixture(t)
	called := false
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		cfg, err := f.s.Settings(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		original := cfg.Cache.MaxEntries
		cfg.Cache.MaxEntries = 43
		if err := f.s.WriteSettings(r.Context(), cfg, []string{"cache.maxEntries"}); err != nil {
			t.Fatal(err)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		held, err := f.s.Settings(r.Context())
		if err != nil || held.Cache.MaxEntries != original {
			t.Fatal("held request adopted newer configuration or mutable caller copy", err)
		}
		cfg.Cache.MaxEntries = 44
		if err := f.s.WriteSettings(r.Context(), cfg, []string{"cache.maxEntries"}); err == nil {
			t.Fatal("old settings request overwrote successor")
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !called {
		t.Fatal("HTTP auth did not run settings test")
	}
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cfg, err := f.s.Settings(r.Context())
		if err != nil || cfg.Cache.MaxEntries != 43 {
			t.Fatal("fresh request did not capture committed settings", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
}
