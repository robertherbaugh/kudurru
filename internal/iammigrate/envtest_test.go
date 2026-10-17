package iammigrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

var testConfig *rest.Config
var envSkip string
var namespaceSequence atomic.Int64

func TestMain(m *testing.M) { os.Exit(runMain(m)) }
func runMain(m *testing.M) int {
	ctrllog.SetLogger(logr.Discard())
	klog.SetLogger(logr.Discard())
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		assets = filepath.Join("..", "..", "bin", "k8s", "1.37.0-linux-amd64")
	}
	// The asset directory is an operator-selected local test toolchain.
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); err != nil { // #nosec G703 -- trusted test toolchain path
		if os.Getenv("KUDURRU_REQUIRE_IAM_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "IAM envtest required: binaries unavailable")
			return 1
		}
		envSkip = "envtest binaries unavailable"
		return m.Run()
	}
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, name := range []string{"serviceaccounts", "rolebindings", "identityproviders"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kustomize", "crd", "bases", "kudurru.io_"+name+".yaml")) // #nosec G304 -- fixed production CRD names
		if err != nil {
			fmt.Fprintln(os.Stderr, "IAM envtest production CRD unavailable")
			return 1
		}
		crd := new(apiextensionsv1.CustomResourceDefinition)
		if yaml.UnmarshalStrict(raw, crd) != nil {
			fmt.Fprintln(os.Stderr, "IAM envtest production CRD invalid")
			return 1
		}
		crds = append(crds, crd)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, CRDs: crds}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	args.Set("authorization-mode", "RBAC")
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "IAM envtest failed:", err)
		return 1
	}
	testConfig = cfg
	code := m.Run()
	if err = env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "IAM envtest stop failed:", err)
		return 1
	}
	return code
}

type testEnv struct {
	m       *Migrator
	core    kubernetes.Interface
	dynamic dynamic.Interface
	ns      string
	ctx     context.Context
}

func newTestEnv(t *testing.T, wrap func(http.RoundTripper) http.RoundTripper) *testEnv {
	t.Helper()
	if envSkip != "" {
		t.Skip(envSkip)
	}
	cfg := rest.CopyConfig(testConfig)
	// JSON makes fault-boundary inspection exact for the typed Secret client.
	cfg.ContentType = "application/json"
	cfg.AcceptContentTypes = "application/json"
	cfg.QPS = 100
	cfg.Burst = 200
	if wrap != nil {
		cfg.Wrap(wrap)
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	core, err := kubernetes.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := dynamic.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	ns := fmt.Sprintf("iam-migration-%d", namespaceSequence.Add(1))
	_, err = core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(core, dc, Options{Namespace: ns, AllowedFiles: []string{"/operator/oidc"}})
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{m: m, core: core, dynamic: dc, ns: ns, ctx: ctx}
}

func (e *testEnv) seed(t *testing.T, kind, name, namespace, spec, status string) *corev1.Secret {
	t.Helper()
	raw := `{"apiVersion":"kudurru.io/v1alpha1","kind":"` + kind + `","metadata":{"name":"` + name + `","namespace":"` + namespace + `","generation":7,"annotations":{"kudurru.io/managed-by":"file:iam.yaml","kubectl.kubernetes.io/last-applied-configuration":"private-old-snapshot"}},"spec":` + spec
	if status != "" {
		raw += `,"status":` + status
	}
	raw += `}`
	return e.seedRaw(t, kind, name, namespace, []byte(raw))
}
func (e *testEnv) seedRaw(t *testing.T, kind, name, namespace string, raw []byte) *corev1.Secret {
	t.Helper()
	labels := map[string]string{kindLabel: kind}
	if namespace != "" {
		labels[namespaceLabel] = namespace
	}
	sec, err := e.core.CoreV1().Secrets(e.ns).Create(e.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName(kind, namespace, name), Namespace: e.ns, Labels: labels, Annotations: map[string]string{nameAnnotation: name}}, Type: internalType, Data: map[string][]byte{"object": raw}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return sec
}
func (e *testEnv) plan(t *testing.T) *Plan {
	t.Helper()
	p, err := e.m.Plan(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func requireProblem(t *testing.T, err error, code string) {
	t.Helper()
	p, ok := err.(*Problem)
	if !ok || p.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// faultTransport still drives the real apiserver. Faults are synchronized so
// race runs can deliberately interrupt network/journal boundaries.
type faultTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	before func(*http.Request) bool
	after  func(*http.Request, *http.Response) bool
}

func (f *faultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if journalPut(r) && r.GetBody == nil {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	}
	f.mu.Lock()
	fail := f.before != nil && f.before(r)
	f.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("injected migration interruption")
	}
	resp, err := f.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	fail = f.after != nil && f.after(r, resp)
	f.mu.Unlock()
	if fail {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("injected migration interruption")
	}
	return resp, nil
}
func journalPut(r *http.Request) bool {
	return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/secrets/"+JournalName)
}
func journalState(r *http.Request) State {
	var raw []byte
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return ""
		}
		defer func() { _ = body.Close() }()
		raw, _ = io.ReadAll(body)
	} else {
		raw, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
	}
	var sec corev1.Secret
	_ = json.Unmarshal(raw, &sec)
	var j journal
	_ = json.Unmarshal(sec.Data["journal"], &j)
	return j.State
}
