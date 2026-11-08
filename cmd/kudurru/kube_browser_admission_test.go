package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestRunServeKubeSharedBrowserAdmissionRestartAndOutage(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var ns corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(restConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport, err = rest.TransportFor(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	var down atomic.Bool
	var writes atomic.Int32
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/kudurru-browser-admission") {
			if r.Method == http.MethodPut {
				writes.Add(1)
			}
			if down.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"ServiceUnavailable","code":503}`)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	configs := []*config.Config{offlineServeConfig(t, gate.URL), offlineServeConfig(t, gate.URL)}
	t.Setenv(users.EnvAdminPassword, "shared-login-test-password")
	t.Setenv(users.EnvAdminPasswordFile, "")
	for i, cfg := range configs {
		cfg.Node.Name = []string{"browser-a", "browser-b"}[i]
		cfg.Store.Kube.ExpectedClusterID = string(ns.UID)
		cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
		cfg.Blocking.Enabled = false
		cfg.DNSSEC.Enabled = false
		source, err := json.Marshal(map[string]any{"store": map[string]any{"driver": "kube"}, "blocking": map[string]any{"enabled": false}, "dnssec": map[string]any{"enabled": false}})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
		if err := os.WriteFile(cfg.Source, source, 0600); err != nil {
			t.Fatal(err)
		}
	}
	_ = startStaticMount(t, configs[0])
	stopB := startStaticMount(t, configs[1])
	hc := &http.Client{Timeout: 10 * time.Second}
	request := func(i int, path string, want int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+configs[i].Listeners.API.Address+api.BasePath+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("replica %d returned %d, want %d: %s", i, res.StatusCode, want, raw)
		}
		if len(res.Cookies()) != 0 {
			t.Fatal("non-provider request issued a cookie")
		}
		if want == 429 && res.Header.Get("Retry-After") == "" {
			t.Fatal("lockout omitted retry delay")
		}
	}
	// Deliberately absent provider: real admission must run before provider
	// selection, even for malformed/unknown callback requests, without I/O to an OP.
	const start = "/auth/oidc/unknown/start"
	const callback = "/auth/oidc/unknown/callback"
	for i := range 5 {
		request(i%2, start, 404)
	}
	request(0, start, 429)
	request(1, start, 429)
	for i := range 10 {
		request(i%2, callback, 404)
	}
	request(0, callback, 429)
	request(1, callback, 429)
	stopB()
	_ = startStaticMount(t, configs[1])
	request(1, start, 429)
	request(1, callback, 429)
	before := writes.Load()
	down.Store(true)
	request(0, start, 503)
	request(0, callback, 503)
	if writes.Load() != before {
		t.Fatal("unavailable read dispatched browser budget mutation")
	}
	down.Store(false)
	request(0, start, 429)
	request(0, callback, 429)
}
