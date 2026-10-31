package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestDNSSECSettingsAPIValidatesBeforePublishing(t *testing.T) {
	var reload *config.Reloadable
	writes := 0
	h := newHarness(t, func(o *Options) {
		reload = config.NewReloadable(o.Config)
		o.Reload = reload
		o.Settings = SettingsFunc(func(context.Context, *config.Config, []string) error { writes++; return nil })
	})
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	patch := map[string]any{"dnssec": map[string]any{"enabled": true, "negativeTrustAnchors": []map[string]any{{"domain": "broken.example.", "reason": "repair pending", "expiresAt": expiry}}}}
	var body SettingsBody
	h.do(http.MethodPatch, "/api/v1/settings", "viewer", patch).want(http.StatusForbidden)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", patch).want(http.StatusOK).json(&body)
	if writes != 1 || !reload.Current().DNSSEC.Enabled || !*body.Persisted || len(body.RestartRequired) != 0 {
		t.Fatalf("DNSSEC change must be authorized, persisted and hot: %+v", body)
	}
	before := reload.Current()
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"dnssec": map[string]any{"useBuiltinAnchors": false}}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"dnssec": map[string]any{"negativeTrustAnchors": []map[string]any{{"domain": "example.", "reason": "", "expiresAt": expiry}}}}).want(http.StatusBadRequest)
	if reload.Current() != before || writes != 1 {
		t.Fatal("invalid trust changes must not publish or persist")
	}
	h.do(http.MethodGet, "/api/v1/settings", "viewer", nil).want(http.StatusOK).json(&body)
	settings := body.Settings["dnssec"].(map[string]any)
	if settings["enabled"] != true || settings["negativeTrustAnchors"].([]any)[0].(map[string]any)["expiresAt"] != expiry {
		t.Fatal("readback lost validation setting or absolute exception expiry")
	}
}

// TestSettingsPatchThroughReloadable: with Options.Reload the PATCH is
// applied with Reloadable.Set, so the hot-reload subscribers see it, and
// node-scope changes never reach the settings writer.
func TestSettingsPatchThroughReloadable(t *testing.T) {
	var writes [][]string
	var reload *config.Reloadable
	h := newHarness(t, func(o *Options) {
		reload = config.NewReloadable(o.Config)
		o.Reload = reload
		o.Settings = SettingsFunc(func(_ context.Context, _ *config.Config, changed []string) error {
			writes = append(writes, changed)
			return nil
		})
	})
	var seen []string
	reload.Subscribe(func(_, _ *config.Config, changed []string) { seen = append(seen, changed...) })

	var body SettingsBody
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"ratelimit": map[string]any{"slip": 10}}).want(http.StatusOK).json(&body)
	if !*body.Persisted || reload.Current().RateLimit.Slip != 10 || !slices.Equal(seen, []string{"ratelimit.slip"}) {
		t.Fatalf("persisted %v slip %d subscribers saw %v", *body.Persisted, reload.Current().RateLimit.Slip, seen)
	}
	before := reload.Current()
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "warn"}}).want(http.StatusNotImplemented)
	if reload.Current() != before || len(writes) != 1 || !slices.Equal(seen, []string{"ratelimit.slip"}) {
		t.Fatal("unsupported node write reached reload/subscribers/writer")
	}

	// A reload from elsewhere (the settings file) is what GET serves.
	next := *reload.Current()
	next.Cache.MaxEntries = 77
	reload.Set(&next)
	h.do(http.MethodGet, "/api/v1/settings", "viewer", nil).want(http.StatusOK).json(&body)
	if body.Settings["cache"].(map[string]any)["maxEntries"] != float64(77) {
		t.Fatalf("GET after reload %v", body.Settings["cache"])
	}
}

func TestSettingsManagedByKubernetes(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Settings = SettingsFunc(func(context.Context, *config.Config, []string) error {
			return ErrSettingsManaged("Kubernetes (ConfigMap kudurru-settings)", "helm upgrade --set config.settings...")
		})
	})
	r := h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"cache": map[string]any{"maxEntries": 10}}).want(http.StatusConflict)
	if r.errKey() != KeyManagedBy {
		t.Fatalf("key %q: %s", r.errKey(), r.Body.String())
	}
	if h.srv.config().Cache.MaxEntries == 10 {
		t.Fatal("a refused write must not change the in-memory settings")
	}
	// A cluster settings writer never qualifies memory-only node writes.
	r = h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "warn"}}).want(http.StatusNotImplemented)
	if r.errKey() != KeyReadOnly {
		t.Fatal("unsupported Kubernetes node write lacks capability refusal")
	}
}
