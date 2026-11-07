package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// ErrAnchorUnavailable refuses missing lifecycle or durable deny capability.
// Managed history requires its exact durable private local-deny carrier.
var ErrAnchorUnavailable = errors.New("store/sqlite: anchor lifecycle unavailable")

// AnchorSource is bound only to actual committed rows of one Store instance.
// It supplies neither serving authority nor a public retirement registration.
// Binding is not a reset: proposals cannot arm or replace an existing source.
type AnchorSource struct {
	mode    *anchorMode
	store   *Store
	cursor  atomic.Pointer[anchorCursor]
	closed  atomic.Bool
	changed chan struct{} // one coalesced local notice; never retirement authority
}

// Changes provides bounded coalesced wakeups for later readiness rebuilding.
// A notification is not an accepted cut, serving permit or retirement boundary.
func (s *AnchorSource) Changes() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.changed
}

func (s *AnchorSource) notify() {
	if s == nil {
		return
	}
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

type anchorCursor struct {
	awaiting bool
	owner    accepted.Snapshot
	mark     retirement.Mark
	gate     *retirement.Gate
	local    *anchorLocal
}

// BindAnchorLifecycle observes committed state under the actual writer lock.
// Writable startup must already carry its original writer; followers only read.
// Managed reopening requires exact committed durable local-deny evidence;
// no missing floor is interpreted as zero and no proposal can rearm this source.
func (s *Store) BindAnchorLifecycle(ctx context.Context) (*AnchorSource, error) {
	if ctx == nil {
		return nil, replication.ErrInvalid
	}
	if err := s.rejectAnchorReentry(ctx); err != nil {
		return nil, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	if s.anchorBlocked.Load() {
		return nil, ErrAnchorUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, err := readAuthority(ctx, s.w)
	if err != nil {
		return nil, err
	}
	if a.Writable() {
		if _, ok := WriterTokenFromContext(ctx); !ok {
			return nil, replication.ErrFenced
		}
		if _, err := s.checkWriter(ctx, s.w); err != nil {
			return nil, err
		}
	}
	rows, err := loadAnchorRows(ctx, s.w)
	if err != nil {
		return nil, err
	}
	current, err := decodeAnchorCursor(rows, a.Term.ClusterID)
	if err != nil {
		return nil, err
	}
	local, err := loadAnchorLocal(ctx, s.w)
	if err != nil {
		return nil, err
	}
	if err := checkAnchorLocal(local, current, a.MemberID); err != nil {
		return nil, err
	}
	current.local = local
	if current.mark != (retirement.Mark{}) {
		if err := requireAnchorAcquisitionLifetime(ctx); err != nil {
			return nil, err
		}
	}
	if existing := s.anchors.Load(); existing != nil {
		if existing.closed.Load() {
			return nil, ErrAnchorUnavailable
		}
		if !sameAnchorCursor(existing.cursor.Load(), current) || !sameAnchorLocal(existing.cursor.Load().local, current.local) {
			return nil, accepted.ErrCAS
		}
		return existing, nil
	}
	if !current.awaiting {
		current.gate, err = restoreAnchorGate(ctx, current)
		if err != nil {
			return nil, err
		}
	}
	source := &AnchorSource{store: s, mode: newAnchorMode(current.mark != (retirement.Mark{})), changed: make(chan struct{}, 1)}
	source.cursor.Store(current)
	s.anchors.Store(source)
	return source, nil
}

func (s *Store) rejectAnchorReentry(ctx context.Context) error {
	if tx, ok := ctx.Value(commandTxKey{}).(*CommandTx); ok && tx != nil {
		return tx.poison(ErrTransactionReentry)
	}
	return nil
}

func anchorKey(section, key string) bool {
	return section == accepted.Section && strings.HasPrefix(key, accepted.Prefix)
}

func loadAnchorRows(ctx context.Context, q queryer) ([]replication.Entry, error) {
	var count, size, longest int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(data AS BLOB))),0),COALESCE(MAX(length(CAST(key AS BLOB))),0) FROM replication_sections WHERE section=? AND substr(CAST(key AS BLOB),1,?)=CAST(? AS BLOB)`, accepted.Section, len(accepted.Prefix), accepted.Prefix).Scan(&count, &size, &longest)
	if err != nil {
		return nil, err
	}
	if count > accepted.MaxRows || size > accepted.MaxDurableBytes || longest > 512 {
		return nil, replication.ErrTooLarge
	}
	rows, err := q.QueryContext(ctx, `SELECT key,data FROM replication_sections WHERE section=? AND substr(CAST(key AS BLOB),1,?)=CAST(? AS BLOB) ORDER BY key`, accepted.Section, len(accepted.Prefix), accepted.Prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]replication.Entry, 0, int(count))
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		out = append(out, replication.Entry{Key: key, Data: []byte(raw)})
	}
	return out, rows.Err()
}

func decodeAnchorCursor(rows []replication.Entry, cluster string) (*anchorCursor, error) {
	if accepted.IsAwaitingOwner(rows) {
		return &anchorCursor{awaiting: true}, nil
	}
	owner, err := accepted.DecodeRows(rows, cluster)
	if err != nil {
		return nil, err
	}
	cursor := &anchorCursor{owner: owner}
	if owner.Inspect().Phase == accepted.Managed {
		projection, err := resolverstate.Project(owner)
		if err != nil {
			return nil, err
		}
		version := owner.Version()
		cursor.mark = retirement.Mark{DomainID: projection.DomainID(), EnvelopeGeneration: version.Generation, PureGeneration: version.Pure.Generation}
		if err := anchorDigest(version.Digest, &cursor.mark.EnvelopeDigest); err != nil {
			return nil, err
		}
		if err := anchorDigest(version.Pure.Digest, &cursor.mark.PureDigest); err != nil {
			return nil, err
		}
	}
	return cursor, nil
}

func anchorDigest(text string, out *[32]byte) error {
	data, err := hex.DecodeString(text)
	if err != nil || len(data) != len(out) {
		return accepted.ErrInvalid
	}
	copy(out[:], data)
	return nil
}

func sameAnchorCursor(a, b *anchorCursor) bool {
	return a != nil && b != nil && a.awaiting == b.awaiting && (a.awaiting || a.owner.Version() == b.owner.Version())
}

func dormantAnchorGate(owner accepted.Snapshot) (*retirement.Gate, error) {
	root := owner.Inspect()
	if root.Phase != accepted.Dormant {
		return nil, ErrAnchorUnavailable
	}
	return retirement.New(retirement.History{ClusterID: root.ClusterID, HistoryID: root.HistoryID}, [32]byte{}, 0)
}

// anchorSQL records the actual SQL boundary. Neither an error nor ErrTxDone is
// evidence of rollback. Commit errors remain unknown even if cleanup succeeds.
// All classification and the matching Gate resolution occur under the same R.
type anchorSQL struct {
	tx        *sql.Tx
	attempted bool
	committed bool
	unchanged bool
	outcome   AnchorOutcome
}

func (a *anchorSQL) commit() error {
	a.attempted = true
	err := a.tx.Commit()
	a.committed = err == nil
	return err
}

func (a *anchorSQL) finish(result *error) {
	if a.committed {
		a.outcome = AnchorCommitted
		return
	}
	if a.tx == nil {
		a.outcome = AnchorRolledBack
		return
	}
	err := a.tx.Rollback()
	if a.attempted || err != nil {
		a.outcome = AnchorUnknown
		if err != nil {
			*result = errors.Join(*result, fmt.Errorf("store/sqlite: transaction rollback unconfirmed: %w", err))
		}
		return
	}
	a.outcome = AnchorRolledBack
	if a.unchanged {
		a.outcome = AnchorUnchanged
	}
}

// AnchorOutcome describes driver completion, never cryptographic acceptance or
// permission to serve. Unknown MUST NOT be replayed as a refused clean command.
type AnchorOutcome uint8

// Driver completion categories preserve uncertainty and actual no-op admission.
const (
	AnchorUnknown AnchorOutcome = iota
	AnchorRolledBack
	AnchorUnchanged
	AnchorCommitted
)

// AnchorCommit has no public constructor. A persistence Staged/Projection/Frame
// cannot be converted to this record or used to mint serving authority.
type AnchorCommit struct {
	outcome    AnchorOutcome
	version    accepted.Version
	hasVersion bool
}

// Outcome reports the original actual driver disposition.
func (c AnchorCommit) Outcome() AnchorOutcome { return c.outcome }

// Version reports a confirmed committed/unchanged owner only, not a grant.
func (c AnchorCommit) Version() (accepted.Version, bool) { return c.version, c.hasVersion }

// AnchorCompletionError preserves a confirmed SQL commit despite a later
// refusal/cancellation. It must never be interpreted as rollback or replayed.
// Existing InstallReplica/ResolveConflict error signatures remain unchanged.
type AnchorCompletionError struct {
	completion AnchorCommit
	cause      error
}

func (e *AnchorCompletionError) Error() string {
	return "store/sqlite: anchor operation committed; completion refused"
}

// Unwrap preserves original refusal categories without logging their inputs.
func (e *AnchorCompletionError) Unwrap() error { return e.cause }

// Completion describes actual driver completion, never response authority.
func (e *AnchorCompletionError) Completion() AnchorCommit { return e.completion }

func (o *anchorOperation) completionError(err error) error {
	if err != nil && o.sql.committed {
		return &AnchorCompletionError{completion: o.receipt(), cause: err}
	}
	return err
}

// requireAnchorAcquisitionLifetime requires an ALREADY bounded original context,
// not a replacement authority or a fresh timeout at admission. Source reads and
// later acquisition using the returned context retain that exact earlier bound.
// Gate capture also preserves its independent original final-admission lifetime.
func requireAnchorAcquisitionLifetime(ctx context.Context) error {
	if ctx == nil {
		return replication.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return retirement.ErrInvalid
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	if remaining > retirement.MaxLifetime {
		return retirement.ErrInvalid
	}
	return nil
}
