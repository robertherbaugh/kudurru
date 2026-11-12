package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// enrolledPrimaryRequired sends one mutation to a follower process and checks
// the one typed refusal of D368: 503 primary_required, the primary's name, a
// hint and no Retry-After.
func enrolledPrimaryRequired(t *testing.T, follower *staticProcessClient, primary, method, path string, body any) enrolledAnswer {
	t.Helper()
	answer := enrolledSend(t, follower, method, path, body)
	if answer.status != http.StatusServiceUnavailable || answer.Error.Key != api.KeyPrimaryRequired || !strings.Contains(answer.Error.Message, "follower of "+primary) ||
		answer.hint() == "" || answer.retryAfter != "" {
		t.Fatalf("%s %s through the follower: want 503 primary_required naming %s with a hint and no Retry-After, got status=%d key=%q retry-after=%q message=%q hint=%q",
			method, path, primary, answer.status, answer.Error.Key, answer.retryAfter, answer.Error.Message, answer.hint())
	}
	return answer
}

// enrolledFollowerContract exercises the follower contract of D368 on two
// real processes: the follower names its role and primary, answers one
// representative mutation of every primary-only group with the typed refusal
// and no effect on either node, runs node-local commands itself, and the
// primary answers the same routes without a role refusal.
func enrolledFollowerContract(t *testing.T, primary, follower *staticProcessClient, primaryName, followerName string) {
	t.Helper()
	var onFollower, onPrimary api.ClusterInfo
	enrolledProcessRead(t, follower, "/cluster", func(body []byte) bool {
		onFollower = api.ClusterInfo{}
		return json.Unmarshal(body, &onFollower) == nil && onFollower.Role == "follower"
	})
	if onFollower.Primary != primaryName || onFollower.Leader != primaryName || onFollower.Node != followerName || !onFollower.PrimaryRequired || onFollower.ReadOnly ||
		!slices.Contains(onFollower.ForwardedOperations, "apply") || !slices.Contains(onFollower.ForwardedOperations, "login") ||
		!slices.Contains(onFollower.LocalOperations, "flushCache") || !slices.Contains(onFollower.ForwardedOperations, "patchSettings") || !slices.Contains(onFollower.ForwardedOperations, "logout") ||
		!slices.Contains(onFollower.ForwardedOperations, "changePassword") || !slices.Contains(onFollower.ForwardedOperations, "createToken") || !slices.Contains(onFollower.ForwardedOperations, "deleteToken") ||
		slices.Contains(onFollower.ForwardedOperations, "patchNodeConfig") {
		t.Fatalf("follower process does not report its role, primary and capability: %+v", onFollower)
	}
	enrolledProcessRead(t, primary, "/cluster", func(body []byte) bool {
		onPrimary = api.ClusterInfo{}
		return json.Unmarshal(body, &onPrimary) == nil && onPrimary.Role == "primary"
	})
	if onPrimary.Primary != primaryName || onPrimary.Leader != primaryName || onPrimary.PrimaryRequired || len(onPrimary.ForwardedOperations) != 0 || len(onPrimary.LocalOperations) != 0 {
		t.Fatalf("primary process reports a follower capability: %+v", onPrimary)
	}
	var status api.AuthStatus
	enrolledProcessRead(t, follower, "/auth/status", func(body []byte) bool { return json.Unmarshal(body, &status) == nil })
	if status.Role != "follower" || status.Primary != primaryName || !status.LocalLogin || len(status.Providers) != 0 {
		t.Fatalf("follower login options do not name the primary: %+v", status)
	}

	// One representative of every primary-only group. The follower refuses
	// each before it reads the body, so the bodies only need to be JSON.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/allowlist", map[string]any{"domain": "refused-allow.e2e.test"}},
		{http.MethodPost, "/domains/allow/exact", map[string]any{"domains": []string{"refused-domain.e2e.test"}}},
		{http.MethodPost, "/blocklists/default/refused-list/refresh", nil},
		{http.MethodPost, "/zones/e2e-test/notify", nil},
		{http.MethodPost, "/iam/users", map[string]any{"name": "refused-user", "password": "refused-user-password-1"}},
		{http.MethodPost, "/iam/rolebindings", map[string]any{}},
		{http.MethodPost, "/auth/totp/setup", map[string]any{}},
		{http.MethodPost, "/tsigkeys", map[string]any{"name": "refused-key."}},
		{http.MethodPost, "/integrations/default/refused/sync", nil},
		{http.MethodPost, "/imports/preview", map[string]any{}},
		{http.MethodPost, "/unblockrequests/default/refused/approve", map[string]any{}},
		{http.MethodPost, "/dhcpscopes:batchDelete", map[string]any{"names": []string{"refused"}}},
		{http.MethodPost, "/auth/cli/refused/prepare", map[string]any{"mode": "browser"}},
	} {
		enrolledPrimaryRequired(t, follower, primaryName, tc.method, tc.path, tc.body)
	}
	if hint := enrolledPrimaryRequired(t, follower, primaryName, http.MethodPatch, "/node/config", map[string]any{"log": map[string]any{"level": "debug"}}).hint(); !strings.Contains(hint, "configuration file or environment") {
		t.Fatal("node configuration refusal does not say what to change on the follower:", hint)
	}
	// The browser OIDC flow is a GET and is refused by its own admission.
	browser := enrolledSend(t, follower, http.MethodGet, "/auth/oidc/refused/start", nil)
	if browser.status != http.StatusServiceUnavailable || browser.Error.Key != api.KeyPrimaryRequired || browser.retryAfter != "" || !strings.Contains(browser.hint(), "OIDC login runs on the primary") {
		t.Fatalf("OIDC start on the follower: status=%d key=%q retry-after=%q hint=%q", browser.status, browser.Error.Key, browser.retryAfter, browser.hint())
	}

	// No refused request had an effect on the primary, and so none can reach
	// the follower later.
	absent := func(path, needle string) {
		t.Helper()
		answer := enrolledSend(t, primary, http.MethodGet, path, nil)
		if answer.status != http.StatusOK || bytes.Contains(answer.raw, []byte(needle)) {
			t.Fatalf("a refused follower mutation reached the primary: GET %s status=%d contains %q", path, answer.status, needle)
		}
	}
	absent("/allowlist", "refused-allow.e2e.test")
	absent("/iam/users", "refused-user")

	// Node-local: the follower flushes its own response cache.
	var flushed api.FlushResult
	follower.request(t, http.MethodPost, "/cache/flush", nil, http.StatusOK, &flushed)
	if flushed.Node != followerName {
		t.Fatal("follower cache flush was answered by another node:", flushed.Node)
	}
	primary.request(t, http.MethodPost, "/cache/flush", nil, http.StatusOK, &flushed)
	if flushed.Node != primaryName {
		t.Fatal("primary cache flush was answered by another node:", flushed.Node)
	}
	// The primary answers the same route on its merits, not for its role.
	if answer := enrolledSend(t, primary, http.MethodPatch, "/settings", map[string]any{}); answer.status != http.StatusBadRequest || answer.Error.Key != api.KeyInvalid {
		t.Fatalf("empty PATCH /settings on the primary: status=%d key=%q", answer.status, answer.Error.Key)
	}
}
