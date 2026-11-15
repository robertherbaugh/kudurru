package ledger

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
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

// PruneEventCursors deletes the event cursors of instances that no longer
// exist, by the exact UID and resource version the caller observed, in one
// Delete (D379). An instance deleted under the Retain policy leaves its
// cursor behind; no Integration can own it again, because a recreated one
// has a new UID. A cursor whose owner Integration still exists with that UID
// is skipped; the guard's own cursor, or one that changed or vanished,
// refuses the whole call.
func (r *Repository) PruneEventCursors(ctx context.Context, g Guard, cursors []*kinds.IntegrationEventCursor) error {
	if r.delete == nil || g.Validate() != nil || len(cursors) == 0 || len(cursors) > MaxPruneDeliveries {
		return ErrInvalid
	}
	seen := map[string]bool{}
	requests := make([]store.DeleteRequest, 0, len(cursors))
	for _, c := range cursors {
		if c == nil || c.Validate() != nil || c.UID == "" || c.ResourceVersion == "" || seen[c.Name] || c.Spec.Owner == g.Owner {
			return ErrInvalid
		}
		seen[c.Name] = true
		current, err := r.ReadEventCursor(ctx, c.Spec.Owner)
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrConflict
		}
		if err != nil {
			return err
		}
		if current.UID != c.UID || current.ResourceVersion != c.ResourceVersion {
			return store.ErrConflict
		}
		var owner public.Integration
		err = r.read.Get(ctx, store.Key{Kind: "Integration", Namespace: current.Spec.Owner.Namespace, Name: current.Spec.Owner.Name}, &owner)
		switch {
		case err == nil && string(owner.UID) == current.Spec.Owner.UID:
			continue
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return err
		}
		requests = append(requests, forgetRequest(kinds.KindIntegrationEventCursor, current))
	}
	if len(requests) == 0 {
		return nil
	}
	slices.SortFunc(requests, func(a, b store.DeleteRequest) int { return strings.Compare(a.Key.Name, b.Key.Name) })
	return r.delete(ctx, g, requests)
}
