package controller

import (
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

type externalSourceFixture struct {
	mu      sync.Mutex
	problem string
	changed chan struct{}
}

func (f *externalSourceFixture) Problems(spec *v1alpha1.ExternalSpec) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec != nil && spec.Ingress && f.problem != "" {
		return []string{f.problem}
	}
	return nil
}

func (f *externalSourceFixture) HealthChanged() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *externalSourceFixture) set(problem string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.problem = problem
	close(f.changed)
	f.changed = make(chan struct{})
}

// An external zone on a server without the source adapter
// (externalZones.enabled off) is served but Degraded{SourcesNotRead}, so an
// operator sees why it stays empty; with the adapter it is Ready and not
// Degraded. A primary zone is unaffected by the setting.
func TestExternalZoneWithoutSourcesIsDegraded(t *testing.T) {
	external := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "ext"}, Spec: v1alpha1.DNSZoneSpec{
		ZoneName: "ext.example.", Type: v1alpha1.ZoneTypeExternal, Nameservers: []string{"ns.ext.example."},
		External: &v1alpha1.ExternalSpec{Ingress: true}}}
	for _, on := range []bool{false, true} {
		sources := &externalSourceFixture{changed: make(chan struct{})}
		e := newEnv(t, func(o *Options) {
			if on {
				o.ExternalZoneSources = sources
			}
		})
		e.start()
		e.apply(external.DeepCopy())
		e.apply(zoneObj("prim", "prim.example."))
		e.eventually("zone conditions", func() bool {
			c := e.zone("ext").Status.Conditions
			return cond(c, v1alpha1.ConditionReady) == "True/Serving" && cond(c, v1alpha1.ConditionDegraded) != "" &&
				cond(e.zone("prim").Status.Conditions, v1alpha1.ConditionDegraded) != ""
		})
		c := e.zone("ext").Status.Conditions
		degraded := condition(c, v1alpha1.ConditionDegraded)
		if on {
			if degraded.Status != metav1.ConditionFalse {
				t.Fatalf("adapter running: Degraded %s/%s %q", degraded.Status, degraded.Reason, degraded.Message)
			}
			sources.set("Ingress: list denied")
			e.eventually("source health alone requeues the zone", func() bool {
				conditions := e.zone("ext").Status.Conditions
				return cond(conditions, v1alpha1.ConditionReady) == "True/Serving" &&
					cond(conditions, v1alpha1.ConditionDegraded) == "True/SourcesIncomplete" &&
					strings.Contains(condition(conditions, v1alpha1.ConditionDegraded).Message, "Ingress: list denied")
			})
			sources.set("")
			e.eventually("source recovery alone clears the condition", func() bool {
				return cond(e.zone("ext").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
			})
		} else if degraded.Status != metav1.ConditionTrue || degraded.Reason != ReasonSourcesNotRead || !strings.Contains(degraded.Message, "externalZones.enabled is off") {
			t.Fatalf("adapter off: Degraded %s/%s %q", degraded.Status, degraded.Reason, degraded.Message)
		}
		if got := cond(e.zone("prim").Status.Conditions, v1alpha1.ConditionDegraded); got != "False/AsExpected" {
			t.Fatalf("primary zone Degraded %s (adapter %v)", got, on)
		}
		e.stop()
	}
}

// On a store that cannot run the source adapter the message does not point
// at a setting that store refuses, and a zone that is already Degraded for
// a more specific reason keeps that reason (D384).
func TestWithoutSourcesNamesTheStoreAndKeepsSpecificReasons(t *testing.T) {
	off := withoutSources(verdict{}, false)
	if off.DegradedRsn != ReasonSourcesNotRead || !strings.Contains(off.DegradedMsg, "externalZones.enabled is off") {
		t.Fatalf("adapter off: %+v", off)
	}
	unsupported := withoutSources(verdict{}, true)
	if unsupported.DegradedRsn != ReasonSourcesNotRead || strings.Contains(unsupported.DegradedMsg, "is off") || !strings.Contains(unsupported.DegradedMsg, "only on the Kubernetes store") {
		t.Fatalf("store without the adapter: %+v", unsupported)
	}
	specific := withoutSources(verdict{Degraded: true, DegradedRsn: v1alpha1.ReasonDuplicated, DegradedMsg: "two record sets claim web"}, false)
	if !specific.Degraded || specific.DegradedRsn != v1alpha1.ReasonDuplicated || !strings.HasPrefix(specific.DegradedMsg, "two record sets claim web; ") || !strings.Contains(specific.DegradedMsg, "externalZones.enabled is off") {
		t.Fatalf("specific reason: %+v", specific)
	}
	incomplete := withSourceProblems(verdict{Degraded: true, DegradedRsn: v1alpha1.ReasonDuplicated, DegradedMsg: "two record sets claim web"}, []string{"Ingress: list denied"})
	if !incomplete.Degraded || incomplete.DegradedRsn != v1alpha1.ReasonDuplicated || !strings.HasPrefix(incomplete.DegradedMsg, "two record sets claim web; ") || !strings.Contains(incomplete.DegradedMsg, "Ingress: list denied") {
		t.Fatalf("sources incomplete with a specific reason: %+v", incomplete)
	}
}
