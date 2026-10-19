package main

import (
	"net/http"
	"path"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// servingMutationRoutes keeps compound convenience routes unavailable until
// their complete current commands are mounted. Legacy helpers can read live
// state, return an unchecked no-op, or write outside ObjectCommand.
// Generic single-object commands remain available through their admitted path.
func servingMutationRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		// nosemgrep: go.lang.security.filepath-clean-misuse.filepath-clean-misuse -- canonicalizes an HTTP route for admission; this path never accesses the filesystem
		if unsafe && servingUnconvertedMutation(r.Method, path.Clean(r.URL.Path)) {
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "current_command_unavailable", Message: "this operation requires a current compound command"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func servingUnconvertedMutation(method, requestPath string) bool {
	relative, ok := strings.CutPrefix(requestPath, api.BasePath+"/")
	if !ok {
		return false
	}
	parts := strings.Split(relative, "/")
	switch parts[0] {
	case "zones":
		// These registered routes use complete current commands, including
		// the durable intent adapter for manual NOTIFY.
		switch {
		case method == http.MethodDelete && len(parts) == 2:
			return false
		case method == http.MethodPost && len(parts) == 3 && (parts[2] == "records" || parts[2] == "import" || parts[2] == "notify"):
			return false
		case method == http.MethodDelete && len(parts) == 5 && parts[2] == "records":
			return false
		default:
			return true
		}
	case "domains", "allowlist", "denylist", "allowlist:batch", "denylist:batch", "unblockrequests":
		return true
	case "imports":
		// Migration journals and detached jobs need current command admission.
		// Read-only previews and history remain available.
		return method == http.MethodPost && ((len(parts) == 2 && (parts[1] == "apply" || parts[1] == "start" || parts[1] == "adopt")) ||
			(len(parts) == 4 && parts[1] == "runs" && parts[3] == "decommission"))
	case "queries":
		// Query actions delegate to the same legacy domain-list mutator.
		return method == http.MethodPost && len(parts) == 3 && parts[2] == "actions"
	case "blocklists":
		return method != http.MethodPatch && len(parts) == 6 && parts[3] == "sources" && parts[5] == "entries"
	default:
		return false
	}
}
