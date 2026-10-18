package policy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// identificationFixture mirrors a home network: the bootstrap default
// group, a kids VLAN with a narrower printer exception, a laptop by IP, a
// PS5 by hostname glob, a TV by exact hostname, a phone by DoT client ID
// and two groups that both claim the same /24 (a tie).
func identificationFixture() Input {
	return Input{ClientGroups: []*v1alpha1.ClientGroup{
		group(ns, "default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}, v1alpha1.ClientMatcher{CIDR: "::/0"}),
		group(ns, "kids",
			v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24", Name: "Kids VLAN"},
			v1alpha1.ClientMatcher{NameGlob: "ps5*"},
			v1alpha1.ClientMatcher{CIDR: "fd00:20::/64"}),
		group(ns, "printers", v1alpha1.ClientMatcher{CIDR: "10.0.20.240/28"}),
		group(ns, "laptop", v1alpha1.ClientMatcher{IP: "10.0.20.7"}, v1alpha1.ClientMatcher{IP: "::ffff:10.0.30.7"}),
		group(ns, "tv", v1alpha1.ClientMatcher{Hostname: "Living-Room-TV."}),
		group(ns, "consoles", v1alpha1.ClientMatcher{NameGlob: "*-x"}),
		group(ns, "phone", v1alpha1.ClientMatcher{ClientID: "Pixel"}),
		group(ns, "lab-a", v1alpha1.ClientMatcher{CIDR: "10.0.99.0/24"}),
		group(ns, "lab-b", v1alpha1.ClientMatcher{CIDR: "10.0.99.0/24"}, v1alpha1.ClientMatcher{MAC: "AA-BB-CC-DD-EE-FF"}),
		group("other", "kids", v1alpha1.ClientMatcher{IP: "192.0.2.1"}),
	}}
}

func TestIdentificationMatrix(t *testing.T) {
	hosts := staticHosts{
		netip.MustParseAddr("192.168.1.50"): {"ps5-kids.lan"},
		netip.MustParseAddr("192.168.1.60"): {"living-room-tv"},
		netip.MustParseAddr("10.0.20.8"):    {"ps5-other"},
		netip.MustParseAddr("192.168.1.70"): {"unknown-box"},
		netip.MustParseAddr("192.168.1.80"): {"LIVING-ROOM-TV.", "ps5-x"},
		netip.MustParseAddr("192.168.1.81"): {"ps5-x"},
	}
	ix := Build(identificationFixture(), Options{Hostnames: hosts})

	cases := []struct {
		name   string
		client pipeline.Client
		want   []string
		tier   Tier
	}{
		{"exact IP beats the containing /24", client("10.0.20.7"), []string{"default/laptop"}, TierIP},
		{"IPv4-mapped client matches IPv4 entries", client("::ffff:10.0.20.7"), []string{"default/laptop"}, TierIP},
		{"IPv4-mapped entry matches an IPv4 client", client("10.0.30.7"), []string{"default/laptop"}, TierIP},
		{"/28 beats /24", client("10.0.20.241"), []string{"default/printers"}, TierCIDR},
		{"/24 member", client("10.0.20.9"), []string{"default/kids"}, TierCIDR},
		{"IPv6 /64", client("fd00:20::1234"), []string{"default/kids"}, TierCIDR},
		{"CIDR beats hostname", client("10.0.20.8"), []string{"default/kids"}, TierCIDR},
		{"hostname glob beats the catch-all", client("192.168.1.50"), []string{"default/kids"}, TierNameGlob},
		{"exact hostname, case and dot insensitive", client("192.168.1.60"), []string{"default/tv"}, TierHostname},
		{"exact hostname beats glob", client("192.168.1.80"), []string{"default/tv"}, TierHostname},
		{"two globs join both groups", client("192.168.1.81"), []string{"default/consoles", "default/kids"}, TierNameGlob},
		{"unknown hostname falls to catch-all", client("192.168.1.70"), []string{"default/default"}, TierCatchAll},
		{"IPv6 catch-all", client("2001:db8::1"), []string{"default/default"}, TierCatchAll},
		{"equal-length CIDR tie joins both groups", client("10.0.99.5"), []string{"default/lab-a", "default/lab-b"}, TierCIDR},
		{"same group name in another namespace", client("192.0.2.1"), []string{"other/kids"}, TierIP},
		{"clientID beats the catch-all", pipeline.Client{Addr: netip.MustParseAddrPort("192.168.1.90:5000"), ClientID: "pixel"}, []string{"default/phone"}, TierClientID},
		{"MAC slot", pipeline.Client{Addr: netip.MustParseAddrPort("192.168.1.91:5000"), MACs: []string{"aa:bb:cc:dd:ee:ff"}}, []string{"default/lab-b"}, TierMAC},
		{"no address: default group", pipeline.Client{}, []string{"default/default"}, TierDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := ix.Identify(tc.client)
			if !slices.Equal(m.GroupIDs, tc.want) || m.Tier != tc.tier {
				t.Errorf("Identify = %v (%s), want %v (%s)", m.GroupIDs, m.Tier, tc.want, tc.tier)
			}
		})
	}

	// Dynamic multi-group memberships are interned.
	if a, b := ix.Identify(client("192.168.1.81")), ix.Identify(client("192.168.1.81")); a != b {
		t.Error("multi-group hostname memberships must be cached")
	}

	var ties []string
	for _, d := range ix.Diagnoses() {
		if d.Reason == ReasonTie {
			ties = append(ties, d.Object+": "+d.Message)
		}
	}
	want := []string{
		"default/lab-a: cidr 10.0.99.0/24 is also listed by default/lab-b; matching clients join all of them",
		"default/lab-b: cidr 10.0.99.0/24 is also listed by default/lab-a; matching clients join all of them",
	}
	if !slices.Equal(ties, want) {
		t.Errorf("tie diagnoses:\n got %q\nwant %q", ties, want)
	}
}

