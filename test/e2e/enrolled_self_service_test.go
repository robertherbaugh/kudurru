package e2e

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// enrolledForwardedSelfService changes a must-change user's password through
// the follower process (auth.password, D368) and runs `kudurru login
// --username` against the follower, which forwards the login, the CLI token
// creation (auth.tokens) and the sign-out of the login's session. The new
// cookie works on both nodes, the cookie from before the change on neither,
// and the CLI's token works on the follower and the primary.
func enrolledForwardedSelfService(t *testing.T, bin string, primary, follower *staticProcessClient) {
	t.Helper()
	const user, initial, changed = "e2e-must-change", "e2e-initial-password-1", "e2e-changed-password-2" // #nosec G101 -- disposable loopback fixture credentials
	primary.request(t, http.MethodPost, "/iam/users", api.CreateLocalUserRequest{Name: user, Password: initial}, http.StatusCreated, nil)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	browser := &staticProcessClient{client: &http.Client{Jar: jar, Transport: transport, Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: follower.base}
	var login api.SessionInfo
	deadline := time.Now().Add(20 * time.Second)
	for {
		// The new account reaches the follower at its next pull.
		answer := enrolledSend(t, browser, http.MethodPost, "/auth/login", api.LoginRequest{Username: user, Password: initial})
		if answer.status == http.StatusOK {
			browser.request(t, http.MethodGet, "/auth/me", nil, http.StatusOK, &login)
			break
		}
		if answer.status != http.StatusUnauthorized || time.Now().After(deadline) {
			t.Fatalf("login of the new account through the follower: %d %s", answer.status, answer.raw)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !login.MustChangePassword {
		t.Fatal("the new account does not have to change its password")
	}
	browser.csrf = login.CSRF
	target, err := url.Parse(follower.base)
	if err != nil {
		t.Fatal(err)
	}
	before := jar.Cookies(target)
	if answer := enrolledSend(t, browser, http.MethodGet, "/zones", nil); answer.status != http.StatusForbidden {
		t.Fatalf("a must-change session reached a route: %d", answer.status)
	}
	if answer := enrolledSend(t, browser, http.MethodPost, "/auth/password", api.PasswordChangeRequest{CurrentPassword: "e2e-wrong-password-9", NewPassword: changed}); answer.status != http.StatusUnauthorized { // #nosec G101 -- deliberately wrong fixture credential
		t.Fatalf("wrong current password through the follower: status=%d key=%q", answer.status, answer.Error.Key)
	}
	var info api.SessionInfo
	browser.request(t, http.MethodPost, "/auth/password", api.PasswordChangeRequest{CurrentPassword: initial, NewPassword: changed}, http.StatusOK, &info)
	if info.MustChangePassword || info.CSRF == "" {
		t.Fatalf("password change through the follower: %+v", info)
	}
	after := jar.Cookies(target)
	for _, base := range []string{follower.base, primary.base} {
		if status := enrolledCookieStatus(t, base, after); status != http.StatusOK {
			t.Fatal("the cookie from the password change does not work", base, status)
		}
		waitUntil := time.Now().Add(20 * time.Second)
		for enrolledCookieStatus(t, base, before) != http.StatusUnauthorized {
			if time.Now().After(waitUntil) {
				t.Fatal("the cookie from before the password change still works", base)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// A client of its own: the primary's client holds the administrator's session.
	onPrimary := &staticProcessClient{client: &http.Client{Timeout: 35 * time.Second}, base: primary.base}
	if answer := enrolledSend(t, onPrimary, http.MethodPost, "/auth/login", api.LoginRequest{Username: user, Password: changed}); answer.status != http.StatusOK {
		t.Fatalf("the new password on the primary: %d", answer.status)
	}

	// `kudurru login --username` against the follower stores a token that
	// works there and on the primary.
	dir := t.TempDir()
	env := append(os.Environ(), "KUDURRU_CLI_CONFIG="+filepath.Join(dir, "cli.yaml"), "KUDURRU_KEYRING=file", "HOME="+dir)
	server := strings.TrimSuffix(follower.base, api.BasePath)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "login", "--server", server, "--username", "admin", "--password-stdin", "--name", "via-follower") //nolint:gosec // the test's binary
	cmd.Env = env
	cmd.Stdin = strings.NewReader("audit-e2e-password-1\n") // #nosec G101 -- disposable loopback fixture credential
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kudurru login --username against the follower: %v\n%s", err, out)
	}
	whoami := exec.CommandContext(ctx, bin, "api", "/auth/me", "--context", "via-follower") //nolint:gosec // the test's binary
	whoami.Env = env
	if out, err := whoami.CombinedOutput(); err != nil || !strings.Contains(string(out), `"admin"`) {
		t.Fatalf("the CLI token from the follower login on the follower: %v\n%s", err, out)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "cli.yaml")) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	token := regexp.MustCompile(`kdr_[a-z0-9]{12}_[A-Za-z0-9_-]{43}`).Find(stored)
	if token == nil {
		t.Fatal("the CLI did not store a token in its file")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, onPrimary.base+"/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+string(token))
	response, err := onPrimary.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("the CLI token from the follower login on the primary:", response.StatusCode)
	}
}
