package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/internal/config"
)

type vipRoundTripFunc func(*http.Request) (*http.Response, error)

func (f vipRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func observationConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.VIP.Observation.Provider = "kube-vip"
	cfg.VIP.Observation.Services = []config.VIPServiceBinding{{Name: "dns", Address: "192.0.2.53", Namespace: "apps", Service: "dns"}}
	cfg.VIP.Observation.Interval = config.Duration(time.Second)
	cfg.VIP.Observation.Timeout = config.Duration(100 * time.Millisecond)
	cfg.VIP.Observation.MaxAge = config.Duration(2 * time.Second)
	return cfg
}

func TestVIPObservationDisabledAndOriginalCredentialsDeadline(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	read, run, err := serviceVIPObservation(cfg, nil)
	if err != nil || read != nil || run != nil {
		t.Fatal("disabled observation acquired credentials or started runtime")
	}
	original := rand.Text()
	var requests atomic.Int32
	var wrongCredential atomic.Bool
	cancelled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+original || r.Header.Get("X-Installed-Observer") != "retained" {
			wrongCredential.Store(true)
		}
		<-r.Context().Done()
		select {
		case cancelled <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	installed := &rest.Config{Host: server.URL, BearerToken: original, Timeout: time.Second, WrapTransport: func(inner http.RoundTripper) http.RoundTripper {
		return vipRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			cloned := r.Clone(r.Context())
			cloned.Header.Set("X-Installed-Observer", "retained")
			return inner.RoundTrip(cloned)
		})
	}}
	cfg = observationConfig(t)
	read, run, err = serviceVIPObservation(cfg, installed)
	if err != nil {
		t.Fatal(err)
	}
	installed.BearerToken = rand.Text() // a later config mutation cannot replace captured credentials
	start := time.Now()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("acquisition did not cancel blocked read")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("observer did not stop on cancellation")
	}
	if time.Since(start) > time.Second || requests.Load() != 1 || wrongCredential.Load() {
		t.Fatal("deadline, installed wrapper, original credential or request bound lost")
	}
	if item := read().VIPs[0]; item.Known || item.Reason != "unavailable" {
		t.Fatal("timed out evidence retained ownership")
	}
}

func TestVIPObservationResponseBound(t *testing.T) {
	oversized := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"dns","annotations":{"padding":"` + strings.Repeat("x", (2<<20)+1024) + `"}},"spec":{"type":"LoadBalancer"},"status":{"loadBalancer":{"ingress":[{"ip":"192.0.2.53"}]}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oversized))
	}))
	defer server.Close()
	read, run, err := serviceVIPObservation(observationConfig(t), &rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for read().VIPs[0].Reason == "not_observed" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("observer did not stop on cancellation")
	}
	if item := read().VIPs[0]; item.Known || item.Reason != "unavailable" {
		t.Fatal("oversized source response admitted")
	}
}
