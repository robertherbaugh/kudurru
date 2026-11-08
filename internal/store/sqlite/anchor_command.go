package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// AnchorUpdate retains one original finite context, carried writer, full owner
// version and Store-bound attempt. Copies share consumption; no later context,
// writer, proof, expected version or Store can rescue a refused operation.
type AnchorUpdate struct{ cell *anchorUpdate }

type anchorUpdate struct {
	source       *AnchorSource
	cursor       *anchorCursor
	original     context.Context
	writer       WriterToken
	attempt      *retirement.WriteAttempt
	mode         *anchorModeAttempt
	used         atomic.Bool
	revoke       *anchorDenial
	authorityCut string
	installed    *replication.Generation
}

// CaptureAnchorUpdate must precede acquisition of DNS proof or other inputs.
// The original context must already expire within 30 seconds; no timeout is
// replaced or started here. It does not sample a writer, admit a projection, or
// turn a proposal into a live gate.
func (s *Store) CaptureAnchorUpdate(ctx context.Context, expected accepted.Version) (*AnchorUpdate, error) {
	if ctx == nil {
		return nil, replication.ErrInvalid
	}
	if err := s.rejectAnchorReentry(ctx); err != nil {
		return nil, err
	}
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	writer, ok := WriterTokenFromContext(ctx)
	if !ok {
		return nil, replication.ErrFenced
	}
	source := s.anchors.Load()
	if source == nil || source.closed.Load() || s.anchorBlocked.Load() {
		return nil, ErrAnchorUnavailable
	}
	cursor := source.cursor.Load()
	if cursor == nil || cursor.awaiting || cursor.gate == nil {
		return nil, ErrAnchorUnavailable
	}
	if cursor.owner.Version() != expected {
		return nil, accepted.ErrCAS
	}
	if err := requireAnchorAcquisitionLifetime(ctx); err != nil {
		return nil, err
	}
	attempt, err := cursor.gate.CaptureWrite(ctx, cursor.mark)
	if err != nil {
		return nil, err
	}
	mode, err := captureAnchorMode(ctx, source, cursor, true)
	if err != nil {
		return nil, err
	}
	if source.cursor.Load() != cursor {
		return nil, accepted.ErrCAS
	}
	return &AnchorUpdate{cell: &anchorUpdate{source: source, cursor: cursor, original: ctx, writer: writer, attempt: attempt, mode: mode}}, nil
}

// AnchorCommand enters original retirement exclusion BEFORE wmu and executes
// the callback once. Persistence retains independent original cryptographic and
// writer checks. A structural owner or successful Staged value grants nothing.
func (s *Store) AnchorCommand(update *AnchorUpdate, fn func(context.Context, *CommandTx) error) (AnchorCommit, error) {
	if update == nil || update.cell == nil || update.cell.source == nil || update.cell.source.store != s {
		return AnchorCommit{}, replication.ErrInvalid
	}
	cell := update.cell
	if cell.revoke != nil {
		return AnchorCommit{}, replication.ErrInvalid
	}
	return s.runAnchorCommand(cell, cell.cursor, fn)
}

func (s *Store) runAnchorCommand(cell *anchorUpdate, base *anchorCursor, fn func(context.Context, *CommandTx) error) (AnchorCommit, error) {
	if !cell.used.CompareAndSwap(false, true) {
		return AnchorCommit{}, retirement.ErrUsed
	}
	if fn == nil {
		return AnchorCommit{}, replication.ErrInvalid
	}
	operation := &anchorOperation{source: cell.source, before: base, authorityCut: cell.authorityCut, expectedInstalled: cell.installed, sql: anchorSQL{outcome: AnchorRolledBack}}
	err := s.withAnchorMode(cell.mode, operation, func() error {
		return cell.cursor.gate.WithWrite(cell.attempt, func(scope *retirement.WriteScope) error {
			change, err := scope.BeginChange()
			if err != nil {
				return err
			}
			operation.change = change
			operation.scope = scope
			result := s.command(cell.original, cell.writer.Term(), fn, operation)
			resolveErr := operation.resolve()
			if result != nil {
				return result
			}
			return resolveErr
		})
	})
	return operation.receipt(), operation.completionError(err)
}

