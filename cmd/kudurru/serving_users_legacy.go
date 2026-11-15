package main

import (
	"net/http"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// registerUnavailableLocalUsers keeps unsupported legacy writes explicit. The
// API still requires users.write before forwarding to these refusals.
func registerUnavailableLocalUsers(mux *http.ServeMux) {
	for _, route := range []string{http.MethodPost + " /iam/users", http.MethodPost + " /iam/users/{name}/password", http.MethodPatch + " /iam/users/{name}", http.MethodDelete + " /iam/users/{name}"} {
		method, path, _ := strings.Cut(route, " ")
		mux.HandleFunc(method+" "+api.BasePath+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "user_management_unavailable", NoRetryAfter: true, Message: "local user commands require the complete current-command runtime",
				Hint: "reset a password on the replica's admin socket (kudurru admin reset-password NAME) and give other people access through OIDC and role bindings"})
		})
	}
}
