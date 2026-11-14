package kube

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
)

// Integration source errors (D384). ErrSourceConflict wraps store.ErrConflict:
// the head moved, or a create found one, and nothing was written. A stored
// carrier that does not match what a head references is ErrSourceCorrupt,
// never an absent source. ErrSourceOutcomeUnknown marks a head write that was
// sent and not acknowledged: the head may or may not have moved, and only a
// later read of the head can tell.
var (
	ErrSourceConflict       = fmt.Errorf("%w: integration source head changed", store.ErrConflict)
	ErrSourceOutcomeUnknown = errors.New("store/kube: integration source head write outcome unknown")
	ErrSourceCorrupt        = errors.New("store/kube: integration source carrier does not match its head")
	ErrSourceInvalid        = errors.New("store/kube: invalid integration source")
	ErrSourceCapacity       = errors.New("store/kube: integration source carriers exceed their bound")
)

// IntegrationSourceBackend is the Kubernetes form of the integration source
// root (D384). Chunks are immutable Secrets created before any head refers
// to them; one IntegrationSourceHead per Integration UID is the only commit
// point, replaced by exactly one create or one exact-resourceVersion update
// and never retried. A lost head CAS leaves the previous source in force; its
// staged chunks are unreferenced garbage, not a partial source.
type IntegrationSourceBackend struct{ s *Store }

// IntegrationSourceBackend requires every source carrier to be a private kind.
func (s *Store) IntegrationSourceBackend() (*IntegrationSourceBackend, error) {
	for _, kind := range integration.SourceKinds {
		if _, ok := s.internal.get(store.Kind(kind)); !ok {
			return nil, fmt.Errorf("%s requires integrationkinds.AddToScheme and Options.InternalKinds", kind)
		}
	}
	return &IntegrationSourceBackend{s: s}, nil
}

// Stage creates one immutable chunk carrier. A chunk that already exists with
// the identical validated spec is success, so a retried pass can reuse what a
// lost pass staged; any other existing carrier under the name refuses.
func (b *IntegrationSourceBackend) Stage(ctx context.Context, c *integration.IntegrationSourceChunk, manager string) (*integration.IntegrationSourceChunk, error) {
	if c == nil || c.Validate() != nil || manager == "" {
		return nil, ErrSourceInvalid
	}
	ki, _ := b.s.internal.get(integration.KindIntegrationSourceChunk)
	cp := c.DeepCopy()
	cp.ResourceVersion, cp.UID = "", ""
	store.SetManagedBy(cp, manager)
	cp.SetGeneration(1)
	data, err := encodeObject(ki, cp)
	if err != nil {
		return nil, err
	}
	if len(data) > integration.MaxObjectBytes || len(data) > corev1.MaxSecretSize {
		return nil, ErrSourceCapacity
	}
	sec := b.s.internalSecret(ki, cp, data)
	sec.Immutable = ptr.To(true)
	if err := b.s.c.Create(ctx, sec); err == nil {
		obj, err := decodeSecret(ki, sec)
		if err != nil {
			return nil, err
		}
		return obj.(*integration.IntegrationSourceChunk), nil
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, writeError("stage integration source chunk", store.KeyOf(c), err)
	}
	old, err := b.chunk(ctx, c.Name)
	if err != nil {
		return nil, err
	}
	if !sameChunk(old, c) {
		return nil, ErrSourceCorrupt
	}
	return old, nil
}

func sameChunk(stored, want *integration.IntegrationSourceChunk) bool {
	return stored != nil && stored.Name == want.Name && stored.Namespace == "" && reflect.DeepEqual(stored.Spec, want.Spec)
}

