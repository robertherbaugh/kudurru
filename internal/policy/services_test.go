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

// catalogueDoc is a small schema 2 catalogue for the category tests.
func catalogueDoc(t *testing.T, services ...[3]string) *catalogue.Catalogue {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"schemaVersion":2,"version":"2099.01.01","license":"Apache-2.0","categories":[{"id":"ai","name":"AI","order":1},{"id":"video","name":"Video","order":2}],"services":[`)
	for i, s := range services {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"` + s[0] + `","name":"` + s[0] + `","category":"` + s[1] + `","rules":["||` + s[2] + `^"],"source":"test"}`)
	}
	b.WriteString(`],"safeSearch":[]}`)
	c, err := catalogue.Parse([]byte(b.String()), "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestBlockedCategories: blockedCategories block every service of the
// category, allowedServices except some of them (but never an ID listed
// in blockedServices), unknown categories are diagnosed, and a service a
// newer catalogue adds to a blocked category is blocked after the
// refresh (the snapshot and the Index are rebuilt from the new catalogue).
func TestBlockedCategories(t *testing.T) {
	h := catalogue.NewHolder(catalogueDoc(t,
		[3]string{"chatbot", "ai", "chat.example"}, [3]string{"painter", "ai", "paint.example"},
		[3]string{"helper", "ai", "help.example"}, [3]string{"tube", "video", "tube.example"}))
	in := Input{
		ClientGroups: []*v1alpha1.ClientGroup{group(ns, "default"), group(ns, "kids", v1alpha1.ClientMatcher{IP: "192.0.2.10"})},
		Policies: []*v1alpha1.Policy{
			pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}),
			pol(ns, "kids", v1alpha1.PolicySpec{
				ClientGroupRefs: []string{"kids"}, Priority: 10,
				BlockedServices:   []string{"tube", "helper"},
				BlockedCategories: []string{"ai", "no-such-category"},
				AllowedServices:   []string{"painter", "helper"},
			}),
		},
	}
	build := func() *Index {
		specs, _ := h.Lists()
		snap, _ := blocking.Rebuild(nil, specs, blocking.CompileOptions{})
		in.Lists = snap
		return Build(in, Options{Services: h})
	}
	ix := build()
	d := ix.SelectPolicy(client("192.0.2.10"), time.Now())
	if got := d.Selection.BlockLists(); !slices.Equal(got, []string{"service:tube", "service:helper", "service:chatbot"}) {
		t.Errorf("block lists = %v", got)
	}
	for q, want := range map[string]string{"www.chat.example.": "service:chatbot", "help.example.": "service:helper", "tube.example.": "service:tube", "paint.example.": ""} {
		bd := d.Selection.Evaluate(q)
		if bd.List != want || bd.Blocked() != (want != "") {
			t.Errorf("%s = %+v, want list %q", q, bd, want)
		}
	}
	var unknown bool
	for _, dg := range ix.Diagnoses() {
		unknown = unknown || (dg.Reason == ReasonUnknownCategory && strings.Contains(dg.Message, `"no-such-category"`))
	}
	if !unknown {
		t.Errorf("no UnknownCategory diagnosis: %v", ix.Diagnoses())
	}

	// A catalogue refresh adds a service to the blocked category.
	h.Set(catalogueDoc(t,
		[3]string{"chatbot", "ai", "chat.example"}, [3]string{"painter", "ai", "paint.example"},
		[3]string{"helper", "ai", "help.example"}, [3]string{"tube", "video", "tube.example"},
		[3]string{"newbot", "ai", "newbot.example"}))
	d = build().SelectPolicy(client("192.0.2.10"), time.Now())
	if bd := d.Selection.Evaluate("api.newbot.example."); bd.List != "service:newbot" {
		t.Errorf("a service added to a blocked category is not blocked: %+v", bd)
	}

	// The effective set, as the policy page shows it.
	ids, unknownCats := EffectiveServices(&in.Policies[1].Spec, h)
	if !slices.Equal(ids, []string{"tube", "helper", "chatbot", "newbot"}) || !slices.Equal(unknownCats, []string{"no-such-category"}) {
		t.Errorf("EffectiveServices = %v, %v", ids, unknownCats)
	}
	if ids, unknownCats := EffectiveServices(&in.Policies[1].Spec, nil); !slices.Equal(ids, []string{"tube", "helper"}) || len(unknownCats) != 2 {
		t.Errorf("EffectiveServices without a catalogue = %v, %v", ids, unknownCats)
	}
}

// TestImportedService: a policy blocks a service of the AdGuard import by
// its ID like any other (the fixture is hand-written, example domains).
func TestImportedService(t *testing.T) {
	imp, err := catalogue.ParseAdGuard([]byte(`{"blocked_services":[{"id":"example_stream","name":"Example Stream","group":"streaming","rules":["||stream.example^"]}]}`), "https://mirror.example/services.json")
	if err != nil {
		t.Fatal(err)
	}
	h := catalogue.NewHolder(nil)
	h.SetImport(imp)
	specs, _ := h.Lists()
	snap, _ := blocking.Rebuild(nil, specs, blocking.CompileOptions{})
	in := Input{
		ClientGroups: []*v1alpha1.ClientGroup{group(ns, "default")},
		Policies:     []*v1alpha1.Policy{pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockedServices: []string{"example-stream"}})},
		Lists:        snap,
	}
	d := Build(in, Options{Services: h}).SelectPolicy(client("192.0.2.1"), time.Now())
	if bd := d.Selection.Evaluate("cdn.stream.example."); bd.List != "service:example-stream" {
		t.Errorf("imported service = %+v", bd)
	}
	// Without the import the ID is unknown.
	h.SetImport(nil)
	specs, _ = h.Lists()
	in.Lists, _ = blocking.Rebuild(nil, specs, blocking.CompileOptions{})
	ix := Build(in, Options{Services: h})
	if len(ix.Diagnoses()) == 0 || ix.Diagnoses()[0].Reason != ReasonUnknownService {
		t.Errorf("diagnoses = %v", ix.Diagnoses())
	}
}
