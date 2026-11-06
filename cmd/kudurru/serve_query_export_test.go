package main

import (
	"context"
	"encoding/json"
	"io"
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
	// DNS can serve before the initial Ready status and lifecycle source writes
	// finish. Observe those actual commits before the single login mutation;
	// do not replay a login refused by its original publication fence.
	awaitControlMountedSource(t, cfg)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + "/api/v1", http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
	awaitQueryExportHTTPPublication(t, client, cfg.Listeners.Ops.Address)
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

// The settled source is not yet an acknowledgment that the complete HTTP
// publication has been built. Observe both public reads and protected serving
// readiness before submitting login once. These observations are not a writer
// reservation; the original login still passes every independent final gate.
func awaitQueryExportHTTPPublication(t *testing.T, client *contractClient, opsAddress string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.base+"/auth/status", nil)
		if err != nil {
			t.Fatal("query export publication request construction failed")
		}
		response, err := client.http.Do(req)
		if err != nil {
			t.Fatal("query export publication observation failed")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || len(body) > 64<<10 {
			t.Fatal("query export publication response exceeded its bounds")
		}
		if response.StatusCode == http.StatusOK {
			var status api.AuthStatus
			if json.Unmarshal(body, &status) != nil || !status.LocalLogin || status.Session == nil {
				t.Fatal("query export publication omitted local login capability")
			}
			// Auth status is a read capability and can be available while the
			// complete publication is still being prepared for commands.
			healthRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+opsAddress+"/healthz", nil)
			if err != nil {
				t.Fatal("query export readiness request construction failed")
			}
			healthResponse, err := client.http.Do(healthRequest)
			if err != nil {
				t.Fatal("query export readiness observation failed")
			}
			healthBody, readErr := io.ReadAll(io.LimitReader(healthResponse.Body, (64<<10)+1))
			closeErr := healthResponse.Body.Close()
			var health struct {
				Components map[string]struct {
					Ready bool `json:"ready"`
				} `json:"components"`
			}
			if readErr != nil || closeErr != nil || len(healthBody) > 64<<10 || json.Unmarshal(healthBody, &health) != nil || (healthResponse.StatusCode != http.StatusOK && healthResponse.StatusCode != http.StatusServiceUnavailable) {
				t.Fatal("query export readiness response invalid")
			}
			if serving, registered := health.Components["serving"]; registered && serving.Ready {
				return
			}
		} else {
			var failure struct{ Error api.ErrorBody }
			if response.StatusCode != http.StatusServiceUnavailable || json.Unmarshal(body, &failure) != nil || failure.Error.Key != "generation_pending" {
				t.Fatalf("query export publication observation: unexpected status=%d", response.StatusCode)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("query export HTTP publication did not become available")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
