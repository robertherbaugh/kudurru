package sqlite

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// RequireKindUnchanged preserves an already validated object-kind postimage in
// this SAME command transaction. The owner must validate its complete intended
// rows before registering. This is not source validation, writer authority or a
// replication-section fence. Existing proof/dependency/time checks still apply.
//
// Every later actual mutation of this kind (including status/finalizers, another
// row or ABA) rejects the whole command. A genuinely unchanged Apply adds no
// mutation and may pass. Earlier fences cannot be replaced by registering again.
// The final closure reads only the original private mutation journal and takes
// no locks, performs no I/O and invokes no CommandTx methods.
func (t *CommandTx) RequireKindUnchanged(ctx context.Context, kind store.Kind) error {
	if t == nil {
		return ErrTransactionEnded
	}
	if !t.active.Load() {
		return t.poison(ErrTransactionEnded)
	}
	if ctx == nil {
		return t.poison(replication.ErrInvalid)
	}
	if err := t.valid(ctx); err != nil {
		return err
	}
	if _, err := t.s.kinds.get(kind); err != nil {
		return t.poison(err)
	}
	index := len(t.mutations)
	return t.RequireCommitCheck(func(context.Context) error {
		// Read the final slice through t, not a captured empty suffix: later
		// appends may have reallocated the journal since registration.
		for _, mutation := range t.mutations[index:] {
			if mutation.key.Kind == kind {
				return replication.ErrCAS
			}
		}
		return nil
	})
}
