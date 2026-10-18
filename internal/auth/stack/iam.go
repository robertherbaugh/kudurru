package stack

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/adminsock"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TokenInfo is an API token as listed (never the secret or its hash).
type TokenInfo struct {
	ID          string                `json:"id"`
	Description string                `json:"description,omitempty"`
	Owner       internalv1.TokenOwner `json:"owner"`
	CreatedAt   time.Time             `json:"createdAt"`
	ExpiresAt   *time.Time            `json:"expiresAt"`
	LastUsedAt  *time.Time            `json:"lastUsedAt,omitempty"`
	LastUsedIP  string                `json:"lastUsedIP,omitempty"`
	Scopes      []string              `json:"scopes,omitempty"`
}

// CreatedToken is the answer of POST /auth/tokens; Token is shown once.
type CreatedToken struct {
	TokenInfo
	Token string `json:"token"`
}

func tokenInfo(t *internalv1.APIToken) TokenInfo {
	out := TokenInfo{ID: t.Name, Description: t.Spec.Description, Owner: t.Spec.Owner, CreatedAt: t.CreationTimestamp.UTC(), Scopes: t.Spec.Scopes,
		LastUsedIP: t.Status.LastUsedIP}
	if e := t.Spec.ExpiresAt; e != nil {
		v := e.UTC()
		out.ExpiresAt = &v
	}
	if u := t.Status.LastUsedAt; u != nil {
		v := u.UTC()
		out.LastUsedAt = &v
	}
	return out
}

// ownerID is the token-owner ID of p ("" when p cannot own tokens).
func ownerID(p auth.Principal) string {
	switch p.Kind {
	case auth.KindUser, auth.KindServiceAccount:
		return p.ID
	}
	return ""
}

func (s *Stack) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	owner := ownerID(p)
	if r.URL.Query().Get("all") == "true" {
		if _, e := auth.Authorize(r.Context(), s.engine, rbac.Attributes{Resource: "tokens", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}); e != nil {
			writeErr(w, r, e)
			return
		}
		owner = ""
	} else if owner == "" {
		writeList(w, r, []TokenInfo{}, nil)
		return
	}
	items := []TokenInfo{}
	for _, t := range s.tokens.List(owner) {
		items = append(items, tokenInfo(t))
	}
	writeList(w, r, items, nil)
}

// CreateTokenRequest is the body of POST /auth/tokens.
type CreateTokenRequest struct {
	Description string `json:"description,omitempty"`
	// ExpiresIn is a Go duration ("720h"); empty means
	// auth.tokens.maxLifetime (or never).
	ExpiresIn string `json:"expiresIn,omitempty"`
	// Scopes narrow the token to grants in wire form ("viewer",
	// "zone-admin@zone:home-example-com").
	Scopes []string `json:"scopes,omitempty"`
}

