package dnsdist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

func resolverFixture(now time.Time) resolverObservation {
	e := cluster.Endpoint{Name: "node-a", Address: "127.0.0.1:5353", InternalAddress: "127.0.0.1:8082", Ready: true, Serving: true}
	st := &internalapi.ResolverState{Version: 1, Node: e.Name, Instance: strings.Repeat("a", 64), Publication: 1, Identity: strings.Repeat("b", 64), Phase: "0:0", ObservedAt: now, ValidUntil: now.Add(internalapi.ResolverLease), Available: true, BlockingEnabled: true, AuthoritativeAvailable: true}
	return resolverObservation{endpoints: []cluster.Endpoint{e}, results: []backendZones{{endpoint: e, zones: internalapi.ZoneList{Node: e.Name, Resolver: st}}}, eligible: true, sent: now}
}

func TestResolverOriginalClientIsolationBypassReasons(t *testing.T) {
	now := time.Now()
	for _, reason := range []string{"async-ptr", "dhcp-runtime", "client-scope", "rule-client-scope", "zone-client-acl", "client-acl", "ecs", "rebinding-client-scope", "rate-limit", "untrusted-console-text"} {
		o := resolverFixture(now)
		o.results[0].zones.Resolver.Available = false
		o.results[0].zones.Resolver.UnavailableReason = reason
		want := "bypass-" + reason
		if reason == "untrusted-console-text" {
			want = "bypass-input-unavailable"
		}
		key, expiry, mode := resolverTarget(o, now, map[string]*resolverHistory{})
		if key != "" || !expiry.IsZero() || mode != want {
			t.Fatal("unavailable original selected a cache or unbounded mode", reason)
		}
	}
}

func TestResolverTargetSequenceABAExpiryAndOwner(t *testing.T) {
	now := time.Now()
	o := resolverFixture(now)
	histories := map[string]*resolverHistory{}
	first, _, mode := resolverTarget(o, now, histories)
	if first == "" || mode != "qualified" {
		t.Fatal("original not qualified")
	}
	st := o.results[0].zones.Resolver
	st.Publication = 2
	st.Identity = strings.Repeat("c", 64)
	st.ObservedAt = now.Add(time.Second)
	second, _, _ := resolverTarget(o, now, histories)
	if second == "" || second == first {
		t.Fatal("replacement cache identity reused")
	}
	st.Publication = 3
	st.Identity = strings.Repeat("b", 64)
	st.ObservedAt = now.Add(2 * time.Second)
	third, _, _ := resolverTarget(o, now, histories)
	if third == "" || third == first {
		t.Fatal("ABA reused original cache")
	}
	st.Publication = 1
	if key, _, mode := resolverTarget(o, now, histories); key != "" || mode != "bypass-stale" {
		t.Fatal("sequence rollback accepted")
	}
	st.Publication = 3
	st.Identity = strings.Repeat("c", 64)
	if key, _, _ := resolverTarget(o, now, histories); key != "" {
		t.Fatal("same-sequence rewrite accepted")
	}
	st.Identity = strings.Repeat("b", 64)
	st.Node = "other"
	if key, _, _ := resolverTarget(o, now, histories); key != "" {
		t.Fatal("source owner substituted")
	}
	st.Node = "node-a"
	if key, _, _ := resolverTarget(o, now.Add(internalapi.ResolverLease), histories); key != "" {
		t.Fatal("lease expiry was renewed by local tick")
	}
}

func TestResolverRetiredInstanceAndHistoryBound(t *testing.T) {
	now := time.Now()
	o := resolverFixture(now)
	h := map[string]*resolverHistory{}
	_, _, _ = resolverTarget(o, now, h)
	st := o.results[0].zones.Resolver
	original := st.Instance
	st.Instance = strings.Repeat("d", 64)
	st.Publication = 1
	if k, _, _ := resolverTarget(o, now, h); k == "" {
		t.Fatal("fresh runtime could not reconnect")
	}
	st.Instance = original
	if k, _, _ := resolverTarget(o, now, h); k != "" {
		t.Fatal("retired runtime resurrected")
	}
	for i := range 32 {
		h[resolverSourceKey(o.endpoints[0])].retired[strings.Repeat("e", i+1)] = true
	}
	st.Instance = strings.Repeat("f", 64)
	if k, _, _ := resolverTarget(o, now, h); k != "" {
		t.Fatal("bounded history overflow forgot retired provenance")
	}
}

