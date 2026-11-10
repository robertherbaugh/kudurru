package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// The follower bounds exactly the registered routes whose effect it forwards
// as a typed member command. A route added to the public API that matches the
// predicate, or a forwarded route that it misses, fails here until reviewed.
func TestMemberForwardRouteCoversEveryForwardedRoute(t *testing.T) {
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
	reviewed := []string{
		"DELETE /blocklists/{namespace}/{name}",
		"DELETE /clientgroups/{namespace}/{name}",
		"DELETE /objects/{kind}/{name}",
		"DELETE /objects/{kind}/{namespace}/{name}",
		"DELETE /policies/{namespace}/{name}",
		"DELETE /zones/{zone}",
		"DELETE /zones/{zone}/records/{name}/{type}",
		"POST /apply",
		"POST /blocklists",
		"POST /blocklists:batchDelete",
		"POST /clientgroups",
		"POST /clientgroups:batchDelete",
		"POST /dhcpscopes:batchDelete",
		"POST /dnsrecordsets:batchDelete",
		"POST /dnszones:batchDelete",
		"POST /objects/{kind}",
		"POST /policies",
		"POST /policies:batchDelete",
		"POST /zones/{zone}/import",
		"POST /zones/{zone}/records",
		"PUT /blocklists/{namespace}/{name}",
		"PUT /clientgroups/{namespace}/{name}",
		"PUT /objects/{kind}/{name}",
		"PUT /objects/{kind}/{namespace}/{name}",
		"PUT /policies/{namespace}/{name}",
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	var bounded []string
	for _, route := range server.Routes() {
		if memberForwardRoute(route.Method, api.BasePath, parameter.ReplaceAllString(route.Path, "example")) {
			bounded = append(bounded, route.Method+" "+strings.TrimPrefix(route.Path, api.BasePath))
		}
	}
	slices.Sort(bounded)
	slices.Sort(reviewed)
	if !slices.Equal(bounded, reviewed) {
		t.Fatalf("forwarded route table changed\nbounded:  %v\nreviewed: %v", bounded, reviewed)
	}
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		// The router keeps an encoded separator inside its path parameter.
		{http.MethodDelete, "/api/v1/zones/sub%2Fzone.test", true},
		{http.MethodDelete, "/api/v1/zones/example.test/records/a%2Fb/TXT", true},
		{http.MethodPut, "/api/v1/objects/dnsrecordsets/default/www", true},
		{http.MethodPost, "/api/v1/dnsrecordsets%3AbatchDelete", true},
		{http.MethodPost, "/api/v%31/apply", true},
		// Reads, other methods, other shapes and routes with their own command.
		{http.MethodGet, "/api/v1/objects/dnszones/example", false},
		{http.MethodPatch, "/api/v1/objects/dnszones/example", false},
		{http.MethodPost, "/api/v1/apply/extra", false},
		{http.MethodPost, "/api/v1/apply/", false},
		{http.MethodPut, "/api/v1/apply", false},
		{http.MethodPost, "/api/v1/zones/example.test/notify", false},
		{http.MethodPost, "/api/v1/zones", false},
		{http.MethodPost, "/api/v1/blocklists/default/ads/refresh", false},
		{http.MethodPatch, "/api/v1/blocklists/default/ads/sources/0/entries", false},
		{http.MethodPost, "/api/v1/domains/allow/exact", false},
		{http.MethodPost, "/api/v1/allowlist:batch", false},
		{http.MethodPost, "/api/v1/dns/blocking", false},
		{http.MethodPost, "/api/v1/settings/sections/upstreams/yaml", false},
		{http.MethodPost, "/other/v1/apply", false},
		{http.MethodPost, "/api/v1", false},
		{http.MethodPost, "/api/v1/objects/%zz", false},
	} {
		if got := memberForwardRoute(tc.method, api.BasePath, tc.path); got != tc.want {
			t.Errorf("memberForwardRoute(%s %s) = %t, want %t", tc.method, tc.path, got, tc.want)
		}
	}
}

// Capture sends nothing. Its refusal says why the follower did not forward and
// only a changed source asks for a review of current state.
func TestMemberCaptureErrorNamesItsCause(t *testing.T) {
	bounded, cancelBounded := context.WithTimeout(t.Context(), memberForwardLifetime)
	defer cancelBounded()
	long, cancelLong := context.WithTimeout(t.Context(), memberForwardLifetime+time.Minute)
	defer cancelLong()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	canceled, cancel := context.WithTimeout(t.Context(), memberForwardLifetime)
	cancel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		err  error
		want string
	}{
		"source changed":    {bounded, replication.ErrCAS, "the follower's source changed"},
		"no lifetime":       {t.Context(), replication.ErrInvalid, "this route is not forwarded from a follower"},
		"lifetime too long": {long, replication.ErrInvalid, "this route is not forwarded from a follower"},
		"lifetime ended":    {expired, replication.ErrInvalid, "30-second forwarding lifetime ended"},
		"deadline error":    {expired, context.DeadlineExceeded, "30-second forwarding lifetime ended"},
		"canceled":          {canceled, context.Canceled, "the request was canceled"},
		"other refusal":     {bounded, replication.ErrInvalid, "this follower cannot forward writes now"},
	} {
		var refusal *api.Error
		err := memberCaptureError(tc.ctx, tc.err)
		if !errors.As(err, &refusal) || refusal.Status != http.StatusServiceUnavailable || refusal.Body.Key != api.KeyUnavailable || !strings.Contains(refusal.Body.Message, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if name != "source changed" && !strings.Contains(refusal.Body.Message, "not applied") {
			t.Errorf("%s: refusal does not say that nothing was applied: %s", name, refusal.Body.Message)
		}
	}
}
