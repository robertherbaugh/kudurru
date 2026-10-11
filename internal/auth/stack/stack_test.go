package stack

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const testConfig = `
auth:
  session: { secure: never }
rbac:
  bindings:
  - { role: viewer, scope: { kind: cluster }, subjects: [{ kind: Group, name: staff }] }
`

func loadConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type env struct {
	t     *testing.T
	s     *Stack
	st    store.Store
	srv   *httptest.Server
	ctx   context.Context
	stop  context.CancelFunc
	runOK chan struct{}
}

func newEnv(t *testing.T, st store.Store, yaml string, mutate ...func(*Options)) *env {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	opts := Options{
		Config: loadConfig(t, yaml), Store: st, Keys: keys.Keys{Current: key},
		InternalToken: func(s string) bool { return s == "internal-node-token" },
		Getenv: func(k string) string {
			if k == users.EnvAdminPassword {
				return "admin-password-1"
			}
			return ""
		},
		Out:    io.Discard,
		Logger: slog.New(slog.DiscardHandler),
	}
	for _, m := range mutate {
		m(&opts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(ctx, opts)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	mux := http.NewServeMux()
	s.Register(mux)
	az := s.Authorizer()
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		d, _ := auth.DecisionFrom(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "kind": p.Kind, "method": p.AuthMethod, "path": d.Path})
	})
	mux.Handle("GET /api/v1/zones", auth.Require(az, auth.ClusterAttrs("zones", "read"), echo))
	mux.Handle("POST /api/v1/zones/home/records", auth.Require(az, func(*http.Request) rbac.Attributes {
		return rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}}
	}, echo))
	srv := httptest.NewUnstartedServer(s.Middleware()(mux))
	srv.Start()
	e := &env{t: t, s: s, st: st, srv: srv, ctx: ctx, stop: cancel, runOK: done}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
	})
	return e
}

type resp struct {
	code   int
	body   map[string]any
	raw    string
	cookie *http.Cookie
	header http.Header
}

type reqOpt func(*http.Request)

func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	}
}
func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func bearer(tok string) reqOpt      { return withHeader("Authorization", "Bearer "+tok) }

func (e *env) do(method, path string, body any, opts ...reqOpt) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	for _, o := range opts {
		o(req)
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	out := resp{code: res.StatusCode, raw: string(raw), header: res.Header}
	_ = json.Unmarshal(raw, &out.body)
	for _, c := range res.Cookies() {
		if strings.HasSuffix(c.Name, "kudurru_session") {
			out.cookie = c
		}
	}
	return out
}

func (r resp) errKey() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		k, _ := e["key"].(string)
		return k
	}
	return ""
}

