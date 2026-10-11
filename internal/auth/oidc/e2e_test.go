package oidc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// This file is the end-to-end suite shared by the dex test (dex_test.go)
// and the in-process mock OP test (e2e_mock_test.go): a kudurru HTTP
// server with the OIDC routes, the cookie authenticator, /auth/me,
// /auth/logout and a mutating route, driven by a cookie-jar "browser" that
// fills in the IdP's login form.

// Canonical test users; every IdP under test provisions them with
// e2ePassword. groupPrefix-less group names are what the IdP emits.
const (
	userAdmin   = "admin@example.com"  // groups kudurru-admins, staff; preferred_username admin
	userNobody  = "nobody@example.com" // groups staff only: no binding
	userMany    = "many@example.com"   // 200 groups, one of them kudurru-viewers
	e2ePassword = "correct horse battery staple"
	manyGroups  = 200
)

// manyUserGroups is the 200-group membership of userMany.
func manyUserGroups() []string {
	out := []string{"kudurru-viewers"}
	for i := 1; len(out) < manyGroups; i++ {
		out = append(out, fmt.Sprintf("org/departments/team-%03d-engineering", i))
	}
	return out
}

// testIDP describes an OpenID Provider the suite logs in against.
type testIDP struct {
	id           string // provider ID at kudurru
	issuer       string
	clientID     string
	clientSecret string
}

var (
	keyA = bytes.Repeat([]byte{0xA1}, session.KeySize)
	keyB = bytes.Repeat([]byte{0xB2}, session.KeySize)
)

// offsetClock is wall time plus an adjustable offset, so ID tokens minted
// by a real IdP stay valid while the session clock is pushed forward.
type offsetClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *offsetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *offsetClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

func (c *offsetClock) Reset() {
	c.mu.Lock()
	c.offset = 0
	c.mu.Unlock()
}

// harness is a kudurru API server whose auth stack can be rebuilt with new
// keys (a rolling restart) while browsers keep their cookie jars.
type harness struct {
	t          *testing.T
	srv        *httptest.Server
	handler    atomic.Value // http.Handler
	clk        offsetClock
	validAfter sync.Map // principal ID -> time.Time
	perms      *rbac.Permissions
	idp        testIDP
	groupPfx   string
	// tweak and resolver customise the next configure call.
	tweak    func(*ProviderConfig)
	resolver RoleResolver
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, perms: perms}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd, _ := h.handler.Load().(http.Handler)
		if hd == nil {
			http.Error(w, "not configured", http.StatusServiceUnavailable)
			return
		}
		hd.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) redirectURL(id string) string {
	return h.srv.URL + "/api/v1/auth/oidc/" + id + "/callback"
}

func (h *harness) bindings() []rbac.Binding {
	p := h.groupPfx
	return []rbac.Binding{
		{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: p + "kudurru-admins"}}},
		{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: p + "kudurru-viewers"}}},
		{Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home-example-com"}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: p + "admin"}}},
	}
}

func (h *harness) roleResolver() RoleResolver {
	if h.resolver != nil {
		return h.resolver
	}
	return BindingResolver{Bindings: h.bindings}
}

// configure (re)builds the auth stack: what a pod restart with the given
// KUDURRU_ENCRYPTION_KEY[_PREVIOUS] does.
func (h *harness) configure(idp testIDP, key, previous []byte) {
	h.t.Helper()
	h.idp = idp
	h.groupPfx = idp.id + ":"
	store, err := session.NewSealed(session.Options{
		Key: key, PreviousKey: previous, Now: h.clk.Now,
		ValidAfter: func(id string) time.Time {
			if v, ok := h.validAfter.Load(id); ok {
				return v.(time.Time)
			}
			return time.Time{}
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	authn, err := session.NewAuthenticator(session.AuthenticatorOptions{Store: store, Now: h.clk.Now})
	if err != nil {
		h.t.Fatal(err)
	}
	cfg := ProviderConfig{
		ID: idp.id, Issuer: idp.issuer, ClientID: idp.clientID, ClientSecret: idp.clientSecret,
		RedirectURL: h.redirectURL(idp.id),
		ClaimMappings: ClaimMappings{
			Username: PrefixedClaim{Claim: "preferred_username", Prefix: h.groupPfx},
			Groups:   PrefixedClaim{Claim: "groups", Prefix: h.groupPfx},
		},
	}
	if h.tweak != nil {
		h.tweak(&cfg)
	}
	prov, err := NewProvider(cfg, Options{
		Key: key, PreviousKey: previous,
		Sessions: store, SessionLifetime: store.Lifetime(),
		Resolver:    h.roleResolver(),
		Permissions: h.perms,
		Now:         h.clk.Now,
		// autoProvision: never admits subjects ending in "known".
		IdentityExists: func(_ context.Context, k IdentityKey) (bool, error) {
			return strings.HasSuffix(k.Subject, "known"), nil
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	reg, err := NewRegistry(prov)
	if err != nil {
		h.t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := reg.Register(mux, "/api/v1"); err != nil {
		h.t.Fatal(err)
	}
	mux.Handle("GET /api/v1/auth/me", session.MeHandler{Permissions: h.perms})
	mux.Handle("POST /api/v1/auth/logout", session.LogoutHandler{Authenticator: authn})
	mux.HandleFunc("POST /api/v1/zones/{zone}/records", func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "created by "+p.Name)
	})
	h.handler.Store(session.SecurityHeaders(false)(authn.Middleware(mux)))
}
