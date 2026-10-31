package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
)

func controlEdgeFlush(t *testing.T) (*dnsdist.CacheFlusher, <-chan struct{}, *atomic.Int64) {
	t.Helper()
	flushed := make(chan struct{}, 8)
	count := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/cache" || r.URL.Query().Get("pool") != "resolver" || r.URL.Query().Get("name") != "." || r.URL.Query().Get("suffix") != "true" {
			t.Error("control change used incorrect edge invalidation")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		count.Add(1)
		_, _ = w.Write([]byte(`{"count":"1","status":"purged"}`))
		flushed <- struct{}{}
	}))
	t.Cleanup(server.Close)
	f := &dnsdist.CacheFlusher{API: &dnsdist.APIClient{BaseURL: server.URL, Key: "fixture-only"}, Log: slog.New(slog.DiscardHandler)}
	t.Cleanup(f.Stop)
	return f, flushed, count
}

func waitControlFlush(t *testing.T, flushed <-chan struct{}) {
	t.Helper()
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("control change did not invalidate local edge cache")
	}
}

func TestControlLegacySettingsFileReload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.yaml")
	environ := []string{config.EnvSettingsFile + "=" + file}
	cfg, err := config.Load("", environ)
	if err != nil {
		t.Fatal("control fixture config failed")
	}
	r := config.NewReloadable(cfg)
	stage := control.New(control.Options{})
	flush, flushed, count := controlEdgeFlush(t)
	targets := reloadTargets{logger: slog.New(slog.DiscardHandler), control: stage, edgeFlush: flush}
	subscribeReload(t.Context(), r, targets)
	query := new(dns.Msg)
	query.SetQuestion("example.", dns.TypeA)
	req := &pipeline.Request{Msg: query, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:1234")}}
	if stage.Decide(req) != control.Allow {
		t.Fatal("empty legacy control defaults refused query")
	}
	if config.WriteSettingsFile(file, map[string]any{"control.allowedNetworks": []string{"192.0.2.0/24"}, "control.blockedQTypes": []string{"A"}}) != nil {
		t.Fatal("control settings write failed")
	}
	if _, err := r.Reload(environ); err != nil || stage.Decide(req) != control.Refuse {
		t.Fatal("legacy settings reload did not atomically adopt control rules")
	}
	waitControlFlush(t, flushed)
	previous := r.Current()
	if config.WriteSettingsFile(file, map[string]any{"control.blockedQTypes": []string{"NO-SUCH-TYPE"}}) != nil {
		t.Fatal("invalid fixture write failed")
	}
	if _, err := r.Reload(environ); err == nil || r.Current() != previous || stage.Decide(req) != control.Refuse {
		t.Fatal("invalid reload replaced last validated config/view")
	}
	// Defend the adoption seam even when called directly with an invalid config.
	invalid := *previous
	invalid.Control = config.Control{BlockedQTypes: []string{"TYPE0"}}
	applyReload(t.Context(), targets, &invalid, []string{"control.blockedQTypes"})
	flush.Stop()
	if stage.Decide(req) != control.Refuse || count.Load() != 1 {
		t.Fatal("invalid control adoption changed rules or flushed edge")
	}
}

func TestControlCompletePublicationInvalidatesEdge(t *testing.T) {
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
				t.Error("control operational subscriber failed")
			}
		case <-time.After(5 * time.Second):
			t.Error("control operational subscriber did not join")
		}
	})
	response := httptest.NewRecorder()
	f.http.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next, err := f.s.Settings(r.Context())
		if err != nil {
			t.Error("control canonical settings capture failed")
			return
		}
		next.Control.BlockedQTypes = []string{"A"}
		if err := f.s.WriteSettings(r.Context(), next, []string{"control.blockedQTypes"}); err != nil {
			t.Error("single control settings command failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, f.request())
	if response.Code != http.StatusNoContent {
		t.Fatal("control canonical command did not acknowledge publication")
	}
	waitControlFlush(t, flushed)
}