func (e *env) login(user, pw string) (*http.Cookie, string) {
	e.t.Helper()
	r := e.do(http.MethodPost, "/api/v1/auth/login", LoginRequest{Username: user, Password: pw})
	if r.code != 200 || r.cookie == nil {
		e.t.Fatalf("login %s: %d %s", user, r.code, r.raw)
	}
	csrf, _ := r.body["csrf"].(string)
	return r.cookie, csrf
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAuthenticatorChainMatrix(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		e := newEnv(t, st, testConfig)
		ctx := e.ctx

		// Anonymous.
		if r := e.do(http.MethodGet, "/api/v1/zones", nil); r.code != 401 || r.errKey() != "unauthenticated" {
			t.Fatalf("anonymous: %d %s", r.code, r.raw)
		}
		st0 := e.do(http.MethodGet, "/api/v1/auth/status", nil)
		if st0.code != 200 || st0.body["localLogin"] != true || st0.body["hasDefaultCredentials"] != false {
			t.Fatalf("status: %s", st0.raw)
		}

		// Password login (env-provided admin password) → cookie session.
		if r := e.do(http.MethodPost, "/api/v1/auth/login", LoginRequest{Username: "admin", Password: "nope"}); r.code != 401 || r.cookie != nil {
			t.Fatalf("bad password: %d %s", r.code, r.raw)
		}
		cookie, csrf := e.login("admin", "admin-password-1")
		if cookie.Name != "kudurru_session" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatalf("cookie %+v", cookie)
		}
		r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(cookie))
		if r.code != 200 || r.body["method"] != "cookie" || r.body["path"] != rbac.PathLocal || r.body["id"] != "local:admin" {
			t.Fatalf("cookie GET: %d %s", r.code, r.raw)
		}
		me := e.do(http.MethodGet, "/api/v1/auth/me", nil, withCookie(cookie))
		perms, _ := me.body["permissions"].(map[string]any)
		if me.code != 200 || me.body["csrf"] != csrf || perms["settings"] == nil {
			t.Fatalf("me: %s", me.raw)
		}

		// CSRF on cookie writes.
		if r := e.do(http.MethodPost, "/api/v1/auth/tokens", CreateTokenRequest{}, withCookie(cookie)); r.code != 403 || r.errKey() != "csrf" {
			t.Fatalf("cookie POST without CSRF: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodPost, "/api/v1/auth/tokens", CreateTokenRequest{}, withCookie(cookie), withHeader(auth.CSRFHeader, csrf), withHeader("Sec-Fetch-Site", "cross-site")); r.code != 403 {
			t.Fatalf("cross-site cookie POST: %d %s", r.code, r.raw)
		}
		tr := e.do(http.MethodPost, "/api/v1/auth/tokens", CreateTokenRequest{Description: "ci", ExpiresIn: "1h"}, withCookie(cookie), withHeader(auth.CSRFHeader, csrf))
		if tr.code != 201 {
			t.Fatalf("token create: %d %s", tr.code, tr.raw)
		}
		adminToken, _ := tr.body["token"].(string)

		// Bearer kdr_ token: CSRF-exempt, wins over a cookie.
		r = e.do(http.MethodGet, "/api/v1/zones", nil, bearer(adminToken), withCookie(&http.Cookie{Name: "kudurru_session", Value: "garbage"})) //nolint:gosec // G124: a request cookie in a test
		if r.code != 200 || r.body["method"] != "token" {
			t.Fatalf("token GET: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodPost, "/api/v1/zones/home/records", nil, bearer(adminToken)); r.code != 200 {
			t.Fatalf("token POST needs no CSRF: %d %s", r.code, r.raw)
		}
		bad := adminToken[:len(adminToken)-2] + "xx"
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, bearer(bad), withCookie(cookie)); r.code != 401 {
			t.Fatalf("wrong secret must be 401 even with a valid cookie: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, bearer("eyJhbGciOiJub25lIn0.eyJpc3MiOiJodHRwczovL2V2aWwifQ.")); r.code != 401 {
			t.Fatalf("JWT from an unknown issuer: %d %s", r.code, r.raw)
		}

		// Internal token: read-only Node principal.
		r = e.do(http.MethodGet, "/api/v1/zones", nil, bearer("internal-node-token"))
		if r.code != 200 || r.body["kind"] != string(auth.KindNode) || r.body["path"] != rbac.PathLocal {
			t.Fatalf("internal token: %d %s", r.code, r.raw)
		}
		if r := e.do(http.MethodPost, "/api/v1/zones/home/records", nil, bearer("internal-node-token")); r.code != 403 {
			t.Fatalf("internal token write: %d %s", r.code, r.raw)
		}

		// A config-file binding (viewer for Group staff) for a local user.
		if _, err := e.s.Users().Create(ctx, users.CreateOptions{Name: "alice", Password: "alice-password", Groups: []string{"staff"}}); err != nil {
			t.Fatal(err)
		}
		alice, aliceCSRF := e.login("alice", "alice-password")
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(alice)); r.code != 200 || r.body["path"] != rbac.PathBinding {
			t.Fatalf("alice read: %d %s", r.code, r.raw)
		}
		w := e.do(http.MethodPost, "/api/v1/zones/home/records", nil, withCookie(alice), withHeader(auth.CSRFHeader, aliceCSRF))
		hint, _ := w.body["error"].(map[string]any)["hint"].(map[string]any)
		missing, _ := hint["missing"].(map[string]any)
		if w.code != 403 || missing["resource"] != "records" || missing["verb"] != "write" || missing["scope"] != "zone:home" {
			t.Fatalf("alice write: %d %s", w.code, w.raw)
		}
		if r := e.do(http.MethodGet, "/api/v1/iam/users", nil, withCookie(alice)); r.code != 403 {
			t.Fatalf("alice IAM: %d", r.code)
		}
		if r := e.do(http.MethodGet, "/api/v1/iam/users", nil, withCookie(cookie)); r.code != 200 || len(r.body["items"].([]any)) != 2 {
			t.Fatalf("admin IAM users: %d %s", r.code, r.raw)
		}
		// Someone else's token is reported missing.
		tokID, _ := tr.body["id"].(string)
		if r := e.do(http.MethodDelete, "/api/v1/auth/tokens/"+tokID, nil, withCookie(alice), withHeader(auth.CSRFHeader, aliceCSRF)); r.code != 404 {
			t.Fatalf("alice deleting admin's token: %d", r.code)
		}

		// A RoleBinding object grants zone-admin on home to alice, live.
		rb := &internalv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "alice-home", Namespace: "dns"}, Spec: internalv1.RoleBindingSpec{
			Role: "zone-admin", Scope: internalv1.BindingScope{Kind: "zone", Name: "home"},
			Subjects: []internalv1.BindingSubject{{Kind: "User", Name: "alice"}},
		}}
		if _, err := st.Apply(ctx, rb, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the RoleBinding to authorize alice", func() bool {
			return e.do(http.MethodPost, "/api/v1/zones/home/records", nil, withCookie(alice), withHeader(auth.CSRFHeader, aliceCSRF)).code == 200
		})
		me = e.do(http.MethodGet, "/api/v1/auth/me", nil, withCookie(alice))
		if scoped, _ := me.body["scopedPermissions"].(map[string]any); scoped["zone:home"] == nil {
			t.Fatalf("me without scoped permissions: %s", me.raw)
		}
		if err := st.Delete(ctx, store.Key{Kind: internalv1.KindRoleBinding, Namespace: "dns", Name: "alice-home"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the deleted RoleBinding to stop authorizing alice", func() bool {
			return e.do(http.MethodPost, "/api/v1/zones/home/records", nil, withCookie(alice), withHeader(auth.CSRFHeader, aliceCSRF)).code == 403
		})

		// Disabling alice kills her session at once.
		if err := e.s.Users().SetDisabled(ctx, "alice", true); err != nil {
			t.Fatal(err)
		}
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(alice)); r.code != 401 {
			t.Fatalf("disabled user's cookie: %d %s", r.code, r.raw)
		}

		// Token scopes narrow the admin.
		sr := e.do(http.MethodPost, "/api/v1/auth/tokens", CreateTokenRequest{Scopes: []string{"viewer"}}, withCookie(cookie), withHeader(auth.CSRFHeader, csrf))
		scoped, _ := sr.body["token"].(string)
		if r := e.do(http.MethodGet, "/api/v1/zones", nil, bearer(scoped)); r.code != 200 {
			t.Fatalf("scoped read: %d", r.code)
		}
		if r := e.do(http.MethodPost, "/api/v1/zones/home/records", nil, bearer(scoped)); r.code != 403 {
			t.Fatalf("scoped write: %d %s", r.code, r.raw)
		}
		// A scoped token cannot mint a broader token.
		if r := e.do(http.MethodPost, "/api/v1/auth/tokens", CreateTokenRequest{Scopes: []string{"operator"}}, bearer(scoped)); r.code != 403 {
			t.Fatalf("scope escalation: %d %s", r.code, r.raw)
		}

		// Logout clears the cookie.
		lo := e.do(http.MethodPost, "/api/v1/auth/logout", nil, withCookie(cookie), withHeader(auth.CSRFHeader, csrf))
		if lo.code != 204 || lo.cookie == nil || lo.cookie.MaxAge >= 0 {
			t.Fatalf("logout: %d %+v", lo.code, lo.cookie)
		}
	})
}

