package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

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
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "warn"}}).want(http.StatusOK).json(&body)
	if *body.Persisted || reload.Current().Log.Level != "warn" || len(writes) != 1 {
		t.Fatalf("node config: persisted %v level %q writes %v", *body.Persisted, reload.Current().Log.Level, writes)
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
	// Node config is not the writer's business.
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"log": map[string]any{"level": "warn"}}).want(http.StatusOK)
}
