package zonecache

import (
	"cmp"
	"slices"
	"sort"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Kind names of the v1alpha1 kinds the typed accessors know.
const (
	KindDNSZone      store.Kind = "DNSZone"
	KindDNSRecordSet store.Kind = "DNSRecordSet"
	KindBlockList    store.Kind = "BlockList"
	KindClientGroup  store.Kind = "ClientGroup"
	KindPolicy       store.Kind = "Policy"
)

// ObjectSet is an immutable, indexed view of every watched object at one
// point in time, plus the compiled blocking lists and the build time. It is
// what a Builder receives and what Snapshot.Objects holds.
//
// Every slice and object it returns is shared with other readers and must
// not be modified. Slices are clipped, so appending to one copies it. The
// zero value is an empty set.
type ObjectSet struct {
	idx          *index
	lists        *blocking.Snapshot
	now          time.Time
	changed      map[store.Kind]bool
	allChanged   bool
	listsChanged bool
}

// index holds the per-kind indexes. Kind indexes of unchanged kinds are
// shared between successive object sets, so a change to one DNSRecordSet
// re-indexes record sets only.
type index struct {
	kinds  []store.Kind // watched kinds, sorted
	byKind map[store.Kind]*kindIndex
	rvs    map[store.Kind]string
}

// kindIndex is the immutable index of one kind.
type kindIndex struct {
	items  []store.Object                       // sorted by namespace, name
	byKey  map[store.Key]store.Object           // by key
	labels map[string]map[string][]store.Object // label key -> value -> items, sorted
	typed  any                                  // []*v1alpha1.<Kind> for the known kinds

	// DNSRecordSet only.
	byZone   map[string][]*v1alpha1.DNSRecordSet // zoneRef.name -> record sets, sorted
	expiries []time.Time                         // distinct endpoint expiresAt values, ascending
}

// newKindIndex indexes objs, which must all be of kind and have distinct
// keys. objs is sorted in place and retained.
func newKindIndex(kind store.Kind, objs []store.Object) *kindIndex {
	slices.SortFunc(objs, compareObjects)
	ki := &kindIndex{
		items: slices.Clip(objs),
		byKey: make(map[store.Key]store.Object, len(objs)),
	}
	for _, o := range objs {
		ki.byKey[store.Key{Kind: kind, Namespace: o.GetNamespace(), Name: o.GetName()}] = o
		for k, v := range o.GetLabels() {
			if ki.labels == nil {
				ki.labels = map[string]map[string][]store.Object{}
			}
			vals := ki.labels[k]
			if vals == nil {
				vals = map[string][]store.Object{}
				ki.labels[k] = vals
			}
			vals[v] = append(vals[v], o)
		}
	}
	for _, vals := range ki.labels {
		for v, items := range vals {
			vals[v] = slices.Clip(items)
		}
	}
	switch kind {
	case KindDNSZone:
		ki.typed = typedItems[*v1alpha1.DNSZone](objs)
	case KindDNSRecordSet:
		rrsets := typedItems[*v1alpha1.DNSRecordSet](objs)
		ki.typed = rrsets
		ki.byZone = map[string][]*v1alpha1.DNSRecordSet{}
		seen := map[int64]bool{}
		for _, rs := range rrsets {
			ki.byZone[rs.Spec.ZoneRef.Name] = append(ki.byZone[rs.Spec.ZoneRef.Name], rs)
			for i := range rs.Spec.Endpoints {
				ext := rs.Spec.Endpoints[i].Kudurru
				if ext == nil || ext.ExpiresAt == nil {
					continue
				}
				t := ext.ExpiresAt.Time
				if !seen[t.UnixNano()] {
					seen[t.UnixNano()] = true
					ki.expiries = append(ki.expiries, t)
				}
			}
		}
		for z, list := range ki.byZone {
			ki.byZone[z] = slices.Clip(list)
		}
		slices.SortFunc(ki.expiries, func(a, b time.Time) int { return a.Compare(b) })
	case KindBlockList:
		ki.typed = typedItems[*v1alpha1.BlockList](objs)
	case KindClientGroup:
		ki.typed = typedItems[*v1alpha1.ClientGroup](objs)
	case KindPolicy:
		ki.typed = typedItems[*v1alpha1.Policy](objs)
	}
	return ki
}

// typedItems converts objs to their concrete type, skipping any object of
// another type (which a well-behaved driver never delivers).
func typedItems[T store.Object](objs []store.Object) []T {
	out := make([]T, 0, len(objs))
	for _, o := range objs {
		if t, ok := o.(T); ok {
			out = append(out, t)
		}
	}
	return out
}

func compareObjects(a, b store.Object) int {
	if c := cmp.Compare(a.GetNamespace(), b.GetNamespace()); c != 0 {
		return c
	}
	return cmp.Compare(a.GetName(), b.GetName())
}

var emptyKindIndex = &kindIndex{}

func (s ObjectSet) kind(k store.Kind) *kindIndex {
	if s.idx == nil {
		return emptyKindIndex
	}
	if ki := s.idx.byKind[k]; ki != nil {
		return ki
	}
	return emptyKindIndex
}

// Kinds returns the watched kinds, sorted.
func (s ObjectSet) Kinds() []store.Kind {
	if s.idx == nil {
		return nil
	}
	return slices.Clip(s.idx.kinds)
}

// Len returns the number of objects of every kind.
func (s ObjectSet) Len() int {
	if s.idx == nil {
		return 0
	}
	return s.idx.total()
}

// Count returns the number of objects of one kind.
func (s ObjectSet) Count(kind store.Kind) int { return len(s.kind(kind).items) }

// Get returns the object with key.
func (s ObjectSet) Get(key store.Key) (store.Object, bool) {
	o, ok := s.kind(key.Kind).byKey[key]
	return o, ok
}

// List returns every object of kind sorted by namespace and name.
func (s ObjectSet) List(kind store.Kind) []store.Object { return s.kind(kind).items }

// WithLabel returns the objects of kind whose label key equals value,
// sorted by namespace and name.
func (s ObjectSet) WithLabel(kind store.Kind, key, value string) []store.Object {
	return s.kind(kind).labels[key][value]
}

// Select returns the objects of kind whose labels contain every pair of
// selector (an equality-based label selector), sorted by namespace and
// name. An empty selector selects every object of kind. The label index
// narrows the scan to the smallest matching bucket.
func (s ObjectSet) Select(kind store.Kind, selector map[string]string) []store.Object {
	ki := s.kind(kind)
	if len(selector) == 0 {
		return ki.items
	}
	var smallest []store.Object
	first := true
	for k, v := range selector {
		bucket := ki.labels[k][v]
		if len(bucket) == 0 {
			return nil
		}
		if first || len(bucket) < len(smallest) {
			smallest, first = bucket, false
		}
	}
	var out []store.Object
	for _, o := range smallest {
		if matchLabels(o.GetLabels(), selector) {
			out = append(out, o)
		}
	}
	return out
}

func matchLabels(have, want map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}

// Zones returns every DNSZone sorted by name.
func (s ObjectSet) Zones() []*v1alpha1.DNSZone {
	return typedOf[*v1alpha1.DNSZone](s.kind(KindDNSZone))
}

// Zone returns the DNSZone named name (DNSZone is cluster-scoped).
func (s ObjectSet) Zone(name string) (*v1alpha1.DNSZone, bool) {
	o, ok := s.Get(store.Key{Kind: KindDNSZone, Name: name})
	if !ok {
		return nil, false
	}
	z, ok := o.(*v1alpha1.DNSZone)
	return z, ok
}

// RecordSets returns every DNSRecordSet sorted by namespace and name.
func (s ObjectSet) RecordSets() []*v1alpha1.DNSRecordSet {
	return typedOf[*v1alpha1.DNSRecordSet](s.kind(KindDNSRecordSet))
}

// RecordSetsForZone returns the DNSRecordSets whose spec.zoneRef.name is
// zone, sorted by namespace and name. DNSZone is cluster-scoped, so
// zoneRef.namespace does not take part. Record sets that reference a zone
// that does not exist are still grouped under its name; ZoneRefs lists
// every referenced name.
func (s ObjectSet) RecordSetsForZone(zone string) []*v1alpha1.DNSRecordSet {
	return s.kind(KindDNSRecordSet).byZone[zone]
}

// ZoneRefs returns every zone name referenced by a DNSRecordSet, sorted.
func (s ObjectSet) ZoneRefs() []string {
	byZone := s.kind(KindDNSRecordSet).byZone
	out := make([]string, 0, len(byZone))
	for z := range byZone {
		out = append(out, z)
	}
	sort.Strings(out)
	return out
}

// Policies returns every Policy sorted by namespace and name.
func (s ObjectSet) Policies() []*v1alpha1.Policy {
	return typedOf[*v1alpha1.Policy](s.kind(KindPolicy))
}

// ClientGroups returns every ClientGroup sorted by namespace and name.
func (s ObjectSet) ClientGroups() []*v1alpha1.ClientGroup {
	return typedOf[*v1alpha1.ClientGroup](s.kind(KindClientGroup))
}

// BlockLists returns every BlockList sorted by namespace and name.
func (s ObjectSet) BlockLists() []*v1alpha1.BlockList {
	return typedOf[*v1alpha1.BlockList](s.kind(KindBlockList))
}

func typedOf[T store.Object](ki *kindIndex) []T {
	t, _ := ki.typed.([]T)
	return slices.Clip(t)
}

// Lists returns the compiled blocking lists last given to Cache.SetLists,
// or nil before the first call.
func (s ObjectSet) Lists() *blocking.Snapshot { return s.lists }

// Now is the time the set was assembled. Builders that honour
// endpoint expiresAt (§4.2) compare against Now rather than the wall clock,
// so every view in one snapshot agrees; the Cache schedules a rebuild for
// the next expiry.
func (s ObjectSet) Now() time.Time { return s.now }

// Changed reports whether objects of kind may differ from the previous
// snapshot's set. It is true for every kind on the first build, when the
// serving source switches (Unsynced, FromSnapshot, Live), and for
// DNSRecordSet when an endpoint's expiresAt has passed.
func (s ObjectSet) Changed(kind store.Kind) bool { return s.allChanged || s.changed[kind] }

// ListsChanged reports whether Lists differs from the previous snapshot's.
func (s ObjectSet) ListsChanged() bool { return s.allChanged || s.listsChanged }

// ResourceVersion returns the resourceVersion high-water mark of kind the
// set reflects: the watch Bookmark's or the newest event's. It is empty for
// drivers whose Bookmark carries none.
func (s ObjectSet) ResourceVersion(kind store.Kind) string {
	if s.idx == nil {
		return ""
	}
	return s.idx.rvs[kind]
}

// nextExpiry returns the first endpoint expiresAt after now, or zero.
func (s ObjectSet) nextExpiry(now time.Time) time.Time {
	ex := s.kind(KindDNSRecordSet).expiries
	i := sort.Search(len(ex), func(i int) bool { return ex[i].After(now) })
	if i == len(ex) {
		return time.Time{}
	}
	return ex[i]
}
