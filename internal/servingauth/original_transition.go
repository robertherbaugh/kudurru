package servingauth

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// AuthorizeOriginalTransition verifies raw proof against this exact immutable
// source. It grants no current mutation authority by itself. The caller must
// exclude publication and dependency replacement, require the concrete Store
// transition to CAS this complete source, and retain the verified lifetime
// through its final commit check. Ordinary commands still reauthorize in their
// CommandTx. Password/TOTP cannot be consumed by this read-only operation.
func (v *View) AuthorizeOriginalTransition(ctx context.Context, proof Credential, attrs rbac.Attributes, clock func() time.Time) (auth.Principal, rbac.Decision, *CredentialLifetime, error) {
	if ctx == nil || v == nil || v.released.Load() {
		return auth.Principal{}, rbac.Decision{}, nil, ErrInvalid
	}
	if proof.Kind != APIToken && proof.Kind != Cookie && proof.Kind != OIDCJWT {
		return auth.Principal{}, rbac.Decision{}, nil, ErrUnsupported
	}
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC()
	principal, validity, err := authenticateCurrentWithValidity(ctx, nil, v, proof, now)
	if err != nil {
		return auth.Principal{}, rbac.Decision{}, nil, err
	}
	decision, err := v.Authorize(principal, attrs, now)
	if err != nil {
		return auth.Principal{}, decision, nil, err
	}
	if !decision.Allowed {
		return auth.Principal{}, decision, nil, ErrDenied
	}
	lifetime := &CredentialLifetime{validity: validity, clock: clock}
	if err := lifetime.Check(ctx); err != nil {
		return auth.Principal{}, decision, nil, err
	}
	return detachedPrincipal(principal), decision, lifetime, nil
}

// Check inspects only the original verified time evidence and the caller's
// original finite lifetime. It never refreshes a View, raw proof or authority.
func (p *CredentialLifetime) Check(ctx context.Context) error {
	if p == nil || p.clock == nil || ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, finite := ctx.Deadline()
	if !finite {
		return ErrInvalid
	}
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return p.validity.validAt(p.clock().UTC())
}
