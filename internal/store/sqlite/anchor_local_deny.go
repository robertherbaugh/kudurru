package sqlite

import (
	"context"
	"sync/atomic"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// AnchorDenial owns one original acquisition against actual committed State.
// It cannot grant/advance/checkpoint keys or accept caller-supplied revoked IDs.
// Copies share consumption. Followers require no primary writer credential.
type AnchorDenial struct{ cell *anchorDenial }

type anchorDenial struct {
	source    *AnchorSource
	cursor    *anchorCursor
	original  context.Context
	life      anchorOriginalLifetime
	clock     *trustanchors.SessionClock
	attempt   *retirement.WriteAttempt
	mode      *anchorModeAttempt
	installed replication.Generation
	authority string
	input     atomic.Pointer[anchorReceiptInput]
	committed atomic.Bool
}

// AnchorVerifiedDenial contains only the original opaque authenticated evidence.
// No public constructor can convert structural state or IDs into this proof.
type AnchorVerifiedDenial struct{ cell *anchorVerifiedDenial }

type anchorVerifiedDenial struct {
	original    *anchorDenial
	observation trustanchors.Observation
}

// AnchorDenialReceipt is a Store-bound single-use own-effect acknowledgment.
// It certifies only local denial completion, never accepted canonical progress
// or serving authority. Failed/unknown persistence produces no usable receipt.
type AnchorDenialReceipt struct{ cell *anchorDenialReceipt }

type anchorDenialReceipt struct {
	original *anchorDenial
	proof    *anchorVerifiedDenial
	after    *anchorCursor
	used     atomic.Bool
}

// CaptureAnchorDenial must run before acquisition. The original concrete clock,
// <=30s context and complete accepted/local/installed/authority cut are retained.
func (s *Store) CaptureAnchorDenial(ctx context.Context, expected accepted.Version, clock *trustanchors.SessionClock) (*AnchorDenial, error) {
	cell, err := s.captureAnchorDenial(ctx, expected, clock)
	if err != nil {
		return nil, err
	}
	return &AnchorDenial{cell: cell}, nil
}

func (s *Store) captureAnchorDenial(ctx context.Context, expected accepted.Version, clock *trustanchors.SessionClock) (*anchorDenial, error) {
	if ctx == nil || clock == nil {
		return nil, replication.ErrInvalid
	}
	if err := s.rejectAnchorReentry(ctx); err != nil {
		return nil, err
	}
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	life, err := captureAnchorLifetime(ctx)
	if err != nil {
		return nil, err
	}
	source := s.anchors.Load()
	if source == nil || source.closed.Load() || s.anchorBlocked.Load() {
		return nil, ErrAnchorUnavailable
	}
	cursor := source.cursor.Load()
	if cursor == nil || cursor.awaiting || cursor.mark == (retirement.Mark{}) || cursor.local == nil || cursor.gate == nil {
		return nil, ErrAnchorUnavailable
	}
	if cursor.owner.Version() != expected {
		return nil, accepted.ErrCAS
	}
	attempt, err := cursor.gate.CaptureWrite(ctx, cursor.mark)
	if err != nil {
		return nil, err
	}
	mode, err := captureAnchorMode(ctx, source, cursor, true)
	if err != nil {
		return nil, err
	}
	installed, authority, err := s.captureAnchorBase(ctx, cursor)
	if err != nil {
		return nil, err
	}
	if source != s.anchors.Load() || source.cursor.Load() != cursor || source.closed.Load() {
		return nil, accepted.ErrCAS
	}
	return &anchorDenial{source: source, cursor: cursor, original: ctx, life: life, clock: clock, attempt: attempt, mode: mode, installed: installed, authority: authority}, nil
}

// CaptureAnchorRevocation pre-captures BOTH attempts before a single original
// acquisition. Phase2 can consume only its own confirmed phase1 exact postimage.
// It never samples a successor writer, clock, owner, expected version or context.
func (s *Store) CaptureAnchorRevocation(ctx context.Context, expected accepted.Version, clock *trustanchors.SessionClock) (*AnchorDenial, *AnchorUpdate, error) {
	if ctx == nil {
		return nil, nil, replication.ErrInvalid
	}
	writer, ok := WriterTokenFromContext(ctx)
	if !ok {
		return nil, nil, replication.ErrFenced
	}
	cell, err := s.captureAnchorDenial(ctx, expected, clock)
	if err != nil {
		return nil, nil, err
	}
	second, err := cell.cursor.gate.CaptureWrite(ctx, cell.cursor.mark)
	if err != nil {
		return nil, nil, err
	}
	mode, err := captureAnchorMode(ctx, cell.source, cell.cursor, true)
	if err != nil {
		return nil, nil, err
	}
	if cell.source.cursor.Load() != cell.cursor {
		return nil, nil, accepted.ErrCAS
	}
	update := &anchorUpdate{source: cell.source, cursor: cell.cursor, original: ctx, writer: writer, attempt: second, mode: mode, revoke: cell, authorityCut: cell.authority, installed: &cell.installed}
	return &AnchorDenial{cell: cell}, &AnchorUpdate{cell: update}, nil
}

// Verify must be invoked by the acquisition adapter directly on receipt, before
// holding/reusing that received message. It samples the ORIGINAL clock once,
// authenticates only captured committed State, and retains original TTL evidence.
// A second input or failed verification cannot restart this attempt.
func (d *AnchorDenial) Verify(message *dns.Msg) (*AnchorVerifiedDenial, error) {
	if d == nil || d.cell == nil {
		return nil, replication.ErrInvalid
	}
	c := d.cell
	if !c.input.CompareAndSwap(nil, anchorReceiptTerminal) {
		return nil, retirement.ErrUsed
	}
	if err := c.original.Err(); err != nil {
		return nil, err
	}
	if err := c.life.check(); err != nil {
		return nil, err
	}
	state, err := c.cursor.owner.State()
	if err != nil {
		return nil, err
	}
	at := c.clock.Sample()
	observation, err := trustanchors.Verify(state, message, at)
	if err != nil {
		return nil, err
	}
	if len(observation.Revocations()) == 0 {
		return nil, replication.ErrInvalid
	}
	return &AnchorVerifiedDenial{cell: &anchorVerifiedDenial{original: c, observation: observation}}, nil
}

// CommitAnchorDenial is a deny-only local transaction, including on followers.
// It neither calls generic Command/checkWriter nor changes canonical authority,
// checkpoints, revision, feed, accepted rows or replicated image. Known valid
// denial that cannot durably complete terminally closes local process admission.
func (s *Store) CommitAnchorDenial(proof *AnchorVerifiedDenial) (AnchorCommit, *AnchorDenialReceipt, error) {
	if proof == nil || proof.cell == nil || proof.cell.original == nil || proof.cell.original.source.store != s {
		return AnchorCommit{}, nil, replication.ErrInvalid
	}
	p := proof.cell
	c := p.original
	defer c.source.flushAnchorClose()
	if !c.committed.CompareAndSwap(false, true) {
		return AnchorCommit{}, nil, retirement.ErrUsed
	}
	operation := &anchorOperation{source: c.source, before: c.cursor, authorityCut: c.authority, originalLife: &c.life, installed: c.installed, sql: anchorSQL{outcome: AnchorRolledBack}}
	var after *anchorCursor
	confirmed := false
	defer func() {
		if !confirmed {
			s.closeAnchorSource(c.source)
		}
	}()
	err := s.withAnchorMode(c.mode, operation, func() error {
		result := c.cursor.gate.WithWrite(c.attempt, func(scope *retirement.WriteScope) error {
			withdrawal, err := scope.PrepareWithdrawal()
			if err != nil {
				return err
			}
			operation.scope = scope
			result := s.writeAnchorDenial(c, p, operation, withdrawal, &after)
			outcome := retirement.FailedOrUnknown
			if operation.sql.outcome == AnchorCommitted || operation.sql.outcome == AnchorUnchanged {
				outcome = retirement.ConfirmedDurableDeny
			}
			resolveErr := withdrawal.Resolve(outcome)
			if outcome == retirement.ConfirmedDurableDeny && resolveErr == nil {
				// Publish only after actual durable disposition AND confirmed resolution.
				// No post-effect ctx/clock check can relabel this completed denial rollback.
				if c.source.cursor.CompareAndSwap(c.cursor, after) {
					confirmed = true
					c.source.notify()
				} else {
					s.closeAnchorSource(c.source)
					resolveErr = accepted.ErrCAS
				}
			}
			if result != nil {
				return result
			}
			return resolveErr
		})
		// Known denial cannot release the common mode writer with open admission
		// after a failed/unknown withdrawal, even if SQL never began.
		if !confirmed {
			s.closeAnchorSource(c.source)
		}
		return result
	})
	receipt := operation.receipt()
	if !confirmed {
		return receipt, nil, operation.completionError(err)
	}
	own := &AnchorDenialReceipt{cell: &anchorDenialReceipt{original: c, proof: p, after: after}}
	return receipt, own, operation.completionError(err)
}

func (s *Store) writeAnchorDenial(c *anchorDenial, p *anchorVerifiedDenial, o *anchorOperation, w *retirement.Withdrawal, after **anchorCursor) (result error) {
	// Original R is already held. No other lock path may acquire R under wmu.
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return store.ErrClosed
	}
	if err := c.original.Err(); err != nil {
		return err
	}
	tx, err := s.w.BeginTx(c.original, nil)
	if err != nil {
		return err
	}
	boundary := anchorSQL{tx: tx}
	defer func() { boundary.finish(&result); o.sql = boundary }()
	if err := o.enter(c.original, s, tx, c.cursor.owner.Inspect().ClusterID); err != nil {
		return err
	}
	a, err := readAuthority(c.original, tx)
	if err != nil {
		return err
	}
	if anchorAuthority(a) != c.authority {
		return replication.ErrCAS
	}
	installed, err := installedID(c.original, s, tx, a)
	if err != nil {
		return err
	}
	if !installed.Same(c.installed) {
		return replication.ErrCAS
	}
	if err := p.observation.CheckLifetime(c.clock.Sample()); err != nil {
		return err
	}
	local, err := anchorLocalDeny(c.cursor.local, c.cursor, p.observation.Revocations())
	if err != nil {
		return err
	}
	next := *c.cursor
	next.local = local
	*after = &next
	o.target, o.prepared = &next, true
	if err := casAnchorLocal(c.original, tx, c.cursor.local, local); err != nil {
		return err
	}
	if err := p.observation.CheckLifetime(c.clock.Sample()); err != nil {
		return err
	}
	if err := c.life.check(); err != nil {
		return err
	}
	if err := o.mode.check(); err != nil {
		return err
	}
	if err := w.CheckPrepared(); err != nil {
		return err
	}
	if sameAnchorLocal(c.cursor.local, local) {
		boundary.unchanged = true
		return nil
	}
	return boundary.commit()
}

