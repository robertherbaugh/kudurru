package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

type anchorInstallKey struct{}

type anchorInstall struct {
	source    *AnchorSource
	cursor    *anchorCursor
	original  context.Context
	life      anchorOriginalLifetime
	attempt   *retirement.WriteAttempt
	mode      *anchorModeAttempt
	installed replication.Generation
	authority string
	used      atomic.Bool
}

type anchorOriginalLifetime struct {
	done     <-chan struct{}
	deadline time.Time
	at       time.Time
}

// anchorAwaitingInstallCursor recognizes only an actual decoded migration cut.
// Its fixed fields are memory-only checks, also used in final admission. It
// creates no synthetic history, registration, accepted Version or live gate.
func anchorAwaitingInstallCursor(cursor *anchorCursor) bool {
	if cursor == nil || !cursor.awaiting || cursor.gate != nil || cursor.mark != (retirement.Mark{}) || cursor.owner.Version() != (accepted.Version{}) || cursor.local == nil {
		return false
	}
	r := cursor.local.record
	return r.Format == 1 && r.Provenance == anchorLocalProvenance && anchorLocalHex(r.Carrier, 32) && r.Revision == 1 && r.Phase == "awaiting" && r.Member == "" && r.Cluster == "" && r.History == "" && r.Domain == "" && r.Owner == (accepted.Version{}) && r.RetiredThrough == 0 && r.Denied != nil && len(r.Denied) == 0 && !r.Terminal
}

func captureAnchorLifetime(ctx context.Context) (anchorOriginalLifetime, error) {
	if err := requireAnchorAcquisitionLifetime(ctx); err != nil {
		return anchorOriginalLifetime{}, err
	}
	deadline, _ := ctx.Deadline()
	return anchorOriginalLifetime{done: ctx.Done(), deadline: deadline, at: time.Now()}, nil
}

func (l anchorOriginalLifetime) check() error {
	now := time.Now()
	if l.at.IsZero() || now.UTC().Before(l.at.UTC()) || !now.Before(l.deadline) {
		return context.DeadlineExceeded
	}
	select {
	case <-l.done:
		return context.Canceled
	default:
		return nil
	}
}

func anchorAuthority(a replication.Authority) string {
	data, _ := json.Marshal(a)
	return string(data)
}

// CaptureAnchorInstall retains one actual Store/owner/local/installed/authority
// cut BEFORE ReadGeneration/Fetch. Original ctx must ALREADY expire within 30s.
// Returning a wrapper/detaching it cannot replace original admission lifetime.
// Awaiting sources require real migration provenance; they mint no live gate.
func (s *Store) CaptureAnchorInstall(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		return nil, replication.ErrInvalid
	}
	if err := s.rejectAnchorReentry(ctx); err != nil {
		return nil, err
	}
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	if ctx.Value(anchorInstallKey{}) != nil {
		return nil, retirement.ErrUsed
	}
	source := s.anchors.Load()
	if source == nil || source.closed.Load() || s.anchorBlocked.Load() {
		return nil, ErrAnchorUnavailable
	}
	cursor := source.cursor.Load()
	if cursor == nil {
		return nil, ErrAnchorUnavailable
	}
	life, err := captureAnchorLifetime(ctx)
	if err != nil {
		return nil, err
	}
	var attempt *retirement.WriteAttempt
	if cursor.gate != nil {
		attempt, err = cursor.gate.CaptureWrite(ctx, cursor.mark)
		if err != nil {
			return nil, err
		}
	} else if !anchorAwaitingInstallCursor(cursor) {
		return nil, ErrAnchorUnavailable
	}
	mode, err := captureAnchorMode(ctx, source, cursor, true)
	if err != nil {
		return nil, err
	}
	installed, authority, err := s.captureAnchorBase(ctx, cursor)
	if err != nil {
		return nil, err
	}
	if source != s.anchors.Load() || source.closed.Load() || source.cursor.Load() != cursor {
		return nil, accepted.ErrCAS
	}
	return context.WithValue(ctx, anchorInstallKey{}, &anchorInstall{source: source, cursor: cursor, original: ctx, life: life, attempt: attempt, mode: mode, installed: installed, authority: authority}), nil
}

