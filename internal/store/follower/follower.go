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

type Store struct {
	mirror  *sqlite.Store
	forward Forwarder
}

func New(mirror *sqlite.Store, forward Forwarder) *Store {
	return &Store{mirror: mirror, forward: forward}
}

var _ store.Store = (*Store)(nil)
var _ replication.Reader = (*Store)(nil)

func (s *Store) Get(ctx context.Context, k store.Key, o store.Object) error {
	return s.mirror.Get(ctx, k, o)
}
func (s *Store) List(ctx context.Context, k store.Kind, o store.ListOptions) (store.ObjectList, error) {
	return s.mirror.List(ctx, k, o)
}
func (s *Store) Watch(ctx context.Context, k store.Kind, o store.WatchOptions) (store.Watcher, error) {
	return s.mirror.Watch(ctx, k, o)
}
func (s *Store) Snapshot(ctx context.Context, w io.Writer, o store.SnapshotOptions) error {
	return s.mirror.Snapshot(ctx, w, o)
}
func (s *Store) Elector(name string) store.Elector { return s.mirror.Elector(name) }
func (s *Store) Info() store.Info                  { info := s.mirror.Info(); info.Driver = "follower"; return info }
func (s *Store) Close() error                      { return s.mirror.Close() }
func (s *Store) ReadGeneration(ctx context.Context) (*replication.Image, error) {
	return s.mirror.ReadGeneration(ctx)
}
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
func (s *Store) Apply(ctx context.Context, o store.Object, opts store.ApplyOptions) (store.Object, error) {
	if s.mirror.Authority().Writable() {
		return s.mirror.Apply(ctx, o, opts)
	}
	if err := s.canForward(ctx); err != nil {
		return nil, err
	}
	return s.forward.Apply(ctx, o, opts)
}
func (s *Store) Delete(ctx context.Context, k store.Key, opts store.DeleteOptions) error {
	if s.mirror.Authority().Writable() {
		return s.mirror.Delete(ctx, k, opts)
	}
	if err := s.canForward(ctx); err != nil {
		return err
	}
	return s.forward.Delete(ctx, k, opts)
}
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
