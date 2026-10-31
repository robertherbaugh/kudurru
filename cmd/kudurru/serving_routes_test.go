package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServingMutationRoutesDomainAliasesAndSourceCommand(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		blocked      bool
	}{
		{http.MethodPost, "/api/v1/queries/42/actions", false},
		{http.MethodPost, "/api/v1/imports/apply", false},
		{http.MethodPost, "/api/v1/imports/start", false},
		{http.MethodPost, "/api/v1/imports/adopt", true},
		{http.MethodPost, "/api/v1/imports/runs/run/decommission", false},
		{http.MethodPost, "/api/v1/imports/preview", false},
		{http.MethodPost, "/api/v1/imports/runs/run/decommission/preview", false},
		{http.MethodGet, "/api/v1/imports/runs/run", false},
		{http.MethodPost, "/api/v1/domains/allow/exact", false},
		{http.MethodPost, "/api/v1/allowlist", false},
		{http.MethodPost, "/api/v1/denylist:batch", false},
		{http.MethodPatch, "/api/v1/blocklists/default/list/sources/0/entries", false},
		{http.MethodGet, "/api/v1/blocklists/default/list/sources/0/entries", false},
		{http.MethodPost, "/api/v1/blocklists/default/list/sources/0/entries", true},
		{http.MethodGet, "/api/v1/queries/42", false},
		{http.MethodGet, "/api/v1/queries", false},
		{http.MethodDelete, "/api/v1/queries", false},
		{http.MethodOptions, "/api/v1/queries/42/actions", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			called := false
			h := servingMutationRoutes(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			want := http.StatusNoContent
			if tc.blocked {
				want = http.StatusServiceUnavailable
			}
			if w.Code != want || called == tc.blocked {
				t.Fatal("wrong complete command gate", w.Code, called)
			}
		})
	}
}
