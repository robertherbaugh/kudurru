package servingauth

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
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
	p, err := view.Authenticate(ctx, proof, now)
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
	return view.Authenticate(ctx, proof, now)
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
	p, err := view.Authenticate(ctx, proof, now)
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
		list, err := reader.List(ctx, store.Kind(kind), store.ListOptions{})
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
