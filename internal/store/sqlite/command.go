package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ErrTransactionReentry rejects independent Store writes from a command callback.
var ErrTransactionReentry = errors.New("store/sqlite: use CommandTx methods inside a command transaction")

// ErrTransactionEnded rejects retained transaction handles after callback return.
var ErrTransactionEnded = errors.New("store/sqlite: command transaction ended")

type commandTxKey struct{}

// Command executes the callback ONCE under the writer lock in one fenced
// transaction. Reads, authorization, mutations and dedup must all use CommandTx.
// Errors (including an ignored mutation-method error) roll back everything.
// Callbacks are synchronous; do not retain/use a tx concurrently or perform
// external effects. Do not reenter Store methods, even with a different context.
func (s *Store) Command(ctx context.Context, expected replication.Term, fn func(context.Context, *CommandTx) error) error {
	if fn == nil {
		return replication.ErrInvalid
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	ctx, err := s.writeContext(ctx)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return store.ErrClosed
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := s.checkWriter(ctx, tx)
	if err != nil {
		return err
	}
	if !a.Term.Same(expected) {
		return replication.ErrCAS
	}
	t := &CommandTx{s: s, tx: tx, q: cachedTx{tx: tx, c: s.ws}, authority: a}
	t.active.Store(true)
	defer t.active.Store(false)
	ctx = context.WithValue(ctx, commandTxKey{}, t)
	err = fn(ctx, t)
	t.active.Store(false)
	if err != nil {
		return err
	}
	if t.failed != nil {
		return t.failed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.checkWriter(ctx, tx); err != nil {
		return err
	}
	// No-op authorization/dedup replay still crossed the same fenced barrier.
	if !t.changed {
		return nil
	}
	if err := advanceRevision(ctx, tx, &a); err != nil {
		return err
	}
	if _, err := s.readGeneration(ctx, tx); err != nil {
		return err
	}
	var compactTo uint64
	if len(t.rvs) > 0 && s.sinceCompacted+len(t.mutations) >= s.compactEvery {
		rv := t.rvs[len(t.rvs)-1]
		retention := uint64(s.opts.FeedRetention) // #nosec G115 -- Open replaces nonpositive retention with its positive default.
		if rv > retention && rv-retention > s.compacted.Load() {
			compactTo = rv - retention
			if err := compactFeed(ctx, t.q, compactTo); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(t.rvs) > 0 {
		s.head.Store(t.rvs[len(t.rvs)-1])
	}
	s.sinceCompacted += len(t.mutations)
	if compactTo > 0 {
		s.compacted.Store(compactTo)
		s.sinceCompacted = 0
	}
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	for n, m := range t.mutations {
		s.bus.publish(change{seq: t.rvs[n], typ: m.typ, key: m.key, obj: m.obj})
	}
	return nil
}

// CommandTx exposes only transaction-bound reads/mutations. Object docs retain
// ordinary Apply/status/ownership/finalizer/audit semantics. Section rows share
// this transaction, including a command's sealed dedup outcome.
type CommandTx struct {
	s         *Store
	tx        *sql.Tx
	q         cachedTx
	authority replication.Authority
	active    atomic.Bool
	failed    error
	changed   bool
	mutations []*mutation
	rvs       []uint64
}

func (t *CommandTx) valid(ctx context.Context) error {
	if !t.active.Load() {
		return ErrTransactionEnded
	}
	if t.failed != nil {
		return t.failed
	}
	if err := ctx.Err(); err != nil {
		return t.poison(err)
	}
	return nil
}
func (t *CommandTx) poison(err error) error {
	if err != nil && t.failed == nil {
		t.failed = err
	}
	return err
}

// Authority returns the immutable term checked at transaction entry.
func (t *CommandTx) Authority() replication.Term { return t.authority.Term }

// Get reads current transaction state, including earlier staged mutations.
func (t *CommandTx) Get(ctx context.Context, key store.Key, into store.Object) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	if into == nil || reflect.ValueOf(into).IsNil() || store.KindOf(into) != key.Kind {
		return replication.ErrInvalid
	}
	ki, err := t.s.kinds.get(key.Kind)
	if err != nil {
		return err
	}
	var raw string
	if err := t.tx.QueryRowContext(ctx, `SELECT data FROM objects WHERE kind=? AND namespace=? AND name=?`, string(key.Kind), key.Namespace, key.Name).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	return decodeInto(ki, raw, into)
}

// List uses ordinary selectors and pagination within the transaction's read cut.
func (t *CommandTx) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	if err := t.valid(ctx); err != nil {
		return store.ObjectList{}, err
	}
	return t.s.list(ctx, t.tx, kind, opts)
}
func (t *CommandTx) write(ctx context.Context, m *mutation) (store.Object, error) {
	if m.noop {
		return m.prev.obj.DeepCopyObject().(store.Object), nil
	}
	rv, err := t.s.writeMutation(ctx, t.q, m)
	if err != nil {
		return nil, t.poison(err)
	}
	t.mutations = append(t.mutations, m)
	t.rvs = append(t.rvs, rv)
	t.changed = true
	return m.obj.DeepCopyObject().(store.Object), nil
}

// Apply stages an ordinary validated and audited object mutation in this tx.
func (t *CommandTx) Apply(ctx context.Context, obj store.Object, opt store.ApplyOptions) (store.Object, error) {
	if err := t.valid(ctx); err != nil {
		return nil, err
	}
	if obj == nil || reflect.ValueOf(obj).IsNil() || opt.Manager == "" {
		return nil, t.poison(replication.ErrInvalid)
	}
	key := store.KeyOf(obj)
	if key.Name == "" {
		return nil, t.poison(replication.ErrInvalid)
	}
	ki, err := t.s.kinds.get(key.Kind)
	if err != nil {
		return nil, t.poison(err)
	}
	prev, err := t.s.load(ctx, t.tx, ki, key)
	if err != nil {
		return nil, t.poison(err)
	}
	m, err := t.s.planApply(ctx, ki, key, obj, prev, opt)
	if err != nil {
		return nil, t.poison(err)
	}
	return t.write(ctx, m)
}

// Delete preserves ownership, finalizer and resourceVersion semantics in this tx.
func (t *CommandTx) Delete(ctx context.Context, key store.Key, opt store.DeleteOptions) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	ki, err := t.s.kinds.get(key.Kind)
	if err != nil {
		return t.poison(err)
	}
	prev, err := t.s.load(ctx, t.tx, ki, key)
	if err != nil {
		return t.poison(err)
	}
	if prev == nil {
		return t.poison(fmt.Errorf("%w: %s", store.ErrNotFound, key))
	}
	m, err := t.s.planDelete(ki, key, prev, opt)
	if err != nil {
		return t.poison(err)
	}
	_, err = t.write(ctx, m)
	return err
}

// UpdateStatus stages a validated status-only mutation in this tx.
func (t *CommandTx) UpdateStatus(ctx context.Context, obj store.Object) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	if obj == nil || reflect.ValueOf(obj).IsNil() {
		return t.poison(replication.ErrInvalid)
	}
	key := store.KeyOf(obj)
	ki, err := t.s.kinds.get(key.Kind)
	if err != nil {
		return t.poison(err)
	}
	prev, err := t.s.load(ctx, t.tx, ki, key)
	if err != nil {
		return t.poison(err)
	}
	m, err := t.s.planStatus(ctx, ki, key, obj, prev)
	if err != nil {
		return t.poison(err)
	}
	_, err = t.write(ctx, m)
	return err
}

