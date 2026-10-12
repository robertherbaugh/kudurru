package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// authAPI wires the identity stack and the API server the way serve.go
// does, over a memory store, with the bootstrap admin's password
// admin-password-1. mod adjusts the configuration first.
func authAPI(t *testing.T, mod func(*config.Config)) (*httptest.Server, store.Store) {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Auth.Session.Secure = "never"
	if mod != nil {
		mod(cfg)
	}
	st := authtest.Memory(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	logger := slog.New(slog.DiscardHandler)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
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
	done := make(chan struct{})
	go func() { _ = ids.Run(ctx); close(done) }() // the "identity" runnable: object caches
	t.Cleanup(func() { cancel(); <-done })
	authMux := http.NewServeMux()
	ids.Register(authMux)
	srv, err := api.New(api.Options{
		Store: st, Validator: authtest.Validator(t), Config: cfg,
		Authenticate: ids.Middleware(), Authorizer: ids.Authorizer(), AuthHandler: authMux, Roles: ids.Roles,
		Logger: logger, NodeName: "node-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, st
}

// TestLoginLockoutCountsClientsBehindTrustedProxy is the regression test of
// the M5 security finding "login lockouts count the reverse proxy, not the
// client": serve.go built the identity stack without ClientIP, so behind
// listeners.api.trustedProxies every login came from the proxy address and
// five failed logins by anyone locked every user out for five minutes.
// The test wires the stack and the API server the way serve.go does.
func TestLoginLockoutCountsClientsBehindTrustedProxy(t *testing.T) {
	ts, _ := authAPI(t, func(cfg *config.Config) {
		cfg.Listeners.API.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	})

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

// TestTokenLastUsedIPBehindTrustedProxy is the regression test of the M5
// security finding "token lastUsedIP records the reverse proxy": the
// tokens step read RemoteAddr, so behind listeners.api.trustedProxies
// every token showed the proxy's address.
func TestTokenLastUsedIPBehindTrustedProxy(t *testing.T) {
	ts, _ := authAPI(t, func(cfg *config.Config) {
		cfg.Listeners.API.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	})
	do := func(method, path, body string, hdr ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+"/api/v1"+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}
	decodeInto := func(res *http.Response, v any) {
		t.Helper()
		if err := json.NewDecoder(res.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}

	res := do(http.MethodPost, "/auth/login", `{"username":"admin","password":"admin-password-1"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", res.StatusCode)
	}
	var me struct{ CSRF string }
	decodeInto(res, &me)
	var cookie string
	for _, c := range res.Cookies() {
		cookie = c.Name + "=" + c.Value
	}
	res = do(http.MethodPost, "/auth/tokens", `{"description":"ci"}`, "Cookie", cookie, "X-Kudurru-CSRF", me.CSRF)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d", res.StatusCode)
	}
	var tok struct{ ID, Token string }
	decodeInto(res, &tok)

	if res := do(http.MethodGet, "/auth/me", "", "Authorization", "Bearer "+tok.Token, "X-Forwarded-For", "198.51.100.9"); res.StatusCode != http.StatusOK {
		t.Fatalf("token request: %d", res.StatusCode)
	}
	// lastUsedIP is written by the tokens runnable and reaches the cache
	// through the store watch.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var info struct{ LastUsedIP string }
		decodeInto(do(http.MethodGet, "/auth/tokens/"+tok.ID, "", "Cookie", cookie), &info)
		if info.LastUsedIP == "198.51.100.9" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lastUsedIP = %q, want the client behind the proxy, 198.51.100.9", info.LastUsedIP)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
