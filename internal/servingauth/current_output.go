package servingauth

import (
	"context"
	"log/slog"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// CredentialLifetime retains only the original verifier's immutable time
// evidence and trusted clock. It contains no credential, View or transaction.
// It grants no permission, commit receipt, current-state or output authority.
// The caller must retain its original operation and dependency exclusions, and
// separately obtain the concrete Store output scope after actual completion.
type CredentialLifetime struct {
	validity credentialValidity
	clock    func() time.Time
}

func (*CredentialLifetime) String() string { return "private credential lifetime" }

// GoString redacts verifier evidence in diagnostics.
func (p *CredentialLifetime) GoString() string { return p.String() }

// LogValue keeps structured diagnostics detached from private proof data.
func (p *CredentialLifetime) LogValue() slog.Value { return slog.StringValue(p.String()) }

// MarshalJSON cannot serialize or restore lifetime evidence.
func (*CredentialLifetime) MarshalJSON() ([]byte, error) { return []byte(`"private"`), nil }

// ReauthorizeCurrentForOutput verifies the original raw token, cookie or JWT in
// the actual command and installs the same final credential-time check as
// ReauthorizeCurrentWithClock. The detached result keeps those verified bounds
// through response emission; a claimed subject or expiration is never input.
// Password/TOTP enrollment does not supply a reusable response credential.
func ReauthorizeCurrentForOutput(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, proof Credential, attrs rbac.Attributes, clock func() time.Time) (auth.Principal, *CredentialLifetime, error) {
	if proof.Kind != APIToken && proof.Kind != Cookie && proof.Kind != OIDCJWT {
		return auth.Principal{}, nil, ErrUnsupported
	}
	if clock == nil {
		clock = time.Now
	}
	p, _, validity, err := reauthorizeCurrentWithValidity(ctx, tx, deps, proof, attrs, clock)
	if err != nil {
		return auth.Principal{}, nil, err
	}
	return p, &CredentialLifetime{validity: validity, clock: clock}, nil
}

// WithOutput shortens an already finite original output context to the verified
// credential expiration (including cookie idle expiry), preserving its values
// and cancellation. It checks time before and after the synchronous callback.
// The callback must install the resulting deadline on actual I/O and join its
// work before returning. Failure after a commit never changes durable completion.
// The transaction's own temporary context is deliberately not retained: it ends
// at commit, whereas the caller's original operation must enclose this scope.
func (p *CredentialLifetime) WithOutput(ctx context.Context, emit func(context.Context) error) error {
	if p == nil || p.clock == nil || ctx == nil || emit == nil {
		return ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	if !finite {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validity.validAt(p.clock().UTC()); err != nil {
		return err
	}
	expires := p.validity.expiresAt
	if p.validity.kind == OIDCJWT {
		expires = p.validity.jwt.Facts().ExpiresAt
	}
	for _, bound := range []time.Time{expires, p.validity.idleExpiresAt} {
		if !bound.IsZero() && bound.Before(deadline) {
			deadline = bound
		}
	}
	output, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := output.Err(); err != nil {
		return err
	}
	if err := emit(output); err != nil {
		return err
	}
	if err := output.Err(); err != nil {
		return err
	}
	return p.validity.validAt(p.clock().UTC())
}
