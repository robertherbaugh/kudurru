package main

import (
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// registerUnavailableLocalUsers keeps unsupported legacy writes explicit. The
// API still requires users.write before forwarding to these refusals.
func registerUnavailableLocalUsers(mux *http.ServeMux) {
	for _, path := range []string{"/iam/users", "/iam/users/{name}/password"} {
		mux.HandleFunc(http.MethodPost+" "+api.BasePath+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "user_management_unavailable", Message: "local user commands require the complete current-command runtime"})
		})
	}
}
