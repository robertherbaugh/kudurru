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