type gatedResolverZones struct {
	mu          sync.Mutex
	observation resolverObservation
	failed      bool
}

func (g *gatedResolverZones) Zones(context.Context, string) (internalapi.ZoneList, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failed {
		return internalapi.ZoneList{}, internalapi.ErrUnauthorized
	}
	st := *g.observation.results[0].zones.Resolver
	st.ObservedAt = time.Now()
	st.ValidUntil = st.ObservedAt.Add(300 * time.Millisecond)
	return internalapi.ZoneList{Node: st.Node, Resolver: &st}, nil
}

func TestResolverControllerFailureExpiryAndSourceEpoch(t *testing.T) {
	fixture := resolverFixture(time.Now())
	zones := &gatedResolverZones{observation: fixture}
	h := newHarness(t, func(c *Config) {
		c.Zones = zones
		c.ZonePoll = 50 * time.Millisecond
		c.Options.AuthoritativeCache = true
	})
	h.waitFor("initial resolver endpoint", func() bool { h.sup.mu.Lock(); defer h.sup.mu.Unlock(); return len(h.sup.endpoints) > 0 })
	// Harness names differ; qualify against the actual accepted source owner.
	h.sup.mu.Lock()
	ep := h.sup.endpoints[0]
	h.sup.mu.Unlock()
	zones.mu.Lock()
	zones.observation.results[0].zones.Resolver.Node = ep.Name
	zones.mu.Unlock()
	h.waitFor("qualified resolver cache", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.cacheMode == "qualified" && h.sup.authoritativeCacheMode == "qualified"
	})
	zones.mu.Lock()
	zones.failed = true
	zones.mu.Unlock()
	h.waitFor("auth failure cache bypass", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.cacheMode == "bypass-unavailable" && h.sup.authoritativeCacheMode == "bypass-unavailable"
	})
	zones.mu.Lock()
	zones.failed = false
	zones.mu.Unlock()
	h.waitFor("authenticated reconnect", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.cacheMode == "qualified" && h.sup.authoritativeCacheMode == "qualified"
	})
	// A source A->B->A event fences a previously pulled A response even when
	// the final endpoint bytes are identical. No stale observation can reattach.
	fence, eps, _ := h.sup.resolverFence()
	h.sup.noteResolverSource(eps, false)
	h.sup.noteResolverSource(eps, false)
	if current, _, _ := h.sup.resolverFence(); current == fence {
		t.Fatal("discovery ABA did not fence response")
	}
}

type blockingZoneReader struct{ calls, active, peak atomic.Int32 }

