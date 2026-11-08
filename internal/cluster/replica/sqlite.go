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

// SQLiteMemberAtomic binds domain authorization/mutation/dedup to one original
// enrolled request. A missing or mismatched request cannot fall back to Command.
// The caller must retain domain dependency pins through completion AND output.
type SQLiteMemberAtomic struct {
	Store   *sqlite.Store
	Request *sqlite.MemberRequest
}

// Transact consumes the original peer request inside the actual SQLite command.
func (a SQLiteMemberAtomic) Transact(ctx context.Context, term replication.Term, fn func(context.Context, CommandTx) error) error {
	if a.Store == nil || a.Request == nil || fn == nil {
		return ErrUnavailable
	}
	if !term.Same(a.Request.OriginalTerm()) {
		return replication.ErrCAS
	}
	return a.Store.MemberCommand(ctx, a.Request, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return fn(ctx, sqliteCommandTx{tx})
	})
}

type sqliteCommandTx struct{ tx *sqlite.CommandTx }

// StageSQLite authorizes and stages domain effects plus dedup in an already
// active original command. Its Outcome is only a proposal: the caller must
// await actual outer Store completion and retain domain/dependency pins through
// one Store-admitted output scope. It never opens or commits a transaction.
func (s *CommandService) StageSQLite(ctx context.Context, tx *sqlite.CommandTx, command Command) (Outcome, error) {
	if s == nil || tx == nil {
		return Outcome{}, ErrUnavailable
	}
	staged := *s
	staged.Backend = sqliteStagedAtomic{tx: tx}
	return staged.Execute(ctx, command)
}

type sqliteStagedAtomic struct{ tx *sqlite.CommandTx }

func (a sqliteStagedAtomic) Transact(ctx context.Context, term replication.Term, fn func(context.Context, CommandTx) error) error {
	if !a.tx.Authority().Same(term) {
		return replication.ErrCAS
	}
	// RequireCommitCheck refuses an ended transaction before any domain code.
	if err := a.tx.RequireCommitCheck(func(ctx context.Context) error { return ctx.Err() }); err != nil {
		return err
	}
	return fn(ctx, sqliteCommandTx(a))
}

func (t sqliteCommandTx) State() any { return t.tx }
func (t sqliteCommandTx) RequireCommitCheck(check func(context.Context) error) error {
	return t.tx.RequireCommitCheck(check)
}
func dedupKey(id string) string { return "replica:" + fingerprint([]byte(id)) }
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
var _ AtomicCommands = SQLiteMemberAtomic{}
