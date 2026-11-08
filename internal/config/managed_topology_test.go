package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedTopologyHostOnlyAndExplicitDirect(t *testing.T) {
	key, ok := Lookup("upstreams.recursion.managedTopology")
	if !ok || !key.HostOnly || !key.RestartRequired || PublicClusterKey(key) || key.Scope != ScopeNode || key.Default != "disabled" {
		t.Fatal("managed topology must remain an explicit host-only startup declaration")
	}
	for _, tc := range []struct {
		name, topology, driver string
		proxy                  []string
		valid                  bool
	}{
		{"disabled", "disabled", "sqlite", nil, true},
		{"direct", "direct", "sqlite", nil, true},
		{"remote", "edges", "sqlite", nil, false},
		{"kube", "direct", "kube", nil, false},
		{"proxy", "direct", "sqlite", []string{"127.0.0.1/32"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := Defaults()
			flat[key.Path] = tc.topology
			flat["store.driver"] = tc.driver
			flat["listeners.proxyProtocolFrom"] = tc.proxy
			cfg, err := FromFlat(flat)
			if (err == nil) != tc.valid {
				t.Fatal("topology acceptance", err)
			}
			if !tc.valid {
				return
			}
			if _, _, err := ApplyClusterDocument(cfg, map[string]any{"upstreams": map[string]any{"recursion": map[string]any{"managedTopology": "direct"}}}); err == nil {
				t.Fatal("cluster settings replaced host topology")
			}
			actual, _, err := ApplyClusterDocument(cfg, nil)
			if err != nil || actual.Upstreams.Recursion.ManagedTopology != tc.topology {
				t.Fatal("canonical settings erased host topology", err)
			}
		})
	}
}

func TestManagedTopologySurvivesUISectionOwnership(t *testing.T) {
	for _, topology := range []string{"disabled", "direct"} {
		t.Run(topology, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.yaml")
			if err := os.WriteFile(path, []byte("upstreams:\n  recursion:\n    managedTopology: "+topology+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			defaults, err := FromFlat(Defaults())
			if err != nil {
				t.Fatal(err)
			}
			owned := ClusterSeed(defaults)["upstreams"].(map[string]any)
			// Even a section loader carrying a foreign host declaration cannot
			// replace the original file/environment topology.
			owned["recursion"].(map[string]any)["managedTopology"] = "foreign"
			load := func(*Config) (map[string]any, error) { return map[string]any{"upstreams": owned}, nil }
			cfg, err := LoadWithSections(path, nil, load)
			if err != nil || cfg.Upstreams.Recursion.ManagedTopology != topology {
				t.Fatal("UI ownership lost original topology", err)
			}
			cfg, err = LoadWithSections(path, []string{"KUDURRU_UPSTREAMS__RECURSION__MANAGEDTOPOLOGY=disabled"}, load)
			if err != nil || cfg.Upstreams.Recursion.ManagedTopology != "disabled" {
				t.Fatal("host environment lost precedence", err)
			}
		})
	}
}
