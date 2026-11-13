package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// An external zone on a server without the source adapter
// (externalZones.enabled off) is served but Degraded{SourcesNotRead}, so an
// operator sees why it stays empty; with the adapter it is Ready and not
// Degraded. A primary zone is unaffected by the setting.
func TestExternalZoneWithoutSourcesIsDegraded(t *testing.T) {
	external := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "ext"}, Spec: v1alpha1.DNSZoneSpec{
		ZoneName: "ext.example.", Type: v1alpha1.ZoneTypeExternal, Nameservers: []string{"ns.ext.example."},
		External: &v1alpha1.ExternalSpec{Ingress: true}}}
	for _, on := range []bool{false, true} {
		e := newEnv(t, func(o *Options) { o.ExternalZoneSources = on })
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
		} else if degraded.Status != metav1.ConditionTrue || degraded.Reason != ReasonSourcesNotRead || !strings.Contains(degraded.Message, "externalZones.enabled is off") {
			t.Fatalf("adapter off: Degraded %s/%s %q", degraded.Status, degraded.Reason, degraded.Message)
		}
		if got := cond(e.zone("prim").Status.Conditions, v1alpha1.ConditionDegraded); got != "False/AsExpected" {
			t.Fatalf("primary zone Degraded %s (adapter %v)", got, on)
		}
		e.stop()
	}
}
