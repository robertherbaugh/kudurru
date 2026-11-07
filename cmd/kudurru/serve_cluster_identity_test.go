package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/settings"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

func TestRunServeKubeOnlineWrongCluster(t *testing.T) {
	var ordinary atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"other-cluster"}}`))
			return
		}
		ordinary.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer api.Close()
	cfg := offlineServeConfig(t, api.URL)
	err := runServe(t.Context(), cfg, "")
	if !errors.Is(err, kube.ErrClusterIdentityMismatch) || ordinary.Load() != 0 {
		t.Fatalf("wrong-cluster startup: refused=%t ordinary_requests=%d", errors.Is(err, kube.ErrClusterIdentityMismatch), ordinary.Load())
	}
}

func TestRunServeKubeOfflineMissingExpectedIdentity(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer api.Close()
	cfg := offlineServeConfig(t, api.URL)
	cfg.Store.Kube.ExpectedClusterID = ""
	if err := runServe(t.Context(), cfg, ""); err == nil {
		t.Fatal("outage startup without an independent pin was admitted")
	}
}

// Exercise the separate production settings client and its saved-state path.
// A URL-wrapped identity refusal must never be mistaken for an outage.
func TestKubeSettingsExpectedIdentityCache(t *testing.T) {
	var mode atomic.Int64
	var probes atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			probes.Add(1)
			if mode.Load() == 1 {
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"other-cluster"}}`))
			} else {
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"offline-cluster"}}`))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotFound","code":404}`))
	}))
	defer api.Close()
	cfg := offlineServeConfig(t, api.URL)
	backend, err := settingsBackend(cfg)
	if err != nil {
		t.Fatal("pinned settings backend construction failed")
	}
	cached, ok := backend.(settings.CachedBackend)
	if !ok {
		t.Fatal("configured pin did not enable a bound settings cache")
	}
	if _, _, err := cached.Read(t.Context()); err != nil {
		t.Fatal("verified online settings did not populate cache")
	}
	mode.Store(2)
	if _, _, err := cached.ReadStartup(t.Context()); err != nil {
		t.Fatal("verified matching cache was refused during an outage")
	}
	mode.Store(1)
	refused, err := settingsBackend(cfg) // fresh process-equivalent identity check
	if err != nil {
		t.Fatal("settings backend reconstruction failed")
	}
	if _, _, err := settings.ReadStartup(t.Context(), refused); !errors.Is(err, kube.ErrClusterIdentityMismatch) {
		t.Fatal("cached settings concealed an online cluster identity mismatch")
	}
	copyConfig := *cfg
	copyConfig.Store.Kube.ExpectedClusterID = "other-cluster"
	other, err := settingsBackend(&copyConfig)
	if err != nil {
		t.Fatal("other pinned settings backend construction failed")
	}
	if other.(settings.CachedBackend).Path == cached.Path {
		t.Fatal("settings cache identity did not include the independent pin")
	}
	copyConfig.Store.Kube.ExpectedClusterID = ""
	unconfigured, err := settingsBackend(&copyConfig)
	if err != nil {
		t.Fatal("online compatibility backend construction failed")
	}
	if _, ok := unconfigured.(settings.CachedBackend); ok {
		t.Fatal("missing independent pin enabled settings fallback")
	}
	mode.Store(0)
	before := probes.Load()
	if _, _, err := settings.ReadStartup(t.Context(), unconfigured); err != nil || probes.Load() != before {
		t.Fatal("empty pin did not preserve online settings compatibility")
	}
	mode.Store(2)
	if _, _, err := settings.ReadStartup(t.Context(), unconfigured); err == nil {
		t.Fatal("empty pin admitted offline settings")
	}
}
