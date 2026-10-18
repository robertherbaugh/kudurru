package validator

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestZoneFallbackValidation(t *testing.T) {
	v := testValidator(t)
	for _, typ := range []v1alpha1.ZoneType{v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeForward, v1alpha1.ZoneTypeSecondary} {
		t.Run(string(typ), func(t *testing.T) {
			z := zoneOf(typedSample(t, "DNSZone"))
			z.Spec.Type = typ
			z.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{}
			err := v.Validate(ctx(), z, nil)
			if typ == v1alpha1.ZoneTypePrimary {
				if err != nil {
					t.Fatal(err)
				}
				if z.Spec.Fallback == nil || z.Spec.Fallback.UpstreamGroup != "" {
					t.Fatal("empty fallback must preserve normal resolver routing")
				}
			} else if err == nil {
				t.Fatal("fallback must be rejected on a non-primary zone")
			}
		})
	}
}
