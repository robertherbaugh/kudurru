package blocking

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	logstage "github.com/robertherbaugh/kudurru/internal/stages/log"
)

// events is a querylog.Collector keeping every event.
type events struct {
	mu  sync.Mutex
	got []*querylog.Event
}

func (e *events) Enqueue(ev *querylog.Event) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev)
	return true
}

// familyIndex: group "family" (10.0.90.0/24) blocks YouTube and TikTok
// through the catalogue and enforces safe search, with an allow list for
// www.bing.com; group "teens" (10.0.91.0/24) has moderate safe search
// only; everyone else gets the default policy without filtering.
func familyIndex(t testing.TB) *policy.Index {
	t.Helper()
	cat := catalogue.Embedded()
	svc, _ := engine.Rebuild(nil, cat.Specs(), engine.CompileOptions{})
	allow := compileLists(t, map[string][]string{"default/allow": {"www.bing.com"}}, map[string]engine.Mode{"default/allow": engine.ModeAllow})
	lists, err := engine.NewSnapshot(3, append(svc.Lists(), allow.Lists()...)...)
	if err != nil {
		t.Fatal(err)
	}
	p := func(name string, spec v1alpha1.PolicySpec) *v1alpha1.Policy {
		return &v1alpha1.Policy{ObjectMeta: meta(name), Spec: spec}
	}
	on := true
	return policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{
			cg("default", v1alpha1.ClientMatcher{CIDR: "10.0.0.0/8"}),
			cg("family", v1alpha1.ClientMatcher{CIDR: "10.0.90.0/24"}),
			cg("teens", v1alpha1.ClientMatcher{CIDR: "10.0.91.0/24"}),
		},
		Policies: []*v1alpha1.Policy{
			p("default", v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}),
			p("family", v1alpha1.PolicySpec{
				ClientGroupRefs: []string{"family"}, Priority: 10, AllowListRefs: []string{"allow"},
				BlockedServices: []string{"youtube", "tiktok"}, SafeSearch: &on, BlockTTL: metav1.Duration{Duration: 5 * time.Minute},
			}),
			p("teens", v1alpha1.PolicySpec{ClientGroupRefs: []string{"teens"}, Priority: 10, SafeSearch: &on, SafeSearchMode: "moderate"}),
		},
		BlockLists: []*v1alpha1.BlockList{bl("allow", "allow", urlSource)},
		Lists:      lists,
	}, policy.Options{Services: catalogue.NewHolder(nil)})
}

func safeUpstream() *fakeUpstream {
	return &fakeUpstream{answers: map[string][]string{
		"forcesafesearch.google.com. A":     {"forcesafesearch.google.com. 300 IN A 216.239.38.120"},
		"forcesafesearch.google.com. AAAA":  {"forcesafesearch.google.com. 300 IN AAAA 2001:4860:4802:32::78"},
		"restrictmoderate.youtube.com. A":   {"restrictmoderate.youtube.com. 300 IN A 216.239.38.119"},
		"strict.bing.com. A":                {"strict.bing.com. 300 IN A 150.171.27.16"},
		"www.bing.com. A":                   {"www.bing.com. 300 IN A 192.0.2.80"},
		"www.google.com. A":                 {"www.google.com. 300 IN A 192.0.2.81"},
		"www.youtube.com. A":                {"www.youtube.com. 300 IN A 192.0.2.82"},
		"safe.duckduckgo.com. A":            {"safe.duckduckgo.com. 300 IN CNAME safe-ddg.example.", "safe-ddg.example. 300 IN A 192.0.2.83"},
		"forcesafe.search.brave.com. HTTPS": {},
	}}
}

