package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestSecondaryControllerClearsLegacyStatusWithoutStamping(t *testing.T) {
	e := newEnv(t)
	secondary := zoneObj("copy", "copy.example.")
	secondary.Spec.Type = v1alpha1.ZoneTypeSecondary
	secondary.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}, TSIGKeyRef: "primary-key", TransferProtocol: "tcp"}
	e.apply(secondary)
	secondary = e.zone("copy")
	secondary.Status = v1alpha1.DNSZoneStatus{
		Serial: 98765, ContentHash: "sha256:legacy-primary-hash", RecordCount: 23, NameserverAddresses: []string{"192.0.2.99"},
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonServing, LastTransitionTime: metav1.NewTime(t0)},
			{Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonInvalid, LastTransitionTime: metav1.NewTime(t0)},
		},
	}
	if err := e.st.UpdateStatus(context.Background(), secondary); err != nil {
		t.Fatal(err)
	}
	// The independently loaded copy is immutable and retains its external
	// serial even while the shared object previously described primary data.
	var records []dns.RR
	for _, text := range []string{
		"copy.example. 7200 IN SOA ns.copy.example. admin.copy.example. 4294967295 3600 60 7200 90",
		"copy.example. 600 IN NS ns.copy.example.",
		"www.copy.example. 42 IN A 192.0.2.80",
	} {
		r, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	runtimeCopy, err := zone.BuildSecondary(secondary, records, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e.apply(zoneObj("primary", "primary.example."))
	e.apply(rrsetObj("apps", "primary-web", "primary", 0, ep("www.primary.example", "A", "192.0.2.1")))
	e.apply(rrsetObj("apps", "ignored-copy-data", "copy", 0, ep("injected.copy.example", "A", "203.0.113.1")))
	e.start()
	assertSecondary := func() {
		t.Helper()
		cur := e.zone("copy")
		if cur.Status.Serial != 0 || cur.Status.ContentHash != "" || cur.Status.RecordCount != 0 || len(cur.Status.NameserverAddresses) != 0 || cur.Status.ObservedGeneration != cur.Generation {
			t.Fatalf("shared secondary still reports primary data: %+v", cur.Status)
		}
		ready := condition(cur.Status.Conditions, v1alpha1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionUnknown || ready.Reason != "PerNodeTransfer" || ready.ObservedGeneration != cur.Generation || !strings.Contains(ready.Message, "independently") {
			t.Fatalf("shared Ready claims node availability: %+v", ready)
		}
		if condition(cur.Status.Conditions, v1alpha1.ConditionDegraded) != nil {
			t.Fatal("legacy primary degraded condition survived")
		}
		if runtimeCopy.Serial() != 4294967295 || runtimeCopy.SOA().Serial != 4294967295 {
			t.Fatal("controller restamped runtime secondary data")
		}
		if n := e.eventCount(EventSerialBumped, key(KindDNSZone, "", "copy")); n != 0 {
			t.Fatalf("controller emitted %d secondary serial bumps", n)
		}
	}
	e.eventually("secondary legacy status cleared", func() bool {
		cur := e.zone("copy")
		return cur.Status.Serial == 0 && cur.Status.ContentHash == "" && cond(cur.Status.Conditions, v1alpha1.ConditionReady) == "Unknown/PerNodeTransfer"
	})
	e.eventually("unrelated primary stamped", func() bool { return e.zone("primary").Status.Serial != 0 })
	assertSecondary()
	e.settle()
	before := e.st.Writes(KindDNSZone)
	e.clock.Advance(time.Hour)
	cur := e.zone("copy")
	cur.Spec.Secondary.Primaries = []string{"192.0.2.99"}
	cur.Spec.DefaultTTL = 1
	e.apply(cur)
	e.eventually("secondary configuration observed", func() bool {
		z := e.zone("copy")
		return z.Generation > cur.Generation && z.Status.ObservedGeneration == z.Generation
	})
	assertSecondary()
	e.settle()
	if e.st.Writes(KindDNSZone) != before+1 {
		t.Fatal("secondary reconciliation keeps rewriting status")
	}
	primarySerial := e.zone("primary").Status.Serial
	e.apply(rrsetObj("apps", "primary-web", "primary", 0, ep("www.primary.example", "A", "192.0.2.2")))
	e.eventually("unrelated primary changes still stamp", func() bool { return e.zone("primary").Status.Serial != primarySerial })
	assertSecondary()
}