func (b *IntegrationSourceBackend) chunk(ctx context.Context, name string) (*integration.IntegrationSourceChunk, error) {
	var c integration.IntegrationSourceChunk
	if err := b.s.Get(ctx, store.Key{Kind: integration.KindIntegrationSourceChunk, Name: name}, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Head returns the committed head of owner, store.ErrNotFound when there is
// none, or an error for a carrier that does not decode and validate.
func (b *IntegrationSourceBackend) Head(ctx context.Context, owner integration.Owner) (*integration.IntegrationSourceHead, error) {
	var h integration.IntegrationSourceHead
	if err := b.s.Get(ctx, store.Key{Kind: integration.KindIntegrationSourceHead, Name: integration.SourceHeadName(owner)}, &h); err != nil {
		return nil, err
	}
	if h.Spec.Owner != owner {
		return nil, ErrSourceCorrupt
	}
	return &h, nil
}

// Commit installs head as the source of its owner. expected is the
// resourceVersion of the head the caller read, "" when it read none. Every
// chunk the head references must be passed and must already be durable with
// identical bytes. The previous head, if any, must belong to the same owner
// and carry the preceding sequence; a first head carries sequence 1. Exactly
// one create or one update is sent. ErrSourceConflict means that write lost
// and nothing changed. An error wrapping ErrSourceOutcomeUnknown means the
// write was sent and not acknowledged: the caller resolves it by reading the
// head again, never by repeating the write. Every other error precedes the
// write and changed nothing.
func (b *IntegrationSourceBackend) Commit(ctx context.Context, expected string, head *integration.IntegrationSourceHead, chunks []*integration.IntegrationSourceChunk, manager string) (*integration.IntegrationSourceHead, error) {
	if head == nil || head.Validate() != nil || len(chunks) != len(head.Spec.Chunks) || manager == "" {
		return nil, ErrSourceInvalid
	}
	for i, ref := range head.Spec.Chunks {
		c := chunks[i]
		if c == nil || c.Validate() != nil || c.Name != ref.Name || c.Spec.Digest != ref.Digest || len(c.Spec.Data) != ref.Bytes || c.Spec.Owner != head.Spec.Owner || c.Spec.Sequence != head.Spec.Sequence || c.Spec.Index != i {
			return nil, ErrSourceInvalid
		}
	}
	ki, _ := b.s.internal.get(integration.KindIntegrationSourceHead)
	sec, err := b.s.getSecret(ctx, ki, store.KeyOf(head))
	if err != nil {
		return nil, err
	}
	switch {
	case sec == nil && expected != "", sec != nil && expected == "", sec != nil && sec.ResourceVersion != expected:
		return nil, ErrSourceConflict
	case sec == nil && head.Spec.Sequence != 1:
		return nil, ErrSourceInvalid
	case sec != nil:
		obj, err := decodeSecret(ki, sec)
		if err != nil {
			return nil, err
		}
		previous := obj.(*integration.IntegrationSourceHead)
		if previous.Spec.Owner != head.Spec.Owner || previous.Spec.Sequence == math.MaxUint64 || head.Spec.Sequence != previous.Spec.Sequence+1 {
			return nil, ErrSourceInvalid
		}
		if owner := store.ManagedBy(previous); owner != "" && owner != manager {
			return nil, &store.ErrManagedBy{Key: store.KeyOf(head), Owner: owner}
		}
	}
	// Before installing a root, recheck every reference is durably present.
	for _, c := range chunks {
		stored, err := b.chunk(ctx, c.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrSourceCorrupt
		}
		if err != nil {
			return nil, err
		}
		if !sameChunk(stored, c) {
			return nil, ErrSourceCorrupt
		}
	}
	next := head.DeepCopy()
	next.ResourceVersion, next.UID = "", ""
	store.SetManagedBy(next, manager)
	data, err := encodeObject(ki, next)
	if err != nil {
		return nil, err
	}
	if len(data) > integration.MaxObjectBytes || len(data) > corev1.MaxSecretSize {
		return nil, ErrSourceCapacity
	}
	out := b.s.internalSecret(ki, next, data)
	if sec == nil {
		err = b.s.c.Create(ctx, out)
	} else {
		out.ResourceVersion = expected
		out.UID = sec.UID
		err = b.s.c.Update(ctx, out)
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return nil, ErrSourceConflict
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceOutcomeUnknown, writeError("commit integration source head", store.KeyOf(head), err))
	}
	obj, err := decodeSecret(ki, out)
	if err != nil {
		// The write was acknowledged, but its answer does not decode.
		return nil, fmt.Errorf("%w: %w", ErrSourceOutcomeUnknown, err)
	}
	return obj.(*integration.IntegrationSourceHead), nil
}

// Read returns the committed head of owner with every chunk it references,
// in order. A missing or different chunk is ErrSourceCorrupt.
func (b *IntegrationSourceBackend) Read(ctx context.Context, owner integration.Owner) (*integration.IntegrationSourceHead, []*integration.IntegrationSourceChunk, error) {
	head, err := b.Head(ctx, owner)
	if err != nil {
		return nil, nil, err
	}
	chunks := make([]*integration.IntegrationSourceChunk, 0, len(head.Spec.Chunks))
	for i, ref := range head.Spec.Chunks {
		c, err := b.chunk(ctx, ref.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, ErrSourceCorrupt
		}
		if err != nil {
			return nil, nil, err
		}
		if c.Spec.Owner != owner || c.Spec.Sequence != head.Spec.Sequence || c.Spec.Index != i || c.Spec.Digest != ref.Digest || len(c.Spec.Data) != ref.Bytes {
			return nil, nil, ErrSourceCorrupt
		}
		chunks = append(chunks, c)
	}
	return head, chunks, nil
}

// Heads lists every committed head, at most integration.MaxSourceCount.
func (b *IntegrationSourceBackend) Heads(ctx context.Context) ([]*integration.IntegrationSourceHead, error) {
	items, err := b.list(ctx, integration.KindIntegrationSourceHead, integration.MaxSourceCount)
	if err != nil {
		return nil, err
	}
	out := make([]*integration.IntegrationSourceHead, 0, len(items))
	for _, item := range items {
		out = append(out, item.(*integration.IntegrationSourceHead))
	}
	return out, nil
}

// Allocations lists every name allocation, at most
// integration.MaxClusterAllocations.
func (b *IntegrationSourceBackend) Allocations(ctx context.Context) ([]*integration.IntegrationNameAllocation, error) {
	items, err := b.list(ctx, integration.KindIntegrationNameAllocation, integration.MaxClusterAllocations)
	if err != nil {
		return nil, err
	}
	out := make([]*integration.IntegrationNameAllocation, 0, len(items))
	for _, item := range items {
		out = append(out, item.(*integration.IntegrationNameAllocation))
	}
	return out, nil
}

// list pages through kind and refuses more than limit items. Undecodable
// carriers fail the list (PrivateCarrierLimit), so a short list is complete.
func (b *IntegrationSourceBackend) list(ctx context.Context, kind string, limit int) ([]store.Object, error) {
	var out []store.Object
	cont := ""
	for {
		page, err := b.s.List(ctx, store.Kind(kind), store.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if len(out) > limit {
			return nil, ErrSourceCapacity
		}
		if cont = page.Continue; cont == "" {
			return out, nil
		}
	}
}

// DeleteChunk removes one chunk carrier by the exact UID and resourceVersion
// the caller read (D302): a successor carrier under the same name is never
// removed. It is how a writer retires the chunks of a head it replaced.
func (b *IntegrationSourceBackend) DeleteChunk(ctx context.Context, c *integration.IntegrationSourceChunk, manager string) error {
	if c == nil || c.UID == "" || c.ResourceVersion == "" || manager == "" {
		return ErrSourceInvalid
	}
	return b.s.Delete(ctx, store.KeyOf(c), store.DeleteOptions{Manager: manager, ExpectedUID: string(c.UID), ExpectedResourceVersion: c.ResourceVersion})
}
