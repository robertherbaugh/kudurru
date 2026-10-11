package k8s_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/k8s"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

var (
	envSkip  string
	admin    *kubernetes.Clientset
	kudurru  *rest.Config // the server's own identity: system:auth-delegator + issuer discovery
	adminCfg *rest.Config
	saUID    string
	testNS   = "dns"
	testSA   = "external-dns"
	otherSA  = "intruder"
	audience = k8s.DefaultAudience
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		envSkip = "KUBEBUILDER_ASSETS is not set"
		return m.Run()
	}
	for _, b := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(assets, b)); err != nil { //nolint:gosec // developer-provided path
			envSkip = fmt.Sprintf("KUBEBUILDER_ASSETS=%s has no %s", assets, b)
			return m.Run()
		}
	}
	if os.Getenv("KUDURRU_TEST_LOGS") == "" {
		ctrllog.SetLogger(logr.Discard())
		klog.SetLogger(logr.Discard())
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s: envtest failed to start: %v\n", err)
		return 1
	}
	defer func() { _ = env.Stop() }()
	if err := setup(env, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "k8s: setup: %v\n", err)
		return 1
	}
	return m.Run()
}

func setup(env *envtest.Environment, cfg *rest.Config) error {
	ctx := context.Background()
	var err error
	if admin, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	for _, ns := range []string{testNS, "kudurru"} {
		if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	adminCfg = cfg
	for _, name := range []string{testSA, otherSA} {
		sa, err := admin.CoreV1().ServiceAccounts(testNS).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		if name == testSA {
			saUID = string(sa.UID)
		}
	}
	// The kudurru server identity: only what §5.3 grants (system:auth-delegator
	// for TokenReview/SubjectAccessReview, issuer discovery for the JWKS).
	u, err := env.AddUser(envtest.User{Name: "kudurru-server", Groups: []string{"system:authenticated"}}, nil)
	if err != nil {
		return err
	}
	kudurru = u.Config()
	for _, role := range []string{"system:auth-delegator", "system:service-account-issuer-discovery"} {
		if err := bindUser(ctx, "kudurru-server-"+role[len("system:"):], role, "kudurru-server"); err != nil {
			return err
		}
	}
	// external-dns may list query logs and write records in zone "home"
	// only (virtual resources and resourceNames, §5.5).
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "kudurru-external-dns"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"kudurru.io"}, Resources: []string{"querylogs", "dnszones"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"kudurru.io"}, Resources: []string{"dnsrecordsets"}, ResourceNames: []string{"home"}, Verbs: []string{"update"}},
	}}
	if _, err := admin.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); err != nil {
		return err
	}
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "kudurru-external-dns"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cr.Name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: testSA, Namespace: testNS}}}
	_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	return err
}

func bindUser(ctx context.Context, name, role, user string) error {
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}}
	_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	return err
}

func need(t *testing.T) {
	t.Helper()
	if envSkip != "" {
		t.Skip(envSkip)
	}
}

func token(t *testing.T, sa string, aud ...string) string {
	t.Helper()
	exp := int64(600)
	tr, err := admin.CoreV1().ServiceAccounts(testNS).CreateToken(context.Background(), sa, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: aud, ExpirationSeconds: &exp},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return tr.Status.Token
}

