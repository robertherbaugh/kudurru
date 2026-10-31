package validator

import (
	"fmt"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestBlockListClientGroupScope(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		name  string
		refs  []string
		valid bool
	}{
		{"general", nil, true}, {"general", []string{"kids", "adults"}, true}, {"general", []string{"kids", "kids"}, false},
		{"general", []string{""}, false}, {"general", []string{"bad/name"}, false}, {"general", []string{"a..b"}, false},
		{"general", []string{strings.Repeat("a", 254)}, false},
		{"ui-allow", nil, true}, {"ui-deny", nil, true}, {"ui-allow", []string{"kids"}, false}, {"ui-deny", []string{"kids"}, false},
		{"ui-allow-kids", nil, true}, {"ui-deny-kids", nil, true},
		{"ui-allow-kids", []string{"kids"}, true}, {"ui-deny-kids", []string{"kids"}, true},
		{"ui-allow-kids", []string{"adults"}, false}, {"ui-deny-kids", []string{"kids", "adults"}, false},
	} {
		t.Run(fmt.Sprintf("%s/%v", tc.name, tc.refs), func(t *testing.T) {
			bl := typedSample(t, "BlockList").(*public.BlockList)
			bl.Name = tc.name
			bl.Spec.ClientGroupRefs = tc.refs
			before := bl.DeepCopy()
			err := v.Validate(ctx(), bl, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !tc.valid && !equalJSON(t, before, bl) {
				t.Fatal("rejected scope mutated input")
			}
		})
	}
	bl := typedSample(t, "BlockList").(*public.BlockList)
	for i := range 65 {
		bl.Spec.ClientGroupRefs = append(bl.Spec.ClientGroupRefs, fmt.Sprintf("group-%d", i))
	}
	if err := v.Validate(ctx(), bl, nil); err == nil {
		t.Fatal("unbounded group scope accepted")
	}
}
