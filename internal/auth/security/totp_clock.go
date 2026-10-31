package security

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// BeginTOTPWithClock verifies the password/existing factor once and retains the
// generated challenge's original time bounds through final commit or no-op.
// The caller still owns original writer/dependency exclusion and HTTP admission.
func BeginTOTPWithClock(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, clock func() time.Time) (FactorSetup, error) {
	if ctx == nil || tx == nil {
		return FactorSetup{}, servingauth.ErrInvalid
	}
	clock = factorClock(clock)
	observed := clock().UTC()
	setup, err := BeginTOTP(ctx, tx, deps, proof, observed)
	if err != nil {
		return FactorSetup{}, err
	}
	validity := factorValidity{observedAt: observed, issuedAt: setup.issuedAt, expiresAt: setup.expiresAt}
	if err := requireFactorClock(tx, validity, clock); err != nil {
		return FactorSetup{}, err
	}
	return setup, nil
}

// ConfirmTOTPWithClock uses the same successful challenge decode for activation
// and final time checks. Neither challenge decryption nor TOTP verification is
// repeated at final commit, and failed final checks roll back all consumption.
func ConfirmTOTPWithClock(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, challenge, code string, clock func() time.Time) error {
	if ctx == nil || tx == nil {
		return servingauth.ErrInvalid
	}
	clock = factorClock(clock)
	observed := clock().UTC()
	c, seed, err := openFactor(deps, challenge, observed)
	if err != nil {
		return err
	}
	if err := confirmTOTP(ctx, tx, deps, proof, c, seed, code, observed); err != nil {
		return err
	}
	return requireFactorClock(tx, factorValidity{observedAt: observed, issuedAt: c.IssuedAt, expiresAt: c.ExpiresAt}, clock)
}

// DisableTOTPWithClock consumes the existing factor once and retains the actual
// verification time through final commit, including already-disabled no-ops.
func DisableTOTPWithClock(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, clock func() time.Time) error {
	if ctx == nil || tx == nil {
		return servingauth.ErrInvalid
	}
	clock = factorClock(clock)
	observed := clock().UTC()
	if err := DisableTOTP(ctx, tx, deps, proof, observed); err != nil {
		return err
	}
	return requireFactorClock(tx, factorValidity{observedAt: observed}, clock)
}

// RecoverTOTPWithClock authorizes one original raw proof against current
// cluster users.write rights and retains its verified expiry through commit or
// no-op. It never rechecks a newly raised target floor after self recovery.
func RecoverTOTPWithClock(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, target totp.Account, clock func() time.Time) error {
	if ctx == nil || tx == nil || totp.Key(target) == "" {
		return servingauth.ErrInvalid
	}
	clock = factorClock(clock)
	var observed time.Time
	first := true
	authClock := func() time.Time {
		now := clock().UTC()
		if first {
			// ReauthorizeCurrentWithClock samples once for verification, then
			// owns this clock through its final detached validity check.
			observed, first = now, false
		}
		return now
	}
	attrs := rbac.Attributes{Resource: "users", Verb: "write", Name: target.Name, Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, authClock); err != nil {
		return err
	}
	return recoverTOTP(ctx, tx, target, observed)
}

func factorClock(clock func() time.Time) func() time.Time {
	if clock == nil {
		return time.Now
	}
	return clock
}

// factorValidity retains detached times only: no seed, raw proof, challenge,
// dependency policy, View or transaction survives into the final predicate.
type factorValidity struct {
	observedAt, issuedAt, expiresAt time.Time
}

func (v factorValidity) validAt(now time.Time) error {
	if v.observedAt.IsZero() || now.IsZero() || now.Before(v.observedAt) {
		return servingauth.ErrCredentials
	}
	if !v.issuedAt.IsZero() || !v.expiresAt.IsZero() {
		if v.issuedAt.IsZero() || !v.expiresAt.Equal(v.issuedAt.Add(factorChallengeLifetime)) || now.Before(v.issuedAt) || !now.Before(v.expiresAt) {
			return servingauth.ErrCredentials
		}
	}
	return nil
}

func requireFactorClock(tx *sqlite.CommandTx, validity factorValidity, clock func() time.Time) error {
	return tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return validity.validAt(clock().UTC())
	})
}
