package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedEdgesExhaustiveHostConfiguration(t *testing.T) {
	const path = "upstreams.recursion.managedEdges"
	key, ok := Lookup(path)
	if !ok || !key.HostOnly || !key.RestartRequired || PublicClusterKey(key) || key.Scope != ScopeNode {
		t.Fatal("edge fleet must be retained host-only startup input")
	}
	for _, name := range []string{"valid", "empty", "duplicate", "wrong-digest", "zero-digest", "wrong-address", "missing-backend", "kube", "unnamed", "direct", "oversized"} {
		t.Run(name, func(t *testing.T) {
			flat := Defaults()
			flat["upstreams.recursion.managedTopology"] = "edges"
			flat["node.name"] = "backend-a"
			edge := map[string]any{"node": "edge-a", "address": "192.0.2.2:8085", "deployment": strings.Repeat("a", 64), "backendAddress": "192.0.2.1:5353", "backendInternalAddress": "192.0.2.1:8082"}
			fleet := []map[string]any{edge}
			switch name {
			case "empty":
				fleet = nil
			case "duplicate":
				fleet = append(fleet, edge)
			case "wrong-digest":
				edge["deployment"] = strings.Repeat("A", 64)
			case "zero-digest":
				edge["deployment"] = strings.Repeat("0", 64)
			case "wrong-address":
				edge["address"] = "https://edge.example:8085"
			case "missing-backend":
				delete(edge, "backendInternalAddress")
			case "kube":
				flat["store.driver"] = "kube"
			case "unnamed":
				flat["node.name"] = ""
			case "direct":
				flat["upstreams.recursion.managedTopology"] = "direct"
			case "oversized":
				fleet = make([]map[string]any, 33)
			}
			flat[path] = fleet
			cfg, err := FromFlat(flat)
			if (err == nil) != (name == "valid") {
				t.Fatal("edge declaration acceptance", err)
			}
			if err != nil {
				return
			}
			actual, _, err := ApplyClusterDocument(cfg, nil)
			if err != nil || len(actual.Upstreams.Recursion.ManagedEdges) != 1 || actual.Upstreams.Recursion.ManagedEdges[0].Deployment != edge["deployment"] {
				t.Fatal("cluster composition erased declared edge", err)
			}
			if _, _, err := ApplyClusterDocument(cfg, map[string]any{"upstreams": map[string]any{"recursion": map[string]any{"managedEdges": fleet}}}); err == nil {
				t.Fatal("cluster settings replaced host edge declaration")
			}
		})
	}
}

func TestManagedEdgesSurviveUIOwnedUpstreamsAndHostPrecedence(t *testing.T) {
	fileEdge := ManagedRootEdge{Node: "edge-a", Address: "192.0.2.2:8085", Deployment: strings.Repeat("a", 64), BackendAddress: "192.0.2.1:5353", BackendInternalAddress: "192.0.2.1:8082"}
	file, err := json.Marshal(map[string]any{"node": map[string]any{"name": "backend-a"}, "upstreams": map[string]any{"recursion": map[string]any{"managedTopology": "edges", "managedEdges": []ManagedRootEdge{fileEdge}}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	defaults, err := FromFlat(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	public := ClusterSeed(defaults)["upstreams"].(map[string]any)
	public["recursion"].(map[string]any)["managedEdges"] = []map[string]any{{"node": "foreign"}}
	load := func(*Config) (map[string]any, error) { return map[string]any{"upstreams": public}, nil }
	cfg, err := LoadWithSections(path, nil, load)
	if err != nil || len(cfg.Upstreams.Recursion.ManagedEdges) != 1 || cfg.Upstreams.Recursion.ManagedEdges[0] != fileEdge {
		t.Fatal("UI-owned section replaced original host fleet", err)
	}
	cfg, err = LoadWithSections(path, []string{"KUDURRU_UPSTREAMS__RECURSION__MANAGEDTOPOLOGY=disabled"}, load)
	if err != nil || cfg.Upstreams.Recursion.ManagedTopology != "disabled" || len(cfg.Upstreams.Recursion.ManagedEdges) != 1 || cfg.Upstreams.Recursion.ManagedEdges[0] != fileEdge {
		t.Fatal("host topology override lost precedence or erased fleet", err)
	}
	value, err := json.Marshal([]ManagedRootEdge{fileEdge})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithSections(path, []string{"KUDURRU_UPSTREAMS__RECURSION__MANAGEDEDGES=" + string(value)}, load); err == nil {
		t.Fatal("fleet list bypassed existing environment collection refusal")
	}
}
