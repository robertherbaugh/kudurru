package deploy_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"
)

func TestChartStoragePaths(t *testing.T) {
	for _, tc := range []struct {
		name, values, kind     string
		replicas               int
		claims                 []string
		splitQuery, splitLocal bool
	}{
		{name: "default", kind: "Deployment", replicas: 2},
		{name: "legacy-query", values: "persistence: {querylog: {enabled: true}}", kind: "StatefulSet", replicas: 2, claims: []string{"data"}},
		{name: "legacy-local", values: "persistence: {local: {enabled: true}}", kind: "StatefulSet", replicas: 2, claims: []string{"data"}},
		{name: "legacy-both-precedence", values: "persistence: {querylog: {enabled: true, size: 12Gi}, local: {enabled: true, size: 2Gi}}", kind: "StatefulSet", replicas: 2, claims: []string{"data"}},
		{name: "query", values: "persistence: {querylog: {enabled: true, separate: true}}", kind: "StatefulSet", replicas: 2, claims: []string{"data", "querylog"}, splitQuery: true},
		{name: "local-single", values: "replicaCount: 1\npersistence: {local: {enabled: true, separate: true}}", kind: "StatefulSet", replicas: 1, claims: []string{"data", "local"}, splitLocal: true},
		{name: "both-multi", values: "replicaCount: 3\npersistence: {querylog: {enabled: true, separate: true}, local: {enabled: true, separate: true}}", kind: "StatefulSet", replicas: 3, claims: []string{"data", "local", "querylog"}, splitQuery: true, splitLocal: true},
		{name: "split-query-legacy-local", values: "persistence: {querylog: {enabled: true, separate: true}, local: {enabled: true}}", kind: "StatefulSet", replicas: 2, claims: []string{"data", "querylog"}, splitQuery: true},
		{name: "split-local-legacy-query", values: "persistence: {querylog: {enabled: true}, local: {enabled: true, separate: true}}", kind: "StatefulSet", replicas: 2, claims: []string{"data", "local"}, splitLocal: true},
		{name: "ephemeral", values: "persistence: {querylog: {separate: true}, local: {separate: true}}", kind: "Deployment", replicas: 2, splitQuery: true, splitLocal: true},
		{name: "standalone", values: "mode: standalone\nreplicaCount: 3\npersistence: {querylog: {enabled: true, separate: true}, local: {enabled: true, separate: true}}", kind: "StatefulSet", replicas: 1, claims: []string{"data", "local", "querylog"}, splitQuery: true, splitLocal: true},
		{name: "standalone-local-only", values: "mode: standalone\npersistence: {standalone: {enabled: false}, local: {enabled: true, separate: true}}", kind: "StatefulSet", replicas: 1, claims: []string{"local"}, splitLocal: true},
		{name: "standalone-ephemeral", values: "mode: standalone\npersistence: {standalone: {enabled: false}, querylog: {separate: true}, local: {separate: true}}", kind: "StatefulSet", replicas: 1, splitQuery: true, splitLocal: true},
		{name: "hostnetwork-ephemeral", values: "hostNetwork: {enabled: true}\npersistence: {querylog: {separate: true}, local: {separate: true}}", kind: "DaemonSet", splitQuery: true, splitLocal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderChart(t, tc.values)
			if err != nil {
				t.Fatal(err)
			}
			r := newRendered(t, out)
			r.checkGeneric(t)
			if r.workloadKind(t) != tc.kind {
				t.Fatal("wrong workload kind")
			}
			if tc.kind != "DaemonSet" && dig(r.workload(t), "spec", "replicas") != tc.replicas {
				t.Fatal("replica compatibility changed")
			}
			claims := list(dig(r.workload(t), "spec", "volumeClaimTemplates"))
			var names []string
			for _, raw := range claims {
				name := str(dig(raw, "metadata", "name"))
				names = append(names, name)
				if !slices.Equal(list(dig(raw, "spec", "accessModes")), []any{"ReadWriteOnce"}) {
					t.Fatal("claim access mode changed")
				}
				if dig(raw, "spec", "resources", "requests", "storage") == nil {
					t.Fatal("claim lacks capacity")
				}
			}
			slices.Sort(names)
			if !slices.Equal(names, tc.claims) {
				t.Fatalf("claims=%v want %v", names, tc.claims)
			}
			if len(claims) > 0 && dig(r.workload(t), "spec", "persistentVolumeClaimRetentionPolicy", "whenDeleted") != "Retain" {
				t.Fatal("claim retention weakened")
			}
			if tc.name == "legacy-both-precedence" && dig(claims[0], "spec", "resources", "requests", "storage") != "12Gi" {
				t.Fatal("legacy querylog claim precedence changed")
			}
			cfg := r.config(t)
			if cfg.Node.DataDir != "/var/lib/kudurru" {
				t.Fatal("default dataDir changed")
			}
			for _, item := range []struct {
				name, actual string
				split        bool
			}{{"querylog", cfg.Node.QuerylogDir, tc.splitQuery}, {"local", cfg.Node.LocalDir, tc.splitLocal}} {
				want := ""
				if item.split {
					want = "/var/lib/kudurru-" + item.name
				}
				if item.actual != want {
					t.Fatalf("%s path=%q want=%q", item.name, item.actual, want)
				}
				found := false
				for _, mount := range list(r.container(t, r.pod(t), "kudurru")["volumeMounts"]) {
					if dig(mount, "name") == item.name {
						found = true
						if dig(mount, "mountPath") != want {
							t.Fatal("split volume mounted at wrong path")
						}
					}
				}
				if found != item.split {
					t.Fatal("split mount opt-in changed")
				}
				if item.split && !slices.Contains(names, item.name) {
					r.wantVolume(t, item.name, "emptyDir")
				}
			}
			if !slices.Contains(names, "data") {
				r.wantVolume(t, "data", "emptyDir")
			}
			if dig(r.pod(t), "spec", "securityContext", "fsGroup") != 65532 || dig(r.container(t, r.pod(t), "kudurru"), "securityContext", "runAsUser") != 65532 {
				t.Fatal("storage identity changed")
			}
		})
	}
}

