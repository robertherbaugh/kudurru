package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheAndECSDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Cache{
		MaxEntries:  200000,
		MinTTL:      Duration(5 * time.Second),
		MaxTTL:      Duration(24 * time.Hour),
		NegativeTTL: Duration(5 * time.Minute),
		Prefetch:    CachePrefetch{Threshold: 5, Window: Duration(2 * time.Hour)},
		ServeStale:  CacheServeStale{TTL: Duration(72 * time.Hour), AnswerTTL: Duration(30 * time.Second), MaxWait: Duration(1800 * time.Millisecond)},
	}
	if cfg.Cache != want {
		t.Errorf("cache defaults:\n got %+v\nwant %+v", cfg.Cache, want)
	}
	if e := cfg.Upstreams.ECS; e != (ECS{Mode: "strip", IPv4Prefix: 24, IPv6Prefix: 56}) {
		t.Errorf("ecs defaults: %+v", e)
	}

	env, err := Load("", []string{
		"KUDURRU_CACHE__SERVESTALE__MAXWAIT=500ms",
		"KUDURRU_CACHE__PREFETCH__THRESHOLD=0",
		"KUDURRU_UPSTREAMS__ECS__MODE=add",
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.Cache.ServeStale.MaxWait.Std() != 500*time.Millisecond || env.Cache.Prefetch.Threshold != 0 || env.Upstreams.ECS.Mode != "add" {
		t.Errorf("env overrides lost: %+v %+v", env.Cache, env.Upstreams.ECS)
	}
}

func TestCacheAndECSValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(`
upstreams: { ecs: { mode: leak, ipv4Prefix: 33, ipv6Prefix: -1 } }
cache: { maxEntries: -1, minTTL: 2h, maxTTL: 1h, negativeTTL: -1s, prefetch: { threshold: -2 } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("invalid cache/ecs config must be rejected")
	}
	for _, want := range []string{
		`upstreams.ecs.mode: unknown value "leak" (strip, forward, add)`,
		"upstreams.ecs.ipv4Prefix: 33",
		"upstreams.ecs.ipv6Prefix: -1",
		"cache.maxEntries: -1 is negative",
		"cache.negativeTTL: -1s is negative",
		"cache.minTTL (2h0m0s) exceeds cache.maxTTL (1h0m0s)",
		"cache.prefetch.threshold: -2 is negative",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}
