package security

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalPasswordChange owns one prepared hash and the original request lifetime.
// Copies share consumption. Preparation performs no authentication or mutation.
type LocalPasswordChange struct{ state *localPasswordChange }

type localPasswordChange struct {
	original            context.Context
	current, hash, code string
	revokeTokens        bool
	used                atomic.Bool
}

func (LocalPasswordChange) String() string { return "private password change" }

// GoString keeps prepared credentials out of diagnostic formatting.
func (c LocalPasswordChange) GoString() string { return c.String() }

// MarshalJSON omits prepared credentials.
func (LocalPasswordChange) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// MatchesPasswordHash only correlates an acknowledged object with this prepared
// hash. It grants no identity, permission or enrollment authority.
func (c LocalPasswordChange) MatchesPasswordHash(hash string) bool {
	return c.state != nil && c.state.hash == hash
}

// PrepareLocalPasswordChange hashes the new password outside transaction and
// dependency locks, under the original finite request bound of at most 30s.
func PrepareLocalPasswordChange(ctx context.Context, hasher *users.Hasher, current, next, code string, revokeTokens bool) (LocalPasswordChange, error) {
	if ctx == nil || hasher == nil || current == "" || len(current) > users.MaxPasswordLen || current == next || users.CheckNewPassword(next) != nil {
		return LocalPasswordChange{}, servingauth.ErrInvalid
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > loginlimit.Lifetime {
		return LocalPasswordChange{}, servingauth.ErrInvalid
	}
	hash, err := hasher.Hash(ctx, next)
	if err != nil {
		return LocalPasswordChange{}, err
	}
	if err := ctx.Err(); err != nil {
		return LocalPasswordChange{}, err
	}
	return LocalPasswordChange{state: &localPasswordChange{original: ctx, current: current, hash: hash, code: code, revokeTokens: revokeTokens}}, nil
}

// PasswordChangeResult is uncommitted until its enclosing command succeeds.
// A token caller receives no enrollment. The root must apply its final issuance
// clock and publish the exact result before returning a cookie or account view.
type PasswordChangeResult struct {
	Principal  auth.Principal
	Enrollment Enrollment
	IssuedAt   time.Time
}

// ChangeLocalPasswordWithAdmission verifies the original caller and supplied
// password/factor before any password mutation. Refused may commit only counters;
// every error after proof admission is a command error, including credential
// errors from later enrollment. The original caller's UID is correlation only.
func ChangeLocalPasswordWithAdmission(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, caller servingauth.Credential, original auth.Principal, prepared LocalPasswordChange, ip string, clock func() time.Time) (PasswordChangeResult, error, error) {
	return changeLocalPassword(ctx, tx, deps, caller, original, prepared, ip, clock, nil)
}

// PrepareForwardedPasswordChange is the primary's half of a password change
// that an enrolled follower forwarded (D368). The follower verified the new
// password's length rules and hashed it under the request's bound; the primary
// receives only that hash, which must be a well-formed Argon2id PHC string,
// together with the current password and factor code that it verifies itself.
// ctx is the member command's context: its finite deadline is the original
// request bound, checked again at commit.
func PrepareForwardedPasswordChange(ctx context.Context, current, hash, code string, revokeTokens bool) (LocalPasswordChange, error) {
	if ctx == nil || current == "" || len(current) > users.MaxPasswordLen || !users.ValidPasswordHash(hash) {
		return LocalPasswordChange{}, servingauth.ErrInvalid
	}
	if _, finite := ctx.Deadline(); !finite {
		return LocalPasswordChange{}, servingauth.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return LocalPasswordChange{}, err
	}
	return LocalPasswordChange{state: &localPasswordChange{original: ctx, current: current, hash: hash, code: code, revokeTokens: revokeTokens}}, nil
}

// ChangeForwardedLocalPasswordWithAdmission is ChangeLocalPasswordWithAdmission
// for a change that an enrolled follower forwarded: the same caller, shared
// counter, password, factor and account checks in the primary's transaction.
// A cookie caller's new session uses the follower-prepared ID and CSRF hash
// (session), as a forwarded login does; a token caller sends no session. The
// managed_by refusal, which precedes every mutation, is returned as refused so
// that it can be retained and relayed; every error after the first mutation is
// a command error, as on the primary's own route.
func ChangeForwardedLocalPasswordWithAdmission(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, caller servingauth.Credential, original auth.Principal, prepared LocalPasswordChange, ip string, session ForwardedSession, clock func() time.Time) (PasswordChangeResult, error, error) {
	if (caller.Kind == servingauth.Cookie) != session.valid() || caller.Kind != servingauth.Cookie && session != (ForwardedSession{}) {
		return PasswordChangeResult{}, nil, servingauth.ErrInvalid
	}
	return changeLocalPassword(ctx, tx, deps, caller, original, prepared, ip, clock, &session)
}

func changeLocalPassword(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, caller servingauth.Credential, original auth.Principal, prepared LocalPasswordChange, ip string, clock func() time.Time, forwarded *ForwardedSession) (PasswordChangeResult, error, error) {
	fail := func(err error) (PasswordChangeResult, error, error) { return PasswordChangeResult{}, nil, err }
	c := prepared.state
	if ctx == nil || tx == nil || c == nil || !c.used.CompareAndSwap(false, true) || caller.Kind != servingauth.Cookie && caller.Kind != servingauth.APIToken {
		return fail(servingauth.ErrInvalid)
	}
	if err := c.original.Err(); err != nil {
		return fail(err)
	}
	if err := tx.RequireCommitCheck(func(context.Context) error { return c.original.Err() }); err != nil {
		return fail(err)
	}
	clock = factorClock(clock)
	var authenticatedAt time.Time
	p, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, caller, func() time.Time {
		now := clock().UTC()
		if authenticatedAt.IsZero() {
			authenticatedAt = now
		}
		return now
	})
	if err != nil {
		return fail(err)
	}
	if p.Kind != auth.KindUser || p.Provider != "local" || p.AccountUID != original.AccountUID || p.ID != original.ID || p.Name != original.Name {
		return fail(servingauth.ErrCredentials)
	}
	var user internalv1.LocalUser
	if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: p.Name}, &user); err != nil {
		return fail(err)
	}
	if string(user.UID) != p.AccountUID || user.Spec.Disabled {
		return fail(servingauth.ErrCredentials)
	}
	manager := store.ManagedBy(&user)
	bootstrap, bootstrapOwner := strings.CutPrefix(manager, store.ManagerBootstrap+":")
	bootstrapAdmin := user.Spec.BuiltinAdmin && bootstrapOwner && bootstrap != ""
	if manager != store.ManagerSystem && manager != store.ManagerAPI && !bootstrapAdmin {
		managed := &auth.Error{Status: http.StatusConflict, Key: "managed_by", Message: "this account's password is managed outside self-service"}
		if forwarded != nil {
			// Nothing is staged yet: a forwarded change retains this refusal.
			return PasswordChangeResult{}, managed, nil
		}
		return fail(managed)
	}
	proof := servingauth.Credential{Kind: servingauth.Password, Username: p.Name, Value: c.current, TOTPCode: c.code}
	var decision servingauth.SessionDecision
	refused, err := withPasswordAdmission(ctx, tx, proof, ip, clock, func(proof servingauth.Credential, clock func() time.Time) error {
		observed := clock().UTC()
		var err error
		decision, err = servingauth.ResolveSessionCurrent(ctx, tx, deps, proof, observed)
		if err != nil {
			return err
		}
		if decision.Principal.AccountUID != p.AccountUID || decision.Principal.ID != p.ID {
			return servingauth.ErrCredentials
		}
		return requireFactorClock(tx, factorValidity{observedAt: observed}, clock)
	})
	if refused != nil || err != nil {
		return PasswordChangeResult{}, refused, err
	}
	// All errors below are fatal to the entire command. The successful result
	// is already staged, so failure also rolls it and any TOTP consumption back.
	floor := authenticatedAt
	user.Spec.PasswordHash, user.Spec.MustChangePassword = c.hash, false
	out, err := Apply(ctx, tx, &user, store.ApplyOptions{Manager: manager, ExpectedResourceVersion: user.ResourceVersion}, floor)
	if err != nil {
		return fail(err)
	}
	current := out.(*internalv1.LocalUser)
	changed := metav1.NewTime(floor)
	current.Status.PasswordChangedAt = &changed
	if err := tx.UpdateStatus(ctx, current); err != nil {
		return fail(err)
	}
	if c.revokeTokens {
		list, err := tx.List(ctx, "APIToken", store.ListOptions{Limit: servingauth.MaxObjects + 1})
		if err != nil {
			return fail(err)
		}
		if len(list.Items) > servingauth.MaxObjects || list.Continue != "" {
			return fail(servingauth.ErrBounds)
		}
		for _, object := range list.Items {
			token := object.(*internalv1.APIToken)
			if LocalUserOwnsToken(token, p.Name, p.AccountUID) {
				if err := tx.Delete(ctx, store.KeyOf(token), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					if store.IsManagedBy(err) {
						return fail(&auth.Error{Status: http.StatusConflict, Key: "managed_by", Message: "a personal token is managed outside self-service"})
					}
					return fail(err)
				}
			}
		}
	}
	p.MustChangePassword = false
	result := PasswordChangeResult{Principal: p}
	if caller.Kind == servingauth.Cookie {
		result.IssuedAt = clock().UTC()
		if !result.IssuedAt.After(floor) {
			return fail(servingauth.ErrCredentials)
		}
		// decision was obtained by actual password/TOTP verification earlier in
		// this same transaction. Enroll after our own password change without
		// consuming the already verified factor again or converting token scopes.
		if forwarded != nil {
			// The follower keeps the CSRF secret and seals the cookie itself.
			result.Enrollment, err = issueLocalDecisionWith(ctx, tx, decision, result.IssuedAt, forwarded.ID, forwarded.CSRFHash)
		} else {
			result.Enrollment, err = issueLocalDecision(ctx, tx, decision, result.IssuedAt)
		}
		if err != nil {
			return fail(err)
		}
	}
	return result, nil, nil
}

// LocalUserOwnsToken matches both provider spellings accepted by the local token
// verifier. Account floors alone cannot revoke personal tokens.
func LocalUserOwnsToken(token *internalv1.APIToken, name, uid string) bool {
	owner := token.Spec.Owner
	return owner.Kind == internalv1.OwnerUser && owner.Namespace == "" && owner.Name == name && owner.UID == uid &&
		owner.ID == "local:"+name && (owner.Provider == "local" || owner.Provider == "")
}
