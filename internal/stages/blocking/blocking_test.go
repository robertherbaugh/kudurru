package blocking

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

const ns = "default"

// fakeUpstream answers from a table of presentation-format records and
// records which names reached it.
type fakeUpstream struct {
	answers map[string][]string // "name. TYPE" -> RRs
	seen    []string
	groups  []string
}

func (f *fakeUpstream) Name() string { return "upstream" }

func (f *fakeUpstream) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	q := req.Question()
	key := strings.ToLower(q.Name) + " " + dns.TypeToString[q.Qtype]
	f.seen = append(f.seen, key)
	f.groups = append(f.groups, req.UpstreamGroup)
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.RecursionAvailable = true
	for _, s := range f.answers[key] {
		rr, err := dns.NewRR(s)
		if err != nil {
			return nil, err
		}
		m.Answer = append(m.Answer, rr)
	}
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Upstream: "default", Cacheable: true}, nil
}

func compileLists(t testing.TB, lists map[string][]string, modes map[string]engine.Mode) *engine.Snapshot {
	t.Helper()
	var out []*engine.List
	for name, lines := range lists {
		mode := modes[name]
		if mode == "" {
			mode = engine.ModeBlock
		}
		l, err := engine.Compile(engine.Spec{Name: name, Mode: mode, Sources: []engine.Source{
			engine.LinesSource("inline", parser.FormatAuto, lines),
		}}, nil, engine.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	snap, err := engine.NewSnapshot(1, out...)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func meta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: ns} }

func cg(name string, clients ...v1alpha1.ClientMatcher) *v1alpha1.ClientGroup {
	return &v1alpha1.ClientGroup{ObjectMeta: meta(name), Spec: v1alpha1.ClientGroupSpec{Clients: clients}}
}

func bl(name string, mode v1alpha1.ListMode, src v1alpha1.ListSource) *v1alpha1.BlockList {
	return &v1alpha1.BlockList{ObjectMeta: meta(name), Spec: v1alpha1.BlockListSpec{Mode: mode, Sources: []v1alpha1.ListSource{src}}}
}

var (
	urlSource    = v1alpha1.ListSource{URL: "https://lists.example/ads.txt"}
	inlineSource = v1alpha1.ListSource{Inline: []string{"placeholder"}}
)

// fixture: groups by /24, one policy each.
//
//	10.0.0.0/8 default: ads (URL list with @@), mine (inline), trackers (regex), allow list
//	10.0.20/24 kids: allow-only with the school list
//	10.0.50/24 nx: nxdomain, blockTTL 1m, upstreamGroup "filtered"
//	10.0.60/24 custom: the custom list (customIP response)
//	10.0.70/24 refused: refused mode
func fixture(t testing.TB) *policy.Index {
	lists := compileLists(t, map[string][]string{
		"default/ads":      {"||ads.example^", "@@||good.ads.example^", "||cdn-ads.net^", "203.0.113.66", "2001:db8::666", "||svc-target.example^"},
		"default/mine":     {"exact.example", "||suffix.example^"},
		"default/trackers": {`/^track[0-9]+\.example$/`},
		"default/allow":    {"||allowed.ads.example^"},
		"default/school":   {"||school.example^"},
		"default/custom":   {"||custom.example^"},
	}, map[string]engine.Mode{"default/allow": engine.ModeAllow, "default/school": engine.ModeAllow})
	custom := bl("custom", "block", urlSource)
	custom.Spec.Response = "customIP"
	custom.Spec.CustomIP = &v1alpha1.CustomIP{V4: "192.0.2.1", V6: "2001:db8::1"}
	p := func(name string, spec v1alpha1.PolicySpec) *v1alpha1.Policy {
		return &v1alpha1.Policy{ObjectMeta: meta(name), Spec: spec}
	}
	return policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{
			cg("default", v1alpha1.ClientMatcher{CIDR: "10.0.0.0/8"}),
			cg("kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}),
			cg("nx", v1alpha1.ClientMatcher{CIDR: "10.0.50.0/24"}),
			cg("custom", v1alpha1.ClientMatcher{CIDR: "10.0.60.0/24"}),
			cg("refused", v1alpha1.ClientMatcher{CIDR: "10.0.70.0/24"}),
			cg("open", v1alpha1.ClientMatcher{CIDR: "10.0.80.0/24"}),
		},
		Policies: []*v1alpha1.Policy{
			p("default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads", "mine", "trackers"}, AllowListRefs: []string{"allow"}}),
			p("kids", v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, AllowOnly: true, AllowListRefs: []string{"school"}, Priority: 10}),
			p("nx", v1alpha1.PolicySpec{ClientGroupRefs: []string{"nx"}, BlockListRefs: []string{"ads"}, BlockMode: "nxdomain", BlockTTL: metav1.Duration{Duration: time.Minute}, UpstreamGroup: "filtered"}),
			p("custom", v1alpha1.PolicySpec{ClientGroupRefs: []string{"custom"}, BlockListRefs: []string{"custom", "ads"}, BlockMode: "customIP"}),
			p("refused", v1alpha1.PolicySpec{ClientGroupRefs: []string{"refused"}, BlockListRefs: []string{"ads"}, BlockMode: "refused"}),
			p("open", v1alpha1.PolicySpec{ClientGroupRefs: []string{"open"}, UpstreamGroup: "open-resolvers"}),
		},
		BlockLists: []*v1alpha1.BlockList{
			bl("ads", "block", urlSource), bl("mine", "block", inlineSource), bl("trackers", "block", urlSource),
			bl("allow", "allow", urlSource), bl("school", "allow", urlSource), custom,
		},
		Lists: lists,
	}, policy.Options{})
}

