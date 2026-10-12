package controller

import (
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// cache is the controller's read model, fed only by Store.Watch: the
// reconcilers never read the store directly, so a reconcile costs no I/O
// and every replica-visible change arrives as an event (the shape of a
// controller-runtime cached client). Objects in the cache are shared and
// must not be mutated; reconcilers deep-copy before writing.
type cache struct {
	mu   sync.RWMutex
	objs map[store.Kind]map[store.Key]store.Object
	// rrsetsByZone maps a DNSZone object name to the record sets that
	// reference it.
	rrsetsByZone map[string]map[store.Key]struct{}
	// owners maps zone name → endpoint identity → record sets declaring
	// it, so a change re-reconciles exactly the record sets it may have
	// (un)duplicated.
	owners map[string]map[string]map[store.Key]struct{}
	// zoneVersion counts changes that affect a zone's view; views built at
	// an older version are rebuilt.
	zoneVersion map[string]uint64
	// pending holds the status this controller wrote for an object until
	// the watch delivers a newer resourceVersion, so a reconcile in
	// between compares against what was written, not the stale cache.
	pending map[store.Key]pendingStatus

	viewMu sync.Mutex
	views  map[string]*zoneView
	viewAt map[string]uint64
	// builds holds the latest zone-model build per zone and buildAt the
	// zone version it was built at. Builds parse every record, so they are
	// made by the zone reconcile (one at a time per zone, coalesced) and
	// record-set reconciles read the latest one instead of rebuilding.
	builds  map[string]*zoneBuild
	buildAt map[string]uint64
}

type pendingStatus struct {
	baseRV string
	obj    store.Object
}

func newCache() *cache {
	return &cache{
		objs:         map[store.Kind]map[store.Key]store.Object{},
		rrsetsByZone: map[string]map[store.Key]struct{}{},
		owners:       map[string]map[string]map[store.Key]struct{}{},
		zoneVersion:  map[string]uint64{},
		pending:      map[store.Key]pendingStatus{},
		views:        map[string]*zoneView{},
		viewAt:       map[string]uint64{},
		builds:       map[string]*zoneBuild{},
		buildAt:      map[string]uint64{},
	}
}

// get returns the cached object (shared) or nil. When this controller has
// written a status the watch has not echoed yet, the written object is
// returned instead.
func (c *cache) get(key store.Key) store.Object {
	c.mu.RLock()
	defer c.mu.RUnlock()
	obj := c.objs[key.Kind][key]
	if obj == nil {
		return nil
	}
	if p, ok := c.pending[key]; ok && p.baseRV == obj.GetResourceVersion() {
		return p.obj
	}
	return obj
}

// cachedRV returns the resourceVersion the watch last delivered for key.
func (c *cache) cachedRV(key store.Key) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if obj := c.objs[key.Kind][key]; obj != nil {
		return obj.GetResourceVersion()
	}
	return ""
}

// wrote records a status write made on top of resourceVersion baseRV.
func (c *cache) wrote(key store.Key, baseRV string, obj store.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur := c.objs[key.Kind][key]; cur != nil && cur.GetResourceVersion() == baseRV {
		c.pending[key] = pendingStatus{baseRV: baseRV, obj: obj}
	}
}

// keys returns every cached key of a kind.
func (c *cache) keys(kind store.Kind) []store.Key {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]store.Key, 0, len(c.objs[kind]))
	for k := range c.objs[kind] {
		out = append(out, k)
	}
	return out
}

// list returns the cached objects of a kind in a namespace ("" = all).
func (c *cache) list(kind store.Kind, ns string) []store.Object {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]store.Object, 0, len(c.objs[kind]))
	for k, o := range c.objs[kind] {
		if ns == "" || k.Namespace == ns {
			out = append(out, o)
		}
	}
	return out
}

// upsert stores obj and returns the previous version (nil if new). An
// event carrying the resourceVersion already cached is reported as
// unchanged (changed == false).
func (c *cache) upsert(obj store.Object) (prev store.Object, changed bool) {
	key := store.KeyOf(obj)
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.objs[key.Kind]
	if m == nil {
		m = map[store.Key]store.Object{}
		c.objs[key.Kind] = m
	}
	prev = m[key]
	if prev != nil && prev.GetResourceVersion() == obj.GetResourceVersion() && obj.GetResourceVersion() != "" {
		return prev, false
	}
	m[key] = obj
	delete(c.pending, key)
	c.index(prev, obj)
	return prev, true
}

// remove deletes key and returns the removed object, or nil.
func (c *cache) remove(key store.Key) store.Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.objs[key.Kind][key]
	if prev == nil {
		return nil
	}
	delete(c.objs[key.Kind], key)
	delete(c.pending, key)
	c.index(prev, nil)
	return prev
}

// index maintains the secondary indexes and zone versions; c.mu is held.
// A status-only change (same generation and labels) touches neither: zone
// views depend on spec, labels and identity only, so this controller's own
// status writes never invalidate them.
func (c *cache) index(prev, next store.Object) {
	if prev != nil && next != nil && prev.GetGeneration() == next.GetGeneration() &&
		equalStringMaps(prev.GetLabels(), next.GetLabels()) {
		return
	}
	switch o := firstNonNil(prev, next).(type) {
	case *v1alpha1.DNSZone:
		c.zoneVersion[o.Name]++
	case *v1alpha1.DNSRecordSet:
		key := store.KeyOf(o)
		if p, ok := prev.(*v1alpha1.DNSRecordSet); ok && p != nil {
			c.unindexRRSet(key, p)
		}
		if n, ok := next.(*v1alpha1.DNSRecordSet); ok && n != nil {
			c.indexRRSet(key, n)
		}
	}
}

