package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/config"
)

type kubeAuthRouteAnswer struct {
	status      int
	contentType string
	key, hint   string
	body        []byte
}

func kubeAuthRouteRequest(t *testing.T, c *http.Client, method, address, csrf string, body any) kubeAuthRouteAnswer {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(t.Context(), method, address, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		request.Header.Set("X-Kudurru-CSRF", csrf)
	}
	response, err := c.Do(request)
	if err != nil {
		t.Fatal("auth route transport failed", method, address)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	answer := kubeAuthRouteAnswer{status: response.StatusCode, contentType: response.Header.Get("Content-Type"), body: raw}
	var envelope struct {
		Error struct {
			Key  string `json:"key"`
			Hint any    `json:"hint"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		answer.key = envelope.Error.Key
		answer.hint, _ = envelope.Error.Hint.(string)
	}
	return answer
}

// Every auth and IAM route that the API forwards to the Kubernetes
// authentication stack answers with a result or the JSON error envelope. None
// falls through to the mux's plain-text 404 (G21-01).
func TestRunServeKubeForwardedAuthRoutesAnswerEnvelope(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	actual, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(actual, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var namespace corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &namespace); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(actual.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(actual); err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	binding := &public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "contract-viewer", Namespace: "default"},
		Spec: public.RoleBindingSpec{Role: "viewer", Subjects: []public.BindingSubject{{Kind: "User", Name: "contract-reader"}}}}
	if err := admin.Create(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	startStaticMount(t, cfg)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	c := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	tsigMountAwaitRead(t, c, base+"/auth/status", nil)
	var session api.SessionInfo
	controlMountedRequest(t, c, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "offline-test-password-1"}, http.StatusOK, &session)

	// Inventory: no auth or IAM operation of the served contract may answer an
	// error without the envelope. Session-ending operations run last.
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Tags        []string `json:"tags"`
		} `json:"paths"`
	}
	contract := kubeAuthRouteRequest(t, c, http.MethodGet, base+"/openapi.json", "", nil)
	if contract.status != http.StatusOK || json.Unmarshal(contract.body, &document) != nil {
		t.Fatal("served OpenAPI document unavailable")
	}
	type operation struct{ method, path, id string }
	var operations, ending []operation
	for path, methods := range document.Paths {
		for method, op := range methods {
			if !slices.Contains(op.Tags, "auth") && !slices.Contains(op.Tags, "iam") {
				continue
			}
			found := operation{strings.ToUpper(method), path, op.OperationID}
			if strings.HasPrefix(path, "/auth/logout") {
				ending = append(ending, found)
			} else {
				operations = append(operations, found)
			}
		}
	}
	if len(operations)+len(ending) < 39 || len(ending) != 2 {
		t.Fatal("auth and IAM operation inventory shrank", len(operations), len(ending))
	}
	inventory := func(operations []operation) {
		t.Helper()
		slices.SortFunc(operations, func(a, b operation) int { return strings.Compare(a.path+" "+a.method, b.path+" "+b.method) })
		replacer := strings.NewReplacer("{idp}", "corp", "{id}", "contract", "{name}", "contract", "{namespace}", "default")
		for _, op := range operations {
			var body any
			if op.method != http.MethodGet {
				body = map[string]any{}
			}
			got := kubeAuthRouteRequest(t, c, op.method, base+replacer.Replace(op.path), session.CSRF, body)
			if got.status >= 400 && (got.key == "" || !strings.HasPrefix(got.contentType, "application/json")) {
				t.Errorf("%s %s (%s) answered status=%d content-type=%q without the error envelope", op.method, op.path, op.id, got.status, got.contentType)
			}
		}
	}
	inventory(operations)

	// The item read serves the same object as the list, with the exact server
	// identity an operator reviews.
	item := kubeAuthRouteRequest(t, c, http.MethodGet, base+"/iam/rolebindings/default/contract-viewer", "", nil)
	var read public.RoleBinding
	if item.status != http.StatusOK || !strings.HasPrefix(item.contentType, "application/json") || json.Unmarshal(item.body, &read) != nil {
		t.Fatalf("RoleBinding item read: status=%d content-type=%q", item.status, item.contentType)
	}
	if read.UID != binding.UID || read.ResourceVersion == "" || read.Kind != public.KindRoleBinding || read.APIVersion != public.GroupVersion.String() ||
		read.Namespace != "default" || read.Name != "contract-viewer" || read.Spec.Role != "viewer" || len(read.Spec.Subjects) != 1 || read.Spec.Subjects[0].Name != "contract-reader" {
		t.Fatal("RoleBinding item read lost the reviewed identity or specification")
	}
	for path, want := range map[string]struct {
		status int
		key    string
	}{
		"/iam/rolebindings/default/absent":                {http.StatusNotFound, "not_found"},
		"/iam/rolebindings/Not_A_Label/contract-viewer":   {http.StatusBadRequest, "invalid"},
		"/iam/rolebindings/default/contract-viewer?x=1":   {http.StatusBadRequest, "invalid"},
		"/iam/rolebindings/default/rbac.bindings%5B0%5D":  {http.StatusBadRequest, "invalid"},
		"/iam/rolebindings/kube-system/contract-viewer":   {http.StatusNotFound, "not_found"},
		"/iam/rolebindings/default/contract-viewer.other": {http.StatusNotFound, "not_found"},
	} {
		got := kubeAuthRouteRequest(t, c, http.MethodGet, base+path, "", nil)
		if got.status != want.status || got.key != want.key || !strings.HasPrefix(got.contentType, "application/json") {
			t.Fatalf("RoleBinding item %s: status=%d key=%q content-type=%q", path, got.status, got.key, got.contentType)
		}
	}

	// Public-client CLI login and provider logout continuation need the
	// complete SQLite runtime. Each route says so with a stable key and a hint.
	for _, route := range []struct {
		method, path, key string
		body              any
	}{
		{http.MethodGet, "/auth/cli/providers", "cli_login_unavailable", nil},
		{http.MethodPost, "/auth/cli/corp/prepare", "cli_login_unavailable", map[string]any{"mode": "device"}},
		{http.MethodPost, "/auth/cli/corp/verify", "cli_login_unavailable", map[string]any{"receipt": "contract", "accessToken": "contract"}},
		{http.MethodGet, "/auth/oidc/corp/logout", "provider_logout_unavailable", nil},
		{http.MethodGet, "/auth/oidc/corp/logout/callback", "provider_logout_unavailable", nil},
	} {
		got := kubeAuthRouteRequest(t, c, route.method, base+route.path, session.CSRF, route.body)
		if got.status != http.StatusServiceUnavailable || got.key != route.key || got.hint == "" || !strings.HasPrefix(got.contentType, "application/json") {
			t.Fatalf("%s %s: status=%d key=%q hint=%t content-type=%q", route.method, route.path, got.status, got.key, got.hint != "", got.contentType)
		}
	}

	// `kudurru login` in its default OIDC mode reaches the provider list first
	// and prints the refusal with the supported alternatives. The mode requires
	// verified HTTPS, so a TLS front relays to the mounted API.
	apiURL, err := url.Parse("http://" + cfg.Listeners.API.Address)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(apiURL))
	t.Cleanup(front.Close)
	apiCA := cliTLSFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw}))
	for _, mode := range [][]string{nil, {"--device"}} {
		o := &cliOpts{getenv: func(string) string { return "" }, stdin: strings.NewReader(""), now: time.Now, creds: &cli.Credentials{Keyring: &memKeyring{}}}
		root := newRootWith(o)
		var out, errOut syncBuffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		contexts := filepath.Join(t.TempDir(), "contexts.yaml")
		root.SetArgs(append([]string{"--cli-config", contexts, "--ca-file", apiCA, "login", "--server", front.URL, "--name", "kube"}, mode...))
		err := root.ExecuteContext(t.Context())
		if err == nil {
			t.Fatal("OIDC CLI login succeeded against the Kubernetes authentication stack")
		}
		for _, want := range []string{"does not offer OIDC login for the command line", "--token", "--username"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("login refusal %q lacks %q", err.Error(), want)
			}
		}
		if saved, err := cli.LoadConfig(contexts); err != nil || len(saved.Contexts) != 0 {
			t.Fatal("a refused login saved a context")
		}
	}

	inventory(ending)
}
