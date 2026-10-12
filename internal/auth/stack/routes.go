package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// Access is how a route is guarded.
type Access int

// Route access levels.
const (
	// AccessPublic routes serve anonymous requests (status, login, OIDC).
	AccessPublic Access = iota
	// AccessAuthenticated routes need any principal (self-service: me,
	// password, logout); users who must change their password get them.
	AccessAuthenticated
	// AccessPermission routes need Resource.Verb at cluster scope.
	AccessPermission
	// AccessAnyScope routes need Resource.Verb at cluster scope or at the
	// scope of one of the principal's grants (a scoped API token's scopes
	// stand in for them, and still restrict every check). The own-token
	// routes use it: every built-in role holds tokens.own, including the
	// zone-scoped zone-admin, whose binding cannot satisfy a cluster-scope
	// check.
	AccessAnyScope
)

// Route is one API route of the identity layer with its RBAC declaration,
// so the API server can register it next to its own routes (and its
// route-permission lint sees the declaration).
type Route struct {
	Method string
	// Path is relative to the API prefix ("/auth/login").
	Path   string
	Access Access
	// Resource and Verb are the permission of AccessPermission and
	// AccessAnyScope routes (internal/rbac/permissions.yaml).
	Resource, Verb string
	// Handler serves the route; it expects the stack's Middleware to have
	// run, but does no Authorize of its own for AccessPermission and
	// AccessAnyScope routes.
	Handler http.Handler
}

// Routes returns the /auth and /iam routes:
//
//	GET    /auth/status                    public
//	POST   /auth/login                     public (CrossOriginProtection, lockouts)
//	POST   /auth/logout                    public (clears the cookie; CSRF via the cookie step)
//	GET    /auth/me                        authenticated
//	POST   /auth/password                  authenticated (local users)
//	GET    /auth/oidc/{idp}/start          public
//	GET    /auth/oidc/{idp}/callback       public
//	GET    /auth/tokens                    tokens.own at any held scope
//	POST   /auth/tokens                    tokens.own at any held scope
//	GET    /auth/tokens/{id}               tokens.own at any held scope (others' tokens need tokens.read)
//	DELETE /auth/tokens/{id}               tokens.own at any held scope (others' tokens need tokens.delete)
//	GET    /iam/users                      users.read
//	GET    /iam/serviceaccounts            serviceaccounts.read
//	GET    /iam/rolebindings               rolebindings.read
//	GET    /iam/identityproviders          identityproviders.read
func (s *Stack) Routes() []Route {
	return []Route{
		{Method: http.MethodGet, Path: "/auth/status", Access: AccessPublic, Handler: http.HandlerFunc(s.handleStatus)},
		{Method: http.MethodPost, Path: "/auth/login", Access: AccessPublic, Handler: http.HandlerFunc(s.handleLogin)},
		{Method: http.MethodPost, Path: "/auth/logout", Access: AccessPublic, Handler: http.HandlerFunc(s.handleLogout)},
		{Method: http.MethodGet, Path: "/auth/me", Access: AccessAuthenticated, Handler: http.HandlerFunc(s.handleMe)},
		{Method: http.MethodPost, Path: "/auth/password", Access: AccessAuthenticated, Handler: http.HandlerFunc(s.handlePassword)},
		{Method: http.MethodGet, Path: "/auth/oidc/{idp}/start", Access: AccessPublic, Handler: http.HandlerFunc(s.handleOIDC)},
		{Method: http.MethodGet, Path: "/auth/oidc/{idp}/callback", Access: AccessPublic, Handler: http.HandlerFunc(s.handleOIDC)},
		{Method: http.MethodGet, Path: "/auth/tokens", Access: AccessAnyScope, Resource: "tokens", Verb: "own", Handler: http.HandlerFunc(s.handleListTokens)},
		{Method: http.MethodPost, Path: "/auth/tokens", Access: AccessAnyScope, Resource: "tokens", Verb: "own", Handler: http.HandlerFunc(s.handleCreateToken)},
		{Method: http.MethodGet, Path: "/auth/tokens/{id}", Access: AccessAnyScope, Resource: "tokens", Verb: "own", Handler: http.HandlerFunc(s.handleGetToken)},
		{Method: http.MethodDelete, Path: "/auth/tokens/{id}", Access: AccessAnyScope, Resource: "tokens", Verb: "own", Handler: http.HandlerFunc(s.handleDeleteToken)},
		{Method: http.MethodGet, Path: "/iam/users", Access: AccessPermission, Resource: "users", Verb: "read", Handler: http.HandlerFunc(s.handleIAMUsers)},
		{Method: http.MethodGet, Path: "/iam/serviceaccounts", Access: AccessPermission, Resource: "serviceaccounts", Verb: "read", Handler: http.HandlerFunc(s.handleIAMServiceAccounts)},
		{Method: http.MethodGet, Path: "/iam/rolebindings", Access: AccessPermission, Resource: "rolebindings", Verb: "read", Handler: http.HandlerFunc(s.handleIAMRoleBindings)},
		{Method: http.MethodGet, Path: "/iam/identityproviders", Access: AccessPermission, Resource: "identityproviders", Verb: "read", Handler: http.HandlerFunc(s.handleIAMIdentityProviders)},
	}
}

