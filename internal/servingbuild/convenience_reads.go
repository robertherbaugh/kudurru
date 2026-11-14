package servingbuild

import (
	"context"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ObjectReadIdentity returns the original whole HTTP publication identity for
// convenience pagination, including dependency changes at the same generation.
func (r *ObjectReads) ObjectReadIdentity(ctx context.Context) (serving.Identity, error) {
	if r == nil || ctx == nil {
		return serving.Identity{}, serving.ErrComponents
	}
	return servinghttp.Identity(ctx)
}

// UnblockRequestsAllowed evaluates the original auth owner's finite entitlement
// at the caller's current clock. It never reads a live license or grants writes.
func (r *ObjectReads) UnblockRequestsAllowed(ctx context.Context, now time.Time) (bool, error) {
	if r == nil || ctx == nil {
		return false, serving.ErrComponents
	}
	id, err := r.ObjectReadIdentity(ctx)
	if err != nil {
		return false, err
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return false, err
	}
	if !view.Base().Same(id.Base) {
		return false, serving.ErrComponents
	}
	return view.UnblockRequestsAllowed(now)
}

// UnblockSnapshot returns detached workflow rows from the original complete
// runtime owner. UnblockRequest intentionally stays outside the DNS roster.
func (r *ObjectReads) UnblockSnapshot(ctx context.Context) ([]*public.UnblockRequest, replication.Generation, error) {
	id, err := r.ObjectReadIdentity(ctx)
	if err != nil {
		return nil, replication.Generation{}, err
	}
	owner, err := servinghttp.Component[*RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, replication.Generation{}, err
	}
	if owner.primary == nil || !owner.primary.Base().Same(id.Base) {
		return nil, replication.Generation{}, serving.ErrComponents
	}
	out := make([]*public.UnblockRequest, len(owner.unblockRequests))
	for i, request := range owner.unblockRequests {
		if ctx.Err() != nil {
			return nil, replication.Generation{}, ctx.Err()
		}
		out[i] = request.DeepCopy()
	}
	return out, id.Base, nil
}
