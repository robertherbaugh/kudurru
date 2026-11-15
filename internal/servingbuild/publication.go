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
// by a new desired placeholder. Store/dependency changes already drive the
// publisher. Waiting must not wake it: a wake cancels in-progress preparation,
// so repeated bounded driver attempts could indefinitely prevent publication.
// No lock is retained while waiting.
func (p *PrimaryPublication) Wait(ctx context.Context, writer primaryruntime.PublicationFence, member primaryruntime.Member) error {
	if writer.Term.Validate() != nil || writer.Incarnation == 0 || member.Zone.ClusterID != writer.Term.ClusterID || member.Incarnation == 0 || member.Sequence == 0 {
		return primaryruntime.ErrInvalid
	}
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

// PublishIncluding implements primaryruntime.PublicationIncluding for a
// committed UPDATE (D409). Like Publish it needs the context's writer fence
// and returns only once one protected complete lease of that writer serves
// the result; it accepts the exact member, or a later head of the same zone
// incarnation whose ledger carries the UPDATE's receipt (publishedIncluding).
func (p *PrimaryPublication) PublishIncluding(ctx context.Context, member primaryruntime.Member, receipt string, committed uint64) error {
	writer, ok := primaryruntime.PublicationFenceFrom(ctx)
	if !ok {
		return primaryruntime.ErrUnavailable
	}
	return p.WaitIncluding(ctx, writer, member, receipt, committed)
}

// WaitIncluding is Wait for a committed UPDATE: see PublishIncluding.
func (p *PrimaryPublication) WaitIncluding(ctx context.Context, writer primaryruntime.PublicationFence, member primaryruntime.Member, receipt string, committed uint64) error {
	if p == nil || p.publisher == nil || writer.Term.Validate() != nil || writer.Incarnation == 0 || member.Zone.ClusterID != writer.Term.ClusterID || member.Incarnation == 0 || member.Sequence == 0 ||
		member.Phase != zk.PhaseActive || member.Type != public.ZoneTypePrimary || receipt == "" || committed == 0 || committed > member.Version.CommitSequence {
		return primaryruntime.ErrInvalid
	}
	target := serving.TermActivation{Term: writer.Term, Incarnation: writer.Incarnation}
	for {
		changed := p.publisher.Subscribe()
		authority := p.publisher.AuthorityChanges()
		lease, err := p.publisher.CaptureHTTP(ctx)
		if err == nil || errors.Is(err, serving.ErrPending) {
			err = p.publisher.CheckWriterLease(ctx, lease, target)
		}
		if err == nil {
			err = publishedIncluding(lease, member, receipt, committed, p.now())
			if current := p.publisher.CheckWriterLease(ctx, lease, target); current != nil {
				err = current
			}
		}
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

// publishedIncluding accepts the exact member (publishedMember), or the
// served durable member of the same lifecycle source when it is the same
// zone incarnation and head identity, active, at least as new in lifecycle
// sequence and commit sequence, and its ledger carries the receipt with the
// UPDATE's own commit sequence; that member must itself be served
// (publishedMember on it, with every routing and source check; until it is,
// the wait goes on). A receipt is
// only ever added by the commit that made it and carried unchanged by every
// later commit of the zone until it is no longer live, which is after the
// UPDATE's own deadline, so such a head descends from the UPDATE's commit.
// A head that does not carry it (another chain after a restore, an evicted
// receipt) never acknowledges.
func publishedIncluding(lease *serving.Lease, want primaryruntime.Member, receipt string, committed uint64, now time.Time) error {
	err := publishedMember(lease, want, now)
	if !errors.Is(err, serving.ErrSuperseded) {
		return err
	}
	value, _ := lease.Bundle().Component("runtime")
	runtime, ok := value.(*RuntimeOwner)
	if !ok || runtime == nil || runtime.primary == nil {
		return serving.ErrComponents
	}
	got, ok := runtime.primary.DurableMember(want.Zone)
	if !ok || got.Zone != want.Zone || got.Type != want.Type || got.Incarnation != want.Incarnation || got.Phase != zk.PhaseActive || got.Sequence < want.Sequence ||
		got.Version.Zone != want.Version.Zone || got.Version.CommitSequence < want.Version.CommitSequence {
		return serving.ErrSuperseded
	}
	held, ok := runtime.primary.DurableReceipt(want.Zone, receipt)
	if !ok || held.CommitSequence != committed {
		return serving.ErrSuperseded
	}
	if err := publishedMember(lease, got, now); !errors.Is(err, serving.ErrSuperseded) {
		return err
	}
	// The served descendant is not current yet (its lifecycle still waits
	// for the driver to realign it with a newer desired source): wait for
	// the next publication within the caller's deadline.
	return serving.ErrPending
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
		// A newer desired source can leave this exact durable member in the
		// image while making it unavailable. The serial driver must reconcile
		// that source before it can serve; waiting here would stall that work
		// until the operation deadline. Never acknowledge the obsolete member.
		if !runtime.primary.SourceMatchesDesired(want.Zone) {
			return serving.ErrSuperseded
		}
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
