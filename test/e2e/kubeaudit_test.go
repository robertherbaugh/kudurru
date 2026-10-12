package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// TestKubeAuditAcrossReplicas is the acceptance test of #10: three
// `kudurru serve` replicas with the kube store driver on one apiserver
// (envtest). A change made through one replica appears in GET /audit on
// every replica, and still appears after every replica restarted with an
// empty data directory (the chart's emptyDir), because the events live in
// AuditLog batches (D157).
func TestKubeAuditAcrossReplicas(t *testing.T) {
	kudurrutest.RequireE2E(t)
	root, err := kudurrutest.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	assets := envtestAssets(root)
	if assets == "" {
		t.Skip("envtest binaries not found: run `make envtest` or set KUBEBUILDER_ASSETS")
	}
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, glob := range []string{"config/crd/bases/*.yaml", "config/crd/audit/*.yaml"} {
		files, _ := filepath.Glob(filepath.Join(root, glob))
		for _, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // the repository's CRDs
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(b, &crd); err != nil {
				t.Fatal(err)
			}
			crds = append(crds, &crd)
		}
	}
	env := &envtest.Environment{CRDs: crds, BinaryAssetsDirectory: assets}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	restCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: restCfg.Host, CertificateAuthorityData: restCfg.CAData}
	kc.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: restCfg.CertData, ClientKeyData: restCfg.KeyData, Token: restCfg.BearerToken}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "admin", Namespace: "default"}
	kc.CurrentContext = "envtest"
	if err := clientcmd.WriteToFile(*kc, kubeconfig); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	nodeEnv := []string{"KUDURRU_ENCRYPTION_KEY=" + hex.EncodeToString(key), "KUBECONFIG=" + kubeconfig}

	names := []string{"kudurru-0", "kudurru-1", "kudurru-2"}
	specs := make([]kudurrutest.NodeSpec, len(names))
	start := func() []*kudurrutest.Node {
		nodes := make([]*kudurrutest.Node, len(names))
		for i, name := range names {
			spec, cfg := kudurrutest.NewNodeSpec(t, dir, name)
			if specs[i].Name != "" {
				spec.Ports = specs[i].Ports
				cfg = kudurrutest.BaseConfig(name, spec.DataDir, spec.Ports)
			}
			specs[i] = spec
			cfg["store"] = map[string]any{"driver": "kube"}
			kudurrutest.WriteConfig(t, spec.Config, cfg)
			nodes[i] = kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: nodeEnv})
		}
		return nodes
	}
	nodes := start()

	// A token from node 0's admin socket; tokens are Secrets, valid on
	// every replica.
	out, err := exec.Command(kudurruBin, "admin", "create-token", "--socket", filepath.Join(specs[0].DataDir, "admin.sock"), "--description", "e2e").Output() //nolint:gosec // the test's binary
	if err != nil {
		t.Fatalf("create-token: %v", err)
	}
	token := strings.TrimSpace(string(out))
	call := func(n *kudurrutest.Node, method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, "http://"+n.Ports.API+"/api/v1"+path, rd)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	zone := map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone",
		"metadata": map[string]any{"name": "audit-e2e"},
		"spec": map[string]any{"zoneName": "audit-e2e.example.", "type": "primary", "nameservers": []any{"ns1.audit-e2e.example."},
			"policy": map[string]any{"queryAccess": "allow", "zoneTransfer": "deny", "notify": "none", "update": "deny"}},
	}
	if code, b := call(nodes[1], http.MethodPost, "/objects/dnszones", zone); code != http.StatusCreated {
		t.Fatalf("create zone on kudurru-1: %d %s", code, b)
	}
	if code, b := call(nodes[2], http.MethodPost, "/dns/blocking", map[string]any{"blocking": false, "timer": 60}); code != http.StatusOK {
		t.Fatalf("toggle blocking on kudurru-2: %d %s", code, b)
	}

	// auditActions maps "<action> <resource name>" to the node of the
	// events GET /audit on n reports.
	auditActions := func(n *kudurrutest.Node) (map[string]string, error) {
		code, b := call(n, http.MethodGet, "/audit?order=desc&limit=100", nil)
		if code != http.StatusOK {
			return nil, fmt.Errorf("GET /audit on %s: %d %s", n.Name, code, b)
		}
		var page struct {
			Items []struct {
				Action   string `json:"action"`
				Node     string `json:"node"`
				Resource struct {
					Name string `json:"name"`
				} `json:"resource"`
			} `json:"items"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		got := map[string]string{}
		for _, it := range page.Items {
			got[it.Action+" "+it.Resource.Name] = it.Node
		}
		return got, nil
	}
	want := map[string]string{"dnszone.create audit-e2e": "kudurru-1", "blocking.toggle ": "kudurru-2"}
	check := func(when string) {
		t.Helper()
		for _, n := range nodes {
			var got map[string]string
			var err error
			if !kudurrutest.Eventually(90*time.Second, 500*time.Millisecond, func() bool {
				got, err = auditActions(n)
				if err != nil {
					return false
				}
				for a, node := range want {
					if got[a] != node {
						return false
					}
				}
				return true
			}) {
				t.Fatalf("%s: GET /audit on %s: %v, err %v; want %v", when, n.Name, got, err, want)
			}
		}
	}
	check("before restart")

	// Every replica replaced: stopped, data directory emptied, started.
	for _, n := range nodes {
		n.Stop(20 * time.Second)
	}
	for _, s := range specs {
		if err := os.RemoveAll(s.DataDir); err != nil {
			t.Fatal(err)
		}
	}
	nodes = start()
	check("after every replica restarted with an empty data directory")
}

// envtestAssets is $KUBEBUILDER_ASSETS or the newest bin/k8s/<version>
// that `make envtest` installed.
func envtestAssets(root string) string {
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		return d
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "bin", "k8s", "*-"+goruntime.GOOS+"-"+goruntime.GOARCH))
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return ""
	}
	return dirs[len(dirs)-1]
}
