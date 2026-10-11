package listfetcher

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
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
