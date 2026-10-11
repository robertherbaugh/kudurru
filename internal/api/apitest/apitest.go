// Package apitest provides test doubles for the seams the API server
// consumes from the auth package (docs/architecture.md §8.8): a
// header-driven authenticator, a CSRF check and binding authorizers built
// from permissions.yaml. They let API, CLI and UI tests exercise the real
// authorization path without sessions, tokens or an IdP.
package apitest

import (
	"errors"
	"net/http"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// Test credentials.
const (
	// BearerPrefix starts a test bearer token: "Bearer test:<user>" or
	// "Bearer test:<user>:<group>,<group>".
	BearerPrefix = "test:"
	// CookieName carries a cookie session for tests: the value is
	// "<user>" or "<user>:<group>,<group>".
	CookieName = "kudurru_test_session"
	// CSRFToken is the X-Kudurru-CSRF value cookie sessions must send.
	CSRFToken = "csrf-test-token" //nolint:gosec // a fixed token of the test double, not a credential
	// HeaderCSRF is the CSRF header name (§8.2).
	HeaderCSRF = "X-Kudurru-CSRF"
)

// Bearer returns the Authorization header value for a test principal.
func Bearer(user string, groups ...string) string {
	v := "Bearer " + BearerPrefix + user
	if len(groups) > 0 {
		v += ":" + strings.Join(groups, ",")
	}
	return v
}

func parse(v string) (string, []string) {
	user, groups, _ := strings.Cut(v, ":")
	var gs []string
	if groups != "" {
		gs = strings.Split(groups, ",")
	}
	return user, gs
}

// Authenticate is the test double of the auth middleware: a "Bearer
// test:..." header or the test cookie puts an auth.Principal (Kind User)
// in the context; any other Authorization header answers 401; requests
// without credentials pass through anonymous. Cookie sessions carry an
// auth.Session with CSRFToken and the roles snapshot ["test"].
func Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			tok, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || !strings.HasPrefix(tok, BearerPrefix) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("WWW-Authenticate", `Bearer realm="kudurru"`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"key":"unauthenticated","message":"invalid credentials"},"took_ms":0}` + "\n"))
				return
			}
			user, groups := parse(strings.TrimPrefix(tok, BearerPrefix))
			p := auth.Principal{Kind: auth.KindUser, ID: user, Name: user, Groups: groups, AuthMethod: "token"}
			if strings.HasPrefix(user, "sa/") {
				p.Kind, p.Name = auth.KindServiceAccount, strings.TrimPrefix(user, "sa/")
			}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
			return
		}
		if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
			user, groups := parse(c.Value)
			p := auth.Principal{Kind: auth.KindUser, ID: user, Name: user, Groups: groups, AuthMethod: "cookie"}
			ctx := auth.WithPrincipal(r.Context(), p)
			ctx = auth.WithSession(ctx, auth.Session{ID: "test", Principal: p, CSRF: CSRFToken, RolesSnapshot: []string{"test"}})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ErrCSRF is returned by CSRF for a missing or wrong header.
var ErrCSRF = errors.New("missing or invalid X-Kudurru-CSRF header")

// CSRF is the test double of the cookie-session CSRF check.
func CSRF(r *http.Request) error {
	if r.Header.Get(HeaderCSRF) != CSRFToken {
		return ErrCSRF
	}
	return nil
}

// Authorizer builds a binding authorizer over the embedded
// permissions.yaml; it panics on invalid bindings (tests only).
func Authorizer(bindings ...rbac.Binding) rbac.Authorizer {
	p, err := rbac.LoadPermissions()
	if err != nil {
		panic(err)
	}
	a, err := rbac.NewBindingAuthorizer(p, bindings)
	if err != nil {
		panic(err)
	}
	return a
}

// Grant binds role to user at cluster scope.
func Grant(role, user string) rbac.Binding {
	return rbac.Binding{Role: role, Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: user}}}
}

// GrantZone binds role to user on one zone.
func GrantZone(role, user, zone string) rbac.Binding {
	return rbac.Binding{Role: role, Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: zone}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: user}}}
}

// StandardBindings grants each built-in role to the user of the same
// name (viewer, auditor, operator, admin) and zone-admin on zone
// "example" to user "zoneadmin".
func StandardBindings() []rbac.Binding {
	return []rbac.Binding{
		Grant("viewer", "viewer"), Grant("auditor", "auditor"), Grant("operator", "operator"), Grant("admin", "admin"),
		GrantZone("zone-admin", "zoneadmin", "example"),
	}
}
