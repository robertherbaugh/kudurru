package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/audit/spool"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

func TestAuditLogDiagnosisSurvivesMissingStartupResource(t *testing.T) {
	realCfg := auditLogTestAPI(t)
	target, err := url.Parse(realCfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := rest.TransportFor(realCfg)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	var missing atomic.Bool
	missing.Store(true)
	var lists atomic.Int64
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/audit.kudurru.io/v1alpha1/namespaces/"+kube.PodNamespace()+"/auditlogs" {
			lists.Add(1)
			if missing.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(metav1.Status{Status: metav1.StatusFailure, Code: 404, Reason: metav1.StatusReasonNotFound})
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	cfg := &rest.Config{Host: gate.URL}
	diagnose := auditLogDiagnosis(cfg)
	if lists.Load() != 0 {
		t.Fatal("diagnostics delayed startup with acquisition")
	}
	sp, err := spool.Open(t.Context(), spool.Options{Path: filepath.Join(t.TempDir(), spool.FileName), Node: "diagnosis"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	store, err := openAuditLog(t.Context(), cfg, sp, bytes.Repeat([]byte{1}, 32), nil, slog.New(slog.DiscardHandler), 2*time.Second)
	if err != nil || store != nil {
		t.Fatal("missing startup resource must retain existing local-spool fallback")
	}
	if result := diagnose(t.Context()); result.State != auditlog.DiagnosticMissing {
		t.Fatalf("missing resource not diagnosed: %+v", result)
	}
	missing.Store(false)
	if result := diagnose(t.Context()); result.State != auditlog.DiagnosticAvailable || !result.Exact || result.Count != 0 {
		t.Fatalf("diagnostic did not recover against real metadata API: %+v", result)
	}
	// The old nil reader/shipper decision is deliberately not rewritten by a
	// diagnostic observation. Public hints tell the operator to restart it.
}

func TestAuditLogDiagnosisHonorsExpectedClusterIdentity(t *testing.T) {
	for _, matches := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong cluster", true: "matching cluster"}[matches], func(t *testing.T) {
			var probes, lists atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					probes.Add(1)
					id := "other"
					if matches {
						id = "expected"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"name": "kube-system", "uid": id}})
					return
				}
				lists.Add(1)
				_, _ = w.Write([]byte(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"1"},"items":[]}`))
			}))
			t.Cleanup(backend.Close)
			cfg, err := kube.WithExpectedClusterID(&rest.Config{Host: backend.URL}, "expected", time.Second)
			if err != nil {
				t.Fatal("construct pinned transport")
			}
			result := auditLogDiagnosis(cfg)(t.Context())
			wantLists, wantState := int64(0), auditlog.DiagnosticUnavailable
			if matches {
				wantLists, wantState = 1, auditlog.DiagnosticAvailable
			}
			if probes.Load() != 1 || lists.Load() != wantLists || result.State != wantState || result.Exact != matches {
				t.Fatal("audit probe bypassed or lost its supplied cluster identity guard")
			}
		})
	}
}
