package listfetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestReloadRefreshKeepsSuccessAnchorsOverridesAndCooldown(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	bl := applyList(t, fx.st, "default", v1alpha1.ListSource{URL: srv.URL + "/default"})
	bl.Spec.RefreshInterval.Duration = 0
	if _, err := fx.st.Apply(t.Context(), bl, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	applyList(t, fx.st, "explicit", v1alpha1.ListSource{URL: srv.URL + "/explicit"})
	fx.f.Reconcile(t.Context())
	fx.clk.Advance(2 * time.Minute)
	fx.f.Reconcile(t.Context())
	if srv.hits.Load() != 2 {
		t.Fatal("startup intervals changed")
	}
	fx.f.Reconfigure(time.Minute)
	fx.f.Reconcile(t.Context())
	if srv.hits.Load() != 3 {
		t.Fatal("shorter default did not use original success anchor or changed explicit interval")
	}
	fx.f.Reconfigure(24 * time.Hour)
	fx.clk.Advance(2 * time.Minute)
	fx.f.Reconcile(t.Context())
	if srv.hits.Load() != 3 {
		t.Fatal("longer interval retained an obsolete short deadline")
	}
	// A settings change is not an on-demand refresh: failure cooldown survives,
	// and an embedding caller cannot reduce the one-minute source floor.
	srv.set("500", "", "")
	fx.f.Reconfigure(time.Nanosecond)
	fx.f.Reconcile(t.Context())
	hits := srv.hits.Load()
	fx.clk.Advance(30 * time.Second)
	fx.f.Reconfigure(time.Minute)
	fx.f.Reconcile(t.Context())
	if srv.hits.Load() != hits {
		t.Fatal("reload bypassed original failure cooldown")
	}
	fx.clk.Advance(30 * time.Second)
	fx.f.Reconcile(t.Context())
	if srv.hits.Load() <= hits {
		t.Fatal("original cooldown never expired")
	}
}

func TestReloadWakesFetcherWithoutReplacingOriginalRequest(t *testing.T) {
	fx := newFixture(t)
	entered := make(chan struct{})
	var calls atomic.Int64
	var wrongSource atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if r.URL.Path != "/original" || !ok || user != "fixture" || password != "original-owner" {
			wrongSource.Store(true)
		}
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	source, err := url.Parse(srv.URL + "/original")
	if err != nil {
		t.Fatal("invalid fixture URL")
	}
	source.User = url.UserPassword("fixture", "original-owner")
	applyList(t, fx.st, "pending", v1alpha1.ListSource{URL: source.String()})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fx.f.Run(ctx) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("original HTTP request did not start")
	}
	for range 100 {
		fx.f.Reconfigure(time.Minute)
	}
	// Updating scheduling must neither cancel/restart this request nor extend
	// the original context. Run joins its bounded download workers on expiry.
	select {
	case err := <-done:
		if err != nil || calls.Load() != 1 || ctx.Err() == nil || wrongSource.Load() {
			t.Fatal("reload replaced original request or deadline", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fetcher did not join original deadline")
	}
	if len(getList(t, fx.st, "pending").Status.Sources) != 0 {
		t.Fatal("canceled pass wrote a new status")
	}
}

func TestResolverReloadReadinessAndFallbackKeepOriginalTimes(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	bl := applyList(t, fx.st, "pending", v1alpha1.ListSource{URL: srv.URL})
	bl.Spec.RefreshInterval.Duration = 0
	if _, err := fx.st.Apply(t.Context(), bl, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// Keep a different published digest so the replica remains behind its
	// unreachable leader after its own fallback download.
	bl = getList(t, fx.st, "pending")
	bl.Status.Sources = []v1alpha1.SourceStatus{{Name: srv.URL, Digest: sha([]byte(hostsV2))}}
	if err := fx.st.UpdateStatus(t.Context(), bl); err != nil {
		t.Fatal(err)
	}
	rep := newReplica(t, newStoreObjects(fx.st), ResolverOptions{Now: fx.clk.Now, InitStrategy: InitBlocking})
	rep.r.opts.Downloader = NewDownloader(rep.cache, DownloadOptions{Now: fx.clk.Now, AllowLocalAddresses: true})
	if _, err := rep.r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ready, _ := rep.r.Ready(); ready {
		t.Fatal("missing lists unexpectedly ready")
	}
	for _, allow := range []bool{true, false, true, false} {
		rep.r.Reconfigure(time.Minute, allow)
		if ready, _ := rep.r.Ready(); ready != allow {
			t.Fatal("degraded setting did not apply immediately")
		}
	}
	fx.clk.Advance(DefaultFallbackAfter)
	if _, err := rep.r.Resolve(t.Context()); err != nil || srv.hits.Load() != 1 {
		t.Fatal("reload renewed original fallback waiting period", err)
	}
	rep.r.Reconfigure(time.Hour, false)
	fx.clk.Advance(2 * time.Minute)
	_, _ = rep.r.Resolve(t.Context())
	if srv.hits.Load() != 1 {
		t.Fatal("longer interval retained obsolete fallback deadline")
	}
	rep.r.Reconfigure(time.Minute, false)
	_, _ = rep.r.Resolve(t.Context())
	if srv.hits.Load() != 2 {
		t.Fatal("shorter interval renewed rather than reused fallback success anchor")
	}
}
