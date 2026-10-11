package config

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsFileLayer(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "kudurru.yaml")
	settings := filepath.Join(dir, "settings", "settings.yaml")
	writeFile(t, node, "node: { name: a }\nsettingsFile: "+settings+"\ncache: { maxEntries: 1000 }\nblocking: { blockTTL: 1m }\n")

	// A missing settings file is an empty layer.
	cfg, err := Load(node, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SettingsFile != settings || cfg.Cache.MaxEntries != 1000 {
		t.Fatalf("settings file %q maxEntries %d", cfg.SettingsFile, cfg.Cache.MaxEntries)
	}

	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, settings, `
cache: { maxEntries: 5000 }
ratelimit: { ipv4: { 32: 100 } }
upstreams:
  groups:
    lan: { servers: ["10.0.0.1:53"] }
  conditional: { "corp.example.": lan }
`)
	cfg, err = Load(node, []string{"KUDURRU_BLOCKING__BLOCKTTL=2m"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.MaxEntries != 5000 {
		t.Errorf("settings file must override the node file: %d", cfg.Cache.MaxEntries)
	}
	if cfg.Blocking.BlockTTL.Std() != 2*time.Minute {
		t.Errorf("the environment must override both files: %s", cfg.Blocking.BlockTTL)
	}
	if cfg.RateLimit.IPv4["32"] != 100 || cfg.Upstreams.Conditional["corp.example."] != "lan" || len(cfg.Upstreams.Groups["lan"].Servers) != 1 {
		t.Errorf("map keys lost: %+v %+v", cfg.RateLimit, cfg.Upstreams)
	}

	// KUDURRU_SETTINGS_FILE wins over the key.
	other := filepath.Join(dir, "other.yaml")
	writeFile(t, other, "cache: { maxEntries: 7 }\n")
	cfg, err = Load(node, []string{EnvSettingsFile + "=" + other})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SettingsFile != other || cfg.Cache.MaxEntries != 7 {
		t.Errorf("env settings file: %q %d", cfg.SettingsFile, cfg.Cache.MaxEntries)
	}
}

func TestSettingsFileRejectsNodeAndUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.yaml")
	for body, want := range map[string]string{
		"node: { name: x }\n":                  "node-scope",
		"listeners: { api: { address: x } }\n": "node-scope",
		"cache: { bogus: 1 }\n":                "unknown settings key",
		"nope: 1\n":                            "unknown settings key",
		"cache: [\n":                           "settings file",
	} {
		writeFile(t, settings, body)
		_, err := Load("", []string{EnvSettingsFile + "=" + settings})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", body, err, want)
		}
	}
}

func TestWriteSettingsFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "s", "settings.yaml")
	if err := WriteSettingsFile(settings, map[string]any{"cache.maxEntries": 42, "upstreams.conditional": map[string]any{"lab.example.": "default"}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettingsFile(settings, map[string]any{"blocking.blockTTL": "30s"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettingsFile(settings, map[string]any{"node.name": "x"}); err == nil {
		t.Error("node-scope keys must be refused")
	}
	cfg, err := Load("", []string{EnvSettingsFile + "=" + settings})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.MaxEntries != 42 || cfg.Blocking.BlockTTL.Std() != 30*time.Second || cfg.Upstreams.Conditional["lab.example."] != "default" {
		t.Errorf("round trip lost values: %+v %+v %+v", cfg.Cache, cfg.Blocking, cfg.Upstreams.Conditional)
	}
}

func TestMergeClusterKeepsNodeKeys(t *testing.T) {
	cur, err := Load("", []string{"KUDURRU_NODE__NAME=a"})
	if err != nil {
		t.Fatal(err)
	}
	cur.Log.Level = "debug" // changed in memory through PATCH /node/config
	fresh, err := Load("", []string{"KUDURRU_NODE__NAME=b", "KUDURRU_CACHE__MAXENTRIES=9", "KUDURRU_LISTENERS__API__ADDRESS=127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := MergeCluster(cur, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if next.Node.Name != "a" || next.Log.Level != "debug" || next.Listeners.API.Address != cur.Listeners.API.Address {
		t.Errorf("node keys must stay: %+v %+v", next.Node, next.Log)
	}
	if next.Cache.MaxEntries != 9 {
		t.Errorf("cluster keys must follow: %d", next.Cache.MaxEntries)
	}
	if got := Changed(cur, next); !slices.Equal(got, []string{"cache.maxEntries"}) {
		t.Errorf("Changed = %v", got)
	}
}

func TestReloadableSubscribers(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := NewReloadable(cfg)
	var got [][]string
	cancel := r.Subscribe(func(old, next *Config, changed []string) {
		if old.Cache.MaxEntries == next.Cache.MaxEntries {
			t.Error("old and next must differ")
		}
		got = append(got, changed)
	})
	next := *cfg
	next.Cache.MaxEntries = 1
	if ch := r.Set(&next); !slices.Equal(ch, []string{"cache.maxEntries"}) {
		t.Errorf("Set = %v", ch)
	}
	same := next
	if ch := r.Set(&same); ch != nil {
		t.Errorf("an unchanged Set must not notify: %v", ch)
	}
	cancel()
	n2 := next
	n2.Cache.MaxEntries = 2
	r.Set(&n2)
	if len(got) != 1 || r.Current().Cache.MaxEntries != 2 {
		t.Errorf("subscriber calls %v current %d", got, r.Current().Cache.MaxEntries)
	}
	if !ChangedUnder([]string{"cache.maxEntries"}, "cache") || ChangedUnder([]string{"cache.maxEntries"}, "cach") {
		t.Error("ChangedUnder prefix semantics")
	}
}

func TestWatchSettingsReloads(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.yaml")
	writeFile(t, settings, "cache: { maxEntries: 10 }\n")
	env := []string{EnvSettingsFile + "=" + settings}
	cfg, err := Load("", env)
	if err != nil {
		t.Fatal(err)
	}
	r := NewReloadable(cfg)
	var mu sync.Mutex
	var reloads []error
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- WatchSettings(ctx, r, WatchOptions{Environ: env, Poll: 50 * time.Millisecond, OnReload: func(_ []string, err error) {
			mu.Lock()
			reloads = append(reloads, err)
			mu.Unlock()
		}})
	}()
	waitUntil := func(what string, f func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !f() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	time.Sleep(100 * time.Millisecond)
	// A ConfigMap-style update: write a new file and rename it over.
	tmp := filepath.Join(dir, ".tmp")
	writeFile(t, tmp, "cache: { maxEntries: 20 }\n")
	if err := os.Rename(tmp, settings); err != nil {
		t.Fatal(err)
	}
	waitUntil("reload to 20", func() bool { return r.Current().Cache.MaxEntries == 20 })

	// An invalid file keeps the last good settings.
	writeFile(t, settings, "cache: { maxEntries: -1 }\n")
	waitUntil("failed reload", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reloads) > 0 && reloads[len(reloads)-1] != nil
	})
	if r.Current().Cache.MaxEntries != 20 {
		t.Errorf("invalid file must not apply: %d", r.Current().Cache.MaxEntries)
	}
	writeFile(t, settings, "cache: { maxEntries: 30 }\n")
	waitUntil("reload to 30", func() bool { return r.Current().Cache.MaxEntries == 30 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestRestartRequired pins the keys applied without a restart; adding one
// here means wiring a config.Reloadable subscriber in serve (reload.go).
func TestRestartRequired(t *testing.T) {
	hot := map[string]bool{"log.level": true}
	for _, p := range []string{
		"upstreams.groups", "upstreams.bootstrap", "upstreams.conditional", "upstreams.ecs.mode", "upstreams.ecs.ipv4Prefix", "upstreams.ecs.ipv6Prefix",
		"cache.maxEntries", "cache.minTTL", "cache.maxTTL", "cache.negativeTTL", "cache.prefetch.threshold", "cache.prefetch.window",
		"cache.serveStale.ttl", "cache.serveStale.answerTTL", "cache.serveStale.maxWait",
		"blocking.enabled", "blocking.defaultResponse", "blocking.blockTTL", "blocking.customIP.v4", "blocking.customIP.v6",
		"querylog.privacy", "querylog.anonymize", "querylog.retention.raw", "querylog.retention.maxDBBytes",
		"querylog.exclude.domains", "querylog.exclude.clients", "querylog.exclude.sudn",
		"ratelimit.enabled", "ratelimit.ipv4", "ratelimit.ipv6", "ratelimit.slip", "ratelimit.maxBuckets", "ratelimit.exempt",
	} {
		hot[p] = true
	}
	for _, k := range Schema {
		if k.RestartRequired == hot[k.Path] {
			t.Errorf("%s: restartRequired %v, but the key is hot=%v", k.Path, k.RestartRequired, hot[k.Path])
		}
		if k.HostOnly && !k.RestartRequired {
			t.Errorf("%s: hostOnly keys are read at start only", k.Path)
		}
	}
}
