package persistence

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Recover stages only the exact recovery proposal and receipt captured from the
// original complete quarantined owner. The operator adapter must independently
// authorize explicit trust, audit it in this transaction, and retain the real
// original driver attempt. This function grants no operator or writer authority.
func Recover(ctx context.Context, tx *sqlite.CommandTx, original Original, proposed trustanchors.State, proof trustanchors.RecoveryObservation) (Staged, error) {
	current, rows, err := transitionBase(ctx, tx, original)
	if err != nil {
		return Staged{}, err
	}
	before, err := current.State()
	if err != nil {
		return Staged{}, ErrPhase
	}
	if err := proof.CheckRecovery(before, proposed, original.clock.Sample()); err != nil {
		return Staged{}, err
	}
	next, err := accepted.ProposeRecovery(current, proposed, proposed.Inspect().Checkpoint.Wall, proof.ValidUntil())
	if err != nil {
		return Staged{}, err
	}
	if err := tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := original.check(ctx); err != nil {
			return err
		}
		return proof.CheckLifetime(original.clock.Sample())
	}); err != nil {
		return Staged{}, err
	}
	return stageRows(ctx, tx, rows, next)
}
