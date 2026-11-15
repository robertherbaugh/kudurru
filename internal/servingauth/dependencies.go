package servingauth

import (
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// Bound the caller-owned dependency graph before JSON cloning or compiler
// allocations. Six bytes per input byte covers worst-case JSON escaping.
func validateDependencies(d Dependencies) error {
	if d.Permissions == nil {
		return ErrInvalid
	}
	p := d.Permissions
	if len(d.Identity) > 512 || len(d.ExpectedIdentity) > 512 || len(d.StaticBindings) > MaxRows || len(p.Resources) > MaxRows || len(p.Roles) > MaxRows || len(p.Scoped) > MaxRows || len(p.Kubernetes) > MaxRows {
		return ErrBounds
	}
	budget := 0
	consume := func(s string) bool {
		if len(s) > 512 {
			return false
		}
		budget += 6*len(s) + 32
		return budget <= MaxRootBytes
	}
	for _, m := range []map[string][]string{p.Resources, p.Roles} {
		for k, values := range m {
			if !consume(k) || len(values) > MaxRows {
				return ErrBounds
			}
			for _, s := range values {
				if !consume(s) {
					return ErrBounds
				}
			}
		}
	}
	for _, s := range p.Scoped {
		if !consume(s) {
			return ErrBounds
		}
	}
	for role, k := range p.Kubernetes {
		if !consume(role) || !consume(k.ClusterRole) || !consume(k.AggregateTo) {
			return ErrBounds
		}
	}
	for _, b := range d.StaticBindings {
		if !consume(b.Role) || !consume(string(b.Scope.Kind)) || !consume(b.Scope.Name) || len(b.Subjects) == 0 || len(b.Subjects) > MaxRows {
			return ErrBounds
		}
		if (b.Scope.Kind == "" || b.Scope.Kind == rbac.ScopeCluster) && b.Scope.Name != "" {
			return ErrInvalid
		}
		for _, s := range b.Subjects {
			if !consume(string(s.Kind)) || !consume(s.Name) || !consume(s.Namespace) {
				return ErrBounds
			}
			if s.Name == "" {
				return ErrInvalid
			}
			switch s.Kind {
			case rbac.SubjectUser, rbac.SubjectGroup:
			case rbac.SubjectServiceAccount:
				if s.Namespace == "" {
					return ErrInvalid
				}
			default:
				return ErrInvalid
			}
		}
	}
	return nil
}
