package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// testNamespaces are created in every test control plane: the conformance
// suite writes DNSRecordSets into "apps" and "dns", and the apiserver's
// NamespaceLifecycle admission rejects objects in missing namespaces.
var testNamespaces = []string{"apps", "dns", "kudurru-system", "measure"}

// shared is the control plane shared by the tests of this package.
var shared struct {
	cfg   *rest.Config
	admin client.Client
	skip  string // non-empty: envtest is unavailable and tests skip
}

func TestMain(m *testing.M) {
	// controller-runtime and client-go log through logr/klog; keep test
	// output readable. Set KUDURRU_TEST_LOGS=1 to see them.
	if os.Getenv("KUDURRU_TEST_LOGS") == "" {
		ctrllog.SetLogger(logr.Discard())
		klog.SetLogger(logr.Discard())
	}
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	assets, reason := findEnvtestAssets()
	if reason != "" {
		shared.skip = reason
		return m.Run()
	}
	env, cfg, admin, err := startControlPlane(assets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store/kube: envtest failed to start with assets in %s: %v\n", assets, err)
		return 1
	}
	defer func() { _ = env.Stop() }()
	shared.cfg, shared.admin = cfg, admin
	return m.Run()
}

// testLogger discards driver logs unless KUDURRU_TEST_LOGS is set.
func testLogger() *slog.Logger {
	if os.Getenv("KUDURRU_TEST_LOGS") != "" {
		return slog.Default()
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// requireEnv skips the test when envtest binaries are unavailable.
func requireEnv(t testing.TB) {
	t.Helper()
	if shared.skip != "" {
		t.Skip(shared.skip)
	}
}

// startControlPlane starts kube-apiserver and etcd with the kudurru CRDs
// installed and the test namespaces created.
func startControlPlane(assets string) (*envtest.Environment, *rest.Config, client.Client, error) {
	crds, err := loadCRDs(filepath.Join(repoRoot(), "config", "crd", "bases"))
	if err != nil {
		return nil, nil, nil, err
	}
	env := &envtest.Environment{CRDs: crds, BinaryAssetsDirectory: assets}
	// Hosts without a default route (CI sandboxes) make kube-apiserver
	// fail to pick an advertise address. Loopback works everywhere once the
	// endpoint reconciler (which publishes default/kubernetes endpoints,
	// unused here) is off.
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		return nil, nil, nil, err
	}
	admin, err := client.New(cfg, client.Options{Scheme: testScheme()})
	if err != nil {
		_ = env.Stop()
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, ns := range testNamespaces {
		err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			_ = env.Stop()
			return nil, nil, nil, err
		}
	}
	return env, cfg, admin, nil
}

// celBudgetPatches bounds arrays whose items carry a CEL rule. Without
// maxItems the apiserver estimates the rule cost for an unbounded list and
// rejects the generated BlockList and ClientGroup CRDs ("estimated rule
// cost exceeds budget by factor of 7.1x" / "1.26x") on Kubernetes 1.37.
// The fix belongs in api/v1alpha1 (+kubebuilder:validation:MaxItems on
// BlockListSpec.Sources and ClientGroupSpec.Clients; see
// docs/spikes/store-kube.md); until it lands the test control plane
// installs the CRDs with exactly these bounds. A field that already has
// maxItems is left alone, so the patch becomes a no-op once api/ is fixed.
var celBudgetPatches = []struct {
	crd   string
	path  []string // property path under openAPIV3Schema
	limit int64
}{
	{crd: "blocklists.kudurru.io", path: []string{"spec", "sources"}, limit: 256},
	{crd: "clientgroups.kudurru.io", path: []string{"spec", "clients"}, limit: 1000},
}

// loadCRDs reads every CRD manifest in dir and applies celBudgetPatches.
func loadCRDs(dir string) ([]*apiextensionsv1.CustomResourceDefinition, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no CRD manifests in %s", dir)
	}
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // f comes from a glob of the repository's CRD directory
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(b, crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, p := range celBudgetPatches {
			if p.crd != crd.Name {
				continue
			}
			for vi := range crd.Spec.Versions {
				v := &crd.Spec.Versions[vi]
				if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
					continue
				}
				if err := setMaxItems(v.Schema.OpenAPIV3Schema, p.path, p.limit); err != nil {
					return nil, fmt.Errorf("%s: %w", crd.Name, err)
				}
			}
		}
		crds = append(crds, crd)
	}
	return crds, nil
}