func upstreamTable() *fakeUpstream {
	return &fakeUpstream{answers: map[string][]string{
		"www.example.com. A":     {"www.example.com. 300 IN A 93.184.215.14"},
		"cname.example.com. A":   {"cname.example.com. 300 IN CNAME x.cdn-ads.net.", "x.cdn-ads.net. 300 IN A 198.51.100.1"},
		"ip.example.com. A":      {"ip.example.com. 300 IN A 203.0.113.66"},
		"ip6.example.com. AAAA":  {"ip6.example.com. 300 IN AAAA 2001:db8::666"},
		"svc.example.com. HTTPS": {"svc.example.com. 300 IN HTTPS 1 svc-target.example. alpn=h2"},
		"hint.example.com. HTTPS": {
			"hint.example.com. 300 IN HTTPS 1 . alpn=h2 ipv4hint=203.0.113.66"},
		"okcname.example.com. A": {"okcname.example.com. 300 IN CNAME allowed.ads.example.", "allowed.ads.example. 300 IN A 198.51.100.2"},
		"allowed.ads.example. A": {"allowed.ads.example. 300 IN CNAME tracker.cdn-ads.net.", "tracker.cdn-ads.net. 300 IN A 198.51.100.3"},
		"school.example. A":      {"school.example. 300 IN A 198.51.100.4"},
		"good.ads.example. A":    {"good.ads.example. 300 IN A 198.51.100.5"},
	}}
}

type chainUnderTest struct {
	chain    *pipeline.Chain
	up       *fakeUpstream
	stage    *Stage
	registry *prometheus.Registry
}

func newChain(t testing.TB, ix *policy.Index, opts Options) *chainUnderTest {
	t.Helper()
	reg := prometheus.NewRegistry()
	opts.Registerer = reg
	opts.Index = policy.Static{Idx: ix}
	st, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	up := upstreamTable()
	chain, err := pipeline.New(
		&ingress.Stage{NodeID: "test"},
		identify.New(policy.Static{Idx: ix}, nil),
		control.New(control.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, BlockedQTypes: []uint16{dns.TypeHINFO}}),
		st,
		up,
	)
	if err != nil {
		t.Fatal(err)
	}
	return &chainUnderTest{chain: chain, up: up, stage: st, registry: reg}
}

func query(name string, qtype uint16, edns bool) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	if edns {
		m.SetEdns0(1232, false)
	}
	return m
}

func (c *chainUnderTest) ask(t testing.TB, from, name string, qtype uint16, edns bool) (*pipeline.Response, error) {
	t.Helper()
	req := &pipeline.Request{
		Msg:    query(name, qtype, edns),
		Client: pipeline.Client{Addr: netip.AddrPortFrom(netip.MustParseAddr(from), 40000), Proto: pipeline.ProtoUDP},
	}
	return c.chain.Serve(req)
}

