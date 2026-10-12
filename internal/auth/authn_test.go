package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	eerbac "github.com/robertherbaugh/kudurru/ee/rbac"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// fakeStep claims requests carrying its header value.
type fakeStep struct {
	header, value string
	p             Principal
	err           error
}

func (f fakeStep) Authenticate(_ http.ResponseWriter, r *http.Request) (Principal, *http.Request, error) {
	if r.Header.Get(f.header) != f.value || f.value == "" {
		return Principal{}, nil, ErrNoCredentials
	}
	if f.err != nil {
		return Principal{}, nil, f.err
	}
	return f.p, r, nil
}

func TestMiddlewareChain(t *testing.T) {
	steps := []Authenticator{
		fakeStep{header: "X-A", value: "good", p: Principal{Kind: KindUser, ID: "a", AuthMethod: MethodToken}},
		fakeStep{header: "X-A", value: "bad", err: Unauthenticated(errors.New("bad"))},
		fakeStep{header: "X-B", value: "good", p: Principal{Kind: KindUser, ID: "b", AuthMethod: MethodCookie}},
		fakeStep{header: "X-B", value: "csrf", err: &Error{Status: http.StatusForbidden, Key: "csrf", Message: "no"}},
		fakeStep{header: "X-C", value: "disabled", p: Principal{Kind: KindUser, ID: "disabled"}},
	}
	load := func(_ context.Context, p Principal) (Principal, error) {
		if p.ID == "disabled" {
			return Principal{}, Unauthenticated(errors.New("disabled"))
		}
		p.Groups = []string{"loaded"}
		return p, nil
	}
	var seen []Error
	mw := Middleware(Options{Steps: steps, Load: load, OnError: func(_ *http.Request, e *Error) { seen = append(seen, *e) }})
	for _, tc := range []struct {
		name    string
		headers map[string]string
		status  int
		id, key string
	}{
		{"anonymous passes through", nil, 200, "", ""},
		{"first step wins", map[string]string{"X-A": "good", "X-B": "good"}, 200, "a", ""},
		{"later step", map[string]string{"X-B": "good"}, 200, "b", ""},
		{"rejected credential stops the chain", map[string]string{"X-A": "bad", "X-B": "good"}, 401, "", "unauthenticated"},
		{"csrf failure is 403", map[string]string{"X-B": "csrf"}, 403, "", "csrf"},
		{"loader rejects", map[string]string{"X-C": "disabled"}, 401, "", "unauthenticated"},
		{"unclaimed bearer is 401, never anonymous", map[string]string{"Authorization": "Bearer what-is-this"}, 401, "", "unauthenticated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *Principal
			h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if p, ok := PrincipalFrom(r.Context()); ok {
					got = &p
				}
				w.WriteHeader(200)
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.status, rec.Body)
			}
			if tc.status != 200 {
				var body struct {
					Error  struct{ Key, Message string } `json:"error"`
					TookMS *int64                        `json:"took_ms"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Key != tc.key || body.TookMS == nil {
					t.Fatalf("error body %s (%v)", rec.Body, err)
				}
				if tc.status == 401 && rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("401 without WWW-Authenticate")
				}
				return
			}
			switch {
			case tc.id == "" && got != nil:
				t.Fatalf("anonymous request got principal %+v", got)
			case tc.id != "" && (got == nil || got.ID != tc.id || got.Groups[0] != "loaded"):
				t.Fatalf("principal %+v, want %s (loaded)", got, tc.id)
			}
		})
	}
	if len(seen) != 4 {
		t.Fatalf("OnError saw %d rejections", len(seen))
	}
}

func TestWriteErrorRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, nil, &Error{Status: http.StatusTooManyRequests, Key: "rate_limited", Message: "slow down", RetryAfter: 1500 * time.Millisecond})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("429: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestCSRF(t *testing.T) {
	sess := Session{ID: "s", CSRF: "tok"}
	for _, tc := range []struct {
		name    string
		method  string
		session bool
		header  string
		fetch   string
		ok      bool
	}{
		{"safe method", http.MethodGet, true, "", "", true},
		{"bearer request (no session)", http.MethodPost, false, "", "", true},
		{"cookie POST without header", http.MethodPost, true, "", "", false},
		{"cookie POST wrong header", http.MethodPost, true, "nope", "", false},
		{"cookie POST right header", http.MethodPost, true, "tok", "", true},
		{"cookie POST right header, cross-site", http.MethodPost, true, "tok", "cross-site", false},
		{"cookie DELETE right header, same-origin", http.MethodDelete, true, "tok", "same-origin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "https://dns.example/api/v1/zones", nil)
			if tc.session {
				req = req.WithContext(WithSession(req.Context(), sess))
			}
			if tc.header != "" {
				req.Header.Set(CSRFHeader, tc.header)
			}
			if tc.fetch != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetch)
			}
			err := CSRF(req)
			if tc.ok != (err == nil) {
				t.Fatalf("CSRF = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && AsError(err).Status != http.StatusForbidden {
				t.Fatalf("status %d", AsError(err).Status)
			}
		})
	}
}

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

type fakeVerifier struct {
	iss string
	err error
}

func (f fakeVerifier) AcceptsIssuer(_ context.Context, iss string) bool { return iss == f.iss }
func (f fakeVerifier) VerifyJWT(_ context.Context, _ string) (Principal, error) {
	if f.err != nil {
		return Principal{}, f.err
	}
	return Principal{Kind: KindUser, ID: f.iss, AuthMethod: MethodJWT}, nil
}

func TestBearerJWTRouting(t *testing.T) {
	oidcV := fakeVerifier{iss: "https://sso.example"}
	k8sV := fakeVerifier{iss: "https://kubernetes.default.svc"}
	step := BearerJWT{Verifiers: []JWTVerifier{oidcV, k8sV}}
	for _, tc := range []struct {
		name, bearer string
		wantID       string
		wantErr      error // nil: authenticated; ErrNoCredentials: skipped; other: rejected
	}{
		{"oidc issuer", jwt(t, map[string]any{"iss": "https://sso.example"}), "https://sso.example", nil},
		{"k8s issuer", jwt(t, map[string]any{"iss": "https://kubernetes.default.svc"}), "https://kubernetes.default.svc", nil},
		{"unknown issuer", jwt(t, map[string]any{"iss": "https://evil.example"}), "", ErrUnknownIssuer},
		{"kdr token is not ours", "kdr_abcdefghijkl_" + strings.Repeat("a", 43), "", ErrNoCredentials},
		{"opaque bearer is not ours", "internal-token-value", "", ErrNoCredentials},
		{"no iss", jwt(t, map[string]any{"sub": "x"}), "", ErrNoCredentials},
		{"no header", "", "", ErrNoCredentials},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			p, _, err := step.Authenticate(nil, req)
			switch {
			case tc.wantErr == nil:
				if err != nil || p.ID != tc.wantID {
					t.Fatalf("got %+v, %v", p, err)
				}
			case errors.Is(tc.wantErr, ErrNoCredentials):
				if !errors.Is(err, ErrNoCredentials) {
					t.Fatalf("want skip, got %+v, %v", p, err)
				}
			default:
				if !errors.Is(err, tc.wantErr) || AsError(err).Status != 401 {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
			}
		})
	}
	// A fallback judges unowned issuers.
	step.Fallback = fakeVerifier{iss: "x"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+jwt(t, map[string]any{"iss": "https://unknown"}))
	if p, _, err := step.Authenticate(nil, req); err != nil || p.ID != "x" {
		t.Fatalf("fallback: %+v, %v", p, err)
	}
}

func TestPeerPolicyAndInternalToken(t *testing.T) {
	pol := PeerPolicy{GID: 2000, ServerUID: 65532}
	for _, tc := range []struct {
		c  PeerCred
		ok bool
	}{
		{PeerCred{UID: 0, GID: 0}, true},
		{PeerCred{UID: 65532, GID: 65532}, true},
		{PeerCred{UID: 1000, GID: 2000}, true},
		{PeerCred{UID: 1000, GID: 1000}, false},
	} {
		if got := pol.Allowed(tc.c); got != tc.ok {
			t.Errorf("Allowed(%+v) = %v", tc.c, got)
		}
	}
	if (PeerPolicy{GID: -1, ServerUID: 65532}).Allowed(PeerCred{UID: 1000, GID: 0}) {
		t.Error("GID -1 must not admit gid 0")
	}
	step := UnixPeer{Policy: pol}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, _, err := step.Authenticate(nil, req); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("TCP request: %v", err)
	}
	p, _, err := step.Authenticate(nil, req.WithContext(WithPeer(req.Context(), PeerCred{UID: 0})))
	if err != nil || p.Kind != KindLocalRoot || !p.BuiltinAdmin || p.AuthMethod != MethodUnix {
		t.Fatalf("root peer: %+v, %v", p, err)
	}
	if _, _, err := step.Authenticate(nil, req.WithContext(WithPeer(req.Context(), PeerCred{UID: 1000, GID: 1000}))); AsError(err).Status != 403 {
		t.Fatalf("foreign peer: %v", err)
	}

	it := InternalToken{Valid: func(s string) bool { return s == "node-secret" }}
	req.Header.Set("Authorization", "Bearer node-secret")
	p, _, err = it.Authenticate(nil, req)
	if err != nil || p.Kind != KindNode || len(p.Implicit) != 2 {
		t.Fatalf("internal token: %+v, %v", p, err)
	}
	req.Header.Set("Authorization", "Bearer other")
	if _, _, err := it.Authenticate(nil, req); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("other bearer: %v", err)
	}
}

func TestAuthorizeHelper(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	src, err := eerbac.NewBindingSource(perms, []rbac.Binding{{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "staff"}}}}, eerbac.SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rbac.NewEngine(rbac.EngineOptions{Permissions: perms, Bindings: src})
	if err != nil {
		t.Fatal(err)
	}
	read := rbac.Attributes{Resource: "zones", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	write := rbac.Attributes{Resource: "zones", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}}
	ctx := context.Background()
	if _, e := Authorize(ctx, eng, read); e == nil || e.Status != 401 {
		t.Fatalf("anonymous: %+v", e)
	}
	staff := WithPrincipal(ctx, Principal{Kind: KindUser, ID: "local:s", Name: "s", Groups: []string{"staff"}})
	d, e := Authorize(staff, eng, read)
	if e != nil || d.Path != rbac.PathBinding {
		t.Fatalf("staff read: %+v %+v", d, e)
	}
	_, e = Authorize(staff, eng, write)
	if e == nil || e.Status != 403 {
		t.Fatalf("staff write: %+v", e)
	}
	hint, _ := e.Hint.(ForbiddenHint)
	if hint.Missing != (Missing{Resource: "zones", Verb: "write", Scope: "zone:home"}) {
		t.Fatalf("hint %+v", e.Hint)
	}
	must := WithPrincipal(ctx, Principal{Kind: KindUser, ID: "local:a", Name: "a", BuiltinAdmin: true, MustChangePassword: true})
	if _, e := Authorize(must, eng, read); e == nil || e.Key != "password_change_required" {
		t.Fatalf("must change: %+v", e)
	}
	node := WithPrincipal(ctx, Principal{Kind: KindNode, ID: "node:internal", Implicit: []string{"auditor"}})
	if d, e := Authorize(node, eng, rbac.Attributes{Resource: "querylog", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}); e != nil || d.Path != rbac.PathLocal {
		t.Fatalf("node querylog read: %+v %+v", d, e)
	}
	if _, e := Authorize(node, eng, rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}); e == nil {
		t.Fatal("node principal may not write settings")
	}

	rec := httptest.NewRecorder()
	h := Require(eng, ClusterAttrs("zones", "read"), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if d, ok := DecisionFrom(r.Context()); !ok || d.Path != rbac.PathBinding {
			t.Errorf("decision %+v", d)
		}
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(staff))
	if rec.Code != 200 {
		t.Fatalf("Require: %d", rec.Code)
	}
}
