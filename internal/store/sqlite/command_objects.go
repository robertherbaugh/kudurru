package sqlite

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// CheckKindBounds checks staged object counts and encoded bytes before a typed
// owner allocates row payloads. Every failure poisons the original command.
func (t *CommandTx) CheckKindBounds(ctx context.Context, kind store.Kind, maxRows, maxBytes int) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	limits := (replication.Limits{}).Defaults()
	if maxRows < 0 || maxRows > limits.MaxObjects || maxBytes < 0 || int64(maxBytes) > limits.MaxBytes {
		return t.poison(replication.ErrInvalid)
	}
	if _, err := t.s.kinds.get(kind); err != nil {
		return t.poison(err)
	}
	var count, size int64
	if err := t.tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(length(CAST(data AS BLOB))),0) FROM objects WHERE kind=?`, string(kind)).Scan(&count, &size); err != nil {
		return t.poison(err)
	}
	if count > int64(maxRows) || size > int64(maxBytes) {
		return t.poison(replication.ErrTooLarge)
	}
	return nil
}
