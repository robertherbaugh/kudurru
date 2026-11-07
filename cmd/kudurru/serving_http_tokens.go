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
func (h *servingHTTP) createToken(w http.ResponseWriter, r *http.Request) error {
	attrs, err := h.anyTokenScope(r.Context())
	if err != nil {
		return err
	}
	var req api.CreateTokenRequest
	if err = servingHTTPDecode(w, r, &req); err != nil {
		return err
	}
	if len(req.Scopes) > servingauth.MaxGrants || len(req.Description) > 4096 {
		return servingHTTPInvalid("token description or scope count exceeds limits")
	}
	for _, wire := range req.Scopes {
		g, err := rbac.ParseGrant(wire)
		if err != nil || !internalv1.ValidGrant(wire) || h.permissions.Roles[g.Role] == nil {
			return servingHTTPInvalid("invalid token scope")
		}
	}
	life := time.Duration(0)
	if req.ExpiresIn != "" {
		life, err = time.ParseDuration(req.ExpiresIn)
		if err != nil || life <= 0 {
			return servingHTTPInvalid("expiresIn must be a positive Go duration")
		}
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	var created *internalv1.APIToken
	var wire string
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		p, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, h.now)
		if err != nil {
			return err
		}
		if p.AccountUID == "" {
			return servingauth.ErrDenied
		}
		owner := internalv1.TokenOwner{ID: p.ID, UID: p.AccountUID, Provider: p.Provider}
		switch p.Kind {
		case auth.KindUser:
			owner.Kind, owner.Name = internalv1.OwnerUser, p.Name
		case auth.KindServiceAccount:
			owner.Kind = internalv1.OwnerServiceAccount
			owner.Namespace, owner.Name, _ = strings.Cut(p.ServiceAccount, "/")
		default:
			return servingHTTPInvalid("this principal cannot own API tokens")
		}
		scopes := slices.Clone(req.Scopes)
		if len(p.Scopes) > 0 {
			if len(scopes) == 0 {
				scopes = slices.Clone(p.Scopes)
			}
			for _, s := range scopes {
				if !slices.Contains(p.Scopes, s) {
					return servingauth.ErrDenied
				}
			}
		}
		slices.Sort(scopes)
		scopes = slices.Compact(scopes)
		var settings publicv1.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		doc, err := servingSettingsDocument(&settings)
		if err != nil {
			return err
		}
		cfg, _, err := config.ApplyClusterDocument(h.opts.Runtime.opts.Config, doc)
		if err != nil {
			return err
		}
		maxLife := cfg.Auth.Tokens.MaxLifetime.Std()
		duration := life
		if maxLife > 0 {
			if duration > maxLife {
				return servingHTTPInvalid("expiresIn exceeds auth.tokens.maxLifetime")
			}
			if duration == 0 {
				duration = maxLife
			}
		}
		now := h.now()
		var expires time.Time
		if duration > 0 {
			expires = now.Add(duration).Truncate(time.Second)
		}
		if p.TokenID != "" {
			var parent internalv1.APIToken
			if err = tx.Get(ctx, store.Key{Kind: "APIToken", Name: p.TokenID}, &parent); err != nil {
				return err
			}
			if !servingHTTPOwnsToken(p, &parent) {
				return servingauth.ErrCredentials
			}
			if parent.Spec.ExpiresAt != nil && (expires.IsZero() || parent.Spec.ExpiresAt.Time.Before(expires)) {
				expires = parent.Spec.ExpiresAt.Time
			}
		}
		if !expires.IsZero() && !expires.After(now) {
			return servingauth.ErrCredentials
		}
		var id string
		wire, id = tokens.Generate()
		t := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(now)}, Spec: internalv1.APITokenSpec{Owner: owner, Description: req.Description, SecretHash: tokens.Hash(wire), Scopes: scopes}}
		if p.External != nil {
			t.Spec.Grants = slices.Clone(p.Grants)
		}
		if !expires.IsZero() {
			e := metav1.NewTime(expires)
			t.Spec.ExpiresAt = &e
		}
		if err = t.Validate(); err != nil {
			return servingHTTPInvalid("invalid token policy")
		}
		out, err := tx.Apply(ctx, t, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		if err != nil {
			return err
		}
		created = out.(*internalv1.APIToken)
		// SQLite metadata has whole-second precision. A newly issued token
		// must authenticate from its persisted representation, including every
		// current floor; never return a token born revoked or backdate it.
		issued, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: wire}, h.now)
		if err != nil {
			return err
		}
		if issued.AccountUID != p.AccountUID || issued.ID != p.ID {
			return servingauth.ErrCredentials
		}
		return tx.RequireCommitCheck(func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			at := h.now()
			if at.Before(now) || !expires.IsZero() && !at.Before(expires) {
				return servingauth.ErrCredentials
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	return servingHTTPJSON(w, 201, api.CreatedToken{TokenInfo: servingHTTPTokenInfo(created), Token: wire})
}
func (h *servingHTTP) deleteToken(w http.ResponseWriter, r *http.Request) error {
	attrs, err := h.anyTokenScope(r.Context())
	if err != nil {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		p, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, h.now)
		if err != nil {
			return err
		}
		var t internalv1.APIToken
		if err = tx.Get(ctx, store.Key{Kind: "APIToken", Name: r.PathValue("id")}, &t); err != nil {
			return err
		}
		if !servingHTTPOwnsToken(p, &t) {
			if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, servingHTTPCluster("tokens", "delete"), h.now); err != nil {
				return servingHTTPMissing()
			}
		}
		return tx.Delete(ctx, store.KeyOf(&t), store.DeleteOptions{Manager: store.ManagerSystem, Force: true})
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	w.WriteHeader(204)
	return nil
}
