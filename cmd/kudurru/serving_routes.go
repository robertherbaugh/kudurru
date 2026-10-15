package main

import (
	"net/http"
	"path"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// servingMutationRoutes keeps compound zone routes unavailable until their
// complete current command is mounted. Their legacy helpers can read live
// state, return an unchecked no-op, or call DeleteAll outside ObjectCommand.
// Generic single-object commands remain available through their admitted path.
func servingMutationRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if unsafe && strings.HasPrefix(path.Clean(r.URL.Path), api.BasePath+"/zones/") {
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "current_zone_command_unavailable", Message: "this zone operation requires a current compound command"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
