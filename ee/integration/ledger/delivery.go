package ledger

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
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

// DeliveryRetention is how long a delivery row outlives its deadline. Past
// the deadline no queue accepts its event again, so deleting the row cannot
// turn into a second delivery of the same ID.
const DeliveryRetention = time.Hour

// MaxPruneDeliveries bounds one PruneDeliveries call.
const MaxPruneDeliveries = 256

// PruneDeliveries deletes delivery rows by the exact UID and resource version
// the caller observed, in one Delete (D379). Every row must be past its
// deadline plus DeliveryRetention. A row of the guard's own instance must be
// terminal (acknowledged or dead-lettered); a row of any other instance is
// pruned only when no Integration with that owner UID exists any more, and is
// skipped while one does. A row that changed or vanished, or an own row that
// does not qualify, refuses the whole call.
func (r *Repository) PruneDeliveries(ctx context.Context, g Guard, rows []*kinds.IntegrationDelivery) error {
	if r.delete == nil || g.Validate() != nil || len(rows) == 0 || len(rows) > MaxPruneDeliveries {
		return ErrInvalid
	}
	now := r.now()
	seen := map[string]bool{}
	requests := make([]store.DeleteRequest, 0, len(rows))
	for _, d := range rows {
		if d == nil || d.Validate() != nil || d.UID == "" || d.ResourceVersion == "" || seen[d.Name] {
			return ErrInvalid
		}
		seen[d.Name] = true
		current, err := r.ReadDelivery(ctx, d.Spec.Owner, d.Spec.DeliveryID)
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrConflict
		}
		if err != nil {
			return err
		}
		if current.UID != d.UID || current.ResourceVersion != d.ResourceVersion {
			return store.ErrConflict
		}
		if now.Before(current.Spec.Deadline.Add(DeliveryRetention)) {
			return ErrInvalid
		}
		if current.Spec.Owner == g.Owner {
			if !current.Spec.Acknowledged && !current.Spec.DeadLetter {
				return ErrInvalid
			}
		} else {
			var owner public.Integration
			err := r.read.Get(ctx, store.Key{Kind: "Integration", Namespace: current.Spec.Owner.Namespace, Name: current.Spec.Owner.Name}, &owner)
			switch {
			case err == nil && string(owner.UID) == current.Spec.Owner.UID:
				continue
			case err != nil && !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		requests = append(requests, forgetRequest(kinds.KindIntegrationDelivery, current))
	}
	if len(requests) == 0 {
		return nil
	}
	slices.SortFunc(requests, func(a, b store.DeleteRequest) int { return strings.Compare(a.Key.Name, b.Key.Name) })
	return r.delete(ctx, g, requests)
}
