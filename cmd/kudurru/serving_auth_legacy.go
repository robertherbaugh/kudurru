package main

import (
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// legacyAuthHandler mounts the legacy authentication stack for a runtime that
// has no complete SQLite authentication handler (the Kubernetes driver). The
// API forwards its /auth and /iam routes here after its own admission, so each
// of them needs an answer in the error envelope: the stack's routes, keyed
// refusals for capabilities that need the complete runtime, and a last
// fallback instead of the mux's plain-text 404.
func legacyAuthHandler(stack interface{ Register(*http.ServeMux) }) http.Handler {
	mux := http.NewServeMux()
	stack.Register(mux)
	registerUnavailableTOTP(mux)
	registerUnavailableLocalUsers(mux)
	registerUnavailableCLILogin(mux)
	registerUnavailableProviderLogout(mux)
	registerLegacyAuthFallback(mux)
	return mux
}

// registerUnavailableCLILogin refuses public-client CLI login. Provider
// advertisement, source receipts and proof verification use the captured
// provider resources that only the complete SQLite runtime owns. An empty
// provider list would instead tell the operator to configure a CLI client that
// this server could never use.
func registerUnavailableCLILogin(mux *http.ServeMux) {
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/auth/cli/providers"},
		{http.MethodPost, "/auth/cli/{idp}/prepare"},
		{http.MethodPost, "/auth/cli/{idp}/verify"},
	} {
		mux.HandleFunc(route.method+" "+api.BasePath+route.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Referrer-Policy", "no-referrer")
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "cli_login_unavailable",
				Message: "public CLI login requires the complete SQLite authentication runtime",
				Hint:    "log in with an API token (kudurru login --token) or a local account (kudurru login --username)"})
		})
	}
}

// registerUnavailableProviderLogout refuses the browser continuation of a
// provider logout. The legacy POST /auth/logout/browser ends the local session
// and never returns a continuation path, so no flow of this server leads here.
func registerUnavailableProviderLogout(mux *http.ServeMux) {
	for _, path := range []string{"/auth/oidc/{idp}/logout", "/auth/oidc/{idp}/logout/callback"} {
		mux.HandleFunc(http.MethodGet+" "+api.BasePath+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Referrer-Policy", "no-referrer")
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "provider_logout_unavailable",
				Message: "provider logout continuation requires the complete SQLite authentication runtime",
				Hint:    "the local session ends with POST /api/v1/auth/logout/browser; end the provider session at the identity provider"})
		})
	}
}

// registerLegacyAuthFallback answers a forwarded route that has no legacy
// handler. Only routes the API registered reach this mux, so the request names
// a real operation that this runtime does not serve.
func registerLegacyAuthFallback(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: api.KeyUnavailable,
			Message: "this operation is unavailable on this server", Hint: "requires the complete SQLite runtime"})
	})
}
