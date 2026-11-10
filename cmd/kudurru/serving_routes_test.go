package main

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// registeredAPIRoutes lists "METHOD /path/{param}" of every route the public
// API registers, relative to the API base path.
func registeredAPIRoutes(t *testing.T) []string {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	server, err := api.New(api.Options{Config: cfg, Store: st, Validator: v, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	var routes []string
	for _, route := range server.Routes() {
		routes = append(routes, route.Method+" "+strings.TrimPrefix(route.Path, api.BasePath))
	}
	slices.Sort(routes)
	return routes
}

// The complete SQLite runtime used to refuse, with 503
// current_command_unavailable, every unsafe request under /zones,
// /unblockrequests and BlockList source entries that was not a reviewed
// complete current command. Every registered route of those families is such a
// command now, so the runtime gate is gone and an unregistered path gets the
// router's 404. This table keeps the review: an unsafe route added to one of
// the families fails here until its command is a complete current command.
func TestCompleteCommandRouteFamiliesAreReviewed(t *testing.T) {
	reviewed := []string{
		"DELETE /unblockrequests/{namespace}/{name}",
		"DELETE /zones/{zone}",
		"DELETE /zones/{zone}/records/{name}/{type}",
		"PATCH /blocklists/{namespace}/{name}/sources/{source}/entries",
		"POST /unblockrequests/{namespace}/{name}/approve",
		"POST /unblockrequests/{namespace}/{name}/deny",
		"POST /zones/{zone}/import",
		"POST /zones/{zone}/notify",
		"POST /zones/{zone}/records",
	}
	var registered []string
	for _, route := range registeredAPIRoutes(t) {
		method, path, _ := strings.Cut(route, " ")
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		sourceEntries := parts[0] == "blocklists" && len(parts) == 6 && parts[3] == "sources" && parts[5] == "entries"
		if parts[0] == "zones" || parts[0] == "unblockrequests" || sourceEntries {
			registered = append(registered, route)
		}
	}
	slices.Sort(reviewed)
	if !slices.Equal(registered, reviewed) {
		t.Fatalf("unsafe routes of the complete-command families changed; review the new command before listing it\nregistered: %v\nreviewed:   %v", registered, reviewed)
	}
}
