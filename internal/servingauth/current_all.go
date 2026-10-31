package servingauth

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ReauthorizeCurrentAllWithClock verifies one original raw proof against all
// current scopes before returning authority for a command. Password/TOTP is
// consumed only once. Root retains the original writer and dependency pins;
// the detached final check performs only context and credential-time checks.
func ReauthorizeCurrentAllWithClock(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof Credential, attrs []rbac.Attributes, clock func() time.Time) (auth.Principal, error) {
	if len(attrs) == 0 || len(attrs) > 1001 {
		return auth.Principal{}, ErrBounds
	}
	for _, attr := range attrs {
		if len(attr.Resource) > 64 || len(attr.Verb) > 32 || len(attr.Scope.Kind) > 32 || len(attr.Scope.Name) > 253 || len(attr.Namespace) > 63 || len(attr.Name) > 253 {
			return auth.Principal{}, ErrBounds
		}
		if attr.Resource == "" || attr.Verb == "" {
			return auth.Principal{}, ErrInvalid
		}
	}
	if clock == nil {
		clock = time.Now
	}
	view, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return auth.Principal{}, err
	}
	defer view.Release()
	now := clock().UTC()
	p, validity, err := authenticateCurrentWithValidity(ctx, tx, view, proof, now)
	if err != nil {
		return auth.Principal{}, err
	}
	for _, attr := range attrs {
		if err := ctx.Err(); err != nil {
			return auth.Principal{}, err
		}
		decision, err := view.Authorize(p, attr, now)
		if err != nil {
			return auth.Principal{}, err
		}
		if !decision.Allowed {
			return auth.Principal{}, ErrDenied
		}
	}
	if err := tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return validity.validAt(clock().UTC())
	}); err != nil {
		return auth.Principal{}, err
	}
	return detachedPrincipal(p), nil
}
