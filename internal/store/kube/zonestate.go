package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
)

const zoneStateManager = "zonestate"

type zoneStateBackend struct {
	s      *Store
	limits zonestate.Limits
}

// ZoneStateBackend requires both runtime kinds in the scheme and the private
// Secret mapping. Safe-export classification is separate, root-owned wiring.
func (s *Store) ZoneStateBackend(l zonestate.Limits) (zonestate.Backend, error) {
	for _, kind := range zonekinds.Kinds {
		if _, ok := s.internal.get(store.Kind(kind)); !ok {
			return nil, fmt.Errorf("%s requires zonekinds.AddToScheme and Options.InternalKinds", kind)
		}
	}
	return &zoneStateBackend{s: s, limits: l.Defaults()}, nil
}
func (b *zoneStateBackend) Head(ctx context.Context, id zonekinds.ZoneID) (*zonekinds.ZoneState, replication.Term, error) {
	var head zonekinds.ZoneState
	err := b.s.Get(ctx, store.Key{Kind: zonekinds.KindZoneState, Name: zonekinds.HeadName(id)}, &head)
	if err != nil {
		return nil, replication.Term{}, err
	}
	return &head, replication.Term{}, nil
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
	ki, _ := b.s.internal.get(zonekinds.KindZoneChunk)
	cp := c.DeepCopy()
	store.SetManagedBy(cp, zoneStateManager)
	data, err := encodeObject(ki, cp)
	if err != nil {
		return err
	}
	if len(data) > corev1.MaxSecretSize {
		return zonestate.ErrCapacity
	}
	sec := b.s.internalSecret(ki, cp, data)
	sec.Immutable = ptr.To(true)
	if err := b.s.c.Create(ctx, sec); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return writeError("stage zone chunk", store.KeyOf(c), err)
	}
	old, err := b.Chunk(ctx, c.Spec.Digest)
	if err != nil {
		return err
	}
	if old.Validate() != nil || old.Spec.Digest != c.Spec.Digest || !bytes.Equal(old.Spec.Data, c.Spec.Data) {
		return zonestate.ErrInvalid
	}
	return nil
}
func (b *zoneStateBackend) Commit(ctx context.Context, e zonestate.Expected, p *zonestate.Prepared, guard zonestate.Guard) (*zonekinds.ZoneState, error) {
	if _, err := zonestate.VerifyPrepared(p, b.limits); err != nil {
		return nil, err
	}
	ki, _ := b.s.internal.get(zonekinds.KindZoneState)
	sec, err := b.s.getSecret(ctx, ki, store.KeyOf(p.Head))
	if err != nil {
		return nil, err
	}
	var old *zonekinds.ZoneState
	if sec != nil {
		obj, err := decodeSecret(ki, sec)
		if err != nil {
			return nil, err
		}
		old = obj.(*zonekinds.ZoneState)
	}
	if err := zonestate.CheckTransition(old, e, p.Head); err != nil {
		return nil, err
	}
	if guard != nil {
		if err := guard(ctx, old.DeepCopy()); err != nil {
			return nil, err
		}
	}
	// Before installing a root, recheck every reference is durably present.
	// Future GC must protect staged candidates and revalidate current roots.
	for _, c := range p.Chunks {
		stored, err := b.Chunk(ctx, c.Spec.Digest)
		if err != nil {
			return nil, err
		}
		if stored.Validate() != nil || !bytes.Equal(stored.Spec.Data, c.Spec.Data) {
			return nil, zonestate.ErrInvalid
		}
	}
	head := p.Head.DeepCopy()
	store.SetManagedBy(head, zoneStateManager)
	data, err := encodeObject(ki, head)
	if err != nil {
		return nil, err
	}
	if len(data) > zonekinds.MaxHeadBytes || len(data) > corev1.MaxSecretSize {
		return nil, zonestate.ErrCapacity
	}
	next := b.s.internalSecret(ki, head, data)
	if sec == nil {
		err = b.s.c.Create(ctx, next)
	} else {
		next.ResourceVersion = e.ResourceVersion
		next.UID = sec.UID
		err = b.s.c.Update(ctx, next)
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return nil, zonestate.ErrConflict
	}
	if err != nil {
		return nil, writeError("commit zone head", store.KeyOf(head), err)
	}
	var result zonekinds.ZoneState
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	result.ResourceVersion = next.ResourceVersion
	result.UID = next.UID
	result.CreationTimestamp = next.CreationTimestamp
	return &result, nil
}