func ede(m *dns.Msg) (uint16, string, bool) {
	opt := m.IsEdns0()
	if opt == nil {
		return 0, "", false
	}
	for _, o := range opt.Option {
		if e, ok := o.(*dns.EDNS0_EDE); ok {
			return e.InfoCode, e.ExtraText, true
		}
	}
	return 0, "", false
}

func answers(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		out = append(out, rr.String())
	}
	return out
}

func TestBlockingDecisionMatrix(t *testing.T) {
	c := newChain(t, fixture(t), Options{})
	const blocked = dns.ExtendedErrorCodeBlocked
	cases := []struct {
		name     string
		from     string
		qname    string
		qtype    uint16
		rcode    int
		answers  []string
		status   pipeline.Status
		list     string
		rule     string
		ede      uint16
		upstream bool // the query reached the upstream
	}{
		{"pass", "10.0.0.5", "www.example.com.", dns.TypeA, dns.RcodeSuccess, []string{"www.example.com.\t300\tIN\tA\t93.184.215.14"}, pipeline.StatusForwarded, "", "", 0, true},
		{"deny suffix rule zeroIP A", "10.0.0.5", "x.ads.example.", dns.TypeA, dns.RcodeSuccess, []string{"x.ads.example.\t600\tIN\tA\t0.0.0.0"}, pipeline.StatusBlockedList, "default/ads", "||ads.example^", blocked, false},
		{"deny zeroIP AAAA", "10.0.0.5", "ads.example.", dns.TypeAAAA, dns.RcodeSuccess, []string{"ads.example.\t600\tIN\tAAAA\t::"}, pipeline.StatusBlockedList, "default/ads", "||ads.example^", blocked, false},
		{"deny zeroIP other type is NODATA", "10.0.0.5", "ads.example.", dns.TypeTXT, dns.RcodeSuccess, nil, pipeline.StatusBlockedList, "default/ads", "||ads.example^", blocked, false},
		{"@@ exception in the same list", "10.0.0.5", "good.ads.example.", dns.TypeA, dns.RcodeSuccess, []string{"good.ads.example.\t300\tIN\tA\t198.51.100.5"}, pipeline.StatusForwarded, "", "", 0, true},
		{"allow list beats deny", "10.0.0.5", "allowed.ads.example.", dns.TypeA, dns.RcodeSuccess, []string{"allowed.ads.example.\t300\tIN\tCNAME\ttracker.cdn-ads.net.", "tracker.cdn-ads.net.\t300\tIN\tA\t198.51.100.3"}, pipeline.StatusForwarded, "", "", 0, true},
		{"inline exact entry is blocked_exact", "10.0.0.5", "exact.example.", dns.TypeA, dns.RcodeSuccess, []string{"exact.example.\t600\tIN\tA\t0.0.0.0"}, pipeline.StatusBlockedExact, "default/mine", "exact.example", blocked, false},
		{"inline suffix entry is blocked_exact", "10.0.0.5", "a.suffix.example.", dns.TypeA, dns.RcodeSuccess, []string{"a.suffix.example.\t600\tIN\tA\t0.0.0.0"}, pipeline.StatusBlockedExact, "default/mine", "||suffix.example^", blocked, false},
		{"regex rule", "10.0.0.5", "track42.example.", dns.TypeA, dns.RcodeSuccess, []string{"track42.example.\t600\tIN\tA\t0.0.0.0"}, pipeline.StatusBlockedRegex, "default/trackers", `/^track[0-9]+\.example$/`, blocked, false},
		{"response-side CNAME target", "10.0.0.5", "cname.example.com.", dns.TypeA, dns.RcodeSuccess, []string{"cname.example.com.\t600\tIN\tA\t0.0.0.0"}, pipeline.StatusBlockedCNAME, "default/ads", "||cdn-ads.net^", blocked, true},
		{"response-side A address", "10.0.0.5", "ip.example.com.", dns.TypeA, dns.RcodeSuccess, []string{"ip.example.com.\t600\tIN\tA\t0.0.0.0"}, pipeline.StatusBlockedCNAME, "default/ads", "203.0.113.66", blocked, true},
		{"response-side AAAA address", "10.0.0.5", "ip6.example.com.", dns.TypeAAAA, dns.RcodeSuccess, []string{"ip6.example.com.\t600\tIN\tAAAA\t::"}, pipeline.StatusBlockedCNAME, "default/ads", "2001:db8::666", blocked, true},
		{"response-side HTTPS target", "10.0.0.5", "svc.example.com.", dns.TypeHTTPS, dns.RcodeSuccess, nil, pipeline.StatusBlockedCNAME, "default/ads", "||svc-target.example^", blocked, true},
		{"response-side HTTPS ipv4hint", "10.0.0.5", "hint.example.com.", dns.TypeHTTPS, dns.RcodeSuccess, nil, pipeline.StatusBlockedCNAME, "default/ads", "203.0.113.66", blocked, true},
		{"response-side skipped after an allow-list match", "10.0.0.5", "allowed.ads.example.", dns.TypeA, dns.RcodeSuccess, []string{"allowed.ads.example.\t300\tIN\tCNAME\ttracker.cdn-ads.net.", "tracker.cdn-ads.net.\t300\tIN\tA\t198.51.100.3"}, pipeline.StatusForwarded, "", "", 0, true},
		{"response-side CNAME into an allow-listed name passes", "10.0.0.5", "okcname.example.com.", dns.TypeA, dns.RcodeSuccess, []string{"okcname.example.com.\t300\tIN\tCNAME\tallowed.ads.example.", "allowed.ads.example.\t300\tIN\tA\t198.51.100.2"}, pipeline.StatusForwarded, "", "", 0, true},
		{"allow-only: listed name passes", "10.0.20.5", "school.example.", dns.TypeA, dns.RcodeSuccess, []string{"school.example.\t300\tIN\tA\t198.51.100.4"}, pipeline.StatusForwarded, "", "", 0, true},
		{"allow-only: everything else is filtered (EDE 17)", "10.0.20.5", "www.example.com.", dns.TypeA, dns.RcodeSuccess, []string{"www.example.com.\t600\tIN\tA\t0.0.0.0"}, pipeline.StatusBlockedList, "", "", dns.ExtendedErrorCodeFiltered, false},
		{"nxdomain mode, SOA-less, policy TTL irrelevant", "10.0.50.5", "x.ads.example.", dns.TypeA, dns.RcodeNameError, nil, pipeline.StatusBlockedList, "default/ads", "||ads.example^", blocked, false},
		{"refused mode", "10.0.70.5", "x.ads.example.", dns.TypeA, dns.RcodeRefused, nil, pipeline.StatusBlockedList, "default/ads", "||ads.example^", blocked, false},
		{"list customIP overrides policy A", "10.0.60.5", "custom.example.", dns.TypeA, dns.RcodeSuccess, []string{"custom.example.\t600\tIN\tA\t192.0.2.1"}, pipeline.StatusBlockedList, "default/custom", "||custom.example^", blocked, false},
		{"list customIP AAAA", "10.0.60.5", "custom.example.", dns.TypeAAAA, dns.RcodeSuccess, []string{"custom.example.\t600\tIN\tAAAA\t2001:db8::1"}, pipeline.StatusBlockedList, "default/custom", "||custom.example^", blocked, false},
		{"policy customIP without addresses is NODATA", "10.0.60.5", "x.ads.example.", dns.TypeA, dns.RcodeSuccess, nil, pipeline.StatusBlockedList, "default/ads", "||ads.example^", blocked, false},
		{"no filtering policy", "10.0.80.5", "x.ads.example.", dns.TypeA, dns.RcodeSuccess, nil, pipeline.StatusForwarded, "", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.up.seen = nil
			resp, err := c.ask(t, tc.from, tc.qname, tc.qtype, true)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.Rcode != tc.rcode {
				t.Errorf("rcode %s, want %s", dns.RcodeToString[resp.Msg.Rcode], dns.RcodeToString[tc.rcode])
			}
			if got := answers(resp.Msg); !slices.Equal(got, tc.answers) {
				t.Errorf("answers:\n got %q\nwant %q", got, tc.answers)
			}
			if resp.Status != tc.status || resp.List != tc.list || resp.Rule != tc.rule {
				t.Errorf("status/list/rule = %s %q %q, want %s %q %q", resp.Status, resp.List, resp.Rule, tc.status, tc.list, tc.rule)
			}
			code, text, has := ede(resp.Msg)
			if tc.ede != 0 && (!has || code != tc.ede || text != tc.list) {
				t.Errorf("EDE = %d %q (present %v), want %d %q", code, text, has, tc.ede, tc.list)
			}
			if tc.ede == 0 && has {
				t.Errorf("unexpected EDE %d", code)
			}
			if got := len(c.up.seen) > 0; got != tc.upstream {
				t.Errorf("reached upstream = %v, want %v", got, tc.upstream)
			}
			if resp.Status.Class() == pipeline.ClassBlocked {
				if len(resp.Msg.Ns) != 0 {
					t.Errorf("block responses carry no authority section: %v", resp.Msg.Ns)
				}
				if resp.Cacheable {
					t.Error("block responses are not cacheable")
				}
				if !resp.Msg.Response || !resp.Msg.RecursionAvailable || resp.Msg.Authoritative {
					t.Errorf("header flags: %+v", resp.Msg.MsgHdr)
				}
			}
		})
	}
}