func (s *Stack) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var req CreateTokenRequest
	if e := decode(r, &req); e != nil {
		writeErr(w, r, e)
		return
	}
	opts := tokens.CreateOptions{Description: req.Description, Scopes: req.Scopes}
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: "expiresIn: not a positive Go duration"})
			return
		}
		opts.ExpiresIn = d
	}
	for i, sc := range req.Scopes {
		if _, err := rbac.ParseGrant(sc); err != nil || !internalv1.ValidGrant(sc) {
			writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: fmt.Sprintf("scopes[%d]: %q is not role or role@kind:name", i, sc)})
			return
		}
		if g, _ := rbac.ParseGrant(sc); s.perms.Roles[g.Role] == nil {
			writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: fmt.Sprintf("scopes[%d]: unknown role %q", i, g.Role)})
			return
		}
	}
	switch p.Kind {
	case auth.KindUser:
		opts.Owner = internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: p.Name, ID: p.ID, Provider: p.Provider}
		if p.Provider != "local" {
			// OIDC users have no stored groups (D48): freeze what they hold
			// now; the binding source keeps honouring each grant only while
			// a live binding still provides it.
			for _, g := range s.effectiveGrants(p) {
				opts.Grants = append(opts.Grants, g.String())
			}
		}
	case auth.KindServiceAccount:
		ns, name, _ := strings.Cut(p.ServiceAccount, "/")
		opts.Owner = internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: name, Namespace: ns}
	default:
		writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "unsupported_principal",
			Message: "API tokens belong to users and kudurru service accounts; " + string(p.Kind) + " principals cannot own them"})
		return
	}
	// A token-authenticated request cannot mint a broader or longer-lived
	// token than its own.
	if p.TokenID != "" {
		if len(p.Scopes) > 0 {
			if len(opts.Scopes) == 0 {
				opts.Scopes = slices.Clone(p.Scopes)
			}
			for _, sc := range opts.Scopes {
				if !slices.Contains(p.Scopes, sc) {
					writeErr(w, r, &auth.Error{Status: http.StatusForbidden, Key: "forbidden", Message: fmt.Sprintf("scope %q exceeds the scopes of the token in use", sc)})
					return
				}
			}
		}
		if parent, ok := s.tokens.Get(p.TokenID); ok && parent.Spec.ExpiresAt != nil {
			left := time.Until(parent.Spec.ExpiresAt.Time)
			if opts.ExpiresIn == 0 || opts.ExpiresIn > left {
				opts.ExpiresIn = max(left, time.Second)
			}
		}
	}
	wire, tok, err := s.tokens.Create(r.Context(), opts)
	switch {
	case errors.Is(err, tokens.ErrLifetimeTooLong):
		writeErr(w, r, &auth.Error{Status: http.StatusBadRequest, Key: "invalid", Message: "expiresIn: " + err.Error()})
		return
	case err != nil:
		writeErr(w, r, &auth.Error{Status: http.StatusInternalServerError, Key: "internal", Message: "could not create the token", Err: err})
		return
	}
	writeJSON(w, http.StatusCreated, CreatedToken{TokenInfo: tokenInfo(tok), Token: wire})
}

// tokenFor returns the token id of the request path when the caller owns
// it or holds verb on tokens. Someone else's token without the permission
// is reported missing, not forbidden (no existence oracle).
func (s *Stack) tokenFor(r *http.Request, verb string) (*internalv1.APIToken, *auth.Error) {
	p, _ := auth.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	notFound := &auth.Error{Status: http.StatusNotFound, Key: "not_found", Message: "no token " + id}
	tok, ok := s.tokens.Get(id)
	if !ok {
		return nil, notFound
	}
	if own := ownerID(p); own == "" || tok.Spec.Owner.ID != own {
		if d, err := s.engine.Authorize(r.Context(), auth.RBACPrincipal(p), rbac.Attributes{Resource: "tokens", Verb: verb, Scope: rbac.Scope{Kind: rbac.ScopeCluster}}); err != nil || !d.Allowed {
			return nil, notFound
		}
	}
	return tok, nil
}

func (s *Stack) handleGetToken(w http.ResponseWriter, r *http.Request) {
	tok, e := s.tokenFor(r, "read")
	if e != nil {
		writeErr(w, r, e)
		return
	}
	writeJSON(w, http.StatusOK, tokenInfo(tok))
}

func (s *Stack) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	notFound := &auth.Error{Status: http.StatusNotFound, Key: "not_found", Message: "no token " + id}
	if _, e := s.tokenFor(r, "delete"); e != nil {
		writeErr(w, r, e)
		return
	}
	if err := s.tokens.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, tokens.ErrNotFound) {
			writeErr(w, r, notFound)
			return
		}
		writeErr(w, r, &auth.Error{Status: http.StatusInternalServerError, Key: "internal", Message: "could not revoke the token", Err: err})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Stack) handleIAMUsers(w http.ResponseWriter, r *http.Request) {
	items := []adminsock.User{}
	for _, u := range s.users.List() {
		items = append(items, adminsock.UserOf(u))
	}
	writeList(w, r, items, nil)
}

