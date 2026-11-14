package controller

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// CommittedDynamicOwnership is an optional capability of a
// CommittedPrimaryStatus: it indexes the records a dynamic update (RFC 2136)
// added to the committed data of a primary zone. known is false while no
// committed head is readable; the controller then keeps what it reported.
type CommittedDynamicOwnership interface {
	DynamicOwnership(ctx context.Context, zone *v1alpha1.DNSZone) (ownership *zonestate.DynamicOwnership, known bool, err error)
}

// dynamicOwnerText says why an endpoint at a dynamically owned name is left
// out, in the words the primary compiler reports on the zone's sources.
const dynamicOwnerText = "a record a dynamic update (RFC 2136) added holds this name; delete that record, or this endpoint, to resolve it"

// dynamicConflicts returns, per record set, the endpoints the primary
// compiler leaves out because one of their records collides with a record
// a dynamic update owns (D373). They are Duplicated conflicts, so a record
// set shows them in its own conditions as it shows any endpoint the zone
// model refuses: Degraded while something of it is served, not Ready when
// nothing is. Only endpoints the view keeps are checked; outside-zone and
// identity duplicates are already reported.
func dynamicConflicts(zone *v1alpha1.DNSZone, view *zoneView, rrsets []*v1alpha1.DNSRecordSet, ownership *zonestate.DynamicOwnership) map[store.Key][]conflict {
	if zone == nil || ownership.Empty() {
		return nil
	}
	ttl := uint32(defaultTTL)
	if zone.Spec.DefaultTTL > 0 {
		ttl = uint32(zone.Spec.DefaultTTL)
	}
	endpoints := field.NewPath("spec", "endpoints")
	var out map[store.Key][]conflict
	for _, rs := range rrsets {
		key := store.KeyOf(rs)
		rv := view.Verdicts[key]
		if rv == nil || rv.Blocked != "" {
			continue
		}
		for _, i := range rv.kept {
			if i >= len(rs.Spec.Endpoints) {
				continue
			}
			ep := &rs.Spec.Endpoints[i]
			records, errs := zonemodel.ParseEndpoint(view.Origin, ttl, ep, endpoints.Index(i))
			if len(errs) > 0 {
				continue // the zone build reports it
			}
			if slices.ContainsFunc(records, ownership.Conflicts) {
				if out == nil {
					out = map[store.Key][]conflict{}
				}
				out[key] = append(out[key], conflict{Index: i, Reason: v1alpha1.ReasonDuplicated, Text: describe(*ep) + " (" + dynamicOwnerText + ")"})
			}
		}
	}
	return out
}

// reconcileDynamicOwnership refreshes the dynamic-ownership conflicts of a
// primary zone from its committed data and re-queues the record sets whose
// conflicts changed. A capability error or an unreadable head keeps the
// conflicts reported before; the next publication re-queues the zone.
func (c *Controller) reconcileDynamicOwnership(ctx context.Context, owners CommittedDynamicOwnership, zone *v1alpha1.DNSZone) {
	ownership, known, err := owners.DynamicOwnership(ctx, zone.DeepCopy())
	if err != nil {
		c.log.Warn("dynamic ownership of the committed zone is not readable; record set conditions keep what they reported", "zone", zone.Name, "err", err)
		return
	}
	if !known {
		return
	}
	view := c.cache.view(zone.Name, c.opts.Validate)
	// requeueCommittedPrimaries reconciles every primary zone on every
	// publication. A zone whose view (cached per zone version) and committed
	// head are both unchanged keeps the conflicts already derived from them.
	stamp := dynamicStamp{view: view, version: ownership.Version()}
	if c.cache.dynamicCurrent(zone.Name, stamp) {
		return
	}
	_, rrsets, _ := c.cache.snapshotZone(zone.Name)
	for _, k := range c.cache.setDynamic(zone.Name, stamp, dynamicConflicts(zone, view, rrsets, ownership)) {
		c.queues[KindDNSRecordSet].Add(k)
	}
}

// dynamicStamp identifies the inputs dynamic conflicts were derived from.
// A zero head version (no committed head) is never reused.
type dynamicStamp struct {
	view    *zoneView
	version zonestate.Version
}

// dynamicCurrent reports whether the conflicts stored for zone were derived
// from exactly these inputs.
func (c *cache) dynamicCurrent(zone string, stamp dynamicStamp) bool {
	if stamp.version == (zonestate.Version{}) {
		return false
	}
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	at, ok := c.dynamicAt[zone]
	return ok && at == stamp
}

// setDynamic stores the dynamic-ownership conflicts of zone and the inputs
// they came from, and returns the record sets whose conflicts changed.
func (c *cache) setDynamic(zone string, stamp dynamicStamp, conflicts map[store.Key][]conflict) []store.Key {
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	c.dynamicAt[zone] = stamp
	changed := changedConflicts(c.dynamic[zone], conflicts)
	if len(conflicts) == 0 {
		delete(c.dynamic, zone)
	} else {
		c.dynamic[zone] = conflicts
	}
	return changed
}

// dynamicConflictsOf returns the dynamic-ownership conflicts of one record
// set of zone.
func (c *cache) dynamicConflictsOf(zone string, key store.Key) []conflict {
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	return c.dynamic[zone][key]
}
