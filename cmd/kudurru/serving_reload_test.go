package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestServingReloadLogsClusterSettingsOverTheFileAsInEffect starts the
// operational watcher with a host file whose ratelimit.enabled (hot) and
// blocking.initStrategy (restart-required) differ from the committed cluster
// settings object. The first pass applies the object's ratelimit.enabled; it
// must say the file's value is not in effect and name the value that is, not
// "settings applied" as if the file edit had taken effect (D373). It does not
// reconfigure blocking.initStrategy, so it must log that key separately as
// kept from startup and must not name the stored value as in effect. A later
// cluster change is logged as applied.
func TestServingReloadLogsClusterSettingsOverTheFileAsInEffect(t *testing.T) {
	f := newServingCommandFixture(t)
	write := func(enabled bool, strategy string) {
		t.Helper()
		response := httptest.NewRecorder()
		f.http.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next, err := f.s.Settings(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			next.RateLimit.Enabled = enabled
			changed := []string{"ratelimit.enabled"}
			if strategy != "" {
				next.Blocking.InitStrategy = strategy
				changed = append(changed, "blocking.initStrategy")
			}
			if err := f.s.WriteSettings(r.Context(), next, changed); err != nil {
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
	if !f.s.opts.Config.RateLimit.Enabled || f.s.opts.Config.Blocking.InitStrategy != "fast" {
		t.Fatal("the fixture's host file must enable the rate limit and load lists fast")
	}
	write(false, "blocking")
	initial, err := ingress.RateLimitFromConfig(f.s.opts.Config.RateLimit)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := ingress.NewRateLimiter(*initial)
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- f.s.runServingReload(ctx, reloadTargets{logger: slog.New(slog.NewJSONHandler(&logs, nil)), limiter: limiter}, nil)
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
	awaitServingRuntime(t, f.s, func() bool {
		return !limiter.Enabled() && strings.Contains(logs.String(), "not in effect") && strings.Contains(logs.String(), "keeps the values it started with")
	})
	first := logs.String()
	if strings.Contains(first, `"msg":"settings applied"`) || !strings.Contains(first, `"keys":["ratelimit.enabled"],"effective":{"ratelimit.enabled":false}`) {
		t.Fatalf("the first pass did not log the cluster value in effect:\n%s", first)
	}
	if strings.Contains(first, "take effect after a restart") || !strings.Contains(first, `keeps the values it started with","keys":["blocking.initStrategy"]`) || strings.Contains(first, `"blocking.initStrategy":`) {
		t.Fatalf("the first pass did not log the restart-required key as kept from startup, without a value:\n%s", first)
	}
	write(true, "")
	awaitServingRuntime(t, f.s, func() bool { return limiter.Enabled() && strings.Contains(logs.String(), `"msg":"settings applied"`) })
	if later := strings.TrimPrefix(logs.String(), first); strings.Contains(later, "not in effect") || !strings.Contains(later, `"keys":["ratelimit.enabled"]`) {
		t.Fatalf("a later cluster change was not logged as applied:\n%s", later)
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

func TestServingReloadJoinsClosedPublisherBeforeCallerCancellation(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Independent owner shutdown precedes cancellation of the subscriber.
	// Reproduces the terminal capture regardless of scheduler ordering.
	f.s.publisher.Close()
	if err := f.s.runServingReload(ctx, reloadTargets{logger: slog.New(slog.DiscardHandler)}, nil); err != nil {
		t.Fatal("closed publisher reported a reload failure", err)
	}
	if ctx.Err() != nil {
		t.Fatal("subscriber waited for caller cancellation")
	}
}
