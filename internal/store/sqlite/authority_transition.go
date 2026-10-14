package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// EnablePrimary is an explicit CAS transition for an existing standalone
// database. Startup flags cannot silently overwrite durable authority.
func (s *Store) EnablePrimary(ctx context.Context, expected replication.Generation) (replication.Authority, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return replication.Authority{}, store.ErrClosed
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return replication.Authority{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return a, err
	}
	current, err := installedID(ctx, s, tx, a)
	if err != nil {
		return a, err
	}
	if !current.Same(expected) {
		return a, replication.ErrCAS
	}
	if a.Mode == replication.Primary && !s.writeBlocked.Load() {
		return a, nil
	}
	if a.Mode != replication.Standalone || s.writeBlocked.Load() {
		return a, replication.ErrFenced
	}
	if a.Incarnation == math.MaxUint64 {
		return a, replication.ErrInvalid
	}
	a.Mode, a.Installed = replication.Primary, current
	a.Incarnation++
	a.WatchLineage = uuid.NewString()
	if err := writeAuthority(ctx, tx, a); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	s.bus.reset()
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	return a, nil
}

// ResolveConflict explicitly chooses one validated branch and allocates a NEW
// epoch. No digest, UUID, address or clock tie-break can resolve a partition.
// The caller supplies operator authorization/confirmation and serves the new
// complete bundle before exposing success to clients.
func (s *Store) ResolveConflict(ctx context.Context, chosen *replication.Validated, req replication.ResolveRequest) (replication.Authority, error) {
	if chosen == nil || req.RequestID == "" || len(req.RequestID) > 256 {
		return replication.Authority{}, replication.ErrInvalid
	}
	image := chosen.Image()
	if _, err := s.ValidateImage(ctx, image); err != nil {
		return replication.Authority{}, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return replication.Authority{}, store.ErrClosed
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return replication.Authority{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return a, err
	}
	if image.Term.ClusterID != a.Term.ClusterID {
		return a, replication.ErrWrongCluster
	}
	if a.Mode == replication.Primary && a.LastResolutionRequest == req.RequestID && a.Parent != nil && a.Parent.Same(image.Generation()) {
		return a, nil
	}
	if s.writeBlocked.Load() {
		return a, replication.ErrFenced
	}
	if a.Conflict == nil && a.Divergence == nil {
		return a, replication.ErrCAS
	}
	if !a.Term.Same(req.ExpectedTerm) || !sameConflict(a.Conflict, req.ExpectedConflict) || !sameDivergence(a.Divergence, req.ExpectedDivergence) {
		return a, replication.ErrCAS
	}
	current, err := installedID(ctx, s, tx, a)
	if err != nil {
		return a, err
	}
	if !current.Same(req.ExpectedInstalled) {
		return a, replication.ErrCAS
	}
	selected := image.Generation().Same(current) || (a.Conflict != nil && image.Term.Same(*a.Conflict)) || (a.Divergence != nil && image.Generation().Same(*a.Divergence))
	if !selected || image.Term.Epoch > a.MaxObservedEpoch {
		return a, replication.ErrCAS
	}
	if a.MaxObservedEpoch == math.MaxUint64 || a.Incarnation == math.MaxUint64 || a.InstallSequence == math.MaxUint64 {
		return a, replication.ErrInvalid
	}
	if current.Term.ClusterID != "" {
		if err := s.quarantine(ctx, tx); err != nil {
			return a, err
		}
	}
	if err := s.replaceReplicaRows(ctx, tx, image); err != nil {
		return a, err
	}
	parent := image.Generation()
	a.Parent = &parent
	a.Term = replication.Term{ClusterID: a.Term.ClusterID, Epoch: a.MaxObservedEpoch + 1, PrimaryID: a.MemberID, PromotionID: uuid.NewString(), PrimaryName: s.opts.Identity}
	a.MaxObservedEpoch, a.Mode, a.Conflict, a.Divergence = a.Term.Epoch, replication.Primary, nil, nil
	a.Incarnation++
	a.InstallSequence++
	a.LastResolutionRequest = req.RequestID
	a.LastPromotionRequest = ""
	a.WatchLineage = uuid.NewString()
	a.Installed = replication.Generation{Term: a.Term}
	for _, key := range []string{metaResourceVersion, metaFeedCompacted} {
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=?`, strconv.FormatUint(image.ObjectResourceVersion, 10), key); err != nil {
			return a, err
		}
	}
	if err := writeAuthority(ctx, tx, a); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	s.head.Store(image.ObjectResourceVersion)
	s.compacted.Store(image.ObjectResourceVersion)
	s.sinceCompacted = 0
	s.bus.reset()
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	return a, nil
}

func sameConflict(a, b *replication.Term) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Same(*b)
}
func sameDivergence(a, b *replication.Generation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Same(*b)
}

// ReadQuarantined is a private recovery seam, never a safe/public backup.
// Rows remain node-local; callers must authorize any export and redact secrets.
func (s *Store) ReadQuarantined(ctx context.Context, generation replication.Generation) (*replication.Image, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	var raw string
	if err := s.r.QueryRowContext(ctx, `SELECT image FROM replication_quarantine WHERE generation=?`, generation.ETag()).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	// Historical images may have an older registered-kind manifest. They are
	// private recovery evidence, never installed without current validation.
	if int64(len(raw)) > s.opts.ReplicationLimits.Defaults().MaxBytes {
		return nil, replication.ErrTooLarge
	}
	var header replication.Image
	if err := json.Unmarshal([]byte(raw), &header); err != nil {
		return nil, err
	}
	v, err := replication.Decode(strings.NewReader(raw), replication.Validation{SchemaVersion: header.StoreSchemaVersion, Kinds: header.KindManifest, Limits: s.opts.ReplicationLimits})
	if err != nil {
		return nil, err
	}
	i := v.Image()
	if !i.Generation().Same(generation) {
		return nil, replication.ErrDivergent
	}
	return &i, nil
}
