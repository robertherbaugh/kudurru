package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (h *servingHTTP) register() {
	h.route("GET", "/auth/status", h.status)
	h.route("POST", "/auth/login", h.login)
	h.route("POST", "/auth/logout", h.logout)
	h.route("GET", "/auth/me", h.me)
	h.route("POST", "/auth/password", h.password)
	h.route("GET", "/auth/sessions", h.sessions)
	h.route("DELETE", "/auth/sessions/{id}", h.revokeSession)
	h.route("GET", "/auth/tokens", h.listTokens)
	h.route("POST", "/auth/tokens", h.createToken)
	h.route("GET", "/auth/tokens/{id}", h.getToken)
	h.route("DELETE", "/auth/tokens/{id}", h.deleteToken)
	h.route("GET", "/iam/users", h.iamUsers)
	h.route("GET", "/iam/serviceaccounts", h.iamServiceAccounts)
	h.route("GET", "/iam/rolebindings", h.iamRoleBindings)
	h.route("GET", "/iam/identityproviders", h.iamProviders)
	for _, p := range []string{"/auth/oidc/{idp}/start", "/auth/oidc/{idp}/callback"} {
		h.route("GET", p, func(w http.ResponseWriter, r *http.Request) error {
			if h.opts.ProviderHandler == nil {
				return &auth.Error{Status: 503, Key: "unavailable", Message: "captured browser provider login unavailable"}
			}
			h.opts.ProviderHandler.ServeHTTP(w, r)
			return nil
		})
	}
}
func (h *servingHTTP) status(w http.ResponseWriter, r *http.Request) error {
	cfg, err := h.opts.Runtime.Settings(r.Context())
	if err != nil {
		return err
	}
	v, err := servinghttp.Component[*servingauth.View](r.Context(), "auth")
	if err != nil {
		return err
	}
	objects, err := v.Objects("LocalUser")
	if err != nil {
		return err
	}
	out := api.AuthStatus{LocalLogin: cfg.Auth.LocalLogin != config.LocalLoginDisabled, LocalLoginMode: cfg.Auth.LocalLogin, Providers: []api.AuthProvider{}, Session: &api.AuthSessionLimits{IdleTimeout: cfg.Auth.Session.IdleTimeout.Std().String(), Lifetime: cfg.Auth.Session.Lifetime.Std().String()}}
	for _, o := range objects {
		u := o.(*internalv1.LocalUser)
		out.HasDefaultCredentials = out.HasDefaultCredentials || u.Spec.BuiltinAdmin && u.Spec.MustChangePassword && !u.Spec.Disabled
	}
	// Browser capabilities are explicit. A verifier alone is not a login route.
	if h.opts.ProviderHandler != nil {
		for _, p := range h.providerInfos(v) {
			out.Providers = append(out.Providers, api.AuthProvider{ID: p.ID, DisplayName: p.DisplayName})
		}
	}
	return servingHTTPJSON(w, 200, out)
}

func (h *servingHTTP) login(w http.ResponseWriter, r *http.Request) error {
	if err := h.opts.CrossOrigin.Check(r); err != nil {
		return &auth.Error{Status: 403, Key: "csrf", Message: "cross-origin login rejected"}
	}
	var req api.LoginRequest
	if err := servingHTTPDecode(w, r, &req); err != nil {
		return err
	}
	req.Username = users.NormalizeUsername(req.Username)
	if req.Username == "" || len(req.Username) > 253 || req.Password == "" || len(req.Password) > users.MaxPasswordLen {
		return servingHTTPInvalid("username and password are required and must fit their limits")
	}
	if err := h.checkLoginLimit(r, req.Username); err != nil {
		return err
	}
	if err := h.factorAdmission(req.Username, req.TOTPCode); err != nil {
		return err
	}
	cfg, err := h.opts.Runtime.Settings(r.Context())
	if err != nil {
		return err
	}
	if cfg.Auth.LocalLogin == config.LocalLoginDisabled {
		return &auth.Error{Status: 403, Key: "local_login_disabled", Message: "local password login is disabled"}
	}
	var enrollment security.Enrollment
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var err error
		observed := h.now()
		enrollment, err = security.IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: req.Username, Value: req.Password, TOTPCode: req.TOTPCode}, observed)
		if err != nil {
			return err
		}
		// Preserve the public IAM login status in the same successful command.
		var current internalv1.LocalUser
		if err = tx.Get(ctx, store.Key{Kind: "LocalUser", Name: req.Username}, &current); err != nil {
			return err
		}
		at := metav1.NewTime(observed)
		current.Status.LastLoginAt = &at
		if err = tx.UpdateStatus(ctx, &current); err != nil {
			return err
		}
		return h.enrollmentClock(ctx, tx, enrollment, observed)
	})
	h.loginResult(r, req.Username, err)
	if err != nil {
		return err
	}
	return h.sealEnrollment(w, r, enrollment)
}

