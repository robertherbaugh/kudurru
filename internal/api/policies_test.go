package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestPolicyState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	w, err := sqlitelog.Open(context.Background(), sqlitelog.Options{
		Path: filepath.Join(t.TempDir(), sqlitelog.FileName), Node: "node-a", Privacy: querylog.PrivacyFull,
		RawRetention: 48 * time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	ev := func(client, name, pol string, st pipeline.Status) *querylog.Event {
		return &querylog.Event{Time: now.Add(-time.Hour), Latency: time.Millisecond, Node: "node-a", Proto: pipeline.ProtoUDP,
			Client: netip.MustParseAddr(client), Name: name, QType: 1, QClass: 1, Status: st, ReplyType: querylog.ReplyIP,
			ClientGroup: "home/kids", Policy: pol}
	}
	if err := w.Write([]*querylog.Event{
		ev("10.0.0.1", "games.example.", "home/kids", pipeline.StatusBlockedList),
		ev("10.0.0.1", "games.example.", "home/kids", pipeline.StatusBlockedList),
		ev("10.0.0.2", "school.example.", "home/kids", pipeline.StatusForwarded),
		ev("10.0.0.2", "news.example.", "home/kids", pipeline.StatusForwarded),
		ev("10.0.0.3", "other.example.", "home/bedtime", pipeline.StatusForwarded),
	}); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) {
		o.Querylog = w.Reader()
		o.Now = func() time.Time { return now }
	})
	off := false
	on := true
	meta := func(name string) metav1.ObjectMeta { return objectMeta("home", name) }
	disabledGroup := &v1alpha1.ClientGroup{ObjectMeta: meta("off"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{IP: "10.0.0.9"}}}}
	disabledGroup.Annotations = map[string]string{v1alpha1.EnabledAnnotation: "false"}
	h.seed(store.ManagerAPI,
		&v1alpha1.ClientGroup{ObjectMeta: meta("kids"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{IP: "10.0.0.1"}, {CIDR: "10.0.0.0/30"}}}},
		disabledGroup,
		&v1alpha1.BlockList{ObjectMeta: meta("games"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{
			{Name: "a", Inline: []string{"games.example"}}, {Name: "b", Inline: []string{"more.example"}, Enabled: &off},
		}}},
		&v1alpha1.BlockList{ObjectMeta: meta("school"), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{{Inline: []string{"school.example"}}}}},
		&v1alpha1.Policy{ObjectMeta: meta("kids"), Spec: v1alpha1.PolicySpec{
			ClientGroupRefs: []string{"kids", "off", "missing"}, BlockListRefs: []string{"games", "nolist"}, AllowListRefs: []string{"school"},
			BlockedServices: []string{"youtube", "twitter", "roblox", "from-the-future"}, SafeSearch: &on, SafeSearchMode: "moderate",
			UpstreamGroup: "kids-dns", Priority: 10, BlockTTL: metav1.Duration{Duration: 5 * time.Minute},
			Schedule: &v1alpha1.Schedule{Start: "00:00", End: "00:00"},
		}},
		&v1alpha1.Policy{ObjectMeta: meta("bedtime"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, Priority: 50}},
	)
	// The list's status, as the fetcher and the controller publish it.
	var games v1alpha1.BlockList
	ctx := context.Background()
	if err := h.st.Get(ctx, store.Key{Kind: "BlockList", Namespace: "home", Name: "games"}, &games); err != nil {
		t.Fatal(err)
	}
	games.Status.Entries = 1
	games.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Serving", LastTransitionTime: metav1.Now()}}
	if err := h.st.UpdateStatus(ctx, &games); err != nil {
		t.Fatal(err)
	}

	var st PolicyState
	h.do(http.MethodGet, "/api/v1/policies/home/kids/state", "viewer", nil).want(http.StatusOK).json(&st)
	if st.Policy != "home/kids" || !st.Enabled || !st.Active || st.Priority != 10 || st.ManagedBy != store.ManagerAPI {
		t.Errorf("header: %+v", st)
	}
	if s := st.Schedule; s == nil || !s.Now || s.NextChange != nil || s.Summary != "every day all day (UTC)" {
		t.Errorf("schedule %+v", s)
	}
	if len(st.ShadowedBy) != 1 || st.ShadowedBy[0] != "home/bedtime" {
		t.Errorf("shadowedBy %v", st.ShadowedBy)
	}
	if len(st.Groups) != 3 {
		t.Fatalf("groups %+v", st.Groups)
	}
	if g := st.Groups[0]; g.Ref != "kids" || !g.Exists || !g.Enabled || g.Clients != 2 || g.Seen24h == nil || *g.Seen24h != 3 {
		t.Errorf("group kids %+v", g)
	}
	if g := st.Groups[1]; !g.Exists || g.Enabled || g.Seen24h == nil || *g.Seen24h != 0 {
		t.Errorf("group off %+v", g)
	}
	if g := st.Groups[2]; g.Exists || g.Clients != 0 {
		t.Errorf("group missing %+v", g)
	}
	want := []PolicyList{
		{Ref: "games", Role: "block", Mode: "block", Exists: true, Enabled: true, Ready: true, Entries: 1, SourcesEnabled: 1, SourcesTotal: 2},
		{Ref: "nolist", Role: "block", Mode: "block"},
		{Ref: "school", Role: "allow", Mode: "allow", Exists: true, Enabled: true, SourcesEnabled: 1, SourcesTotal: 1},
	}
	if len(st.Lists) != len(want) {
		t.Fatalf("lists %+v", st.Lists)
	}
	for i := range want {
		if st.Lists[i] != want[i] {
			t.Errorf("list %d = %+v, want %+v", i, st.Lists[i], want[i])
		}
	}
	svc := map[string]PolicyService{}
	for _, s := range st.Services {
		svc[s.ID] = s
	}
	if y := svc["youtube"]; !y.Known || y.Name != "YouTube" || y.Rules == 0 || y.Icon != "/api/v1/catalogue/icons/youtube.svg" {
		t.Errorf("youtube %+v", y)
	}
	if x := svc["twitter"]; x.IconOnDark != "/api/v1/catalogue/icons/x-light.svg" {
		t.Errorf("twitter %+v", x)
	}
	if r := svc["roblox"]; !r.Known || r.Icon != "" {
		t.Errorf("roblox %+v", r)
	}
	if f := svc["from-the-future"]; f.Known || f.Name != "from-the-future" {
		t.Errorf("unknown service %+v", f)
	}
	if ss := st.SafeSearch; !ss.Enabled || ss.Mode != "moderate" || len(ss.Engines) < 5 {
		t.Errorf("safe search %+v", ss)
	}
	if ug := st.UpstreamGroup; ug == nil || ug.Name != "kids-dns" || ug.Exists {
		t.Errorf("upstream group %+v", ug)
	}
	if st.BlockMode == "" || st.BlockTTL != "5m0s" {
		t.Errorf("blockMode %q blockTTL %q", st.BlockMode, st.BlockTTL)
	}
	s := st.Stats24h
	if s == nil || s.Queries != 4 || s.Blocked != 2 || s.BlockedPercent != 50 || len(s.TopBlocked) != 1 || s.TopBlocked[0].Domain != "games.example" ||
		len(s.TopClients) != 2 || s.TopClients[0].Count != 2 {
		t.Errorf("stats24h %+v", s)
	}

	// The query log rows carry the policy and filter by it.
	var page QueriesBody
	h.do(http.MethodGet, "/api/v1/queries?policy=home/kids", "operator", nil).want(http.StatusOK).json(&page)
	if len(page.Queries) != 4 || page.Queries[0].Policy != "home/kids" {
		t.Errorf("queries?policy: %d rows, first %+v", len(page.Queries), page.Queries[0])
	}
	var sug SuggestionsBody
	h.do(http.MethodGet, "/api/v1/queries/suggestions?field=policy&prefix=home/", "operator", nil).want(http.StatusOK).json(&sug)
	if len(sug.Values) != 2 {
		t.Errorf("policy suggestions %v", sug.Values)
	}

	h.do(http.MethodGet, "/api/v1/policies/home/none/state", "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/policies/home/kids/state", "", nil).want(http.StatusUnauthorized)

	// Without a query log: no stats and no seen24h.
	h2 := newHarness(t)
	h2.seed(store.ManagerAPI, &v1alpha1.Policy{ObjectMeta: meta("plain"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}}})
	var plain PolicyState
	h2.do(http.MethodGet, "/api/v1/policies/home/plain/state", "viewer", nil).want(http.StatusOK).json(&plain)
	if plain.Stats24h != nil || plain.Schedule != nil || len(plain.Groups) != 1 || plain.Groups[0].Seen24h != nil || plain.UpstreamGroup != nil || !plain.Active {
		t.Errorf("plain state %+v", plain)
	}
}
