// Package zoneview connects zonecache to the zone model: a zonecache.Builder
// that compiles every DNSZone and its DNSRecordSets into a *zone.Snapshot,
// and an authoritative.Source that reads it per query, gated by the cache
// state (decision D80).
package zoneview

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// ViewName is the zonecache view the builder publishes.
const ViewName = "zones"

// Builder returns the zonecache.Builder that compiles zones. Each zone is
// built from zone.ServedRecordSets (recordSetSelector, whole-object
// validity, oldest first), the same selection the controller hashes, so
// status.contentHash is the hash of the zone served here. A zone whose
// spec does not build is skipped and logged (the controller reports the
// condition); record-set errors drop only the offending endpoints. Duplicate
// apexes retain the last served zone until the conflict is resolved. On cold
// start the first metadata name wins deterministically. Other zones continue
// rebuilding, so an accepted conflict cannot poison the entire view.
func Builder(logger *slog.Logger) zonecache.Builder {
	if logger == nil {
		logger = slog.Default()
	}
	var mu sync.Mutex
	var generation uint64
	lastGood := map[string]*zone.Zone{}
	return zonecache.NewBuilder(ViewName, func(objects zonecache.ObjectSet) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		byApex := map[string][]*zone.Zone{}
		names := map[string][]string{}
		for _, dz := range objects.Zones() {
			if dz.Spec.Type == v1alpha1.ZoneTypeSecondary {
				z, err := zone.BuildSecondaryPlaceholder(dz)
				if err != nil {
					logger.Warn("secondary zone skipped", "zone", dz.Name, "error", err)
					continue
				}
				zones = append(zones, z)
				continue
			}
			sets := zone.ServedRecordSets(dz, objects.RecordSetsForZone(dz.Name))
			z, errs := zone.Build(dz, sets, zone.BuildOptions{Now: objects.Now()})
			if z == nil {
				logger.Warn("zone skipped", "zone", dz.Name, "errors", errs.Zone.ToAggregate())
				continue
			}
			if !errs.Empty() {
				logger.Debug("zone built with record errors", "zone", dz.Name, "recordSets", len(errs.RecordSets))
			}
			byApex[z.Name()] = append(byApex[z.Name()], z)
			names[z.Name()] = append(names[z.Name()], dz.Name)
		}
		var zones []*zone.Zone
		nextGood := make(map[string]*zone.Zone, len(byApex))
		for apex, candidates := range byApex {
			selected := candidates[0]
			if len(candidates) > 1 {
				if previous := lastGood[apex]; previous != nil {
					selected = previous
				}
				logger.Warn("duplicate zone apex; retaining selected serving data", "apex", apex, "objects", names[apex], "lastGood", lastGood[apex] != nil)
			}
			zones = append(zones, selected)
			nextGood[apex] = selected
		}
		generation++
		snap, err := zone.NewSnapshot(generation, true, zones...)
		if err != nil {
			return nil, fmt.Errorf("zoneview: %w", err)
		}
		lastGood = nextGood
		return snap, nil
	})
}

// Source adapts a zonecache.Cache to authoritative.Source. The gate follows
// the cache state (D80, §4.4):
//
//   - Unsynced (nothing loaded, no usable local.db): zone.GateUnsynced, every
//     query SERVFAIL;
//   - FromSnapshot older than MaxAge: zone.GateStale, SERVFAIL for names in
//     primary and secondary zones while forward zones and other names are
//     forwarded; fresh transferred data does not override stale configuration;
//   - Live, or FromSnapshot within MaxAge: zone.GateOpen.
type Source struct {
	Cache     *zonecache.Cache
	Secondary *secondary.Manager
}

var _ authoritative.Source = Source{}

// Zones implements authoritative.Source. It loads immutable configuration
// and transferred snapshots once per query, without store or network I/O.
func (s Source) Zones() zone.Set {
	snap := s.Cache.Snapshot()
	if snap == nil {
		return unsynced{}
	}
	zs, ok := zonecache.View[*zone.Snapshot](snap, ViewName)
	if !ok || zs == nil {
		return unsynced{}
	}
	base := zs.WithGate(Gate(snap))
	if s.Secondary != nil {
		return secondarySet{Set: base, copies: s.Secondary.Snapshot()}
	}
	return base
}

// secondarySet resolves only configured placeholders. Old transfers can
// never revive a deleted zone or a replaced configuration.
type secondarySet struct {
	zone.Set
	copies *secondary.Snapshot
}

func (s secondarySet) Match(name string) *zone.Zone {
	z := s.Set.Match(name)
	if z != nil && z.Type() == v1alpha1.ZoneTypeSecondary {
		if transferred := s.copies.Lookup(z.SecondaryIdentity()); transferred != nil {
			return transferred
		}
	}
	return z
}

// Gate maps a zonecache snapshot's state onto the authoritative gate.
func Gate(snap *zonecache.Snapshot) zone.Gate {
	switch {
	case snap == nil || snap.State == zonecache.Unsynced:
		return zone.GateUnsynced
	case !snap.Authoritative():
		return zone.GateStale
	}
	return zone.GateOpen
}

// unsynced is the set before the zone builder has produced anything.
type unsynced struct{}

func (unsynced) Synced() bool            { return false }
func (unsynced) Gate() zone.Gate         { return zone.GateUnsynced }
func (unsynced) Match(string) *zone.Zone { return nil }
