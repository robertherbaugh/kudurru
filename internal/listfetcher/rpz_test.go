package listfetcher

import (
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func rpzSource() v1alpha1.ListSource {
	return v1alpha1.ListSource{Name: "feed", Format: "rpz", RPZ: &v1alpha1.RPZSource{ZoneName: "policy.test", Primaries: []string{"127.0.0.1:5300"}}}
}

func TestRPZFetcherAndResolverKeepRoleWithoutTextBody(t *testing.T) {
	for _, kind := range []string{"pure", "mixed", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			fx := newFixture(t)
			sources := []v1alpha1.ListSource{rpzSource()}
			if kind == "mixed" {
				sources = append(sources, v1alpha1.ListSource{Name: "ordinary", Inline: []string{"||ordinary.test^"}})
			}
			if kind == "disabled" {
				sources[0].Enabled = off()
			}
			applyList(t, fx.st, "ads", sources...)
			fx.f.Reconcile(t.Context())
			status := getList(t, fx.st, "ads").Status
			wantStatus := v1alpha1.SourceStatus{Name: "feed"}
			if kind == "disabled" {
				wantStatus.Enabled = off()
			}
			if !reflect.DeepEqual(status.Sources[0], wantStatus) {
				t.Fatalf("RPZ fabricated shared transfer status: %+v", status.Sources[0])
			}
			wantEntries := 0
			if kind == "mixed" {
				wantEntries = 1
			}
			if status.Entries != wantEntries || status.RuleCount != wantEntries || status.Unsupported != 0 {
				t.Fatal("RPZ polluted ordinary counts", status)
			}
			if len(fx.cache.Keys()) != 0 {
				t.Fatal("RPZ created list-cache body")
			}
			sk := &sink{}
			r, err := NewResolver(ResolverOptions{Objects: newStoreObjects(fx.st), Cache: fx.cache, Sink: sk, Logger: quiet(), Now: fx.clk.Now})
			if err != nil {
				t.Fatal(err)
			}
			res, err := r.Resolve(t.Context())
			list := sk.last().List("default/ads")
			if err != nil || res.Waiting != 0 || res.Pending != 0 || len(res.Failures) != 0 || list == nil || list.Stats().Entries != wantEntries {
				t.Fatalf("RPZ-only list role missing: %+v %v list=%v", res, err, list)
			}
			if len(r.Lists()) != 1 {
				t.Fatal("external policy list absent from loaded names")
			}
			wantDigests := 0
			if kind == "mixed" {
				wantDigests = 1
			}
			if len(r.Lists()[0].Digests) != wantDigests {
				t.Fatal("RPZ invented a shared digest")
			}
		})
	}
}

func TestRPZTransitionClearsLegacyRulesAndStatus(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	url := srv.URL + "/hosts"
	applyList(t, fx.st, "ads", v1alpha1.ListSource{Name: "feed", URL: url})
	fx.f.Reconcile(t.Context())
	sk := &sink{}
	r, err := NewResolver(ResolverOptions{Objects: newStoreObjects(fx.st), Cache: fx.cache, Sink: sk, Logger: quiet(), Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sk.last().List("default/ads").Stats().Entries == 0 {
		t.Fatal("legacy list not loaded")
	}
	legacy := getList(t, fx.st, "ads")
	legacy.Status.Sources[0].Error = "old error"
	legacy.Status.Sources[0].Invalid, legacy.Status.Sources[0].Excluded = 7, 9
	legacy.Status.Sources[0].StaleExcludes = []string{"old.test"}
	if err := fx.st.UpdateStatus(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	applyList(t, fx.st, "ads", rpzSource())
	fx.clk.Advance(2 * time.Hour)
	fx.f.Reconcile(t.Context())
	status := getList(t, fx.st, "ads").Status
	if len(status.Sources) != 1 || !reflect.DeepEqual(status.Sources[0], v1alpha1.SourceStatus{Name: "feed"}) || status.Entries != 0 || status.RuleCount != 0 || status.LastFetched != nil {
		t.Fatal("legacy shared status retained by RPZ", status)
	}
	if _, err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if list := sk.last().List("default/ads"); list == nil || list.Stats().Entries != 0 || len(list.Sources()) != 0 {
		t.Fatal("legacy body retained after source kind changed", list)
	}
	if len(fx.cache.Keys()) != 0 || srv.hits.Load() != 1 {
		t.Fatal("RPZ used legacy URL cache/download")
	}
	applyList(t, fx.st, "ads", v1alpha1.ListSource{Name: "feed", Inline: []string{"||new.test^"}})
	fx.f.Reconcile(t.Context())
	if _, err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := sk.last().List("default/ads").Match("new.test"); !ok {
		t.Fatal("RPZ-to-text transition did not restore ordinary compiler")
	}
}

func TestRPZMissingOrdinaryBodyStillRetainsRequiredFeed(t *testing.T) {
	fx := newFixture(t)
	applyList(t, fx.st, "ads", rpzSource(), v1alpha1.ListSource{URL: "https://unreachable.invalid/ads"})
	sk := &sink{}
	r, err := NewResolver(ResolverOptions{Objects: newStoreObjects(fx.st), Cache: fx.cache, Sink: sk, Logger: quiet(), Now: fx.clk.Now, InitStrategy: InitBlocking})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve(t.Context())
	if err != nil || res.Waiting != 1 || res.Pending != 1 || sk.last().List("default/ads") == nil {
		t.Fatal("missing ordinary body dropped required RPZ role", res, err)
	}
	if ready, _ := r.Ready(); ready {
		t.Fatal("pending ordinary source reported ready")
	}
}

func TestRPZInvalidConfigurationNeverUsesDownloader(t *testing.T) {
	for _, src := range []v1alpha1.ListSource{{Format: "rpz", URL: "https://never.invalid"}, {RPZ: &v1alpha1.RPZSource{}, URL: "https://never.invalid", Inline: []string{"||bad.test^"}}} {
		list := &v1alpha1.BlockList{Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{src}}}
		list.Name, list.Namespace = "ads", "default"
		pl := plan(list, time.Hour, time.Minute)
		if !pl.externalPolicy() || !pl.sources[0].external || pl.sources[0].key != "" || len(pl.sources[0].inline) != 0 || SourceKey("default", src) != "" {
			t.Fatal("invalid external source escaped into ordinary pipeline", pl)
		}
	}
}

func TestRPZFetcherDoesNotOverwriteOtherStatusOwnership(t *testing.T) {
	fx := newFixture(t)
	list := applyList(t, fx.st, "ads", rpzSource())
	list.Status.Conditions = nil
	list.Status.ObservedGeneration = 42
	if err := fx.st.UpdateStatus(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	fx.f.Reconcile(t.Context())
	if got := getList(t, fx.st, "ads").Status.ObservedGeneration; got != 42 {
		t.Fatal("fetcher took controller status ownership", got)
	}
}