// Managed/initialized captured lanes acquire R before wmu. The explicit
// awaiting-to-awaiting exception has no history/gate and retains only exact
// non-granting provenance under wmu. A first-bind candidate remains private
// until atomic owner+local commit AND plan resolution. Source/local/base CAS
// arbitrates all competing captures; projection bytes mint no serving permit.
func (s *Store) withAnchorInstall(ctx context.Context, image replication.Image, fn func(context.Context, *anchorOperation) error) error {
	if ctx == nil {
		return replication.ErrInvalid
	}
	if err := s.rejectAnchorReentry(ctx); err != nil {
		return err
	}
	candidate, err := decodeAnchorCursor(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		return err
	}
	cell, _ := ctx.Value(anchorInstallKey{}).(*anchorInstall)
	if cell != nil {
		if cell.source == nil || cell.source.store != s {
			return replication.ErrInvalid
		}
		if !cell.used.CompareAndSwap(false, true) {
			return retirement.ErrUsed
		}
		if err := cell.original.Err(); err != nil {
			return err
		}
		if err := cell.life.check(); err != nil {
			return err
		}
		operation := &anchorOperation{source: cell.source, before: cell.cursor, installed: cell.installed, authorityCut: cell.authority, originalLife: &cell.life, sql: anchorSQL{outcome: AnchorRolledBack}}
		err = s.withAnchorMode(cell.mode, operation, func() error {
			gate, attempt := cell.cursor.gate, cell.attempt
			if gate == nil {
				if candidate.awaiting {
					if cell.attempt != nil || !anchorAwaitingInstallCursor(cell.cursor) {
						return ErrAnchorUnavailable
					}
					operation.awaitingInstall = true
					return fn(cell.original, operation)
				}
				root := candidate.owner.Inspect()
				gate, err = retirement.New(retirement.History{ClusterID: root.ClusterID, HistoryID: root.HistoryID}, [32]byte{}, 0)
				if err != nil {
					return err
				}
				attempt, err = gate.CaptureWrite(cell.original, retirement.Mark{})
				if err != nil {
					return err
				}
			}
			operation.installGate = gate
			err = gate.WithWrite(attempt, func(scope *retirement.WriteScope) error {
				change, err := scope.BeginChange()
				if err != nil {
					return err
				}
				operation.change, operation.scope = change, scope
				result := fn(cell.original, operation)
				resolveErr := operation.resolve()
				if result != nil {
					return result
				}
				return resolveErr
			})
			return err
		})
		return operation.completionError(err)
	}
	// Legacy static installs retain their original context. They cannot admit
	// Managed, consume a schema8 local carrier or bypass captured local CAS.
	if !candidate.awaiting && candidate.owner.Inspect().Phase == accepted.Managed {
		return ErrAnchorUnavailable
	}
	source := s.anchors.Load()
	operation := &anchorOperation{source: source, sql: anchorSQL{outcome: AnchorRolledBack}}
	if source != nil {
		if source.closed.Load() {
			return ErrAnchorUnavailable
		}
		operation.before = source.cursor.Load()
		if operation.before == nil || operation.before.mark != (retirement.Mark{}) || operation.before.local != nil {
			return ErrAnchorUnavailable
		}
	}
	err = fn(ctx, operation)
	if operation.sql.outcome == AnchorUnknown && operation.source != nil {
		if cursor := operation.source.cursor.Load(); cursor != nil && cursor.gate != nil {
			cursor.gate.Close()
		}
	}
	return operation.completionError(err)
}