// GetSection reads a registered owner's current extension row in this tx.
func (t *CommandTx) GetSection(ctx context.Context, section, key string) (json.RawMessage, error) {
	if err := t.valid(ctx); err != nil {
		return nil, err
	}
	if replication.Section(&replication.Payload{}, section) == nil || key == "" {
		return nil, replication.ErrInvalid
	}
	var raw string
	if err := t.tx.QueryRowContext(ctx, `SELECT data FROM replication_sections WHERE section=? AND key=?`, section, key).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// ListSections reads registered owner rows ordered by key in this tx.
func (t *CommandTx) ListSections(ctx context.Context, section string) ([]replication.Entry, error) {
	if err := t.valid(ctx); err != nil {
		return nil, err
	}
	if replication.Section(&replication.Payload{}, section) == nil {
		return nil, replication.ErrInvalid
	}
	rows, err := t.tx.QueryContext(ctx, `SELECT key,data FROM replication_sections WHERE section=? ORDER BY key`, section)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	entries := []replication.Entry{}
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		entries = append(entries, replication.Entry{Key: key, Data: json.RawMessage(raw)})
	}
	return entries, rows.Err()
}

// PutSection stages a bounded, owner-validated row in the same object commit.
func (t *CommandTx) PutSection(ctx context.Context, section string, entry replication.Entry) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	e, err := replication.ValidateEntry(section, entry, t.s.opts.ReplicationLimits)
	if err != nil {
		return t.poison(err)
	}
	if t.s.opts.ValidateReplicaEntry != nil {
		if err := t.s.opts.ValidateReplicaEntry(section, replication.Entry{Key: e.Key, Data: append(json.RawMessage(nil), e.Data...)}); err != nil {
			return t.poison(err)
		}
	}
	result, err := t.tx.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?) ON CONFLICT(section,key) DO UPDATE SET data=excluded.data WHERE replication_sections.data!=excluded.data`, section, e.Key, string(e.Data))
	if err != nil {
		return t.poison(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return t.poison(err)
	}
	t.changed = t.changed || n > 0
	return nil
}

// DeleteSection stages row removal; an absent row is a fenced no-op.
func (t *CommandTx) DeleteSection(ctx context.Context, section, key string) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	if replication.Section(&replication.Payload{}, section) == nil || key == "" || len(key) > 512 {
		return t.poison(replication.ErrInvalid)
	}
	result, err := t.tx.ExecContext(ctx, `DELETE FROM replication_sections WHERE section=? AND key=?`, section, key)
	if err != nil {
		return t.poison(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return t.poison(err)
	}
	t.changed = t.changed || n > 0
	return nil
}
