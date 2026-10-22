package security

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// BeginBrowser stages a create-only pending attempt with current provider and
// actual-time checks. The caller generates a fresh random ID and authenticated
// preauth payload, retains the original request/writer and all owner pins, and
// seals a cookie only after complete publication. This grants no user authority.
func BeginBrowser(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, attempt servingauth.BrowserAttempt, clock func() time.Time) error {
	if clock == nil {
		clock = time.Now
	}
	attempt.IssuedAt, attempt.ExpiresAt = attempt.IssuedAt.UTC(), attempt.ExpiresAt.UTC()
	observed := clock().UTC()
	if err := servingauth.CheckBrowserProviderCurrent(ctx, tx, deps, attempt, observed); err != nil {
		return err
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return err
	}
	// Check collisions before GC: a caller cannot renew an expired row by
	// reusing its identifier inside the same command.
	for _, old := range s.value.BrowserAttempts {
		if old.ID == attempt.ID {
			return ErrConflict
		}
	}
	if err := s.CollectExpired(observed); err != nil {
		return err
	}
	if len(s.value.BrowserAttempts) >= servingauth.MaxBrowserAttempts {
		return servingauth.ErrBounds
	}
	if err := s.capacity(); err != nil {
		return err
	}
	if err := browserClock(ctx, tx, attempt, observed, clock); err != nil {
		return err
	}
	s.value.BrowserAttempts = append(s.value.BrowserAttempts, attempt)
	s.changed = true
	return s.Commit(ctx)
}

// IssueBrowser consumes the exact pending row and issues an external enrollment
// in the SAME command. Every error must abort the caller's command. A missing or
// consumed attempt cannot revive after restart, promotion or expired-row GC.
// Exchange occurs before this call, outside every owner/transaction guard.
func IssueBrowser(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, attempt servingauth.BrowserAttempt, proof servingauth.ExternalLoginProof, clock func() time.Time) (Enrollment, error) {
	if proof.Mode != oidccore.BrowserIDToken || proof.Provider != attempt.Provider || proof.SpecIdentity != attempt.SpecIdentity || proof.ResourceIdentity != attempt.ResourceIdentity {
		return Enrollment{}, servingauth.ErrCredentials
	}
	if clock == nil {
		clock = time.Now
	}
	observed := clock().UTC()
	if err := servingauth.CheckBrowserProviderCurrent(ctx, tx, deps, attempt, observed); err != nil {
		return Enrollment{}, err
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return Enrollment{}, err
	}
	index := -1
	for i, old := range s.value.BrowserAttempts {
		if old.ID == attempt.ID {
			old.IssuedAt, old.ExpiresAt = old.IssuedAt.UTC(), old.ExpiresAt.UTC()
			attempt.IssuedAt, attempt.ExpiresAt = attempt.IssuedAt.UTC(), attempt.ExpiresAt.UTC()
			if old != attempt {
				return Enrollment{}, servingauth.ErrCredentials
			}
			index = i
			break
		}
	}
	if index < 0 {
		return Enrollment{}, servingauth.ErrCredentials
	}
	if err := browserClock(ctx, tx, attempt, observed, clock); err != nil {
		return Enrollment{}, err
	}
	s.value.BrowserAttempts = append(s.value.BrowserAttempts[:index], s.value.BrowserAttempts[index+1:]...)
	s.changed = true
	if err := s.Commit(ctx); err != nil {
		return Enrollment{}, err
	}
	return IssueExternal(ctx, tx, deps, proof, clock)
}

func browserClock(ctx context.Context, tx *sqlite.CommandTx, attempt servingauth.BrowserAttempt, observed time.Time, clock func() time.Time) error {
	return tx.RequireCommitCheck(func(final context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := final.Err(); err != nil {
			return err
		}
		now := clock().UTC()
		if now.IsZero() || now.Before(observed) || now.Before(attempt.IssuedAt) || !now.Before(attempt.ExpiresAt) {
			return servingauth.ErrCredentials
		}
		return nil
	})
}