func (o *anchorOperation) enterInstall(ctx context.Context, s *Store, tx queryer, cluster string) error {
	if s.anchorBlocked.Load() || s.anchors.Load() != o.source {
		return ErrAnchorUnavailable
	}
	if o.source != nil && (o.source.closed.Load() || o.source.cursor.Load() != o.before) {
		return accepted.ErrCAS
	}
	if o.originalLife != nil {
		if err := o.originalLife.check(); err != nil {
			return err
		}
	}
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return err
	}
	if o.authorityCut != "" && anchorAuthority(a) != o.authorityCut {
		return replication.ErrCAS
	}
	rows, err := loadAnchorRows(ctx, tx)
	if err != nil {
		return err
	}
	current, err := decodeAnchorCursor(rows, cluster)
	if err != nil {
		return err
	}
	if o.before != nil && !sameAnchorCursor(o.before, current) {
		return accepted.ErrCAS
	}
	if current.mark != (retirement.Mark{}) && o.scope == nil {
		return ErrAnchorUnavailable
	}
	if err := o.captureLocalTx(ctx, s, tx, current); err != nil {
		return err
	}
	if current.local != nil && o.scope == nil && !o.awaitingInstall {
		return ErrAnchorUnavailable
	}
	if o.awaitingInstall {
		if o.source == nil || o.originalLife == nil || o.scope != nil || o.change != nil || !anchorAwaitingInstallCursor(o.before) || !anchorAwaitingInstallCursor(current) {
			return ErrAnchorUnavailable
		}
		// ResolveConflict's RequestID no-op also needs the actual original
		// baseline before its early return, even though no R scope exists.
		sqlTx, ok := tx.(*sql.Tx)
		if !ok {
			return replication.ErrInvalid
		}
		installed, err := installedID(ctx, s, sqlTx, a)
		if err != nil {
			return err
		}
		if !installed.Same(o.installed) {
			return replication.ErrCAS
		}
	}
	if o.before == nil {
		o.before = current
	}
	return nil
}

func (o *anchorOperation) prepareInstall(image replication.Image) error {
	next, err := decodeAnchorCursor(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		return err
	}
	if o.before.mark != (retirement.Mark{}) || next.mark != (retirement.Mark{}) {
		if o.scope == nil || o.before.local == nil {
			return ErrAnchorUnavailable
		}
	}
	if o.before.local != nil {
		if err := anchorSuccessor(o.before, next, true); err != nil {
			return err
		}
	}
	if next.awaiting && !o.before.awaiting {
		return accepted.ErrCAS
	}
	if !next.awaiting && o.source != nil {
		if o.installGate != nil {
			next.gate = o.installGate
		} else if sameAnchorCursor(o.before, next) {
			next.gate = o.before.gate
		} else {
			next.gate, err = dormantAnchorGate(next.owner)
			if err != nil {
				return err
			}
		}
	}
	return o.prepare(next)
}

func (o *anchorOperation) prepareCurrent() error {
	// Clone before local staging; never mutate the published immutable cursor.
	next := *o.before
	return o.prepare(&next)
}

// One actual read transaction binds original authority, complete owner, local
// carrier and recomputed installed image. A proven never-installed follower has
// legitimate zero baseline; no received-image reseal/conversion occurs here.
func (s *Store) captureAnchorBase(ctx context.Context, cursor *anchorCursor) (generation replication.Generation, authority string, result error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return generation, "", err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && result == nil {
			result = err
		}
	}()
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return generation, "", err
	}
	rows, err := loadAnchorRows(ctx, tx)
	if err != nil {
		return generation, "", err
	}
	current, err := decodeAnchorCursor(rows, a.Term.ClusterID)
	if err != nil {
		return generation, "", err
	}
	local, err := loadAnchorLocal(ctx, tx)
	if err != nil {
		return generation, "", err
	}
	if err := checkAnchorLocal(local, current, a.MemberID); err != nil {
		return generation, "", err
	}
	if !sameAnchorCursor(cursor, current) || !sameAnchorLocal(cursor.local, local) {
		return generation, "", accepted.ErrCAS
	}
	generation, err = installedID(ctx, s, tx, a)
	if err != nil {
		return generation, "", err
	}
	return generation, anchorAuthority(a), nil
}
