package cache

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func conditionalHarness(t *testing.T, enabled bool, mutate func(*config.Cache)) *harness {
	t.Helper()
	h := newHarness(t, mutate)
	set, err := upstream.New(upstream.Config{
		Groups: map[string]upstream.GroupConfig{
			"default": {Servers: []string{"127.0.0.1:10001"}},
			"lan":     {Servers: []string{"127.0.0.1:10002"}, Cache: enabled},
			"policy":  {Servers: []string{"127.0.0.1:10003"}, Cache: enabled},
		},
		Conditional: map[string]string{"corp.example.": "lan"},
	}, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.stage.Wait(); _ = set.Close() })
	h.stage.ecs = upstreamstage.NewStage(set)
	h.up.set(func(req *pipeline.Request) (*pipeline.Response, error) {
		p := req.Resolution.(*upstream.QueryPlan)
		ip := "192.0.2.1"
		if p.Group() == "policy" {
			ip = "192.0.2.2"
		}
		r, err := forwarded(ip, 60)(req)
		r.Upstream, r.CacheScope, r.Cacheable = p.Group()+"/test", p.Scope(), p.Cacheable()
		return r, err
	})
	return h
}

func TestConditionalCacheOptInAndSplitHorizon(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-off", true: "opt-in"}[enabled], func(t *testing.T) {
			h := conditionalHarness(t, enabled, nil)
			for _, hint := range []string{"", "policy", "lan"} {
				for i := range 2 {
					req := request("host.corp.example.", dns.TypeA, uint16(i+1))
					req.UpstreamGroup = hint
					r := h.serve(req)
					wantGroup, wantIP := "lan", "192.0.2.1"
					if hint == "policy" {
						wantGroup, wantIP = "policy", "192.0.2.2"
					}
					wantStatus := pipeline.StatusForwarded
					if enabled && (i == 1 || hint == "lan") {
						wantStatus = pipeline.StatusCached
					}
					if r.Status != wantStatus || r.Upstream != wantGroup+"/test" || r.Msg.Answer[1].(*dns.A).A.String() != wantIP {
						t.Fatalf("hint %q iteration %d: status=%v upstream=%s answer=%v", hint, i, r.Status, r.Upstream, r.Msg.Answer)
					}
				}
			}
			wantCalls := int64(6)
			if enabled {
				wantCalls = 2
			}
			if h.up.calls.Load() != wantCalls {
				t.Fatalf("upstream calls=%d, want %d", h.up.calls.Load(), wantCalls)
			}
		})
	}
}

func TestConditionalCacheTTLAndStale(t *testing.T) {
	h := conditionalHarness(t, true, func(c *config.Cache) {
		c.MinTTL, c.MaxTTL = config.Duration(2*time.Minute), config.Duration(3*time.Minute)
	})
	h.serve(request("host.corp.example.", dns.TypeA, 1))
	h.clk.Advance(time.Second)
	hit := h.serve(request("host.corp.example.", dns.TypeA, 2))
	if hit.Status != pipeline.StatusCached || hit.Msg.Answer[0].Header().Ttl != 179 || hit.Msg.Answer[1].Header().Ttl != 119 {
		t.Fatalf("conditional TTL clamps/countdown lost: %v %v", hit.Status, ttls(hit.Msg))
	}
	h.clk.Advance(2 * time.Minute)
	h.up.set(failing(pipeline.StatusServfailUpstream))
	stale := h.serve(request("host.corp.example.", dns.TypeA, 3))
	if stale.Status != pipeline.StatusCachedStale || stale.Upstream != "lan/test" || stale.Msg.Answer[1].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatalf("conditional stale lost original answer/upstream: %v %v", stale.Status, stale.Msg)
	}
}

func TestConditionalCachePrefetch(t *testing.T) {
	h := conditionalHarness(t, true, func(c *config.Cache) { c.Prefetch.Threshold = 1 })
	h.serve(request("host.corp.example.", dns.TypeA, 1))
	h.clk.Advance(55 * time.Second)
	r := h.serve(request("host.corp.example.", dns.TypeA, 2))
	if r.Status != pipeline.StatusPrefetchRefresh || r.Upstream != "lan/test" {
		t.Fatalf("conditional prefetch missing: %v", r.Status)
	}
	h.stage.Wait()
	if h.up.calls.Load() != 2 {
		t.Fatalf("prefetch calls=%d, want 2", h.up.calls.Load())
	}
}

func TestConditionalCacheInspectionHonorsForwardZoneOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-off", true: "opt-in"}[enabled], func(t *testing.T) {
			h := conditionalHarness(t, enabled, nil)
			u := h.stage.ecs.(*upstreamstage.Stage)
			resolution, err := upstreamstage.NewView(u.Set(), nil, false, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			u.Capture = func(*pipeline.Request) (*upstreamstage.View, error) { return resolution, nil }
			view, err := NewView(Settings{}, false, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			h.stage.Capture = func(*pipeline.Request) (*View, error) { return view, nil }
			req := request("host.corp.example.", dns.TypeA, 1)
			req.UpstreamGroup = "lan"
			refs := &atomic.Int64{}
			refs.Store(1)
			req.Serving = &captureLease{refs: refs, view: view}
			defer req.Serving.Release()
			h.serve(req)
			_, found, eligible, err := h.stage.Inspect(req)
			if err != nil || found != enabled || eligible != enabled || h.up.calls.Load() != 1 {
				t.Fatalf("inspection found=%t eligible=%t err=%v calls=%d", found, eligible, err, h.up.calls.Load())
			}
		})
	}
}
