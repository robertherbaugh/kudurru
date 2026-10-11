package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// TestLoginLockoutCountsClientsBehindTrustedProxy is the regression test of
// the M5 security finding "login lockouts count the reverse proxy, not the
// client": serve.go built the identity stack without ClientIP, so behind
// listeners.api.trustedProxies every login came from the proxy address and
// five failed logins by anyone locked every user out for five minutes.
// The test wires the stack and the API server the way serve.go does.
func TestLoginLockoutCountsClientsBehindTrustedProxy(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Listeners.API.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	cfg.Auth.Session.Secure = "never"
	st := authtest.Memory(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	logger := slog.New(slog.DiscardHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := identityOptions(cfg, st, keys.Keys{Current: key}, logger)
	opts.Out = io.Discard
	opts.Getenv = func(k string) string {
		if k == users.EnvAdminPassword {
			return "admin-password-1"
		}
		return ""
	}
	ids, err := stack.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	authMux := http.NewServeMux()
	ids.Register(authMux)
	srv, err := api.New(api.Options{
		Store: st, Validator: authtest.Validator(t), Config: cfg,
		Authenticate: ids.Middleware(), Authorizer: ids.Authorizer(), AuthHandler: authMux,
		Logger: logger, NodeName: "node-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	login := func(client, password string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login",
			bytes.NewBufferString(`{"username":"admin","password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", client)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for range 5 {
		if code := login("198.51.100.7", "wrong-password"); code != http.StatusUnauthorized {
			t.Fatalf("failed login: %d", code)
		}
	}
	if code := login("198.51.100.7", "admin-password-1"); code != http.StatusTooManyRequests {
		t.Fatalf("the guessing client is not locked out: %d", code)
	}
	if code := login("198.51.100.8", "admin-password-1"); code != http.StatusOK {
		t.Fatalf("another client behind the same proxy: %d, want 200", code)
	}
}
