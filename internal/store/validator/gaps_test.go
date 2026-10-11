package validator

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestSchemaGaps lists inputs the current CRDs accept although they are
// meaningless. The apiserver accepts them too, so the validator must as
// well: closing a gap is a marker change in api/v1alpha1 (see
// docs/spikes/store-validator.md), after which the case fails here and
// moves to invalidCases.
func TestSchemaGaps(t *testing.T) {
	v := testValidator(t)
	cases := []struct {
		name   string
		kind   store.Kind
		mutate func(store.Object)
	}{
		// `^([a-zA-Z0-9_-]+\.)*$` matches the empty string; zoneName needs
		// MinLength=1.
		{"empty zoneName", "DNSZone", func(o store.Object) { zoneOf(o).Spec.ZoneName = "" }},
		// ObjectRef.Name has no omitempty and no MinLength, so "" satisfies
		// `required`.
		{"empty zoneRef.name", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.ZoneRef.Name = "" }},
		{"empty dnsName", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints[0].DNSName = "" }},
		{"empty endpoints list", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints = []v1alpha1.Endpoint{} }},
		{"customIP response without customIP", "BlockList", func(o store.Object) { blOf(o).Spec.Response = "customIP" }},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+tc.name, func(t *testing.T) {
			obj := typedSample(t, tc.kind)
			tc.mutate(obj)
			if err := v.Validate(ctx(), obj, nil); err != nil {
				t.Fatalf("now rejected (good): move this case to invalidCases and update the spike report: %v", err)
			}
		})
	}
}
