package zonestate

import (
	"encoding/json"
	"math"
	"sort"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PrepareNotify rearms an acknowledged active head without re-encoding its
// aggregate. Original immutable bytes, expiry, receipts and SOA serial survive
// unchanged. Already-pending requests must be coalesced by the caller instead.
func PrepareNotify(current *zk.ZoneState, get func(string) (*zk.ZoneChunk, error), limits Limits) (*Prepared, error) {
	if current == nil || get == nil {
		return nil, ErrInvalid
	}
	if len(current.Kind) > 512 || len(current.APIVersion) > 512 ||
		len(current.Spec.RootDigest) != 64 || len(current.Spec.AuthDigest) != 64 || len(current.Spec.ContentHash) != 64 {
		return nil, ErrCapacity
	}
	if current.Validate() != nil ||
		current.Spec.Lifecycle != zk.LifecycleActive || current.Spec.NotifySequence != 0 ||
		current.Spec.NotifySerial != 0 || current.Spec.CommitSequence == math.MaxUint64 {
		return nil, ErrInvalid
	}
	if err := notifyCarrierBounds(current.ObjectMeta); err != nil {
		return nil, err
	}
	limits = limits.Defaults()
	chunks := make(map[string]*zk.ZoneChunk)
	var total int64
	_, err := Decode(current, func(digest string) (*zk.ZoneChunk, error) {
		if c := chunks[digest]; c != nil {
			return c, nil
		}
		if len(chunks) >= limits.MaxChunks {
			return nil, ErrCapacity
		}
		c, err := get(digest)
		if err != nil {
			return nil, err
		}
		if c == nil || len(c.Spec.Digest) != 64 || c.Spec.Digest != digest {
			return nil, ErrInvalid
		}
		if len(c.Kind) > 512 || len(c.APIVersion) > 512 {
			return nil, ErrCapacity
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		total += int64(len(c.Spec.Data))
		if total > limits.MaxAggregateBytes+zk.MaxChunkBytes {
			return nil, ErrCapacity
		}
		if err := notifyCarrierBounds(c.ObjectMeta); err != nil {
			return nil, err
		}
		chunks[digest] = c.DeepCopy()
		return chunks[digest], nil
	}, limits)
	if err != nil {
		return nil, err
	}
	p := &Prepared{Head: current.DeepCopy()}
	p.Head.Spec.CommitSequence++
	p.Head.Spec.NotifySequence = p.Head.Spec.CommitSequence
	p.Head.Spec.NotifySerial = p.Head.Spec.Serial
	for _, c := range chunks {
		p.Chunks = append(p.Chunks, c)
	}
	sort.Slice(p.Chunks, func(i, j int) bool { return p.Chunks[i].Spec.Digest < p.Chunks[j].Spec.Digest })
	if !exactNotifyRearm(current, p.Head) {
		return nil, ErrInvalid
	}
	if _, err := VerifyPrepared(p, limits); err != nil {
		return nil, err
	}
	return p, nil
}

// Whole-spec equality is intentional: RootDigest binds config, expiry, source
// receipts and reservations; content/serial equality alone cannot pin them.
func exactNotifyRearm(current, next *zk.ZoneState) bool {
	if current.Spec.Lifecycle != zk.LifecycleActive || current.Spec.NotifySequence != 0 ||
		current.Spec.NotifySerial != 0 || current.Spec.CommitSequence == math.MaxUint64 {
		return false
	}
	want := current.Spec
	want.CommitSequence++
	want.NotifySequence = want.CommitSequence
	want.NotifySerial = want.Serial
	return next.Spec == want
}

// Bound arbitrary carrier metadata before copying or JSON encoding. Size is a
// no-allocation protobuf size calculation; JSON escaping is checked afterward.
func notifyCarrierBounds(m metav1.ObjectMeta) error {
	if len(m.Labels) > zk.MaxHeadBytes || len(m.Annotations) > zk.MaxHeadBytes ||
		len(m.Finalizers) > zk.MaxHeadBytes || len(m.OwnerReferences) > zk.MaxHeadBytes ||
		len(m.ManagedFields) > zk.MaxHeadBytes || m.Size() > zk.MaxHeadBytes {
		return ErrCapacity
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > zk.MaxHeadBytes {
		return ErrCapacity
	}
	return nil
}