func TestIdentificationWithoutDefaultGroup(t *testing.T) {
	in := Input{
		ClientGroups: []*v1alpha1.ClientGroup{group(ns, "kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}, v1alpha1.ClientMatcher{Hostname: "ps5"})},
		Policies:     []*v1alpha1.Policy{pol(ns, "default", v1alpha1.PolicySpec{Priority: 0})},
	}
	ix := Build(in, Options{})
	m := ix.Identify(client("192.168.1.1"))
	if len(m.GroupIDs) != 0 || m.Tier != TierDefault {
		t.Fatalf("no default group: got %v (%s)", m.GroupIDs, m.Tier)
	}
	if d := ix.Select(m, time.Now()); d.Policy != "default/default" || !d.Fallback {
		t.Errorf("fallback policy expected, got %+v", d)
	}
	if !hasDiag(ix, "ClientGroup", "default/kids", ReasonUnsupported, "hostname source") {
		t.Errorf("hostname matcher without a resolver must be diagnosed: %v", ix.Diagnoses())
	}
}

func TestReleaseNamespaceFallback(t *testing.T) {
	ix := Build(Input{
		ClientGroups: []*v1alpha1.ClientGroup{group("dns", "default")},
		Policies: []*v1alpha1.Policy{
			pol("default", "default", v1alpha1.PolicySpec{BlockMode: "zeroIP"}),
			pol("dns", "default", v1alpha1.PolicySpec{BlockMode: "nxdomain"}),
		},
	}, Options{DefaultNamespace: "dns"})
	m := ix.Identify(pipeline.Client{})
	if !slices.Equal(m.GroupIDs, []string{"dns/default"}) {
		t.Fatalf("default group = %v, want dns/default", m.GroupIDs)
	}
	if d := ix.Select(m, time.Now()); d.Policy != "dns/default" || !d.Fallback || d.BlockMode != "nxdomain" {
		t.Fatalf("release policy must be the fallback: %+v", d)
	}
}

func TestInvalidMatchersAreDiagnosed(t *testing.T) {
	ix := Build(Input{ClientGroups: []*v1alpha1.ClientGroup{group(ns, "bad",
		v1alpha1.ClientMatcher{IP: "10.0.0.300"},
		v1alpha1.ClientMatcher{CIDR: "10.0.0.0/33"},
		v1alpha1.ClientMatcher{CIDR: "10.0.20.5/24"},
		v1alpha1.ClientMatcher{MAC: "not-a-mac"},
		v1alpha1.ClientMatcher{NameGlob: "ps5["},
		v1alpha1.ClientMatcher{Name: "nothing set"},
	)}}, Options{})
	for _, want := range []string{"clients[0].ip", "clients[1].cidr", "has host bits set; read as 10.0.20.0/24", "clients[3].mac", "clients[4] glob", "clients[5] sets no matcher"} {
		if !hasDiag(ix, "ClientGroup", "default/bad", ReasonInvalidMatcher, want) {
			t.Errorf("missing diagnosis %q in %v", want, ix.Diagnoses())
		}
	}
	// The host-bits entry still works, as its masked prefix.
	if m := ix.Identify(client("10.0.20.77")); !slices.Equal(m.GroupIDs, []string{"default/bad"}) {
		t.Errorf("masked prefix should match: %v", m.GroupIDs)
	}
}

func hasDiag(ix *Index, kind, obj, reason, substr string) bool {
	for _, d := range ix.Diagnoses() {
		if d.Kind == kind && d.Object == obj && d.Reason == reason && strings.Contains(d.Message, substr) {
			return true
		}
	}
	return false
}

func TestPolicyPriorityMerge(t *testing.T) {
	lists := compile(t, map[string][]string{
		"default/ads":     {"||ads.example^"},
		"default/adult":   {"||adult.example^"},
		"default/school":  {"||school.example^"},
		"default/gaming":  {"||games.example^"},
		"default/social":  {"||social.example^"},
		"default/trusted": {"||cdn.ads.example^"},
	}, map[string]blocking.Mode{"default/school": blocking.ModeAllow, "default/trusted": blocking.ModeAllow})
	in := Input{
		ClientGroups: []*v1alpha1.ClientGroup{
			group(ns, "default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}),
			group(ns, "kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}),
			group(ns, "gamers", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}),
		},
		Policies: []*v1alpha1.Policy{
			pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads"}, BlockTTL: dur(10 * time.Minute)}),
			pol(ns, "kids", v1alpha1.PolicySpec{
				ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{"ads", "adult"}, AllowListRefs: []string{"school"},
				BlockMode: "nxdomain", BlockTTL: dur(time.Minute), Priority: 10, QueryLog: ptr(false),
			}),
			pol(ns, "gamers", v1alpha1.PolicySpec{
				ClientGroupRefs: []string{"gamers"}, BlockListRefs: []string{"gaming"}, AllowListRefs: []string{"trusted"},
				BlockMode: "zeroIP", UpstreamGroup: "filtered", Priority: 5, SafeSearch: ptr(true), AllowOnly: true,
			}),
			pol(ns, "kids-extra", v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{"social"}, Priority: 10}),
		},
		BlockLists: []*v1alpha1.BlockList{
			blockList(ns, "ads", "block"), blockList(ns, "adult", "block"), blockList(ns, "school", "allow"),
			blockList(ns, "gaming", "block"), blockList(ns, "social", "block"), blockList(ns, "trusted", "allow"),
		},
		Lists: lists,
	}
	ix := Build(in, Options{})
	d := ix.SelectPolicy(client("10.0.20.9"), time.Now())

	if want := []string{"default/kids", "default/kids-extra", "default/gamers"}; !slices.Equal(d.Policies, want) {
		t.Fatalf("policies %v, want %v (priority desc, then name)", d.Policies, want)
	}
	if d.Policy != "default/kids" || d.Fallback {
		t.Errorf("top policy %q fallback=%v", d.Policy, d.Fallback)
	}
	if d.BlockMode != "nxdomain" || d.BlockTTL != time.Minute {
		t.Errorf("blockMode/TTL from the highest priority: %q %s", d.BlockMode, d.BlockTTL)
	}
	if d.UpstreamGroup != "filtered" || d.SafeSearch == nil || !*d.SafeSearch {
		t.Errorf("settings only a lower-priority policy sets must still apply: %q %v", d.UpstreamGroup, d.SafeSearch)
	}
	if d.QueryLog == nil || *d.QueryLog {
		t.Errorf("queryLog from kids: %v", d.QueryLog)
	}
	if d.AllowOnly {
		t.Error("allowOnly comes from the highest-priority policy (kids: false)")
	}
	if got, want := d.Selection.BlockLists(), []string{"default/ads", "default/adult", "default/social", "default/gaming"}; !slices.Equal(got, want) {
		t.Errorf("block lists %v, want %v", got, want)
	}
	if got, want := d.Selection.AllowLists(), []string{"default/school", "default/trusted"}; !slices.Equal(got, want) {
		t.Errorf("allow lists %v, want %v", got, want)
	}
	// Allow wins across groups: gamers' trusted list unblocks a kids deny.
	if dec := d.Selection.Evaluate("cdn.ads.example."); dec.Action != blocking.ActionAllow || dec.List != "default/trusted" {
		t.Errorf("allow across groups: %+v", dec)
	}
	if dec := d.Selection.Evaluate("x.games.example."); !dec.Blocked() || dec.List != "default/gaming" {
		t.Errorf("gamers' list applies to the joint membership: %+v", dec)
	}
	if !d.Filters() || !d.Applies() {
		t.Error("decision must filter")
	}

	// A client only in the default group gets Policy/default normally.
	dd := ix.SelectPolicy(client("192.168.1.1"), time.Now())
	if dd.Policy != "default/default" || dd.Fallback || dd.BlockTTL != 10*time.Minute {
		t.Errorf("default policy: %+v", dd)
	}
	// Decisions are shared.
	if ix.SelectPolicy(client("10.0.20.10"), time.Now()) != d {
		t.Error("decisions of one group set must be precomputed and shared")
	}
}

func TestScheduledPolicyAndFallback(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	in := Input{
		ClientGroups: []*v1alpha1.ClientGroup{
			group(ns, "default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}),
			group(ns, "kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}),
			group(ns, "guests", v1alpha1.ClientMatcher{CIDR: "10.0.30.0/24"}),
		},
		Policies: []*v1alpha1.Policy{
			pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}),
			pol(ns, "kids-school", v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, Priority: 10, BlockMode: "nxdomain",
				Schedule: &v1alpha1.Schedule{Weekdays: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "08:00", End: "20:00", TZ: "Europe/Berlin"}}),
			pol(ns, "kids-night", v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, Priority: 20, BlockMode: "refused",
				Schedule: &v1alpha1.Schedule{Start: "21:00", End: "07:00", TZ: "Europe/Berlin"}}),
			pol(ns, "broken", v1alpha1.PolicySpec{ClientGroupRefs: []string{"guests"}, Priority: 1, BlockMode: "customIP",
				Schedule: &v1alpha1.Schedule{TZ: "Nowhere/Land"}}),
		},
	}
	ix := Build(in, Options{})
	kids := client("10.0.20.9")
	cases := []struct {
		at       time.Time
		policy   string
		mode     v1alpha1.BlockResponse
		fallback bool
	}{
		{time.Date(2026, 10, 12, 9, 0, 0, 0, berlin), "default/kids-school", "nxdomain", false},
		{time.Date(2026, 10, 12, 20, 30, 0, 0, berlin), "default/default", "", true},
		{time.Date(2026, 10, 12, 22, 0, 0, 0, berlin), "default/kids-night", "refused", false},
		{time.Date(2026, 10, 13, 6, 59, 0, 0, berlin), "default/kids-night", "refused", false},
		{time.Date(2026, 10, 17, 12, 0, 0, 0, berlin), "default/default", "", true},
	}
	for _, tc := range cases {
		d := ix.SelectPolicy(kids, tc.at)
		if d.Policy != tc.policy || d.BlockMode != tc.mode || d.Fallback != tc.fallback {
			t.Errorf("%s: got %s mode=%q fallback=%v, want %s mode=%q fallback=%v",
				tc.at.Format(time.RFC3339), d.Policy, d.BlockMode, d.Fallback, tc.policy, tc.mode, tc.fallback)
		}
	}
	// Both windows overlap 21:00-07:00 on a weekday morning? No: school
	// starts at 08:00. Overlap at 07:30 is neither.
	if d := ix.SelectPolicy(kids, time.Date(2026, 10, 13, 7, 30, 0, 0, berlin)); !d.Fallback {
		t.Errorf("07:30 should fall back, got %s", d.Policy)
	}
	// An unparsable schedule fails closed: always active, diagnosed.
	if d := ix.SelectPolicy(client("10.0.30.1"), time.Now()); d.Policy != "default/broken" {
		t.Errorf("invalid schedule must apply at all times, got %s", d.Policy)
	}
	if !hasDiag(ix, "Policy", "default/broken", ReasonInvalidSchedule, "tz") {
		t.Errorf("invalid schedule must be diagnosed: %v", ix.Diagnoses())
	}
	// Group decisions for the gauge and the UI.
	if d, ok := ix.GroupDecision("default/kids", time.Date(2026, 10, 12, 9, 0, 0, 0, berlin)); !ok || d.Policy != "default/kids-school" {
		t.Errorf("GroupDecision: %v %v", d, ok)
	}
	if _, ok := ix.GroupDecision("default/nope", time.Now()); ok {
		t.Error("unknown group")
	}
}

