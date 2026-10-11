// Package zoneview connects zonecache to the zone model: a zonecache.Builder
// that compiles every DNSZone and its DNSRecordSets into a *zone.Snapshot,
// and an authoritative.Source that reads it per query.
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

// Builder returns the zonecache.Builder that compiles zones. A zone whose
// spec does not build is skipped and logged (its reconciler reports the
// condition); record-set errors drop only the offending records, which is
// what zone.Build already does.
func Builder(logger *slog.Logger) zonecache.Builder {
	if logger == nil {
		logger = slog.Default()
	}
	var generation atomic.Uint64
	return zonecache.NewBuilder(ViewName, func(objects zonecache.ObjectSet) (any, error) {
		var zones []*zone.Zone
		for _, dz := range objects.Zones() {
			z, errs := zone.Build(dz, objects.RecordSetsForZone(dz.Name), zone.BuildOptions{Now: objects.Now()})
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

// Source adapts a zonecache.Cache to authoritative.Source. Synced is false
// while the cache is unsynced or serving a snapshot older than MaxAge, so
// the stage answers SERVFAIL rather than NXDOMAIN (§4.4).
type Source struct {
	Cache *zonecache.Cache
}

var _ authoritative.Source = Source{}

// Zones implements authoritative.Source.
func (s Source) Zones() zone.Set {
	snap := s.Cache.Snapshot()
	if snap == nil {
		return unsynced{}
	}
	zs, ok := zonecache.View[*zone.Snapshot](snap, ViewName)
	if !ok || zs == nil {
		return unsynced{}
	}
	if !snap.Authoritative() {
		return gated{Set: zs}
	}
	return zs
}

type unsynced struct{}

func (unsynced) Synced() bool            { return false }
func (unsynced) Match(string) *zone.Zone { return nil }

// gated wraps a built snapshot whose data must not be answered as
// authoritative (stale local.db); Match still works so forward zones route.
type gated struct{ zone.Set }

func (gated) Synced() bool { return false }
