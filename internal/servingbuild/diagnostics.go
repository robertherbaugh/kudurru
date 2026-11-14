package servingbuild

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Diagnostics reads actual committed serving data. Public methods require the
// original servinghttp request lease; only explicit internal Capture acquires one.
type Diagnostics struct {
	publisher *serving.Publisher
	now       func() time.Time
}

// NewDiagnostics does no I/O and starts no workers. A nil clock uses time.Now.
func NewDiagnostics(publisher *serving.Publisher, now func() time.Time) (*Diagnostics, error) {
	if publisher == nil {
		return nil, serving.ErrComponents
	}
	if now == nil {
		now = time.Now
	}
	return &Diagnostics{publisher: publisher, now: now}, nil
}

type diagnosticView struct {
	objects zonecache.ObjectSet
	runtime *RuntimeOwner
	check   func() error
	now     func() time.Time
}

func diagnosticOwners(objects *zonecache.Snapshot, runtime *RuntimeOwner, zones *servingzones.Prepared, identity serving.Identity) error {
	if objects == nil || runtime == nil || runtime.primary == nil || runtime.zones == nil || zones != runtime.zones ||
		objects.Generation != identity.Base.Revision || !runtime.primary.Base().Same(identity.Base) || !zones.Base().Same(identity.Base) {
		return serving.ErrComponents
	}
	return nil
}

func (d *Diagnostics) request(ctx context.Context) (diagnosticView, error) {
	if d == nil || d.publisher == nil || d.now == nil || ctx == nil {
		return diagnosticView{}, serving.ErrComponents
	}
	identity, err := servinghttp.Identity(ctx)
	if err != nil {
		return diagnosticView{}, err
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil {
		return diagnosticView{}, err
	}
	runtime, err := servinghttp.Component[*RuntimeOwner](ctx, "runtime")
	if err != nil {
		return diagnosticView{}, err
	}
	zones, err := servinghttp.Component[*servingzones.Prepared](ctx, "zones")
	if err != nil {
		return diagnosticView{}, err
	}
	if err := diagnosticOwners(objects, runtime, zones, identity); err != nil {
		return diagnosticView{}, err
	}
	return diagnosticView{objects: objects.Objects, runtime: runtime, now: d.now, check: func() error {
		_, err := servinghttp.Identity(ctx)
		return err
	}}, nil
}

// ZoneObjects returns immutable public desired objects from this request's
// original complete cut. Do not mutate or retain them beyond the handler.
func (d *Diagnostics) ZoneObjects(ctx context.Context) (zonecache.ObjectSet, error) {
	v, err := d.request(ctx)
	if err != nil {
		return zonecache.ObjectSet{}, err
	}
	return v.objects, nil
}

// ReadZone returns the exact available model for a DNSZone metadata.name.
// Missing desired keys return store.ErrNotFound; unavailable, replaced or expired
// authority returns serving.ErrPending. The model is immutable and lease-bound.
func (d *Diagnostics) ReadZone(ctx context.Context, objectName string) (*zone.Zone, error) {
	v, err := d.request(ctx)
	if err != nil {
		return nil, err
	}
	return v.zone(objectName, d.now())
}

// ReadTransferredMetadata reports the original cut's last acquired copy even
// after expiry. It never recaptures a manager, returns another UID's data, or
// changes ReadZone's refusal of unavailable contents.
func (d *Diagnostics) ReadTransferredMetadata(ctx context.Context, objectName string) (*zone.Zone, error) {
	v, err := d.request(ctx)
	if err != nil {
		return nil, err
	}
	desired, found := v.objects.Zone(objectName)
	if !found {
		return nil, store.ErrNotFound
	}
	model := v.runtime.zones.TransferredMetadata(desired)
	if model == nil {
		return nil, serving.ErrPending
	}
	return model, nil
}

func (v diagnosticView) zone(objectName string, now time.Time) (*zone.Zone, error) {
	return v.zoneFrom(v.runtime.zones.Zones(now), objectName, now)
}

func (v diagnosticView) zoneFrom(set zone.Set, objectName string, now time.Time) (*zone.Zone, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	desired, found := v.objects.Zone(objectName)
	if !found {
		return nil, store.ErrNotFound
	}
	if now.IsZero() {
		return nil, serving.ErrPending
	}
	apex := dns.CanonicalName(desired.Spec.ZoneName)
	model := set.Match(apex)
	if model == nil || model.Name() != apex || model.Type() != desired.Spec.Type || !model.RoutingAvailableAt(now) {
		return nil, serving.ErrPending
	}
	if desired.Spec.Type == public.ZoneTypePrimary {
		o, ok, err := v.primaryStatus(objectName)
		if err != nil {
			return nil, err
		}
		if !ok || o.Member.Zone.Apex != apex || !v.runtime.primary.Admitted(apex, now) {
			return nil, serving.ErrPending
		}
	}
	return model, nil
}

// PrimaryStatus returns truthful durable head metadata for this same desired
// UID/type/apex, even when contents are unavailable. Availability must be checked
// separately through ReadZone. A replacement UID cannot borrow historical data.
func (d *Diagnostics) PrimaryStatus(ctx context.Context, objectName string) (primaryruntime.PrimaryStatusObservation, bool, error) {
	v, err := d.request(ctx)
	if err != nil {
		return primaryruntime.PrimaryStatusObservation{}, false, err
	}
	return v.primaryStatus(objectName)
}

func (v diagnosticView) primaryStatus(name string) (primaryruntime.PrimaryStatusObservation, bool, error) {
	if err := v.check(); err != nil {
		return primaryruntime.PrimaryStatusObservation{}, false, err
	}
	desired, found := v.objects.Zone(name)
	if !found {
		return primaryruntime.PrimaryStatusObservation{}, false, store.ErrNotFound
	}
	o, ok := v.runtime.primary.CommittedPrimaryStatus(name)
	if !ok || desired.Spec.Type != public.ZoneTypePrimary || o.Member.Type != desired.Spec.Type || o.Member.Zone.ObjectName != name ||
		o.Member.Zone.UID != string(desired.UID) || o.Source.UID != string(desired.UID) || o.Member.Zone.Apex != dns.CanonicalName(desired.Spec.ZoneName) {
		return primaryruntime.PrimaryStatusObservation{}, false, nil
	}
	return o, true, nil
}

// ZoneDiagnostic separates actual head evidence from serving availability. A
// desired-only placeholder has no durable HeadVersion, serial or record count.
type ZoneDiagnostic struct {
	ObjectName  string
	UID         string
	Apex        string
	Type        public.ZoneType
	Available   bool
	Serial      uint32
	RecordCount int
	// Names counts names in the available served model; unavailable is zero.
	Names       int
	ContentHash string
	HeadVersion zonestate.Version
}

// DiagnosticCut retains one eligible complete internal/metrics lease. Call
// Release after synchronous use; never race owner access against Release.
type DiagnosticCut struct {
	lease *serving.Lease
	view  diagnosticView
}

// Capture is exclusively for internal callbacks. Public reads use the original
// request methods, which never fall back to this fresh capture.
func (d *Diagnostics) Capture(ctx context.Context) (*DiagnosticCut, error) {
	if d == nil || d.publisher == nil || d.now == nil || ctx == nil {
		return nil, serving.ErrComponents
	}
	lease, err := d.publisher.CaptureHTTP(ctx)
	if err != nil {
		return nil, err
	}
	b := lease.Bundle()
	objectsValue, _ := b.Component("objects")
	objects, _ := objectsValue.(*zonecache.Snapshot)
	runtimeValue, _ := b.Component("runtime")
	runtime, _ := runtimeValue.(*RuntimeOwner)
	zonesValue, _ := b.Component("zones")
	zones, _ := zonesValue.(*servingzones.Prepared)
	if err := diagnosticOwners(objects, runtime, zones, b.Identity()); err != nil {
		lease.Release()
		return nil, err
	}
	v := diagnosticView{objects: objects.Objects, runtime: runtime, now: d.now, check: func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lease.Bundle() == nil {
			return serving.ErrReleased
		}
		return nil
	}}
	return &DiagnosticCut{lease: lease, view: v}, nil
}

