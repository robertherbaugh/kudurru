package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
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
		Authenticate: ids.Middleware(), Authorizer: ids.Authorizer(), CSRF: auth.CSRF, AuthHandler: authMux, Roles: ids.Roles,
		Logger: logger, NodeName: "node-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, st
}

func TestAPICookieSessionCSRFFailure(t *testing.T) {
	ts, _ := authAPI(t, nil)
	client := ts.Client()
	var err error
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	loginBody := `{"username":"admin","password":"admin-password-1"}`
	login, err := client.Post(ts.URL+"/api/v1/auth/login", "application/json", strings.NewReader(loginBody))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = login.Body.Close() }()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login: %d, want 200", login.StatusCode)
	}
	var me struct{ CSRF string }
	if err := json.NewDecoder(login.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.CSRF == "" {
		t.Fatal("login must return a CSRF token")
	}

	for _, tc := range []struct {
		name, method, path, body, csrf string
		anonymous                      bool
		status                         int
		key                            string
	}{
		{"missing header", "POST", "/dns/blocking", `{"blocking":true}`, "", false, http.StatusForbidden, api.KeyCSRF},
		{"invalid header", "POST", "/dns/blocking", `{"blocking":true}`, "wrong-token", false, http.StatusForbidden, api.KeyCSRF},
		{"patch", "PATCH", "/settings", `{}`, "", false, http.StatusForbidden, api.KeyCSRF},
		{"delete", "DELETE", "/querylog", "", "wrong-token", false, http.StatusForbidden, api.KeyCSRF},
		{"forwarded auth route", "POST", "/auth/tokens", `{"description":"csrf regression"}`, "", false, http.StatusForbidden, api.KeyCSRF},
		{"valid header", "POST", "/auth/tokens", `{"description":"csrf regression"}`, me.CSRF, false, http.StatusCreated, ""},
		{"safe method", "GET", "/auth/me", "", "", false, http.StatusOK, ""},
		{"anonymous", "POST", "/dns/blocking", `{"blocking":true}`, "", true, http.StatusUnauthorized, api.KeyUnauthenticated},
		{"public re-login", "POST", "/auth/login", loginBody, "", false, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+"/api/v1"+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Kudurru-CSRF", tc.csrf)
			caller := client
			if tc.anonymous {
				caller = &http.Client{Transport: client.Transport}
			}
			res, err := caller.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			var envelope api.Error
			if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tc.status || envelope.Body.Key != tc.key {
				t.Fatalf("%s %s: %d %s, want %d %s", tc.method, tc.path, res.StatusCode, envelope.Body.Key, tc.status, tc.key)
			}
			if tc.key == api.KeyCSRF {
				if hint, ok := envelope.Body.Hint.(string); !ok || !strings.Contains(hint, "X-Kudurru-CSRF") {
					t.Fatalf("CSRF failure must explain the required header: %+v", envelope.Body)
				}
				if res.Header.Get("WWW-Authenticate") != "" {
					t.Fatal("CSRF failure must not request new bearer credentials")
				}
			}
		})
	}
}

