package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// migrateAnchorLocal is ONLY the parent schema-8 migration callback, not a
// startup repair API. The parent must register it in the same physical migration
// transaction as marker 8. It refuses naked Managed7 and preexisting carriers.
func migrateAnchorLocal(ctx context.Context, tx *sql.Tx) error {
	var schema string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaSchemaVersion).Scan(&schema); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		schema = "0" // fresh 0->8 chain has not written its final marker yet
	}
	version, err := strconv.Atoi(schema)
	if err != nil || version > 7 || version < 0 {
		return ErrAnchorUnavailable
	}
	rows, err := loadAnchorRows(ctx, tx)
	if err != nil {
		return err
	}
	if !accepted.IsAwaitingOwner(rows) {
		var root accepted.Root
		for _, row := range rows {
			if row.Key == accepted.RootKey {
				if err := json.Unmarshal(row.Data, &root); err != nil {
					return accepted.ErrInvalid
				}
			}
		}
		owner, err := accepted.DecodeRows(rows, root.ClusterID)
		if err != nil {
			return err
		}
		if owner.Inspect().Phase != accepted.Dormant {
			return ErrAnchorUnavailable
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE substr(CAST(key AS BLOB),1,?)=CAST(? AS BLOB)`, len(anchorLocalPrefix), anchorLocalPrefix).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return accepted.ErrCAS
	}
	var carrier [16]byte
	if _, err := rand.Read(carrier[:]); err != nil {
		return err
	}
	local, err := encodeAnchorLocal(anchorLocalRecord{Format: 1, Provenance: anchorLocalProvenance, Carrier: hex.EncodeToString(carrier[:]), Revision: 1, Phase: "awaiting", Denied: []anchorLocalRevocation{}})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES (?,?)`, anchorLocalKey, local.raw)
	return err
}

// Missing current-schema history is an error, including an existing inventory.
// Schema7 static-only databases remain compatible until the real migration.
func loadAnchorLocal(ctx context.Context, q queryer) (*anchorLocal, error) {
	var count, size, longest int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(value AS BLOB))),0),COALESCE(MAX(length(CAST(key AS BLOB))),0) FROM meta WHERE substr(CAST(key AS BLOB),1,?)=CAST(? AS BLOB)`, len(anchorLocalPrefix), anchorLocalPrefix).Scan(&count, &size, &longest); err != nil {
		return nil, err
	}
	if count > 1 || size > anchorLocalMaxBytes || longest > int64(len(anchorLocalKey)) {
		return nil, accepted.ErrBounds
	}
	var schema string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaSchemaVersion).Scan(&schema); err != nil {
		return nil, err
	}
	version, err := strconv.Atoi(schema)
	if err != nil {
		return nil, accepted.ErrInvalid
	}
	if count == 0 {
		if version >= 8 {
			return nil, ErrAnchorUnavailable
		}
		return nil, nil
	}
	// Schema9 preserves the exact schema8 carrier and its provenance. Unknown
	// later schemas still refuse; this is not a marker conversion or repair.
	if version != 8 && version != 9 {
		return nil, ErrAnchorUnavailable
	}
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, anchorLocalKey).Scan(&raw); err != nil {
		return nil, ErrAnchorUnavailable
	}
	return decodeAnchorLocal(raw)
}

func checkAnchorLocal(local *anchorLocal, current *anchorCursor, member string) error {
	if local == nil {
		if !current.awaiting && current.owner.Inspect().Phase == accepted.Managed {
			return ErrAnchorUnavailable
		}
		return nil
	}
	r := local.record
	if current.awaiting {
		if r.Phase != "awaiting" {
			return accepted.ErrCAS
		}
		return nil
	}
	root := current.owner.Inspect()
	if r.Phase == "awaiting" {
		// Only never-managed startup/first-bind can consume migration provenance.
		if root.Phase == accepted.Managed {
			return ErrAnchorUnavailable
		}
		return nil
	}
	if r.Member != member || r.Cluster != root.ClusterID || r.History != root.HistoryID || r.Owner != current.owner.Version() || r.Phase != string(root.Phase) {
		return accepted.ErrCAS
	}
	if root.Phase == accepted.Managed {
		if r.Domain != hex.EncodeToString(current.mark.DomainID[:]) {
			return accepted.ErrCAS
		}
		ids, err := anchorRevoked(current)
		if err != nil {
			return err
		}
		for _, id := range ids {
			found := false
			for _, d := range r.Denied {
				if d.ID == id {
					found = true
					break
				}
			}
			if !found && !r.Terminal {
				return accepted.ErrCAS
			}
		}
		// Extra pending denies cannot reopen this exact installed generation.
		for _, d := range r.Denied {
			found := false
			for _, id := range ids {
				if id == d.ID {
					found = true
					break
				}
			}
			if !found && r.RetiredThrough < r.Owner.Generation {
				return accepted.ErrCAS
			}
		}
	}
	return nil
}

func casAnchorLocal(ctx context.Context, tx *sql.Tx, old, next *anchorLocal) error {
	if old == nil || next == nil {
		return ErrAnchorUnavailable
	}
	if sameAnchorLocal(old, next) {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=? AND value=?`, next.raw, anchorLocalKey, old.raw)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return accepted.ErrCAS
	}
	return nil
}

