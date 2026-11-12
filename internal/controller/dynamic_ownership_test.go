package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// dynamicOwnershipProbe is a committed primary status that also reports the
// records a dynamic update added to the committed zone.
type dynamicOwnershipProbe struct {
	*primaryStatusProbe
	mu      sync.Mutex
	dynamic []string
}

func (p *dynamicOwnershipProbe) set(t *testing.T, records ...string) {
	t.Helper()
	p.mu.Lock()
	p.dynamic = records
	p.mu.Unlock()
	p.wake()
}

func (p *dynamicOwnershipProbe) DynamicOwnership(_ context.Context, _ *v1alpha1.DNSZone) (*zonestate.DynamicOwnership, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := &zonestate.Aggregate{}
	for _, text := range p.dynamic {
		rr, err := dns.NewRR(text)
		if err != nil {
			return nil, false, err
		}
		rec, err := zonestate.FromRR(rr, zonestate.Provenance{})
		if err != nil {
			return nil, false, err
		}
		a.Records = append(a.Records, rec)
	}
	o, err := zonestate.NewDynamicOwnership(&zonestate.View{Head: &zonekinds.ZoneState{}, Aggregate: a})
	return o, err == nil, err
}

// TestRecordSetAtDynamicallyOwnedNameReportsDuplicated proves that a record
// set's own conditions show the endpoints the committed primary leaves out
// for a record a dynamic update added (D373): a partly served set is
// Degraded/Duplicated and names the left-out endpoint, a set with nothing
// served is not Ready, and both clear once the dynamic record is gone.
func TestRecordSetAtDynamicallyOwnedNameReportsDuplicated(t *testing.T) {
	p := &dynamicOwnershipProbe{primaryStatusProbe: newStatusProbe()}
	p.dynamic = []string{"dyn.example.com. 300 IN A 192.0.2.10", "moved.example.com. 300 IN DNAME elsewhere.example.net."}
	e := newEnv(t, func(o *Options) { o.CommittedPrimaryStatus = p })
	e.apply(zoneObj("primary", "example.com."))
	e.apply(rrsetObj("a", "mixed", "primary", 0, ep("kept.example.com", "A", "192.0.2.1"), ep("dyn.example.com", "A", "192.0.2.2")))
	e.apply(rrsetObj("a", "below", "primary", 0, ep("x.moved.example.com", "A", "192.0.2.3")))
	e.apply(rrsetObj("a", "other", "primary", 0, ep("dyn.example.com", "TXT", "unrelated")))
	e.start()
	e.eventually("dynamic ownership conditions", func() bool {
		mixed, below := e.rrset("a", "mixed").Status.Conditions, e.rrset("a", "below").Status.Conditions
		return cond(mixed, v1alpha1.ConditionDegraded) == "True/Duplicated" && cond(mixed, v1alpha1.ConditionReady) == "True/Serving" &&
			cond(below, v1alpha1.ConditionReady) == "False/Duplicated"
	})
	mixed := e.rrset("a", "mixed")
	if msg := condition(mixed.Status.Conditions, v1alpha1.ConditionDegraded).Message; !strings.Contains(msg, "dyn.example.com. A (a record a dynamic update (RFC 2136) added holds this name") || strings.Contains(msg, "kept.example.com") {
		t.Fatalf("degraded message %q does not name exactly the left-out endpoint", msg)
	}
	if msg := condition(mixed.Status.Conditions, v1alpha1.ConditionReady).Message; msg != "1 of 2 endpoints in zone example.com." {
		t.Fatalf("ready message %q", msg)
	}
	if got := cond(e.rrset("a", "other").Status.Conditions, v1alpha1.ConditionDegraded); got != "False/AsExpected" {
		t.Fatalf("a TXT record next to a dynamic A is served, got %s", got)
	}
	p.set(t)
	e.eventually("conditions clear once the dynamic records are gone", func() bool {
		return cond(e.rrset("a", "mixed").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected" &&
			cond(e.rrset("a", "below").Status.Conditions, v1alpha1.ConditionReady) == "True/Serving"
	})
}

// TestRecordSetWithoutDynamicOwnershipCapabilityIsUnchanged keeps the
// Kubernetes path (no committed primary) and a committed primary without
// the capability exactly as before: nothing is reported for dynamic names.
func TestRecordSetWithoutDynamicOwnershipCapabilityIsUnchanged(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.CommittedPrimaryStatus = newStatusProbe() })
	e.apply(zoneObj("primary", "example.com."))
	e.apply(rrsetObj("a", "mixed", "primary", 0, ep("kept.example.com", "A", "192.0.2.1"), ep("dyn.example.com", "A", "192.0.2.2")))
	e.start()
	e.eventually("served", func() bool {
		return cond(e.rrset("a", "mixed").Status.Conditions, v1alpha1.ConditionReady) == "True/Serving"
	})
	if got := cond(e.rrset("a", "mixed").Status.Conditions, v1alpha1.ConditionDegraded); got != "False/AsExpected" {
		t.Fatalf("degraded %s", got)
	}
}
