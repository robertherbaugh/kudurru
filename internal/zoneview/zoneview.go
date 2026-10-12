// Package zoneview connects zonecache to the zone model: a zonecache.Builder
// that compiles every DNSZone and its DNSRecordSets into a *zone.Snapshot,
// and an authoritative.Source that reads it per query, gated by the cache
// state (decision D80).
package zoneview

import (
	"fmt"
	"log/slog"
	"sync/atomic"

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
// condition); record-set errors drop only the offending endpoints.
func Builder(logger *slog.Logger) zonecache.Builder {
	if logger == nil {
		logger = slog.Default()
	}
	var generation atomic.Uint64
	return zonecache.NewBuilder(ViewName, func(objects zonecache.ObjectSet) (any, error) {
		var zones []*zone.Zone
		for _, dz := range objects.Zones() {
			sets := zone.ServedRecordSets(dz, objects.RecordSetsForZone(dz.Name))
			z, errs := zone.Build(dz, sets, zone.BuildOptions{Now: objects.Now()})
			if z == nil {
				logger.Warn("zone skipped", "zone", dz.Name, "errors", errs.Zone.ToAggregate())
				continue
			}
			if !errs.Empty() {
				logger.Debug("zone built with record errors", "zone", dz.Name, "recordSets", len(errs.RecordSets))
			}
			zones = append(zones, z)
		}
		snap, err := zone.NewSnapshot(generation.Add(1), true, zones...)
		if err != nil {
			return nil, fmt.Errorf("zoneview: %w", err)
		}
		return snap, nil
	})
}

// Source adapts a zonecache.Cache to authoritative.Source. The gate follows
// the cache state (D80, §4.4):
//
//   - Unsynced (nothing loaded, no usable local.db): zone.GateUnsynced, every
//     query SERVFAIL;
//   - FromSnapshot older than MaxAge: zone.GateStale, SERVFAIL for names in
//     primary zones while forward zones and other names are forwarded;
//   - Live, or FromSnapshot within MaxAge: zone.GateOpen.
type Source struct {
	Cache *zonecache.Cache
}

var _ authoritative.Source = Source{}

// Zones implements authoritative.Source. On the live path it returns the
// built snapshot itself and does not allocate.
func (s Source) Zones() zone.Set {
	snap := s.Cache.Snapshot()
	if snap == nil {
		return unsynced{}
	}
	zs, ok := zonecache.View[*zone.Snapshot](snap, ViewName)
	if !ok || zs == nil {
		return unsynced{}
	}
	return zs.WithGate(Gate(snap))
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
