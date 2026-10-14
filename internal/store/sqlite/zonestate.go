package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

const zoneStateManager = "zonestate"

type zoneStateBackend struct {
	s      *Store
	limits zonestate.Limits
}

// ZoneStateBackend requires explicit scheme registration by the integration
// owner. It does not change shared driver defaults, snapshots or kind exposure.
func (s *Store) ZoneStateBackend(l zonestate.Limits) (zonestate.Backend, error) {
	for _, kind := range zonekinds.Kinds {
		if _, err := s.kinds.get(store.Kind(kind)); err != nil {
			return nil, fmt.Errorf("zone runtime kinds require zonekinds.AddToScheme: %w", err)
		}
	}
	return &zoneStateBackend{s: s, limits: l.Defaults()}, nil
}
func (b *zoneStateBackend) Head(ctx context.Context, id zonekinds.ZoneID) (*zonekinds.ZoneState, replication.Term, error) {
	term := b.s.Authority().Term
	var head zonekinds.ZoneState
	err := b.s.Get(ctx, store.Key{Kind: zonekinds.KindZoneState, Name: zonekinds.HeadName(id)}, &head)
	if err != nil {
		return nil, term, err
	}
	return &head, term, nil
}
func (b *zoneStateBackend) Chunk(ctx context.Context, digest string) (*zonekinds.ZoneChunk, error) {
	if !zonekinds.ValidDigest(digest) {
		return nil, zonestate.ErrInvalid
	}
	var c zonekinds.ZoneChunk
	err := b.s.Get(ctx, store.Key{Kind: zonekinds.KindZoneChunk, Name: zonekinds.ChunkName(digest)}, &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}
func (b *zoneStateBackend) Stage(ctx context.Context, c *zonekinds.ZoneChunk) error {
	if c == nil || c.Validate() != nil {
		return zonestate.ErrInvalid
	}
	return b.s.Command(ctx, b.s.Authority().Term, func(ctx context.Context, tx *CommandTx) error { return tx.stageZoneChunk(ctx, c) })
}
func (t *CommandTx) stageZoneChunk(ctx context.Context, c *zonekinds.ZoneChunk) error {
	var old zonekinds.ZoneChunk
	err := t.Get(ctx, store.KeyOf(c), &old)
	if err == nil {
		if old.Validate() != nil || old.Spec.Digest != c.Spec.Digest || !bytes.Equal(old.Spec.Data, c.Spec.Data) {
			return zonestate.ErrInvalid
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err = t.Apply(ctx, c, store.ApplyOptions{Manager: zoneStateManager})
	return err
}
func (b *zoneStateBackend) Commit(ctx context.Context, e zonestate.Expected, p *zonestate.Prepared, guard zonestate.Guard) (*zonekinds.ZoneState, error) {
	var out *zonekinds.ZoneState
	err := b.s.Command(ctx, e.Term, func(ctx context.Context, tx *CommandTx) error {
		var err error
		out, err = tx.CommitZoneState(ctx, e, p, b.limits, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CommitZoneState is the transaction-bound HA/domain integration seam. Root
// can validate current TSIG credentials using the same enclosing CommandTx
// before invoking it. Chunks, head/serial, receipt and notify intent share the
// transaction's audit, replication revision and writer-incarnation fence.
func (t *CommandTx) CommitZoneState(ctx context.Context, e zonestate.Expected, p *zonestate.Prepared, l zonestate.Limits, guard zonestate.Guard) (*zonekinds.ZoneState, error) {
	if err := t.valid(ctx); err != nil {
		return nil, err
	}
	if !t.Authority().Same(e.Term) || e.Zone.ClusterID != t.Authority().ClusterID {
		return nil, t.poison(replication.ErrCAS)
	}
	if _, err := zonestate.VerifyPrepared(p, l); err != nil {
		return nil, t.poison(err)
	}
	var current zonekinds.ZoneState
	err := t.Get(ctx, store.KeyOf(p.Head), &current)
	var old *zonekinds.ZoneState
	if err == nil {
		old = &current
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, t.poison(err)
	}
	if err := zonestate.CheckTransition(old, e, p.Head); err != nil {
		return nil, t.poison(err)
	}
	if guard != nil {
		if err := guard(ctx, old.DeepCopy()); err != nil {
			return nil, t.poison(err)
		}
	}
	for _, c := range p.Chunks {
		if err := t.stageZoneChunk(ctx, c); err != nil {
			return nil, t.poison(err)
		}
	}
	head := p.Head.DeepCopy()
	head.SetResourceVersion("")
	head.SetUID("")
	head.SetCreationTimestamp(current.CreationTimestamp)
	if old != nil {
		head.SetUID(old.UID)
	}
	out, err := t.Apply(ctx, head, store.ApplyOptions{Manager: zoneStateManager, ExpectedResourceVersion: e.ResourceVersion})
	if err != nil {
		return nil, t.poison(err)
	}
	return out.(*zonekinds.ZoneState), nil
}
