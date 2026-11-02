package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// Status and current durable enrollment must apply the same installed host
// locks. Canonical seed values deliberately remain defaults for locked keys.
func TestRunServeAuthSessionHostLocksMatchIssuance(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.Session.IdleTimeout = config.Duration(2 * time.Minute)
	cfg.Auth.Session.Lifetime = config.Duration(7 * time.Minute)
	cfg.EnvLocked = append(cfg.EnvLocked, "auth.session.idleTimeout", "auth.session.lifetime")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("auth control mount shutdown failed")
			}
		case <-time.After(15 * time.Second):
			t.Error("auth control mount shutdown did not join")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("auth control mount never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("host lock cookie jar failed")
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var status api.AuthStatus
	mountedFactorRequest(t, client, base, http.MethodGet, "/auth/status", "", nil, http.StatusOK, &status)
	if status.Session == nil || status.Session.IdleTimeout != "2m0s" || status.Session.Lifetime != "7m0s" {
		t.Fatal("status lost installed host session locks")
	}
	var enrolled api.SessionInfo
	before := time.Now()
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &enrolled)
	after := time.Now()
	if enrolled.ExpiresAt == nil || enrolled.IdleExpiresAt == nil || enrolled.ExpiresAt.Before(before.Add(7*time.Minute)) || enrolled.ExpiresAt.After(after.Add(7*time.Minute)) || enrolled.IdleExpiresAt.Before(before.Add(2*time.Minute)) || enrolled.IdleExpiresAt.After(after.Add(2*time.Minute)) {
		t.Fatal("durable issuance disagrees with status and installed host session locks")
	}
	mountedFactorRequest(t, client, base, "POST", "/auth/password", enrolled.CSRF, api.PasswordChangeRequest{CurrentPassword: "mounted-test-password-1", NewPassword: "locked-test-password-2"}, 200, &enrolled)
	mountedFactorRequest(t, client, base, "PATCH", "/settings", enrolled.CSRF, map[string]any{"auth": map[string]any{"session": map[string]any{"lifetime": "10m"}}}, 409, nil)
}

func startAuthReloadMount(t *testing.T, cfg *config.Config) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("auth reload shutdown failed")
				}
			case <-time.After(15 * time.Second):
				t.Error("auth reload shutdown did not join")
			}
		})
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("auth reload mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return stop
}

func TestRunServeAuthControlsCanonicalReloadRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("auth reload cookie jar failed")
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var session api.SessionInfo
	for restart := range 2 {
		stop := startAuthReloadMount(t, cfg)
		if restart == 0 {
			mountedFactorRequest(t, client, base, "POST", "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, 200, &session)
			mountedFactorRequest(t, client, base, "POST", "/auth/password", session.CSRF, api.PasswordChangeRequest{CurrentPassword: "mounted-test-password-1", NewPassword: "reload-test-password-2"}, 200, &session)
			var patched api.SettingsBody
			mountedFactorRequest(t, client, base, "PATCH", "/settings", session.CSRF, map[string]any{"auth": map[string]any{"session": map[string]any{"idleTimeout": "2m", "lifetime": "7m"}}}, 200, &patched)
			if len(patched.RestartRequired) != 0 {
				t.Fatal("live mounted auth settings advertised restart")
			}
		}
		// Read-only readiness polling cannot retry an admission or credential
		// command. Only the original successful PATCH is issued above.
		deadline := time.Now().Add(10 * time.Second)
		for {
			var status api.AuthStatus
			mountedFactorRequest(t, client, base, "GET", "/auth/status", "", nil, 200, &status)
			if status.Session != nil && status.Session.IdleTimeout == "2m0s" && status.Session.Lifetime == "7m0s" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("canonical auth settings were not published/restored")
			}
			time.Sleep(10 * time.Millisecond)
		}
		var records api.AuthList[api.SessionRecordInfo]
		mountedFactorRequest(t, client, base, "GET", "/auth/sessions", "", nil, 200, &records)
		found := false
		for _, row := range records.Items {
			if row.Current {
				found = true
				if session.ExpiresAt == nil || session.IdleExpiresAt == nil || !row.ExpiresAt.Equal(*session.ExpiresAt) || !row.IdleExpiresAt.Equal(*session.IdleExpiresAt) {
					t.Fatal("reload/restart changed original enrolled deadlines")
				}
			}
		}
		if !found {
			t.Fatal("original session did not survive actual server restart")
		}
		stop()
	}
}
