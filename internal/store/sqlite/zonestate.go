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
	if old == nil || old.Spec.RootDigest != head.Spec.RootDigest {
		if err := t.collectZoneChunks(ctx, l); err != nil {
			return nil, t.poison(err)
		}
	}
	return out.(*zonekinds.ZoneState), nil
}

// collectZoneChunks deletes, in the commit that moved a head to new content,
// every ZoneChunk that no ZoneState head references any more (D405). Chunks
// are immutable and content-addressed and only heads resolve them, so a chunk
// that no head names is unreachable: before this, every content commit (each
// signed UPDATE) left the previous content's chunks behind, and the
// replicated image, with the time to read, check and prepare it for
// publication, grew with every commit. Every head is marked, including
// tombstoned ones, and the sweep also removes chunks that an earlier version
// left behind. A head whose root manifest cannot be read references an
// unknown set: nothing is deleted then, the skip is logged as a warning, and
// the commit is not refused for it.
func (t *CommandTx) collectZoneChunks(ctx context.Context, l zonestate.Limits) error {
	heads, err := t.List(ctx, zonekinds.KindZoneState, store.ListOptions{})
	if err != nil {
		return err
	}
	referenced := map[string]bool{}
	get := func(digest string) (*zonekinds.ZoneChunk, error) {
		var c zonekinds.ZoneChunk
		if err := t.Get(ctx, store.Key{Kind: zonekinds.KindZoneChunk, Name: zonekinds.ChunkName(digest)}, &c); err != nil {
			return nil, err
		}
		return &c, nil
	}
	for _, item := range heads.Items {
		head, ok := item.(*zonekinds.ZoneState)
		if !ok {
			return nil
		}
		refs, err := zonestate.References(head, get, l)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Until that head is repaired or deleted, no commit collects
			// chunks, and the stored zone content grows with every commit
			// again: say so on every skipped sweep instead of silently.
			t.s.log.Warn("zone chunk collection skipped: a zone head's chunks cannot be resolved", "head", head.Name, "err", err)
			return nil
		}
		for _, digest := range refs {
			referenced[zonekinds.ChunkName(digest)] = true
		}
	}
	rows, err := t.tx.QueryContext(ctx, `SELECT name FROM objects WHERE kind = ? AND namespace = ''`, zonekinds.KindZoneChunk)
	if err != nil {
		return fmt.Errorf("store/sqlite: list zone chunks: %w", err)
	}
	var unreferenced []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		if !referenced[name] {
			unreferenced = append(unreferenced, name)
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, name := range unreferenced {
		// Only zone state writes chunks; Force keeps a chunk that an import
		// recorded under another manager from refusing this commit.
		if err := t.Delete(ctx, store.Key{Kind: zonekinds.KindZoneChunk, Name: name}, store.DeleteOptions{Manager: zoneStateManager, Force: true}); err != nil {
			return err
		}
	}
	return nil
}