// enrollmentClock captures detached durable bounds after IssueLocal, never
// authenticating or consuming a factor again. The final check is memory/clock only.
func (h *servingHTTP) enrollmentClock(ctx context.Context, tx *sqlite.CommandTx, e security.Enrollment, observed time.Time) error {
	state, err := servingHTTPSecurity(ctx, tx)
	if err != nil {
		return err
	}
	for _, row := range state.Sessions {
		if row.ID == e.ID() {
			return tx.RequireCommitCheck(func(ctx context.Context) error {
				now := h.now()
				if err := ctx.Err(); err != nil {
					return err
				}
				if now.Before(observed) || now.Before(row.IssuedAt) || !now.Before(row.ExpiresAt) || !now.Before(row.IdleExpiresAt) {
					return servingauth.ErrCredentials
				}
				return nil
			})
		}
	}
	return servingauth.ErrManifest
}
func (h *servingHTTP) factorAdmission(name, code string) error {
	h.factorMu.Lock()
	defer h.factorMu.Unlock()
	if code == "" {
		return nil
	}
	if h.factorLimit.Locked(name) > 0 {
		return &auth.Error{Status: 429, Key: "rate_limited", Message: "factor attempts must be at least one second apart", RetryAfter: time.Second}
	}
	h.factorLimit.Fail(name)
	return nil
}
func (h *servingHTTP) sealEnrollment(w http.ResponseWriter, r *http.Request, e security.Enrollment) error {
	h.opts.Runtime.publisher.Wake()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	for {
		changed := h.opts.Runtime.publisher.Subscribe()
		lease, err := h.opts.Runtime.publisher.CaptureHTTP(ctx)
		if err == nil {
			value, ok := lease.Bundle().Component("auth")
			v, typed := value.(*servingauth.View)
			if ok && typed {
				raw, sess, sealErr := e.Seal(v, h.now())
				if sealErr == nil {
					out, err := h.sessionInfo(v, sess.Principal, &sess)
					if err == nil {
						h.opts.Cookie.Set(w, r, raw, sess.ExpiresAt.Sub(h.now()))
						err = servingHTTPJSON(w, 200, out)
					}
					lease.Release()
					return err
				}
			}
			lease.Release()
		} else if !errors.Is(err, serving.ErrPending) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (h *servingHTTP) sessionInfo(v *servingauth.View, p auth.Principal, sess *auth.Session) (api.SessionInfo, error) {
	roles, err := v.Roles(p, h.now())
	if err != nil {
		return api.SessionInfo{}, err
	}
	out := api.SessionInfo{Principal: api.SessionPrincipal{Kind: string(p.Kind), ID: p.ID, Name: p.Name, Groups: append([]string{}, p.Groups...), Provider: p.Provider, AuthMethod: p.AuthMethod, TokenID: p.TokenID}, Roles: roles, Permissions: map[string][]string{}, MustChangePassword: p.MustChangePassword}
	if sess != nil {
		out.CSRF = sess.CSRF
		exp, idle := sess.ExpiresAt, sess.IdleExpiresAt
		out.ExpiresAt, out.IdleExpiresAt = &exp, &idle
	}
	if p.MustChangePassword {
		return out, nil
	}
	matrix := func(scope rbac.Scope) map[string][]string {
		out := map[string][]string{}
		for resource, verbs := range h.permissions.Resources {
			for _, verb := range verbs {
				d, err := v.Authorize(p, rbac.Attributes{Resource: resource, Verb: verb, Scope: scope}, h.now())
				if err == nil && d.Allowed {
					out[resource] = append(out[resource], verb)
				}
			}
		}
		return out
	}
	out.Permissions = matrix(rbac.Scope{Kind: rbac.ScopeCluster})
	for _, wire := range roles {
		g, _ := rbac.ParseGrant(wire)
		scope := g.Normalized().Scope
		if scope.Kind == rbac.ScopeCluster {
			continue
		}
		if out.ScopedPermissions == nil {
			out.ScopedPermissions = map[string]map[string][]string{}
		}
		out.ScopedPermissions[auth.ScopeString(scope)] = matrix(scope)
	}
	return out, nil
}
func (h *servingHTTP) me(w http.ResponseWriter, r *http.Request) error {
	v, p, err := h.readPrincipal(r.Context())
	if err != nil {
		return err
	}
	var sess *auth.Session
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	if proof.Kind == servingauth.Cookie {
		s, err := v.CookieSession(proof.Value, h.now())
		if err != nil {
			return err
		}
		sess = &s
	}
	out, err := h.sessionInfo(v, p, sess)
	if err != nil {
		return err
	}
	return servingHTTPJSON(w, 200, out)
}
func (h *servingHTTP) logout(w http.ResponseWriter, r *http.Request) error {
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if _, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, proof, h.now); err != nil {
			return err
		}
		if proof.Kind != servingauth.Cookie {
			return nil
		}
		// The original verified session is used only as an identifier. Current raw
		// cookie verification above proves the same durable enrollment and UID.
		s, ok := auth.SessionFrom(r.Context())
		if !ok {
			return servingauth.ErrCredentials
		}
		state, err := security.Load(ctx, tx)
		if err != nil {
			return err
		}
		if err = state.Revoke(s.ID); err != nil {
			return err
		}
		return state.Commit(ctx)
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	h.opts.Cookie.Clear(w, r)
	w.WriteHeader(204)
	return nil
}

func servingHTTPSessionOwner(a servingauth.AccountRef, p auth.Principal) bool {
	if a.UID == "" || a.UID != p.AccountUID {
		return false
	}
	switch a.Kind {
	case "LocalUser":
		return p.Kind == auth.KindUser && p.Provider == "local" && a.Name == p.Name && a.Namespace == ""
	case "ExternalIdentity":
		return p.External != nil
	default:
		return false
	}
}
func (h *servingHTTP) sessions(w http.ResponseWriter, r *http.Request) error {
	v, p, err := h.readPrincipal(r.Context())
	if err != nil {
		return err
	}
	local, external, err := v.SessionRecords()
	if err != nil {
		return err
	}
	sess, _ := auth.SessionFrom(r.Context())
	out := []api.SessionRecordInfo{}
	add := func(id string, a servingauth.AccountRef, issued, expires, idle time.Time, revoked bool) {
		if servingHTTPSessionOwner(a, p) {
			out = append(out, api.SessionRecordInfo{ID: id, IssuedAt: issued, ExpiresAt: expires, IdleExpiresAt: idle, Current: id == sess.ID, Revoked: revoked})
		}
	}
	for _, s := range local {
		add(s.ID, s.Account, s.IssuedAt, s.ExpiresAt, s.IdleExpiresAt, s.Revoked)
	}
	for _, s := range external {
		add(s.ID, s.Account, s.IssuedAt, s.ExpiresAt, s.IdleExpiresAt, s.Revoked)
	}
	slices.SortFunc(out, func(a, b api.SessionRecordInfo) int { return b.IssuedAt.Compare(a.IssuedAt) })
	return servingHTTPList(w, r, out)
}
func (h *servingHTTP) revokeSession(w http.ResponseWriter, r *http.Request) error {
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		p, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, proof, h.now)
		if err != nil {
			return err
		}
		inventory, err := servingHTTPSecurity(ctx, tx)
		if err != nil {
			return err
		}
		found := false
		for _, s := range inventory.Sessions {
			if s.ID == r.PathValue("id") && servingHTTPSessionOwner(s.Account, p) {
				found = true
			}
		}
		for _, s := range inventory.ExternalSessions {
			if s.ID == r.PathValue("id") && servingHTTPSessionOwner(s.Account, p) {
				found = true
			}
		}
		if !found {
			return servingHTTPMissing()
		}
		state, err := security.Load(ctx, tx)
		if err != nil {
			return err
		}
		if err = state.Revoke(r.PathValue("id")); err != nil {
			return err
		}
		return state.Commit(ctx)
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	w.WriteHeader(204)
	return nil
}
