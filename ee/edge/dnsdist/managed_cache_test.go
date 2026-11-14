package dnsdist

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

func TestManagedCacheRenderBothPoolsAndImmutableRule(t *testing.T) {
	for _, zones := range [][]string{nil, {"example."}, {"."}} {
		top, err := edge.Build(edge.Options{ManagedCacheBypass: true, AuthoritativeCache: true,
			AuthoritativeZones: zones, ProxyProtocolSecret: goldenSecret},
			[]edge.Backend{{Name: "node-a", Address: "192.0.2.1:5353"}}, goldenCreds)
		if err != nil {
			t.Fatal(err)
		}
		for _, renderer := range []Renderer{YAMLRenderer{}, LuaRenderer{}} {
			files, err := renderer.Render(top)
			if err != nil {
				t.Fatal(err)
			}
			if renderer.Name() == "yaml" {
				var cfg yConfig
				if err := yaml.Unmarshal(files[0].Data, &cfg); err != nil {
					t.Fatal(err)
				}
				seen := false
				for _, rule := range cfg.QueryRules {
					if rule.Name == ruleManagedNoCache {
						if seen || rule.Selector.Type != "All" || rule.Action.Type != "SetSkipCache" {
							t.Fatal("managed bypass is not one unconditional continuing rule")
						}
						seen = true
					}
					if rule.Action.Type == "Pool" && !seen {
						t.Fatal("a route precedes managed bypass")
					}
				}
				if !seen || len(cfg.PacketCaches) != 0 || cfg.QueryRules[0].Name != ruleRefuseTransfers || cfg.QueryRules[1].Name != ruleProxySecret || !strings.Contains(string(files[1].Data), luaManagedCacheBypass) {
					t.Fatal("managed target lost cache refusal, authentication or installed marker")
				}
			} else {
				body := string(files[0].Data)
				at := strings.Index(body, "addAction(AllRule(), SetSkipCacheAction()")
				if at < 0 || strings.Index(body, "PoolAction(") < at || strings.Index(body, "SetAdditionalProxyProtocolValueAction(") > at || strings.Contains(body, ":setCache(") || !strings.Contains(body, luaManagedCacheBypass) {
					t.Fatal("Lua managed bypass ordering or cache target changed")
				}
			}
			for i := range top.Pools {
				top.Pools[i].Cache = edge.DefaultResolverCache()
				if _, err := renderer.Render(top); err == nil {
					t.Fatal("managed topology accepted a packet cache", top.Pools[i].Name)
				}
				top.Pools[i].Cache = nil
			}
		}
	}
}

// The existing fake tests process/fence/ack plumbing only. It does not execute
// the Lua check or qualify actual dnsdist caching behavior.
func TestManagedCacheControllerIgnoresObservationsAndFencesChild(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Options.ManagedCacheBypass = true; c.Options.AuthoritativeCache = true })
	h.waitFor("installed managed bypass without polling", h.ready)
	installed := func() bool { h.sup.mu.Lock(); defer h.sup.mu.Unlock(); return h.sup.managedBypassInstalledLocked() }
	if !installed() || strings.Contains(h.sup.resolverCacheCommand(true), ":setCache(") || strings.Contains(h.sup.authoritativeCacheCommand(true), ":setCache(") {
		t.Fatal("bypass permitted a cache attachment")
	}
	o := resolverFixture(time.Now())
	fence, eps, eligible := h.sup.resolverFence()
	if !eligible {
		t.Fatal("fixture source not eligible")
	}
	o.fence, o.endpoints = fence, eps
	o.results[0].endpoint = eps[0]
	o.results[0].zones.Node, o.results[0].zones.Resolver.Node = eps[0].Name, eps[0].Name
	if key, _, mode := resolverTarget(o, time.Now(), map[string]*resolverHistory{}); key == "" || mode != "qualified" {
		t.Fatal("original observation would not actually qualify a static cache")
	}
	h.sup.offerResolverObservation(o)
	h.waitFor("both fixed bypass modes", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.cacheMode == "bypass-managed-anchors" && h.sup.authoritativeCacheMode == "bypass-managed-anchors" && h.sup.cacheIdentity == "" && h.sup.authoritativeCacheIdentity == ""
	})
	h.sup.noteResolverSource(nil, true)
	if installed() || h.ready() {
		t.Fatal("source loss retained installed membership proof")
	}
	h.sup.offerResolverObservation(o)
	h.sup.noteResolverSource(eps, false)
	h.waitFor("source ABA requires new acknowledgment", h.ready)
	h.sup.mu.Lock()
	if h.sup.cacheBypassFence == fence {
		t.Error("source ABA reused installed receipt")
	}
	old := h.sup.child
	h.sup.mu.Unlock()
	if err := old.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitFor("replacement child with new acknowledgment", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.child != old && h.sup.managedBypassInstalledLocked()
	})
	h.sup.offerResolverObservation(o)
	h.stop()
	if installed() {
		t.Fatal("joined worker retained installed proof")
	}
}

func TestManagedCacheUnknownAcknowledgmentNeverReady(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Options.ManagedCacheBypass = true
		if err := os.MkdirAll(c.ConfigDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(c.ConfigDir, "fail-ping"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	h.waitFor("failed acknowledgment", func() bool { return h.count("console ping") > 0 && h.count("stop") > 0 })
	if h.ready() {
		t.Fatal("unknown console completion granted readiness")
	}
	h.sup.mu.Lock()
	installed := h.sup.managedBypassInstalledLocked()
	h.sup.mu.Unlock()
	if installed {
		t.Fatal("unknown acknowledgment granted installed bypass")
	}
}
