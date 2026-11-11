package e2e

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/audit/spool"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// envtestKubeconfig starts an envtest API server with the CRDs of this
// checkout and returns a kubeconfig file for it. Without the envtest
// binaries the test skips, or fails when KUDURRU_REQUIRE_ENVTEST is set.
func envtestKubeconfig(t *testing.T) string {
	t.Helper()
	root, err := kudurrutest.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	assets := envtestAssets(root)
	if assets == "" {
		kudurrutest.Unavailable(t, kudurrutest.EnvRequireEnvtest, "envtest binaries not found: run `make envtest` or set KUBEBUILDER_ASSETS")
	}
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, glob := range []string{"deploy/kustomize/crd/bases/*.yaml", "deploy/kustomize/crd/audit/*.yaml"} {
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
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: restCfg.Host, CertificateAuthorityData: restCfg.CAData}
	kc.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: restCfg.CertData, ClientKeyData: restCfg.KeyData, Token: restCfg.BearerToken}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "admin", Namespace: "default"}
	kc.CurrentContext = "envtest"
	if err := clientcmd.WriteToFile(*kc, kubeconfig); err != nil {
		t.Fatal(err)
	}
	return kubeconfig
}

// TestBuiltBinaryRefusesNewerAuditSpool is TestBuiltBinaryRefusesNewerDatabases
// for the one local database only the Kubernetes driver has: the audit spool
// that holds a node's events until they are shipped. The binary under test
// does not open a spool written by a newer release: it exits non-zero, names
// the file and leaves it as it is, and starts again once the file is its own.
func TestBuiltBinaryRefusesNewerAuditSpool(t *testing.T) {
	kudurrutest.RequireE2E(t)
	kubeconfig := envtestKubeconfig(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	env := []string{"KUBECONFIG=" + kubeconfig, "KUDURRU_ENCRYPTION_KEY=" + hex.EncodeToString(key), "KUDURRU_ADMIN_PASSWORD=" + upgradePassword}
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "spool")
	cfg["store"] = map[string]any{"driver": "kube"}
	kudurrutest.WriteConfig(t, spec.Config, cfg)

	// The spool as this release creates it, then one schema ahead.
	if err := os.MkdirAll(spec.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spec.DataDir, spool.FileName)
	created, err := spool.Open(t.Context(), spool.Options{Path: path, Node: spec.Name})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	version := func() string {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var v string
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := version(); got != fmt.Sprint(spool.SchemaVersion) {
		t.Fatalf("spool schema %s, want %d", got, spool.SchemaVersion)
	}
	bumpSchema(t, path, 1)
	newer := version()
	if newer != fmt.Sprint(spool.SchemaVersion+1) {
		t.Fatalf("spool schema not raised: %s", newer)
	}

	n := kudurrutest.Start(t, kudurruBin, spec, kudurrutest.Options{Env: env})
	code, err := n.Wait(90 * time.Second)
	if err != nil {
		t.Fatalf("the node kept running on a newer audit spool: %v\n%s", err, n.Log())
	}
	if log := n.Log(); code == 0 || !strings.Contains(log, "error: "+spool.FileName+" was written by a newer kudurru") {
		t.Errorf("newer %s: exit %d\n%s", spool.FileName, code, log)
	}
	if got := version(); got != newer {
		t.Errorf("the refused start changed the spool schema: %s, was %s", got, newer)
	}
	bumpSchema(t, path, -1)
	ready := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
	ready.Stop(20 * time.Second)
	if got := version(); got != fmt.Sprint(spool.SchemaVersion) {
		t.Errorf("spool schema %s after the file was put back", got)
	}
}
