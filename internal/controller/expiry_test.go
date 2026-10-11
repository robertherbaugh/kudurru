package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// TestExpiryRequeuesZone: a record whose expiresAt passes leaves the
// zone's contentHash and moves the serial at that instant, without any
// object changing (the zone is requeued at zone.Zone.NextExpiry).
func TestExpiryRequeuesZone(t *testing.T) {
	e := newEnv(t)
	e.apply(zoneObj("example", "example.com."))
	e.apply(rrsetObj("apps", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1")))
	// expiresAt is stored with whole seconds.
	expires := time.Now().Truncate(time.Second).Add(2 * time.Second)
	temp := ep("temp.example.com", "A", "192.0.2.9")
	temp.Kudurru = &v1alpha1.EndpointExt{ExpiresAt: &metav1.Time{Time: expires}}
	e.apply(rrsetObj("apps", "temp", "example", time.Second, temp))
	e.start()

	before := zoneHash(t, "example.com.", []RecordTuple{
		{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}},
		{Name: "temp.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.9"}},
	})
	e.eventually("zone with the temporary record", func() bool { return e.zone("example").Status.ContentHash == before })
	if time.Now().After(expires) {
		t.Skip("the host was too slow to observe the record before it expired")
	}
	serial := e.zone("example").Status.Serial

	after := zoneHash(t, "example.com.", []RecordTuple{
		{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}},
	})
	deadline := time.Now().Add(10 * time.Second)
	for e.zone("example").Status.ContentHash != after {
		if time.Now().After(deadline) {
			t.Fatalf("contentHash still %s 10 s after the expiry, want %s", e.zone("example").Status.ContentHash, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
	z := e.zone("example")
	if !time.Now().After(expires) {
		t.Fatal("the record left the zone before its expiresAt")
	}
	if z.Status.Serial == serial {
		t.Fatalf("serial %d did not move at the expiry", serial)
	}
	if z.Status.RecordCount != 1 {
		t.Fatalf("recordCount %d after the expiry, want 1", z.Status.RecordCount)
	}
}