// anchorOperation retains private original admission. Managed/initialized
// installs hold R; captured awaiting-to-awaiting installs have no history/gate
// and can only preserve their exact non-granting owner and local provenance.
// It never crosses an outward callback; actual SQL termination owns completion.
type anchorOperation struct {
	source            *AnchorSource
	before            *anchorCursor
	target            *anchorCursor
	localTx           *sql.Tx
	localContext      context.Context
	member            string
	localChanged      bool
	publishPending    bool
	installGate       *retirement.Gate
	authorityCut      string
	expectedInstalled *replication.Generation
	originalLife      *anchorOriginalLifetime
	awaitingInstall   bool                   // explicitly captured pre-owner lane, never a managed permit
	installed         replication.Generation // original installation baseline, zero for commands/static legacy
	change            *retirement.Change
	scope             *retirement.WriteScope
	mode              *anchorModeScope
	sql               anchorSQL
	prepared          bool
}

func (o *anchorOperation) enter(ctx context.Context, s *Store, tx queryer, cluster string) error {
	if s.anchorBlocked.Load() || o.source != s.anchors.Load() || o.source == nil || o.source.closed.Load() || o.source.cursor.Load() != o.before {
		return accepted.ErrCAS
	}
	rows, err := loadAnchorRows(ctx, tx)
	if err != nil {
		return err
	}
	current, err := decodeAnchorCursor(rows, cluster)
	if err != nil {
		return err
	}
	if !sameAnchorCursor(o.before, current) {
		return accepted.ErrCAS
	}
	if o.authorityCut != "" {
		a, err := readAuthority(ctx, tx)
		if err != nil {
			return err
		}
		if anchorAuthority(a) != o.authorityCut {
			return replication.ErrCAS
		}
		if o.expectedInstalled != nil {
			sqlTx, ok := tx.(*sql.Tx)
			if !ok {
				return replication.ErrInvalid
			}
			installed, err := installedID(ctx, s, sqlTx, a)
			if err != nil {
				return err
			}
			if !installed.Same(*o.expectedInstalled) {
				return replication.ErrCAS
			}
		}
	}
	return o.captureLocalTx(ctx, s, tx, current)
}

func (o *anchorOperation) prepare(next *anchorCursor) error {
	if o.prepared {
		return retirement.ErrUsed
	}
	if o.awaitingInstall && (!anchorAwaitingInstallCursor(o.before) || next == nil || !next.awaiting || next.gate != nil || next.mark != (retirement.Mark{}) || next.owner.Version() != (accepted.Version{})) {
		return ErrAnchorUnavailable
	}
	if next == o.before {
		copyNext := *next
		next = &copyNext
	}
	if err := o.stageLocal(next); err != nil {
		return err
	}
	if o.awaitingInstall && !sameAnchorLocal(o.before.local, next.local) {
		return accepted.ErrCAS
	}
	if sameAnchorCursor(o.before, next) && sameAnchorLocal(o.before.local, next.local) {
		next = o.before
	}
	if o.change != nil && next.mark != (retirement.Mark{}) {
		if err := o.change.Stage(next.mark); err != nil {
			return err
		}
	}
	o.target = next
	o.prepared = true
	return nil
}

