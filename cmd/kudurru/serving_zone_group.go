package main

import (
	"context"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// capturedZoneGroup states the zone group of a zone attribute from this
// request's captured publication, the same cut its View authorizes against
// (D385). It is the follower's early admission only: the primary's command
// transaction resolves the group again from its own read and discards this
// value. A zone that the cut does not hold is in no group.
func capturedZoneGroup(ctx context.Context, attrs rbac.Attributes) rbac.Attributes {
	attrs.ZoneGroup = ""
	if attrs.Scope.Kind != rbac.ScopeZone || attrs.Scope.Name == "" {
		return attrs
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil || objects == nil {
		return attrs
	}
	z, _ := objects.Objects.Zone(attrs.Scope.Name)
	attrs.ZoneGroup = public.ZoneGroupOf(z)
	return attrs
}