func TestBlockTTLAndDefaults(t *testing.T) {
	ix := fixture(t)
	// Config defaults: nxdomain by default and a 30 s TTL; the policy's
	// blockTTL (1 m on "nx") wins over the config.
	c := newChain(t, ix, OptionsFrom(config.Blocking{Enabled: true, DefaultResponse: "nxdomain", BlockTTL: config.Duration(30 * time.Second)}))
	resp, err := c.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Rcode != dns.RcodeNameError {
		t.Errorf("config default response nxdomain: %s", dns.RcodeToString[resp.Msg.Rcode])
	}
	if _, _, has := ede(resp.Msg); has || resp.Msg.IsEdns0() != nil {
		t.Error("no OPT (and no EDE) for a query without EDNS")
	}
	c2 := newChain(t, ix, OptionsFrom(config.Blocking{Enabled: true, DefaultResponse: "zeroIP", BlockTTL: config.Duration(30 * time.Second)}))
	resp, _ = c2.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
	if ttl := resp.Msg.Answer[0].Header().Ttl; ttl != 30 {
		t.Errorf("config blockTTL: %d", ttl)
	}
	m := Synthesize(query("x.example.", dns.TypeA, true), Response{Mode: "zeroIP", TTL: time.Minute, EDE: dns.ExtendedErrorCodeBlocked})
	if m.Answer[0].Header().Ttl != 60 {
		t.Errorf("policy TTL: %v", m.Answer[0])
	}
	if code, _, ok := ede(m); !ok || code != dns.ExtendedErrorCodeBlocked {
		t.Error("EDE 15 expected")
	}
	// The synthesised message survives the wire, EDE included.
	m.Id = 4242
	wire, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	back := new(dns.Msg)
	if err := back.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	if code, _, ok := ede(back); !ok || code != dns.ExtendedErrorCodeBlocked || back.Id != 4242 || len(back.Answer) != 1 {
		t.Errorf("wire round trip lost data: %v", back)
	}
	if Synthesize(new(dns.Msg), Response{}).Rcode != dns.RcodeFormatError {
		t.Error("no question: FORMERR")
	}
	// Defaults of New: zeroIP, 10 m.
	c3 := newChain(t, ix, Options{})
	resp, _ = c3.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
	if ttl := resp.Msg.Answer[0].Header().Ttl; ttl != 600 {
		t.Errorf("default blockTTL: %d", ttl)
	}
	opts := OptionsFrom(config.Blocking{Enabled: false, CustomIP: config.BlockingCustomIP{V4: "192.0.2.9", V6: "2001:db8::9"}})
	if !opts.Disabled || opts.CustomV4 != netip.MustParseAddr("192.0.2.9") || opts.CustomV6 != netip.MustParseAddr("2001:db8::9") {
		t.Errorf("OptionsFrom: %+v", opts)
	}
}