func setMaxItems(s *apiextensionsv1.JSONSchemaProps, path []string, limit int64) error {
	if len(path) == 0 {
		if s.Type != "array" {
			return fmt.Errorf("maxItems patch target is a %s, not an array", s.Type)
		}
		if s.MaxItems == nil {
			s.MaxItems = &limit
		}
		return nil
	}
	child, ok := s.Properties[path[0]]
	if !ok {
		return fmt.Errorf("maxItems patch: no property %q", path[0])
	}
	if err := setMaxItems(&child, path[1:], limit); err != nil {
		return err
	}
	s.Properties[path[0]] = child
	return nil
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// findEnvtestAssets locates kube-apiserver and etcd: $KUBEBUILDER_ASSETS,
// else the newest <repo>/bin/k8s/<version>-<os>-<arch> that `make envtest`
// installs. It returns a skip reason when neither is usable or when
// KUDURRU_SKIP_ENVTEST is set.
func findEnvtestAssets() (dir, skipReason string) {
	if os.Getenv("KUDURRU_SKIP_ENVTEST") != "" {
		return "", "KUDURRU_SKIP_ENVTEST is set"
	}
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		if hasEnvtestBinaries(d) {
			return d, ""
		}
		return "", fmt.Sprintf("KUBEBUILDER_ASSETS=%s does not contain kube-apiserver and etcd; run `make envtest`", d)
	}
	matches, _ := filepath.Glob(filepath.Join(repoRoot(), "bin", "k8s", "*-"+goruntime.GOOS+"-"+goruntime.GOARCH))
	sort.Slice(matches, func(i, j int) bool { return versionLess(filepath.Base(matches[i]), filepath.Base(matches[j])) })
	for i := len(matches) - 1; i >= 0; i-- {
		if hasEnvtestBinaries(matches[i]) {
			return matches[i], ""
		}
	}
	return "", "envtest binaries (kube-apiserver, etcd) not found: run `make envtest` or set KUBEBUILDER_ASSETS"
}

func hasEnvtestBinaries(dir string) bool {
	for _, b := range []string{"kube-apiserver", "etcd"} {
		if st, err := os.Stat(filepath.Join(dir, b)); err != nil || st.IsDir() { //nolint:gosec // dir is KUBEBUILDER_ASSETS or a repo bin/ glob
			return false
		}
	}
	return true
}

// versionLess orders "1.9.0-linux-amd64" before "1.37.0-linux-amd64".
func versionLess(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

func versionParts(s string) []int {
	v, _, _ := strings.Cut(s, "-")
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

// repoRoot walks up from the package directory to the directory holding
// go.mod.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// newTestStore opens a driver on the shared control plane and closes it at
// the end of the test.
func newTestStore(t testing.TB, opts Options) *Store {
	t.Helper()
	return newStoreOn(t, shared.cfg, opts)
}

func newStoreOn(t testing.TB, cfg *rest.Config, opts Options) *Store {
	t.Helper()
	if opts.Namespace == "" {
		opts.Namespace = "kudurru-system"
	}
	if opts.Identity == "" {
		opts.Identity = "test-" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-")
	}
	if opts.Logger == nil {
		opts.Logger = testLogger()
	}
	s, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// purge deletes every kudurru.io object on the control plane and waits
// until the apiserver reports none, giving each conformance case the
// "fresh, empty store" the suite requires.
func purge(t testing.TB, c client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reg, err := newRegistry(testScheme())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range reg.kinds {
		ki := reg.byKind[k]
		for {
			list := ki.newList().(client.ObjectList)
			if err := c.List(ctx, list); err != nil {
				t.Fatalf("purge list %s: %v", k, err)
			}
			items, _ := meta.ExtractList(list)
			if len(items) == 0 {
				break
			}
			for _, it := range items {
				o := it.(client.Object)
				if len(o.GetFinalizers()) > 0 {
					o.SetFinalizers(nil)
					_ = c.Update(ctx, o)
				}
				if err := c.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
					t.Fatalf("purge delete %s/%s: %v", k, o.GetName(), err)
				}
			}
			if ctx.Err() != nil {
				t.Fatalf("purge %s: timed out", k)
			}
		}
	}
}

// partition is an http.RoundTripper that fails every request while down is
// set, simulating a replica cut off from the apiserver.
type partition struct {
	rt   http.RoundTripper
	down *atomic.Bool
}

func (p *partition) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.down.Load() {
		return nil, errors.New("test partition: apiserver unreachable")
	}
	return p.rt.RoundTrip(r)
}

// partitionedConfig returns a copy of cfg whose requests fail while the
// returned flag is set.
func partitionedConfig(cfg *rest.Config) (*rest.Config, *atomic.Bool) {
	down := &atomic.Bool{}
	c := rest.CopyConfig(cfg)
	c.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &partition{rt: rt, down: down} })
	return c, down
}

// eventually polls cond every 50 ms until it holds or timeout elapses.
func eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// zone and rrset build test objects.
func zone(name, zoneName string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: zoneName, Type: v1alpha1.ZoneTypePrimary},
	}
}

func blocklist(ns, name string) *v1alpha1.BlockList {
	return &v1alpha1.BlockList{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{
			{Name: "starter", Inline: []string{"ads.example.", "tracker.example."}},
			{Name: "remote", URL: "https://lists.example/hosts.txt"},
		}},
	}
}

func clientgroup(ns, name string) *v1alpha1.ClientGroup {
	return &v1alpha1.ClientGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.10.0/24"}, {IP: "10.0.0.7"}}},
	}
}

func rrset(ns, name string, labels map[string]string) *v1alpha1.DNSRecordSet {
	return &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: v1alpha1.DNSRecordSetSpec{
			ZoneRef: v1alpha1.ObjectRef{Name: "home"},
			Endpoints: []v1alpha1.Endpoint{
				{DNSName: name + ".home.example.", RecordType: "A", Targets: []string{"10.0.0.1", "10.0.0.2"}, RecordTTL: 300},
				{DNSName: name + ".home.example.", RecordType: "TXT", Targets: []string{"owner=" + ns}},
			},
		},
	}
}
