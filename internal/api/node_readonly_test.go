package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestNodeConfigWithoutDurableCommandReadOnly(t *testing.T) {
	for _, driver := range []string{"sqlite", "kube"} {
		t.Run(driver, func(t *testing.T) {
			var reload *config.Reloadable
			writes, adoptions := 0, 0
			h := newHarness(t, func(o *Options) {
				o.Config.Store.Driver = driver
				reload = config.NewReloadable(o.Config)
				o.Reload = reload
				o.Settings = SettingsFunc(func(context.Context, *config.Config, []string) error { writes++; return nil })
			})
			before := reload.Current()
			stop := reload.Subscribe(func(_, _ *config.Config, _ []string) { adoptions++ })
			defer stop()
			for _, path := range []string{"/api/v1/node/config", "/api/v1/settings?scope=node"} {
				for _, patch := range []map[string]any{
					{"log": map[string]any{"level": "debug"}},
					{"log": map[string]any{"format": "text"}},
					{"log": map[string]any{"level": nil, "format": nil}},
				} {
					r := h.do(http.MethodPatch, path, "admin", patch).want(http.StatusNotImplemented)
					if r.errKey() != KeyReadOnly {
						t.Fatal("missing explicit read-only refusal", r.errKey())
					}
				}
			}
			_, err := h.srv.patchSettings(t.Context(), config.ScopeNode, map[string]any{"log": map[string]any{"level": nil}})
			var failure *Error
			if !errors.As(err, &failure) || failure.Status != http.StatusNotImplemented || failure.Body.Key != KeyReadOnly {
				t.Fatal("direct node scope retained legacy write", err)
			}
			if reload.Current() != before || writes != 0 || adoptions != 0 {
				t.Fatal("unsupported node write escaped to legacy adoption/persistence")
			}
			var body SettingsBody
			h.do(http.MethodGet, "/api/v1/node/config", "viewer", nil).want(http.StatusOK).json(&body)
			if body.Settings["log"].(map[string]any)["level"] != before.Log.Level {
				t.Fatal("read-only node GET changed")
			}
			var schema struct {
				Items []SettingKey `json:"items"`
			}
			h.do(http.MethodGet, "/api/v1/settings/_schema", "viewer", nil).want(http.StatusOK).json(&schema)
			// Schema capability assertions follow the runtime denial, so the original
			// regression first proves the actual previously admitted write.
			if len(schema.Items) != len(config.Schema) {
				t.Fatal("read-only capability removed schema")
			}
			for _, k := range schema.Items {
				if k.ReadOnly != (k.Scope == string(config.ScopeNode)) {
					t.Fatal("incorrect scope capability", k.Path)
				}
			}
			h.do(http.MethodPatch, "/api/v1/node/config", "viewer", map[string]any{"log": map[string]any{"level": nil}}).want(http.StatusForbidden)
			h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"cache": map[string]any{"maxEntries": 42}}).want(http.StatusOK)
			if writes != 1 || reload.Current().Cache.MaxEntries != 42 {
				t.Fatal("node read-only capability blocked independent cluster writes")
			}
		})
	}
}
