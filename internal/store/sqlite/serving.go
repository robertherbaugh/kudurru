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

// AdvancedTo reports that a is this token's own writable authority with only
// its installed revision moved forward, or unchanged: every other field is
// compared exactly. It reads no database and grants nothing. A request that
// has committed uses it to tell that the node still writes in the same
// authority cut it began on; who committed in between is a separate proof.
func (t GenerationToken) AdvancedTo(a replication.Authority) bool {
	original := t.authority
	if t.source == nil || t.DNSOnly() || !original.Writable() || !a.Installed.Term.Same(original.Installed.Term) || a.Installed.Revision < original.Installed.Revision {
		return false
	}
	original.Installed.Revision = a.Installed.Revision
	return reflect.DeepEqual(original, a)
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
	image, _, token, err := s.readServingCandidate(ctx, false)
	return image, token, err
}

// ReadValidatedServingCandidate retains the complete local validation result
// from the same read transaction as the publication token. Consumers can derive
// their own projections from its defensive copies without repeating Store
// validation. It does not grant publication or command authority.
func (s *Store) ReadValidatedServingCandidate(ctx context.Context) (*replication.Validated, GenerationToken, error) {
	_, validated, token, err := s.readServingCandidate(ctx, true)
	return validated, token, err
}

func (s *Store) readServingCandidate(ctx context.Context, requireValidation bool) (*replication.Image, *replication.Validated, GenerationToken, error) {
	if s.isClosed() {
		return nil, nil, GenerationToken{}, store.ErrClosed
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, GenerationToken{}, err
	}
	defer func() { _ = tx.Rollback() }()
	image, err := s.readGeneration(ctx, tx)
	if err != nil {
		return nil, nil, GenerationToken{}, err
	}
	var validated *replication.Validated
	if requireValidation || s.opts.ValidateReplicaImage != nil {
		validated, err = s.ValidateImage(ctx, *image)
		if err != nil {
			return nil, nil, GenerationToken{}, err
		}
	}
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return nil, nil, GenerationToken{}, err
	}
	return image, validated, GenerationToken{source: s, generation: image.Generation(), authority: a}, nil
}

// PublicationOptions explicitly permits a fenced/conflicted last-good cut for
// DNS alone. It never makes that cut eligible for auth, readiness or promotion.
type PublicationOptions struct {
	DNSOnly bool
	// Anchor retains the original committed managed-root publication attempt.
	// It supplies mechanical source evidence, never a complete-bundle seal.
	Anchor *AnchorPublication
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
	if opts.Anchor != nil {
		return s.publishAnchorIfCurrent(ctx, t, opts, publish)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.publishIfCurrentLocked(ctx, t, opts, publish)
}

// publishIfCurrentLocked preserves the ordinary publication checks. The anchor
// lane calls it only after taking R(write) before wmu; it never reenters Store.
func (s *Store) publishIfCurrentLocked(ctx context.Context, t GenerationToken, opts PublicationOptions, publish func() error) error {
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

// DNSServingAuthority is ServingAuthority for the answers a node keeps giving
// from the publication it serves: DNS output and the root anchor inputs that
// admit it (D369). It differs only while an observed fence, conflict or
// divergence of an unblocked store is being persisted (fenceInFlight): writers
// are already blocked and every elected term is retired, but the member keeps
// answering DNS from the same last-good data and accepted managed trust that
// its DNS-only role will serve once the new authority is published. A fence
// whose persistence failed keeps the store blocked, and with it DNS, as before.
// Protected reads, publication and every writer use ServingAuthority.
func (s *Store) DNSServingAuthority() (a replication.Authority, blocked bool) {
	blocked = s.dnsOutputBlocked()
	a = s.Authority()
	blocked = blocked || s.dnsOutputBlocked()
	return a, blocked
}

// dnsOutputBlocked is the DNS form of the write block (DNSServingAuthority).
// writeBlocked is read first: an attempt sets fenceInFlight before it blocks
// and clears it only after it published its outcome, so a failed fence is
// observed as blocked by every later read.
func (s *Store) dnsOutputBlocked() bool {
	blocked := s.writeBlocked.Load()
	return s.isClosed() || (blocked && !s.fenceInFlight.Load())
}

// AuthorityChanges is coalesced. Subscribe BEFORE sampling Authority. Unlike
// GenerationChanges this also signals higher-term observations without images.
func (s *Store) AuthorityChanges() <-chan struct{} {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.authorityChanged
}
