package validator

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestSecondaryConfigurationValidation(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		name      string
		secondary *v1alpha1.SecondarySpec
		valid     bool
	}{
		{name: "missing block"},
		{name: "missing primaries", secondary: &v1alpha1.SecondarySpec{}},
		{name: "empty primaries", secondary: &v1alpha1.SecondarySpec{Primaries: []string{}}},
		{name: "empty endpoint", secondary: &v1alpha1.SecondarySpec{Primaries: []string{""}}},
		{name: "tcp", valid: true, secondary: &v1alpha1.SecondarySpec{Primaries: []string{"primary.example:5300"}, TransferProtocol: "tcp"}},
		{name: "tls", valid: true, secondary: &v1alpha1.SecondarySpec{Primaries: []string{"[2001:db8::1]:853"}, TransferProtocol: "tls"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := typedSample(t, "DNSZone").(*v1alpha1.DNSZone)
			z.Spec.Type = v1alpha1.ZoneTypeSecondary
			z.Spec.Secondary = tc.secondary
			err := v.Validate(ctx(), z, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
