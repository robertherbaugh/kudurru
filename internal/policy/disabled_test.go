package policy

import (
	"slices"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
)

func disable[T interface{ SetAnnotations(map[string]string) }](o T, value string) T {
	o.SetAnnotations(map[string]string{v1alpha1.EnabledAnnotation: value})
	return o
}

// TestDisabledObjectsAreAbsent: kudurru.io/enabled: "false" removes a
// ClientGroup, Policy or BlockList from the index; any other value keeps
// it.
func TestDisabledObjectsAreAbsent(t *testing.T) {
	lists := compile(t, map[string][]string{
		"dns/ads":    {"||ads.example^"},
		"dns/social": {"||social.example^"},
	}, map[string]blocking.Mode{})
	build := func(kidsOff, socialOff, strictOff string) *Index {
		return Build(Input{
			ClientGroups: []*v1alpha1.ClientGroup{
				group("dns", "lan", v1alpha1.ClientMatcher{CIDR: "10.0.0.0/8"}),
				disable(group("dns", "kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}), kidsOff),
			},
			Policies: []*v1alpha1.Policy{
				pol("dns", "lan", v1alpha1.PolicySpec{ClientGroupRefs: []string{"lan"}, BlockListRefs: []string{"ads"}}),
				disable(pol("dns", "strict", v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{"ads", "social"}, Priority: 10}), strictOff),
			},
			BlockLists: []*v1alpha1.BlockList{
				blockList("dns", "ads", "block"),
				disable(blockList("dns", "social", "block"), socialOff),
			},
			Lists: lists,
		}, Options{DefaultNamespace: "dns"})
	}
	now := time.Now()

	ix := build("true", "TRUE", "")
	if got := ix.Groups(); !slices.Equal(got, []string{"dns/kids", "dns/lan"}) {
		t.Fatalf("enabled groups %v", got)
	}
	d := ix.SelectPolicy(client("10.0.20.5"), now)
	if d.Policy != "dns/strict" || !slices.Equal(d.Selection.BlockLists(), []string{"dns/ads", "dns/social"}) {
		t.Fatalf("enabled: policy %q lists %v", d.Policy, d.Selection.BlockLists())
	}

	// The disabled list is skipped although it is still compiled.
	ix = build("true", "false", "true")
	d = ix.SelectPolicy(client("10.0.20.5"), now)
	if !slices.Equal(d.Selection.BlockLists(), []string{"dns/ads"}) {
		t.Fatalf("disabled list applied: %v", d.Selection.BlockLists())
	}
	if !hasDiag(ix, "Policy", "dns/strict", ReasonDisabled, `"social"`) || hasDiag(ix, "Policy", "dns/strict", ReasonMissingReference, "social") {
		t.Fatalf("diagnoses %v", ix.Diagnoses())
	}

	// A disabled policy no longer selects: kids (the longest prefix) has
	// no other policy and dns has no default policy, so nothing applies.
	ix = build("true", "true", " False ")
	if d := ix.SelectPolicy(client("10.0.20.5"), now); d.Applies() {
		t.Fatalf("disabled policy selected: %q", d.Policy)
	}

	// A disabled group drops its members (and the policy reference).
	ix = build("false", "true", "true")
	if got := ix.Groups(); !slices.Equal(got, []string{"dns/lan"}) {
		t.Fatalf("groups with kids disabled %v", got)
	}
	if d := ix.SelectPolicy(client("10.0.20.5"), now); d.Policy != "dns/lan" {
		t.Fatalf("disabled group still matched: %q", d.Policy)
	}
	if !hasDiag(ix, "Policy", "dns/strict", ReasonDisabled, `clientGroupRefs "kids"`) {
		t.Fatalf("diagnoses %v", ix.Diagnoses())
	}
}
