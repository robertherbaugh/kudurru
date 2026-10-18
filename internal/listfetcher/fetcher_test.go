package listfetcher

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

const hostsV1 = "# Title: test list\n0.0.0.0 ads.example\n0.0.0.0 tracker.example\n0.0.0.0 ads.example\nnot a rule!!\n"
const hostsV2 = "0.0.0.0 ads.example\n0.0.0.0 tracker.example\n0.0.0.0 new.example\n"

func TestFetchPipeline(t *testing.T) {
	fx := newFixture(t)
	reg := prometheus.NewRegistry()
	f, err := New(Options{Store: fx.st, Objects: newStoreObjects(fx.st), Cache: fx.cache, Downloader: fx.dl, Logger: quiet(), Now: fx.clk.Now, Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	srv := newListServer(t, hostsV1)
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: srv.URL + "/hosts"}, v1alpha1.ListSource{Inline: []string{"||inline.example^"}})
	ctx := context.Background()

	// 1. First fetch: digest, etag, counts and lastFetched published.
	next := f.Reconcile(ctx)
	bl := getList(t, fx.st, "ads")
	s := bl.Status
	if len(s.Sources) != 2 {
		t.Fatalf("sources = %+v", s.Sources)
	}
	src := s.Sources[0]
	if src.Name != srv.URL+"/hosts" || src.Digest != sha([]byte(hostsV1)) || src.ETag != `"v1"` || src.Error != "" || src.LastFetched == nil {
		t.Fatalf("source status = %+v", src)
	}
	if src.Entries != 3 || src.Invalid != 1 {
		t.Errorf("source counts entries=%d invalid=%d, want 3 rules and 1 invalid", src.Entries, src.Invalid)
	}
	if s.Sources[1].Name != "inline-1" || s.Sources[1].Digest != inlineDigest([]string{"||inline.example^"}) || s.Sources[1].Entries != 1 {
		t.Errorf("inline status = %+v", s.Sources[1])
	}
	if s.Entries != 3 || s.RuleCount != 4 || s.Invalid != 1 || s.LastFetched == nil {
		t.Errorf("list counts entries=%d ruleCount=%d invalid=%d", s.Entries, s.RuleCount, s.Invalid)
	}
	if want := fx.clk.Now().Add(time.Hour - jitter(srv.URL+"/hosts", time.Hour)); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
	if got := testutil.ToFloat64(f.m.entries.WithLabelValues("default/ads")); got != 3 {
		t.Errorf("entries gauge = %v", got)
	}
	if got := testutil.ToFloat64(f.m.lastUpdate.WithLabelValues("default/ads")); got != float64(fx.clk.Now().Unix()) {
		t.Errorf("last update gauge = %v", got)
	}
	firstFetched := src.LastFetched.Time

	// Not due: nothing fetched, nothing written.
	writes := fx.st.updates.Load()
	f.Reconcile(ctx)
	if srv.hits.Load() != 1 || fx.st.updates.Load() != writes {
		t.Errorf("idle pass fetched %d times / wrote %d", srv.hits.Load()-1, fx.st.updates.Load()-writes)
	}

	// 2. Refresh due: conditional GET answered 304; only lastFetched moves.
	fx.clk.Advance(time.Hour)
	f.Reconcile(ctx)
	if srv.conds.Load() != 1 {
		t.Fatalf("conditional requests = %d", srv.conds.Load())
	}
	s = getList(t, fx.st, "ads").Status
	if s.Sources[0].Digest != src.Digest || !s.Sources[0].LastFetched.After(firstFetched) {
		t.Errorf("after 304: %+v", s.Sources[0])
	}

	// 3. 500: retried, then the old body is kept and the error published.
	srv.set("500", "", "")
	hits := srv.hits.Load()
	fx.clk.Advance(time.Hour)
	f.Reconcile(ctx)
	if got := srv.hits.Load() - hits; got != DefaultAttempts {
		t.Errorf("attempts on 500 = %d, want %d", got, DefaultAttempts)
	}
	s = getList(t, fx.st, "ads").Status
	if s.Sources[0].Digest != src.Digest || !strings.Contains(s.Sources[0].Error, "500") || s.Entries != 3 {
		t.Errorf("after 500: %+v (entries %d)", s.Sources[0], s.Entries)
	}
	if b, _ := os.ReadFile(fx.cache.Path(srv.URL + "/hosts")); string(b) != hostsV1 {
		t.Error("cached body changed after a failed download")
	}
	if testutil.ToFloat64(f.m.failures.WithLabelValues("default/ads")) != 1 || testutil.ToFloat64(f.m.stale.WithLabelValues("default/ads")) != 1 {
		t.Error("failure metrics not raised")
	}
	// Cooldown: an immediate pass does not retry.
	hits = srv.hits.Load()
	f.Reconcile(ctx)
	if srv.hits.Load() != hits {
		t.Error("retried inside the cooldown")
	}

	// 4. HTML (captive portal) rejected, not retried, old body kept.
	srv.set("html", "", "")
	fx.clk.Advance(DefaultCooldown * 2)
	hits = srv.hits.Load()
	f.Reconcile(ctx)
	s = getList(t, fx.st, "ads").Status
	if srv.hits.Load()-hits != 1 || !strings.Contains(s.Sources[0].Error, "HTML") || s.Sources[0].Digest != src.Digest {
		t.Errorf("after HTML (%d hits): %+v", srv.hits.Load()-hits, s.Sources[0])
	}

	// 5. Size cap.
	srv.set("", strings.Repeat("0.0.0.0 big.example\n", 60000), `"big"`)
	fx.clk.Advance(DefaultCooldown * 4)
	f.Reconcile(ctx)
	s = getList(t, fx.st, "ads").Status
	if !strings.Contains(s.Sources[0].Error, "size cap") || s.Sources[0].Digest != src.Digest {
		t.Errorf("after an oversized body: %+v", s.Sources[0])
	}

	// 6. Recovery with a new body: new digest, error cleared, stale 0.
	srv.set("", hostsV2, `"v2"`)
	fx.clk.Advance(DefaultCooldown * 8)
	f.Reconcile(ctx)
	s = getList(t, fx.st, "ads").Status
	if s.Sources[0].Digest != sha([]byte(hostsV2)) || s.Sources[0].Error != "" || s.Sources[0].ETag != `"v2"` || s.Entries != 4 {
		t.Errorf("after recovery: %+v (entries %d)", s.Sources[0], s.Entries)
	}
	if testutil.ToFloat64(f.m.stale.WithLabelValues("default/ads")) != 0 {
		t.Error("stale not cleared")
	}

	// A deleted list drops its metrics.
	if err := fx.st.Delete(ctx, storeKey("ads"), deleteOpts()); err != nil {
		t.Fatal(err)
	}
	f.Reconcile(ctx)
	if n := testutil.CollectAndCount(f.m.entries); n != 0 {
		t.Errorf("%d entries series left after delete", n)
	}
}

