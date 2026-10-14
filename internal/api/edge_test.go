package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func edgeTestServer(t *testing.T, status string, ready bool) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz/ready" {
			if !ready {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(status))
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestEdgeLiveSupervisors(t *testing.T) {
	edge := edgeTestServer(t, `{"ready":true,"dnsdist":{"servers":[{"name":"backend-a","address":"10.0.0.11:5353","state":"up"},{"name":"backend-a","address":"10.0.0.11:5353","state":"up"},{"name":"backend-b","address":"10.0.0.12:5353","state":"down"}]}}`, true)
	vip := edgeTestServer(t, `{"ready":true,"node":"worker-a","addresses":{"dns-a":"10.0.0.53/24","dns-b":"10.0.0.54/24"},"state":{"vips":{"dns-a":{"state":"MASTER","priority":200},"dns-b":{"state":"BACKUP","priority":180}}}}`, true)
	fault := edgeTestServer(t, `{"ready":true,"node":"worker-b","addresses":{"dns-a":"10.0.0.53/24","dns-b":"10.0.0.54/24"},"state":{"vips":{"dns-a":{"state":"FAULT","priority":180},"dns-b":{"state":"MASTER","priority":200}}}}`, true)
	h := newHarness(t, func(o *Options) {
		o.VIPSupervised = true
		o.EdgeSupervisors = func(context.Context) ([]EdgeSupervisorEndpoint, error) {
			return []EdgeSupervisorEndpoint{{Node: "edge-a", EdgeURL: edge, VIPURL: vip}, {Node: "edge-b", EdgeURL: edge, VIPURL: fault}}, nil
		}
	})
	var got EdgeInfo
	h.do(http.MethodGet, "/api/v1/edge", "admin", nil).want(http.StatusOK).json(&got)
	if got.Edge.State != "running" || got.VIP.State != "degraded" || len(got.Supervisors) != 2 || len(got.Backends) != 4 || len(got.VIPs) != 4 {
		t.Fatalf("edge = %+v", got)
	}
	if !got.Backends[0].Up || got.Backends[1].Up {
		t.Fatalf("backends = %+v", got.Backends)
	}
	// A FAULT in another instance must not erase a live MASTER on the node.
	if got.VIPs[3].Node != "worker-b" || got.VIPs[3].State != "MASTER" || got.VIPs[3].Priority != 200 {
		t.Fatalf("VIPs = %+v", got.VIPs)
	}
}

func TestEdgeReadinessAndRollingUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name, status, state string
		ready               bool
	}{
		{"old ready", `{"dnsdist":{"servers":[]}}`, "running", true},
		{"old unready", `{"dnsdist":{"servers":[]}}`, "down", false},
		{"stale backend", `{"ready":false,"dnsdist":{"servers":[{"name":"a","address":"10.0.0.1:5353","state":"up"}]}}`, "down", false},
		{"invalid JSON", `{`, "unavailable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := edgeTestServer(t, tc.status, tc.ready)
			h := newHarness(t, func(o *Options) {
				o.EdgeSupervisors = func(context.Context) ([]EdgeSupervisorEndpoint, error) {
					return []EdgeSupervisorEndpoint{{Node: "edge", EdgeURL: url}}, nil
				}
			})
			var got EdgeInfo
			h.do(http.MethodGet, "/api/v1/edge", "admin", nil).want(http.StatusOK).json(&got)
			if !got.Edge.Enabled || got.Edge.State != tc.state {
				t.Fatalf("edge = %+v", got)
			}
			if !tc.ready && len(got.Backends) > 0 && got.Backends[0].Up {
				t.Fatal("stale backend reported up")
			}
		})
	}
}

func TestEdgeDiscoveryFailuresRemainEnabled(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHarness(t, func(o *Options) {
			o.VIPSupervised = true
			o.EdgeSupervisors = func(context.Context) ([]EdgeSupervisorEndpoint, error) {
				if fail {
					return nil, errors.New("discovery offline")
				}
				return nil, nil
			}
		})
		var got EdgeInfo
		h.do(http.MethodGet, "/api/v1/edge", "admin", nil).want(http.StatusOK).json(&got)
		if !got.Edge.Enabled || !got.VIP.Enabled || got.Edge.State != "unavailable" || got.Backends == nil || got.VIPs == nil || got.Supervisors == nil {
			t.Fatalf("edge = %+v", got)
		}
	}
}

func TestEdgeTimeoutsAreConcurrent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(ts.Close)
	good := edgeTestServer(t, `{"ready":true}`, true)
	h := newHarness(t, func(o *Options) {
		o.VIPSupervised = true
		o.EdgeSupervisors = func(context.Context) ([]EdgeSupervisorEndpoint, error) {
			return []EdgeSupervisorEndpoint{{Node: "a", EdgeURL: ts.URL, VIPURL: good}, {Node: "b", EdgeURL: good, VIPURL: ts.URL}}, nil
		}
	})
	start := time.Now()
	var got EdgeInfo
	h.do(http.MethodGet, "/api/v1/edge", "admin", nil).want(http.StatusOK).json(&got)
	if time.Since(start) > 3500*time.Millisecond {
		t.Fatal("supervisor reads were serialized")
	}
	if got.Edge.State != "degraded" || got.VIP.State != "degraded" || got.Supervisors[0].VIP.State != "running" {
		t.Fatalf("edge = %+v", got)
	}
}
