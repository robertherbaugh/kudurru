package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
)

// A follower starts the 30-second forwarding lifetime before the body is read
// for every forwarded route. Naming those routes did not take the bound away
// from the other unsafe requests under /objects/ and /zones/ that had it
// before: POST /zones/{zone}/notify is the registered one. Every other request
// on a follower, and every request on the primary, keeps the ordinary route
// lifetime. The lifetime is observed where the API handler receives the
// request, behind the production middleware chain of each node.
func TestRootMemberFollowerRequestLifetimeBound(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	observe := func(runtime *servingRuntime) func(method, path string) time.Duration {
		h, err := newServingHTTP(servingHTTPOptions{Runtime: runtime, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
		if err != nil {
			t.Fatal(err)
		}
		return func(method, path string) time.Duration {
			t.Helper()
			reached, remaining := false, time.Duration(0)
			handler := h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				reached = true
				if deadline, finite := r.Context().Deadline(); finite {
					remaining = time.Until(deadline)
				}
			}))
			request := httptest.NewRequest(method, api.BasePath+path, nil)
			request.Header.Set("Authorization", "Bearer "+f.primary.token)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if !reached {
				t.Fatalf("%s %s did not reach the API handler: status=%d body=%s", method, path, recorder.Code, recorder.Body)
			}
			return remaining
		}
	}
	follower, primary := observe(f.follower), observe(f.primary.s)
	for _, tc := range []struct {
		method, path string
		bounded      bool
	}{
		// Forwarded routes.
		{http.MethodPost, "/apply", true},
		{http.MethodPost, "/objects/dnszones", true},
		{http.MethodPut, "/objects/dnsrecordsets/default/www", true},
		{http.MethodDelete, "/objects/dnszones/example", true},
		{http.MethodPost, "/policies", true},
		{http.MethodPut, "/clientgroups/default/lan", true},
		{http.MethodDelete, "/blocklists/default/ads", true},
		{http.MethodPost, "/dnsrecordsets:batchDelete", true},
		{http.MethodPost, "/zones/example.test/records", true},
		{http.MethodPost, "/zones/example.test/import", true},
		{http.MethodDelete, "/zones/example.test/records/www/A", true},
		{http.MethodDelete, "/zones/example.test", true},
		// Forwarded with commands of their own.
		{http.MethodPatch, "/settings", true},
		{http.MethodPost, "/auth/logout", true},
		{http.MethodPost, "/auth/logout/browser", true},
		{http.MethodDelete, "/auth/sessions/some-session", true},
		// Not forwarded, but bounded before the forwarded routes were named.
		{http.MethodPost, "/zones/example.test/notify", true},
		{http.MethodPost, "/zones/example.test/records/www", true},
		{http.MethodPut, "/zones/example.test", true},
		{http.MethodPost, "/objects/dnszones/example/extra/segments/here", true},
		{http.MethodDelete, "/objects/dnszones", true},
		// Never bounded: reads, other methods and routes with their own command.
		{http.MethodGet, "/objects/dnszones/example", false},
		{http.MethodGet, "/zones/example.test/records", false},
		{http.MethodPatch, "/objects/dnszones/example", false},
		{http.MethodPost, "/zones", false},
		{http.MethodPut, "/apply", false},
		{http.MethodPost, "/blocklists/default/ads/refresh", false},
		{http.MethodPatch, "/blocklists/default/ads/sources/0/entries", false},
		{http.MethodPost, "/domains/allow/exact", false},
		{http.MethodPost, "/settings/sections/upstreams/yaml", false},
		{http.MethodPatch, "/node/config", false},
		{http.MethodPost, "/auth/password", false},
	} {
		got := follower(tc.method, tc.path)
		if bounded := got > 0 && got <= memberForwardLifetime; bounded != tc.bounded {
			t.Errorf("follower %s %s: remaining lifetime %s, want the %s forwarding bound: %t", tc.method, tc.path, got, memberForwardLifetime, tc.bounded)
		}
		if got := primary(tc.method, tc.path); got <= memberForwardLifetime {
			t.Errorf("primary %s %s: remaining lifetime %s, want the ordinary route lifetime", tc.method, tc.path, got)
		}
	}
}
