package controller

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestStubControllerNeverPublishesASharedRouteSerial(t *testing.T) {
	e := newEnv(t)
	z := zoneObj("stub", "stub.example.")
	z.Spec.Type = v1alpha1.ZoneTypeStub
	z.Spec.Stub = &v1alpha1.StubSpec{Nameservers: []string{"192.0.2.53"}}
	e.apply(z)
	z = e.zone("stub")
	z.Status.Serial, z.Status.RecordCount, z.Status.ContentHash = 98765, 20, "old-primary-content"
	z.Status.NameserverAddresses = []string{"192.0.2.1"}
	if err := e.st.UpdateStatus(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	e.apply(rrsetObj("apps", "ignored-stub-data", "stub", 0, ep("injected.stub.example", "A", "203.0.113.1")))
	e.start()
	e.eventually("stub status reports per-node discovery", func() bool {
		cur := e.zone("stub")
		return cur.Status.Serial == 0 && cur.Status.ContentHash == "" && cur.Status.RecordCount == 0 && len(cur.Status.NameserverAddresses) == 0 && cond(cur.Status.Conditions, v1alpha1.ConditionReady) == "Unknown/PerNodeDiscovery"
	})
	e.settle()
	before := e.st.Writes(KindDNSZone)
	e.clock.Advance(time.Hour)
	cur := e.zone("stub")
	cur.Spec.Stub.Nameservers = []string{"192.0.2.99"}
	e.apply(cur)
	e.eventually("new bootstrap observed without restamping", func() bool {
		next := e.zone("stub")
		return next.Status.ObservedGeneration == next.Generation && next.Generation > cur.Generation && next.Status.Serial == 0
	})
	e.settle()
	if e.st.Writes(KindDNSZone) != before+1 || e.eventCount(EventSerialBumped, key(KindDNSZone, "", "stub")) != 0 {
		t.Fatal("stub controller stamped local discovery into shared status or repeatedly rewrote it")
	}
}

func TestStubControllerDiagnosesUnsupportedTransport(t *testing.T) {
	e := newEnv(t)
	z := zoneObj("stub", "stub.example.")
	z.Spec.Type = v1alpha1.ZoneTypeStub
	z.Spec.Stub = &v1alpha1.StubSpec{Nameservers: []string{"tls://ns.example:853"}}
	e.apply(z)
	e.start()
	e.eventually("invalid bootstrap diagnosed", func() bool {
		return cond(e.zone("stub").Status.Conditions, v1alpha1.ConditionReady) == "False/Invalid"
	})
}
