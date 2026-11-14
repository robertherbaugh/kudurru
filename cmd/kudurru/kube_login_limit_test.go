package main

import (
	"bytes"
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

func TestRunServeKubeSharedLoginLockoutRestartAndOutage(t *testing.T) {
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
		if strings.HasSuffix(r.URL.Path, "/kudurru-login-lockouts") {
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
		cfg.Node.Name = []string{"login-a", "login-b"}[i]
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
	login := func(i int, password string, want int) {
		t.Helper()
		body, err := json.Marshal(map[string]string{"username": "admin", "password": password})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+configs[i].Listeners.API.Address+api.BasePath+"/auth/login", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.GetBody = nil
		req.Header.Set("Content-Type", "application/json")
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
			t.Fatalf("replica %d login returned %d, want %d: %s", i, res.StatusCode, want, raw)
		}
		if want != 200 && len(res.Cookies()) != 0 {
			t.Fatal("refused login issued a cookie")
		}
	}
	login(0, "shared-login-test-password", 200)
	for i := range 5 {
		login(i%2, "wrong-password", 401)
	}
	login(0, "shared-login-test-password", 429)
	login(1, "shared-login-test-password", 429)
	stopB()
	_ = startStaticMount(t, configs[1])
	login(1, "shared-login-test-password", 429)
	before := writes.Load()
	down.Store(true)
	login(0, "shared-login-test-password", 503)
	if writes.Load() != before {
		t.Fatal("unavailable read dispatched mutation")
	}
	down.Store(false)
	login(0, "shared-login-test-password", 429)
}
