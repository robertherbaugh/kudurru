// Package follower supplies a transport-independent mirror facade and pull
// loop. Authentication, membership discovery and HTTP live above this layer.
package follower

import (
	"context"
	"errors"
	"io"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Forwarder is the requested-write seam. A coordinator must attach primary
// term/principal/preconditions and command deduplication, never local fallback.
// Domain read-modify-write commands should be forwarded BEFORE using the
// facade; forwarding their final stale follower-computed Apply is insufficient.
type Forwarder interface {
	Apply(context.Context, store.Object, store.ApplyOptions) (store.Object, error)
	Delete(context.Context, store.Key, store.DeleteOptions) error
	UpdateStatus(context.Context, store.Object) error
}

// Store serves reads from the local mirror and forwards admitted writes.
type Store struct {
	mirror  *sqlite.Store
	forward Forwarder
}

// New wraps a mirror; a nil forwarder makes requested writes unavailable.
func New(mirror *sqlite.Store, forward Forwarder) *Store {
	return &Store{mirror: mirror, forward: forward}
}

var _ store.Store = (*Store)(nil)
var _ replication.Reader = (*Store)(nil)

// Get reads an object from the local mirror.
func (s *Store) Get(ctx context.Context, k store.Key, o store.Object) error {
	return s.mirror.Get(ctx, k, o)
}

// List reads matching objects from the local mirror.
func (s *Store) List(ctx context.Context, k store.Kind, o store.ListOptions) (store.ObjectList, error) {
	return s.mirror.List(ctx, k, o)
}

// Watch observes local mirror changes.
func (s *Store) Watch(ctx context.Context, k store.Kind, o store.WatchOptions) (store.Watcher, error) {
	return s.mirror.Watch(ctx, k, o)
}

// Snapshot exports the local mirror through the ordinary backup interface.
func (s *Store) Snapshot(ctx context.Context, w io.Writer, o store.SnapshotOptions) error {
	return s.mirror.Snapshot(ctx, w, o)
}

// Elector returns the local mirror elector.
func (s *Store) Elector(name string) store.Elector { return s.mirror.Elector(name) }

// Info reports mirror state with the follower driver name.
func (s *Store) Info() store.Info { info := s.mirror.Info(); info.Driver = "follower"; return info }

// Close closes the local mirror.
func (s *Store) Close() error { return s.mirror.Close() }

// ReadGeneration returns the complete local replicated image.
func (s *Store) ReadGeneration(ctx context.Context) (*replication.Image, error) {
	return s.mirror.ReadGeneration(ctx)
}

// GenerationChanges signals changes to the installed mirror generation.
func (s *Store) GenerationChanges() <-chan struct{} { return s.mirror.GenerationChanges() }

func (s *Store) canForward(ctx context.Context) error {
	if sqlite.HasWriterToken(ctx) {
		return replication.ErrFenced
	}
	if _, err := s.mirror.WriterToken(); errors.Is(err, store.ErrClosed) {
		return err
	}
	a := s.mirror.Authority()
	if s.mirror.Info().Fenced || a.Mode == replication.Conflict {
		return replication.ErrFenced
	}
	if a.Mode != replication.Follower || s.forward == nil {
		return replication.ErrReadOnly
	}
	return nil
}

// Apply writes locally when authoritative or forwards an admitted requested write.
func (s *Store) Apply(ctx context.Context, o store.Object, opts store.ApplyOptions) (store.Object, error) {
	if s.mirror.Authority().Writable() {
		return s.mirror.Apply(ctx, o, opts)
	}
	if err := s.canForward(ctx); err != nil {
		return nil, err
	}
	return s.forward.Apply(ctx, o, opts)
}

// Delete removes an object locally when authoritative or forwards the request.
func (s *Store) Delete(ctx context.Context, k store.Key, opts store.DeleteOptions) error {
	if s.mirror.Authority().Writable() {
		return s.mirror.Delete(ctx, k, opts)
	}
	if err := s.canForward(ctx); err != nil {
		return err
	}
	return s.forward.Delete(ctx, k, opts)
}

// UpdateStatus writes status locally when authoritative or forwards the request.
func (s *Store) UpdateStatus(ctx context.Context, o store.Object) error {
	if s.mirror.Authority().Writable() {
		return s.mirror.UpdateStatus(ctx, o)
	}
	if err := s.canForward(ctx); err != nil {
		return err
	}
	return s.forward.UpdateStatus(ctx, o)
}

// Restore remains backup-as-apply, not replication. Do not silently install
// untrusted backup documents through the privileged mirror lane.
func (s *Store) Restore(ctx context.Context, r io.Reader) error {
	if !s.mirror.Authority().Writable() {
		return replication.ErrReadOnly
	}
	return s.mirror.Restore(ctx, r)
}
