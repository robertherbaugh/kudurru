package main

import (
	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// ready reports continuing eligibility of the complete publication. The
// publisher's Initialized channel is only a historical startup signal.
func (s *servingRuntime) ready() (bool, []string) {
	lease, err := s.publisher.CaptureHTTP(s.life)
	if err != nil {
		// Status reports bounded local lifecycle labels, never private owner
		// data or raw preparation errors. Keep the unavailable decision while
		// distinguishing failed preparation from an unserved current cut.
		return false, []string{"complete serving generation unavailable: " + s.publisher.Status().Reason}
	}
	defer lease.Release()
	value, _ := lease.Bundle().Component("runtime")
	owner, ok := value.(*servingbuild.RuntimeOwner)
	if !ok || owner == nil || owner.Primary() == nil || owner.Zones() == nil {
		return false, []string{"complete primary routing unavailable"}
	}
	now := s.opts.Now()
	zones := owner.Zones().Zones(now)
	value, _ = lease.Bundle().Component("objects")
	objects, ok := value.(*zonecache.Snapshot)
	if !ok || objects == nil {
		return false, []string{"complete zone configuration unavailable"}
	}
	for _, desired := range objects.Objects.Zones() {
		if desired.Spec.Type != public.ZoneTypePrimary && desired.Spec.Type != public.ZoneTypeForward {
			continue // Transferred data availability is local to its zone.
		}
		apex := dns.CanonicalName(desired.Spec.ZoneName)
		route := zones.Match(apex)
		if !owner.Primary().Admitted(apex, now) || route == nil || route.Name() != apex || route.Type() != desired.Spec.Type || !route.RoutingAvailableAt(now) {
			return false, []string{"zone awaiting committed publication: " + apex}
		}
	}
	return true, nil
}
