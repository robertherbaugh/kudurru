package listfetcher

import (
	"context"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestExcludeURLSource: spec.sources[].exclude switches single entries of
// a fetched body off on every node, survives a refresh that changes the
// body, and reports the counts and the values that match nothing.
func TestExcludeURLSource(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	srv := newListServer(t, hostsV1)
	url := srv.URL + "/hosts"
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url, Exclude: []string{"tracker.example", "nothere.example"}})
	fx.f.Reconcile(ctx)
	s := getList(t, fx.st, "ads").Status
	src := s.Sources[0]
	// hostsV1 has ads.example twice and tracker.example once.
	if src.Entries != 2 || src.Excluded != 1 || !slices.Equal(src.StaleExcludes, []string{"nothere.example"}) || s.RuleCount != 2 {
		t.Fatalf("status %+v", s)
	}

	sk := &sink{}
	r, err := NewResolver(ResolverOptions{Objects: newStoreObjects(fx.st), Cache: fx.cache, Sink: sk, Logger: quiet(), Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	snap := sk.last()
	if got := snap.Contains("tracker.example"); got["default/ads"] != "" {
		t.Errorf("an excluded entry still matches: %v", got)
	}
	if got := snap.Contains("ads.example"); got["default/ads"] == "" {
		t.Errorf("the other entry stopped matching: %v", got)
	}

	// A refresh brings a new body; the exclusion stays and now matches
	// nothing new, while new.example is compiled.
	srv.set("", hostsV2, `"v2"`)
	fx.clk.Advance(25 * time.Hour)
	fx.f.Reconcile(ctx)
	if _, err := r.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	snap = sk.last()
	if got := snap.Contains("tracker.example"); got["default/ads"] != "" {
		t.Errorf("after the refresh the excluded entry matches again: %v", got)
	}
	if got := snap.Contains("new.example"); got["default/ads"] == "" {
		t.Errorf("new.example not compiled: %v", got)
	}

	// Removing the exclusion recompiles; the status counts follow.
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url})
	fx.f.Reconcile(ctx)
	if _, err := r.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sk.last().Contains("tracker.example"); got["default/ads"] == "" {
		t.Errorf("re-enabled entry does not match: %v", got)
	}
	if src := getList(t, fx.st, "ads").Status.Sources[0]; src.Entries != 3 || src.Excluded != 0 || len(src.StaleExcludes) != 0 {
		t.Errorf("status after removing the exclusion: %+v", src)
	}
}

// TestStructuredEntries: entries compile as rules of their type; a
// disabled entry is left out and counted as excluded.
func TestStructuredEntries(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	entries := []v1alpha1.ListEntry{
		{Value: "exact.example", Type: v1alpha1.EntryExact},
		{Value: "*.wild.example", Type: v1alpha1.EntryWildcard},
		{Value: `^re[0-9]+\.example$`, Type: v1alpha1.EntryRegex},
		{Value: "off.example", Enabled: off()},
	}
	applyList(t, fx.st, "mine", v1alpha1.ListSource{Name: "ui", Entries: entries})
	fx.f.Reconcile(ctx)
	src := getList(t, fx.st, "mine").Status.Sources[0]
	if src.Entries != 3 || src.Excluded != 1 || len(src.StaleExcludes) != 0 {
		t.Fatalf("status %+v", src)
	}
	sk := &sink{}
	r, err := NewResolver(ResolverOptions{Objects: newStoreObjects(fx.st), Cache: fx.cache, Sink: sk, Logger: quiet(), Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	snap := sk.last()
	for name, want := range map[string]bool{
		"exact.example": true, "a.exact.example": false, "wild.example": true, "a.b.wild.example": true,
		"re42.example": true, "re.example": false, "off.example": false,
	} {
		if got := snap.Contains(name)["default/mine"] != ""; got != want {
			t.Errorf("%s: matches %v, want %v", name, got, want)
		}
	}
}

// TestAllowlistChain is the end-to-end check of the Allowlist through
// store → zonecache → resolver → policy index → stages: an exclusion of a
// block list entry unblocks that name; an allow entry for everyone wins
// over the block list; one scoped to a group unblocks only that group's
// clients; a wildcard covers subdomains; switching an entry off blocks
// again.
func TestAllowlistChain(t *testing.T) {
	ch := newChain(t)
	ctx := context.Background()
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	ch.apply(&v1alpha1.ClientGroup{ObjectMeta: meta("default"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}})
	ch.apply(&v1alpha1.ClientGroup{ObjectMeta: meta("kids"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{IP: "192.0.2.77"}}}})
	ch.apply(&v1alpha1.BlockList{ObjectMeta: meta("ads"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{
		{Name: "feed", Inline: []string{"0.0.0.0 one.example two.example", "||wild.example^", "three.example"}, Format: "hosts"},
	}}})
	ch.apply(&v1alpha1.Policy{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads"}}})
	ch.apply(&v1alpha1.Policy{ObjectMeta: meta("kids"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}, BlockListRefs: []string{"ads"}}})
	adult, kid := "192.0.2.10", "192.0.2.77"
	for _, n := range []string{"one.example.", "two.example.", "a.wild.example.", "three.example."} {
		ch.waitBlocked(n, adult, true)
		ch.waitBlocked(n, kid, true)
	}

	// Switch one entry of the block list off.
	bl := &v1alpha1.BlockList{}
	if err := ch.st.Get(ctx, store.Key{Kind: "BlockList", Namespace: "default", Name: "ads"}, bl); err != nil {
		t.Fatal(err)
	}
	bl.Spec.Sources[0].Exclude = []string{"two.example"}
	ch.apply(bl)
	ch.waitBlocked("two.example.", adult, false)
	ch.waitBlocked("one.example.", adult, true)

	lists := domainlist.New(domainlist.Options{Store: ch.st})
	if _, err := lists.Add(ctx, domainlist.AddRequest{Action: domainlist.Allow, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"one.example"}}); err != nil {
		t.Fatal(err)
	}
	ch.waitBlocked("one.example.", adult, false)
	ch.waitBlocked("one.example.", kid, false)

	if _, err := lists.Add(ctx, domainlist.AddRequest{Action: domainlist.Allow, Namespace: "default", Type: v1alpha1.EntryWildcard, Values: []string{"*.wild.example"}, Scope: domainlist.Scope{Groups: []string{"kids"}}}); err != nil {
		t.Fatal(err)
	}
	ch.waitBlocked("a.wild.example.", kid, false)
	ch.waitBlocked("wild.example.", kid, false)
	ch.waitBlocked("a.wild.example.", adult, true)

	off := false
	if _, err := lists.Update(ctx, domainlist.Allow, "default", v1alpha1.EntryExact, "one.example", domainlist.Patch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	ch.waitBlocked("one.example.", adult, true)

	if _, err := lists.Add(ctx, domainlist.AddRequest{Action: domainlist.Deny, Namespace: "default", Type: v1alpha1.EntryRegex, Values: []string{`^deny[0-9]+\.example$`}}); err != nil {
		t.Fatal(err)
	}
	ch.waitBlocked("deny7.example.", adult, true)
	ch.waitBlocked("allowed.example.", adult, false)
}
