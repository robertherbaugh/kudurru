package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
)

// TestZoneModelConflicts: CNAME exclusivity and DNAME occlusion, which only
// the zone model detects, become record-set conditions and Events, and the
// refused endpoints are not in contentHash.
func TestZoneModelConflicts(t *testing.T) {
	e := newEnv(t)
	e.apply(zoneObj("example", "example.com."))
	e.apply(rrsetObj("a", "first", "example", 0,
		ep("x.example.com", "A", "192.0.2.1"),
		ep("dn.example.com", "DNAME", "example.net")))
	e.apply(rrsetObj("b", "cname", "example", time.Second,
		ep("x.example.com", "CNAME", "elsewhere.example.net"), // conflicts with a/first
		ep("ok.example.com", "A", "192.0.2.2")))
	e.apply(rrsetObj("c", "self", "example", 2*time.Second,
		ep("w.example.com", "A", "192.0.2.3"),
		ep("w.example.com", "CNAME", "v.example.com"))) // conflicts with itself
	e.apply(rrsetObj("d", "below", "example", 3*time.Second,
		ep("www.dn.example.com", "A", "192.0.2.4"))) // below a/first's DNAME
	e.start()

	for _, tc := range []struct {
		ns, name, ready, degraded, event, text string
	}{
		{"b", "cname", "True/Serving", "True/Duplicated", EventDuplicated, "cannot coexist"},
		{"c", "self", "True/Serving", "True/Invalid", EventInvalid, "cannot coexist"},
		{"d", "below", "False/Occluded", "True/Occluded", EventOccluded, "below the DNAME"},
		{"a", "first", "True/Serving", "False/AsExpected", "", ""},
	} {
		e.eventually(tc.ns+"/"+tc.name+" conditions", func() bool {
			rs := e.rrset(tc.ns, tc.name)
			return cond(rs.Status.Conditions, v1alpha1.ConditionReady) == tc.ready &&
				cond(rs.Status.Conditions, v1alpha1.ConditionDegraded) == tc.degraded
		})
		if tc.text != "" && !strings.Contains(conditionText(e.rrset(tc.ns, tc.name).Status.Conditions), tc.text) {
			t.Errorf("%s/%s: messages lack %q: %s", tc.ns, tc.name, tc.text, conditionText(e.rrset(tc.ns, tc.name).Status.Conditions))
		}
		if tc.event != "" {
			e.eventually(tc.ns+"/"+tc.name+" event", func() bool { return e.eventCount(tc.event, key(KindDNSRecordSet, tc.ns, tc.name)) == 1 })
		}
	}
	want := zoneHash(t, "example.com.", []RecordTuple{
		{Name: "x.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}},
		{Name: "dn.example.com.", Type: "DNAME", TTL: 300, Targets: []string{"example.net"}},
		{Name: "ok.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.2"}},
		{Name: "w.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.3"}},
	})
	e.eventually("zone hash without the refused endpoints", func() bool { return e.zone("example").Status.ContentHash == want })
	e.eventually("zone degraded", func() bool {
		return cond(e.zone("example").Status.Conditions, v1alpha1.ConditionDegraded) == "True/Duplicated"
	})

	// Removing the DNAME frees the occluded record: the zone reconcile
	// re-queues the record set whose conflicts changed.
	e.apply(rrsetObj("a", "first", "example", 0, ep("x.example.com", "A", "192.0.2.1")))
	e.eventually("occlusion cleared", func() bool {
		rs := e.rrset("d", "below")
		return cond(rs.Status.Conditions, v1alpha1.ConditionReady) == "True/Serving" &&
			cond(rs.Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
	e.settle()
	w := e.st.Writes("")
	e.settle()
	if e.st.Writes("") != w {
		t.Fatal("conflict conditions keep writing")
	}
}

// TestZoneSpecInvalid: a DNSZone the zone model cannot build (here a bad
// notify target) is reported, since no replica serves it.
func TestZoneSpecInvalid(t *testing.T) {
	e := newEnv(t)
	z := zoneObj("example", "example.com.")
	z.Spec.Policy.NotifyTargets = []string{"not-an-address"}
	e.apply(z)
	e.start()
	e.eventually("zone invalid", func() bool {
		return cond(e.zone("example").Status.Conditions, v1alpha1.ConditionReady) == "False/Invalid"
	})
	if msg := conditionText(e.zone("example").Status.Conditions); !strings.Contains(msg, "notifyTargets") {
		t.Fatalf("message %q", msg)
	}
	if e.zone("example").Status.ContentHash != "" {
		t.Fatal("an unservable zone has no content hash")
	}
}

// TestContentHashIncludesZoneSpec: the zone model's hash covers the SOA
// fields and the apex NS from the DNSZone spec (the M1 tuple hash did
// not), so changing them moves the serial (D12).
func TestContentHashIncludesZoneSpec(t *testing.T) {
	e := newEnv(t)
	z := zoneObj("example", "example.com.")
	e.apply(z)
	e.apply(rrsetObj("apps", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1")))
	e.start()
	e.eventually("stamped", func() bool { return e.zone("example").Status.Serial != 0 })
	e.settle()
	first := e.zone("example").Status
	z = e.zone("example").DeepCopy()
	z.Spec.Nameservers = []string{"ns1.example.com."}
	z.Generation++
	e.clock.Advance(time.Hour)
	e.apply(z)
	e.eventually("serial moved", func() bool { return e.zone("example").Status.Serial != first.Serial })
	if got := e.zone("example").Status; got.ContentHash == first.ContentHash || got.RecordCount != first.RecordCount+1 {
		t.Fatalf("status %+v after adding an apex NS (was %+v)", got, first)
	}
}

// TestDefaultValidatorIsZoneParser: Options.Validate defaults to the zone
// model's parser, so a target only miekg rejects is Invalid.
func TestDefaultValidatorIsZoneParser(t *testing.T) {
	e := newEnv(t)
	e.apply(zoneObj("example", "example.com."))
	// The miekg-free validator accepts any non-empty TXT and SRV with a
	// root target; the zone parser rejects an over-long label.
	long := strings.Repeat("a", 64) + ".example.com"
	e.apply(rrsetObj("apps", "x", "example", 0, ep("srv.example.com", "SRV", "10 5 443 "+long)))
	e.start()
	e.eventually("invalid", func() bool {
		return cond(e.rrset("apps", "x").Status.Conditions, v1alpha1.ConditionReady) == "False/Invalid"
	})
	if err := zonemodel.ValidateEndpoint(ep("srv.example.com", "SRV", "10 5 443 "+long)); err == nil {
		t.Fatal("the zone parser must reject the label")
	}
}