// Guard wraps a route's handler with its access rule (auth.Require for
// permission routes). The API server may use its own authorize wrapper
// instead; this one records the decision with auth.WithDecision.
func (s *Stack) Guard(rt Route) http.Handler {
	switch rt.Access {
	case AccessAuthenticated:
		return auth.RequireAuthenticated(rt.Handler)
	case AccessPermission:
		return auth.Require(s.engine, auth.ClusterAttrs(rt.Resource, rt.Verb), rt.Handler)
	case AccessAnyScope:
		return s.requireAnyScope(rt.Resource, rt.Verb, rt.Handler)
	}
	return rt.Handler
}

// requireAnyScope is auth.Require for AccessAnyScope: resource.verb at
// cluster scope, else at the scope of each grant the principal holds and
// of each scope of a scoped API token. Every check goes through the
// engine, so a token's scopes keep restricting it (both the token scope
// and the owner's grant must cover the scope checked) and a scope the
// principal holds nothing at grants nothing. The 403 is the
// cluster-scope one.
func (s *Stack) requireAnyScope(resource, verb string, next http.Handler) http.Handler {
	cluster := auth.ClusterAttrs(resource, verb)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		d, e := auth.Authorize(ctx, s.engine, cluster(r))
		if e != nil && e.Status == http.StatusForbidden && e.Key == "forbidden" {
			p, _ := auth.PrincipalFrom(ctx)
			rp := auth.RBACPrincipal(p)
			grants := s.effectiveGrants(p)
			if len(p.Scopes) > 0 {
				grants = append(grants, s.shownGrants(p)...)
			}
			seen := map[rbac.Scope]bool{{Kind: rbac.ScopeCluster}: true}
			for _, g := range grants {
				if seen[g.Scope] {
					continue
				}
				seen[g.Scope] = true
				a := rbac.Attributes{Resource: resource, Verb: verb, Scope: g.Scope}
				if sd, err := s.engine.Authorize(ctx, rp, a); err == nil && sd.Allowed {
					d, e = sd, nil
					break
				}
			}
		}
		if e != nil {
			auth.WriteError(w, r, e)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithDecision(ctx, d)))
	})
}

// Register mounts every route on mux under the API prefix, guarded by
// Guard. The mux must sit behind Middleware.
func (s *Stack) Register(mux *http.ServeMux) {
	for _, rt := range s.Routes() {
		mux.Handle(rt.Method+" "+s.opts.APIPrefix+rt.Path, s.Guard(rt))
	}
}

// StatusResponse is GET /auth/status.
type StatusResponse struct {
	// LocalLogin is false only when auth.localLogin is disabled.
	LocalLogin bool `json:"localLogin"`
	// LocalLoginMode is enabled, admins-only or disabled.
	LocalLoginMode        string              `json:"localLoginMode"`
	Providers             []oidcProviderInfo  `json:"providers"`
	HasDefaultCredentials bool                `json:"hasDefaultCredentials"`
	Session               *statusSessionLimit `json:"session,omitempty"`
}

type oidcProviderInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type statusSessionLimit struct {
	IdleTimeout string `json:"idleTimeout"`
	Lifetime    string `json:"lifetime"`
}

func (s *Stack) localLoginMode() string {
	if m := s.cfg.Auth.LocalLogin; m != "" {
		return m
	}
	return "enabled"
}