func TestUpstreamGroupFromPolicy(t *testing.T) {
	c := newChain(t, fixture(t), Options{})
	if _, err := c.ask(t, "10.0.50.5", "www.example.com.", dns.TypeA, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ask(t, "10.0.80.5", "www.example.com.", dns.TypeA, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ask(t, "10.0.0.5", "www.example.com.", dns.TypeA, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"filtered", "open-resolvers", ""}; !slices.Equal(c.up.groups, want) {
		t.Errorf("upstream groups %q, want %q", c.up.groups, want)
	}
	// A forward zone's own group is kept.
	ix := fixture(t)
	st, _ := New(Options{})
	req := &pipeline.Request{Msg: query("www.example.com.", dns.TypeA, false), UpstreamGroup: "internal-ad",
		Client: pipeline.Client{Addr: netip.MustParseAddrPort("10.0.50.5:1")}}
	req.Policy = ix.SelectPolicy(req.Client, time.Now())
	up := upstreamTable()
	if _, err := st.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) { return up.Serve(r, nil) }); err != nil {
		t.Fatal(err)
	}
	if up.groups[0] != "internal-ad" {
		t.Errorf("forward-zone group overridden: %q", up.groups[0])
	}
}

func TestToggleAndDisabled(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	toggle := NewToggle(clock)
	c := newChain(t, fixture(t), Options{Toggle: toggle, Now: clock})
	blockedNow := func() bool {
		resp, err := c.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Status.Class() == pipeline.ClassBlocked
	}
	if !blockedNow() {
		t.Fatal("blocking should be on")
	}
	pub := &recordingPublisher{}
	toggle.SetPublisher(pub)
	until, err := toggle.Disable(5 * time.Minute)
	if err != nil || !until.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("Disable: %v %v", until, err)
	}
	if blockedNow() || c.stage.Enabled() {
		t.Fatal("temporarily disabled")
	}
	if got := toggle.DisabledUntil(); !got.Equal(until) {
		t.Errorf("DisabledUntil %v", got)
	}
	if got := testutil.ToFloat64(c.stage.metrics.decisions.WithLabelValues("pass", "disabled", "default/default")); got != 1 {
		t.Errorf("disabled decisions: %v", got)
	}
	now = now.Add(5*time.Minute - time.Second)
	if blockedNow() {
		t.Fatal("still disabled one second before expiry")
	}
	now = now.Add(time.Second)
	if !blockedNow() || !toggle.DisabledUntil().IsZero() {
		t.Fatal("disable must expire")
	}
	if _, err := toggle.Disable(0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(1000 * time.Hour)
	if blockedNow() || !toggle.DisabledUntil().Equal(Forever) {
		t.Fatal("a disable without timer lasts until Enable")
	}
	if err := toggle.Enable(); err != nil {
		t.Fatal(err)
	}
	if !blockedNow() {
		t.Fatal("re-enabled")
	}
	if want := []time.Time{until, Forever, {}}; !slices.EqualFunc(pub.got, want, time.Time.Equal) {
		t.Errorf("published %v, want %v", pub.got, want)
	}
	// Replicated state applies without publishing again.
	toggle.Apply(now.Add(time.Minute))
	if blockedNow() || len(pub.got) != 3 {
		t.Error("Apply must disable without publishing")
	}
	toggle.Apply(time.Time{})

	var nilToggle *Toggle
	if !nilToggle.Enabled() {
		t.Error("nil toggle is enabled")
	}

	off := newChain(t, fixture(t), Options{Disabled: true})
	resp, _ := off.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
	if resp.Status != pipeline.StatusForwarded {
		t.Errorf("blocking.enabled=false must not block: %s", resp.Status)
	}
}