func TestNoPolicyApplies(t *testing.T) {
	ix := Build(Input{
		ClientGroups: []*v1alpha1.ClientGroup{group(ns, "kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"})},
		Policies: []*v1alpha1.Policy{pol(ns, "default", v1alpha1.PolicySpec{
			Schedule: &v1alpha1.Schedule{Weekdays: []string{"sat"}},
		})},
	}, Options{})
	d := ix.SelectPolicy(client("10.0.20.1"), time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	if d == nil || d.Applies() || d.Filters() {
		t.Fatalf("expected a non-nil decision that applies nothing, got %+v", d)
	}
	var nilDecision *Decision
	if nilDecision.Applies() || nilDecision.Filters() || nilDecision.List("x") != nil {
		t.Error("nil Decision methods must be safe")
	}
}

func TestListResolution(t *testing.T) {
	lists := compile(t, map[string][]string{
		"dns/ads": {"||ads.example^"},
		"family":  {"||adult.example^"},
		"allowed": {"||ok.example^"},
	}, map[string]blocking.Mode{"allowed": blocking.ModeAllow})
	bls := []*v1alpha1.BlockList{
		blockList("dns", "ads", "block"),
		blockList("dns", "family", ""),
		blockList("dns", "allowed", "allow", "||ok.example^"),
		blockList("dns", "pending", "block"),
		{
			ObjectMeta: blockList("dns", "custom", "block").ObjectMeta,
			Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: "https://example.org/list"}}, Response: "customIP",
				CustomIP: &v1alpha1.CustomIP{V4: "192.0.2.53", V6: "2001:db8::53"}},
		},
	}
	ix := Build(Input{
		ClientGroups: []*v1alpha1.ClientGroup{group("dns", "lan", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"})},
		Policies: []*v1alpha1.Policy{pol("dns", "lan", v1alpha1.PolicySpec{
			ClientGroupRefs: []string{"lan", "ghost"},
			BlockListRefs:   []string{"ads", "family", "allowed", "pending", "missing"},
		})},
		BlockLists: bls,
		Lists:      lists,
	}, Options{DefaultNamespace: "dns"})
	d := ix.SelectPolicy(client("10.1.1.1"), time.Now())
	if got, want := d.Selection.BlockLists(), []string{"dns/ads", "family"}; !slices.Equal(got, want) {
		t.Errorf("namespaced then bare names: %v, want %v", got, want)
	}
	if got := d.Selection.AllowLists(); !slices.Equal(got, []string{"allowed"}) {
		t.Errorf("an allow list under blockListRefs keeps its mode: %v", got)
	}
	if got := ix.NotLoaded(); !slices.Equal(got, []string{"dns/pending"}) {
		t.Errorf("NotLoaded = %v", got)
	}
	for _, c := range []struct{ reason, substr string }{
		{ReasonModeMismatch, `"allowed" is a allow list`},
		{ReasonNotLoaded, `"pending"`},
		{ReasonMissingReference, `blockListRefs "missing"`},
		{ReasonMissingReference, `clientGroupRefs "ghost"`},
	} {
		if !hasDiag(ix, "Policy", "dns/lan", c.reason, c.substr) {
			t.Errorf("missing %s %q in %v", c.reason, c.substr, ix.Diagnoses())
		}
	}
	if info := d.List("allowed"); info == nil || !info.Inline || info.Object != "dns/allowed" {
		t.Errorf("inline list info: %+v", info)
	}
	if info := d.List("dns/ads"); info == nil || info.Inline {
		t.Errorf("ads has an inline source with no entries, still inline? %+v", info)
	}
	info := d.List("dns/custom")
	if info == nil || info.Response != "customIP" || info.CustomV4 != netip.MustParseAddr("192.0.2.53") || info.CustomV6 != netip.MustParseAddr("2001:db8::53") || info.Inline {
		t.Errorf("custom list info: %+v", info)
	}
}

func TestBuildWithoutLists(t *testing.T) {
	ix := Build(Input{
		ClientGroups: []*v1alpha1.ClientGroup{group(ns, "default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"})},
		Policies:     []*v1alpha1.Policy{pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, AllowOnly: true, BlockListRefs: []string{"ads"}})},
		BlockLists:   []*v1alpha1.BlockList{blockList(ns, "ads", "block")},
	}, Options{})
	d := ix.SelectPolicy(client("10.0.0.1"), time.Now())
	if d.Selection == nil || !d.Filters() {
		t.Fatal("allow-only must filter even before lists load")
	}
	if dec := d.Selection.Evaluate("example.com."); !dec.Blocked() {
		t.Errorf("allow-only without allow lists blocks: %+v", dec)
	}
	if got := ix.NotLoaded(); !slices.Equal(got, []string{"default/ads"}) {
		t.Errorf("NotLoaded = %v", got)
	}
}