func (s *Stack) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := StatusResponse{
		LocalLoginMode: s.localLoginMode(), Providers: []oidcProviderInfo{},
		HasDefaultCredentials: s.users.HasDefaultCredentials(),
		Session:               &statusSessionLimit{IdleTimeout: s.sealed.IdleTimeout().String(), Lifetime: s.sealed.Lifetime().String()},
	}
	resp.LocalLogin = resp.LocalLoginMode != "disabled"
	if reg := s.activeRegistry(); reg != nil {
		for _, p := range reg.Providers() {
			resp.Providers = append(resp.Providers, oidcProviderInfo{ID: p.ID, DisplayName: p.DisplayName})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// LoginRequest is the body of POST /auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Stack) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.cop.Check(r); err != nil {
		writeErr(w, r, &auth.Error{Status: http.StatusForbidden, Key: "csrf", Message: "cross-origin login rejected", Err: err})
		return
	}
	mode := s.localLoginMode()
	if mode == "disabled" {
		writeErr(w, r, &auth.Error{Status: http.StatusForbidden, Key: "local_login_disabled", Message: "local password login is disabled",
			Hint: "sign in with an identity provider, or use `kudurru admin` on the host"})
		return
	}
	var req LoginRequest
	if e := decode(r, &req); e != nil {
		writeErr(w, r, e)
		return
	}
	if req.Username == "" || req.Password == "" {
		writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: "username and password are required"})
		return
	}
	usr, err := s.users.Login(r.Context(), req.Username, req.Password, s.opts.ClientIP(r))
	var locked *users.LockedError
	switch {
	case errors.As(err, &locked):
		writeErr(w, r, &auth.Error{Status: http.StatusTooManyRequests, Key: "rate_limited", Message: "too many failed logins; try again later", RetryAfter: locked.RetryAfter})
		return
	case errors.Is(err, users.ErrDisabled):
		writeErr(w, r, &auth.Error{Status: http.StatusForbidden, Key: "account_disabled", Message: "this account is disabled"})
		return
	case errors.Is(err, users.ErrInvalidCredentials):
		writeErr(w, r, &auth.Error{Status: http.StatusUnauthorized, Key: "unauthenticated", Message: "invalid username or password"})
		return
	case err != nil:
		writeErr(w, r, &auth.Error{Status: http.StatusInternalServerError, Key: "internal", Message: "login failed", Err: err})
		return
	}
	p := users.Principal(usr)
	if mode == "admins-only" && !s.isAdmin(p) {
		writeErr(w, r, &auth.Error{Status: http.StatusForbidden, Key: "local_login_admins_only", Message: "local password login is limited to administrators",
			Hint: "sign in with an identity provider"})
		return
	}
	sess, e := s.issueSession(w, r, p)
	if e != nil {
		writeErr(w, r, e)
		return
	}
	p.AuthMethod = auth.MethodCookie
	writeJSON(w, http.StatusOK, s.me(r, p, &sess))
}

// isAdmin reports whether p holds admin at cluster scope (builtin or bound).
func (s *Stack) isAdmin(p auth.Principal) bool {
	if p.BuiltinAdmin {
		return true
	}
	for _, g := range s.grantsFor(auth.RBACPrincipal(p)) {
		if g.Role == "admin" && (g.Scope.Kind == "" || g.Scope.Kind == rbac.ScopeCluster) {
			return true
		}
	}
	return false
}

// issueSession seals a session for a local user and sets the cookie. Its
// iat is placed after the user's sessionsValidAfter (one-second
// resolution), so a session issued right after a password change is not
// born revoked.
func (s *Stack) issueSession(w http.ResponseWriter, r *http.Request, p auth.Principal) (auth.Session, *auth.Error) {
	sess := s.sealed.NewSession(p, nil, users.Provider)
	if va := s.users.ValidAfter(p.ID); !va.IsZero() && !sess.IssuedAt.Truncate(time.Second).After(va) {
		sess.IssuedAt = va.Add(time.Second)
		sess.ExpiresAt = sess.IssuedAt.Add(s.sealed.Lifetime())
	}
	v, err := s.sealed.Issue(r.Context(), sess)
	if err != nil {
		return sess, &auth.Error{Status: http.StatusInternalServerError, Key: "internal", Message: "could not create the session", Err: err}
	}
	s.cookie.Set(w, r, v, sess.ExpiresAt.Sub(s.opts.Now()))
	return sess, nil
}

