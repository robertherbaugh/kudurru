package servingauth

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ReauthorizeCurrentWithClock verifies current raw proof and installs its final
// credential-time check before returning success. The trusted clock defaults to
// time.Now. Root must retain original writer and dependency exclusion through the
// whole Command, and call this before mutations or returning a dedup result.
func ReauthorizeCurrentWithClock(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof Credential, attrs rbac.Attributes, clock func() time.Time) (auth.Principal, rbac.Decision, error) {
	if clock == nil {
		clock = time.Now
	}
	view, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return auth.Principal{}, rbac.Decision{}, err
	}
	defer view.Release()
	now := clock().UTC()
	p, validity, err := authenticateCurrentWithValidity(ctx, tx, view, proof, now)
	if err != nil {
		return auth.Principal{}, rbac.Decision{}, err
	}
	decision, err := view.Authorize(p, attrs, now)
	if err != nil {
		return auth.Principal{}, decision, err
	}
	if !decision.Allowed {
		return auth.Principal{}, decision, ErrDenied
	}
	// The View is released at helper return, but this detached evidence belongs
	// to the original Command through its final checks, when tx is inactive.
	if err := tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return validity.validAt(clock().UTC())
	}); err != nil {
		return auth.Principal{}, decision, err
	}
	return detachedPrincipal(p), decision, nil
}

// credentialValidity has no public constructor or raw credential. It retains
// only verified bounds or original immutable JWT evidence, never a View or tx.
type credentialValidity struct {
	kind                     CredentialKind
	authenticatedAt          time.Time
	expiresAt, idleExpiresAt time.Time
	jwt                      *oidccore.VerifiedToken
}

func (v credentialValidity) validAt(now time.Time) error {
	if v.authenticatedAt.IsZero() || now.IsZero() || now.Before(v.authenticatedAt) {
		return ErrCredentials
	}
	switch v.kind {
	case OIDCJWT:
		if v.jwt.ValidAt(now) != nil {
			return ErrCredentials
		}
	case Cookie:
		if !now.Before(v.expiresAt) || !now.Before(v.idleExpiresAt) {
			return ErrCredentials
		}
	case APIToken:
		if !v.expiresAt.IsZero() && !now.Before(v.expiresAt) {
			return ErrCredentials
		}
	case Password:
		// Password has no absolute expiry. A required TOTP step was already
		// checked and consumed once in the same transaction, never here.
	default:
		return ErrCredentials
	}
	return nil
}

func authenticateCurrentWithValidity(ctx context.Context, tx *sqlite.CommandTx, view *View, proof Credential, now time.Time) (auth.Principal, credentialValidity, error) {
	if err := ctx.Err(); err != nil {
		return auth.Principal{}, credentialValidity{}, err
	}
	if now.IsZero() || proof.Kind != OIDCJWT && proof.ProviderID != "" || proof.Kind != Password && proof.TOTPCode != "" {
		return auth.Principal{}, credentialValidity{}, ErrCredentials
	}
	switch proof.Kind {
	case OIDCJWT:
		return view.verifyJWTWithValidity(ctx, proof, now)
	case APIToken:
		return view.verifyTokenWithValidity(proof.Value, now)
	case Cookie:
		sess, err := view.CookieSession(proof.Value, now)
		if err != nil {
			return auth.Principal{}, credentialValidity{}, err
		}
		return sess.Principal, credentialValidity{kind: Cookie, authenticatedAt: now, expiresAt: sess.ExpiresAt, idleExpiresAt: sess.IdleExpiresAt}, nil
	case Password:
		p, err := authenticateInCommand(ctx, tx, view, proof, now)
		if err != nil {
			return auth.Principal{}, credentialValidity{}, err
		}
		return p, credentialValidity{kind: Password, authenticatedAt: now}, nil
	default:
		return auth.Principal{}, credentialValidity{}, ErrUnsupported
	}
}
