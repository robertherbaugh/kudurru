package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/upstream"
)

type upstreamReadFunc func(context.Context) ([]upstream.ServerStatus, error)

func (f upstreamReadFunc) UpstreamStates(ctx context.Context) ([]upstream.ServerStatus, error) {
	return f(ctx)
}

func TestCapturedUpstreamReadsLocalRoutingAndProjection(t *testing.T) {
	for _, route := range []string{"no-querylog", "direct", "central"} {
		t.Run(route, func(t *testing.T) {
			var calls int
			until := time.Now().Add(time.Minute).UTC()
			status := []upstream.ServerStatus{{Group: "original", Server: "original-a", Healthy: false, ConsecutiveFailures: 3, BackoffUntil: until, RTT: 1500 * time.Microsecond}}
			h := newHarness(t, func(o *Options) {
				o.Upstreams = UpstreamStatusFunc(func() []upstream.ServerStatus { panic("live stage status fallback") })
				o.UpstreamReads = upstreamReadFunc(func(ctx context.Context) ([]upstream.ServerStatus, error) {
					if ctx.Err() != nil {
						t.Error("received canceled request")
					}
					calls++
					return status, nil
				})
				if route != "no-querylog" {
					w, _ := querylogFixture(t)
					o.Querylog = w.Reader()
					if route == "central" {
						o.CentralQuerylog = w.Reader()
					}
				}
			})
			path := "/api/v1/stats/upstreams?window=1h"
			if route == "central" {
				path += "&scope=cluster"
			}
			var out UpstreamsBody
			h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(&out)
			if calls != 1 || len(out.Servers) != 1 || out.Servers[0].Node != "node-a" || out.Servers[0].Group != "original" || out.Servers[0].Server != "original-a" || out.Servers[0].Healthy || out.Servers[0].ConsecutiveFailures != 3 || out.Servers[0].RTTMS != 1.5 || out.Servers[0].BackoffUntil == nil || !out.Servers[0].BackoffUntil.Equal(until) {
				t.Fatal("local branch mixed membership/health/telemetry", calls, out)
			}
			if route == "no-querylog" && len(out.Items) != 0 || route != "no-querylog" && len(out.Items) == 0 {
				t.Fatal("upstream health relabeled or discarded query history", out)
			}
			// As in the existing central-routing fixture, a second local
			// reader stands in for central SQL. Preserve its truthful scope.
			if route == "central" && out.Scope.Scope != "node" {
				t.Fatal("local health relabeled query-history scope", out.Scope)
			}
			out.Servers[0].BackoffUntil = nil
			out.Servers[0].Server = "mutated"
			if status[0].Server != "original-a" || !status[0].BackoffUntil.Equal(until) {
				t.Fatal("output aliased original scalar records")
			}
		})
	}
}

func TestCapturedUpstreamReadsFailureNeverFallback(t *testing.T) {
	for _, route := range []string{"no-querylog", "direct", "central"} {
		t.Run(route, func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				o.Upstreams = UpstreamStatusFunc(func() []upstream.ServerStatus { panic("failure used live fallback") })
				o.UpstreamReads = upstreamReadFunc(func(context.Context) ([]upstream.ServerStatus, error) {
					return []upstream.ServerStatus{{Server: "must-not-return"}}, errors.New("original routes unavailable")
				})
				if route != "no-querylog" {
					w, _ := querylogFixture(t)
					o.Querylog = w.Reader()
					if route == "central" {
						o.CentralQuerylog = w.Reader()
					}
				}
			})
			path := "/api/v1/stats/upstreams?window=1h"
			if route == "central" {
				path += "&scope=cluster"
			}
			h.do(http.MethodGet, path, "viewer", nil).want(http.StatusServiceUnavailable)
		})
	}
	for _, timing := range []string{"before", "during"} {
		t.Run(timing, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			s := &Server{opts: Options{UpstreamReads: upstreamReadFunc(func(context.Context) ([]upstream.ServerStatus, error) {
				calls++
				cancel()
				return nil, nil
			})}}
			if timing == "before" {
				cancel()
			}
			if _, err := s.upstreamStates(ctx); err == nil || timing == "before" && calls != 0 {
				t.Fatal("canceled read accepted or called original source", calls, err)
			}
			if _, err := s.upstreamStates(nil); err == nil { //nolint:staticcheck // Deliberate missing request context.
				t.Fatal("missing context accepted")
			}
		})
	}
}

