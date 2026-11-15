package security

import (
	"context"
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// factorAdmissionRefusal is issued only by challenge/candidate verification
// before factorAccount can stage old-factor consumption. It is not authentication.
type factorAdmissionRefusal struct{ tx *sqlite.CommandTx }

func (*factorAdmissionRefusal) Error() string { return servingauth.ErrCredentials.Error() }
func (*factorAdmissionRefusal) Unwrap() error { return servingauth.ErrCredentials }
func refuseFactorAdmission(tx *sqlite.CommandTx, err error) error {
	if errors.Is(err, servingauth.ErrCredentials) {
		return &factorAdmissionRefusal{tx: tx}
	}
	return err
}
func isFactorAdmissionRefusal(err error, tx *sqlite.CommandTx) bool {
	var refusal *factorAdmissionRefusal
	return tx != nil && errors.As(err, &refusal) && refusal.tx == tx
}

// BeginTOTPWithAdmission stages shared failure counters and the existing setup
// operation in the caller's original dedicated command. Only refused may be
// returned as a response after commit; commandErr must abort the transaction.
func BeginTOTPWithAdmission(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, ip string, clock func() time.Time) (setup FactorSetup, refused error, commandErr error) {
	refused, commandErr = withPasswordAdmission(ctx, tx, proof, ip, clock, func(proof servingauth.Credential, clock func() time.Time) error {
		var err error
		setup, err = BeginTOTPWithClock(ctx, tx, deps, proof, clock)
		return err
	})
	if refused != nil || commandErr != nil {
		setup = FactorSetup{}
	}
	return
}

// ConfirmTOTPWithAdmission retains the exact original challenge and proof.
// Invalid candidate evidence may count only before any authentication mutation;
// final errors roll back counters, old-factor consumption and activation together.
func ConfirmTOTPWithAdmission(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, challenge, code, ip string, clock func() time.Time) (refused error, commandErr error) {
	return withPasswordAdmission(ctx, tx, proof, ip, clock, func(proof servingauth.Credential, clock func() time.Time) error {
		return ConfirmTOTPWithClock(ctx, tx, deps, proof, challenge, code, clock)
	})
}

// DisableTOTPWithAdmission stages the same shared admission and exact original
// disable operation. A disabled-factor no-op can still change operational counters.
func DisableTOTPWithAdmission(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, ip string, clock func() time.Time) (refused error, commandErr error) {
	return withPasswordAdmission(ctx, tx, proof, ip, clock, func(proof servingauth.Credential, clock func() time.Time) error {
		return DisableTOTPWithClock(ctx, tx, deps, proof, clock)
	})
}