func (o *anchorOperation) check() error {
	if o.mode != nil {
		if err := o.mode.check(); err != nil {
			return err
		}
	}
	if !o.prepared {
		return ErrAnchorUnavailable
	}
	if o.originalLife != nil {
		if err := o.originalLife.check(); err != nil {
			return err
		}
	}
	if o.awaitingInstall {
		// Pre-held wmu excludes SQL interposition through this final handoff.
		// Only retained immutable identities/atomic flags are checked here: no
		// context methods, SQL, JSON, gate construction or serving registration.
		if o.source == nil || o.source.closed.Load() || o.source.store.anchorBlocked.Load() || o.source.store.anchors.Load() != o.source || o.source.cursor.Load() != o.before || !anchorAwaitingInstallCursor(o.before) || o.target != o.before || o.originalLife == nil || o.scope != nil || o.change != nil || o.installGate != nil || o.localChanged {
			return ErrAnchorUnavailable
		}
	}
	if o.change != nil && o.target.mark != (retirement.Mark{}) {
		return o.change.CheckPrepared()
	}
	// Dormant plans stage no managed Mark, but retain their original lifetime.
	if o.scope != nil {
		return o.scope.Check()
	}
	return nil
}

func (o *anchorOperation) resolve() (result error) {
	defer func() {
		if result != nil {
			if o.source != nil {
				o.source.store.closeAnchorSource(o.source)
			}
			return
		}
		if o.publishPending {
			o.publishResolved()
		}
	}()
	if o.change == nil {
		return nil
	}
	switch o.sql.outcome {
	case AnchorCommitted:
		if o.prepared && o.target.mark != (retirement.Mark{}) {
			return o.change.Resolve(retirement.ConfirmedInstalled)
		}
		return o.change.Resolve(retirement.ConfirmedRollback)
	case AnchorUnchanged, AnchorRolledBack:
		return o.change.Resolve(retirement.ConfirmedRollback)
	default:
		return o.change.Resolve(retirement.ChangeUnknown)
	}
}

func (o *anchorOperation) publish(_ *Store) {
	// Existing SQL callers invoke this only after actual Commit success. A
	// private first-bind candidate is NOT visible until confirmed plan resolution.
	if o.scope != nil {
		o.publishPending = true
		return
	}
	o.publishResolved()
}

func (o *anchorOperation) publishResolved() {
	if o.source == nil || !o.prepared {
		return
	}
	s := o.source.store
	if s.anchors.Load() != o.source || o.source.closed.Load() {
		return
	}
	if !o.source.cursor.CompareAndSwap(o.before, o.target) {
		s.closeAnchorSource(o.source)
		return
	}
	if o.target.local != nil && o.target.local.record.Terminal && o.target.gate != nil {
		o.target.gate.Close()
	}
	o.source.notify()
}

func (o *anchorOperation) receipt() AnchorCommit {
	receipt := AnchorCommit{outcome: o.sql.outcome}
	if (o.sql.outcome == AnchorCommitted || o.sql.outcome == AnchorUnchanged) && o.target != nil && !o.target.awaiting {
		receipt.version, receipt.hasVersion = o.target.owner.Version(), true
	}
	return receipt
}

