package servingauth

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ReauthorizeCurrent accepts an actual command transaction and raw credential
// proof. It rereads ALL current IAM objects/security in that transaction, even
// for dedup replay. Neither an old View nor its Principal is an input. Root
// retains command admission, dependency freshness and the term commit fence.
func ReauthorizeCurrent(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof Credential, attrs rbac.Attributes, now time.Time) (auth.Principal, rbac.Decision, error) {
	view, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return auth.Principal{}, rbac.Decision{}, err
	}
	defer view.Release()
	p, err := authenticateInCommand(ctx, tx, view, proof, now)
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
	return detachedPrincipal(p), decision, nil
}

// AuthenticateCurrent reverifies raw proof in an actual current command
// transaction without granting mutation rights. It permits password-change
// enrollment for MustChangePassword accounts; TOTP never has a password bypass.
// Root retains login policy, issuance/replay admission and the commit fence.
func AuthenticateCurrent(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof Credential, now time.Time) (auth.Principal, error) {
	view, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return auth.Principal{}, err
	}
	defer view.Release()
	return authenticateInCommand(ctx, tx, view, proof, now)
}

// ResolveSessionCurrent resolves password login, licensed normalized binding
// grants and canonical settings from the SAME current transaction view. Token
// to cookie conversion is unavailable: this session schema carries no token
// scope restriction, so accepting a token here would broaden its authority.
// Root owns login policy, TTL, randomness, durable enrollment and commit fencing.
func ResolveSessionCurrent(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof Credential, now time.Time) (SessionDecision, error) {
	if proof.Kind != Password {
		return SessionDecision{}, ErrUnsupported
	}
	view, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return SessionDecision{}, err
	}
	defer view.Release()
	p, err := authenticateInCommand(ctx, tx, view, proof, now)
	if err != nil {
		return SessionDecision{}, err
	}
	grants := []string{}
	if view.license.RBAC {
		for _, grant := range rbac.GrantsFor(view.bindings, auth.RBACPrincipal(p)) {
			grants = append(grants, grant.Normalized().String())
		}
		slices.Sort(grants)
	}
	settings, err := view.Settings()
	if err != nil {
		return SessionDecision{}, err
	}
	return SessionDecision{Principal: detachedPrincipal(p), Grants: grants, Settings: settings}, nil
}

func authenticateInCommand(ctx context.Context, tx *sqlite.CommandTx, view *View, proof Credential, now time.Time) (auth.Principal, error) {
	if proof.Kind != Password {
		if proof.TOTPCode != "" {
			return auth.Principal{}, ErrCredentials
		}
		return view.Authenticate(ctx, proof, now)
	}
	if err := ctx.Err(); err != nil {
		return auth.Principal{}, err
	}
	if now.IsZero() {
		return auth.Principal{}, ErrCredentials
	}
	user, err := view.passwordAccount(ctx, proof.Username, proof.Value)
	if err != nil {
		return auth.Principal{}, err
	}
	p, err := view.passwordPrincipal(user, now)
	if err != nil {
		return auth.Principal{}, err
	}
	if user.Spec.TOTPSecret == "" {
		if proof.TOTPCode != "" {
			return auth.Principal{}, ErrCredentials
		}
		return p, nil
	}
	if proof.TOTPCode == "" {
		return auth.Principal{}, ErrCredentials
	}
	a := totp.Account{Name: user.Name, UID: string(user.UID)}
	seed := view.factors[a]
	step, err := totp.Match(seed, proof.TOTPCode, now)
	if err != nil {
		return auth.Principal{}, ErrCredentials
	}
	state := view.cut.security
	state.TOTP = slices.Clone(state.TOTP)
	if state.Epoch == math.MaxUint64 {
		return auth.Principal{}, ErrBounds
	}
	found := false
	for i, row := range state.TOTP {
		if row.Account != a {
			continue
		}
		next, err := totp.Advance(row, seed, step)
		if err != nil {
			return auth.Principal{}, ErrCredentials
		}
		state.TOTP[i] = next
		found = true
		break
	}
	if !found {
		return auth.Principal{}, ErrManifest
	}
	state.Epoch++
	rows, err := EncodeSecurity(state)
	if err != nil {
		return auth.Principal{}, err
	}
	for _, row := range rows {
		if err = tx.PutSection(ctx, "security", row); err != nil {
			return auth.Principal{}, err
		}
	}
	return p, nil
}

func prepareCurrent(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies) (*View, error) {
	if tx == nil {
		return nil, ErrInvalid
	}
	cut, err := captureCurrent(ctx, tx)
	if err != nil {
		return nil, err
	}
	return Prepare(ctx, cut, deps)
}
func captureCurrent(ctx context.Context, reader CurrentReader) (*Cut, error) {
	rows, err := reader.ListSections(ctx, "security")
	if err != nil {
		return nil, ErrInvalid
	}
	security, err := DecodeSecurity(rows)
	if err != nil {
		return nil, err
	}
	cut := &Cut{objects: map[store.Key]store.Object{}, security: security}
	count := 0
	bytes := 0
	for _, kind := range requiredKinds {
		// One extra object detects overflow without an unbounded kind read.
		// Keep a positive limit even after the shared object budget is spent.
		list, err := reader.List(ctx, store.Kind(kind), store.ListOptions{Limit: MaxObjects - count + 1})
		if err != nil {
			return nil, ErrInvalid
		}
		objects := list.Items
		if list.Continue != "" {
			return nil, ErrBounds
		}
		count += len(objects)
		if count > MaxObjects {
			return nil, ErrBounds
		}
		for _, obj := range objects {
			o := obj
			raw, err := json.Marshal(o)
			if err != nil {
				return nil, ErrInvalid
			}
			bytes += len(raw)
			if len(raw) > MaxObjectBytes || bytes > MaxImageBytes {
				return nil, ErrBounds
			}
			if err = addObject(cut, o); err != nil {
				return nil, err
			}
		}
	}
	if err = validateSettings(cut); err != nil {
		return nil, err
	}
	return cut, nil
}
