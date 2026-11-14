package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// The follower bounds exactly the registered routes whose effect it forwards
// as a typed member command. Which routes forward is observed, not derived
// from the predicate: every registered unsafe route is requested on an API
// server whose ObjectForward and ZoneForward hooks record that they were
// reached, with the documents, name lists and zone inputs that forwarded
// routes take. A route that reaches a hook without satisfying
// memberForwardRoute fails here, because a follower would refuse it with "this
// route is not forwarded from a follower"; so does a route that satisfies the
// predicate without reaching a hook, and any change of the reviewed table.
// The probes cover the request shapes below. A new route that needs a body of
// another shape before it writes a canonical object reaches no hook here and
// still needs a review.
func TestMemberForwardRouteCoversEveryForwardedRoute(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	// The hooks stand where an enrolled follower mounts its forwards. Each
	// answers as a handled forward, so no probe writes anything.
	var reached atomic.Int32
	refusal := objectForwardUnavailable("route probe")
	server, err := api.New(api.Options{Config: cfg, Store: st, Validator: v, Logger: slog.New(slog.DiscardHandler),
		Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.StandardBindings()...),
		ObjectCommand: func(context.Context, store.Key, func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error) error {
			return refusal
		},
		ZoneCommand: func(context.Context, api.ZoneCommandTarget, api.ZoneCommandPrepare) error { return refusal },
		ObjectForward: func(context.Context, api.ForwardedObject) (api.ForwardedObjectResult, bool, error) {
			reached.Add(1)
			return api.ForwardedObjectResult{}, true, refusal
		},
		ZoneForward: func(context.Context, api.ForwardedZone) (json.RawMessage, bool, error) {
			reached.Add(1)
			return nil, true, refusal
		}})
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
		"POST /integrations:batchDelete",
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
	meta := func(namespaced bool) map[string]any {
		if namespaced {
			return map[string]any{"name": "probe", "namespace": "default"}
		}
		return map[string]any{"name": "probe"}
	}
	// One valid document for each kind that has write routes of its own.
	documents := map[string]map[string]any{
		"dnszones": {"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": meta(false),
			"spec": map[string]any{"zoneName": "probe.test.", "type": "primary", "nameservers": []any{"ns1.probe.test."},
				"soa": map[string]any{"mname": "ns1.probe.test.", "rname": "hostmaster.probe.test."}}},
		"dnsrecordsets": {"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": meta(true),
			"spec": map[string]any{"zoneRef": map[string]any{"name": "probe"}, "endpoints": []any{
				map[string]any{"dnsName": "www.probe.test.", "recordType": "A", "targets": []any{"192.0.2.60"}}}}},
		"blocklists": {"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList", "metadata": meta(true),
			"spec": map[string]any{"mode": "block", "sources": []any{map[string]any{"name": "inline", "inline": []any{"blocked.probe.test"}}}}},
		"clientgroups": {"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": meta(true),
			"spec": map[string]any{"clients": []any{map[string]any{"ip": "192.0.2.70"}}}},
		"policies": {"apiVersion": "kudurru.io/v1alpha1", "kind": "Policy", "metadata": meta(true), "spec": map[string]any{}},
	}
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	type body struct {
		contentType string
		raw         []byte
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	var forwarded, bounded []string
	for _, route := range server.Routes() {
		if route.Method != http.MethodPost && route.Method != http.MethodPut && route.Method != http.MethodDelete && route.Method != http.MethodPatch {
			continue
		}
		name := route.Method + " " + strings.TrimPrefix(route.Path, api.BasePath)
		observed, matched := false, false
		for plural, document := range documents {
			path := strings.NewReplacer("{kind}", plural, "{namespace}", "default", "{type}", "A").Replace(route.Path)
			path = parameter.ReplaceAllString(path, "probe")
			matched = memberForwardRoute(route.Method, api.BasePath, path)
			bodies := []body{{}}
			if route.Method != http.MethodDelete {
				bodies = []body{
					{"application/json", encode(document)},
					{"application/json", encode([]any{document})},
					{"application/json", encode(map[string]any{"names": []string{"probe"}})},
					{"application/json", encode(map[string]any{"dnsName": "www.probe.test.", "recordType": "A", "targets": []any{"192.0.2.61"}})},
					{"text/dns", []byte("www 300 IN A 192.0.2.62\n")},
				}
			}
			for _, b := range bodies {
				before := reached.Load()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				request := httptest.NewRequest(route.Method, path, bytes.NewReader(b.raw)).WithContext(ctx)
				if b.contentType != "" {
					request.Header.Set("Content-Type", b.contentType)
				}
				request.Header.Set("Authorization", apitest.Bearer("admin"))
				server.ServeHTTP(httptest.NewRecorder(), request)
				cancel()
				observed = observed || reached.Load() != before
			}
		}
		if observed {
			forwarded = append(forwarded, name)
		}
		if matched {
			bounded = append(bounded, name)
		}
		switch {
		case observed && !matched:
			t.Errorf("%s reaches the follower forward but memberForwardRoute does not bound it: a follower refuses it as not forwarded", name)
		case matched && !observed:
			t.Errorf("%s is bounded by memberForwardRoute but no probe reached a forward: correct the predicate or give this test the route's request", name)
		}
	}
	slices.Sort(forwarded)
	slices.Sort(bounded)
	slices.Sort(reviewed)
	if !slices.Equal(forwarded, reviewed) {
		t.Fatalf("forwarded route table changed\nforwarded: %v\nreviewed:  %v", forwarded, reviewed)
	}
	if !slices.Equal(bounded, reviewed) {
		t.Fatalf("bounded route table changed\nbounded:  %v\nreviewed: %v", bounded, reviewed)
	}
	// The API's follower table (D368) calls an operation forwarded exactly
	// when it reaches one of these forwards, or has a forward of its own: the
	// blocking switch, local login, the settings patch, the three session
	// routes, the password change and the two token routes
	// (memberOwnForwardRoute, tested below). The one observed route that the table
	// refuses instead is the DHCPScope batch deletion, whose kind no typed
	// command carries.
	var classified []string
	for path, item := range server.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete} {
			if op != nil && api.OperationFollowerClass(op.OperationID) == api.FollowerForwarded {
				classified = append(classified, method+" "+path)
			}
		}
	}
	want := append(slices.DeleteFunc(slices.Clone(reviewed), func(route string) bool { return route == "POST /dhcpscopes:batchDelete" }), "POST /dns/blocking", "POST /auth/login",
		"PATCH /settings", "POST /auth/logout", "POST /auth/logout/browser", "DELETE /auth/sessions/{id}",
		"POST /auth/password", "POST /auth/tokens", "DELETE /auth/tokens/{id}")
	slices.Sort(classified)
	slices.Sort(want)
	if !slices.Equal(classified, want) {
		t.Fatalf("the follower table's forwarded operations differ from the routes that forward\nclassified: %v\nforwarding: %v", classified, want)
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
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPatch, "/api/v1/settings", true},
		{http.MethodPost, "/api/v1/auth/logout", true},
		{http.MethodPost, "/api/v1/auth/logout/browser", true},
		{http.MethodDelete, "/api/v1/auth/sessions/abc", true},
		{http.MethodDelete, "/api/v1/auth/sessions/a%2Fb", true},
		{http.MethodPatch, "/api/v1/node/config", false},
		{http.MethodPut, "/api/v1/settings", false},
		{http.MethodPatch, "/api/v1/settings/extra", false},
		{http.MethodPost, "/api/v1/auth/logout/other", false},
		{http.MethodPost, "/api/v1/auth/login", false},
		{http.MethodGet, "/api/v1/auth/sessions/abc", false},
		{http.MethodDelete, "/api/v1/auth/sessions", false},
		{http.MethodDelete, "/api/v1/auth/sessions/abc/extra", false},
		{http.MethodPost, "/api/v1/auth/password", true},
		{http.MethodPost, "/api/v1/auth/tokens", true},
		{http.MethodDelete, "/api/v1/auth/tokens/abc", true},
		{http.MethodPut, "/api/v1/auth/password", false},
		{http.MethodPost, "/api/v1/auth/password/extra", false},
		{http.MethodDelete, "/api/v1/auth/tokens", false},
		{http.MethodPost, "/api/v1/auth/tokens/abc", false},
		{http.MethodPost, "/api/v1/auth/totp/setup", false},
	} {
		if got := memberOwnForwardRoute(tc.method, api.BasePath, tc.path); got != tc.want {
			t.Errorf("memberOwnForwardRoute(%s %s) = %t, want %t", tc.method, tc.path, got, tc.want)
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