func TestManyScheduledPolicies(t *testing.T) {
	in := Input{ClientGroups: []*v1alpha1.ClientGroup{group(ns, "g", v1alpha1.ClientMatcher{CIDR: "10.0.0.0/8"})}}
	// Ten hourly windows: more than maxEagerSched, so decisions are cached
	// lazily by mask.
	for h := 0; h < 10; h++ {
		start := time.Duration(h) * time.Hour
		in.Policies = append(in.Policies, pol(ns, "p"+string(rune('a'+h)), v1alpha1.PolicySpec{
			ClientGroupRefs: []string{"g"}, Priority: h,
			Schedule: &v1alpha1.Schedule{Start: clock(start), End: clock(start + time.Hour)},
		}))
	}
	ix := Build(in, Options{})
	for h := 0; h < 12; h++ {
		at := time.Date(2026, 10, 12, h, 30, 0, 0, time.UTC)
		d := ix.SelectPolicy(client("10.1.2.3"), at)
		want := ""
		if h < 10 {
			want = "default/p" + string(rune('a'+h))
		}
		if d.Policy != want {
			t.Errorf("%02d:30 policy %q, want %q", h, d.Policy, want)
		}
		if again := ix.SelectPolicy(client("10.1.2.3"), at); again != d {
			t.Errorf("%02d:30 lazily cached decision not reused", h)
		}
	}
}

