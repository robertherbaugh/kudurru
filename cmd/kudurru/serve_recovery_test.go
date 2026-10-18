package main

import (
	"context"
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

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/bootstrap"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// A real apiserver sits behind an outage gate. Start the actual process
// while the gate is closed, then reopen it without restarting kudurru.
func TestRunServeKubeBootstrapAfterRecovery(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		assets = filepath.Join("..", "..", "bin", "k8s", "1.37.0-linux-amd64")
	}
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); err != nil { //nolint:gosec // G703: trusted test binary directory from KUBEBUILDER_ASSETS or the repository
		t.Skip("envtest kube-apiserver unavailable; set KUBEBUILDER_ASSETS")
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "kustomize", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	realCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("envtest stop: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(realCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(realCfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport, err = rest.TransportFor(realCfg)
	if err != nil {
		t.Fatal(err)
	}
	var reachable atomic.Bool
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !reachable.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"ServiceUnavailable","message":"test outage","code":503}`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.StartupTimeout = config.Duration(2 * time.Second)
	cfg.Auth.Session.Secure = "never"
	cfg.Auth.IdentityProviders = []map[string]any{{
		"id": "test", "issuer": "https://issuer.example.test", "clientID": "kudurru",
		"clientSecretRef": map[string]any{"name": "oidc-test", "key": "secret"},
		"redirectURL":     "http://localhost/api/v1/auth/oidc/test/callback",
	}}
	wire, id := tokens.Generate()
	t.Setenv(tokens.EnvBootstrapToken, wire)
	t.Setenv(tokens.EnvBootstrapTokenFile, "")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runServe: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("runServe did not stop")
		}
	})
	question := new(dns.Msg)
	question.SetQuestion("missing.home.example.", dns.TypeA)
	dc := &dns.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, _, err := dc.Exchange(question, cfg.Listeners.DNS[0].Address)
		if err == nil && res.Rcode == dns.RcodeServerFailure && offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first boot did not start DNS while API unavailable: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ns := kube.PodNamespace()
	if err := admin.Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "oidc-test"}, Data: map[string][]byte{"secret": []byte("test-client-secret")}}); err != nil {
		t.Fatal(err)
	}
	reachable.Store(true)
	deadline = time.Now().Add(30 * time.Second)
	for {
		var root v1alpha1.DNSZone
		var pol v1alpha1.Policy
		var group v1alpha1.ClientGroup
		var list v1alpha1.BlockList
		var userSecret, tokenSecret corev1.Secret
		checks := []struct {
			key    client.ObjectKey
			object client.Object
		}{
			{client.ObjectKey{Name: bootstrap.RootZoneName}, &root},
			{client.ObjectKey{Namespace: ns, Name: bootstrap.DefaultPolicyName}, &pol},
			{client.ObjectKey{Namespace: ns, Name: bootstrap.DefaultClientGroupName}, &group},
			{client.ObjectKey{Namespace: ns, Name: "starter"}, &list},
			{client.ObjectKey{Namespace: ns, Name: kube.InternalSecretName("LocalUser", "", "admin")}, &userSecret},
			{client.ObjectKey{Namespace: ns, Name: kube.InternalSecretName("APIToken", "", id)}, &tokenSecret},
		}
		complete := true
		for _, c := range checks {
			if err := admin.Get(t.Context(), c.key, c.object); err != nil {
				complete = false
				break
			}
		}
		if complete && offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK {
			if root.Spec.ZoneName != "." || pol.Namespace != ns || len(list.Spec.Sources) == 0 {
				t.Fatal("incomplete default objects")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("defaults, admin, token and readiness did not recover without restarting")
		}
		time.Sleep(50 * time.Millisecond)
	}
	hc := &http.Client{Timeout: time.Second}
	response, err := hc.Post("http://"+cfg.Listeners.API.Address+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"offline-test-password-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recovered first-boot admin login: %d", response.StatusCode)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+cfg.Listeners.API.Address+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+wire)
	response2, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response2.Body.Close() }()
	if response2.StatusCode != http.StatusOK {
		t.Fatalf("recovered bootstrap token: %d", response2.StatusCode)
	}
	t.Log("actual DNS starts during outage; default objects, admin login, bootstrap token and readiness recover against real kube without restart")
}
