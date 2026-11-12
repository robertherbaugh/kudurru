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
	return s.command(ctx, expected, fn, nil)
}

func (s *Store) command(ctx context.Context, expected replication.Term, fn func(context.Context, *CommandTx) error, anchor *anchorOperation) (result error) {
	defer func() {
		if anchor == nil {
			s.flushAnchorSourceClosed()
		}
	}() // captured lanes flush after mode/R return
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
	output, _ := ctx.Value(commandOutputKey{}).(*commandOutputInvocation)
	var emission *memberEmission
	if output != nil {
		if output.request == nil || output.request.s != s || output.emit == nil || output.request.state.Load() != commandOutputActive || ctx.Value(memberRequestKey{}) != nil || anchor != nil {
			return replication.ErrInvalid
		}
		if err := output.request.lock(ctx); err != nil {
			return err
		}
	} else if member, ok := ctx.Value(memberRequestKey{}).(*memberRequest); ok {
		if member == nil || member.s != s || member.state.Load() != memberActive {
			return replication.ErrInvalid
		}
		if emission, _ = ctx.Value(memberEmissionKey{}).(*memberEmission); emission != nil && (emission.emit == nil || anchor != nil) {
			return replication.ErrInvalid
		}
		if err := member.lock(ctx); err != nil {
			return err
		}
	} else {
		s.wmu.Lock()
	}
	defer s.wmu.Unlock()
	if s.isClosed() {
		return store.ErrClosed
	}
	receipt, _ := ctx.Value(commandReceiptKey{}).(*CommandReceipt)
	if receipt != nil && !receipt.claim() {
		return replication.ErrInvalid
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		if receipt != nil {
			receipt.state.Store(receiptRolledBack) // no transaction was opened
		}
		return err
	}
	sqlBoundary := anchorSQL{tx: tx}
	var t *CommandTx
	succeeded := false
	defer func() {
		sqlBoundary.finish(&result)
		if receipt != nil {
			// The receipt states the SQL disposition alone. A later output or
			// acknowledgment failure does not change what was committed.
			receipt.complete(s, sqlBoundary, t != nil && t.changed)
		}
		if t != nil && t.member != nil {
			if succeeded && result == nil && (sqlBoundary.outcome == AnchorCommitted || sqlBoundary.outcome == AnchorUnchanged) {
				t.member.complete(s.Authority(), true)
				if emission != nil {
					emission.committed = true
				}
			} else {
				t.member.state.Store(memberTerminal)
				if sqlBoundary.outcome == AnchorUnknown {
					result = errors.Join(result, ErrMemberCompletionUnknown)
				}
			}
		}
		if t != nil && t.primary != nil && sqlBoundary.outcome == AnchorUnknown {
			// Ambiguous completion cannot leave the old standalone writer usable.
			// A restart must read the actual atomic membership/authority outcome.
			s.blockWritesLocked()
			result = errors.Join(result, ErrEnrollmentCompletionUnknown)
			close(s.authorityChanged)
			s.authorityChanged = make(chan struct{})
		}
		operation := anchor
		if t != nil && t.anchor != nil {
			operation = t.anchor
		}
		if operation == nil && t != nil && t.anchorInit != nil {
			operation = &anchorOperation{source: s.anchors.Load()}
		}
		if operation != nil {
			operation.sql = sqlBoundary
			if sqlBoundary.outcome == AnchorUnknown {
				s.anchorBlocked.Store(true)
				if operation.source != nil {
					operation.source.closed.Store(true)
					operation.source.mode.closeMemory()
				}
			}
		}
		if output != nil {
			output.finish(ctx, succeeded, sqlBoundary, &result)
		}
		if emission != nil && emission.committed && result == nil {
			// The acknowledgment of a member command leaves under the writer
			// exclusion of its own commit, like a local command's output.
			result = emission.finish(t.member)
		}
	}()
	a, err := s.checkWriter(ctx, tx)
	if err != nil {
		return err
	}
	if !a.Term.Same(expected) {
		return replication.ErrCAS
	}
	if output != nil {
		if err := output.request.check(); err != nil {
			return err
		}
		if !output.request.token.Matches(a) {
			return replication.ErrCAS
		}
	}
	t = &CommandTx{s: s, tx: tx, q: cachedTx{tx: tx, c: s.ws}, authority: a, anchor: anchor}
	if anchor != nil {
		if err := anchor.enter(ctx, s, tx, a.Term.ClusterID); err != nil {
			return err
		}
	}
	t.active.Store(true)
	defer t.active.Store(false)
	ctx = context.WithValue(ctx, commandTxKey{}, t)
	if output != nil {
		if err := t.RequireCommitCheck(func(context.Context) error { return output.request.check() }); err != nil {
			return err
		}
	}
	if member, ok := ctx.Value(memberRequestKey{}).(*memberRequest); ok {
		t.member = member
		if err := t.enterMember(ctx); err != nil {
			return err
		}
	}
	err = fn(ctx, t)
	t.active.Store(false)
	if err != nil {
		return err
	}
	if t.failed != nil {
		return t.failed
	}
	if err := t.prepareEnrollment(ctx); err != nil {
		return err
	}
	if err := t.prepareAnchor(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.checkWriter(ctx, tx); err != nil {
		return err
	}
	// No-op authorization/dedup replay still crossed the same fenced barrier.
	if !t.changed {
		if err := t.checkCommit(ctx); err != nil {
			return err
		}
		if t.anchor != nil {
			if err := t.anchor.check(); err != nil {
				return err
			}
		}
		if t.member != nil {
			if err := t.member.check(); err != nil {
				return err
			}
		}
		if t.localChanged {
			// Local metadata shares the original admission/rollback boundary,
			// without advancing or publishing the canonical replicated image.
			if err := sqlBoundary.commit(); err != nil {
				return err
			}
			if t.anchor != nil {
				t.anchor.publish(s)
			}
			succeeded = true
			return nil
		}
		sqlBoundary.unchanged = true
		succeeded = true
		return nil
	}
	if err := advanceRevision(ctx, tx, &a); err != nil {
		return err
	}
	if err := t.preparePrimary(ctx, &a); err != nil {
		return err
	}
	image, err := s.readGeneration(ctx, tx)
	if err != nil {
		return err
	}
	if t.primary != nil {
		if _, err := s.ValidateImage(ctx, *image); err != nil {
			return err
		}
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
	if err := t.checkCommit(ctx); err != nil {
		return err
	}
	if t.anchor != nil {
		if err := t.anchor.check(); err != nil {
			return err
		}
	}
	if t.member != nil {
		if err := t.member.check(); err != nil {
			return err
		}
	}
	if err := sqlBoundary.commit(); err != nil {
		return err
	}
	if t.anchor != nil {
		t.anchor.publish(s)
	}
	if len(t.rvs) > 0 {
		s.head.Store(t.rvs[len(t.rvs)-1])
	}
	s.sinceCompacted += len(t.mutations)
	if compactTo > 0 {
		s.compacted.Store(compactTo)
		s.sinceCompacted = 0
	}
	if t.primary != nil {
		s.bus.reset()
	}
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	for n, m := range t.mutations {
		s.bus.publish(change{seq: t.rvs[n], typ: m.typ, key: m.key, obj: m.obj})
	}
	succeeded = true
	return nil
}

// CommandTx exposes only transaction-bound reads/mutations. Object docs retain
// ordinary Apply/status/ownership/finalizer/audit semantics. Section rows share
// this transaction, including a command's sealed dedup outcome.
type CommandTx struct {
	s                *Store
	tx               *sql.Tx
	q                cachedTx
	authority        replication.Authority
	active           atomic.Bool
	failed           error
	changed          bool
	localChanged     bool // node-local metadata, outside the canonical image
	mutations        []*mutation
	rvs              []uint64
	checks           []func(context.Context) error
	sectionFences    []*sectionPrefixFence // bounded by successful final-check registrations
	anchor           *anchorOperation      // only a private original pre-held retirement scope
	anchorInit       *anchorCursor         // narrow sentinel -> Dormant proposal, never durable authority
	anchorTouched    bool                  // monotonic actual-change marker, including ABA
	anchorLocalInit  *anchorLocal          // typed migration-proven unchanged-Dormant local initialization
	enrollmentBefore []replication.Entry   // actual owner before first membership mutation
	primary          *enrollmentPrimary    // same-command initial membership + authority
	member           *memberRequest        // original enrolled peer, never a claimed header
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

// Incarnation returns the local writer incarnation admitted at transaction
// entry. Domain proofs must bind it as well as Authority: a term alone cannot
// distinguish a writer that was fenced and subsequently admitted again.
func (t *CommandTx) Incarnation() uint64 { return t.authority.Incarnation }

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

// UpdateFinalizer stages the metadata-only finalizer change of
// Store.UpdateFinalizer in this tx: exact UID and resourceVersion, nothing
// but metadata.finalizers written, and the object removed when the last
// finalizer of an object that is being deleted goes. It returns the staged
// object, or the removed one (store.Terminated).
func (t *CommandTx) UpdateFinalizer(ctx context.Context, key store.Key, opt store.FinalizerOptions) (store.Object, error) {
	if err := t.valid(ctx); err != nil {
		return nil, err
	}
	if err := opt.Validate(); err != nil {
		return nil, t.poison(err)
	}
	ki, err := t.s.kinds.get(key.Kind)
	if err != nil {
		return nil, t.poison(err)
	}
	prev, err := t.s.load(ctx, t.tx, ki, key)
	if err != nil {
		return nil, t.poison(err)
	}
	if prev == nil {
		return nil, t.poison(fmt.Errorf("%w: %s", store.ErrNotFound, key))
	}
	m, err := t.s.planFinalizer(ki, key, prev, opt)
	if err != nil {
		return nil, t.poison(err)
	}
	return t.write(ctx, m)
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

// CheckSectionBounds rejects excessive section data before a caller allocates
// row payloads. The scalar query sees the same transaction, including staged
// changes. Limits may be zero for an empty section; protocol-default image caps
// bound the requested budget. Errors poison the command, including read no-ops.
func (t *CommandTx) CheckSectionBounds(ctx context.Context, section string, maxRows, maxDataBytes int) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	limits := (replication.Limits{}).Defaults()
	if replication.Section(&replication.Payload{}, section) == nil || maxRows < 0 || maxRows > limits.MaxEntries || maxDataBytes < 0 || int64(maxDataBytes) > limits.MaxBytes {
		return t.poison(replication.ErrInvalid)
	}
	var count, size, longestKey int64
	err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(data AS BLOB))),0),COALESCE(MAX(length(CAST(key AS BLOB))),0) FROM replication_sections WHERE section=?`, section).Scan(&count, &size, &longestKey)
	if err != nil {
		return t.poison(err)
	}
	if count > int64(maxRows) || size > int64(maxDataBytes) || longestKey > 512 {
		return t.poison(replication.ErrTooLarge)
	}
	return nil
}

// ListSections reads registered owner rows ordered by key in this tx. Owners
// call CheckSectionBounds first to enforce their aggregate budget before read.
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
	if err := t.rawAnchorWrite(ctx, section, e.Key, e.Data, false); err != nil {
		return err
	}
	if err := t.captureEnrollment(ctx, section, e.Key); err != nil {
		return err
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
	if n > 0 {
		t.markSectionMutation(section, e.Key)
		t.anchorTouched = t.anchorTouched || anchorKey(section, e.Key)
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
	if err := t.rawAnchorWrite(ctx, section, key, nil, true); err != nil {
		return err
	}
	if err := t.captureEnrollment(ctx, section, key); err != nil {
		return err
	}
	result, err := t.tx.ExecContext(ctx, `DELETE FROM replication_sections WHERE section=? AND key=?`, section, key)
	if err != nil {
		return t.poison(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return t.poison(err)
	}
	if n > 0 {
		t.markSectionMutation(section, key)
		t.anchorTouched = t.anchorTouched || anchorKey(section, key)
	}
	t.changed = t.changed || n > 0
	return nil
}