func clock(d time.Duration) string {
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(d).Format("15:04")
}

func TestSelectPolicyDoesNotAllocate(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	ix := benchIndex(t)
	clients := []pipeline.Client{client("10.0.20.9"), client("10.0.30.4"), client("192.168.1.1"), client("2001:db8::1")}
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, berlin)
	allocs := testing.AllocsPerRun(1000, func() {
		for _, c := range clients {
			_ = ix.SelectPolicy(c, at)
		}
	})
	if allocs != 0 {
		t.Errorf("SelectPolicy allocates %.1f times per run", allocs)
	}
}

// benchIndex: 3 groups, 5 lists, one scheduled policy.
func benchIndex(t testing.TB) *Index {
	lists := compile(t, map[string][]string{
		"default/ads":      {"||ads.example^", "||tracker.example^"},
		"default/malware":  {"||malware.example^"},
		"default/adult":    {"||adult.example^"},
		"default/social":   {"||social.example^"},
		"default/allowlst": {"||ok.example^"},
	}, map[string]blocking.Mode{"default/allowlst": blocking.ModeAllow})
	return Build(Input{
		ClientGroups: []*v1alpha1.ClientGroup{
			group(ns, "default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}, v1alpha1.ClientMatcher{CIDR: "::/0"}),
			group(ns, "kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}),
			group(ns, "iot", v1alpha1.ClientMatcher{CIDR: "10.0.30.0/24"}, v1alpha1.ClientMatcher{IP: "10.0.40.1"}),
		},
		Policies: []*v1alpha1.Policy{
			pol(ns, "default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads", "malware"}, AllowListRefs: []string{"allowlst"}}),
			pol(ns, "kids", v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{"ads", "malware", "adult", "social"}, Priority: 10,
				Schedule: &v1alpha1.Schedule{Weekdays: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "08:00", End: "20:00", TZ: "Europe/Berlin"}}),
			pol(ns, "iot", v1alpha1.PolicySpec{ClientGroupRefs: []string{"iot"}, BlockListRefs: []string{"malware"}, Priority: 5}),
		},
		BlockLists: []*v1alpha1.BlockList{
			blockList(ns, "ads", "block"), blockList(ns, "malware", "block"), blockList(ns, "adult", "block"),
			blockList(ns, "social", "block"), blockList(ns, "allowlst", "allow"),
		},
		Lists: lists,
	}, Options{})
}

