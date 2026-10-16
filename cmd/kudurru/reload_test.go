package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func TestSubscribeReloadAppliesHotKeys(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := upstream.New(upstream.ConfigFrom(cfg.Upstreams), upstream.Options{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	fwd := upstreamstage.NewStage(set)
	rc := cache.New(cache.OptionsFrom(cfg.Cache, nil))
	cs := cachestage.New(rc, cachestage.SettingsFrom(cfg.Cache), fwd)
	bs, err := blockingstage.New(blockingstage.OptionsFrom(cfg.Blocking))
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := ingress.NewRateLimiter(ingress.RateLimitConfig{})
	if err != nil {
		t.Fatal(err)
	}
	level := new(slog.LevelVar)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := config.NewReloadable(cfg)
	subscribeReload(ctx, r, reloadTargets{logger: logger, level: level, upstreams: fwd, upOpts: upstream.Options{Logger: logger},
		cache: rc, cacheStg: cs, blocking: bs, limiter: limiter})

	k := cache.MakeKey("a.example.", 1, 1, false, false, netip.Prefix{})
	rc.Set(k, []byte{1}, cache.Positive, time.Minute)

	next := *cfg
	next.Log.Level = "debug"
	next.Upstreams.Groups = map[string]config.UpstreamGroup{
		"default": {Servers: []string{"192.0.2.1:53"}, Strategy: "strict", Timeout: config.Duration(time.Second)},
		"lan":     {Servers: []string{"192.0.2.2:53"}},
	}
	next.Cache.MaxEntries = 0
	next.Blocking.Enabled = false
	next.RateLimit.Enabled = false
	r.Set(&next)

	if level.Level() != slog.LevelDebug {
		t.Errorf("log level %v", level.Level())
	}
	if fwd.Set() == set || len(fwd.Set().Groups()) != 2 {
		t.Errorf("upstream set not swapped: %v", fwd.Set().Groups())
	}
	if _, ok := rc.Peek(k); ok {
		t.Error("an upstream change must flush the cache")
	}
	if !cs.Disabled() {
		t.Error("cache.maxEntries 0 must disable the cache stage")
	}
	if bs.Enabled() {
		t.Error("blocking.enabled false not applied")
	}
	if limiter.Enabled() {
		t.Error("ratelimit.enabled false not applied")
	}

	again := next
	again.Cache.MaxEntries = 10
	again.RateLimit.Enabled = true
	r.Set(&again)
	if cs.Disabled() || !limiter.Enabled() {
		t.Errorf("re-enable: cache disabled %v limiter %v", cs.Disabled(), limiter.Enabled())
	}
}

func TestSettingsWriter(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if settingsWriter(cfg) != nil {
		t.Fatal("no settings file: changes stay in memory")
	}
	kube := *cfg
	kube.Store.Driver = "kube"
	err = settingsWriter(&kube).WriteSettings(context.Background(), cfg, []string{"cache.maxEntries"})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Body.Key != api.KeyManagedBy {
		t.Fatalf("kube: %v", err)
	}

	file := *cfg
	file.SettingsFile = filepath.Join(t.TempDir(), "settings.yaml")
	next := *cfg
	next.Cache.MaxEntries = 1234
	next.Log.Level = "debug" // not in changed: never written
	if err := settingsWriter(&file).WriteSettings(context.Background(), &next, []string{"cache.maxEntries"}); err != nil {
		t.Fatal(err)
	}
	back, err := config.Load("", []string{config.EnvSettingsFile + "=" + file.SettingsFile})
	if err != nil {
		t.Fatal(err)
	}
	if back.Cache.MaxEntries != 1234 || back.Log.Level != "info" {
		t.Fatalf("persisted %d %q", back.Cache.MaxEntries, back.Log.Level)
	}
}

// TestUpstreamReloadIsAllOrNothing: when the new upstream groups are
// invalid, the recursion settings that came with them are not applied to
// the shared resolver either (review of PR #73).
func TestUpstreamReloadIsAllOrNothing(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := recursor.New(upstream.RecursionFrom(cfg.Upstreams.Recursion), recursor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	opts := upstream.Options{Logger: slog.New(slog.DiscardHandler), Recursor: rec}
	changed := []string{"upstreams.groups", "upstreams.recursion.dnssec"}

	bad := *cfg
	bad.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Mode: config.UpstreamModeForward}} // no servers
	bad.Upstreams.Recursion.DNSSEC = false
	if _, err := buildUpstreams(&bad, changed, opts); err == nil {
		t.Fatal("a group without servers was accepted")
	}
	if !rec.Config().DNSSEC {
		t.Fatal("a failed reload changed the resolver")
	}

	good := bad
	good.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Mode: config.UpstreamModeRecursive}}
	set, err := buildUpstreams(&good, changed, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if rec.Config().DNSSEC || set.Recursor() != rec {
		t.Fatal("a successful reload must apply the recursion settings")
	}
}
