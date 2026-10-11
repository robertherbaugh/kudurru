package auth

import (
	"context"
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// Missing is the hint of a 403: what the request needed.
type Missing struct {
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
	Scope    string `json:"scope"`
}

// ForbiddenHint is the 403 hint body: {"missing":{resource,verb,scope}}.
type ForbiddenHint struct {
	Missing Missing `json:"missing"`
}

type decisionCtxKey struct{}

// WithDecision records the authorization decision of a request so the
// Audit step can write Decision.Path (binding | sar | local) next to the
// outcome.
func WithDecision(ctx context.Context, d rbac.Decision) context.Context {
	return context.WithValue(ctx, decisionCtxKey{}, d)
}

// DecisionFrom returns the decision set by WithDecision.
func DecisionFrom(ctx context.Context) (rbac.Decision, bool) {
	d, ok := ctx.Value(decisionCtxKey{}).(rbac.Decision)
	return d, ok
}

// ScopeString renders a scope as the 403 hint and the permission matrix
// write it: "cluster" or "<kind>:<name>".
func ScopeString(s rbac.Scope) string {
	if s.Kind == "" || s.Kind == rbac.ScopeCluster {
		return string(rbac.ScopeCluster)
	}
	return string(s.Kind) + ":" + s.Name
}

// Authorize is the Authorize step of §8.8 for one request: 401
// "unauthenticated" without a principal, 403 "password_change_required"
// for a local user who must change the password first, 403 "forbidden"
// with the missing permission in the hint when az denies, and otherwise
// the allowing decision (whose Path the audit row records).
func Authorize(ctx context.Context, az rbac.Authorizer, a rbac.Attributes) (rbac.Decision, *Error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		if e := CSRFFailure(ctx); e != nil {
			return rbac.Decision{Path: rbac.PathNone}, e
		}
		return rbac.Decision{Path: rbac.PathNone}, &Error{Status: http.StatusUnauthorized, Key: "unauthenticated", Message: "authentication required",
			Hint: "log in, or send Authorization: Bearer <token>"}
	}
	if p.MustChangePassword {
		return rbac.Decision{Path: rbac.PathNone}, &Error{Status: http.StatusForbidden, Key: "password_change_required",
			Message: "change your password before using the API", Hint: "POST /api/v1/auth/password"}
	}
	d, err := az.Authorize(ctx, RBACPrincipal(p), a)
	if err != nil {
		return d, &Error{Status: http.StatusInternalServerError, Key: "internal", Message: "authorization failed", Err: err}
	}
	if !d.Allowed {
		m := a
		if d.Missing != nil {
			m = *d.Missing
		}
		return d, &Error{Status: http.StatusForbidden, Key: "forbidden",
			Message: "you may not " + m.Verb + " " + m.Resource + " at scope " + ScopeString(m.Scope),
			Hint:    ForbiddenHint{Missing: Missing{Resource: m.Resource, Verb: m.Verb, Scope: ScopeString(m.Scope)}}}
	}
	return d, nil
}

// Require wraps next with Authorize; attrs derives the attributes from the
// request (resource and verb from the route declaration, scope from the
// path). The decision is stored with WithDecision.
func Require(az rbac.Authorizer, attrs func(*http.Request) rbac.Attributes, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, e := Authorize(r.Context(), az, attrs(r))
		if e != nil {
			WriteError(w, r, e)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithDecision(r.Context(), d)))
	})
}

// RequireAuthenticated wraps next so it only runs for an authenticated
// principal (the self-service routes: /auth/me, /auth/password, own
// tokens). Local users who must change their password are let through.
func RequireAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFrom(r.Context()); !ok {
			if e := CSRFFailure(r.Context()); e != nil {
				WriteError(w, r, e)
				return
			}
			WriteError(w, r, &Error{Status: http.StatusUnauthorized, Key: "unauthenticated", Message: "authentication required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClusterAttrs returns attributes for a cluster-scoped permission.
func ClusterAttrs(resource, verb string) func(*http.Request) rbac.Attributes {
	return func(*http.Request) rbac.Attributes {
		return rbac.Attributes{Resource: resource, Verb: verb, Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	}
}
