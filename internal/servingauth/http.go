package servingauth

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Objects returns detached captured IAM objects for trusted HTTP projections.
// Callers must project safe public fields, never serialize these private objects.
func (v *View) Objects(kind store.Kind) ([]store.Object, error) {
	if v == nil || v.cut == nil || v.released.Load() {
		return nil, ErrInvalid
	}
	if !slices.Contains(requiredKinds, string(kind)) || kind == "Kudurru" {
		return nil, ErrUnsupported
	}
	out := []store.Object{}
	for k, o := range v.cut.objects {
		if k.Kind == kind {
			if len(out) >= MaxObjects {
				return nil, ErrBounds
			}
			out = append(out, o.DeepCopyObject().(store.Object))
		}
	}
	slices.SortFunc(out, func(a, b store.Object) int {
		return strings.Compare(a.GetNamespace()+"/"+a.GetName(), b.GetNamespace()+"/"+b.GetName())
	})
	return out, nil
}

// SessionRecords returns detached enrollments for trusted safe projections.
// Evidence and verifier fields are private and must never enter HTTP responses.
func (v *View) SessionRecords() ([]SessionRecord, []ExternalSessionRecord, error) {
	if v == nil || v.cut == nil || v.released.Load() {
		return nil, nil, ErrInvalid
	}
	if len(v.sessions)+len(v.externalSessions) >= MaxRows {
		return nil, nil, ErrBounds
	}
	local, external := []SessionRecord{}, []ExternalSessionRecord{}
	for _, r := range v.sessions {
		r.Grants = slices.Clone(r.Grants)
		local = append(local, r)
	}
	for _, r := range v.externalSessions {
		r.Grants = slices.Clone(r.Grants)
		external = append(external, r)
	}
	slices.SortFunc(local, func(a, b SessionRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(external, func(a, b ExternalSessionRecord) int { return strings.Compare(a.ID, b.ID) })
	return local, external, nil
}

// Roles projects effective grants from this View only. The caller must first
// verify raw proof against this same View. This result is not mutation authority.
func (v *View) Roles(p auth.Principal, now time.Time) ([]string, error) {
	p, err := v.LoadPrincipal(p, now)
	if err != nil {
		return nil, err
	}
	candidates := []string{}
	if len(p.Scopes) > 0 {
		candidates = append(candidates, p.Scopes...)
	} else {
		if p.BuiltinAdmin {
			candidates = append(candidates, "admin")
		}
		if v.license.RBAC {
			if p.External != nil {
				candidates = append(candidates, p.Grants...)
			} else {
				for _, g := range rbac.GrantsFor(v.bindings, auth.RBACPrincipal(p)) {
					candidates = append(candidates, g.Normalized().String())
				}
				candidates = append(candidates, p.Grants...)
			}
		}
	}
	out := []string{}
	for _, wire := range candidates {
		g, err := rbac.ParseGrant(wire)
		if err != nil {
			continue
		}
		allowed, seen := true, false
		for resource, verbs := range v.permissions.Resources {
			for _, verb := range verbs {
				if !v.permissions.Grants(g.Role, resource, verb) {
					continue
				}
				seen = true
				d, err := v.Authorize(p, rbac.Attributes{Resource: resource, Verb: verb, Scope: g.Normalized().Scope}, now)
				allowed = allowed && err == nil && d.Allowed
			}
		}
		if allowed && seen {
			out = append(out, g.Normalized().String())
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// AuthenticateCurrentWithClock verifies current raw proof without granting any
// permission and registers its original validity through this Command's final
// commit/no-op. Must-change-password self-service remains possible; factors are
// verified/consumed once by the existing actual transaction path.
func AuthenticateCurrentWithClock(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof Credential, clock func() time.Time) (auth.Principal, error) {
	if clock == nil {
		clock = time.Now
	}
	v, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return auth.Principal{}, err
	}
	defer v.Release()
	p, validity, err := authenticateCurrentWithValidity(ctx, tx, v, proof, clock().UTC())
	if err != nil {
		return auth.Principal{}, err
	}
	if err = tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return validity.validAt(clock().UTC())
	}); err != nil {
		return auth.Principal{}, err
	}
	return detachedPrincipal(p), nil
}