func TestServiceAccountTokens(t *testing.T) {
	need(t)
	ctx := context.Background()
	good := token(t, testSA, audience)
	wrongAud := token(t, testSA, "https://kubernetes.default.svc")
	for _, mode := range []struct {
		name string
		jwks bool
	}{{"jwks", true}, {"tokenreview", false}} {
		t.Run(mode.name, func(t *testing.T) {
			a, err := k8s.New(k8s.Options{Config: kudurru, DisableJWKS: !mode.jwks})
			if err != nil {
				t.Fatal(err)
			}
			iss, err := a.Issuer(ctx)
			if mode.jwks && (err != nil || iss == "") {
				t.Fatalf("issuer discovery: %q, %v", iss, err)
			}
			if !mode.jwks && err == nil {
				t.Fatal("discovery should be disabled")
			}
			if tokIss, _ := auth.UnverifiedIssuer(good); mode.jwks && !a.AcceptsIssuer(ctx, tokIss) {
				t.Fatalf("issuer %q not accepted (discovered %q)", tokIss, iss)
			}
			verify := a.VerifyJWT
			if !mode.jwks {
				verify = a.Fallback().VerifyJWT
			}
			start := time.Now()
			p, err := verify(ctx, good)
			if err != nil {
				t.Fatal(err)
			}
			first := time.Since(start)
			fresh := token(t, testSA, audience) // not cached: keys (jwks) or a TokenReview round trip
			start = time.Now()
			if _, err := verify(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			second := time.Since(start)
			start = time.Now()
			if _, err := verify(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: first token %s (with discovery), new token %s, cached %s", mode.name, first, second, time.Since(start))
			if p.Kind != auth.KindKubernetesServiceAccount || p.Name != "system:serviceaccount:dns:external-dns" || p.ServiceAccount != "dns/external-dns" ||
				p.UID != saUID || p.AuthMethod != auth.MethodK8s {
				t.Fatalf("principal %+v", p)
			}
			hasGroup := false
			for _, g := range p.Groups {
				hasGroup = hasGroup || g == "system:serviceaccounts:dns"
			}
			if !hasGroup {
				t.Fatalf("groups %v", p.Groups)
			}
			if _, err := verify(ctx, wrongAud); err == nil {
				t.Fatal("a token for another audience was accepted")
			}
			if _, err := verify(ctx, good[:len(good)-4]+"AAAA"); err == nil {
				t.Fatal("a token with a broken signature was accepted")
			}
		})
	}
	// With discovery working, a JWT from another issuer is rejected by the
	// fallback without a TokenReview.
	a, _ := k8s.New(k8s.Options{Config: kudurru})
	if _, err := a.Fallback().VerifyJWT(ctx, "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2V2aWwifQ.c2ln"); err == nil {
		t.Fatal("unknown issuer accepted")
	}
}

func TestSubjectAccessReview(t *testing.T) {
	need(t)
	ctx := context.Background()
	az, err := authorizationv1client.NewForConfig(kudurru)
	if err != nil {
		t.Fatal(err)
	}
	sar, err := rbac.NewSARAuthorizer(rbac.SAROptions{Client: az})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := k8s.New(k8s.Options{Config: kudurru})
	p, err := a.VerifyJWT(ctx, token(t, testSA, audience))
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := a.VerifyJWT(ctx, token(t, otherSA, audience))
	if err != nil {
		t.Fatal(err)
	}
	cluster := rbac.Scope{Kind: rbac.ScopeCluster}
	for _, tc := range []struct {
		name string
		p    auth.Principal
		a    rbac.Attributes
		ok   bool
	}{
		{"virtual querylogs list", p, rbac.Attributes{Resource: "querylog", Verb: "read", Scope: cluster}, true},
		{"zones list", p, rbac.Attributes{Resource: "zones", Verb: "read", Scope: cluster}, true},
		{"records write in zone home (resourceNames)", p, rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}}, true},
		{"records write in zone lab", p, rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "lab"}}, false},
		{"settings write", p, rbac.Attributes{Resource: "settings", Verb: "write", Scope: cluster}, false},
		{"cache flush (custom verb)", p, rbac.Attributes{Resource: "cache", Verb: "flush", Scope: cluster}, false},
		{"other service account", intruder, rbac.Attributes{Resource: "querylog", Verb: "read", Scope: cluster}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := sar.Authorize(ctx, auth.RBACPrincipal(tc.p), tc.a)
			if err != nil {
				t.Fatal(err)
			}
			if d.Allowed != tc.ok || d.Path != rbac.PathSAR {
				t.Fatalf("decision %+v, want allowed=%v", d, tc.ok)
			}
		})
	}
}