// rawAnchorWrite allows only an exact non-granting initialization or identical
// root write. Managed writes require the private pre-held original permit.
// The first successful initialization image is immutable within this command;
// a second history, unknown version, root deletion or ABA poisons the whole tx.
func (t *CommandTx) rawAnchorWrite(ctx context.Context, section, key string, data []byte, remove bool) error {
	if !anchorKey(section, key) {
		return nil
	}
	if t.s.anchorBlocked.Load() {
		return t.poison(ErrAnchorUnavailable)
	}
	if !validAnchorRowKey(key) {
		return t.poison(ErrAnchorUnavailable)
	}
	fencedNoop, err := t.anchorPostimageWrite(ctx, section, key, data, remove)
	if err != nil {
		return err
	}
	if remove && key == accepted.RootKey {
		return t.poison(ErrAnchorUnavailable)
	}
	if t.anchor != nil {
		if t.anchor.source != t.s.anchors.Load() || t.anchor.source.cursor.Load() != t.anchor.before {
			return t.poison(accepted.ErrCAS)
		}
		if !remove {
			if err := accepted.ValidateEntry(section, replication.Entry{Key: key, Data: data}); err != nil {
				return t.poison(err)
			}
		}
		return nil
	}
	if remove && fencedNoop {
		rows, err := loadAnchorRows(ctx, t.tx)
		if err != nil {
			return t.poison(err)
		}
		current, err := decodeAnchorCursor(rows, t.authority.Term.ClusterID)
		if err != nil {
			return t.poison(err)
		}
		if !current.awaiting && current.owner.Inspect().Phase == accepted.Dormant {
			return nil
		}
	}
	if key != accepted.RootKey || remove {
		return t.poison(ErrAnchorUnavailable)
	}
	rows, err := loadAnchorRows(ctx, t.tx)
	if err != nil {
		return t.poison(err)
	}
	current, err := decodeAnchorCursor(rows, t.authority.Term.ClusterID)
	if err != nil {
		return t.poison(err)
	}
	// Identical sentinel/Dormant writes remain compatible without any lifecycle.
	// Identical managed writes still require original exclusion.
	if !current.awaiting && current.owner.Inspect().Phase == accepted.Managed {
		return t.poison(ErrAnchorUnavailable)
	}
	if len(rows) == 1 && bytes.Equal(rows[0].Data, data) {
		return nil
	}
	if !current.awaiting || t.anchorInit != nil {
		return t.poison(ErrAnchorUnavailable)
	}
	next, err := decodeAnchorCursor([]replication.Entry{{Key: key, Data: data}}, t.authority.Term.ClusterID)
	if err != nil {
		return t.poison(err)
	}
	if next.awaiting || next.owner.Inspect().Phase != accepted.Dormant {
		return t.poison(ErrAnchorUnavailable)
	}
	// This proposal is kept private until the driver has a real commit receipt.
	t.anchorInit = next
	return nil
}

func validAnchorRowKey(key string) bool {
	if key == accepted.RootKey {
		return true
	}
	if !strings.HasPrefix(key, accepted.ChunkPrefix) {
		return false
	}
	text := strings.TrimPrefix(key, accepted.ChunkPrefix)
	decoded, err := hex.DecodeString(text)
	return err == nil && len(decoded) == 32 && text == strings.ToLower(text)
}

func (t *CommandTx) prepareAnchor(ctx context.Context) error {
	if t.anchor == nil && t.anchorInit == nil && t.anchorLocalInit == nil {
		return nil
	}
	rows, err := loadAnchorRows(ctx, t.tx)
	if err != nil {
		return t.poison(err)
	}
	next, err := decodeAnchorCursor(rows, t.authority.Term.ClusterID)
	if err != nil {
		return t.poison(err)
	}
	if t.anchor == nil {
		if t.anchorInit != nil && !sameAnchorCursor(next, t.anchorInit) {
			return t.poison(accepted.ErrCAS)
		}
		if next.awaiting || next.owner.Inspect().Phase != accepted.Dormant {
			return t.poison(ErrAnchorUnavailable)
		}
		source := t.s.anchors.Load()
		if source != nil {
			before := source.cursor.Load()
			if before == nil || (!before.awaiting && !sameAnchorCursor(before, next)) {
				return t.poison(accepted.ErrCAS)
			}
			next.gate, err = dormantAnchorGate(next.owner)
			if err != nil {
				return t.poison(err)
			}
			t.anchor = &anchorOperation{source: source, before: before}
		} else {
			rowsBefore, err := loadAnchorLocal(ctx, t.tx)
			if err != nil {
				return t.poison(err)
			}
			before := &anchorCursor{awaiting: true, local: rowsBefore}
			if t.anchorInit == nil {
				*before = *next
				before.local = rowsBefore
			}
			t.anchor = &anchorOperation{before: before}
		}
		local, err := loadAnchorLocal(ctx, t.tx)
		if err != nil {
			return t.poison(err)
		}
		if !sameAnchorLocal(t.anchor.before.local, local) {
			return t.poison(accepted.ErrCAS)
		}
		if local != nil && t.anchorLocalInit == nil {
			return t.poison(ErrAnchorUnavailable)
		}
		t.anchor.localTx, t.anchor.localContext, t.anchor.member = t.tx, ctx, t.authority.MemberID
		if err := t.anchor.prepare(next); err != nil {
			return t.poison(err)
		}
		t.localChanged = t.localChanged || t.anchor.localChanged
		return nil
	}
	old := t.anchor.before
	if err := anchorSuccessor(old, next, false); err != nil {
		return t.poison(err)
	}
	// A byte-identical owner after actual mutations is ABA, not a clean no-op.
	if t.anchorTouched && sameAnchorCursor(old, next) {
		return t.poison(accepted.ErrCAS)
	}
	next.gate = old.gate
	if err := t.anchor.prepare(next); err != nil {
		return t.poison(err)
	}
	t.localChanged = t.localChanged || t.anchor.localChanged
	return nil
}

