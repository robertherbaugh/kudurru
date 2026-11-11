package e2e

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// enrolledForwardedSettings changes one cluster setting through the follower
// process (settings.patch, D368): the follower answers with the primary's
// committed result and both nodes serve the value afterward.
// Two changes are sent back to back, as the dashboard's Settings page saves one
// key per request: the follower answers the first only once it serves it, so
// the second is made against the follower's own change and commits too.
func enrolledForwardedSettings(t *testing.T, primary, follower *staticProcessClient, followerName, previous, ttl string) {
	t.Helper()
	for _, value := range []string{previous, ttl} {
		var body api.SettingsBody
		follower.request(t, http.MethodPatch, "/settings", map[string]any{"blocking": map[string]any{"blockTTL": value}}, http.StatusOK, &body)
		if body.Node != followerName || !slices.Equal(body.Changed, []string{"blocking.blockTTL"}) || body.Persisted == nil || !*body.Persisted {
			t.Fatalf("settings patch through the follower did not answer with the primary's committed result: %+v", body)
		}
	}
	has := func(raw []byte) bool { return strings.Contains(string(raw), `"`+ttl+`"`) }
	enrolledProcessRead(t, primary, "/settings", has)
	enrolledProcessRead(t, follower, "/settings", has)
	// The primary's own refusal is relayed exactly and writes nothing.
	if answer := enrolledSend(t, follower, http.MethodPatch, "/settings", map[string]any{"blocking": map[string]any{"blockTTL": "not-a-duration"}}); answer.status != http.StatusBadRequest || answer.Error.Key != api.KeyInvalid {
		t.Fatalf("invalid settings patch through the follower: status=%d key=%q", answer.status, answer.Error.Key)
	}
}

// enrolledForwardedSignOut signs a session out through the follower process
// (auth.logout, D368) and proves that the session ended on both nodes: a
// saved copy of the cookie is refused by the primary at once and by the
// follower after it installs the primary's state.
func enrolledForwardedSignOut(t *testing.T, follower *staticProcessClient, jar *cookiejar.Jar, bases ...string) {
	t.Helper()
	target, err := url.Parse(follower.base)
	if err != nil {
		t.Fatal(err)
	}
	saved := jar.Cookies(target)
	if len(saved) == 0 {
		t.Fatal("the signed-in client holds no session cookie")
	}
	me := func(base string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/auth/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range saved {
			r.AddCookie(cookie)
		}
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(r)
		if err != nil {
			return 0
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, base := range bases {
		if status := me(base); status != http.StatusOK {
			t.Fatal("the session is not valid before sign-out", base, status)
		}
	}
	follower.request(t, http.MethodPost, "/auth/logout", nil, http.StatusNoContent, nil)
	if len(jar.Cookies(target)) != 0 {
		t.Fatal("sign-out through the follower did not clear the browser's cookies")
	}
	for _, base := range bases {
		deadline := time.Now().Add(20 * time.Second)
		for me(base) != http.StatusUnauthorized {
			if time.Now().After(deadline) {
				t.Fatal("a signed-out session is still accepted", base)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// enrolledUnreachedSettings sends a settings patch through a follower that
// cannot reach its primary. The answer is an explicit 503 that says either
// that nothing was sent or that the outcome is unknown; it never reports a
// commit and is never retried.
func enrolledUnreachedSettings(t *testing.T, follower *staticProcessClient, ttl string) {
	t.Helper()
	answer := enrolledSend(t, follower, http.MethodPatch, "/settings", map[string]any{"blocking": map[string]any{"blockTTL": ttl}})
	if answer.status != http.StatusServiceUnavailable || answer.Error.Key != api.KeyUnavailable && answer.Error.Key != "completion_unknown" {
		t.Fatalf("settings patch through a partitioned follower: status=%d key=%q message=%q", answer.status, answer.Error.Key, answer.Error.Message)
	}
}
