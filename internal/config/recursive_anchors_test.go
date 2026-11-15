package config

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagedRootPolicyRequiresExplicitBoundedSeeds(t *testing.T) {
	seed := "12345 15 2 " + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, mode string
		dnssec     bool
		seeds      []string
		valid      bool
	}{
		{"static-default", "static", true, nil, true},
		{"explicit", "managed", true, []string{seed}, true},
		{"implicit-seed", "managed", true, nil, false},
		{"disabled", "managed", false, []string{seed}, false},
		{"unknown", "automatic", true, []string{seed}, false},
		{"short-digest", "managed", true, []string{"12345 15 2 AB"}, false},
		{"unsupported", "managed", true, []string{"12345 253 2 " + strings.Repeat("AB", 32)}, false},
		{"duplicate", "managed", true, []string{seed, ". 3600 IN DS " + seed}, false},
		{"blank", "managed", true, []string{seed, " "}, false},
		{"oversized", "managed", true, []string{strings.Repeat("x", 2049)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := Defaults()
			flat["upstreams.recursion.anchorMode"] = tc.mode
			flat["upstreams.recursion.dnssec"] = tc.dnssec
			flat["upstreams.recursion.trustAnchors"] = tc.seeds
			cfg, err := FromFlat(flat)
			if (err == nil) != tc.valid {
				t.Fatalf("managed policy validity: accepted=%t", err == nil)
			}
			if err != nil {
				return
			}
			policy, err := cfg.Upstreams.Recursion.RootAnchorPolicy()
			if err != nil {
				t.Fatal(err)
			}
			if tc.mode == "static" && (policy.PolicyID != "" || len(policy.Seeds) != 0) {
				t.Fatal("static settings fabricated managed seeds")
			}
			restored, err := FromFlat(Flatten(cfg))
			if err != nil || !reflect.DeepEqual(cfg.Upstreams.Recursion, restored.Upstreams.Recursion) {
				t.Fatal("mode/seed restore changed policy")
			}
		})
	}
}

func TestManagedRootPolicyCanonicalIdentityAndDetachedInput(t *testing.T) {
	a := "12345 15 2 " + strings.Repeat("ab", 32)
	b := "12346 15 2 " + strings.Repeat("cd", 32)
	r := Recursion{DNSSEC: true, AnchorMode: RootAnchorsManaged, TrustAnchors: []string{b, a}}
	first, err := r.RootAnchorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	r.TrustAnchors = []string{". 30 IN DS " + strings.ToUpper(a), ". 60 IN DS " + strings.ToUpper(b)}
	second, err := r.RootAnchorPolicy()
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("presentation changes replaced immutable seed policy")
	}
	first.Seeds[0] = "mutated"
	third, err := r.RootAnchorPolicy()
	if err != nil || !reflect.DeepEqual(second, third) {
		t.Fatal("caller changed retained seed configuration")
	}
	r.TrustAnchors = []string{a}
	different, err := r.RootAnchorPolicy()
	if err != nil || different.PolicyID == second.PolicyID || different.SeedDigest == second.SeedDigest {
		t.Fatal("seed change reused policy identity")
	}
}

func TestRootHintsOnlyNumericOptionalPorts(t *testing.T) {
	for _, tc := range []struct {
		text  string
		valid bool
	}{
		{"192.0.2.53", true}, {"192.0.2.53:5353", true}, {"2001:db8::53", true}, {"[2001:db8::53]:5353", true},
		{"dns.example:53", false}, {"192.0.2.53:0", false}, {"[fe80::1%eth0]:53", false}, {":53", false},
	} {
		flat := Defaults()
		flat["upstreams.recursion.rootHints"] = []string{tc.text}
		_, err := FromFlat(flat)
		if (err == nil) != tc.valid {
			t.Errorf("numeric endpoint %q: accepted=%t", tc.text, err == nil)
		}
	}
}

// D370: managed root anchors are mounted only by the SQLite complete runtime.
// On the Kubernetes driver the setting used to validate and then run the seeds
// as plain static anchors; it is now an explicit validation error in every
// path that builds a configuration (file, environment, cluster document).
func TestManagedRootAnchorsRequireSQLiteDriver(t *testing.T) {
	seed := "12345 15 2 " + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, driver, mode string
		valid              bool
	}{
		{"sqlite-managed", "sqlite", "managed", true},
		{"sqlite-static", "sqlite", "static", true},
		{"kube-static", "kube", "static", true},
		{"kube-managed", "kube", "managed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := Defaults()
			flat["store.driver"] = tc.driver
			flat["upstreams.recursion.anchorMode"] = tc.mode
			flat["upstreams.recursion.dnssec"] = true
			flat["upstreams.recursion.trustAnchors"] = []string{seed}
			_, err := FromFlat(flat)
			if (err == nil) != tc.valid {
				t.Fatalf("accepted=%t, want %t: %v", err == nil, tc.valid, err)
			}
			if !tc.valid && !errors.Is(err, errManagedAnchorsKube) {
				t.Fatal("kube managed refusal is not the specific driver error", err)
			}
		})
	}
	// The cluster document path (settings PATCH, member image settings) uses
	// the same validation on a Kubernetes host.
	host, err := FromFlat(func() map[string]any { f := Defaults(); f["store.driver"] = "kube"; return f }())
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"upstreams": map[string]any{"recursion": map[string]any{"anchorMode": "managed", "dnssec": true, "trustAnchors": []string{seed}}}}
	if _, _, err := ApplyClusterDocument(host, doc); !errors.Is(err, errManagedAnchorsKube) {
		t.Fatal("cluster document enabled managed anchors on the Kubernetes driver", err)
	}
}

// A ConfigMap settings reload on the Kubernetes driver that switches to
// anchorMode managed is refused, and the running configuration keeps static.
func TestManagedRootAnchorsKubeReloadKeepsPrevious(t *testing.T) {
	seed := "12345 15 2 " + strings.Repeat("ab", 32)
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.yaml")
	writeFile(t, settings, "upstreams: { recursion: { dnssec: true, trustAnchors: [\""+seed+"\"] } }\n")
	env := []string{EnvSettingsFile + "=" + settings, "KUDURRU_STORE__DRIVER=kube"}
	cfg, err := Load("", env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Driver != "kube" || cfg.Upstreams.Recursion.AnchorMode != RootAnchorsStatic {
		t.Fatal("fixture did not start static on the Kubernetes driver", cfg.Store.Driver, cfg.Upstreams.Recursion.AnchorMode)
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
	time.Sleep(100 * time.Millisecond)
	writeFile(t, settings, "upstreams: { recursion: { anchorMode: managed, dnssec: true, trustAnchors: [\""+seed+"\"] } }\n")
	deadline := time.Now().Add(5 * time.Second)
	var refused error
	for refused == nil {
		mu.Lock()
		if len(reloads) > 0 {
			refused = reloads[len(reloads)-1]
		}
		mu.Unlock()
		if refused == nil && time.Now().After(deadline) {
			t.Fatal("managed reload was neither refused nor applied")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(refused, errManagedAnchorsKube) {
		t.Fatal("reload refusal is not the specific driver error", refused)
	}
	if r.Current() != cfg || r.Current().Upstreams.Recursion.AnchorMode != RootAnchorsStatic {
		t.Fatal("refused reload replaced the running static configuration")
	}
}