// ServiceAccountInfo is a kudurru service account as listed.
type ServiceAccountInfo struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Disabled     bool   `json:"disabled"`
	BuiltinAdmin bool   `json:"builtinAdmin"`
	Tokens       int    `json:"tokens"`
}

func (s *Stack) handleIAMServiceAccounts(w http.ResponseWriter, r *http.Request) {
	sas := s.tokens.ServiceAccounts().List()
	slices.SortFunc(sas, func(a, b *internalv1.ServiceAccount) int {
		return compareStrings(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	items := []ServiceAccountInfo{}
	for _, sa := range sas {
		items = append(items, ServiceAccountInfo{Namespace: sa.Namespace, Name: sa.Name, Description: sa.Spec.Description,
			Disabled: sa.Spec.Disabled, BuiltinAdmin: sa.Spec.BuiltinAdmin, Tokens: len(s.tokens.List(tokens.ServiceAccountID(sa.Namespace, sa.Name)))})
	}
	writeList(w, r, items, nil)
}

// RoleBindingInfo is one binding as listed: from the config file
// (rbac.bindings, Source "config") or a RoleBinding object.
type RoleBindingInfo struct {
	Source    string         `json:"source"`
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Role      string         `json:"role"`
	Scope     rbac.Scope     `json:"scope"`
	Subjects  []rbac.Subject `json:"subjects"`
	ManagedBy string         `json:"managedBy,omitempty"`
}

func (s *Stack) handleIAMRoleBindings(w http.ResponseWriter, r *http.Request) {
	items := []RoleBindingInfo{}
	static, _ := rbac.ParseBindings(s.cfg.RBAC.Bindings)
	for i, b := range static {
		items = append(items, RoleBindingInfo{Source: "config", Name: fmt.Sprintf("rbac.bindings[%d]", i), Role: b.Role, Scope: b.Scope, Subjects: b.Subjects})
	}
	objs := s.roleBindings.List()
	slices.SortFunc(objs, func(a, b *internalv1.RoleBinding) int {
		return compareStrings(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	for _, o := range objs {
		b := rbac.FromRoleBinding(o)
		items = append(items, RoleBindingInfo{Source: "object", Namespace: o.Namespace, Name: o.Name, Role: b.Role, Scope: b.Scope, Subjects: b.Subjects,
			ManagedBy: store.ManagedBy(o)})
	}
	writeList(w, r, items, s.bindings.Diagnoses())
}

// IdentityProviderInfo is a configured provider as listed (no secrets).
type IdentityProviderInfo struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Issuer      string   `json:"issuer"`
	ClientID    string   `json:"clientID"`
	CLIClientID string   `json:"cliClientID,omitempty"`
	Scopes      []string `json:"scopes"`
	RedirectURL string   `json:"redirectURL"`
	Source      string   `json:"source"`
}

func (s *Stack) handleIAMIdentityProviders(w http.ResponseWriter, r *http.Request) {
	items := []IdentityProviderInfo{}
	fromConfig := map[string]bool{}
	for _, c := range s.configIDPs {
		fromConfig[c.Name] = true
	}
	if reg := s.activeRegistry(); reg != nil {
		for _, pi := range reg.Providers() {
			p, _ := reg.Provider(pi.ID)
			c := p.Config()
			src := "object"
			if fromConfig[c.ID] {
				src = "config"
			}
			items = append(items, IdentityProviderInfo{ID: c.ID, DisplayName: c.DisplayName, Issuer: c.Issuer, ClientID: c.ClientID,
				CLIClientID: c.CLIClientID, Scopes: c.Scopes, RedirectURL: c.RedirectURL, Source: src})
		}
	}
	s.regMu.Lock()
	diag := slices.Clone(s.regDiag)
	s.regMu.Unlock()
	writeList(w, r, items, diag)
}
