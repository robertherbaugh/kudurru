package session

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

type mwFixture struct {
	clk        *fakeClock
	store      *Sealed
	authn      *Authenticator
	handler    http.Handler
	validAfter sync.Map // principal ID -> time.Time
}

func newMWFixture(t *testing.T) *mwFixture {
	t.Helper()
	f := &mwFixture{clk: newFakeClock()}
	f.store = newTestStore(t, f.clk, func(o *Options) {
		o.ValidAfter = func(id string) time.Time {
			if v, ok := f.validAfter.Load(id); ok {
				return v.(time.Time)
			}
			return time.Time{}
		}
	})
	var err error
	f.authn, err = NewAuthenticator(AuthenticatorOptions{Store: f.store, Now: f.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/auth/me", MeHandler{Permissions: perms})
	mux.Handle("POST /api/v1/auth/logout", LogoutHandler{Authenticator: f.authn})
	mux.HandleFunc("/api/v1/echo", func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(p.Name + " via " + p.AuthMethod))
	})
	f.handler = SecurityHeaders(true)(f.authn.Middleware(mux))
	return f
}

func (f *mwFixture) issue(t *testing.T) (cookie string, s auth.Session) {
	t.Helper()
	s = f.store.NewSession(testPrincipal(), []string{"operator", "zone-admin@zone:home-example-com"}, "corp")
	v, err := f.store.Issue(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return v, s
}

func (f *mwFixture) do(method, path, cookie string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://kudurru.test"+path, nil)
	if cookie != "" {
		r.AddCookie(requestCookie(cookie))
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// requestCookie is the session cookie as a browser sends it back: name and
// value only.
func requestCookie(value string) *http.Cookie {
	return &http.Cookie{Name: DefaultCookieName, Value: value} //nolint:gosec // G124: request cookies carry no attributes
}

func setCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestAuthenticatorCSRFTable(t *testing.T) {
	f := newMWFixture(t)
	cookie, s := f.issue(t)
	cases := []struct {
		name   string
		method string
		cookie string
		hdr    map[string]string
		want   int
	}{
		{"GET needs no CSRF header", http.MethodGet, cookie, nil, http.StatusOK},
		{"HEAD needs no CSRF header", http.MethodHead, cookie, nil, http.StatusOK},
		{"POST without header", http.MethodPost, cookie, nil, http.StatusForbidden},
		{"POST with wrong header", http.MethodPost, cookie, map[string]string{CSRFHeader: "nope"}, http.StatusForbidden},
		{"POST with right header", http.MethodPost, cookie, map[string]string{CSRFHeader: s.CSRF}, http.StatusOK},
		{"PUT with right header", http.MethodPut, cookie, map[string]string{CSRFHeader: s.CSRF}, http.StatusOK},
		{"DELETE without header", http.MethodDelete, cookie, nil, http.StatusForbidden},
		{"cross-site Sec-Fetch-Site even with header", http.MethodPost, cookie, map[string]string{CSRFHeader: s.CSRF, "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-origin Sec-Fetch-Site with header", http.MethodPost, cookie, map[string]string{CSRFHeader: s.CSRF, "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"foreign Origin even with header", http.MethodPost, cookie, map[string]string{CSRFHeader: s.CSRF, "Origin": "https://evil.example"}, http.StatusForbidden},
		{"matching Origin with header", http.MethodPost, cookie, map[string]string{CSRFHeader: s.CSRF, "Origin": "http://kudurru.test"}, http.StatusOK},
		{"bearer request bypasses cookie and CSRF", http.MethodPost, cookie, map[string]string{"Authorization": "Bearer kdr_x"}, http.StatusUnauthorized},
		{"no cookie is anonymous", http.MethodPost, "", nil, http.StatusUnauthorized},
		{"garbage cookie is anonymous", http.MethodGet, "garbage", nil, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := f.do(c.method, "/api/v1/echo", c.cookie, c.hdr)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d; body %s", w.Code, c.want, w.Body)
			}
			if c.want == http.StatusOK && c.method != http.MethodHead && w.Body.String() != "corp:alice via cookie" {
				t.Errorf("body = %q", w.Body)
			}
			if c.want == http.StatusForbidden && !strings.Contains(w.Body.String(), `"error":"csrf"`) {
				t.Errorf("403 body = %q", w.Body)
			}
		})
	}
}

func TestAuthenticatorClearsBadCookie(t *testing.T) {
	f := newMWFixture(t)
	w := f.do(http.MethodGet, "/api/v1/echo", "tampered", nil)
	c := setCookie(w, DefaultCookieName)
	if c == nil || c.MaxAge >= 0 {
		t.Fatalf("bad cookie not cleared: %+v", c)
	}
}

func TestAuthenticatorSlidesIdleWithSetCookie(t *testing.T) {
	f := newMWFixture(t)
	cookie, _ := f.issue(t)

	// Within a minute: authenticated, no Set-Cookie.
	f.clk.Advance(30 * time.Second)
	w := f.do(http.MethodGet, "/api/v1/echo", cookie, nil)
	if w.Code != http.StatusOK || setCookie(w, DefaultCookieName) != nil {
		t.Fatalf("30s: status %d, Set-Cookie %v", w.Code, setCookie(w, DefaultCookieName))
	}
	// 20 minutes later: re-sealed.
	f.clk.Advance(20 * time.Minute)
	w = f.do(http.MethodGet, "/api/v1/echo", cookie, nil)
	c := setCookie(w, DefaultCookieName)
	if w.Code != http.StatusOK || c == nil || c.Value == cookie {
		t.Fatalf("20m: status %d, Set-Cookie %+v", w.Code, c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure || c.Path != "/" {
		t.Errorf("cookie attributes: %+v", c)
	}
	if want := int((DefaultLifetime - 20*time.Minute - 30*time.Second).Seconds()); c.MaxAge != want {
		t.Errorf("Max-Age = %d, want %d (absolute expiry, not idle)", c.MaxAge, want)
	}
	// 40 minutes after login the original cookie is idle-expired, the
	// re-sealed one is not.
	f.clk.Advance(20 * time.Minute)
	if w := f.do(http.MethodGet, "/api/v1/echo", cookie, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("stale cookie still accepted: %d", w.Code)
	}
	if w := f.do(http.MethodGet, "/api/v1/echo", c.Value, nil); w.Code != http.StatusOK {
		t.Fatalf("slid cookie rejected: %d", w.Code)
	}
}

func TestAuthenticatorSecureAuto(t *testing.T) {
	f := newMWFixture(t)
	cookie, _ := f.issue(t)
	f.clk.Advance(5 * time.Minute)
	r := httptest.NewRequest(http.MethodGet, "https://kudurru.test/api/v1/echo", nil)
	r.TLS = &tls.ConnectionState{}
	r.AddCookie(requestCookie(cookie))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if c := setCookie(w, DefaultCookieName); c == nil || !c.Secure {
		t.Fatalf("TLS request: cookie %+v, want Secure", c)
	}
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing on TLS response")
	}

	cfg := CookieConfig{Secure: SecureAuto}
	plain := httptest.NewRequest(http.MethodGet, "http://kudurru.test/", nil)
	plain.Header.Set("X-Forwarded-Proto", "https")
	if cfg.IsSecure(plain) {
		t.Error("X-Forwarded-Proto honoured without TrustForwardedProto")
	}
	cfg.TrustForwardedProto = true
	if !cfg.IsSecure(plain) {
		t.Error("X-Forwarded-Proto ignored with TrustForwardedProto")
	}
	if (CookieConfig{Secure: SecureAlways}).IsSecure(httptest.NewRequest(http.MethodGet, "/", nil)) != true {
		t.Error("always mode not Secure")
	}
	bad := CookieConfig{Secure: "sometimes"}
	if err := bad.Validate(); err == nil {
		t.Error("invalid secure mode accepted")
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newMWFixture(t)
	w := f.do(http.MethodGet, "/api/v1/echo", "", nil)
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; base-uri 'self'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain HTTP")
	}
}

func TestAuthenticatorValidAfterRevokes(t *testing.T) {
	f := newMWFixture(t)
	cookie, s := f.issue(t)
	if w := f.do(http.MethodGet, "/api/v1/echo", cookie, nil); w.Code != http.StatusOK {
		t.Fatalf("before revocation: %d", w.Code)
	}
	f.validAfter.Store(s.Principal.ID, f.clk.Now().Add(time.Hour))
	w := f.do(http.MethodGet, "/api/v1/echo", cookie, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("after sessionsValidAfter: %d", w.Code)
	}
	if c := setCookie(w, DefaultCookieName); c == nil || c.MaxAge >= 0 {
		t.Error("revoked cookie not cleared")
	}
}

func TestMeAndLogout(t *testing.T) {
	f := newMWFixture(t)
	if w := f.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous /me: %d %v", w.Code, w.Header())
	}
	cookie, s := f.issue(t)
	w := f.do(http.MethodGet, "/api/v1/auth/me", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/me: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("/me is cacheable")
	}
	var me MeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Principal.ID != s.Principal.ID || me.Principal.Name != "corp:alice" || me.Principal.AuthMethod != AuthMethodCookie {
		t.Errorf("principal = %+v", me.Principal)
	}
	if me.CSRF != s.CSRF || me.IDP != "corp" {
		t.Errorf("csrf/idp = %q/%q", me.CSRF, me.IDP)
	}
	if len(me.Bindings) != 2 || me.Bindings[1].Scope != (rbac.Scope{Kind: rbac.ScopeZone, Name: "home-example-com"}) {
		t.Errorf("bindings = %+v", me.Bindings)
	}
	if !contains(me.Permissions["cluster"], "records.write") || contains(me.Permissions["cluster"], "settings.write") {
		t.Errorf("operator permissions = %v", me.Permissions["cluster"])
	}
	if !contains(me.Permissions["zone:home-example-com"], "zones.transfer") {
		t.Errorf("zone-admin permissions = %v", me.Permissions["zone:home-example-com"])
	}

	// Logout is a mutating route: CSRF applies.
	if w := f.do(http.MethodPost, "/api/v1/auth/logout", cookie, nil); w.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF: %d", w.Code)
	}
	w = f.do(http.MethodPost, "/api/v1/auth/logout", cookie, map[string]string{CSRFHeader: s.CSRF})
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if c := setCookie(w, DefaultCookieName); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout did not clear the cookie: %+v", c)
	}
	// Documented limitation of sealed sessions: a copy of the cookie
	// replayed after logout still authenticates until it expires.
	if w := f.do(http.MethodGet, "/api/v1/auth/me", cookie, nil); w.Code != http.StatusOK {
		t.Fatalf("replayed cookie after logout: %d (expected the sealed-store limitation)", w.Code)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
