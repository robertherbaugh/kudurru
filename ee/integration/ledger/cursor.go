package ledger

import (
	"context"
	"errors"
	"reflect"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// ReadEventCursor returns the validated event cursor of one instance with
// its exact durable RV, or store.ErrNotFound before its first production.
func (r *Repository) ReadEventCursor(ctx context.Context, owner kinds.Owner) (*kinds.IntegrationEventCursor, error) {
	if owner.Validate() != nil {
		return nil, ErrInvalid
	}
	c := &kinds.IntegrationEventCursor{}
	if err := r.read.Get(ctx, store.Key{Kind: kinds.KindIntegrationEventCursor, Name: kinds.EventCursorName(owner)}, c); err != nil {
		return nil, err
	}
	if c.Validate() != nil || c.ResourceVersion == "" || c.Spec.Owner != owner {
		return nil, ErrInvalid
	}
	return c.DeepCopy(), nil
}

// SaveEventCursor creates the cursor (empty resourceVersion) or advances it
// by exact compare-and-swap. A stale caller never acquires a fresh version,
// so two productions of one instance cannot both advance it (D379).
func (r *Repository) SaveEventCursor(ctx context.Context, g Guard, next *kinds.IntegrationEventCursor) (*kinds.IntegrationEventCursor, error) {
	if next == nil || next.Validate() != nil || next.Spec.Owner != g.Owner || g.Validate() != nil {
		return nil, ErrInvalid
	}
	create := next.ResourceVersion == ""
	if !create {
		old, err := r.ReadEventCursor(ctx, g.Owner)
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.ErrConflict
		}
		if err != nil {
			return nil, err
		}
		if old.ResourceVersion != next.ResourceVersion || old.UID != next.UID {
			return nil, store.ErrConflict
		}
		if next.Spec.UpdatedAt.Before(old.Spec.UpdatedAt) {
			return nil, ErrInvalid
		}
	}
	saved, err := r.write(ctx, g, next.DeepCopy(), store.ApplyOptions{Manager: manager, CreateOnly: create, ExpectedResourceVersion: next.ResourceVersion})
	if err != nil {
		return nil, err
	}
	out, ok := saved.(*kinds.IntegrationEventCursor)
	if !ok || out.Validate() != nil || out.ResourceVersion == "" || !reflect.DeepEqual(out.Spec, next.Spec) {
		return nil, ErrInvalid
	}
	return out.DeepCopy(), nil
}
