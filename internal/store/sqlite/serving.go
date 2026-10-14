package sqlite

import (
	"context"
	"database/sql"
	"reflect"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// GenerationToken binds a complete image to its local installation/authority
// cut. Its fields are private: only this store can issue a publication token.
type GenerationToken struct {
	source     *Store
	generation replication.Generation
	authority  replication.Authority
}

// Generation is the full canonical image identity, including its digest.
func (t GenerationToken) Generation() replication.Generation { return t.generation }

// Authority returns a defensive copy of the captured local authority.
func (t GenerationToken) Authority() replication.Authority {
	a := t.authority
	if a.Parent != nil {
		v := *a.Parent
		a.Parent = &v
	}
	if a.Conflict != nil {
		v := *a.Conflict
		a.Conflict = &v
	}
	if a.Divergence != nil {
		v := *a.Divergence
		a.Divergence = &v
	}
	return a
}

// Matches compares an in-memory authority cursor without reading the database.
// Primary cursors intentionally have an empty digest; the token's full digest
// was established in the same transaction as this cursor.
func (t GenerationToken) Matches(a replication.Authority) bool {
	return t.source != nil && reflect.DeepEqual(t.authority, a)
}

// DNSOnly reports whether this cut may only be published as last-good DNS data.
func (t GenerationToken) DNSOnly() bool {
	a := t.authority
	return a.Mode == replication.Fenced || a.Mode == replication.Conflict ||
		a.Conflict != nil || a.Divergence != nil || !a.Term.Same(t.generation.Term)
}

// ReadServingCandidate returns an image and token from ONE read transaction.
// The image remains installed data until an actual serving publisher uses it.
func (s *Store) ReadServingCandidate(ctx context.Context) (*replication.Image, GenerationToken, error) {
	if s.isClosed() {
		return nil, GenerationToken{}, store.ErrClosed
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, GenerationToken{}, err
	}
	defer func() { _ = tx.Rollback() }()
	image, err := s.readGeneration(ctx, tx)
	if err != nil {
		return nil, GenerationToken{}, err
	}
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return nil, GenerationToken{}, err
	}
	return image, GenerationToken{source: s, generation: image.Generation(), authority: a}, nil
}

// PublicationOptions explicitly permits a fenced/conflicted last-good cut for
// DNS alone. It never makes that cut eligible for auth, readiness or promotion.
type PublicationOptions struct {
	DNSOnly bool
}

// PublishIfCurrent holds the writer barrier while validating the exact token
// and running the publication callback. The callback must only check immutable
// dependency cursors and swap local pointers: no Store calls, I/O, compilation,
// hooks, goroutines or resource releases. An error must precede any pointer swap.
func (s *Store) PublishIfCurrent(ctx context.Context, t GenerationToken, opts PublicationOptions, publish func() error) error {
	if t.source != s || publish == nil {
		return replication.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return store.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.writeBlocked.Load() {
		return replication.ErrFenced
	}
	a := s.Authority()
	if !t.Matches(a) {
		return replication.ErrCAS
	}
	if t.DNSOnly() != opts.DNSOnly {
		return replication.ErrFenced
	}
	if !opts.DNSOnly && a.Mode != replication.Standalone && a.Mode != replication.Primary && a.Mode != replication.Follower {
		return replication.ErrReadOnly
	}
	return publish()
}

// ServingAuthority provides cheap local authority/fence observations for
// protected-read gates. It never reads SQLite. blocked includes a closed store
// or a failed durable fence, even before authority metadata is published.
func (s *Store) ServingAuthority() (a replication.Authority, blocked bool) {
	blocked = s.writeBlocked.Load() || s.isClosed()
	a = s.Authority()
	blocked = blocked || s.writeBlocked.Load() || s.isClosed()
	return a, blocked
}

// AuthorityChanges is coalesced. Subscribe BEFORE sampling Authority. Unlike
// GenerationChanges this also signals higher-term observations without images.
func (s *Store) AuthorityChanges() <-chan struct{} {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.authorityChanged
}
