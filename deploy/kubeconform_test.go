package deploy_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// KUDURRU_KUBECONFORM=1 turns on schema validation of the rendered chart and
// the kustomize overlays with kubeconform (Apache-2.0) against the
// Kubernetes 1.30 schemas, the kudurru CRDs and the Prometheus Operator CRDs
// from the datreeio CRDs-catalog. It downloads schemas, so it is off in
// `make test` and on in `make helm-check` and the CI helm job.
const kubeconformEnv = "KUDURRU_KUBECONFORM"

// kubernetesVersion is the minimum supported Kubernetes (§5.7).
const kubernetesVersion = "1.30.0"

func kubeconformBin(t *testing.T) string {
	t.Helper()
	if os.Getenv(kubeconformEnv) == "" {
		t.Skipf("set %s=1 to validate with kubeconform (make helm-check)", kubeconformEnv)
	}
	p, err := exec.LookPath("kubeconform")
	if err != nil {
		t.Fatalf("%s is set but kubeconform is not installed: go install github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0", kubeconformEnv)
	}
	return p
}

// crdSchemaDir converts deploy/kustomize/crd/bases into kubeconform's JSON schema
// layout <dir>/<group>/<kind>_<version>.json, with additionalProperties
// false wherever the CRD lists properties and does not preserve unknown
// fields (what kubeconform's openapi2jsonschema -strict does).
func crdSchemaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bases, err := filepath.Glob(filepath.Join(repoRoot(t), "deploy/kustomize/crd/bases/*.yaml"))
	if err != nil || len(bases) == 0 {
		t.Fatalf("no CRDs: %v", err)
	}
	for _, b := range bases {
		raw, err := os.ReadFile(b) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		for _, crd := range decodeDocs(t, raw) {
			group := str(dig(crd, "spec", "group"))
			kind := strings.ToLower(str(dig(crd, "spec", "names", "kind")))
			for _, v := range list(dig(crd, "spec", "versions")) {
				schema := mapOf(dig(v, "schema", "openAPIV3Schema"))
				strictify(schema)
				out, err := json.MarshalIndent(schema, "", " ")
				if err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(dir, group, kind+"_"+str(mapOf(v)["name"])+".json")
				if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, out, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return dir
}

func strictify(s map[string]any) {
	if s == nil {
		return
	}
	props := mapOf(s["properties"])
	if props != nil {
		if _, set := s["additionalProperties"]; !set && s["x-kubernetes-preserve-unknown-fields"] != true {
			s["additionalProperties"] = false
		}
		for _, p := range props {
			strictify(mapOf(p))
		}
	}
	strictify(mapOf(s["items"]))
	if ap := mapOf(s["additionalProperties"]); ap != nil {
		strictify(ap)
	}
}

// runKubeconform validates files and fails with kubeconform's report.
func runKubeconform(t *testing.T, files ...string) {
	t.Helper()
	bin := kubeconformBin(t)
	schemas := crdSchemaDir(t)
	args := []string{
		"-kubernetes-version", kubernetesVersion,
		"-strict", "-summary", "-output", "text",
		"-schema-location", "default",
		"-schema-location", filepath.Join(schemas, "{{.Group}}", "{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"),
		"-schema-location", "https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json",
	}
	if cache := os.Getenv("KUBECONFORM_CACHE"); cache != "" {
		if err := os.MkdirAll(cache, 0o750); err != nil { //nolint:gosec // cache directory chosen by the operator
			t.Fatal(err)
		}
		args = append(args, "-cache", cache)
	}
	args = append(args, files...)
	out, err := exec.Command(bin, args...).CombinedOutput() //nolint:gosec // fixed binary, test arguments
	if err != nil {
		t.Fatalf("kubeconform: %v\n%s", err, out)
	}
	t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
}

func TestChartKubeconform(t *testing.T) {
	kubeconformBin(t)
	dir := t.TempDir()
	var files []string
	for _, tc := range chartCases {
		out, err := renderChart(t, tc.values)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		p := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(p, out, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	// The CRDs themselves are validated by the apiserver in the envtest
	// suites; the default schema registry has no strict CRD schema for 1.30.
	runKubeconform(t, files...)
}
