package e2e

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
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
	restCfg := startEnvtest(t, "deploy/kustomize/crd/bases/*.yaml", "deploy/kustomize/crd/audit/*.yaml")
	return writeKubeconfig(t, restCfg, restCfg.Host)
}

// startEnvtest starts an envtest API server with the CRDs the globs (below
// the module root) name and stops it at the end of the test.
func startEnvtest(t *testing.T, globs ...string) *rest.Config {
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
	for _, glob := range globs {
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
	return restCfg
}

// writeKubeconfig writes a kubeconfig for the envtest API server at
// restCfg, reached at server (its own address or a relay in front of it).
func writeKubeconfig(t *testing.T, restCfg *rest.Config, server string) string {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: restCfg.CAData}
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
	before := databaseSums(t, path)

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
	if got := databaseSums(t, path); !maps.Equal(got, before) {
		t.Errorf("the refused start changed the spool: %v, was %v", got, before)
	}
	bumpSchema(t, path, -1)
	ready := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
	ready.Stop(20 * time.Second)
	if got := version(); got != fmt.Sprint(spool.SchemaVersion) {
		t.Errorf("spool schema %s after the file was put back", got)
	}
}

// kubeUpgradeQuestions are the names TestKubeUpgradeFromV01 asks before and
// after the upgrade: records added through the zone records API and in a
// record set object, the apex NS and a name that does not exist.
var kubeUpgradeQuestions = []upgradeQuestion{
	{"www.kube-upgrade.test.", dns.TypeA}, {"ns1.kube-upgrade.test.", dns.TypeA}, {"txt.kube-upgrade.test.", dns.TypeTXT},
	{"nas.kube-upgrade.test.", dns.TypeA}, {"kube-upgrade.test.", dns.TypeNS}, {"missing.kube-upgrade.test.", dns.TypeA},
}

// kubeAnswers is answers for kubeUpgradeQuestions.
func kubeAnswers(t *testing.T, addr string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, q := range kubeUpgradeQuestions {
		for _, tcp := range []bool{false, true} {
			r := ask(t, addr, q.name, q.qtype, tcp)
			var rrs []string
			for _, rr := range r.Answer {
				rr = dns.Copy(rr)
				rr.Header().Ttl = 0
				rrs = append(rrs, rr.String())
			}
			sort.Strings(rrs)
			out[fmt.Sprintf("%s %s tcp=%t", q.name, dns.TypeToString[q.qtype], tcp)] = fmt.Sprintf("%s aa=%v %v", dns.RcodeToString[r.Rcode], r.Authoritative, rrs)
		}
	}
	return out
}

// auditLogs lists the AuditLog batches on the API server.
func auditLogs(t *testing.T, c client.Client) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "audit.kudurru.io", Version: "v1alpha1", Kind: "AuditLogList"})
	if err := c.List(t.Context(), list); err != nil {
		t.Fatalf("list AuditLogs: %v", err)
	}
	return list.Items
}

// upgradeCRDs replaces the CRDs on the API server by this checkout's, as
// `helm upgrade` of the CRD chart does, and waits until each serves its new
// schema with v1alpha1 as the only stored version.
func upgradeCRDs(t *testing.T, restCfg *rest.Config) {
	t.Helper()
	root, err := kudurrutest.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, glob := range []string{"bases", "audit", "dhcp"} {
		files, _ := filepath.Glob(filepath.Join(root, "deploy", "kustomize", "crd", glob, "*.yaml"))
		for _, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // the repository's CRDs
			if err != nil {
				t.Fatal(err)
			}
			var want apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(b, &want); err != nil {
				t.Fatal(err)
			}
			var cur apiextensionsv1.CustomResourceDefinition
			switch err := c.Get(t.Context(), client.ObjectKey{Name: want.Name}, &cur); {
			case apierrors.IsNotFound(err):
				if err := c.Create(t.Context(), &want); err != nil {
					t.Fatalf("create CRD %s: %v", want.Name, err)
				}
			case err != nil:
				t.Fatal(err)
			default:
				cur.Spec = want.Spec
				cur.Annotations = want.Annotations
				if err := c.Update(t.Context(), &cur); err != nil {
					t.Fatalf("update CRD %s: %v", want.Name, err)
				}
			}
			names = append(names, want.Name)
		}
	}
	if len(names) != 14 {
		t.Fatalf("%d CRDs in this checkout, want 14", len(names))
	}
	for _, name := range names {
		var last string
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			var cur apiextensionsv1.CustomResourceDefinition
			if err := c.Get(t.Context(), client.ObjectKey{Name: name}, &cur); err != nil {
				last = err.Error()
				return false
			}
			established := false
			var conds []string
			for _, cond := range cur.Status.Conditions {
				established = established || cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue
				conds = append(conds, fmt.Sprintf("%s=%s (%s: %s)", cond.Type, cond.Status, cond.Reason, cond.Message))
			}
			last = fmt.Sprintf("conditions %v, stored versions %v", conds, cur.Status.StoredVersions)
			return established && slices.Equal(cur.Status.StoredVersions, []string{"v1alpha1"})
		}) {
			t.Fatalf("CRD %s is not established with stored versions [v1alpha1] after the upgrade: %s", name, last)
		}
	}
}

