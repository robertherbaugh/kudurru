package oidc

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// RoleResolver turns a freshly mapped principal (username and prefixed IdP
// groups) into the grants sealed in Session.RolesSnapshot.
type RoleResolver interface {
	ResolveRoles(ctx context.Context, p auth.Principal) ([]auth.Grant, error)
}

// RoleResolverFunc adapts a function to RoleResolver.
type RoleResolverFunc func(ctx context.Context, p auth.Principal) ([]auth.Grant, error)

// ResolveRoles implements RoleResolver.
func (f RoleResolverFunc) ResolveRoles(ctx context.Context, p auth.Principal) ([]auth.Grant, error) {
	return f(ctx, p)
}

// BindingResolver resolves grants from the current rbac bindings (config
// file and RoleBinding objects, read from the store snapshot) with the one
// subject-matching rule of kudurru, rbac.SubjectsMatch: a User subject
// matches the mapped username, a Group subject matches one of the prefixed
// groups, and ServiceAccount subjects never match an OIDC user (who has no
// service account).
type BindingResolver struct {
	// Bindings returns the current bindings; it is called once per login.
	Bindings func() []rbac.Binding
}

// ResolveRoles implements RoleResolver. The result is de-duplicated and in
// binding order.
func (b BindingResolver) ResolveRoles(_ context.Context, p auth.Principal) ([]auth.Grant, error) {
	grants := rbac.GrantsFor(b.Bindings(), rbac.Principal{Name: p.Name, Groups: p.Groups})
	if len(grants) == 0 {
		return nil, nil
	}
	out := make([]auth.Grant, 0, len(grants))
	for _, g := range grants {
		ag := auth.Grant{Role: g.Role, ScopeKind: string(g.Scope.Kind), ScopeName: g.Scope.Name}
		if ag.IsCluster() {
			ag.ScopeKind, ag.ScopeName = auth.ScopeCluster, ""
		}
		out = append(out, ag)
	}
	return out, nil
}