type recordingPublisher struct{ got []time.Time }

func (p *recordingPublisher) PublishDisabledUntil(until time.Time) error {
	p.got = append(p.got, until)
	return nil
}

func TestMetrics(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	c := newChain(t, fixture(t), Options{Now: func() time.Time { return now }})
	for _, q := range []struct{ from, name string }{
		{"10.0.0.5", "x.ads.example."},
		{"10.0.0.5", "y.ads.example."},
		{"10.0.0.5", "www.example.com."},
		{"10.0.0.5", "good.ads.example."},
		{"10.0.0.5", "allowed.ads.example."},
		{"10.0.0.5", "cname.example.com."},
		{"10.0.20.5", "www.example.com."},
	} {
		if _, err := c.ask(t, q.from, q.name, dns.TypeA, false); err != nil {
			t.Fatal(err)
		}
	}
	want := map[[3]string]float64{
		{"block", "deny", "default/default"}:      2,
		{"pass", "none", "default/default"}:       1,
		{"allow", "exception", "default/default"}: 1,
		{"allow", "allowlist", "default/default"}: 1,
		{"block", "response", "default/default"}:  1,
		{"block", "allow_only", "default/kids"}:   1,
		{"pass", "disabled", "default/kids"}:      0,
	}
	for k, v := range want {
		if got := testutil.ToFloat64(c.stage.metrics.decisions.WithLabelValues(k[0], k[1], k[2])); got != v {
			t.Errorf("decisions%v = %v, want %v", k, got, v)
		}
	}
	if got := testutil.ToFloat64(c.stage.metrics.listHits.WithLabelValues("default/ads")); got != 3 {
		t.Errorf("list hits = %v", got)
	}
	expected := `
# HELP kudurru_blocking_enabled 1 when blocking applies to the client group right now (blocking.enabled, not temporarily disabled, and the group's current policy names lists or is allow-only).
# TYPE kudurru_blocking_enabled gauge
kudurru_blocking_enabled{group="default/custom"} 1
kudurru_blocking_enabled{group="default/default"} 1
kudurru_blocking_enabled{group="default/kids"} 1
kudurru_blocking_enabled{group="default/nx"} 1
kudurru_blocking_enabled{group="default/open"} 0
kudurru_blocking_enabled{group="default/refused"} 1
`
	if err := testutil.GatherAndCompare(c.registry, strings.NewReader(expected), "kudurru_blocking_enabled"); err != nil {
		t.Error(err)
	}
	if _, err := c.stage.Toggle().Disable(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := testutil.GatherAndCompare(c.registry, strings.NewReader(strings.ReplaceAll(expected, "} 1", "} 0")), "kudurru_blocking_enabled"); err != nil {
		t.Error(err)
	}
}

func TestLabelLimiter(t *testing.T) {
	var l labelLimiter
	for i := 0; i < maxLabelValues; i++ {
		if got := l.admit(string(rune('A' + i))); got == otherLabel {
			t.Fatalf("value %d folded early", i)
		}
	}
	if l.admit("one-too-many") != otherLabel {
		t.Error("value beyond the cap must be folded")
	}
	if l.admit("A") != "A" {
		t.Error("admitted values stay admitted")
	}
}

func TestReady(t *testing.T) {
	ix := policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{cg("default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"})},
		Policies:     []*v1alpha1.Policy{{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads"}}}},
		BlockLists:   []*v1alpha1.BlockList{bl("ads", "block", urlSource)},
	}, policy.Options{})
	strict, _ := New(Options{Index: policy.Static{Idx: ix}})
	if ok, why := strict.Ready(); ok || len(why) != 1 || !strings.Contains(why[0], "default/ads") {
		t.Errorf("not ready while a list is missing: %v %v", ok, why)
	}
	degraded, _ := New(Options{Index: policy.Static{Idx: ix}, AllowDegraded: true})
	if ok, _ := degraded.Ready(); !ok {
		t.Error("allowDegraded is ready")
	}
	empty, _ := New(Options{Index: policy.Static{}})
	if ok, _ := empty.Ready(); ok {
		t.Error("no index yet: not ready")
	}
	loaded := policy.Build(policy.Input{}, policy.Options{})
	fine, _ := New(Options{Index: policy.Static{Idx: loaded}})
	if ok, _ := fine.Ready(); !ok {
		t.Error("nothing missing: ready")
	}
}