// AnchorCommandAfterDenial consumes the receipt once and only for the paired
// original Update. The postimage is precomputed by its own phase1, never read
// latest, merged or supplied by a caller. Both complete owner and writer remain
// ORIGINAL; the exact original proof is checked at phase2 entry and final hook.
func (s *Store) AnchorCommandAfterDenial(update *AnchorUpdate, receipt *AnchorDenialReceipt, fn func(context.Context, *CommandTx, trustanchors.Observation) error) (AnchorCommit, error) {
	if update == nil || update.cell == nil || receipt == nil || receipt.cell == nil || fn == nil {
		return AnchorCommit{}, replication.ErrInvalid
	}
	u, r := update.cell, receipt.cell
	if u.source == nil || u.source.store != s || u.revoke == nil || r.original != u.revoke || r.original.source.store != s {
		return AnchorCommit{}, replication.ErrInvalid
	}
	if !r.used.CompareAndSwap(false, true) {
		return AnchorCommit{}, retirement.ErrUsed
	}
	if r.after == nil || r.after.owner.Version() != u.cursor.owner.Version() || r.after.gate != u.cursor.gate {
		return AnchorCommit{}, replication.ErrInvalid
	}
	return s.runAnchorCommand(u, r.after, func(ctx context.Context, tx *CommandTx) error {
		p := r.proof
		if err := p.observation.CheckLifetime(u.revoke.clock.Sample()); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(func(context.Context) error { return p.observation.CheckLifetime(u.revoke.clock.Sample()) }); err != nil {
			return err
		}
		return fn(ctx, tx, p.observation)
	})
}
