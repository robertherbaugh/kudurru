package servingauth

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// OperationProof retains original verified bounds and one originally allowed
// scope for a bounded operation containing distinct transactions. It is not a
// credential or command authority: every effect must reverify its private raw
// proof in the actual transaction under root's original writer/resource gates.
type OperationProof struct {
	origin     context.Context
	dependency string
	credential Credential
	attrs      rbac.Attributes
	identity   operationIdentity
	validity   credentialValidity
}

type operationIdentity struct {
	kind                                                       auth.PrincipalKind
	id, name, provider, account, serviceAccount, token, method string
	providerUID, origin, session                               string
	issued                                                     time.Time
}

func (*OperationProof) String() string { return "private original operation proof" }

// GoString prevents raw proof and private identity disclosure in diagnostics.
func (p *OperationProof) GoString() string { return p.String() }

// MarshalJSON refuses to serialize operation authority or raw credentials.
func (*OperationProof) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// CaptureOperation verifies the original bearer/cookie/JWT against exactly the
// supplied original view. Password/TOTP and other enrollment proofs are refused.
// Root must retain that request's complete resource lifetime and independently
// perform initial current-command admission before executing any effect.
func CaptureOperation(ctx context.Context, view *View, proof Credential, attrs rbac.Attributes, now time.Time) (*OperationProof, error) {
	if ctx == nil || view == nil || view.released.Load() || now.IsZero() {
		return nil, ErrInvalid
	}
	if proof.Kind != APIToken && proof.Kind != Cookie && proof.Kind != OIDCJWT {
		return nil, ErrUnsupported
	}
	if attrs.Resource == "" || attrs.Verb == "" || len(attrs.Resource) > 64 || len(attrs.Verb) > 32 || len(attrs.Scope.Kind) > 32 || len(attrs.Scope.Name) > 253 || len(attrs.Namespace) > 63 || len(attrs.Name) > 253 {
		return nil, ErrBounds
	}
	principal, validity, err := authenticateCurrentWithValidity(ctx, nil, view, proof, now)
	if err != nil {
		return nil, err
	}
	decision, err := view.Authorize(principal, attrs, now)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, ErrDenied
	}
	return &OperationProof{origin: ctx, dependency: view.DependencyIdentity(), credential: proof, attrs: attrs, identity: operationPrincipalIdentity(principal), validity: validity}, nil
}

func operationPrincipalIdentity(p auth.Principal) operationIdentity {
	i := operationIdentity{kind: p.Kind, id: p.ID, name: p.Name, provider: p.Provider, account: p.AccountUID, serviceAccount: p.ServiceAccount, token: p.TokenID, method: p.AuthMethod, issued: p.CredentialIssuedAt}
	if p.External != nil {
		i.providerUID, i.origin, i.session = p.External.ProviderUID, p.External.Origin, p.External.SessionID
	}
	return i
}

// CheckOriginal performs only detached original time and cancellation checks.
// Success grants no current permission, source identity or writer authority.
func (p *OperationProof) CheckOriginal(ctx context.Context, now time.Time) error {
	if p == nil || p.origin == nil || p.dependency == "" || ctx == nil {
		return ErrInvalid
	}
	if err := p.origin.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.validity.validAt(now.UTC())
}

// ReauthorizeOperationCurrent verifies the same original raw proof and scope
// for one distinct transaction. The current view's aggregate dependency identity
// must equal the original aggregate; an input cursor is not that identity.
// Current changes may reject the operation but cannot extend its original time,
// substitute an account or grant a scope that was not originally authorized.
func ReauthorizeOperationCurrent(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof *OperationProof, clock func() time.Time) (auth.Principal, error) {
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC()
	if err := proof.CheckOriginal(ctx, now); err != nil {
		return auth.Principal{}, err
	}
	view, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return auth.Principal{}, err
	}
	defer view.Release()
	if view.DependencyIdentity() != proof.dependency {
		return auth.Principal{}, ErrDependency
	}
	principal, validity, err := authenticateCurrentWithValidity(ctx, tx, view, proof.credential, now)
	if err != nil {
		return auth.Principal{}, err
	}
	if operationPrincipalIdentity(principal) != proof.identity {
		return auth.Principal{}, ErrCredentials
	}
	decision, err := view.Authorize(principal, proof.attrs, now)
	if err != nil {
		return auth.Principal{}, err
	}
	if !decision.Allowed {
		return auth.Principal{}, ErrDenied
	}
	if err := tx.RequireCommitCheck(func(ctx context.Context) error {
		actual := clock().UTC()
		if err := proof.CheckOriginal(ctx, actual); err != nil {
			return err
		}
		return validity.validAt(actual)
	}); err != nil {
		return auth.Principal{}, err
	}
	return detachedPrincipal(principal), nil
}
