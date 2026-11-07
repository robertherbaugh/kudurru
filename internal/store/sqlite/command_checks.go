package sqlite

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// MaxCommitChecks bounds final immutable dependency/proof checks per command.
const MaxCommitChecks = 16

// RequireCommitCheck registers a synchronous check after all callback work and
// complete-image validation, immediately before commit or successful no-op.
// Registration errors poison the command even if its caller ignores them.
//
// Checks may inspect only already pinned immutable data, memory and the actual
// clock. They cannot use CommandTx, reenter Store, perform I/O, acquire publication
// or dependency locks, or register more checks. The caller must hold dependency
// replacement exclusion through the entire Command call; a check is not a pin.
// This is the admission time, not a promise that time cannot advance in fsync.
func (t *CommandTx) RequireCommitCheck(check func(context.Context) error) error {
	if t == nil {
		return ErrTransactionEnded
	}
	if !t.active.Load() {
		return t.poison(ErrTransactionEnded)
	}
	if t.failed != nil {
		return t.failed
	}
	if check == nil || len(t.checks) >= MaxCommitChecks {
		return t.poison(replication.ErrInvalid)
	}
	t.checks = append(t.checks, check)
	return nil
}

func (t *CommandTx) checkCommit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := t.s.checkWriter(ctx, t.tx); err != nil {
		return err
	}
	for _, check := range t.checks {
		if err := check(ctx); err != nil {
			return err
		}
		if t.failed != nil {
			return t.failed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}
