package policy

import (
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
)

func TestConvenienceListsSharedPolicyExactGroup(t *testing.T) {
	for _, mode := range []public.ListMode{"allow", "block"} {
		t.Run(string(mode), func(t *testing.T) {
			name := "ui-deny-kids"
			spec := public.PolicySpec{ClientGroupRefs: []string{"kids", "adults"}, BlockListRefs: []string{name}}
			bodies := map[string][]string{"default/" + name: {"exact.example", "*.wild.example", `/^regex[0-9]+\.example$/`}}
			modes := map[string]blocking.Mode{}
			lists := []*public.BlockList{blockList(ns, name, mode)}
			if mode == "allow" {
				name = "ui-allow-kids"
				bodies = map[string][]string{"default/" + name: {"exact.example", "*.wild.example", `/^regex[0-9]+\.example$/`}, "default/base": {"exact.example", "*.wild.example", `/^regex[0-9]+\.example$/`}}
				modes["default/"+name] = blocking.ModeAllow
				lists = []*public.BlockList{blockList(ns, name, mode), blockList(ns, "base", "block")}
				spec.AllowListRefs, spec.BlockListRefs = []string{name}, []string{"base"}
			}
			ix := Build(Input{ClientGroups: []*public.ClientGroup{group(ns, "kids", public.ClientMatcher{IP: "192.0.2.1"}), group(ns, "adults", public.ClientMatcher{IP: "192.0.2.2"})}, Policies: []*public.Policy{pol(ns, "shared", spec)}, BlockLists: lists, Lists: compile(t, bodies, modes)}, Options{})
			for range 3 { // alternate the same names through shared content/regex caches
				for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
					d := ix.SelectPolicy(client(ip), time.Now())
					for _, q := range []string{"exact.example.", "a.wild.example.", "regex42.example."} {
						got := d.Selection.Evaluate(q).Blocked()
						want := (ip == "192.0.2.1") == (mode == "block")
						if got != want {
							t.Errorf("%s %s blocked=%v, want %v", ip, q, got, want)
						}
					}
				}
			}
		})
	}
}

// Scope intersects only the winning captured tier, including ties. It is not
// implied by the policies attached to the groups or the default policy's name.
func TestListScopeWinningMembershipAndFallback(t *testing.T) {
	scoped := blockList(ns, "scoped", "block")
	scoped.Spec.ClientGroupRefs = []string{"kids"}
	everyone := blockList(ns, "ui-deny", "block")
	bodies := map[string][]string{"default/scoped": {"scoped.example"}, "default/ui-deny": {"everyone.example"}}
	groups := []*public.ClientGroup{
		group(ns, "kids", public.ClientMatcher{IP: "192.0.2.1"}, public.ClientMatcher{IP: "192.0.2.3"}, public.ClientMatcher{CIDR: "192.0.2.0/24"}),
		group(ns, "adults", public.ClientMatcher{IP: "192.0.2.2"}, public.ClientMatcher{IP: "192.0.2.3"}),
		group("other", "kids", public.ClientMatcher{IP: "198.51.100.1"}),
		disable(group(ns, "disabled", public.ClientMatcher{IP: "192.0.2.4"}), "false"),
	}
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "fallback"}[fallback], func(t *testing.T) {
			spec := public.PolicySpec{ClientGroupRefs: []string{"kids", "adults"}, BlockListRefs: []string{"scoped", "ui-deny"}}
			name := "shared"
			if fallback {
				name = "default"
				spec.ClientGroupRefs = nil
			}
			ix := Build(Input{ClientGroups: groups, Policies: []*public.Policy{pol(ns, name, spec)}, BlockLists: []*public.BlockList{scoped, everyone}, Lists: compile(t, bodies, nil)}, Options{})
			for _, tc := range []struct {
				ip   string
				want bool
			}{
				{"192.0.2.1", true}, {"192.0.2.2", false}, {"192.0.2.3", true}, {"192.0.2.4", true}, {"198.51.100.1", false}, {"203.0.113.1", false},
			} {
				m := ix.Identify(client(tc.ip))
				d := ix.Select(m, time.Now())
				got := d.Selection != nil && d.Selection.Evaluate("scoped.example.").Blocked()
				if got != tc.want {
					t.Errorf("%s scoped=%v, want %v", tc.ip, got, tc.want)
				}
				// A caller-provided diagnostic group list cannot grant another group's scope.
				forged := &Membership{GroupIDs: []string{"default/kids"}}
				if m != nil {
					forged.set = m.set
				}
				fd := ix.Select(forged, time.Now())
				if got := fd.Selection != nil && fd.Selection.Evaluate("scoped.example.").Blocked(); got != tc.want {
					t.Fatal("diagnostic GroupIDs granted scope")
				}
			}
			if fallback {
				if d := ix.Select(nil, time.Now()); !d.Fallback || d.Selection.Evaluate("scoped.example.").Blocked() || !d.Selection.Evaluate("everyone.example.").Blocked() {
					t.Fatal("no-membership fallback widened list scope")
				}
				for _, g := range []string{"default/kids", "default/adults", "other/kids"} {
					d, ok := ix.GroupDecision(g, time.Now())
					if !ok || d.Selection.Evaluate("scoped.example.").Blocked() != (g == "default/kids") {
						t.Fatal("GroupDecision fallback lost original group", g)
					}
				}
			}
		})
	}
}

func TestListScopeCapturedCopyAndMissingGroups(t *testing.T) {
	bl := blockList(ns, "scoped", "block")
	bl.Spec.ClientGroupRefs = []string{"kids"}
	bodies := map[string][]string{"default/scoped": {`/^cache[0-9]+\.example$/`}}
	snap := compile(t, bodies, nil)
	in := Input{ClientGroups: []*public.ClientGroup{group(ns, "kids", public.ClientMatcher{IP: "192.0.2.1"}), group(ns, "adults", public.ClientMatcher{IP: "192.0.2.2"})}, Policies: []*public.Policy{pol(ns, "shared", public.PolicySpec{ClientGroupRefs: []string{"kids", "adults"}, BlockListRefs: []string{"scoped"}})}, BlockLists: []*public.BlockList{bl}, Lists: snap}
	old := Build(in, Options{})
	bl.Spec.ClientGroupRefs[0] = "adults"
	fresh := Build(in, Options{}) // same compiled matcher and caches, successor metadata
	for range 4 {
		for _, ix := range []*Index{old, fresh} {
			for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
				want := (ix == old) == (ip == "192.0.2.1")
				if got := ix.SelectPolicy(client(ip), time.Now()).Selection.Evaluate("cache42.example.").Blocked(); got != want {
					t.Fatal("scope/matcher cache crossed captured index")
				}
			}
		}
	}
	bl.Spec.ClientGroupRefs[0] = "missing"
	missing := Build(in, Options{})
	if missing.SelectPolicy(client("192.0.2.1"), time.Now()).Selection.Evaluate("cache42.example.").Blocked() {
		t.Fatal("missing scope became unrestricted")
	}
	in.ClientGroups[0] = disable(in.ClientGroups[0], "false")
	bl.Spec.ClientGroupRefs[0] = "kids"
	off := Build(in, Options{})
	if off.SelectPolicy(client("192.0.2.2"), time.Now()).Selection.Evaluate("cache42.example.").Blocked() {
		t.Fatal("disabled scope became unrestricted")
	}
}
