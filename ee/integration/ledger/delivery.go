package ledger

import (
	"context"
	"errors"
	"reflect"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// ReadDelivery returns one validated, private copy with its exact durable RV.
func (r *Repository) ReadDelivery(ctx context.Context, owner kinds.Owner, id string) (*kinds.IntegrationDelivery, error) {
	if owner.Validate() != nil || !kinds.ValidDigest(id) {
		return nil, ErrInvalid
	}
	d := &kinds.IntegrationDelivery{}
	if err := r.read.Get(ctx, store.Key{Kind: kinds.KindIntegrationDelivery, Name: kinds.DeliveryName(owner, id)}, d); err != nil {
		return nil, err
	}
	if d.Validate() != nil || d.ResourceVersion == "" || d.Spec.Owner != owner || d.Spec.DeliveryID != id {
		return nil, ErrInvalid
	}
	return d.DeepCopy(), nil
}
func (r *Repository) putDelivery(ctx context.Context, g Guard, d *kinds.IntegrationDelivery, create bool) (*kinds.IntegrationDelivery, error) {
	if g.Validate() != nil || d == nil || d.Validate() != nil || d.Spec.Owner != g.Owner || !create && d.ResourceVersion == "" {
		return nil, ErrInvalid
	}
	saved, err := r.write(ctx, g, d.DeepCopy(), store.ApplyOptions{Manager: manager, CreateOnly: create, ExpectedResourceVersion: d.ResourceVersion})
	if err != nil {
		return nil, err
	}
	out, ok := saved.(*kinds.IntegrationDelivery)
	if !ok || out.Validate() != nil || out.ResourceVersion == "" || !reflect.DeepEqual(out.Spec, d.Spec) {
		return nil, ErrInvalid
	}
	return out.DeepCopy(), nil
}

// QueueDelivery persists immutable sanitized input before any external callback.
// A stable ID is idempotent even after ack; different content cannot reuse it.
func (r *Repository) QueueDelivery(ctx context.Context, g Guard, d *kinds.IntegrationDelivery) (*kinds.IntegrationDelivery, error) {
	if d == nil || d.Validate() != nil || d.ResourceVersion != "" || d.Spec.Attempts != 0 || d.Spec.Acknowledged || d.Spec.DeadLetter || d.Spec.ErrorClass != "" || g.Owner != d.Spec.Owner || g.Validate() != nil {
		return nil, ErrInvalid
	}
	old, err := r.ReadDelivery(ctx, g.Owner, d.Spec.DeliveryID)
	if errors.Is(err, store.ErrNotFound) {
		return r.putDelivery(ctx, g, d, true)
	}
	if err != nil {
		return nil, err
	}
	a, b := old.Spec, d.Spec
	a.Attempts, b.Attempts = 0, 0
	a.Acknowledged, b.Acknowledged = false, false
	a.DeadLetter, b.DeadLetter = false, false
	a.ErrorClass, b.ErrorClass = "", ""
	a.NextAttempt, b.NextAttempt = a.CreatedAt, b.CreatedAt
	if !reflect.DeepEqual(a, b) {
		return nil, ErrInvalid
	}
	return old, nil
}

// SaveDelivery advances an exact token once without regressing acknowledged,
// terminal or attempted outcomes; stale callers never acquire a fresh token.
func (r *Repository) SaveDelivery(ctx context.Context, g Guard, next *kinds.IntegrationDelivery) (*kinds.IntegrationDelivery, error) {
	if next == nil || next.Validate() != nil || next.ResourceVersion == "" || next.Spec.Owner != g.Owner || g.Validate() != nil {
		return nil, ErrInvalid
	}
	old, err := r.ReadDelivery(ctx, g.Owner, next.Spec.DeliveryID)
	if err != nil {
		return nil, err
	}
	if old.ResourceVersion != next.ResourceVersion {
		return nil, store.ErrConflict
	}
	a, b := old.Spec, next.Spec
	attempts := a.Attempts
	terminal := a.Acknowledged || a.DeadLetter
	a.Attempts, b.Attempts = 0, 0
	a.Acknowledged, b.Acknowledged = false, false
	a.DeadLetter, b.DeadLetter = false, false
	a.ErrorClass, b.ErrorClass = "", ""
	a.NextAttempt, b.NextAttempt = a.CreatedAt, b.CreatedAt
	if !reflect.DeepEqual(a, b) || terminal && !reflect.DeepEqual(old.Spec, next.Spec) || next.Spec.Attempts < attempts || next.Spec.Attempts > attempts+1 || next.Spec.NextAttempt.Before(old.Spec.NextAttempt) {
		return nil, ErrInvalid
	}
	return r.putDelivery(ctx, g, next, false)
}
