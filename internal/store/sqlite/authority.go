package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

const metaAuthority = "replication_authority"

func readAuthority(ctx context.Context, q queryer) (replication.Authority, error) {
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaAuthority).Scan(&raw); err != nil {
		return replication.Authority{}, err
	}
	var a replication.Authority
	d := json.NewDecoder(bytes.NewReader([]byte(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, fmt.Errorf("sqlite: corrupt authority: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return a, fmt.Errorf("sqlite: corrupt authority: trailing data")
	}
	if err := validateAuthority(a); err != nil {
		return a, fmt.Errorf("sqlite: corrupt authority: %w", err)
	}
	return a, nil
}

func validateAuthority(a replication.Authority) error {
	if a.MemberID == "" || len(a.MemberID) > 256 || a.WatchLineage == "" || a.Incarnation == 0 {
		return replication.ErrInvalid
	}
	if a.Mode == replication.Unenrolled {
		if a.Term != (replication.Term{}) || a.Installed != (replication.Generation{}) || a.MaxObservedEpoch != 0 || a.Parent != nil || a.Conflict != nil || a.Divergence != nil {
			return replication.ErrInvalid
		}
		return nil
	}
	if err := a.Term.Validate(); err != nil || a.MaxObservedEpoch < a.Term.Epoch {
		return replication.ErrInvalid
	}
	switch a.Mode {
	case replication.Primary, replication.Standalone:
		if !a.Writable() || !a.Installed.Term.Same(a.Term) {
			return replication.ErrInvalid
		}
	case replication.Follower, replication.Fenced:
		if a.Mode == replication.Follower && (a.Term.PrimaryID == a.MemberID || (a.Installed.Term.ClusterID != "" && !a.Installed.Term.Same(a.Term))) {
			return replication.ErrInvalid
		}
	case replication.Conflict:
		if a.Conflict == nil || a.Conflict.Same(a.Term) || a.Conflict.Epoch != a.Term.Epoch || a.Conflict.ClusterID != a.Term.ClusterID {
			return replication.ErrInvalid
		}
	default:
		return replication.ErrInvalid
	}
	if a.Installed.Term.ClusterID != "" {
		if err := a.Installed.Term.Validate(); err != nil || a.Installed.Term.ClusterID != a.Term.ClusterID || a.Installed.Term.Epoch > a.Term.Epoch {
			return replication.ErrInvalid
		}
		if a.Installed.Digest != "" {
			if err := a.Installed.Validate(); err != nil {
				return err
			}
		}
	}
	if a.Parent != nil {
		if err := a.Parent.Validate(); err != nil || a.Parent.Term.ClusterID != a.Term.ClusterID || a.Parent.Term.Epoch >= a.Installed.Term.Epoch {
			return replication.ErrInvalid
		}
	}
	if a.Conflict != nil {
		if err := a.Conflict.Validate(); err != nil || a.Mode != replication.Conflict {
			return replication.ErrInvalid
		}
	}
	if a.Divergence != nil {
		if err := a.Divergence.Validate(); err != nil || (a.Mode != replication.Fenced && a.Mode != replication.Conflict) {
			return replication.ErrInvalid
		}
	}
	return nil
}

func writeAuthority(ctx context.Context, tx *sql.Tx, a replication.Authority) error {
	if err := validateAuthority(a); err != nil {
		return err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaAuthority, string(b)); err != nil {
		return err
	}
	for k, v := range map[string]string{metaClusterID: a.Term.ClusterID, metaClusterEpoch: strconv.FormatUint(a.Term.Epoch, 10)} {
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=?`, v, k); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) initializeAuthority(ctx context.Context) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readAuthority(ctx, tx)
	if errors.Is(err, sql.ErrNoRows) {
		a = replication.Authority{MemberID: uuid.NewString(), Mode: replication.Standalone, Incarnation: 1, WatchLineage: uuid.NewString(), MaxObservedEpoch: s.epoch}
		a.Term = replication.Term{ClusterID: s.clusterID, Epoch: s.epoch, PrimaryID: a.MemberID, PrimaryName: s.opts.Identity, PromotionID: uuid.NewString()}
		a.Installed = replication.Generation{Term: a.Term}
		if s.opts.InitialMode == replication.Follower || s.opts.InitialMode == replication.Unenrolled {
			if s.head.Load() != 0 {
				return fmt.Errorf("%w: existing data cannot become an unenrolled mirror", replication.ErrCAS)
			}
			a.Mode, a.Term, a.Installed, a.MaxObservedEpoch = replication.Unenrolled, replication.Term{}, replication.Generation{}, 0
		} else if s.opts.InitialMode != "" && s.opts.InitialMode != replication.Standalone && s.opts.InitialMode != replication.Primary {
			return fmt.Errorf("sqlite: unsupported initial mode %q", s.opts.InitialMode)
		}
		if s.opts.InitialMode == replication.Primary {
			a.Mode = replication.Primary
		}
		if err := writeAuthority(ctx, tx, a); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.authority.Store(&a)
	return nil
}

// Authority returns a defensive copy of local durable authority.
func (s *Store) Authority() replication.Authority {
	a := s.authority.Load()
	if a == nil {
		return replication.Authority{}
	}
	out := *a
	if a.Parent != nil {
		p := *a.Parent
		out.Parent = &p
	}
	if a.Conflict != nil {
		c := *a.Conflict
		out.Conflict = &c
	}
	if a.Divergence != nil {
		g := *a.Divergence
		out.Divergence = &g
	}
	return out
}

func (s *Store) publishAuthorityLocked(a replication.Authority) {
	old := s.authority.Load()
	s.authority.Store(&a)
	if old == nil || old.Incarnation != a.Incarnation || old.Mode != a.Mode || !old.Term.Same(a.Term) {
		close(s.authorityChanged)
		s.authorityChanged = make(chan struct{})
	}
}

func (s *Store) publishGenerationLocked() {
	close(s.generationChanged)
	s.generationChanged = make(chan struct{})
}

// WriterToken binds work to an authority incarnation, including no-op writes.
// Fields are private: Manager/Force/context strings cannot mint write authority.
type WriterToken struct {
	member      string
	term        replication.Term
	incarnation uint64
}
type writerTokenKey struct{}

// WithWriterToken binds a mutation context to the captured authority incarnation.
func WithWriterToken(ctx context.Context, t WriterToken) context.Context {
	return context.WithValue(ctx, writerTokenKey{}, t)
}

// HasWriterToken lets a facade reject stale controller work instead of turning
// a cancelled local leader write into a forwarded request on the new primary.
func HasWriterToken(ctx context.Context) bool {
	_, ok := ctx.Value(writerTokenKey{}).(WriterToken)
	return ok
}

// WriterToken captures current writable authority for later fenced commit checks.
func (s *Store) WriterToken() (WriterToken, error) {
	if s.isClosed() {
		return WriterToken{}, store.ErrClosed
	}
	a := s.Authority()
	if s.writeBlocked.Load() || a.Mode == replication.Fenced || a.Mode == replication.Conflict {
		return WriterToken{}, replication.ErrFenced
	}
	if !a.Writable() {
		return WriterToken{}, replication.ErrReadOnly
	}
	return WriterToken{a.MemberID, a.Term, a.Incarnation}, nil
}

func (s *Store) writeContext(ctx context.Context) (context.Context, error) {
	if tx, ok := ctx.Value(commandTxKey{}).(*CommandTx); ok {
		return nil, tx.poison(ErrTransactionReentry)
	}
	if _, ok := ctx.Value(writerTokenKey{}).(WriterToken); ok {
		return ctx, nil
	}
	t, err := s.WriterToken()
	if err != nil {
		return nil, err
	}
	return WithWriterToken(ctx, t), nil
}

func (s *Store) checkWriter(ctx context.Context, q queryer) (replication.Authority, error) {
	if s.isClosed() {
		return replication.Authority{}, store.ErrClosed
	}
	a, err := readAuthority(ctx, q)
	if err != nil {
		return a, err
	}
	t, ok := ctx.Value(writerTokenKey{}).(WriterToken)
	if s.writeBlocked.Load() || !ok || !a.Writable() || t.member != a.MemberID || t.incarnation != a.Incarnation || !t.term.Same(a.Term) {
		return a, replication.ErrFenced
	}
	return a, nil
}

// Enroll pins the cluster once, on an empty, explicitly unenrolled mirror.
// Authentication and join-token consumption belong to the coordinator.
func (s *Store) Enroll(ctx context.Context, term replication.Term) error {
	if err := term.Validate(); err != nil {
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
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return err
	}
	if a.Mode != replication.Unenrolled {
		if a.Term.Same(term) && a.Mode == replication.Follower {
			return nil
		}
		return replication.ErrCAS
	}
	if term.PrimaryID == a.MemberID {
		return fmt.Errorf("%w: enroll cannot elect self", replication.ErrCAS)
	}
	if a.Incarnation == math.MaxUint64 {
		return replication.ErrInvalid
	}
	a.Term, a.Mode, a.MaxObservedEpoch = term, replication.Follower, term.Epoch
	a.Incarnation++
	if err := writeAuthority(ctx, tx, a); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishAuthorityLocked(a)
	return nil
}

// ObserveAuthority must be called only for authenticated enrolled peers.
// A newer schema can still carry a higher authority that fences this writer.
func (s *Store) ObserveAuthority(ctx context.Context, term replication.Term) (replication.Authority, error) {
	if err := term.Validate(); err != nil {
		return replication.Authority{}, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return replication.Authority{}, store.ErrClosed
	}
	a := s.Authority()
	if term.ClusterID != a.Term.ClusterID {
		return a, replication.ErrWrongCluster
	}
	if term.Epoch < a.Term.Epoch {
		return a, replication.ErrStale
	}
	var result error
	if a.Mode == replication.Conflict {
		result = replication.ErrPromotionConflict
	}
	if a.Divergence != nil {
		result = replication.ErrDivergent
	}
	same := term.Same(a.Term) || (a.Conflict != nil && term.Same(*a.Conflict))
	if same && !s.writeBlocked.Load() {
		return a, result
	}
	// Block immediately, including when disk failure prevents a durable fence.
	s.writeBlocked.Store(true)
	if a.Incarnation < math.MaxUint64 {
		a.Incarnation++
	}
	// A retry of the same failed durable observation preserves its authority.
	if !same {
		if term.Epoch == a.Term.Epoch {
			a.Mode, a.Conflict = replication.Conflict, &term
			result = replication.ErrPromotionConflict
		} else {
			a.Term, a.Mode, a.Conflict, a.Divergence = term, replication.Fenced, nil, nil
			result = nil
		}
	}
	a.MaxObservedEpoch = max(a.MaxObservedEpoch, term.Epoch)
	if _, err := s.w.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
		s.publishAuthorityLocked(a)
		return a, err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		err = writeAuthority(ctx, tx, a)
		if err == nil {
			err = tx.Commit()
		}
	}
	// Publish the fail-closed memory fence even when persistence failed. A
	// failed observation never restores writability; retry it before restart.
	s.publishAuthorityLocked(a)
	if err != nil {
		return a, err
	}
	s.writeBlocked.Store(false)
	return a, result
}

// Promote is the atomic store CAS. The coordinator enforces monotonic primary
// unreachability/two-pull-interval and human confirmation before calling it.
func (s *Store) Promote(ctx context.Context, req replication.PromoteRequest) (replication.Authority, error) {
	if req.RequestID == "" || len(req.RequestID) > 256 {
		return replication.Authority{}, replication.ErrInvalid
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
	if a.Mode == replication.Primary && a.LastPromotionRequest == req.RequestID && a.Parent != nil && a.Parent.Same(req.ExpectedInstalled) && a.Parent.Term.Same(req.ExpectedTerm) {
		return a, nil
	}
	if s.writeBlocked.Load() || a.Mode != replication.Follower || a.Conflict != nil {
		return a, replication.ErrFenced
	}
	if !a.Term.Same(req.ExpectedTerm) {
		return a, replication.ErrCAS
	}
	image, err := s.readGeneration(ctx, tx)
	if err != nil {
		return a, err
	}
	if !image.Generation().Same(req.ExpectedInstalled) || !image.Term.Same(a.Term) {
		return a, replication.ErrCAS
	}
	if a.MaxObservedEpoch == math.MaxUint64 || a.Incarnation == math.MaxUint64 {
		return a, replication.ErrInvalid
	}
	parent := image.Generation()
	a.Parent = &parent
	a.Term = replication.Term{ClusterID: a.Term.ClusterID, Epoch: max(a.Term.Epoch, a.MaxObservedEpoch) + 1, PrimaryID: a.MemberID, PromotionID: uuid.NewString(), PrimaryName: s.opts.Identity}
	a.Mode, a.Conflict, a.LastPromotionRequest = replication.Primary, nil, req.RequestID
	a.MaxObservedEpoch = a.Term.Epoch
	a.Incarnation++
	a.Installed = replication.Generation{Term: a.Term}
	a.WatchLineage = uuid.NewString()
	if err := writeAuthority(ctx, tx, a); err != nil {
		return a, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM changes`); err != nil {
		return a, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=?`, strconv.FormatUint(s.head.Load(), 10), metaFeedCompacted); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	s.compacted.Store(s.head.Load())
	s.bus.reset()
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	return a, nil
}

func (e *Elector) runDynamic(ctx context.Context, start func(context.Context), stop func()) error {
	for {
		e.s.wmu.Lock()
		closed := e.s.isClosed()
		a := e.s.Authority()
		changed := e.s.authorityChanged
		e.s.wmu.Unlock()
		if closed {
			if ctx.Err() != nil {
				return nil
			}
			return store.ErrClosed
		}
		if ctx.Err() != nil {
			return nil
		}
		if !a.Writable() || e.s.writeBlocked.Load() {
			select {
			case <-ctx.Done():
				return nil
			case <-changed:
				continue
			}
		}
		token := WriterToken{a.MemberID, a.Term, a.Incarnation}
		term, cancel := context.WithCancel(WithWriterToken(ctx, token))
		done := make(chan struct{})
		go func() {
			defer close(done)
			if start != nil {
				start(term)
			}
		}()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		cancel()
		<-done
		if stop != nil {
			stop()
		}
	}
}
