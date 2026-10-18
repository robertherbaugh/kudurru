package deploy_test

import (
	"strings"
	"testing"
)

// Helm must never render the mutable API-owned settings resource, even when
// GitOps upstream values change during an upgrade or a custom name is selected.
func TestChartSettingsOwnershipResource(t *testing.T) {
	for _, tc := range []struct{ values, name string }{
		{"", "kudurru-ui-settings"},
		{"config:\n  uiSettingsResource: shared-ui-settings\n  settings:\n    upstreams:\n      groups:\n        default:\n          servers: ['192.0.2.1:53']\n", "shared-ui-settings"},
		{"config:\n  settings:\n    upstreams: null\n", "kudurru-ui-settings"},
	} {
		out, err := renderChart(t, tc.values)
		if err != nil {
			t.Fatal(err)
		}
		r := newRendered(t, out)
		r.wantNoObject(t, "ConfigMap", tc.name)
		container := r.container(t, r.pod(t), "kudurru")
		found := false
		for _, env := range list(container["env"]) {
			entry := mapOf(env)
			if entry["name"] == "KUDURRU_SETTINGS_RESOURCE" {
				found = true
				if entry["value"] != tc.name {
					t.Errorf("resource name: %v", entry["value"])
				}
			}
		}
		if !found {
			t.Fatal("shared settings resource environment missing")
		}
		if strings.Contains(tc.values, "upstreams: null") {
			file := str(dig(r.object(t, "ConfigMap", "kudurru-settings"), "data", "settings.yaml"))
			if strings.Contains(file, "upstreams:") {
				t.Fatalf("Helm did not remove its upstream defaults: %s", file)
			}
		}
	}
}