// InitializeAnchorLocal composes with the parent's original writer-bound
// InitializeDormant callback, including unchanged Dormant. It cannot initialize
// Managed, invent migration provenance, reset a carrier or accept proposals.
// Call while CommandTx is active, AFTER global initialization/validation.
func (t *CommandTx) InitializeAnchorLocal(ctx context.Context) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	rows, err := loadAnchorRows(ctx, t.tx)
	if err != nil {
		return t.poison(err)
	}
	current, err := decodeAnchorCursor(rows, t.authority.Term.ClusterID)
	if err != nil {
		return t.poison(err)
	}
	if current.awaiting || current.owner.Inspect().Phase != accepted.Dormant {
		return t.poison(ErrAnchorUnavailable)
	}
	local, err := loadAnchorLocal(ctx, t.tx)
	if err != nil {
		return t.poison(err)
	}
	if local == nil {
		return t.poison(ErrAnchorUnavailable)
	}
	if err := checkAnchorLocal(local, current, t.authority.MemberID); err != nil {
		return t.poison(err)
	}
	if err := t.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix); err != nil {
		return err
	}
	t.anchorLocalInit = local
	return nil
}

func (o *anchorOperation) captureLocalTx(ctx context.Context, _ *Store, q queryer, current *anchorCursor) error {
	local, err := loadAnchorLocal(ctx, q)
	if err != nil {
		return err
	}
	a, err := readAuthority(ctx, q)
	if err != nil {
		return err
	}
	if err := checkAnchorLocal(local, current, a.MemberID); err != nil {
		return err
	}
	if o.before != nil && !sameAnchorLocal(o.before.local, local) {
		return accepted.ErrCAS
	}
	current.local = local
	tx, ok := q.(*sql.Tx)
	if !ok {
		return replication.ErrInvalid
	}
	o.localTx, o.localContext, o.member = tx, ctx, a.MemberID
	return nil
}

func (o *anchorOperation) stageLocal(next *anchorCursor) error {
	old := o.before.local
	if old == nil {
		if next.mark != (retirement.Mark{}) {
			return ErrAnchorUnavailable
		}
		return nil
	}
	if o.localTx == nil {
		return ErrAnchorUnavailable
	}
	local, err := anchorLocalForOwner(old, next, o.member)
	if err != nil {
		return err
	}
	if err := casAnchorLocal(o.localContext, o.localTx, old, local); err != nil {
		return err
	}
	next.local = local
	o.localChanged = !sameAnchorLocal(old, local)
	return nil
}

// restoreAnchorGate loads only the validated committed cut. It creates no read
// registration/serving permit and grants no timer progress. Restored max floor
// denies forever. Resolution precedes publication even after post-effect cancel.
func restoreAnchorGate(ctx context.Context, current *anchorCursor) (*retirement.Gate, error) {
	if current.mark == (retirement.Mark{}) {
		return dormantAnchorGate(current.owner)
	}
	if current.local == nil {
		return nil, ErrAnchorUnavailable
	}
	root := current.owner.Inspect()
	var domain [32]byte
	if err := anchorDigest(current.local.record.Domain, &domain); err != nil {
		return nil, err
	}
	gate, err := retirement.New(retirement.History{ClusterID: root.ClusterID, HistoryID: root.HistoryID}, domain, current.local.record.RetiredThrough)
	if err != nil {
		return nil, err
	}
	if current.local.record.RetiredThrough == math.MaxUint64 {
		gate.Close()
		return gate, nil
	}
	attempt, err := gate.CaptureWrite(ctx, retirement.Mark{})
	if err != nil {
		return nil, err
	}
	err = gate.WithWrite(attempt, func(scope *retirement.WriteScope) error {
		change, err := scope.BeginChange()
		if err != nil {
			return err
		}
		if err := change.Stage(current.mark); err != nil {
			return err
		}
		return change.Resolve(retirement.ConfirmedInstalled)
	})
	if err != nil {
		gate.Close()
		return nil, err
	}
	return gate, nil
}

func (s *Store) closeAnchorSource(source *AnchorSource) {
	s.anchorBlocked.Store(true)
	if source != nil {
		source.closed.Store(true)
		source.mode.closeMemory()
	}
}
