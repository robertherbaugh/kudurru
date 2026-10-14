package rbac

import (
	"context"
	"errors"
	"sort"
)

// Decision paths recorded in audit rows (§8.5).
const (
	PathBinding = "binding"
	PathSAR     = "sar"
	PathLocal   = "local"
	PathNone    = "none"
)

// Engine is the Authorize step of the §8.8 middleware chain:
//
//  1. API token scopes (Principal.Restrict) narrow everything below;
//  2. the implicit admin (Principal.BuiltinAdmin: unix-socket peer,
//     first-boot admin, bootstrap service account) is allowed, Path local,
//     and so is whatever Principal.Implicit grants (peer nodes);
//  3. otherwise, while RBAC is licensed (EngineOptions.Licensed), the Chain
//     decides, first Allowed wins: SubjectAccessReview for Kubernetes
//     identities (Path sar), then kudurru bindings (Path binding). Either
//     path suffices (D22); Decision.Path names it. Without the license
//     there is no step 3: the built-in administrator is the only role
//     (docs/architecture/licensing.md).
//
// Steps 1 and 2 are everything an unlicensed node needs; the binding source
// and the SubjectAccessReview authorizer are in ee/rbac.
type Engine struct {
	perms    *Permissions
	chain    Chain
	licensed func() bool
}

// EngineOptions configures NewEngine.
type EngineOptions struct {
	// Permissions is the permissions table. Required.
	Permissions *Permissions
	// Bindings is the live binding authorizer (ee/rbac.BindingSource).
	// Required.
	Bindings Authorizer
	// SAR is the SubjectAccessReview authorizer (kube driver only).
	SAR Authorizer
	// Licensed reports whether role bindings, scoped roles and Kubernetes
	// RBAC delegation are licensed (license.FeatureRBAC). nil means they
	// are (tests).
	Licensed func() bool
}

// NewEngine builds the authorizer chain.
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.Permissions == nil {
		return nil, errors.New("rbac: engine needs the permissions table")
	}
	if opts.Bindings == nil {
		return nil, errors.New("rbac: engine needs a binding source")
	}
	var chain Chain
	if opts.SAR != nil {
		chain = append(chain, opts.SAR)
	}
	chain = append(chain, opts.Bindings)
	return &Engine{perms: opts.Permissions, chain: chain, licensed: opts.Licensed}, nil
}

// Licensed reports whether role-based access control beyond the built-in
// administrator is in force.
func (e *Engine) Licensed() bool { return e.licensed == nil || e.licensed() }

// Permissions returns the permissions table.
func (e *Engine) Permissions() *Permissions { return e.perms }

// Authorize implements Authorizer.
func (e *Engine) Authorize(ctx context.Context, p Principal, a Attributes) (Decision, error) {
	if len(p.Restrict) > 0 && !e.grantsAllow(p.Restrict, a) {
		missing := a
		return Decision{Allowed: false, Path: PathNone, Missing: &missing, Reason: "outside the API token's scopes"}, nil
	}
	if p.BuiltinAdmin {
		return Decision{Allowed: true, Path: PathLocal, Reason: "builtin admin"}, nil
	}
	if len(p.Implicit) > 0 && e.grantsAllow(p.Implicit, a) {
		return Decision{Allowed: true, Path: PathLocal, Reason: "implicit grant"}, nil
	}
	if !e.Licensed() {
		missing := a
		return Decision{Allowed: false, Path: PathNone, Missing: &missing, Reason: "roles other than the built-in administrator need a license key"}, nil
	}
	return e.chain.Authorize(ctx, p, a)
}

func (e *Engine) grantsAllow(grants []Grant, a Attributes) bool {
	for _, g := range grants {
		if g.Scope.Covers(a.Scope) && e.perms.Grants(g.Role, a.Resource, a.Verb) {
			return true
		}
	}
	return false
}

// PermissionMatrix expands grants into "<resource>.<verb>" lists keyed by
// scope ("cluster", "zone:<name>"), the permissions object of
// GET /api/v1/auth/me. A builtin admin gets "*.*"-equivalent cluster
// permissions.
func (p *Permissions) PermissionMatrix(grants []Grant, builtinAdmin bool) map[string][]string {
	if builtinAdmin {
		grants = append([]Grant{{Role: "admin", Scope: Scope{Kind: ScopeCluster}}}, grants...)
	}
	sets := map[string]map[string]struct{}{}
	for _, g := range grants {
		g = g.Normalized()
		key := string(ScopeCluster)
		if g.Scope.Kind != ScopeCluster {
			key = string(g.Scope.Kind) + ":" + g.Scope.Name
		}
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
