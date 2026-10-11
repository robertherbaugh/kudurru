package session

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

func TestAuthenticateStep(t *testing.T) {
	f := newMWFixture(t)
	s := f.store.NewSession(auth.Principal{Kind: auth.KindUser, ID: "oidc:corp:x:1", Name: "corp:alice", Provider: "corp"}, []string{"viewer", "zone-admin@zone:home"}, "corp")
	v, err := f.store.Issue(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	ck := &http.Cookie{Name: DefaultCookieName, Value: v}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
	if _, _, err := f.authn.Authenticate(httptest.NewRecorder(), req); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("no cookie: %v", err)
	}
	req.AddCookie(ck)
	p, r2, err := f.authn.Authenticate(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthMethod != auth.MethodCookie || len(p.Grants) != 2 || !p.GrantsIssuedAt.Equal(s.IssuedAt.Truncate(time.Second)) {
		t.Fatalf("principal %+v", p)
	}
	if got, ok := auth.SessionFrom(r2.Context()); !ok || got.ID != s.ID {
		t.Fatal("the returned request carries no session")
	}

	// Bearer requests are not the cookie step's, even with a cookie.
	breq := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
	breq.AddCookie(ck)
	breq.Header.Set("Authorization", "Bearer x")
	if _, _, err := f.authn.Authenticate(httptest.NewRecorder(), breq); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("bearer request: %v", err)
	}

	// Unsafe method: CSRF header required (403, contract body via auth.Error).
	post := httptest.NewRequest(http.MethodPost, "/api/v1/zones", nil)
	post.AddCookie(ck)
	_, _, err = f.authn.Authenticate(httptest.NewRecorder(), post)
	if ae := auth.AsError(err); ae.Status != http.StatusForbidden || ae.Key != "csrf" {
		t.Fatalf("POST without CSRF: %v", err)
	}
	post.Header.Set(auth.CSRFHeader, s.CSRF)
	if _, _, err := f.authn.Authenticate(httptest.NewRecorder(), post); err != nil {
		t.Fatalf("POST with CSRF: %v", err)
	}

	// A broken cookie is cleared and the request continues anonymous.
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
	bad.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: v[:len(v)-3] + "AAA"})
	rec := httptest.NewRecorder()
	if _, _, err := f.authn.Authenticate(rec, bad); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("broken cookie: %v", err)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("broken cookie not cleared: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestHostPrefix(t *testing.T) {
	c := CookieConfig{HostPrefix: true}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	plain := httptest.NewRequest(http.MethodGet, "http://dns.example/", nil)
	tlsReq := httptest.NewRequest(http.MethodGet, "https://dns.example/", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if c.NameFor(plain) != DefaultCookieName || c.NameFor(tlsReq) != "__Host-"+DefaultCookieName {
		t.Fatalf("names %q %q", c.NameFor(plain), c.NameFor(tlsReq))
	}
	rec := httptest.NewRecorder()
	c.Set(rec, tlsReq, "v", time.Hour)
	if sc := rec.Header().Get("Set-Cookie"); !strings.HasPrefix(sc, "__Host-kudurru_session=v") || !strings.Contains(sc, "Secure") || !strings.Contains(sc, "Path=/") {
		t.Fatalf("Set-Cookie %q", sc)
	}
	scoped := CookieConfig{HostPrefix: true, Path: "/api/v1/auth/oidc/x/", Name: "pre"}
	if scoped.NameFor(tlsReq) != "pre" {
		t.Fatal("__Host- requires Path=/")
	}
	if (CookieConfig{}).NameFor(tlsReq) != DefaultCookieName {
		t.Fatal("HostPrefix is opt-in")
	}
}
