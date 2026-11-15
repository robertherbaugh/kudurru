package validator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/store"
	"k8s.io/apimachinery/pkg/api/validate"
)

const storedValidationCapacity = 256

type storedValidationKey struct {
	kind                 store.Kind
	digest               [32]byte
	beta, allDeclarative bool
}

// Only success fingerprints are retained, never documents or decoded secrets.
// A cache belongs to one immutable compiled CRD registry, not a global schema.
type storedValidationCache struct {
	mu    sync.Mutex
	keys  map[storedValidationKey]struct{}
	order []storedValidationKey
	next  int
}

// ValidateStored checks the complete spec and status of an existing object on
// defensive copies. Unlike write admission, it neither applies defaults to the
// caller nor compares different old/new objects. Identical encoded objects have
// identical results under this immutable CRD schema, so successful checks can
// be reused within a fixed bound. Failed checks are never retained.
//
// This covers only CRD validation. Strict wire decoding, source metadata, image
// completeness/digests, private owners, current authority and time-dependent
// admission remain the consumer's responsibility on every use. Apply and
// UpdateStatus must continue to use their original admission methods.
func (v *Validator) ValidateStored(ctx context.Context, obj store.Object) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ks, err := v.kindFor(obj, nil)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("validator: encode %s: %w", ks.gvk.Kind, err)
	}
	key := storedValidationKey{kind: store.KindOf(obj), digest: sha256.Sum256(raw), beta: declarativeBeta(), allDeclarative: validate.AllDeclarativeEnforced(ctx)}
	c := &v.stored
	c.mu.Lock()
	_, known := c.keys[key]
	c.mu.Unlock()
	if known {
		return ctx.Err()
	}
	if err := v.Validate(ctx, obj.DeepCopyObject().(store.Object), nil); err != nil {
		return err
	}
	if ks.hasStatus {
		if err := v.ValidateStatus(ctx, obj.DeepCopyObject().(store.Object), obj.DeepCopyObject().(store.Object)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if key.beta != declarativeBeta() {
		return nil // never retain a result across a changed validation mode
	}
	c.mu.Lock()
	if _, present := c.keys[key]; !present {
		if c.keys == nil {
			c.keys = make(map[storedValidationKey]struct{}, storedValidationCapacity)
			c.order = make([]storedValidationKey, 0, storedValidationCapacity)
		}
		if len(c.order) < storedValidationCapacity {
			c.order = append(c.order, key)
		} else {
			delete(c.keys, c.order[c.next])
			c.order[c.next] = key
			c.next = (c.next + 1) % storedValidationCapacity
		}
		c.keys[key] = struct{}{}
	}
	c.mu.Unlock()
	return ctx.Err()
}