func TestStageWithoutDecision(t *testing.T) {
	st, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	up := upstreamTable()
	resp, err := st.Serve(&pipeline.Request{Msg: query("ads.example.", dns.TypeA, false)}, func(r *pipeline.Request) (*pipeline.Response, error) { return up.Serve(r, nil) })
	if err != nil || resp.Status != pipeline.StatusForwarded {
		t.Fatalf("no policy: pass through, got %v %v", resp, err)
	}
	if st.Name() != "blocking" {
		t.Error(st.Name())
	}
}

// BenchmarkStageOverhead measures what identify + control + blocking add
// to a query that passes (the common case) and one that is blocked, with
// 3 groups and 5 lists. The baseline chain is the same minus the three
// stages.
func BenchmarkStageOverhead(b *testing.B) {
	ix := benchFixture(b)
	reg := prometheus.NewRegistry()
	st, err := New(Options{Registerer: reg, Index: policy.Static{Idx: ix}})
	if err != nil {
		b.Fatal(err)
	}
	answer := new(dns.Msg)
	answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "www.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: netip.MustParseAddr("93.184.215.14").AsSlice()}}
	terminal := &staticStage{resp: &pipeline.Response{Msg: answer, Status: pipeline.StatusForwarded}}
	full := pipeline.MustNew(identify.New(policy.Static{Idx: ix}, nil), control.New(control.Options{}), st, terminal)
	base := pipeline.MustNew(terminal)
	client := pipeline.Client{Addr: netip.MustParseAddrPort("10.0.20.9:40000")}
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for _, bc := range []struct {
		name  string
		chain *pipeline.Chain
		qname string
	}{
		{"baseline", base, "www.example.com."},
		{"pass", full, "www.example.com."},
		{"block", full, "x.ads.example."},
	} {
		b.Run(bc.name, func(b *testing.B) {
			msg := query(bc.qname, dns.TypeA, false)
			b.ReportAllocs()
			for b.Loop() {
				req := &pipeline.Request{Msg: msg, Client: client, Received: at}
				if _, err := bc.chain.Serve(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type staticStage struct{ resp *pipeline.Response }

func (s *staticStage) Name() string { return "upstream" }
func (s *staticStage) Serve(*pipeline.Request, pipeline.Next) (*pipeline.Response, error) {
	return s.resp, nil
}

// benchFixture: 3 groups, 5 lists of a few thousand entries.
func benchFixture(b testing.TB) *policy.Index {
	gen := func(prefix string, n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, "||"+prefix+strings.Repeat("x", i%7)+string(rune('a'+i%26))+"-"+itoa(i)+".example^")
		}
		return out
	}
	lists := map[string][]string{
		"default/ads":     append(gen("ad", 5000), "||ads.example^"),
		"default/malware": gen("mw", 5000),
		"default/adult":   gen("adult", 5000),
		"default/social":  gen("soc", 2000),
		"default/allow":   {"||ok.example^"},
	}
	snap := compileLists(b, lists, map[string]engine.Mode{"default/allow": engine.ModeAllow})
	p := func(name string, prio int, refs ...string) *v1alpha1.Policy {
		return &v1alpha1.Policy{ObjectMeta: meta(name), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{name}, BlockListRefs: refs, AllowListRefs: []string{"allow"}, Priority: prio}}
	}
	return policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{
			cg("default", v1alpha1.ClientMatcher{CIDR: "0.0.0.0/0"}),
			cg("kids", v1alpha1.ClientMatcher{CIDR: "10.0.20.0/24"}),
			cg("iot", v1alpha1.ClientMatcher{CIDR: "10.0.30.0/24"}),
		},
		Policies: []*v1alpha1.Policy{
			p("default", 0, "ads", "malware"),
			p("kids", 10, "ads", "malware", "adult", "social"),
			p("iot", 5, "malware"),
		},
		BlockLists: []*v1alpha1.BlockList{
			bl("ads", "block", urlSource), bl("malware", "block", urlSource), bl("adult", "block", urlSource),
			bl("social", "block", urlSource), bl("allow", "allow", urlSource),
		},
		Lists: snap,
	}, policy.Options{})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestTTLSeconds(t *testing.T) {
	for d, want := range map[time.Duration]uint32{
		-time.Second:               0,
		0:                          0,
		1500 * time.Millisecond:    1,
		10 * time.Minute:           600,
		200 * 365 * 24 * time.Hour: 1<<31 - 1,
	} {
		if got := ttlSeconds(d); got != want {
			t.Errorf("ttlSeconds(%s) = %d, want %d", d, got, want)
		}
	}
}