// Release drops ownership exactly once. All later reads fail closed.
func (c *DiagnosticCut) Release() {
	if c != nil {
		c.lease.Release()
	}
}

// Zones returns detached summaries at one observation time. Retiring primary
// coverage without a desired object retains its actual durable diagnostic, with
// Available false. Callers advertising served serials must filter Available.
func (c *DiagnosticCut) Zones() ([]ZoneDiagnostic, error) {
	if c == nil || c.lease == nil {
		return nil, serving.ErrReleased
	}
	if err := c.view.check(); err != nil {
		return nil, err
	}
	now := c.view.now()
	if now.IsZero() {
		return nil, serving.ErrPending
	}
	set := c.view.runtime.zones.Zones(now)
	out := make([]ZoneDiagnostic, 0, len(c.view.objects.Zones()))
	seen := map[string]bool{}
	for _, desired := range c.view.objects.Zones() {
		if err := c.view.check(); err != nil {
			return nil, err
		}
		z := ZoneDiagnostic{ObjectName: desired.Name, UID: string(desired.UID), Apex: dns.CanonicalName(desired.Spec.ZoneName), Type: desired.Spec.Type}
		seen[z.Apex] = true
		model, err := c.view.zoneFrom(set, desired.Name, now)
		if err == nil {
			z.Available, z.Serial, z.RecordCount, z.Names, z.ContentHash = true, model.Serial(), model.RecordCount(), model.Names(), model.ContentHash()
		}
		if o, ok, err := c.view.primaryStatus(desired.Name); err != nil {
			return nil, err
		} else if ok {
			z.Serial, z.RecordCount, z.ContentHash, z.HeadVersion = o.Serial, o.RecordCount, o.ContentHash, o.Member.Version
		}
		out = append(out, z)
	}
	cut := c.view.runtime.primary.Cut(now)
	for apex, version := range c.view.runtime.primary.Versions() {
		if seen[apex] {
			continue
		}
		m, ok := cut.Member(apex)
		if !ok {
			continue
		}
		o, _ := c.view.runtime.primary.CommittedPrimaryStatus(m.Zone.ObjectName)
		out = append(out, ZoneDiagnostic{ObjectName: m.Zone.ObjectName, UID: m.Zone.UID, Apex: apex, Type: m.Type,
			Serial: o.Serial, RecordCount: o.RecordCount, ContentHash: o.ContentHash, HeadVersion: version})
	}
	slices.SortFunc(out, func(a, b ZoneDiagnostic) int {
		if cmp := strings.Compare(a.Apex, b.Apex); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.ObjectName, b.ObjectName)
	})
	if err := c.view.check(); err != nil {
		return nil, err
	}
	return out, nil
}
