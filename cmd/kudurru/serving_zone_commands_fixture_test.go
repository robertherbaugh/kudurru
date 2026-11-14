package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// awaitServingZoneCommandForward settles the directly seeded forward zone before
// the test installs its final clock or sends its one challenged request. This
// fixture has no controller; the driver's lifecycle/source transitions and whole
// publication acknowledgment are the remaining background work. ProtectedReady
// alone can include a desired-only placeholder or an unacknowledged member.
func awaitServingZoneCommandForward(t *testing.T, f *servingCommandFixture, name string) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool {
		image, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		current := image.Generation()
		status := f.s.publisher.Status()
		if !status.ProtectedReady || status.Preparing || !status.Installed.Same(current) || !status.Served.Base.Same(current) {
			return false
		}
		var desired public.DNSZone
		for _, raw := range image.Payload.Objects {
			var header metav1.TypeMeta
			if err := json.Unmarshal(raw, &header); err != nil {
				t.Fatal(err)
			}
			if header.Kind != "DNSZone" {
				continue
			}
			var zone public.DNSZone
			if err := json.Unmarshal(raw, &zone); err != nil {
				t.Fatal(err)
			}
			if zone.Name == name {
				desired = zone
				break
			}
		}
		if desired.UID == "" || desired.Spec.Type != public.ZoneTypeForward || desired.DeletionTimestamp != nil {
			return false
		}
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		defer lease.Release()
		if err != nil {
			return false
		}
		component, ok := lease.Bundle().Component("runtime")
		owner, typed := component.(*servingbuild.RuntimeOwner)
		if !ok || !typed || owner.Primary() == nil || !owner.Primary().Base().Same(current) {
			return false
		}
		observed, ok := owner.Primary().CommittedPrimaryStatus(name)
		member := observed.Member
		if !ok || member.Zone.ObjectName != name || member.Zone.UID != string(desired.UID) ||
			member.Zone.Apex != dns.CanonicalName(desired.Spec.ZoneName) || member.Type != public.ZoneTypeForward ||
			member.Phase != zk.PhaseActive || observed.Source.UID != string(desired.UID) ||
			observed.Source.ResourceVersion != desired.ResourceVersion || !owner.Primary().SourceMatchesDesired(member.Zone) ||
			!owner.Primary().Admitted(member.Zone.Apex, time.Now()) {
			return false
		}
		driver := f.s.primary.driver.Status()
		writer := f.s.primary.writer.active.Load()
		if !driver.Running || !driver.Ready || !f.s.primary.writer.current(writer) ||
			!driver.Writer.Term.Same(writer.fence.Term) || driver.Writer.Incarnation != writer.fence.Incarnation {
			return false
		}
		acknowledged := false
		for _, zone := range driver.Zones {
			if zone.ObjectName == name {
				acknowledged = zone.Pending == "" && zone.Installed && zone.Published && zone.Acknowledged &&
					zone.Apex == member.Zone.Apex && zone.Phase == member.Phase && zone.Version == member.Version &&
					zone.Incarnation == member.Incarnation && zone.Sequence == member.Sequence
				break
			}
		}
		target := serving.TermActivation{Term: driver.Writer.Term, Incarnation: driver.Writer.Incarnation}
		return acknowledged && f.s.primary.writer.current(writer) && f.s.publisher.CheckWriterLease(t.Context(), lease, target) == nil
	})
}
