package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type legacyStackRoutes func(*http.ServeMux)

func (f legacyStackRoutes) Register(mux *http.ServeMux) { f(mux) }

// A forwarded route without a legacy handler answers the envelope, never the
// mux's plain-text 404, and the fallback does not shadow registered routes.
func TestLegacyAuthHandlerFallbackAndRefusals(t *testing.T) {
	h := legacyAuthHandler(legacyStackRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/auth/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}))
	for _, tc := range []struct {
		method, path string
		status       int
		key          string
	}{
		{http.MethodGet, "/api/v1/auth/status", http.StatusNoContent, ""},
		{http.MethodGet, "/api/v1/auth/cli/providers", http.StatusServiceUnavailable, "cli_login_unavailable"},
		{http.MethodPost, "/api/v1/auth/cli/corp/prepare", http.StatusServiceUnavailable, "cli_login_unavailable"},
		{http.MethodPost, "/api/v1/auth/cli/corp/verify", http.StatusServiceUnavailable, "cli_login_unavailable"},
		{http.MethodGet, "/api/v1/auth/oidc/corp/logout", http.StatusServiceUnavailable, "provider_logout_unavailable"},
		{http.MethodGet, "/api/v1/auth/oidc/corp/logout/callback", http.StatusServiceUnavailable, "provider_logout_unavailable"},
		{http.MethodGet, "/api/v1/auth/totp", http.StatusServiceUnavailable, "factor_management_unavailable"},
		{http.MethodPost, "/api/v1/iam/users", http.StatusServiceUnavailable, "user_management_unavailable"},
		{http.MethodGet, "/api/v1/auth/future-route", http.StatusServiceUnavailable, "unavailable"},
		{http.MethodPost, "/api/v1/auth/status", http.StatusServiceUnavailable, "unavailable"},
		{http.MethodDelete, "/api/v1/iam/future/route", http.StatusServiceUnavailable, "unavailable"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))
		if w.Code != tc.status {
			t.Fatalf("%s %s: status %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
		if tc.key == "" {
			continue
		}
		var envelope struct {
			Error struct {
				Key, Message, Hint string
			} `json:"error"`
		}
		if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || json.Unmarshal(w.Body.Bytes(), &envelope) != nil ||
			envelope.Error.Key != tc.key || envelope.Error.Message == "" {
			t.Fatalf("%s %s: not the keyed envelope: %q %s", tc.method, tc.path, w.Header().Get("Content-Type"), w.Body.String())
		}
		if tc.key == "cli_login_unavailable" || tc.key == "provider_logout_unavailable" || tc.key == "unavailable" {
			if envelope.Error.Hint == "" {
				t.Fatalf("%s %s: refusal has no hint", tc.method, tc.path)
			}
		}
	}
}
