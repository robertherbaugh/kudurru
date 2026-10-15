package main

import (
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// registerUnavailableTOTP documents the unsupported legacy command capability
// at the same public routes. API authentication and authorization still apply.
func registerUnavailableTOTP(mux *http.ServeMux) {
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/auth/totp"},
		{http.MethodPost, "/auth/totp/setup"},
		{http.MethodPost, "/auth/totp/confirm"},
		{http.MethodPost, "/auth/totp/disable"},
		{http.MethodPost, "/iam/users/{name}/totp/recover"},
	} {
		mux.HandleFunc(route.method+" "+api.BasePath+route.path, func(w http.ResponseWriter, r *http.Request) {
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "factor_management_unavailable", Message: "factor management requires the complete current-command runtime"})
		})
	}
}