func TestChartStorageRefusals(t *testing.T) {
	for _, name := range []string{"querylog", "local"} {
		for _, tc := range []struct{ name, values, want string }{
			{"daemonset", fmt.Sprintf("hostNetwork: {enabled: true}\npersistence: {%s: {enabled: true, separate: true}}", name), "DaemonSet"},
			{"shared", fmt.Sprintf("persistence: {%s: {enabled: true, separate: true, accessModes: [ReadWriteMany]}}", name), "ReadWriteOnce"},
			{"override", fmt.Sprintf("persistence: {%s: {separate: true}}\nconfig: {node: {node: {%sDir: /elsewhere}}}", name, name), "requires config.node.node"},
		} {
			t.Run(name+"-"+tc.name, func(t *testing.T) {
				if _, err := renderChart(t, tc.values); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatal("expected storage refusal", err)
				}
			})
		}
	}
}

func TestComposeStorageOverrides(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"querylog", "local"} {
		path := filepath.Join(root, "deploy/compose/storage-"+name+".yml")
		raw, err := os.ReadFile(path) //nolint:gosec // Repository file selected from two fixed override names.
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yamlv3.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		service := mapOf(dig(doc, "services", "kudurru"))
		env := mapOf(service["environment"])
		key := "KUDURRU_NODE__" + strings.ToUpper(name) + "DIR"
		target := "/var/lib/kudurru-" + name
		if len(env) != 1 || env[key] != target {
			t.Fatal("wrong override environment")
		}
		if !slices.Equal(list(service["volumes"]), []any{"kudurru-" + name + ":" + target}) || len(mapOf(doc["volumes"])) != 1 {
			t.Fatal("override must mount only its independent volume")
		}
		// Merge each opt-in into every base using the same map/list rules needed
		// here, then reuse the existing full service/config validation.
		for _, dir := range composeDirs {
			b, err := os.ReadFile(filepath.Join(root, dir, "docker-compose.yml")) //nolint:gosec // Repository file from the fixed Compose base matrix.
			if err != nil {
				t.Fatal(err)
			}
			var base map[string]any
			if err := yamlv3.Unmarshal(b, &base); err != nil {
				t.Fatal(err)
			}
			services := mapOf(base["services"])
			s := mapOf(services["kudurru"])
			mapOf(s["environment"])[key] = target
			s["volumes"] = append(list(s["volumes"]), list(service["volumes"])...)
			volumes := mapOf(base["volumes"])
			volumes["kudurru-"+name] = map[string]any{}
			checkComposeService(t, filepath.Join(root, dir), "kudurru", s, services, volumes, mapOf(base["secrets"]))
		}
	}
}

func TestChartStorageKeepsOriginalDataClaim(t *testing.T) {
	for _, tc := range []struct{ name, values string }{
		{"query", "persistence: {querylog: {enabled: true, size: 13Gi, storageClass: query-original}}"},
		{"local", "persistence: {local: {enabled: true, size: 3Gi, storageClass: local-original}}"},
		{"both", "persistence: {querylog: {enabled: true, size: 13Gi, storageClass: query-original}, local: {enabled: true, size: 3Gi, storageClass: local-original}}"},
		{"standalone", "mode: standalone\npersistence: {standalone: {enabled: true, size: 7Gi, storageClass: state-original}, querylog: {enabled: true}, local: {enabled: true}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, err := renderChart(t, tc.values)
			if err != nil {
				t.Fatal(err)
			}
			baseline := newRendered(t, original)
			dataClaim := func(r *rendered) any {
				for _, claim := range list(dig(r.workload(t), "spec", "volumeClaimTemplates")) {
					if dig(claim, "metadata", "name") == "data" {
						return claim
					}
				}
				t.Fatal("data claim missing")
				return nil
			}
			want := dataClaim(baseline)
			for _, selection := range []string{"querylog", "local", "both"} {
				// Append only separate flags inside the same values tree, without
				// replacing original capacity/class values through duplicate YAML keys.
				var values map[string]any
				if err := yamlv3.Unmarshal([]byte(tc.values), &values); err != nil {
					t.Fatal(err)
				}
				p := mapOf(values["persistence"])
				for _, component := range []string{"querylog", "local"} {
					if selection != "both" && selection != component {
						continue
					}
					if p[component] == nil {
						p[component] = map[string]any{}
					}
					mapOf(p[component])["separate"] = true
				}
				raw, err := yamlv3.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				out, err := renderChart(t, string(raw))
				if err != nil {
					t.Fatal(err)
				}
				r := newRendered(t, out)
				r.checkGeneric(t)
				if !reflect.DeepEqual(dataClaim(r), want) {
					t.Fatalf("%s split changed original data claim", selection)
				}
				found := false
				for _, mount := range list(r.container(t, r.pod(t), "kudurru")["volumeMounts"]) {
					if dig(mount, "name") == "data" {
						found = true
						if dig(mount, "mountPath") != "/var/lib/kudurru" {
							t.Fatal("data mount moved")
						}
					}
				}
				if !found || r.config(t).Node.DataDir != "/var/lib/kudurru" {
					t.Fatal("data directory durability changed")
				}
			}
		})
	}
}
