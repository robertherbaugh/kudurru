package deploy_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"
)

// The kustomize tree under config/ (§5.7) is derived from the chart so the
// two install paths cannot drift: the bases config/rbac and config/manager
// and the overlays that change the workload shape (hostnetwork-daemonset,
// standalone, with-edge) are rendered with the values below and committed.
// Regenerate with
//
//	go test ./deploy -run TestKustomizeBases -update   (make kustomize)
//
// The metallb, kube-vip and cloud-lb-mixed overlays are hand-written
// patches over config/default.

// kustomizeCommon names pre-created Secrets instead of chart-generated ones:
// kustomize has no random generator, and docs/deploy/kustomize.md creates them.
const kustomizeCommon = `auth:
  encryptionKeySecret: kudurru-encryption-key
  bootstrapAdminSecret: kudurru-admin
`

type kustomizeBase struct {
	dir    string // output file, relative to the repo root
	values string
	// keep selects the documents for this file.
	keep func(doc map[string]any) bool
}

var rbacKinds = []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"}

func isRBAC(d map[string]any) bool { return slices.Contains(rbacKinds, str(d["kind"])) }

// inDefaultRBAC is true for the documents config/rbac already carries.
func inDefaultRBAC(d map[string]any) bool {
	return isRBAC(d) && !strings.HasPrefix(str(dig(d, "metadata", "name")), "kudurru-edge")
}

var kustomizeBases = []kustomizeBase{
	{dir: "config/rbac/rbac.yaml", values: kustomizeCommon, keep: isRBAC},
	{dir: "config/manager/manager.yaml", values: kustomizeCommon, keep: func(d map[string]any) bool { return !isRBAC(d) }},
	{dir: "config/overlays/hostnetwork-daemonset/resources.yaml", values: kustomizeCommon + "hostNetwork: {enabled: true}\n", keep: func(d map[string]any) bool { return !inDefaultRBAC(d) }},
	{dir: "config/overlays/with-edge/resources.yaml", values: kustomizeCommon + "edge: {enabled: true, existingSecret: kudurru-proxy-protocol}\n", keep: func(d map[string]any) bool { return !inDefaultRBAC(d) }},
	{dir: "config/overlays/standalone/resources.yaml", values: kustomizeCommon + "mode: standalone\n", keep: func(map[string]any) bool { return true }},
}

// kustomizeDoc strips the Helm-only labels from a rendered document.
func kustomizeDoc(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "labels" {
				if l := mapOf(child); l != nil {
					delete(l, "helm.sh/chart")
					if _, ok := l["app.kubernetes.io/managed-by"]; ok {
						l["app.kubernetes.io/managed-by"] = "kustomize"
					}
				}
			}
			kustomizeDoc(child)
		}
	case []any:
		for _, child := range x {
			kustomizeDoc(child)
		}
	}
}

func TestKustomizeBases(t *testing.T) {
	root := repoRoot(t)
	for _, b := range kustomizeBases {
		t.Run(b.dir, func(t *testing.T) {
			out, err := renderChart(t, b.values)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			buf.WriteString("# Generated from charts/kudurru by `go test ./deploy -run TestKustomizeBases -update`\n# (make kustomize). Do not edit; change the chart and regenerate.\n")
			for _, d := range decodeDocs(t, out) {
				if !b.keep(d) {
					continue
				}
				kustomizeDoc(d)
				buf.WriteString("---\n")
				enc := yamlv3.NewEncoder(&buf)
				enc.SetIndent(2)
				if err := enc.Encode(d); err != nil {
					t.Fatal(err)
				}
				if err := enc.Close(); err != nil {
					t.Fatal(err)
				}
			}
			writeOrCompare(t, filepath.Join(root, b.dir), buf.Bytes(), "go test ./deploy -run TestKustomizeBases -update")
		})
	}
}

// overlays lists every kustomization the tests build and validate.
var overlays = []struct {
	dir      string
	wantKind string
}{
	{"config/default", "Deployment"},
	{"config/overlays/metallb", "Deployment"},
	{"config/overlays/kube-vip", "Deployment"},
	{"config/overlays/cloud-lb-mixed", "Deployment"},
	{"config/overlays/hostnetwork-daemonset", "DaemonSet"},
	{"config/overlays/standalone", "StatefulSet"},
	{"config/overlays/with-edge", "Deployment"},
}

func kustomizeBuild(t *testing.T, dir string) []byte {
	t.Helper()
	bin, err := exec.LookPath("kustomize")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("kustomize is required in CI")
		}
		t.Skip("kustomize not installed (go install sigs.k8s.io/kustomize/kustomize/v5@v5.8.0)")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "build", filepath.Join(repoRoot(t), dir)) //nolint:gosec // fixed binary, test arguments
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kustomize build %s: %v\n%s", dir, err, stderr.String())
	}
	return out
}

func TestKustomizeOverlays(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for _, o := range overlays {
		t.Run(o.dir, func(t *testing.T) {
			out := kustomizeBuild(t, o.dir)
			r := newRendered(t, out)
			r.externalSecrets = true
			r.checkGeneric(t)
			if k := r.workloadKind(t); k != o.wantKind {
				t.Errorf("workload is a %s, want %s", k, o.wantKind)
			}
			r.config(t)
			if o.dir != "config/overlays/standalone" {
				r.wantObject(t, "CustomResourceDefinition", "dnszones.kudurru.io")
			}
			r.wantObject(t, "Namespace", releaseNS)
			p := filepath.Join(dir, strings.ReplaceAll(o.dir, "/", "_")+".yaml")
			if err := os.WriteFile(p, out, 0o600); err != nil {
				t.Fatal(err)
			}
			files = append(files, p)
		})
	}
	if os.Getenv(kubeconformEnv) != "" && len(files) == len(overlays) {
		var noCRDs []string
		for _, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // test output
			if err != nil {
				t.Fatal(err)
			}
			var keep bytes.Buffer
			for _, d := range decodeDocs(t, b) {
				if d["kind"] == "CustomResourceDefinition" {
					continue
				}
				keep.WriteString("---\n")
				enc := yamlv3.NewEncoder(&keep)
				if err := enc.Encode(d); err != nil {
					t.Fatal(err)
				}
				_ = enc.Close()
			}
			p := f + ".nocrd.yaml"
			if err := os.WriteFile(p, keep.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			noCRDs = append(noCRDs, p)
		}
		runKubeconform(t, noCRDs...)
	}
}