func BenchmarkSelectPolicy(b *testing.B) {
	ix := benchIndex(b)
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for _, bc := range []struct {
		name string
		c    pipeline.Client
	}{
		{"cidr-scheduled", client("10.0.20.9")},
		{"cidr", client("10.0.30.4")},
		{"exact-ip", client("10.0.40.1")},
		{"catch-all", client("192.168.1.1")},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = ix.SelectPolicy(bc.c, at)
			}
		})
	}
}

func BenchmarkBuild(b *testing.B) {
	in := Input{}
	for g := 0; g < 100; g++ {
		var clients []v1alpha1.ClientMatcher
		for c := 0; c < 10; c++ {
			clients = append(clients, v1alpha1.ClientMatcher{IP: netip.AddrFrom4([4]byte{10, byte(g), byte(c), 1}).String()})
		}
		clients = append(clients, v1alpha1.ClientMatcher{CIDR: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(g), 0, 0}), 16).String()})
		name := "g" + netip.AddrFrom4([4]byte{0, 0, 0, byte(g)}).String()
		in.ClientGroups = append(in.ClientGroups, group(ns, name, clients...))
		in.Policies = append(in.Policies, pol(ns, name, v1alpha1.PolicySpec{ClientGroupRefs: []string{name}, Priority: g}))
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = Build(in, Options{})
	}
}
