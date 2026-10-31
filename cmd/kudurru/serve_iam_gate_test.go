package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/iamgate"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/iammigrate"
)

type migrationAdmissionSource struct{ state atomic.Int32 }

func (s *migrationAdmissionSource) Capture(context.Context) (iammigrate.Status, *iammigrate.Bindings, error) {
	switch s.state.Load() {
	case 1:
		return iammigrate.Status{State: iammigrate.Running}, nil, nil
	case 2:
		return iammigrate.Status{State: iammigrate.Blocked}, nil, errors.New("private API diagnostic")
	default:
		return iammigrate.Status{State: iammigrate.Native, RemoteAllowed: true}, nil, nil
	}
}

func TestServeAuthMigrationGateCoversPublicAndAuthenticatedRoutes(t *testing.T) {
	for _, prefix := range []string{"", "/kudurru", "/kudurru/"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			source := &migrationAdmissionSource{}
			ts, _ := authAPIWithGate(t, func(cfg *config.Config) { cfg.Listeners.API.PathPrefix = prefix }, iamgate.New(source))
			client := ts.Client()
			client.Jar, _ = cookiejar.New(nil)
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			base := ts.URL + strings.TrimSuffix(prefix, "/") + "/api/v1"
			loginBody := `{"username":"admin","password":"admin-password-1"}`
			login, err := client.Post(base+"/auth/login", "application/json", strings.NewReader(loginBody))
			if err != nil {
				t.Fatal(err)
			}
			_ = login.Body.Close()
			if login.StatusCode != http.StatusOK || len(login.Cookies()) != 1 {
				t.Fatalf("native login: %d", login.StatusCode)
			}
			for _, state := range []int32{1, 2} {
				source.state.Store(state)
				for _, route := range []struct{ method, path, body string }{
					{http.MethodGet, "/auth/status", ""},
					{http.MethodPost, "/auth/login", loginBody},
					{http.MethodGet, "/auth/oidc/corp/start", ""},
					{http.MethodGet, "/auth/oidc/corp/callback?code=unused&state=unused", ""},
					{http.MethodGet, "/auth/me", ""},
					{http.MethodGet, "/auth/tokens", ""},
					{http.MethodGet, "/iam/users", ""},
					{http.MethodGet, "/zones", ""},
				} {
					req, err := http.NewRequest(route.method, base+route.path, strings.NewReader(route.body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					res, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					var body api.Error
					err = json.NewDecoder(res.Body).Decode(&body)
					_ = res.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if res.StatusCode != http.StatusServiceUnavailable || body.Body.Key != "iam_migration_required" || strings.Contains(body.Body.Message, "private") {
						t.Fatalf("state %d %s %s: %d %+v", state, route.method, route.path, res.StatusCode, body.Body)
					}
					if len(res.Cookies()) != 0 || res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Retry-After") == "" {
						t.Fatal("closed admission issued a cookie or omitted retry/cache controls")
					}
				}
			}
			// The earlier cookie is admitted again only after a fresh native
			// verification; failures above cannot reuse its previous success.
			source.state.Store(0)
			res, err := client.Get(base + "/auth/me")
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("reopened admission: %d", res.StatusCode)
			}
		})
	}
}
