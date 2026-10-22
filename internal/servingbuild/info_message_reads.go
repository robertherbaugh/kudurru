package servingbuild

import (
	"context"
	"errors"
	"slices"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// InfoMessageReads projects diagnoses from original complete request owners.
// It has no publisher, store, manager, or mutable policy/list holder.
type InfoMessageReads struct{}

// NewInfoMessageReads constructs a stateless original-request reader.
func NewInfoMessageReads() *InfoMessageReads { return &InfoMessageReads{} }

// InfoMessages evaluates this request's immutable configuration at exactly now.
// Acquisition/attempt history is not retained by the bound zone owner; only
// actual serving availability is reported. emit must not retain owner data.
func (r *InfoMessageReads) InfoMessages(ctx context.Context, now time.Time, emit func(severity, source, reason, object, message, hint string) error) error {
	if r == nil || ctx == nil || now.IsZero() || emit == nil {
		return serving.ErrComponents
	}
	id, err := servinghttp.Identity(ctx)
	if err != nil {
		return err
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil {
		return err
	}
	ix, err := servinghttp.Component[*policy.Index](ctx, "policy")
	if err != nil {
		return err
	}
	runtime, err := servinghttp.Component[*RuntimeOwner](ctx, "runtime")
	if err != nil {
		return err
	}
	zones, err := servinghttp.Component[*servingzones.Prepared](ctx, "zones")
	if err != nil {
		return err
	}
	if id.Base.Validate() != nil || objects == nil || ix == nil || ix.Generation() != id.Base.Revision ||
		diagnosticOwners(objects, runtime, zones, id) != nil || !slices.Equal(objects.Objects.Kinds(), zonecache.DefaultKinds()) ||
		objects.Objects.Lists() == nil || objects.Objects.Len() > 100_000 {
		return serving.ErrComponents
	}
	check := func() error {
		_, err := servinghttp.Identity(ctx)
		return err
	}
	add := func(severity, source, reason, object, message, hint string) error {
		if err := check(); err != nil {
			return err
		}
		return emit(severity, source, reason, object, message, hint)
	}
	ix.RangeDiagnoses(func(d policy.Diagnosis) bool {
		if len(d.Kind) > 512 || len(d.Object) > 512-len(d.Kind)-1 {
			err = serving.ErrComponents
			return false
		}
		severity := "warning"
		if d.Reason == policy.ReasonDisabled {
			severity = "info"
		}
		err = add(severity, "policy", d.Reason, d.Kind+"/"+d.Object, d.Message, "")
		return err == nil
	})
	if err != nil {
		return err
	}
	for _, list := range objects.Objects.BlockLists() {
		if err := check(); err != nil {
			return err
		}
		if list == nil || len(list.Namespace) > 253 || len(list.Name) > 253 {
			return serving.ErrComponents
		}
		if public.IsDisabled(list) {
			continue
		}
		_, ready, err := NewPolicyListReads().PolicyListState(ctx, list.Namespace, list.Name, now)
		if err != nil {
			return err
		}
		if !ready {
			name := list.Name
			if list.Namespace != "" {
				name = list.Namespace + "/" + name
			}
			if err := add("error", "blocklists", "ListsNotLoaded", "BlockList/"+name, "This node has no available compiled copy of this enabled blocklist.", ""); err != nil {
				return err
			}
		}
	}
	for _, condition := range objects.Degraded() {
		if len(condition.Reason) > 256 || len(condition.Message) > 64<<10 || len(condition.Reason)+2 > (64<<10)-len(condition.Message) {
			return serving.ErrComponents
		}
		if err := add("warning", "zonecache", condition.Reason, condition.Builder, condition.String(), ""); err != nil {
			return err
		}
	}
	if objects.State == zonecache.FromSnapshot {
		message := "serving from the original local.db snapshot while the store is unreachable"
		if objects.Stale {
			message = "serving a stale local.db snapshot; authority coverage may be unavailable"
		}
		if err := add("warning", "zonecache", "ServingFromSnapshot", "", message, ""); err != nil {
			return err
		}
	}
	view := diagnosticView{objects: objects.Objects, runtime: runtime, check: check}
	set := zones.Zones(now)
	for _, desired := range objects.Objects.Zones() {
		if err := check(); err != nil {
			return err
		}
		if desired == nil || len(desired.Name) > 253 {
			return serving.ErrComponents
		}
		source, message := "", ""
		switch desired.Spec.Type {
		case public.ZoneTypeSecondary:
			source, message = "secondary", "This node has no available transferred copy for this zone."
		case public.ZoneTypeStub:
			source, message = "stub", "This node has no available discovered route for this zone."
		default:
			continue
		}
		if _, err := view.zoneFrom(set, desired.Name, now); err != nil {
			if !errors.Is(err, serving.ErrPending) {
				return err
			}
			if err := add("error", source, "ServingUnavailable", "DNSZone/"+desired.Name, message, "Inspect this node's zone status for serving availability."); err != nil {
				return err
			}
		}
	}
	return check()
}