func (s *Stack) handleLogout(w http.ResponseWriter, r *http.Request) {
	if e := auth.CSRFFailure(r.Context()); e != nil {
		writeErr(w, r, e) // no logout CSRF
		return
	}
	s.cookie.Clear(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// MeResponse is GET /auth/me.
type MeResponse struct {
	Principal MePrincipal `json:"principal"`
	// Roles are the effective grants in wire form ("admin",
	// "zone-admin@zone:home-example-com").
	Roles []string `json:"roles"`
	// CSRF is the token cookie sessions echo in X-Kudurru-CSRF.
	CSRF string `json:"csrf,omitempty"`
	// Permissions are the cluster-scope verbs per resource.
	Permissions map[string][]string `json:"permissions"`
	// ScopedPermissions add verbs per scope ("zone:home-example-com").
	ScopedPermissions  map[string]map[string][]string `json:"scopedPermissions,omitempty"`
	MustChangePassword bool                           `json:"mustChangePassword,omitempty"`
	ExpiresAt          *time.Time                     `json:"expiresAt,omitempty"`
	IdleExpiresAt      *time.Time                     `json:"idleExpiresAt,omitempty"`
}

// MePrincipal is the principal as shown to itself.
type MePrincipal struct {
	Kind       auth.PrincipalKind `json:"kind"`
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Groups     []string           `json:"groups"`
	Provider   string             `json:"provider,omitempty"`
	AuthMethod string             `json:"authMethod,omitempty"`
	TokenID    string             `json:"tokenID,omitempty"`
}

func (s *Stack) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var sess *auth.Session
	if v, ok := auth.SessionFrom(r.Context()); ok {
		sess = &v
	}
	writeJSON(w, http.StatusOK, s.me(r, p, sess))
}

// effectiveGrants lists what p holds: builtin admin, implicit grants,
// live bindings and the still-valid grants of a session or token.
func (s *Stack) effectiveGrants(p auth.Principal) []rbac.Grant {
	rp := auth.RBACPrincipal(p)
	var out []rbac.Grant
	add := func(g rbac.Grant) {
		if g.Scope.Kind == "" {
			g.Scope.Kind = rbac.ScopeCluster
		}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	if rp.BuiltinAdmin {
		add(rbac.Grant{Role: "admin"})
	}
	for _, g := range rp.Implicit {
		add(g)
	}
	for _, g := range s.grantsFor(rp) {
		add(g)
	}
	for _, g := range s.validGrants(rp.Grants, rp.GrantsIssuedAt) {
		add(g)
	}
	return out
}

// shownGrants are the grants /auth/me reports: the effective grants, or
// a scoped API token's scopes, to which every decision is narrowed (the
// permission matrix shows what remains of the owner's grants).
func (s *Stack) shownGrants(p auth.Principal) []rbac.Grant {
	if len(p.Scopes) == 0 {
		return s.effectiveGrants(p)
	}
	var out []rbac.Grant
	for _, sc := range p.Scopes {
		g, err := rbac.ParseGrant(sc)
		if err != nil {
			continue
		}
		if g.Scope.Kind == "" {
			g.Scope.Kind = rbac.ScopeCluster
		}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

func (s *Stack) me(r *http.Request, p auth.Principal, sess *auth.Session) MeResponse {
	resp := MeResponse{
		Principal: MePrincipal{Kind: p.Kind, ID: p.ID, Name: p.Name, Groups: p.Groups, Provider: p.Provider, AuthMethod: p.AuthMethod, TokenID: p.TokenID},
		Roles:     []string{}, Permissions: map[string][]string{}, MustChangePassword: p.MustChangePassword,
	}
	if resp.Principal.Groups == nil {
		resp.Principal.Groups = []string{}
	}
	if sess != nil {
		resp.CSRF = sess.CSRF
		exp, idle := sess.ExpiresAt.UTC(), sess.IdleExpiresAt.UTC()
		resp.ExpiresAt, resp.IdleExpiresAt = &exp, &idle
	}
	grants := s.shownGrants(p)
	for _, g := range grants {
		resp.Roles = append(resp.Roles, g.String())
	}
	slices.Sort(resp.Roles)
	if p.MustChangePassword {
		// Authorize answers 403 password_change_required everywhere else.
		return resp
	}
	ctx := r.Context()
	rp := auth.RBACPrincipal(p)
	matrix := func(scope rbac.Scope) map[string][]string {
		out := map[string][]string{}
		for res, verbs := range s.perms.Resources {
			for _, verb := range verbs {
				if d, err := s.engine.Authorize(ctx, rp, rbac.Attributes{Resource: res, Verb: verb, Scope: scope}); err == nil && d.Allowed {
					out[res] = append(out[res], verb)
				}
			}
		}
		return out
	}
	resp.Permissions = matrix(rbac.Scope{Kind: rbac.ScopeCluster})
	for _, g := range grants {
		if g.Scope.Kind == rbac.ScopeCluster {
			continue
		}
		key := auth.ScopeString(g.Scope)
		if resp.ScopedPermissions == nil {
			resp.ScopedPermissions = map[string]map[string][]string{}
		}
		if _, done := resp.ScopedPermissions[key]; !done {
			resp.ScopedPermissions[key] = matrix(g.Scope)
		}
	}
	return resp
}

// PasswordRequest is the body of POST /auth/password.
type PasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	// RevokeTokens also revokes every API token of the user (a changed
	// password alone revokes sessions, not tokens).
	RevokeTokens bool `json:"revokeTokens,omitempty"`
}

func (s *Stack) handlePassword(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if p.Kind != auth.KindUser || p.Provider != users.Provider {
		writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "not_local_user", Message: "only local accounts have a kudurru password"})
		return
	}
	var req PasswordRequest
	if e := decode(r, &req); e != nil {
		writeErr(w, r, e)
		return
	}
	if err := users.CheckNewPassword(req.NewPassword); err != nil {
		writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: "newPassword: " + err.Error()})
		return
	}
	if req.NewPassword == req.CurrentPassword {
		writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: "newPassword: must differ from the current password"})
		return
	}
	// Verify through Login so the lockouts apply to the current password.
	_, err := s.users.Login(r.Context(), p.Name, req.CurrentPassword, s.opts.ClientIP(r))
	var locked *users.LockedError
	switch {
	case errors.As(err, &locked):
		writeErr(w, r, &auth.Error{Status: http.StatusTooManyRequests, Key: "rate_limited", Message: "too many failed attempts; try again later", RetryAfter: locked.RetryAfter})
		return
	case errors.Is(err, users.ErrInvalidCredentials):
		writeErr(w, r, &auth.Error{Status: http.StatusForbidden, Key: "forbidden", Message: "the current password is wrong"})
		return
	case err != nil:
		writeErr(w, r, &auth.Error{Status: http.StatusForbidden, Key: "forbidden", Message: err.Error()})
		return
	}
	if err := s.users.SetPassword(r.Context(), p.Name, req.NewPassword, false); err != nil {
		writeErr(w, r, &auth.Error{Status: http.StatusInternalServerError, Key: "internal", Message: "could not change the password", Err: err})
		return
	}
	if req.RevokeTokens {
		n, err := s.tokens.RevokeOwner(r.Context(), p.ID)
		if err != nil {
			writeErr(w, r, &auth.Error{Status: http.StatusInternalServerError, Key: "internal", Message: "the password was changed, but its API tokens could not all be revoked", Err: err})
			return
		}
		s.log.InfoContext(r.Context(), "api tokens revoked with a password change", "user", p.Name, "count", n)
	}
	p.MustChangePassword = false
	var sess *auth.Session
	if _, ok := auth.SessionFrom(r.Context()); ok {
		// Every earlier session is revoked now; hand this browser a new one.
		ns, e := s.issueSession(w, r, p)
		if e != nil {
			writeErr(w, r, e)
			return
		}
		sess = &ns
	}
	writeJSON(w, http.StatusOK, s.me(r, p, sess))
}