func TestMustChangePasswordFlow(t *testing.T) {
	e := newEnv(t, authtest.Memory(t), testConfig)
	if _, err := e.s.Users().Create(e.ctx, users.CreateOptions{Name: "bob", Password: "bob-password-1", MustChangePassword: true, Groups: []string{"staff"}}); err != nil {
		t.Fatal(err)
	}
	old, csrf := e.login("bob", "bob-password-1")
	if r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(old)); r.code != 403 || r.errKey() != "password_change_required" {
		t.Fatalf("before change: %d %s", r.code, r.raw)
	}
	if r := e.do(http.MethodGet, "/api/v1/auth/me", nil, withCookie(old)); r.code != 200 || r.body["mustChangePassword"] != true {
		t.Fatalf("me before change: %d %s", r.code, r.raw)
	}
	if r := e.do(http.MethodPost, "/api/v1/auth/password", PasswordRequest{CurrentPassword: "wrong", NewPassword: "bob-password-2"}, withCookie(old), withHeader(auth.CSRFHeader, csrf)); r.code != 403 {
		t.Fatalf("wrong current password: %d %s", r.code, r.raw)
	}
	ch := e.do(http.MethodPost, "/api/v1/auth/password", PasswordRequest{CurrentPassword: "bob-password-1", NewPassword: "bob-password-2"}, withCookie(old), withHeader(auth.CSRFHeader, csrf))
	if ch.code != 200 || ch.cookie == nil {
		t.Fatalf("change: %d %s", ch.code, ch.raw)
	}
	if r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(ch.cookie)); r.code != 200 {
		t.Fatalf("new session after change: %d %s", r.code, r.raw)
	}
	if r := e.do(http.MethodGet, "/api/v1/zones", nil, withCookie(old)); r.code != 401 {
		t.Fatalf("old session after change: %d %s", r.code, r.raw)
	}
}

