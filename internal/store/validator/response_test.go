package validator

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestPolicyResponseSchema(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		prefix string
		valid  bool
	}{
		{"", true}, {"64:ff9b::/96", true}, {"2001:db8::/32", true},
		{"192.0.2.0/24", false}, {"64:ff9b::1/96", false}, {"2001:db8::/72", false}, {"ff00::/96", false}, {"junk", false},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			p := typedSample(t, "Policy").(*v1alpha1.Policy)
			p.Spec.Response = &v1alpha1.PolicyResponse{DNS64Prefix: &tc.prefix, FilterAAAA: new(false)}
			err := v.Validate(ctx(), p, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if err == nil && (p.Spec.Response.DNS64Prefix == nil || *p.Spec.Response.DNS64Prefix != tc.prefix || p.Spec.Response.FilterAAAA == nil || *p.Spec.Response.FilterAAAA) {
				t.Fatal("explicit overrides lost")
			}
		})
	}
	for _, rewrite := range []v1alpha1.Rewrite{{Name: "example"}, {Name: "example", CNAME: "alias.example", Targets: []string{"192.0.2.1"}}, {Name: "example", Type: "SOA", Targets: []string{"bad"}}} {
		p := typedSample(t, "Policy").(*v1alpha1.Policy)
		p.Spec.Rewrites = []v1alpha1.Rewrite{rewrite}
		if err := v.Validate(ctx(), p, nil); err == nil {
			t.Fatalf("invalid rewrite accepted: %+v", rewrite)
		}
	}
}