func anchorSuccessor(old, next *anchorCursor, install bool) error {
	if sameAnchorCursor(old, next) {
		return nil
	}
	if old == nil || next == nil || next.awaiting {
		return accepted.ErrCAS
	}
	if old.awaiting {
		if next.owner.Inspect().Phase == accepted.Dormant || install {
			return nil
		}
		return ErrAnchorUnavailable
	}
	before, after := old.owner.Inspect(), next.owner.Inspect()
	if before.ClusterID != after.ClusterID || before.HistoryID != after.HistoryID || !before.InitializedAt.Equal(after.InitializedAt) || after.UpdatedAt.Before(before.UpdatedAt) {
		return accepted.ErrCAS
	}
	if after.Phase != accepted.Managed {
		return accepted.ErrCAS
	}
	if before.Phase == accepted.Managed && (old.mark.DomainID != next.mark.DomainID || after.PolicyID != before.PolicyID || after.SeedDigest != before.SeedDigest) {
		return accepted.ErrCAS
	}
	if after.Generation <= before.Generation || after.Pure.Generation <= before.Pure.Generation {
		return accepted.ErrCAS
	}
	if !install && (after.Generation != before.Generation+1 || after.Previous != old.owner.Version()) {
		return accepted.ErrCAS
	}
	if before.Phase == accepted.Managed {
		oldState, err := old.owner.State()
		if err != nil {
			return err
		}
		newState, err := next.owner.State()
		if err != nil {
			return err
		}
		if !newState.RetainsRecoveryHistory(oldState) {
			return accepted.ErrCAS
		}
		denied := map[string]bool{}
		for _, key := range newState.Inspect().Keys {
			if key.Status == trustanchors.Revoked {
				denied[key.ID] = true
			}
		}
		for _, key := range oldState.Inspect().Keys {
			if key.Status == trustanchors.Revoked && !denied[key.ID] {
				return accepted.ErrCAS
			}
		}
	}
	return nil
}

// anchorPostimageWrite preserves existing postimage-fence refusal semantics.
// This SQL comparison is part of the mutation body, NEVER a final check. Once
// a caller has fenced the intended image, an actual later owned change poisons
// with the original CAS category; re-registration or ABA cannot disarm it.
func (t *CommandTx) anchorPostimageWrite(ctx context.Context, section, key string, data []byte, remove bool) (bool, error) {
	covered := false
	for _, fence := range t.sectionFences {
		if fence.section == section && strings.HasPrefix(key, fence.prefix) {
			covered = true
			break
		}
	}
	if !covered {
		return false, nil
	}
	var raw string
	err := t.tx.QueryRowContext(ctx, `SELECT data FROM replication_sections WHERE section=? AND key=?`, section, key).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, t.poison(err)
	}
	if remove && errors.Is(err, sql.ErrNoRows) || !remove && err == nil && bytes.Equal([]byte(raw), data) {
		return true, nil
	}
	return false, t.poison(replication.ErrCAS)
}
