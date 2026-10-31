package replica

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// SQLiteAtomic owns the production authorization/mutation/dedup transaction.
// Domain callbacks obtain *sqlite.CommandTx from State(), never reenter Store.
type SQLiteAtomic struct{ Store *sqlite.Store }

// Transact binds current domain reads and dedup to Store.Command's fenced tx.
func (a SQLiteAtomic) Transact(ctx context.Context, term replication.Term, fn func(context.Context, CommandTx) error) error {
	if a.Store == nil || fn == nil {
		return ErrUnavailable
	}
	return a.Store.Command(ctx, term, func(ctx context.Context, tx *sqlite.CommandTx) error { return fn(ctx, sqliteCommandTx{tx}) })
}

type sqliteCommandTx struct{ tx *sqlite.CommandTx }

func (t sqliteCommandTx) State() any { return t.tx }
func dedupKey(id string) string      { return "replica:" + fingerprint([]byte(id)) }
func (t sqliteCommandTx) GetDedup(ctx context.Context, id string) (DedupRecord, bool, error) {
	raw, err := t.tx.GetSection(ctx, "dedup", dedupKey(id))
	if errors.Is(err, store.ErrNotFound) {
		return DedupRecord{}, false, nil
	}
	if err != nil {
		return DedupRecord{}, false, err
	}
	var record DedupRecord
	if err := decodeStrict(raw, &record); err != nil {
		return record, false, err
	}
	return record, true, nil
}
func (t sqliteCommandTx) PutDedup(ctx context.Context, id string, r DedupRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return t.tx.PutSection(ctx, "dedup", replication.Entry{Key: dedupKey(id), Data: raw})
}

var _ AtomicCommands = SQLiteAtomic{}
