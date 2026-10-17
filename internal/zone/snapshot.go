package zone

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
)

// Gate says how far the authoritative stage may trust a Set (decision D80,
// dev-docs/design-record.md §4.4).
type Gate uint8

// Gates.
const (
	// GateOpen: the set is live, or restored from a local.db snapshot
	// younger than maxAge. Every zone is answered normally.
	GateOpen Gate = iota
	// GateUnsynced: nothing trustworthy has loaded yet, so any name might
	// belong to a zone that is still missing. Every query is answered
	// SERVFAIL except positive data a loaded primary zone asserts; nothing
	// is forwarded.
	GateUnsynced
	// GateStale: the set comes from a local.db snapshot older than maxAge.
	// Names under primary zones are answered SERVFAIL (never NXDOMAIN, and
	// never from data that old); forward zones and names under no local
	// zone continue to the rest of the pipeline.
	GateStale
)

// String returns "open", "unsynced" or "stale".
func (g Gate) String() string {
	switch g {
	case GateOpen:
		return "open"
	case GateUnsynced:
		return "unsynced"
	case GateStale:
		return "stale"
	}
	return "unknown"
}

// Set is the read side of the zone cache as the authoritative stage sees
// it: how far it may be trusted, and longest-suffix zone routing. The zone
// cache publishes an immutable implementation per change (Snapshot is the
// stock one) so readers never lock.
type Set interface {
	// Synced reports whether every store watch has synced at least once
	// or the set was restored from a fresh-enough local snapshot (§4.4);
	// it is Gate() == GateOpen. Until then denials are not trustworthy:
	// the authoritative stage answers SERVFAIL instead of NXDOMAIN or
	// NODATA.
	Synced() bool
	// Gate refines Synced for the authoritative stage (D80): GateUnsynced
	// answers every query SERVFAIL, GateStale only names under primary
	// zones.
	Gate() Gate
	// Match returns the zone with the longest name that is a suffix of
	// qname (the root zone "." matches everything), or nil.
	Match(qname string) *Zone
}

// Snapshot is an immutable set of built zones keyed by name. The owner
// publishes it through an atomic.Pointer (Holder) and replaces it on every
// change with With, Without or NewSnapshot.
type Snapshot struct {
	byKey      map[string]*Zone
	gate       Gate
	generation uint64
}

var _ Set = (*Snapshot)(nil)

// NewSnapshot returns a snapshot of zones. Zone names must be unique. A
// synced snapshot has GateOpen, an unsynced one GateUnsynced.
func NewSnapshot(generation uint64, synced bool, zones ...*Zone) (*Snapshot, error) {
	s := &Snapshot{byKey: make(map[string]*Zone, len(zones)), gate: gateOf(synced), generation: generation}
	for _, z := range zones {
		if z == nil {
			continue
		}
		if _, dup := s.byKey[z.key]; dup {
			return nil, fmt.Errorf("zone: duplicate zone %q in snapshot", z.name)
		}
		s.byKey[z.key] = z
	}
	return s, nil
}

func gateOf(synced bool) Gate {
	if synced {
		return GateOpen
	}
	return GateUnsynced
}

// Synced implements Set.
func (s *Snapshot) Synced() bool { return s.gate == GateOpen }

// Gate implements Set.
func (s *Snapshot) Gate() Gate { return s.gate }

// Generation returns the generation given when the snapshot was made.
func (s *Snapshot) Generation() uint64 { return s.generation }

// Len returns the number of zones.
func (s *Snapshot) Len() int { return len(s.byKey) }

// Zone returns the zone with exactly this name, or nil.
func (s *Snapshot) Zone(name string) *Zone {
	k, err := canonicalKey(name)
	if err != nil {
		return nil
	}
	return s.byKey[k]
}

// Zones returns every zone sorted by name in canonical order.
func (s *Snapshot) Zones() []*Zone {
	out := make([]*Zone, 0, len(s.byKey))
	for _, z := range s.byKey {
		out = append(out, z)
	}
	slices.SortFunc(out, func(a, b *Zone) int { return strings.Compare(a.key, b.key) })
	return out
}

// Match implements Set: longest-suffix routing. It walks the query name's
// ancestors from the longest down to the root with one map probe each and
// does not allocate.
func (s *Snapshot) Match(qname string) *Zone {
	if len(s.byKey) == 0 {
		return nil
	}
	var kb [256]byte
	key, err := appendKey(kb[:0], qname)
	if err != nil {
		return nil
	}
	for i := len(key); i > 0; i-- {
		if key[i-1] != keySep {
			continue
		}
		if z, ok := s.byKey[string(key[:i])]; ok {
			return z
		}
	}
	return s.byKey[""]
}

// With returns a copy of s with z added or replacing the zone of the same
// name, and the given generation.
func (s *Snapshot) With(generation uint64, z *Zone) *Snapshot {
	n := s.clone(generation)
	n.byKey[z.key] = z
	return n
}

// Without returns a copy of s without the named zone.
func (s *Snapshot) Without(generation uint64, name string) *Snapshot {
	n := s.clone(generation)
	if k, err := canonicalKey(name); err == nil {
		delete(n.byKey, k)
	}
	return n
}

// WithSynced returns a copy of s with the gate set to GateOpen (synced)
// or GateUnsynced.
func (s *Snapshot) WithSynced(generation uint64, synced bool) *Snapshot {
	n := s.clone(generation)
	n.gate = gateOf(synced)
	return n
}

// WithGate returns s with gate g and the same generation. The zones are
// shared, not copied (a snapshot is never modified), so this is cheap
// enough to call per query: the zone cache adapter gates a built snapshot
// by the cache state without rebuilding it.
func (s *Snapshot) WithGate(g Gate) *Snapshot {
	if s.gate == g {
		return s
	}
	n := *s
	n.gate = g
	return &n
}

func (s *Snapshot) clone(generation uint64) *Snapshot {
	m := make(map[string]*Zone, len(s.byKey)+1)
	for k, v := range s.byKey {
		m[k] = v
	}
	return &Snapshot{byKey: m, gate: s.gate, generation: generation}
}

// Holder publishes snapshots lock-free. The zero value holds nothing,
// which reads as "not synced". It satisfies the authoritative stage's
// Source interface; the zone cache embeds one or implements Source itself.
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// Store publishes s.
func (h *Holder) Store(s *Snapshot) { h.p.Store(s) }

// Load returns the current snapshot or nil.
func (h *Holder) Load() *Snapshot { return h.p.Load() }

// Zones returns the current snapshot as a Set, or nil before the first
// Store.
func (h *Holder) Zones() Set {
	if s := h.p.Load(); s != nil {
		return s
	}
	return nil
}
