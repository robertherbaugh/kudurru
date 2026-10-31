package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

func TestPinnedClusterIdentityTransport(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"matching", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"expected"}}`, 200, nil},
		{"wrong cluster", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"other"}}`, 200, ErrClusterIdentityMismatch},
		{"missing UID", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system"}}`, 200, errIdentityUntrusted},
		{"wrong object", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"other","uid":"expected"}}`, 200, errIdentityUntrusted},
		{"malformed", `{`, 200, errIdentityUntrusted},
		{"oversized", strings.Repeat(" ", maxIdentityResponse) + `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"expected"}}`, 200, errIdentityUntrusted},
		{"forbidden", "", 403, errIdentityUntrusted},
		{"unavailable", "", 503, errIdentityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probes, ordinary atomic.Int64
			var authenticated atomic.Bool
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				authenticated.Store(true)
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					probes.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				ordinary.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"default","uid":"namespace"}}`))
			}))
			t.Cleanup(server.Close)
			cfg := &rest.Config{Host: server.URL, Transport: server.Client().Transport, BearerToken: "fixture-token"}
			s, err := New(cfg, Options{ExpectedClusterID: "expected", StartupTimeout: time.Second, Logger: testLogger()})
			if err != nil {
				t.Fatal("supported kube driver did not construct")
			}
			t.Cleanup(func() { _ = s.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err = s.CheckClusterIdentity(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("identity result mismatch: got_error=%t want_error=%t", err != nil, tc.want != nil)
			}
			if !authenticated.Load() || probes.Load() == 0 {
				t.Fatal("identity probe did not use configured Kubernetes credentials")
			}
			auxiliary, err := coreclient.NewForConfig(s.RESTConfig())
			if err != nil {
				t.Fatal("auxiliary guarded client did not construct")
			}
			_, auxErr := auxiliary.Namespaces().Get(ctx, "default", metav1.GetOptions{})
			if tc.want == nil {
				if auxErr != nil || ordinary.Load() == 0 || s.Info().ClusterID != "expected" {
					t.Fatal("matching identity did not admit authenticated auxiliary reads")
				}
				if probes.Load() != 1 {
					t.Fatalf("verified identity was probed again: probes=%d", probes.Load())
				}
			} else if auxErr == nil || ordinary.Load() != 0 {
				t.Fatalf("unverified identity admitted ordinary requests: requests=%d", ordinary.Load())
			}
		})
	}
}

func TestPinnedClusterIdentityRecoveryDoesNotReplayWrites(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			if down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"name": "kube-system", "uid": "expected"}})
			return
		}
		if r.Method == http.MethodPost {
			writes.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cfg, identity, err := pinClusterIdentity(&rest.Config{Host: server.URL}, "expected", time.Second)
	if err != nil {
		t.Fatal("guard construction failed")
	}
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal("client construction failed")
	}
	send := func() error {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/mutation", nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
		return err
	}
	if err := send(); err == nil || writes.Load() != 0 {
		t.Fatal("outage admitted a mutation")
	}
	down.Store(false)
	if err := identity.check(t.Context()); err != nil || writes.Load() != 0 {
		t.Fatal("identity recovery replayed the refused mutation")
	}
	if err := send(); err != nil || writes.Load() != 1 {
		t.Fatal("fresh mutation did not execute exactly once after recovery")
	}
}

func TestPinnedClusterIdentityDeadlineCancellationAndRedirect(t *testing.T) {
	t.Run("deadline and cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		_, identity, err := pinClusterIdentity(&rest.Config{Host: server.URL}, "expected", 50*time.Millisecond)
		if err != nil {
			t.Fatal("guard construction failed")
		}
		start := time.Now()
		if err := identity.check(t.Context()); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Fatal("identity probe ignored its bounded deadline")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := identity.check(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("identity probe ignored caller cancellation")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var redirected atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusOK) }))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
		defer server.Close()
		_, identity, err := pinClusterIdentity(&rest.Config{Host: server.URL, BearerToken: "fixture-token"}, "expected", time.Second)
		if err != nil {
			t.Fatal("guard construction failed")
		}
		if err := identity.check(t.Context()); !errors.Is(err, errIdentityUntrusted) || redirected.Load() != 0 {
			t.Fatal("identity probe followed a redirect")
		}
	})
}
