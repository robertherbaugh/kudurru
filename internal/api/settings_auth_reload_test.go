package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestAuthControlsSchemaRequiresExplicitRuntimeCapability(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "canonical-reader-only"}[canonical], func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				if canonical {
					o.CanonicalSettings = policyReadSettings{cfg: o.Config}
				} else {
					o.Settings = SettingsFunc(func(context.Context, *config.Config, []string) error { return nil })
				}
			})
			var schema struct {
				Items []SettingKey `json:"items"`
			}
			h.do(http.MethodGet, "/api/v1/settings/_schema", "viewer", nil).want(http.StatusOK).json(&schema)
			for _, key := range schema.Items {
				if strings.HasPrefix(key.Path, "auth.") && !key.RestartRequired {
					t.Fatal("unmounted auth reload was advertised")
				}
			}
			if !canonical {
				var out SettingsBody
				h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"auth": map[string]any{"localLogin": "disabled"}}).want(http.StatusOK).json(&out)
				if len(out.RestartRequired) != 1 || out.RestartRequired[0] != "auth.localLogin" {
					t.Fatal("legacy PATCH lost restart classification")
				}
			}
		})
	}
}