func (s *Stack) handleOIDC(w http.ResponseWriter, r *http.Request) {
	if reg := s.activeRegistry(); reg != nil {
		if p, ok := reg.Provider(r.PathValue("idp")); ok {
			if strings.HasSuffix(r.URL.Path, "/callback") {
				p.Callback(w, r)
			} else {
				p.Start(w, r)
			}
			return
		}
	}
	writeErr(w, r, &auth.Error{Status: http.StatusNotFound, Key: "not_found", Message: "no identity provider " + r.PathValue("idp")})
}

// listEnvelope is the contract's list body.
type listEnvelope struct {
	Items    any    `json:"items"`
	Continue string `json:"continue"`
	TookMS   int64  `json:"took_ms"`
	// Diagnoses explain ignored entries (rolebindings, identity providers).
	Diagnoses []string `json:"diagnoses,omitempty"`
}

func writeList(w http.ResponseWriter, r *http.Request, items any, diag []string) {
	writeJSON(w, http.StatusOK, listEnvelope{Items: items, TookMS: auth.TookMS(r), Diagnoses: diag})
}

func writeErr(w http.ResponseWriter, r *http.Request, e *auth.Error) { auth.WriteError(w, r, e) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body (64 KiB cap, unknown fields rejected).
func decode(r *http.Request, v any) *auth.Error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil {
		return &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: "unreadable body"}
	}
	if len(body) > 64<<10 {
		return &auth.Error{Status: http.StatusRequestEntityTooLarge, Key: "invalid", Message: "body too large"}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: "body: " + err.Error()}
	}
	return nil
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Roles lists p's effective grants in their wire form (role or
// role@kind:name), for api.Options.Roles; a scoped API token reports its
// scopes.
func (s *Stack) Roles(_ context.Context, p auth.Principal) []string {
	grants := s.shownGrants(p)
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		out = append(out, g.String())
	}
	slices.Sort(out)
	return out
}
