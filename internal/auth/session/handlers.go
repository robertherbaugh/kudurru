package session

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// MeResponse is the body of GET /api/v1/auth/me (§8.2): the principal, its
// effective grants, the permission matrix the UI gates on, and the CSRF
// token the SPA echoes in X-Kudurru-CSRF.
type MeResponse struct {
	Principal     MePrincipal         `json:"principal"`
	Roles         []string            `json:"roles"`
	Bindings      []MeBinding         `json:"bindings"`
	Permissions   map[string][]string `json:"permissions,omitempty"`
	IDP           string              `json:"idp,omitempty"`
	CSRF          string              `json:"csrf,omitempty"`
	IssuedAt      *time.Time          `json:"issuedAt,omitempty"`
	ExpiresAt     *time.Time          `json:"expiresAt,omitempty"`
	IdleExpiresAt *time.Time          `json:"idleExpiresAt,omitempty"`
}

// MePrincipal is the principal as shown to its owner.
type MePrincipal struct {
	Kind       auth.PrincipalKind `json:"kind"`
	ID         string             `json:"id"`
	Name       string             `json:"name,omitempty"`
	Provider   string             `json:"provider,omitempty"`
	AuthMethod string             `json:"authMethod,omitempty"`
}

// MeBinding is one effective grant in structured form.
type MeBinding struct {
	Role  string     `json:"role"`
	Scope rbac.Scope `json:"scope"`
}

// MeHandler serves GET /api/v1/auth/me for cookie sessions. Permissions,
// when set, expands each grant into "<resource>.<verb>" entries keyed by
// scope ("cluster", "zone:<name>") so the UI can hide what it cannot do.
type MeHandler struct {
	Permissions *rbac.Permissions
}

// ServeHTTP implements http.Handler.
func (h MeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, ok := auth.SessionFrom(r.Context())
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kudurru"`)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	grants, err := auth.ParseGrants(s.RolesSnapshot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "session roles snapshot is malformed")
		return
	}
	resp := MeResponse{
		Principal: MePrincipal{
			Kind: p.Kind, ID: p.ID, Name: p.Name, Provider: p.Provider, AuthMethod: p.AuthMethod,
		},
		Roles:         append([]string{}, s.RolesSnapshot...),
		Bindings:      make([]MeBinding, 0, len(grants)),
		IDP:           s.IDP,
		CSRF:          s.CSRF,
		IssuedAt:      &s.IssuedAt,
		ExpiresAt:     &s.ExpiresAt,
		IdleExpiresAt: &s.IdleExpiresAt,
	}
	for _, g := range grants {
		resp.Bindings = append(resp.Bindings, MeBinding{Role: g.Role, Scope: grantScope(g)})
	}
	if h.Permissions != nil {
		resp.Permissions = PermissionMatrix(h.Permissions, grants)
	}
	writeJSON(w, http.StatusOK, resp)
}

func grantScope(g auth.Grant) rbac.Scope {
	if g.IsCluster() {
		return rbac.Scope{Kind: rbac.ScopeCluster}
	}
	return rbac.Scope{Kind: rbac.ScopeKind(g.ScopeKind), Name: g.ScopeName}
}

// PermissionMatrix expands grants into sorted "<resource>.<verb>" lists
// keyed by scope. Unknown roles contribute nothing.
func PermissionMatrix(p *rbac.Permissions, grants []auth.Grant) map[string][]string {
	sets := map[string]map[string]struct{}{}
	for _, g := range grants {
		key := scopeString(g)
		set := sets[key]
		if set == nil {
			set = map[string]struct{}{}
			sets[key] = set
		}
		for res, verbs := range p.Resources {
			for _, verb := range verbs {
				if p.Grants(g.Role, res, verb) {
					set[res+"."+verb] = struct{}{}
				}
			}
		}
	}
	out := make(map[string][]string, len(sets))
	for scope, set := range sets {
		list := make([]string, 0, len(set))
		for perm := range set {
			list = append(list, perm)
		}
		sort.Strings(list)
		out[scope] = list
	}
	return out
}

// LogoutHandler serves POST /api/v1/auth/logout: it clears the session
// cookie and asks the store to revoke the session. The sealed store cannot
// revoke (auth.ErrUnsupported), so a copy of the cookie stays valid until
// its idle or absolute expiry; "log out everywhere" is sessionsValidAfter.
// The route sits behind the Authenticator, so a logout without the CSRF
// header is rejected before it gets here (no logout CSRF).
type LogoutHandler struct {
	Authenticator *Authenticator
}

// ServeHTTP implements http.Handler.
func (h LogoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	if s, ok := auth.SessionFrom(r.Context()); ok {
		if err := h.Authenticator.store.Revoke(r.Context(), s.ID); err != nil && !errors.Is(err, auth.ErrUnsupported) {
			writeError(w, http.StatusInternalServerError, "internal", "revoke failed")
			return
		}
	}
	h.Authenticator.cookie.Clear(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// scopeString renders a grant scope as a permission-matrix key.
func scopeString(g auth.Grant) string {
	if g.IsCluster() {
		return auth.ScopeCluster
	}
	return g.ScopeKind + ":" + g.ScopeName
}
