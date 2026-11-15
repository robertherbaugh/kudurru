package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

func TestRunServeSQLiteSourceEntriesRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	for restart := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("source mount shutdown failed", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("source mount shutdown did not join")
			}
		}
		defer stop()
		until := time.Now().Add(20 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("source mount startup ended", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(until) {
				t.Fatal("source mount never became ready")
			}
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		var login api.SessionInfo
		mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		const source = "/blocklists/default/source-command/sources/inline/entries"
		if restart == 0 {
			mountedFactorRequest(t, client, base, http.MethodPost, "/blocklists", login.CSRF, servingSourceList(), http.StatusCreated, nil)
			var page api.SourceEntries
			mountedFactorRequest(t, client, base, http.MethodGet, source, login.CSRF, nil, http.StatusOK, &page)
			if page.Total != 2 {
				t.Fatal("initial captured source absent")
			}
			// Mutation is a single HTTP attempt, without a retry transport.
			var result api.SourceEntriesResult
			mountedFactorRequest(t, client, base, http.MethodPatch, source, login.CSRF,
				api.SourceEntriesPatch{Add: []string{"gamma.example"}, Disable: []string{"beta.example"}}, http.StatusOK, &result)
			if result.ResourceVersion == "" || result.Total != 3 || result.Excluded != 1 {
				t.Fatal("mounted source command lacks exact result")
			}
		}
		var page api.SourceEntries
		mountedFactorRequest(t, client, base, http.MethodGet, source, login.CSRF, nil, http.StatusOK, &page)
		if page.Total != 3 || page.Excluded != 1 || len(page.Items) != 3 || page.Items[1].Value != "beta.example" || page.Items[1].Enabled || page.Items[2].Value != "gamma.example" {
			t.Fatal("source edit/exclusion did not survive publication and restart")
		}
		// This fixture disables query history. The admitted domain command
		// returns that semantic error, never the obsolete mutation-route gate.
		var unavailable struct {
			Error api.ErrorBody `json:"error"`
		}
		mountedFactorRequest(t, client, base, http.MethodPost, "/queries/42/actions", login.CSRF, map[string]string{"action": "allow"}, http.StatusServiceUnavailable, &unavailable)
		if unavailable.Error.Key != api.KeyUnavailable || unavailable.Error.Message != "the query log is not stored on this node" {
			t.Fatal("query action did not reach current semantic admission", unavailable.Error)
		}
		stop()
	}
}
