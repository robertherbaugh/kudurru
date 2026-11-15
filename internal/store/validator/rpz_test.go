package validator

import (
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestRPZSourceConfiguration(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		name   string
		change func(*v1alpha1.BlockList)
		valid  bool
	}{
		{"tcp", func(*v1alpha1.BlockList) {}, true},
		{"tls", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.TransferProtocol = "tls" }, true},
		{"default protocol", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.TransferProtocol = "" }, true},
		{"no trailing dot", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.ZoneName = "policy.example" }, true},
		{"source plus URL", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].URL = "https://lists.example/" }, false},
		{"source plus inline", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].Inline = []string{"ads.example"} }, false},
		{"source plus ConfigMap", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].ConfigMapRef = &v1alpha1.ObjectRef{Name: "list"} }, false},
		{"source plus entries", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].Entries = []v1alpha1.ListEntry{{Value: "ads.example"}} }, false},
		{"RPZ format without block", func(l *v1alpha1.BlockList) {
			l.Spec.Sources[0].RPZ = nil
			l.Spec.Sources[0].URL = "https://lists.example/"
		}, false},
		{"RPZ block with auto format", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].Format = "auto" }, false},
		{"RPZ block with adblock format", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].Format = "adblock" }, false},
		{"allow mode", func(l *v1alpha1.BlockList) { l.Spec.Mode = "allow" }, false},
		{"subdomains override", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].Subdomains = true }, false},
		{"UDP", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.TransferProtocol = "udp" }, false},
		{"no primaries", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.Primaries = nil }, false},
		{"empty primaries", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.Primaries = []string{} }, false},
		{"empty primary", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.Primaries = []string{""} }, false},
		{"long primary", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.Primaries = []string{strings.Repeat("a", 301)} }, false},
		{"too many primaries", func(l *v1alpha1.BlockList) {
			l.Spec.Sources[0].RPZ.Primaries = make([]string, 33)
			for i := range l.Spec.Sources[0].RPZ.Primaries {
				l.Spec.Sources[0].RPZ.Primaries[i] = "primary.example"
			}
		}, false},
		{"empty origin", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.ZoneName = "" }, false},
		{"root origin", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.ZoneName = "." }, false},
		{"whitespace origin", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.ZoneName = "bad name.example." }, false},
		{"long origin", func(l *v1alpha1.BlockList) { l.Spec.Sources[0].RPZ.ZoneName = strings.Repeat("a.", 128) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := typedSample(t, "BlockList").(*v1alpha1.BlockList)
			list.Spec.Mode = "block"
			list.Spec.Sources = []v1alpha1.ListSource{{Format: "rpz", RPZ: &v1alpha1.RPZSource{ZoneName: "policy.example.", Primaries: []string{"primary.example:5300"}, TransferProtocol: "tcp"}}}
			tc.change(list)
			before := list.DeepCopy()
			err := v.Validate(ctx(), list, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
			if !tc.valid && !equalJSON(t, before, list) {
				t.Fatal("rejected input was mutated")
			}
		})
	}
}