func TestAuthRoutesWithPathPrefix(t *testing.T) {
	for _, prefix := range []string{"", "/kudurru", "/kudurru/"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			ts, _ := authAPI(t, func(cfg *config.Config) {
				cfg.Listeners.API.PathPrefix = prefix
			})
			base := strings.TrimSuffix(prefix, "/") + "/api/v1"
			client := ts.Client()
			var err error
			client.Jar, err = cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			var csrf string
			do := func(method, path, body string, want int, out any) *http.Response {
				t.Helper()
				req, err := http.NewRequest(method, ts.URL+base+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Kudurru-CSRF", csrf)
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = res.Body.Close() }()
				if res.StatusCode != want {
					raw, _ := io.ReadAll(res.Body)
					t.Fatalf("%s %s: %d, want %d: %s", method, path, res.StatusCode, want, raw)
				}
				if out != nil {
					if err := json.NewDecoder(res.Body).Decode(out); err != nil {
						t.Fatal(err)
					}
				}
				return res
			}
			do(http.MethodGet, "/version", "", http.StatusOK, nil)
			var status struct{ LocalLogin bool }
			do(http.MethodGet, "/auth/status", "", http.StatusOK, &status)
			if !status.LocalLogin {
				t.Fatal("local login should be enabled")
			}
			var me struct{ CSRF string }
			res := do(http.MethodPost, "/auth/login", `{"username":"admin","password":"admin-password-1"}`, http.StatusOK, &me)
			csrf = me.CSRF
			if csrf == "" || len(res.Cookies()) != 1 || res.Cookies()[0].Path != "/" {
				t.Fatal("login must return CSRF and a session cookie covering the API")
			}
			for _, path := range []string{"/auth/me", "/auth/tokens", "/iam/users", "/iam/serviceaccounts", "/iam/rolebindings", "/iam/identityproviders"} {
				do(http.MethodGet, path, "", http.StatusOK, nil)
			}
			var tok struct{ ID, Token string }
			do(http.MethodPost, "/auth/tokens", `{"description":"path-prefix regression"}`, http.StatusCreated, &tok)
			if tok.ID == "" || tok.Token == "" {
				t.Fatal("create token must return its ID and secret")
			}
			do(http.MethodGet, "/auth/tokens/"+tok.ID, "", http.StatusOK, nil)
			do(http.MethodDelete, "/auth/tokens/"+tok.ID, "", http.StatusNoContent, nil)
			do(http.MethodPost, "/auth/password", `{"currentPassword":"admin-password-1","newPassword":"new-admin-password-1"}`, http.StatusOK, &me)
			csrf = me.CSRF
			do(http.MethodPost, "/auth/logout", "", http.StatusNoContent, nil)
			do(http.MethodGet, "/auth/me", "", http.StatusUnauthorized, nil)
		})
	}
}

func TestOIDCRoutesWithPathPrefix(t *testing.T) {
	// Only discovery is needed to check the authorization redirect; callback
	// without a pre-auth cookie exercises routing without exchanging a code.
	mux := http.NewServeMux()
	op := httptest.NewServer(mux)
	t.Cleanup(op.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": op.URL, "authorization_endpoint": op.URL + "/authorize",
			"token_endpoint": op.URL + "/token", "jwks_uri": op.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretFile, []byte("test-client-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"", "/kudurru", "/kudurru/"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			base := strings.TrimSuffix(prefix, "/") + "/api/v1"
			redirectURL := "https://dns.example" + base + "/auth/oidc/corp/callback"
			ts, _ := authAPI(t, func(cfg *config.Config) {
				cfg.Listeners.API.PathPrefix = prefix
				cfg.Auth.IdentityProviders = []map[string]any{{
					"id": "corp", "issuer": op.URL, "clientID": "kudurru",
					"clientSecretFile": secretFile, "redirectURL": redirectURL,
				}}
			})
			client := ts.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			res, err := client.Get(ts.URL + base + "/auth/oidc/corp/start")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusFound {
				t.Fatalf("OIDC start: %d, want 302", res.StatusCode)
			}
			location, err := res.Location()
			if err != nil {
				t.Fatal(err)
			}
			if got := location.Query().Get("redirect_uri"); got != redirectURL {
				t.Fatalf("redirect_uri = %q, want %q", got, redirectURL)
			}
			cookies := res.Cookies()
			if len(cookies) != 1 || cookies[0].Path != base+"/auth/oidc/corp/" {
				t.Fatalf("pre-auth cookie must cover the external callback path: %+v", cookies)
			}
			callback, err := client.Get(ts.URL + base + "/auth/oidc/corp/callback")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = callback.Body.Close() }()
			if callback.StatusCode != http.StatusBadRequest {
				t.Fatalf("OIDC callback without cookie: %d, want 400", callback.StatusCode)
			}
		})
	}
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
