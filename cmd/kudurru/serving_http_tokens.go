package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servingHTTPOwnsToken(p auth.Principal, t *internalv1.APIToken) bool {
	o := t.Spec.Owner
	if p.AccountUID == "" || o.UID != p.AccountUID || o.ID != p.ID {
		return false
	}
	switch p.Kind {
	case auth.KindUser:
		return o.Kind == internalv1.OwnerUser && o.Provider == p.Provider && o.Name == p.Name && o.Namespace == ""
	case auth.KindServiceAccount:
		return o.Kind == internalv1.OwnerServiceAccount && o.Namespace+"/"+o.Name == p.ServiceAccount
	default:
		return false
	}
}
func servingHTTPTokenInfo(t *internalv1.APIToken) api.TokenInfo {
	o := t.Spec.Owner
	out := api.TokenInfo{ID: t.Name, Description: t.Spec.Description, Owner: api.TokenOwner{Kind: o.Kind, Name: o.Name, Namespace: o.Namespace, ID: o.ID, Provider: o.Provider, UID: o.UID}, CreatedAt: t.CreationTimestamp.Time, Scopes: slices.Clone(t.Spec.Scopes), LastUsedIP: t.Status.LastUsedIP}
	if t.Spec.ExpiresAt != nil {
		e := t.Spec.ExpiresAt.Time
		out.ExpiresAt = &e
	}
	if t.Status.LastUsedAt != nil {
		e := t.Status.LastUsedAt.Time
		out.LastUsedAt = &e
	}
	return out
}
func (h *servingHTTP) listTokens(w http.ResponseWriter, r *http.Request) error {
	if _, err := h.anyTokenScope(r.Context()); err != nil {
		return err
	}
	v, p, err := h.readPrincipal(r.Context())
	if err != nil {
		return err
	}
	all := r.URL.Query().Get("all") == "true"
	if all {
		d, err := v.Authorize(p, servingHTTPCluster("tokens", "read"), h.now())
		if err != nil {
			return err
		}
		if !d.Allowed {
			return servingauth.ErrDenied
		}
	}
	objects, err := v.Objects("APIToken")
	if err != nil {
		return err
	}
	items := []api.TokenInfo{}
	for _, o := range objects {
		t := o.(*internalv1.APIToken)
		if all || servingHTTPOwnsToken(p, t) {
			items = append(items, servingHTTPTokenInfo(t))
		}
	}
	slices.SortFunc(items, func(a, b api.TokenInfo) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return servingHTTPList(w, r, items)
}
func (h *servingHTTP) getToken(w http.ResponseWriter, r *http.Request) error {
	if _, err := h.anyTokenScope(r.Context()); err != nil {
		return err
	}
	v, p, err := h.readPrincipal(r.Context())
	if err != nil {
		return err
	}
	objects, err := v.Objects("APIToken")
	if err != nil {
		return err
	}
	for _, o := range objects {
		t := o.(*internalv1.APIToken)
		if t.Name != r.PathValue("id") {
			continue
		}
		if !servingHTTPOwnsToken(p, t) {
			d, err := v.Authorize(p, servingHTTPCluster("tokens", "read"), h.now())
			if err != nil || !d.Allowed {
				return servingHTTPMissing()
			}
		}
		return servingHTTPJSON(w, 200, servingHTTPTokenInfo(t))
	}
	return servingHTTPMissing()
}

// tokenIntent is one validated personal token creation. Wire is set on the
// node that generated the secret; a forwarded creation (D368) carries only the
// follower-generated ID and the secret's hash.
type tokenIntent struct {
	Description string
	Scopes      []string
	Life        time.Duration
	ID          string
	SecretHash  string
	Wire        string
}

// tokenRequestIntent validates the request body of POST /auth/tokens. It is
// repeated by the primary for a forwarded creation.
func tokenRequestIntent(permissions *rbac.Permissions, req api.CreateTokenRequest) (tokenIntent, error) {
	if len(req.Scopes) > servingauth.MaxGrants || len(req.Description) > 4096 {
		return tokenIntent{}, servingHTTPInvalid("token description or scope count exceeds limits")
	}
	for _, wire := range req.Scopes {
		g, err := rbac.ParseGrant(wire)
		if err != nil || !internalv1.ValidGrant(wire) || permissions == nil || permissions.Roles[g.Role] == nil {
			return tokenIntent{}, servingHTTPInvalid("invalid token scope")
		}
	}
	life := time.Duration(0)
	if req.ExpiresIn != "" {
		var err error
		life, err = time.ParseDuration(req.ExpiresIn)
		if err != nil || life <= 0 {
			return tokenIntent{}, servingHTTPInvalid("expiresIn must be a positive Go duration")
		}
	}
	return tokenIntent{Description: req.Description, Scopes: slices.Clone(req.Scopes), Life: life}, nil
}

func (h *servingHTTP) createToken(w http.ResponseWriter, r *http.Request) error {
	attrs, err := h.anyTokenScope(r.Context())
	if err != nil {
		return err
	}
	var req api.CreateTokenRequest
	if err = servingHTTPDecode(w, r, &req); err != nil {
		return err
	}
	intent, err := tokenRequestIntent(h.permissions, req)
	if err != nil {
		return err
	}
	if handled, err := h.forwardTokenCreate(w, r, attrs, req); handled {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	intent.Wire, intent.ID = tokens.Generate()
	intent.SecretHash = tokens.Hash(intent.Wire)
	var created *internalv1.APIToken
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var err error
		created, _, err = h.opts.Runtime.stageToken(ctx, tx, deps, proof, attrs, intent)
		return err
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	return servingHTTPJSON(w, 201, api.CreatedToken{TokenInfo: servingHTTPTokenInfo(created), Token: intent.Wire})
}

// stageToken is the body of POST /auth/tokens inside one current command:
// the caller's exact reauthorization, ownership, the scope subset, the
// cluster's maximum lifetime and the parent token's expiry, then a create-only
// write that must authenticate from its persisted form. staged reports whether
// the write was attempted; an error before it changed nothing.
func (s *servingRuntime) stageToken(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, attrs rbac.Attributes, in tokenIntent) (*internalv1.APIToken, bool, error) {
	p, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, s.opts.Now)
	if err != nil {
		return nil, false, err
	}
	if p.AccountUID == "" {
		return nil, false, servingauth.ErrDenied
	}
	owner := internalv1.TokenOwner{ID: p.ID, UID: p.AccountUID, Provider: p.Provider}
	switch p.Kind {
	case auth.KindUser:
		owner.Kind, owner.Name = internalv1.OwnerUser, p.Name
	case auth.KindServiceAccount:
		owner.Kind = internalv1.OwnerServiceAccount
		owner.Namespace, owner.Name, _ = strings.Cut(p.ServiceAccount, "/")
	default:
		return nil, false, servingHTTPInvalid("this principal cannot own API tokens")
	}
	scopes := slices.Clone(in.Scopes)
	if len(p.Scopes) > 0 {
		if len(scopes) == 0 {
			scopes = slices.Clone(p.Scopes)
		}
		for _, s := range scopes {
			if !slices.Contains(p.Scopes, s) {
				return nil, false, servingauth.ErrDenied
			}
		}
	}
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	var settings publicv1.Kudurru
	if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
		return nil, false, err
	}
	doc, err := servingSettingsDocument(&settings)
	if err != nil {
		return nil, false, err
	}
	cfg, _, err := config.ApplyClusterDocument(s.opts.Config, doc)
	if err != nil {
		return nil, false, err
	}
	maxLife := cfg.Auth.Tokens.MaxLifetime.Std()
	duration := in.Life
	if maxLife > 0 {
		if duration > maxLife {
			return nil, false, servingHTTPInvalid("expiresIn exceeds auth.tokens.maxLifetime")
		}
		if duration == 0 {
			duration = maxLife
		}
	}
	now := s.opts.Now().UTC()
	var expires time.Time
	if duration > 0 {
		expires = now.Add(duration).Truncate(time.Second)
	}
	if p.TokenID != "" {
		var parent internalv1.APIToken
		if err = tx.Get(ctx, store.Key{Kind: "APIToken", Name: p.TokenID}, &parent); err != nil {
			return nil, false, err
		}
		if !servingHTTPOwnsToken(p, &parent) {
			return nil, false, servingauth.ErrCredentials
		}
		if parent.Spec.ExpiresAt != nil && (expires.IsZero() || parent.Spec.ExpiresAt.Time.Before(expires)) {
			expires = parent.Spec.ExpiresAt.Time
		}
	}
	if !expires.IsZero() && !expires.After(now) {
		return nil, false, servingauth.ErrCredentials
	}
	t := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: in.ID, CreationTimestamp: metav1.NewTime(now)}, Spec: internalv1.APITokenSpec{Owner: owner, Description: in.Description, SecretHash: in.SecretHash, Scopes: scopes}}
	if p.External != nil {
		t.Spec.Grants = slices.Clone(p.Grants)
	}
	if !expires.IsZero() {
		e := metav1.NewTime(expires)
		t.Spec.ExpiresAt = &e
	}
	if err = t.Validate(); err != nil {
		return nil, false, servingHTTPInvalid("invalid token policy")
	}
	out, err := tx.Apply(ctx, t, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
	if err != nil {
		return nil, true, err
	}
	created := out.(*internalv1.APIToken)
	// SQLite metadata has whole-second precision. A newly issued token
	// must authenticate from its persisted representation, including every
	// current floor; never return a token born revoked or backdate it. A
	// forwarded creation has no secret here and checks the stored hash.
	var issued auth.Principal
	if in.Wire != "" {
		issued, err = servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: in.Wire}, s.opts.Now)
	} else {
		issued, err = servingauth.AuthenticateIssuedTokenCurrent(ctx, tx, deps, in.ID, in.SecretHash, s.opts.Now)
	}
	if err != nil {
		return nil, true, err
	}
	if issued.AccountUID != p.AccountUID || issued.ID != p.ID {
		return nil, true, servingauth.ErrCredentials
	}
	return created, true, tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		at := s.opts.Now().UTC()
		if at.Before(now) || !expires.IsZero() && !at.Before(expires) {
			return servingauth.ErrCredentials
		}
		return nil
	})
}
func (h *servingHTTP) deleteToken(w http.ResponseWriter, r *http.Request) error {
	attrs, err := h.anyTokenScope(r.Context())
	if err != nil {
		return err
	}
	if handled, err := h.forwardTokenDelete(w, r, attrs, r.PathValue("id")); handled {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		_, err := h.opts.Runtime.deleteTokenCurrent(ctx, tx, deps, proof, attrs, r.PathValue("id"))
		return err
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	w.WriteHeader(204)
	return nil
}

// deleteTokenCurrent is the body of DELETE /auth/tokens/{id} inside one
// current command: an own token, or someone else's with tokens.delete;
// otherwise the route's 404. staged reports whether the delete was attempted.
func (s *servingRuntime) deleteTokenCurrent(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, attrs rbac.Attributes, id string) (bool, error) {
	p, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, s.opts.Now)
	if err != nil {
		return false, err
	}
	var t internalv1.APIToken
	if err = tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &t); err != nil {
		return false, err
	}
	if !servingHTTPOwnsToken(p, &t) {
		if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, servingHTTPCluster("tokens", "delete"), s.opts.Now); err != nil {
			return false, servingHTTPMissing()
		}
	}
	return true, tx.Delete(ctx, store.KeyOf(&t), store.DeleteOptions{Manager: store.ManagerSystem, Force: true})
}
