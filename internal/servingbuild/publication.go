package servingbuild

import (
	"context"
	"errors"
	"slices"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
)

// PrimaryPublication acknowledges lifecycle/head commits only through the real
// complete publisher. It neither reads SQLite nor trusts a Runtime's local cut.
type PrimaryPublication struct {
	publisher *serving.Publisher
	now       func() time.Time
}

// NewPrimaryPublication binds the complete publisher before the driver starts.
func NewPrimaryPublication(publisher *serving.Publisher, now func() time.Time) (*PrimaryPublication, error) {
	if publisher == nil {
		return nil, serving.ErrComponents
	}
	if now == nil {
		now = time.Now
	}
	return &PrimaryPublication{publisher: publisher, now: now}, nil
}

// Wake coalesces preparation requests; it never publishes an independent cut.
func (p *PrimaryPublication) Wake() { p.publisher.Wake() }

// Subscribe lets the driver register before inspecting its work queue.
func (p *PrimaryPublication) Subscribe() <-chan struct{} { return p.publisher.Subscribe() }

// Publish implements primaryruntime.Publication. Context carries the verified
// UPDATE fence or the admitted background writer; absence cannot adopt a term.
func (p *PrimaryPublication) Publish(ctx context.Context, _ primaryruntime.Cut, member primaryruntime.Member) error {
	fence, ok := primaryruntime.PublicationFenceFrom(ctx)
	if !ok {
		return primaryruntime.ErrUnavailable
	}
	return p.Wait(ctx, fence, member)
}

// Capture returns one owned protected lease that actually serves the exact
// durable member in the given writer incarnation. The caller must release it.
// A pending or superseded cut is never repaired by reading another owner.
func (p *PrimaryPublication) Capture(ctx context.Context, writer primaryruntime.PublicationFence, member primaryruntime.Member) (*serving.Lease, error) {
	if p == nil || p.publisher == nil || member.Zone.ClusterID != writer.Term.ClusterID || member.Incarnation == 0 || member.Sequence == 0 {
		return nil, primaryruntime.ErrInvalid
	}
	target := serving.TermActivation{Term: writer.Term, Incarnation: writer.Incarnation}
	lease, err := p.publisher.CaptureHTTP(ctx)
	if err == nil || errors.Is(err, serving.ErrPending) {
		err = p.publisher.CheckWriterLease(ctx, lease, target)
	}
	if err == nil {
		err = publishedMember(lease, member, p.now())
		if current := p.publisher.CheckWriterLease(ctx, lease, target); current != nil {
			err = current
		}
	}
	if err != nil {
		lease.Release()
		return nil, err
	}
	return lease, nil
}

// Wait observes one protected complete lease per attempt. Exact persisted
// lifecycle identity is checked before routing, including retired members hidden
// by a new desired placeholder. No lock is retained while waking or waiting.
func (p *PrimaryPublication) Wait(ctx context.Context, writer primaryruntime.PublicationFence, member primaryruntime.Member) error {
	if writer.Term.Validate() != nil || writer.Incarnation == 0 || member.Zone.ClusterID != writer.Term.ClusterID || member.Incarnation == 0 || member.Sequence == 0 {
		return primaryruntime.ErrInvalid
	}
	p.Wake()
	for {
		changed := p.publisher.Subscribe()
		authority := p.publisher.AuthorityChanges()
		lease, err := p.Capture(ctx, writer, member)
		lease.Release()
		if !errors.Is(err, serving.ErrPending) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-authority:
		}
	}
}

func publishedMember(lease *serving.Lease, want primaryruntime.Member, now time.Time) error {
	bundle := lease.Bundle()
	value, _ := bundle.Component("runtime")
	runtime, ok := value.(*RuntimeOwner)
	if !ok || runtime == nil || runtime.primary == nil || runtime.zones == nil || !runtime.primary.Base().Same(bundle.Identity().Base) {
		return serving.ErrComponents
	}
	value, _ = bundle.Component("zones")
	zones, ok := value.(*servingzones.Prepared)
	if !ok || zones != runtime.zones || !zones.Base().Same(bundle.Identity().Base) {
		return serving.ErrComponents
	}
	got, ok := runtime.primary.DurableMember(want.Zone)
	if !ok || got.Zone != want.Zone || got.Type != want.Type || got.Incarnation != want.Incarnation || got.Sequence != want.Sequence || got.Phase != want.Phase || got.Version != want.Version || !slices.Equal(got.Parents, want.Parents) {
		return serving.ErrSuperseded
	}
	set := zones.Zones(now)
	if now.IsZero() || !set.Synced() {
		return serving.ErrPending
	}
	route := set.Match(want.Zone.Apex)
	exact := route != nil && route.Name() == want.Zone.Apex
	switch want.Phase {
	case zk.PhaseActive:
		if !runtime.primary.Admitted(want.Zone.Apex, now) || !exact {
			return serving.ErrPending
		}
		if want.Type == public.ZoneTypePrimary {
			if want.Version.CommitSequence == 0 || want.Version.RootDigest == "" || route.Type() != public.ZoneTypePrimary || !route.RoutingAvailableAt(now) {
				return serving.ErrPending
			}
		} else if route.Type() != want.Type && route.RoutingAvailableAt(now) {
			return serving.ErrPending
		}
	case zk.PhasePreparing, zk.PhaseFencing:
		if !exact || route.RoutingAvailableAt(now) {
			return serving.ErrPending
		}
	case zk.PhaseRetiring, zk.PhaseRetired:
		// Removal may expose an ancestor; a replacement placeholder may
		// occupy the same apex. Neither may serve the old exact zone.
		if exact && route.RoutingAvailableAt(now) {
			return serving.ErrPending
		}
	default:
		return primaryruntime.ErrInvalid
	}
	return nil
}