func (c *cache) indexRRSet(key store.Key, rs *v1alpha1.DNSRecordSet) {
	z := rs.Spec.ZoneRef.Name
	c.zoneVersion[z]++
	if c.rrsetsByZone[z] == nil {
		c.rrsetsByZone[z] = map[store.Key]struct{}{}
	}
	c.rrsetsByZone[z][key] = struct{}{}
	if c.owners[z] == nil {
		c.owners[z] = map[string]map[store.Key]struct{}{}
	}
	for _, ep := range rs.Spec.Endpoints {
		id := identity(ep)
		if c.owners[z][id] == nil {
			c.owners[z][id] = map[store.Key]struct{}{}
		}
		c.owners[z][id][key] = struct{}{}
	}
}

func (c *cache) unindexRRSet(key store.Key, rs *v1alpha1.DNSRecordSet) {
	z := rs.Spec.ZoneRef.Name
	c.zoneVersion[z]++
	delete(c.rrsetsByZone[z], key)
	if len(c.rrsetsByZone[z]) == 0 {
		delete(c.rrsetsByZone, z)
	}
	for _, ep := range rs.Spec.Endpoints {
		id := identity(ep)
		delete(c.owners[z][id], key)
		if len(c.owners[z][id]) == 0 {
			delete(c.owners[z], id)
		}
	}
	if len(c.owners[z]) == 0 {
		delete(c.owners, z)
	}
}

// rrsetsOf returns the keys of the record sets referencing a zone.
func (c *cache) rrsetsOf(zone string) []store.Key {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]store.Key, 0, len(c.rrsetsByZone[zone]))
	for k := range c.rrsetsByZone[zone] {
		out = append(out, k)
	}
	return out
}

// identityOwners returns the record sets in zone declaring any identity of
// the given record sets' endpoints.
func (c *cache) identityOwners(zone string, from ...*v1alpha1.DNSRecordSet) []store.Key {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[store.Key]struct{}{}
	for _, rs := range from {
		if rs == nil {
			continue
		}
		for _, ep := range rs.Spec.Endpoints {
			for k := range c.owners[zone][identity(ep)] {
				seen[k] = struct{}{}
			}
		}
	}
	out := make([]store.Key, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// view returns the zone view for a DNSZone object name, rebuilding it when
// the zone or any of its record sets changed since it was built.
func (c *cache) view(zone string, validate EndpointValidator) *zoneView {
	c.mu.RLock()
	version := c.zoneVersion[zone]
	c.mu.RUnlock()
	c.viewMu.Lock()
	if v, ok := c.views[zone]; ok && c.viewAt[zone] == version {
		c.viewMu.Unlock()
		return v
	}
	c.viewMu.Unlock()

	c.mu.RLock()
	version = c.zoneVersion[zone]
	var z *v1alpha1.DNSZone
	if o, ok := c.objs[KindDNSZone][store.Key{Kind: KindDNSZone, Name: zone}].(*v1alpha1.DNSZone); ok {
		z = o
	}
	rrsets := make([]*v1alpha1.DNSRecordSet, 0, len(c.rrsetsByZone[zone]))
	for k := range c.rrsetsByZone[zone] {
		if rs, ok := c.objs[KindDNSRecordSet][k].(*v1alpha1.DNSRecordSet); ok {
			rrsets = append(rrsets, rs)
		}
	}
	c.mu.RUnlock()

	v := buildZoneView(z, rrsets, validate)
	c.viewMu.Lock()
	c.views[zone] = v
	c.viewAt[zone] = version
	c.viewMu.Unlock()
	return v
}

func firstNonNil(a, b store.Object) store.Object {
	if a != nil {
		return a
	}
	return b
}

// snapshotZone returns the zone object, its record sets and the version
// they were read at.
func (c *cache) snapshotZone(zone string) (*v1alpha1.DNSZone, []*v1alpha1.DNSRecordSet, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var z *v1alpha1.DNSZone
	if o, ok := c.objs[KindDNSZone][store.Key{Kind: KindDNSZone, Name: zone}].(*v1alpha1.DNSZone); ok {
		z = o
	}
	rrsets := make([]*v1alpha1.DNSRecordSet, 0, len(c.rrsetsByZone[zone]))
	for k := range c.rrsetsByZone[zone] {
		if rs, ok := c.objs[KindDNSRecordSet][k].(*v1alpha1.DNSRecordSet); ok {
			rrsets = append(rrsets, rs)
		}
	}
	return z, rrsets, c.zoneVersion[zone]
}

// build returns the zone-model build of zone at the current version,
// building it when the cached one is older or past its NextExpiry, and the
// build it replaced (nil when there was none or it is the same).
func (c *cache) build(zone string) (cur, prev *zoneBuild) {
	c.mu.RLock()
	version := c.zoneVersion[zone]
	c.mu.RUnlock()
	c.viewMu.Lock()
	if b, ok := c.builds[zone]; ok && c.buildAt[zone] == version && (b.NextExpiry.IsZero() || time.Now().Before(b.NextExpiry)) {
		c.viewMu.Unlock()
		return b, nil
	}
	c.viewMu.Unlock()

	z, rrsets, version := c.snapshotZone(zone)
	b := buildZone(z, rrsets)
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	prev = c.builds[zone]
	if at, ok := c.buildAt[zone]; !ok || at <= version {
		c.builds[zone] = b
		c.buildAt[zone] = version
	}
	return b, prev
}

// latestBuild returns the most recent build of zone whatever its version,
// building one when there is none yet (the start of a term).
func (c *cache) latestBuild(zone string) *zoneBuild {
	c.viewMu.Lock()
	b, ok := c.builds[zone]
	c.viewMu.Unlock()
	if ok {
		return b
	}
	b, _ = c.build(zone)
	return b
}
