package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServingReloadUsesPublishedCanonicalSettings(t *testing.T) {
	f := newServingCommandFixture(t)
	write := func(enabled bool) {
		t.Helper()
		response := httptest.NewRecorder()
		f.http.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next, err := f.s.Settings(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			next.RateLimit.Enabled = enabled
			if err := f.s.WriteSettings(r.Context(), next, []string{"ratelimit.enabled"}); err != nil {
				t.Error(err)
				return
			}
			w.WriteHeader(204)
		})).ServeHTTP(response, f.request())
		if response.Code != 204 {
			t.Fatal("canonical command failed", response.Code)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	}
	// A restart begins from local host settings but must apply the already
	// committed cluster values as soon as the first complete image is ready.
	write(false)
	initial, err := ingress.RateLimitFromConfig(f.s.opts.Config.RateLimit)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := ingress.NewRateLimiter(*initial)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- f.s.runServingReload(ctx, reloadTargets{logger: slog.New(slog.DiscardHandler), limiter: limiter}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("operational settings watcher did not join")
		}
	})
	awaitServingRuntime(t, f.s, func() bool { return !limiter.Enabled() })
	write(true)
	awaitServingRuntime(t, f.s, func() bool { return limiter.Enabled() })
	if !f.s.opts.Config.RateLimit.Enabled {
		t.Fatal("operational watcher mutated startup host settings")
	}
}

func TestServingReloadInvalidatesRegexOnlyPublications(t *testing.T) {
	f := newServingCommandFixture(t)
	flush, flushed, _ := controlEdgeFlush(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- f.s.runServingReload(ctx, reloadTargets{logger: slog.New(slog.DiscardHandler), edgeFlush: flush}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("publication watcher did not join")
		}
	})
	waitControlFlush(t, flushed) // accepted initial publication
	for _, expression := range []string{`^first\.example$`, `^second\.example$`, `^first\.example$`} {
		if _, err := f.s.opts.Store.Apply(t.Context(), &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "reload-regex", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "inline", Entries: []public.ListEntry{{Type: public.EntryRegex, Value: expression}}}}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		// No desired settings changed, and this fixture has no list callback.
		// Only observing the actually accepted publication can trigger this.
		waitControlFlush(t, flushed)
	}
}