func TestLocalLoginModesAndLockout(t *testing.T) {
	cfg := `
auth:
  localLogin: admins-only
  session: { secure: never }
`
	e := newEnv(t, authtest.Memory(t), cfg)
	if _, err := e.s.Users().Create(e.ctx, users.CreateOptions{Name: "carol", Password: "carol-password"}); err != nil {
		t.Fatal(err)
	}
	if r := e.do(http.MethodPost, "/api/v1/auth/login", LoginRequest{Username: "carol", Password: "carol-password"}); r.code != 403 || r.errKey() != "local_login_admins_only" {
		t.Fatalf("admins-only, non-admin: %d %s", r.code, r.raw)
	}
	e.login("admin", "admin-password-1")
	for range 4 {
		e.do(http.MethodPost, "/api/v1/auth/login", LoginRequest{Username: "admin", Password: "x"})
	}
	r := e.do(http.MethodPost, "/api/v1/auth/login", LoginRequest{Username: "admin", Password: "x"})
	if r.code != 401 {
		t.Fatalf("5th failure: %d", r.code)
	}
	r = e.do(http.MethodPost, "/api/v1/auth/login", LoginRequest{Username: "admin", Password: "admin-password-1"})
	if r.code != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("locked IP: %d %s", r.code, r.raw)
	}

	e = newEnv(t, authtest.Memory(t), "auth:\n  localLogin: disabled\n")
	if r := e.do(http.MethodPost, "/api/v1/auth/login", LoginRequest{Username: "admin", Password: "admin-password-1"}); r.code != 403 || r.errKey() != "local_login_disabled" {
		t.Fatalf("disabled: %d %s", r.code, r.raw)
	}
	if st := e.do(http.MethodGet, "/api/v1/auth/status", nil); st.body["localLogin"] != false {
		t.Fatalf("status: %s", st.raw)
	}
}

func TestUnixPeerInChain(t *testing.T) {
	e := newEnv(t, authtest.Memory(t), testConfig)
	srv := httptest.NewUnstartedServer(e.srv.Config.Handler)
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return auth.WithPeer(ctx, auth.PeerCred{UID: 0, GID: 0, PID: 1})
	}
	srv.Start()
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/api/v1/zones")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != 200 || body["kind"] != string(auth.KindLocalRoot) || body["path"] != rbac.PathLocal {
		t.Fatalf("unix peer: %d %v", res.StatusCode, body)
	}
}

func TestGeneratedAdminAndBootstrapToken(t *testing.T) {
	var out bytes.Buffer
	wire := "kdr_abcdefghijk2_" + strings.Repeat("A", 43)
	e := newEnv(t, authtest.SQLite(t), testConfig, func(o *Options) {
		o.Out = &out
		o.Getenv = func(k string) string {
			if k == "KUDURRU_BOOTSTRAP_TOKEN" {
				return wire
			}
			return ""
		}
	})
	if !strings.Contains(out.String(), "generated password") {
		t.Fatalf("no generated password printed: %q", out.String())
	}
	if st := e.do(http.MethodGet, "/api/v1/auth/status", nil); st.body["hasDefaultCredentials"] != true {
		t.Fatalf("status: %s", st.raw)
	}
	r := e.do(http.MethodGet, "/api/v1/iam/serviceaccounts", nil, bearer(wire))
	if r.code != 200 || !strings.Contains(r.raw, `"name":"bootstrap"`) {
		t.Fatalf("bootstrap token: %d %s", r.code, r.raw)
	}
	if r := e.do(http.MethodGet, "/api/v1/iam/rolebindings", nil, bearer(wire)); r.code != 200 || !strings.Contains(r.raw, `"source":"config"`) {
		t.Fatalf("rolebindings: %d %s", r.code, r.raw)
	}
}

func TestInvalidConfigurations(t *testing.T) {
	st := authtest.Memory(t)
	key := make([]byte, 32)
	for _, tc := range []struct{ name, yaml string }{
		{"unknown role", "rbac:\n  bindings:\n  - { role: superuser, subjects: [{ kind: User, name: x }] }\n"},
		{"scoped role at cluster scope", "rbac:\n  bindings:\n  - { role: zone-admin, subjects: [{ kind: User, name: x }] }\n"},
		{"binding typo", "rbac:\n  bindings:\n  - { role: admin, subject: [{ kind: User, name: x }], subjects: [{ kind: User, name: x }] }\n"},
		{"identity provider without secret", "auth:\n  identityProviders:\n  - { id: corp, issuer: https://sso.example, clientID: k, redirectURL: https://dns.example/api/v1/auth/oidc/corp/callback }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "k.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path, nil)
			if err != nil {
				return // rejected by config validation already
			}
			if _, err := New(context.Background(), Options{Config: cfg, Store: st, Keys: keys.Keys{Current: key}, Out: io.Discard,
				Logger: slog.New(slog.DiscardHandler), Getenv: func(string) string { return "" }}); err == nil {
				t.Fatal("New accepted an invalid configuration")
			}
		})
	}
}