// TestSafeSearchAndBlockedServicesEndToEnd runs ingress → identify →
// control → blocking → upstream (fake) with the query-log observer and
// checks the answers and what the query log records.
func TestSafeSearchAndBlockedServicesEndToEnd(t *testing.T) {
	ix := familyIndex(t)
	reg := prometheus.NewRegistry()
	st, err := New(Options{Index: policy.Static{Idx: ix}, Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	up := safeUpstream()
	chain := pipeline.MustNew(
		&ingress.Stage{NodeID: "test"},
		identify.New(policy.Static{Idx: ix}, nil),
		control.New(control.Options{}),
		st, up,
	)
	log := &events{}
	chain.Observe(&logstage.Observer{Node: "test", Collector: log})

	type step struct {
		from, name string
		qtype      uint16
		status     pipeline.Status
		list, rule string
		answers    []string
	}
	steps := []step{
		// Blocked service: zeroIP like any block list, list service:youtube.
		{"10.0.90.5", "www.youtube.com.", dns.TypeA, pipeline.StatusBlockedList, "service:youtube", "||youtube.com^",
			[]string{"www.youtube.com.\t300\tIN\tA\t0.0.0.0"}},
		{"10.0.90.5", "v16m.tiktokcdn.com.", dns.TypeAAAA, pipeline.StatusBlockedList, "service:tiktok", "||tiktokcdn.com^",
			[]string{"v16m.tiktokcdn.com.\t300\tIN\tAAAA\t::"}},
		// Safe search: CNAME plus the target's addresses from upstream.
		{"10.0.90.5", "www.google.com.", dns.TypeA, pipeline.StatusBlockedSafeSearch, "safesearch:google", "CNAME forcesafesearch.google.com.",
			[]string{"www.google.com.\t300\tIN\tCNAME\tforcesafesearch.google.com.", "forcesafesearch.google.com.\t300\tIN\tA\t216.239.38.120"}},
		{"10.0.90.5", "WWW.Google.Co.UK.", dns.TypeAAAA, pipeline.StatusBlockedSafeSearch, "safesearch:google", "CNAME forcesafesearch.google.com.",
			[]string{"WWW.Google.Co.UK.\t300\tIN\tCNAME\tforcesafesearch.google.com.", "forcesafesearch.google.com.\t300\tIN\tAAAA\t2001:4860:4802:32::78"}},
		// A chained target keeps the whole chain.
		{"10.0.90.5", "duckduckgo.com.", dns.TypeA, pipeline.StatusBlockedSafeSearch, "safesearch:duckduckgo", "CNAME safe.duckduckgo.com.",
			[]string{"duckduckgo.com.\t300\tIN\tCNAME\tsafe.duckduckgo.com.", "safe.duckduckgo.com.\t300\tIN\tCNAME\tsafe-ddg.example.", "safe-ddg.example.\t300\tIN\tA\t192.0.2.83"}},
		// NODATA at the target: the CNAME alone.
		{"10.0.90.5", "search.brave.com.", dns.TypeHTTPS, pipeline.StatusBlockedSafeSearch, "safesearch:brave", "CNAME forcesafe.search.brave.com.",
			[]string{"search.brave.com.\t300\tIN\tCNAME\tforcesafe.search.brave.com."}},
		// A CNAME question is answered without asking the upstream.
		{"10.0.90.5", "www.google.de.", dns.TypeCNAME, pipeline.StatusBlockedSafeSearch, "safesearch:google", "CNAME forcesafesearch.google.com.",
			[]string{"www.google.de.\t300\tIN\tCNAME\tforcesafesearch.google.com."}},
		// Engines documented with an address are answered directly.
		{"10.0.90.5", "yandex.ru.", dns.TypeA, pipeline.StatusBlockedSafeSearch, "safesearch:yandex", "213.180.193.56",
			[]string{"yandex.ru.\t300\tIN\tA\t213.180.193.56"}},
		{"10.0.90.5", "yandex.ru.", dns.TypeAAAA, pipeline.StatusBlockedSafeSearch, "safesearch:yandex", "213.180.193.56", nil},
		// The allow list wins over safe search.
		{"10.0.90.5", "www.bing.com.", dns.TypeA, pipeline.StatusForwarded, "", "", []string{"www.bing.com.\t300\tIN\tA\t192.0.2.80"}},
		// Moderate mode picks YouTube's moderate target; the TTL is the
		// default block TTL (10m).
		{"10.0.91.5", "www.youtube.com.", dns.TypeA, pipeline.StatusBlockedSafeSearch, "safesearch:youtube", "CNAME restrictmoderate.youtube.com.",
			[]string{"www.youtube.com.\t600\tIN\tCNAME\trestrictmoderate.youtube.com.", "restrictmoderate.youtube.com.\t300\tIN\tA\t216.239.38.119"}},
		// Without the policy nothing is rewritten or blocked.
		{"10.0.0.5", "www.google.com.", dns.TypeA, pipeline.StatusForwarded, "", "", []string{"www.google.com.\t300\tIN\tA\t192.0.2.81"}},
		{"10.0.0.5", "www.youtube.com.", dns.TypeA, pipeline.StatusForwarded, "", "", []string{"www.youtube.com.\t300\tIN\tA\t192.0.2.82"}},
	}
	for _, s := range steps {
		req := &pipeline.Request{Msg: query(s.name, s.qtype, true), Client: pipeline.Client{Addr: addrPort(s.from)}}
		resp, err := chain.Serve(req)
		if err != nil {
			t.Fatalf("%s %s: %v", s.name, dns.TypeToString[s.qtype], err)
		}
		if resp.Status != s.status || resp.Msg.Rcode != dns.RcodeSuccess || resp.Msg.Id != req.Msg.Id {
			t.Errorf("%s %s from %s: %s/%s", s.name, dns.TypeToString[s.qtype], s.from, dns.RcodeToString[resp.Msg.Rcode], resp.Status)
		}
		if got := answers(resp.Msg); !slices.Equal(got, s.answers) {
			t.Errorf("%s %s answers\n got %q\nwant %q", s.name, dns.TypeToString[s.qtype], got, s.answers)
		}
		if s.status == pipeline.StatusBlockedSafeSearch {
			if code, text, ok := ede(resp.Msg); !ok || code != dns.ExtendedErrorCodeForgedAnswer || text != s.list {
				t.Errorf("%s: EDE %d %q %v", s.name, code, text, ok)
			}
		}
	}
	if len(log.got) != len(steps) {
		t.Fatalf("query log has %d events, want %d", len(log.got), len(steps))
	}
	for i, s := range steps {
		ev := log.got[i]
		if ev.Name != strings.ToLower(s.name) || ev.Status != s.status || ev.List != s.list || ev.Rule != s.rule {
			t.Errorf("query log %d: %s %s list %q rule %q; want %s list %q rule %q", i, ev.Name, ev.Status, ev.List, ev.Rule, s.status, s.list, s.rule)
		}
	}
	if s := pipeline.StatusBlockedSafeSearch; s.String() != "blocked_safesearch" || s.Class() != pipeline.ClassBlocked {
		t.Errorf("status %s class %s", s, s.Class())
	}
	// The upstream saw the safe targets, never the rewritten names, and
	// nothing for blocked services.
	for _, q := range up.seen {
		if strings.HasPrefix(q, "www.google.co.uk.") || strings.Contains(q, "tiktok") || q == "duckduckgo.com. A" {
			t.Errorf("upstream saw %q", q)
		}
	}
	if got := testutil.ToFloat64(st.metrics.decisions.WithLabelValues("rewrite", "safesearch", "default/family")); got != 7 {
		t.Errorf("rewrite/safesearch decisions = %v, want 7", got)
	}

	// The temporary disable turns safe search off with the block lists.
	st.Toggle().Disable(0)
	resp, err := chain.Serve(&pipeline.Request{Msg: query("www.google.com.", dns.TypeA, false), Client: pipeline.Client{Addr: addrPort("10.0.90.5")}})
	if err != nil || resp.Status != pipeline.StatusForwarded {
		t.Errorf("disabled: %v %v", resp, err)
	}
}

// A catalogue swapped in at runtime takes effect on the next query.
func TestSafeSearchFollowsTheCatalogue(t *testing.T) {
	ix := familyIndex(t)
	h := catalogue.NewHolder(nil)
	st, err := New(Options{Index: policy.Static{Idx: ix}, Catalogue: h})
	if err != nil {
		t.Fatal(err)
	}
	chain := pipeline.MustNew(identify.New(policy.Static{Idx: ix}, nil), st, safeUpstream())
	ask := func() *pipeline.Response {
		resp, err := chain.Serve(&pipeline.Request{Msg: query("pixabay.com.", dns.TypeCNAME, false), Client: pipeline.Client{Addr: addrPort("10.0.90.5")}})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := ask(); resp.Status != pipeline.StatusBlockedSafeSearch {
		t.Fatalf("embedded: %s", resp.Status)
	}
	raw := catalogue.Embedded().Raw()
	next, err := catalogue.Parse([]byte(strings.Replace(string(raw), `"pixabay.com"`, `"www.pixabay.com"`, 1)), "test")
	if err != nil {
		t.Fatal(err)
	}
	h.Set(next)
	if resp := ask(); resp.Status != pipeline.StatusForwarded {
		t.Errorf("after the swap pixabay.com is no longer rewritten: %s", resp.Status)
	}
}