func (b *blockingZoneReader) Zones(ctx context.Context, _ string) (internalapi.ZoneList, error) {
	b.calls.Add(1)
	n := b.active.Add(1)
	defer b.active.Add(-1)
	for p := b.peak.Load(); n > p; p = b.peak.Load() {
		if b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-ctx.Done()
	return internalapi.ZoneList{}, ctx.Err()
}
func TestResolverPullConcurrencyAndCancellation(t *testing.T) {
	b := &blockingZoneReader{}
	s := &Supervisor{cfg: Config{Zones: b}}
	eps := make([]cluster.Endpoint, 20)
	for i := range eps {
		eps[i].InternalAddress = "127.0.0.1:8082"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	results, _ := s.readBackendZones(ctx, eps)
	if len(results) != 20 || b.peak.Load() > 2 || b.active.Load() != 0 {
		t.Fatal("pull concurrency/cancellation escaped bounds")
	}
}

func TestResolverControllerRejectsDelayedDiscoveryAndChildReplies(t *testing.T) {
	o := resolverFixture(time.Now())
	s, err := New(Config{Options: edge.Options{AuthoritativeCache: true}, Source: &cluster.StaticSource{}, Binary: fakeBinary(t), ConfigDir: t.TempDir(), ChildEnv: []string{"KUDURRU_FAKE_CONSOLE_MODE=ack"}})
	if err != nil {
		t.Fatal(err)
	}
	s.endpoints = o.endpoints
	s.status = &ServerStatus{}
	s.noteResolverSource(o.endpoints, false)
	o.fence, _, _ = s.resolverFence()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.runResolverCache(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	wait := func(mode string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			s.mu.Lock()
			got := s.cacheMode
			if s.authoritativeCacheMode != got {
				got = "different-pool-modes"
			}
			s.mu.Unlock()
			if got == mode {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("resolver controller mode = %s, want %s", got, mode)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	s.offerResolverObservation(o)
	wait("qualified")
	for _, replace := range []func(){
		func() { s.noteResolverSource(nil, true); s.noteResolverSource(o.endpoints, false) },
		func() { s.mu.Lock(); s.cacheChildEpoch++; s.mu.Unlock(); s.wakeResolverCache() },
	} {
		replace()
		wait("bypass-source")
		s.offerResolverObservation(o) // reply from before replacement
		time.Sleep(150 * time.Millisecond)
		s.mu.Lock()
		mode, identity := s.cacheMode, s.cacheIdentity
		authMode, authIdentity := s.authoritativeCacheMode, s.authoritativeCacheIdentity
		s.mu.Unlock()
		if mode != "bypass-source" || identity != "" || authMode != "bypass-source" || authIdentity != "" || s.resolverObservationCurrent(o.fence) {
			t.Fatal("delayed original observation reattached cache or zone routing")
		}
		o.fence, _, _ = s.resolverFence()
		s.offerResolverObservation(o)
		wait("qualified")
	}
}

func TestResolverControllerLostAcknowledgmentAndRetry(t *testing.T) {
	zones := &gatedResolverZones{observation: resolverFixture(time.Now())}
	h := newHarness(t, func(c *Config) {
		c.Zones = zones
		c.Options.AuthoritativeCache = true
		if err := os.MkdirAll(c.ConfigDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(c.ConfigDir, "fail-ping"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	h.waitFor("lost cache acknowledgment", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.cacheMode == "uncertain" && h.sup.authoritativeCacheMode == "uncertain" && h.sup.cacheError != "" && h.sup.cacheIdentity == "" && h.sup.authoritativeCacheIdentity == ""
	})
	if ready, _ := h.sup.Ready(); ready {
		t.Fatal("uncertain command outcome reported ready")
	}
	before := h.count("console ping")
	time.Sleep(150 * time.Millisecond)
	if h.count("console ping") != before {
		t.Fatal("cache failure retried without backoff")
	}
	if err := os.Remove(filepath.Join(h.dir, "fail-ping")); err != nil {
		t.Fatal(err)
	}
	h.waitFor("bounded fresh cache retry", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.cacheMode == "qualified" && h.sup.authoritativeCacheMode == "qualified" && h.sup.cacheError == "" && h.sup.cacheIdentity != ""
	})
	if h.count("start") < 2 {
		t.Fatal("uncertain cache acknowledgment retained the original child")
	}
}

func TestResolverTargetLegacyUnavailableAndBoundaries(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		edit func(*resolverObservation)
	}{
		{"legacy", func(o *resolverObservation) { o.results[0].zones.Resolver = nil }},
		{"missing", func(o *resolverObservation) { o.results = nil }},
		{"unavailable-input", func(o *resolverObservation) { o.results[0].zones.Resolver.Available = false }},
		{"overlong-lease", func(o *resolverObservation) { o.results[0].zones.Resolver.ValidUntil = now.Add(time.Minute) }},
		{"past-boundary", func(o *resolverObservation) { o.results[0].zones.Resolver.DisabledUntil = now.Add(time.Second) }},
		{"mismatched-owner", func(o *resolverObservation) { o.results[0].zones.Node = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := resolverFixture(now)
			tc.edit(&o)
			if k, _, mode := resolverTarget(o, now, map[string]*resolverHistory{}); k != "" || mode == "qualified" {
				t.Fatal("ineligible original source enabled caching")
			}
		})
	}
}

func TestResolverUnconfiguredStartupCannotRestorePacketCache(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Options.AuthoritativeCache = true })
	h.waitFor("explicit unconfigured bypass", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.cacheMode == "bypass-unconfigured"
	})
	h.sup.mu.Lock()
	eps := h.sup.endpoints
	h.sup.mu.Unlock()
	topology, err := h.sup.topology(eps, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range topology.Pools {
		if pool.Cache != nil {
			t.Fatal("unconfigured proof restored an eligible resolver cache")
		}
	}
}
