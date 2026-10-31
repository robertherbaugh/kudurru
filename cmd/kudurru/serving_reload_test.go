package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
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
