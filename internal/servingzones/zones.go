package servingzones

import (
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

type composed struct {
	primary     primaryruntime.Cut
	nonprimary  *zone.Snapshot
	secondary   *secondary.BoundSnapshot
	stub        *stub.Snapshot
	prepared    *primaryruntime.PreparedImage
	coordinated map[string]bool
	coverage    map[string]*zone.Zone
	now         time.Time
}

func (c composed) Synced() bool { return c.primary.Synced() && c.nonprimary.Synced() }
func (c composed) Gate() zone.Gate {
	if !c.Synced() {
		return zone.GateUnsynced
	}
	return zone.GateOpen
}
func (c composed) Match(name string) *zone.Zone {
	primary, other := c.primary.Match(name), c.nonprimary.Match(name)
	if primary != nil && (other == nil || len(primary.Name()) >= len(other.Name())) {
		return primary
	}
	if other == nil {
		return nil
	}
	if c.coordinated[other.Name()] && !c.prepared.Admitted(other.Name(), c.now) {
		return c.coverage[other.Name()]
	}
	var runtime *zone.Zone
	switch other.Type() {
	case public.ZoneTypeSecondary:
		runtime = c.secondary.Lookup(other.SecondaryIdentity())
	case public.ZoneTypeStub:
		runtime = c.stub.Lookup(other.StubIdentity())
	}
	if runtime != nil && runtime.Name() == other.Name() && runtime.RoutingAvailableAt(c.now) {
		return runtime
	}
	return other
}
