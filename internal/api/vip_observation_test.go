package api

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestVIPEvidenceReadPermissionAndNoWriterAuthority(t *testing.T) {
	var reads atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.VIPObservation = func() VIPObservationSnapshot {
			reads.Add(1)
			return VIPObservationSnapshot{Provider: "metallb", VIPs: []VIPOwnerObservation{{Name: "dns", Address: "192.0.2.53", Owner: "node-a", Known: true, CheckedAt: time.Now(), Evidence: &VIPAnnouncerEvidence{Kind: "metallb-l2-status", CreatedAt: time.Now().Add(-time.Hour)}}}}
		}
	})
	h.do(http.MethodGet, "/api/v1/edge", "", nil).want(http.StatusUnauthorized)
	h.do(http.MethodGet, "/api/v1/edge", "zone-admin", nil).want(http.StatusForbidden)
	if reads.Load() != 0 {
		t.Fatal("unauthorized principal acquired observation")
	}
	var got EdgeInfo
	h.do(http.MethodGet, "/api/v1/edge", "admin", nil).want(http.StatusOK).json(&got)
	if got.Observation == nil || got.Observation.VIPs[0].Owner != "node-a" || got.Observation.VIPs[0].Evidence.RenewedAt != nil || got.VIP.Enabled {
		t.Fatal("provider report fabricated supervisor health/heartbeat")
	}
	// Observations add no mutation route and do not relax host-only settings.
	h.do(http.MethodPatch, "/api/v1/settings", "admin", map[string]any{"vip": map[string]any{"observation": map[string]any{"provider": "metallb"}}}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, "/api/v1/node/config", "admin", map[string]any{"vip": map[string]any{"observation": map[string]any{"provider": "metallb"}}}).want(http.StatusBadRequest)
	h.do(http.MethodPost, "/api/v1/edge/weight", "admin", map[string]any{"vip": "dns", "weight": 300}).want(http.StatusNotFound)
}