// TestStackEndToEnd sends a projected ServiceAccount token through the
// whole identity stack: the bearer JWT step routes it by iss to the
// Kubernetes verifier, and the Authorize step asks SubjectAccessReview.
func TestStackEndToEnd(t *testing.T) {
	need(t)
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := stack.New(ctx, stack.Options{Config: cfg, Store: authtest.Memory(t), Keys: keys.Keys{Current: key}, Kube: kudurru, Namespace: "kudurru",
		Getenv: func(string) string { return "" }, Out: io.Discard, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Run(ctx) }()
	mux := http.NewServeMux()
	s.Register(mux)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := auth.DecisionFrom(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]string{"path": d.Path})
	})
	mux.Handle("GET /api/v1/querylog", auth.Require(s.Authorizer(), auth.ClusterAttrs("querylog", "read"), ok))
	mux.Handle("PATCH /api/v1/settings", auth.Require(s.Authorizer(), auth.ClusterAttrs("settings", "write"), ok))
	srv := httptest.NewServer(s.Middleware()(mux))
	defer srv.Close()
	call := func(method, path, tok string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	tok := token(t, testSA, audience)
	if code, body := call(http.MethodGet, "/api/v1/querylog", tok); code != 200 || body != "{\"path\":\"sar\"}\n" {
		t.Fatalf("querylog: %d %s", code, body)
	}
	if code, body := call(http.MethodPatch, "/api/v1/settings", tok); code != 403 {
		t.Fatalf("settings: %d %s", code, body)
	}
	if code, _ := call(http.MethodGet, "/api/v1/querylog", token(t, testSA, "other-audience")); code != 401 {
		t.Fatalf("wrong audience: %d", code)
	}
	start := time.Now()
	for range 20 { // cached verdicts: no apiserver round trip per request
		call(http.MethodGet, "/api/v1/querylog", tok)
	}
	t.Logf("20 cached SA-token requests in %s", time.Since(start))
}

// TestIdentityOnKubeStore runs the identity layer on the kube store driver:
// users, tokens and RoleBinding objects live in Secrets labelled
// kudurru.io/kind in the release namespace.
func TestIdentityOnKubeStore(t *testing.T) {
	need(t)
	scheme, err := internalv1.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := kube.New(adminCfg, kube.Options{Namespace: "kudurru", Identity: "identity-test", Scheme: scheme, InternalKinds: stack.InternalKinds(),
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.Session.Secure = "never"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := stack.New(ctx, stack.Options{Config: cfg, Store: st, Keys: keys.Keys{Current: make([]byte, 32)}, Kube: kudurru, Namespace: "kudurru",
		Getenv: func(k string) string {
			if k == "KUDURRU_ADMIN_PASSWORD" {
				return "kube-admin-password"
			}
			return ""
		}, Out: io.Discard, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Run(ctx) }()
	mux := http.NewServeMux()
	s.Register(mux)
	mux.Handle("POST /api/v1/zones/home/records", auth.Require(s.Authorizer(), func(*http.Request) rbac.Attributes {
		return rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}}
	}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	srv := httptest.NewServer(s.Middleware()(mux))
	defer srv.Close()
	post := func(path string, body any, hdr map[string]string) *http.Response {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := post("/api/v1/auth/login", map[string]string{"username": "admin", "password": "kube-admin-password"}, nil)
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("login on the kube store: %d", res.StatusCode)
	}
	if _, err := s.Users().Create(ctx, users.CreateOptions{Name: "dave", Password: "dave-password"}); err != nil {
		t.Fatal(err)
	}
	wire, tok, err := s.Tokens().Create(ctx, tokens.CreateOptions{Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: "dave", ID: users.PrincipalID("dave"), Provider: users.Provider}})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := admin.CoreV1().Secrets("kudurru").List(ctx, metav1.ListOptions{LabelSelector: "kudurru.io/kind=APIToken"})
	if err != nil || len(sec.Items) != 1 {
		t.Fatalf("APIToken Secrets: %v, %v", sec, err)
	}
	if bytes.Contains(sec.Items[0].Data["object"], []byte(wire[17:])) {
		t.Fatal("the token secret is stored in the Secret")
	}
	bearer := map[string]string{"Authorization": "Bearer " + wire}
	if res := post("/api/v1/zones/home/records", nil, bearer); res.StatusCode != 403 {
		t.Fatalf("dave before the binding: %d", res.StatusCode)
	}
	rb := &internalv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "dave-home", Namespace: "dns"}, Spec: internalv1.RoleBindingSpec{
		Role: "zone-admin", Scope: internalv1.BindingScope{Kind: "zone", Name: "home"}, Subjects: []internalv1.BindingSubject{{Kind: "User", Name: "dave"}}}}
	if _, err := st.Apply(ctx, rb, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		res := post("/api/v1/zones/home/records", nil, bearer)
		_ = res.Body.Close()
		if res.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the RoleBinding Secret never authorized dave")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.Tokens().Revoke(ctx, tok.Name); err != nil {
		t.Fatal(err)
	}
	if res := post("/api/v1/zones/home/records", nil, bearer); res.StatusCode != 401 {
		t.Fatalf("revoked token: %d", res.StatusCode)
	}
	if err := s.Users().SetPassword(ctx, "dave", "dave-password-2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Users().Login(ctx, "dave", "dave-password-2", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
}
