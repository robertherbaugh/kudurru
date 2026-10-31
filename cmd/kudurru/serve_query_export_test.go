package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/license"
)

func TestRunServeQueryExport(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.Querylog.Enabled = true
	cfg.Objects = []map[string]any{{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "export"}, "spec": map[string]any{"zoneName": "export.example.", "type": "primary", "policy": map[string]any{"notify": "none"}}}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("serve failed to stop")
		}
	})
	awaitMountedDNSAbsent(t, cfg, "missing.export.example.")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + "/api/v1", http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
	var login api.SessionInfo
	client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	client.csrf = login.CSRF
	deadline := time.Now().Add(10 * time.Second)
	for {
		var page api.QueriesBody
		client.call(http.MethodGet, "/queries/export?domain=missing.export.example&limit=1", nil, http.StatusOK, &page)
		if len(page.Queries) == 1 {
			if dns.Fqdn(page.Queries[0].Domain) != "missing.export.example." || page.Queries[0].Node != cfg.Node.Name || page.Scope.Scope != "node" || page.Partial {
				t.Fatal("actual export lost filtered query identity or scope")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("query writer did not publish the observed DNS request")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