// TestStatusOwnership: the fetcher writes only its fields, keeps the
// controller's, writes once per change and retries conflicts.
func TestStatusOwnership(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	bl := applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: srv.URL})
	ctx := context.Background()

	// The controller's view: observedGeneration and conditions.
	bl.Status.ObservedGeneration = bl.Generation
	meta.SetStatusCondition(&bl.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonServing, Message: "1 sources"})
	if err := fx.st.Store.UpdateStatus(ctx, bl); err != nil {
		t.Fatal(err)
	}

	fx.st.conflicts.Store(2) // two lost races against the controller
	fx.f.Reconcile(ctx)
	if got := fx.st.updates.Load(); got != 1 {
		t.Fatalf("UpdateStatus calls = %d, want 1 (plus 2 conflicts)", got)
	}
	got := getList(t, fx.st, "ads")
	if got.Status.ObservedGeneration != bl.Generation || len(got.Status.Conditions) != 1 || got.Status.Conditions[0].Reason != v1alpha1.ReasonServing {
		t.Errorf("controller fields changed: %+v", got.Status)
	}
	if len(got.Status.Sources) != 1 || got.Status.Sources[0].Digest == "" {
		t.Errorf("fetcher fields missing: %+v", got.Status)
	}

	// Ten idle passes and a pass after a controller-only change: no writes.
	for range 10 {
		fx.f.Reconcile(ctx)
	}
	meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionFalse, Reason: "AsExpected"})
	if err := fx.st.Store.UpdateStatus(ctx, got); err != nil {
		t.Fatal(err)
	}
	fx.f.Reconcile(ctx)
	if n := fx.st.updates.Load(); n != 1 {
		t.Errorf("UpdateStatus calls after idle passes = %d, want 1", n)
	}

	// A new term (fresh Fetcher) resumes timers from status: no refetch,
	// no recompile-driven write.
	f2, err := New(Options{Store: fx.st, Objects: newStoreObjects(fx.st), Cache: fx.cache, Downloader: fx.dl, Logger: quiet(), Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	hits := srv.hits.Load()
	f2.Reconcile(ctx)
	if srv.hits.Load() != hits || fx.st.updates.Load() != 1 {
		t.Errorf("new term refetched (%d) or rewrote (%d)", srv.hits.Load()-hits, fx.st.updates.Load()-1)
	}

	// A spec change between plan and write is not overwritten with stale
	// data: the write is skipped and the next pass handles the new spec.
	stale := getList(t, fx.st, "ads")
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: srv.URL + "/other"})
	pl := plan(stale, time.Hour, time.Minute)
	if err := f2.writeStatus(ctx, &pl, ownedStatus{Entries: 99}); err != nil {
		t.Fatal(err)
	}
	if getList(t, fx.st, "ads").Status.Entries == 99 {
		t.Error("status for an outdated spec was written")
	}
}

func TestFetcherConfigMapAndBadURL(t *testing.T) {
	fx := newFixture(t)
	applyList(t, fx.st, "cm", v1alpha1.ListSource{ConfigMapRef: &v1alpha1.ObjectRef{Name: "extra"}})
	applyList(t, fx.st, "ftp", v1alpha1.ListSource{URL: "ftp://lists.example/hosts"})
	fx.f.Reconcile(context.Background())
	if e := getList(t, fx.st, "cm").Status.Sources[0].Error; !strings.Contains(e, "not supported") {
		t.Errorf("configMapRef error = %q", e)
	}
	if e := getList(t, fx.st, "ftp").Status.Sources[0].Error; !strings.Contains(e, "http and https") {
		t.Errorf("ftp error = %q", e)
	}
}

func TestFetcherConcurrencyAndSharedURL(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	// Two lists share one URL: one download, two status writes.
	applyList(t, fx.st, "a", v1alpha1.ListSource{URL: srv.URL})
	applyList(t, fx.st, "b", v1alpha1.ListSource{URL: srv.URL})
	fx.f.Reconcile(context.Background())
	if srv.hits.Load() != 1 {
		t.Errorf("shared URL downloaded %d times", srv.hits.Load())
	}
	if a, b := getList(t, fx.st, "a").Status.Sources[0].Digest, getList(t, fx.st, "b").Status.Sources[0].Digest; a == "" || a != b {
		t.Errorf("digests %q %q", a, b)
	}
}
