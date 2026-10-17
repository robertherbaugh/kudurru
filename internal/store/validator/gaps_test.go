package validator

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestSchemaGaps lists inputs the current CRDs accept although they are
// meaningless. The apiserver accepts them too, so the validator must as
// well: closing a gap is a marker change in api/v1alpha1 (see
// dev-docs/spikes/store-validator.md), after which the case fails here and
// moves to invalidCases.
func TestSchemaGaps(t *testing.T) {
	v := testValidator(t)
	cases := []struct {
		name   string
		kind   store.Kind
		mutate func(store.Object)
	}{
		// An empty record set is a valid placeholder (nothing to serve).
		{"empty endpoints list", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints = []v1alpha1.Endpoint{} }},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+tc.name, func(t *testing.T) {
			obj := typedSample(t, tc.kind)
			tc.mutate(obj)
			if err := v.Validate(ctx(), obj, nil); err != nil {
				t.Fatalf("now rejected (good): move this case to TestClosedGaps and update the spike report: %v", err)
			}
		})
	}
}

// TestClosedGaps pins the gaps the spike found and api/v1alpha1 has since
// closed (MinLength=1 on names, the customIP one-of rule).
func TestClosedGaps(t *testing.T) {
	v := testValidator(t)
	cases := []struct {
		name   string
		kind   store.Kind
		mutate func(store.Object)
	}{
		{"empty zoneName", "DNSZone", func(o store.Object) { zoneOf(o).Spec.ZoneName = "" }},
		{"empty zoneRef.name", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.ZoneRef.Name = "" }},
		{"empty dnsName", "DNSRecordSet", func(o store.Object) { rrsetOf(o).Spec.Endpoints[0].DNSName = "" }},
		{"customIP response without customIP", "BlockList", func(o store.Object) { blOf(o).Spec.Response = "customIP" }},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+tc.name, func(t *testing.T) {
			obj := typedSample(t, tc.kind)
			tc.mutate(obj)
			if err := v.Validate(ctx(), obj, nil); err == nil {
				t.Fatal("accepted; the api/v1alpha1 marker that closed this gap is missing")
			}
		})
	}
}