// TestKubeUpgradeFromV01 upgrades a Kubernetes installation written by the
// 0.1.0 binary, offline, as docs/deploy/iam-upgrade.md and the AuditLog
// section of docs/deploy/runbooks.md describe it (D373):
//
//  1. The 0.1.0 binary (kudurrutest.Previous) runs with the kube driver on
//     an envtest API server that has the 0.1.0 CRDs. Through its API it
//     stores a zone, a record set and two placeholder zones this release's
//     schema refuses, mints break-glass tokens and a personal token
//     (private Secrets) and ships HMAC-sealed AuditLog batches.
//  2. It stops. The CRDs of this release replace the 0.1.0 ones.
//  3. `kudurru iam migrate status`, `plan` and `begin --offline` move the
//     private IAM configuration to public objects and report it complete,
//     with one token unbound: the first break-glass token, issued in the
//     second 0.1.0 created its service account.
//  4. `kudurru audit keys init` with the 0.1.0 encryption key as the legacy
//     master key and a cutoff after the last 0.1.0 batch writes the ring,
//     which the operator publishes as the kudurru-audit-keys Secret.
//  5. This release starts on the same data directory with the cluster ID
//     pinned: it gives the same answers over UDP and TCP, the bound 0.1.0
//     tokens authenticate as the same principal and the unbound one is
//     refused, the placeholder zones are still stored, `kudurru audit
//     verify` accepts the retained trail including the 0.1.0 batches, and a
//     write still works.
//
// The final node is the licensed fixture's binary only because
// `audit verify` needs an audit license.
func TestKubeUpgradeFromV01(t *testing.T) {
	kudurrutest.RequireE2E(t)
	previous := kudurrutest.Previous(t)
	restCfg := startEnvtest(t, "internal/store/validator/testdata/v0.1.0/crds/*.yaml", "internal/store/kube/testdata/v0.1.0/*.yaml")
	final, licensedEnv := licensedNodeFixture(t)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var system corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &system); err != nil {
		t.Fatal(err)
	}
	clusterID := string(system.UID)
	kubeconfig := writeKubeconfig(t, restCfg, restCfg.Host)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "old-current.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"KUBECONFIG=" + kubeconfig, "KUDURRU_ENCRYPTION_KEY_FILE=" + keyFile, "KUDURRU_ADMIN_PASSWORD=" + upgradePassword}
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, "kudurru-0")
	cfg["store"] = map[string]any{"driver": "kube"}
	kudurrutest.WriteConfig(t, spec.Config, cfg)

	// 1. 0.1.0.
	old := kudurrutest.StartReady(t, previous, spec, kudurrutest.Options{Env: env, Args: []string{"serve", "--config", spec.Config}})
	if log := old.Log(); !strings.Contains(log, "version="+kudurrutest.PreviousRelease) {
		t.Fatalf("not a %s node:\n%s", kudurrutest.PreviousRelease, log)
	}
	// Kubernetes creation timestamps have one-second resolution, and the
	// migration leaves a UID-less 0.1.0 token issued in the same second as
	// its account unbound (docs/deploy/iam-upgrade.md). 0.1.0 creates the
	// break-glass service account in the request that mints the first
	// break-glass token, so that token is always unbound: it is minted first
	// and must be refused after the upgrade. The admin user was created
	// during startup. The tokens that must keep working are issued in a
	// later second than both accounts.
	first := adminSocketToken(t, previous, spec, "--description", "first break-glass")
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 100*time.Millisecond)))
	tokens := map[string]string{
		"break-glass": adminSocketToken(t, previous, spec, "--description", "break-glass", "--expires-in", "720h"),
		"personal":    adminSocketToken(t, previous, spec, "--user", "admin", "--description", "personal"),
	}
	api := upgradeAPI{addr: spec.Ports.API, token: tokens["break-glass"]}
	api.must(t, http.MethodPost, "/objects/dnszones", map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "kube-upgrade"},
		"spec": map[string]any{"zoneName": "kube-upgrade.test.", "type": "primary", "nameservers": []any{"ns1.kube-upgrade.test."}},
	}, http.StatusCreated)
	api.must(t, http.MethodPost, "/objects/dnsrecordsets", map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": map[string]any{"name": "nas", "namespace": "default"},
		"spec": map[string]any{"zoneRef": map[string]any{"name": "kube-upgrade"}, "endpoints": []any{
			map[string]any{"dnsName": "nas.kube-upgrade.test", "recordType": "A", "targets": []any{"192.0.2.20"}},
		}},
	}, http.StatusCreated)
	// Placeholder zones 0.1.0 accepted and did not serve, which this
	// release's schema refuses. On SQLite they stop the upgrade (D373); on
	// Kubernetes they stay stored and the rest is served as before.
	for name, spec := range map[string]map[string]any{
		"legacy-secondary": {"zoneName": "secondary.kube-upgrade-legacy.test.", "type": "secondary"},
		"legacy-stub":      {"zoneName": "stub.kube-upgrade-legacy.test.", "type": "stub"},
	} {
		api.must(t, http.MethodPost, "/objects/dnszones", map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": name}, "spec": spec,
		}, http.StatusCreated)
	}
	record := func(c upgradeAPI, dnsName, recordType string, targets ...any) {
		t.Helper()
		var last string
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			code, b := c.do(t, http.MethodPost, "/zones/kube-upgrade/records", map[string]any{"dnsName": dnsName, "recordType": recordType, "targets": targets})
			last = fmt.Sprintf("%d %s", code, b)
			return code == http.StatusCreated || code == http.StatusOK
		}) {
			t.Fatalf("add %s %s: %s", dnsName, recordType, last)
		}
	}
	record(api, "ns1.kube-upgrade.test", "A", "127.0.0.1")
	record(api, "www.kube-upgrade.test", "A", "192.0.2.10")
	record(api, "txt.kube-upgrade.test", "TXT", "written by 0.1.0")
	var before map[string]string
	if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
		before = kubeAnswers(t, spec.Ports.DNS)
		return strings.HasPrefix(before["www.kube-upgrade.test. A tcp=true"], "NOERROR aa=true [www") &&
			strings.HasPrefix(before["nas.kube-upgrade.test. A tcp=false"], "NOERROR aa=true [nas") &&
			strings.HasPrefix(before["txt.kube-upgrade.test. TXT tcp=false"], "NOERROR aa=true [txt")
	}) {
		t.Fatalf("%s never served the zone: %v", kudurrutest.PreviousRelease, before)
	}
	if got := (upgradeAPI{addr: spec.Ports.API, token: first}).principal(t); !strings.Contains(got, "[admin]") {
		t.Fatalf("first break-glass token on %s: %s", kudurrutest.PreviousRelease, got)
	}
	principals := map[string]string{}
	for label, token := range tokens {
		principals[label] = upgradeAPI{addr: spec.Ports.API, token: token}.principal(t)
		if !strings.Contains(principals[label], "[admin]") {
			t.Fatalf("%s token on %s: %s", label, kudurrutest.PreviousRelease, principals[label])
		}
	}
	if !kudurrutest.Eventually(60*time.Second, 500*time.Millisecond, func() bool { return len(auditLogs(t, admin)) > 0 }) {
		t.Fatalf("%s shipped no AuditLog batch:\n%s", kudurrutest.PreviousRelease, old.Log())
	}

	// 2. Stop 0.1.0 and upgrade the CRDs.
	old.Stop(30 * time.Second)
	if code, err := old.Wait(time.Second); err != nil || code != 0 {
		t.Fatalf("%s did not stop cleanly: %d %v\n%s", kudurrutest.PreviousRelease, code, err, old.Log())
	}
	legacy := auditLogs(t, admin)
	var last time.Time
	for _, batch := range legacy {
		if c := batch.GetCreationTimestamp().Time; c.After(last) {
			last = c
		}
	}
	cutoff := last.Truncate(time.Second).Add(2 * time.Second)
	upgradeCRDs(t, restCfg)

	// 3. The offline IAM migration.
	finalCfg := kudurrutest.BaseConfig(spec.Name, spec.DataDir, spec.Ports)
	finalCfg["store"] = map[string]any{"driver": "kube", "kube": map[string]any{"expectedClusterID": clusterID}}
	finalConfig := filepath.Join(dir, "kudurru-0.2.yaml")
	kudurrutest.WriteConfig(t, finalConfig, finalCfg)
	migrate := func(args ...string) map[string]any {
		t.Helper()
		cmd := exec.Command(final, append([]string{"--config", finalConfig, "iam", "migrate", "--release-namespace", "default", "--kubeconfig", kubeconfig}, args...)...) //nolint:gosec // the test's binary
		cmd.Env = append(os.Environ(), env...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("iam migrate %v: %v\n%s\n%s", args, err, out, stderr.String())
		}
		var status map[string]any
		if err := json.Unmarshal(out, &status); err != nil {
			t.Fatalf("iam migrate %v printed %q: %v", args, out, err)
		}
		t.Logf("iam migrate %v: %s", args, strings.TrimSpace(string(out)))
		return status
	}
	if state := migrate("status")["State"]; state != "required" {
		t.Fatalf("iam migrate status on the %s Secrets: %v, want required", kudurrutest.PreviousRelease, state)
	}
	migrate("plan")
	migrate("begin", "--offline")
	status := migrate("status")
	if status["State"] != "complete" || status["RemoteAllowed"] != true {
		t.Fatalf("iam migrate status after begin: %v", status)
	}
	if counts, _ := status["Counts"].(map[string]any); counts["BoundTokens"] != float64(len(tokens)) || counts["UnboundTokens"] != float64(1) {
		t.Fatalf("iam migrate bound %v and left %v unbound of the %d %s tokens, want %d and 1 (the first break-glass token)", counts["BoundTokens"], counts["UnboundTokens"], len(tokens)+1, kudurrutest.PreviousRelease, len(tokens))
	}

	// 4. AuditLog trust, with the 0.1.0 key as the legacy master key.
	for time.Now().Before(cutoff) {
		time.Sleep(100 * time.Millisecond)
	}
	ringFile := filepath.Join(dir, "audit-ring.json")
	if out, err := exec.Command(final, "audit", "keys", "init", "--file", ringFile, "--cluster-id", clusterID, "--namespace", "default", //nolint:gosec // the test's binary
		"--legacy-master-file", keyFile, "--legacy-before", cutoff.UTC().Format(time.RFC3339)).CombinedOutput(); err != nil {
		t.Fatalf("audit keys init: %v\n%s", err, out)
	}
	ring, err := os.ReadFile(ringFile) //nolint:gosec // the file written above
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kudurru-audit-keys", Namespace: "default"},
		Data:       map[string][]byte{"ring.json": ring},
	}); err != nil {
		t.Fatal(err)
	}

	// 5. This release on the same data directory.
	nodeEnv := append(append([]string(nil), env...), licensedEnv[0])
	spec.Config = finalConfig
	node := kudurrutest.StartReady(t, final, spec, kudurrutest.Options{Env: nodeEnv})
	var after map[string]string
	if !kudurrutest.Eventually(60*time.Second, 200*time.Millisecond, func() bool {
		after = kubeAnswers(t, spec.Ports.DNS)
		return maps.Equal(after, before)
	}) {
		for q, a := range before {
			if after[q] != a {
				t.Errorf("%s: %s before the upgrade, %s after", q, a, after[q])
			}
		}
		t.Fatalf("this release does not answer as %s did", kudurrutest.PreviousRelease)
	}
	for label, token := range tokens {
		var got string
		if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
			got = upgradeAPI{addr: spec.Ports.API, token: token}.principal(t)
			return got == principals[label]
		}) {
			t.Fatalf("the %s token of %s after the upgrade: %s, was %s", label, kudurrutest.PreviousRelease, got, principals[label])
		}
	}
	for _, name := range []string{"legacy-secondary", "legacy-stub"} {
		api.must(t, http.MethodGet, "/objects/dnszones/"+name, nil, http.StatusOK)
	}
	if got := (upgradeAPI{addr: spec.Ports.API, token: first}).principal(t); !strings.HasPrefix(got, "401 ") {
		t.Fatalf("the unbound first break-glass token of %s after the upgrade: %s, want 401", kudurrutest.PreviousRelease, got)
	}
	api = upgradeAPI{addr: spec.Ports.API, token: tokens["break-glass"]}
	record(api, "new.kube-upgrade.test", "A", "192.0.2.30")
	if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
		r, err := exchange(spec.Ports.DNS, "new.kube-upgrade.test.", dns.TypeA, false)
		return err == nil && firstA(r) == "192.0.2.30"
	}) {
		t.Fatal("a record added after the upgrade is not served")
	}
	var out []byte
	if !kudurrutest.Eventually(60*time.Second, time.Second, func() bool {
		out, err = exec.Command(final, "audit", "verify", "--server", "http://"+spec.Ports.API, "--token", tokens["break-glass"], "-o", "json").CombinedOutput() //nolint:gosec // the test's binary
		return err == nil && len(auditLogs(t, admin)) > len(legacy)
	}) {
		t.Fatalf("kudurru audit verify after the upgrade: %v\n%s\n%d batches, %d of %s\n%s", err, out, len(auditLogs(t, admin)), len(legacy), kudurrutest.PreviousRelease, node.Log())
	}
	t.Logf("upgraded: %d answers unchanged, %d AuditLog batches of %s verified; %s", len(before), len(legacy), kudurrutest.PreviousRelease, strings.TrimSpace(string(out)))
	node.Stop(20 * time.Second)
}
