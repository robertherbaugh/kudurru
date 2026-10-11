package policy

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

// TestBlockedServices: blockedServices IDs resolve to the catalogue's
// compiled "service:<id>" lists and block like block lists, allow lists
// still win, and unknown or not yet compiled IDs are diagnosed.
func TestBlockedServices(t *testing.T) {
	cat := catalogue.Embedded()
	var lists []*blocking.List
	snap, _ := blocking.Rebuild(nil, cat.Specs(), blocking.CompileOptions{})
	lists = append(lists, snap.Lists()...)
	allow := compile(t, map[string][]string{"default/allow": {"music.youtube.com"}}, map[string]blocking.Mode{"default/allow": blocking.ModeAllow})
	lists = append(lists, allow.Lists()...)
	all, err := blocking.NewSnapshot(2, lists...)
	if err != nil {
		t.Fatal(err)
	}
	in := Input{
		ClientGroups: []*v1alpha1.ClientGroup{group(ns, "default"), group(ns, "kids", v1alpha1.ClientMatcher{IP: "192.0.2.10"})},
		BlockLists:   []*v1alpha1.BlockList{blockList(ns, "allow", "allow", "music.youtube.com")},
		Policies: []*v1alpha1.Policy{
			pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}),
			pol(ns, "kids", v1alpha1.PolicySpec{
				ClientGroupRefs: []string{"kids"}, AllowListRefs: []string{"allow"}, Priority: 10,
				BlockedServices: []string{"youtube", "tiktok", "no-such-service"},
				SafeSearch:      ptr(true), SafeSearchMode: catalogue.ModeModerate,
			}),
		},
		Lists: all,
	}
	ix := Build(in, Options{Services: catalogue.NewHolder(nil)})
	d := ix.SelectPolicy(client("192.0.2.10"), time.Now())
	if d.Policy != "default/kids" || !d.Filters() || !d.SafeSearchOn() || d.SafeSearchMode != catalogue.ModeModerate {
		t.Fatalf("decision = %+v", d)
	}
	if got := d.Selection.BlockLists(); !slices.Equal(got, []string{"service:youtube", "service:tiktok"}) {
		t.Errorf("block lists = %v", got)
	}
	bd := d.Selection.Evaluate("www.youtube.com.")
	if !bd.Blocked() || bd.List != "service:youtube" || bd.Rule() != "||youtube.com^" || bd.Status().String() != "blocked_list" {
		t.Errorf("www.youtube.com = %+v", bd)
	}
	if bd := d.Selection.Evaluate("music.youtube.com."); bd.Action != blocking.ActionAllow || bd.Reason != blocking.ReasonAllowList {
		t.Errorf("an allow list must win over a blocked service: %+v", bd)
	}
	if bd := d.Selection.Evaluate("v16.tiktokcdn.com."); bd.List != "service:tiktok" {
		t.Errorf("tiktok cdn = %+v", bd)
	}
	if bd := d.Selection.Evaluate("www.netflix.com."); bd.Blocked() {
		t.Errorf("a service the policy does not list was blocked: %+v", bd)
	}
	var unknown bool
	for _, dg := range ix.Diagnoses() {
		if dg.Reason == ReasonUnknownService && dg.Object == "default/kids" && strings.Contains(dg.Message, `"no-such-service"`) {
			unknown = true
		}
	}
	if !unknown {
		t.Errorf("no UnknownService diagnosis: %v", ix.Diagnoses())
	}

	// The default policy filters nothing: no lists, no safe search.
	if d := ix.SelectPolicy(client("192.0.2.99"), time.Now()); d.Filters() || d.SafeSearchOn() {
		t.Errorf("default decision = %+v", d)
	}

	// Before the resolver compiled the catalogue, known IDs are NotLoaded
	// (readiness waits for them) and unknown ones stay UnknownService.
	in.Lists = allow
	ix = Build(in, Options{Services: catalogue.NewHolder(nil)})
	if nl := ix.NotLoaded(); !slices.Equal(nl, []string{"service:tiktok", "service:youtube"}) {
		t.Errorf("NotLoaded = %v", nl)
	}
	reasons := map[string]int{}
	for _, dg := range ix.Diagnoses() {
		reasons[dg.Reason]++
	}
	if reasons[ReasonNotLoaded] != 2 || reasons[ReasonUnknownService] != 1 {
		t.Errorf("diagnoses = %v", ix.Diagnoses())
	}
	// Safe search alone makes a decision filter.
	d = ix.SelectPolicy(client("192.0.2.10"), time.Now())
	if !d.Filters() || len(d.Selection.BlockLists()) != 0 {
		t.Errorf("safe-search-only decision = %+v", d)
	}
}
