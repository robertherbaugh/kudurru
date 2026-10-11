package stack

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// BenchmarkAuthenticateAuthorize measures Middleware + Authorize per
// request for each credential kind, the per-request cost the API server
// pays before its handler runs.
func BenchmarkAuthenticateAuthorize(b *testing.B) {
	cfg, err := config.Load("", nil)
	if err != nil {
		b.Fatal(err)
	}
	cfg.RBAC.Bindings = []map[string]any{{"role": "viewer", "subjects": []any{map[string]any{"kind": "Group", "name": "staff"}}}}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	ctx := context.Background()
	s, err := New(ctx, Options{Config: cfg, Store: authtest.Memory(b), Keys: keys.Keys{Current: key}, Out: io.Discard,
		Logger: slog.New(slog.DiscardHandler), Getenv: func(string) string { return "" },
		InternalToken: func(t string) bool { return t == "node-token" }})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := s.Users().Create(ctx, users.CreateOptions{Name: "alice", Password: "alice-password", Groups: []string{"staff"}}); err != nil {
		b.Fatal(err)
	}
	usr, _ := s.Users().Get("alice")
	sess := s.Sessions().NewSession(users.Principal(usr), nil, "local")
	cookieValue, err := s.Sessions().Issue(ctx, sess)
	if err != nil {
		b.Fatal(err)
	}
	wire, _, err := s.Tokens().Create(ctx, tokens.CreateOptions{Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: usr.Name, ID: users.PrincipalID(usr.Name), Provider: users.Provider}})
	if err != nil {
		b.Fatal(err)
	}
	h := s.Middleware()(auth.Require(s.Authorizer(), func(*http.Request) rbac.Attributes {
		return rbac.Attributes{Resource: "zones", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	for _, bc := range []struct {
		name string
		set  func(*http.Request)
	}{
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "kudurru_session", Value: cookieValue}) }}, //nolint:gosec // G124: request cookie
		{"token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+wire) }},
		{"internal", func(r *http.Request) { r.Header.Set("Authorization", "Bearer node-token") }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
			bc.set(req)
			b.ReportAllocs()
			for b.Loop() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 200 {
					b.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
			}
		})
	}
}