func TestCapturedUpstreamReadsSelfShardFailureIsExplicitPartial(t *testing.T) {
	calls := 0
	f := newClusterLogFixture(t, func(o *Options) {
		o.Upstreams = UpstreamStatusFunc(func() []upstream.ServerStatus { panic("self shard used live status") })
		o.UpstreamReads = upstreamReadFunc(func(context.Context) ([]upstream.ServerStatus, error) {
			calls++
			return nil, errors.New("original self routes unavailable")
		})
	})
	var out UpstreamsBody
	f.h.do(http.MethodGet, "/api/v1/stats/upstreams?scope=cluster&window=1h", "viewer", nil).want(http.StatusOK).json(&out)
	if calls != 1 || !out.Partial || len(out.Nodes) != 2 || out.Nodes[0].Name != "node-a" || out.Nodes[0].OK || out.Nodes[0].Error == "" || !out.Nodes[1].OK || len(out.Items) == 0 || len(out.Servers) != 0 {
		t.Fatal("failed self shard became successful empty health", calls, out)
	}
	// A selected remote node is an operational peer observation, not this
	// request's local resolution routes. It must not invoke the local reader.
	f.h.do(http.MethodGet, "/api/v1/stats/upstreams?node=node-b&window=1h", "viewer", nil).want(http.StatusOK)
	if calls != 1 {
		t.Fatal("selected remote read sampled local captured routes", calls)
	}
	f.h.srv.opts.InternalClient = nil
	f.h.do(http.MethodGet, "/api/v1/stats/upstreams?scope=cluster&window=1h", "viewer", nil).want(http.StatusServiceUnavailable)
}

func TestCapturedUpstreamReadsSelfShardSuccessAndLegacy(t *testing.T) {
	calls := 0
	f := newClusterLogFixture(t, func(o *Options) {
		o.UpstreamReads = upstreamReadFunc(func(context.Context) ([]upstream.ServerStatus, error) {
			calls++
			return []upstream.ServerStatus{{Group: "held", Server: "local-a", Healthy: true}}, nil
		})
	})
	var out UpstreamsBody
	f.h.do(http.MethodGet, "/api/v1/stats/upstreams?scope=cluster&window=1h", "viewer", nil).want(http.StatusOK).json(&out)
	if calls != 1 || out.Partial || len(out.Servers) != 1 || out.Servers[0].Node != "node-a" || out.Servers[0].Server != "local-a" || len(out.Items) == 0 {
		t.Fatal("self shard ignored original health or query history", calls, out)
	}
	h := newHarness(t, func(o *Options) {
		o.Upstreams = UpstreamStatusFunc(func() []upstream.ServerStatus {
			return []upstream.ServerStatus{{Group: "legacy", Server: "legacy-a", Healthy: true}}
		})
	})
	h.do(http.MethodGet, "/api/v1/stats/upstreams", "viewer", nil).want(http.StatusOK).json(&out)
	if len(out.Servers) != 1 || out.Servers[0].Server != "legacy-a" {
		t.Fatal("nil capability changed legacy status", out)
	}
	h.srv.opts.Upstreams = nil
	out = UpstreamsBody{}
	h.do(http.MethodGet, "/api/v1/stats/upstreams", "viewer", nil).want(http.StatusOK).json(&out)
	if len(out.Servers) != 0 {
		t.Fatal("nil legacy source fabricated health", out)
	}
}
