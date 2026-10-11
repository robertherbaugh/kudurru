package oidc

import (
	"context"
	"slices"

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
// file and RoleBinding objects, read from the store snapshot). It applies
// the same subject rules as rbac.BindingAuthorizer: a User subject matches
// the mapped username, a Group subject matches one of the prefixed groups,
// and ServiceAccount subjects never match an OIDC user. A test pins the
// two implementations together.
type BindingResolver struct {
	// Bindings returns the current bindings; it is called once per login.
	Bindings func() []rbac.Binding
}

// ResolveRoles implements RoleResolver. The result is de-duplicated and in
// binding order.
func (b BindingResolver) ResolveRoles(_ context.Context, p auth.Principal) ([]auth.Grant, error) {
	var out []auth.Grant
	for _, bd := range b.Bindings() {
		if !subjectsMatch(bd.Subjects, p) {
			continue
		}
		g := auth.Grant{Role: bd.Role, ScopeKind: string(bd.Scope.Kind), ScopeName: bd.Scope.Name}
		if g.IsCluster() {
			g.ScopeKind, g.ScopeName = auth.ScopeCluster, ""
		}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out, nil
}

func subjectsMatch(subjects []rbac.Subject, p auth.Principal) bool {
	for _, s := range subjects {
		switch s.Kind {
		case rbac.SubjectUser:
			if p.Name != "" && s.Name == p.Name {
				return true
			}
		case rbac.SubjectGroup:
			if slices.Contains(p.Groups, s.Name) {
				return true
			}
		}
	}
	return false
}
