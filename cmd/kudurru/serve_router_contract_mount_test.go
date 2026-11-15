package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

type routerContractAnswer struct {
	status             int
	contentType        string
	key, message, hint string
}

// routerContractRequest sends one request to the mounted API. A complete
// SQLite node answers 503 generation_pending while a publication is being
// replaced; only that transient answer is observed again. Every probe here is
// refused before any command, so a new attempt replays no mutation.
func routerContractRequest(t *testing.T, c *http.Client, method, address, csrf, bearer string, body any) routerContractAnswer {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		request, err := http.NewRequestWithContext(t.Context(), method, address, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if csrf != "" {
			request.Header.Set("X-Kudurru-CSRF", csrf)
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := c.Do(request)
		if err != nil {
			t.Fatal("router contract transport failed", method, address)
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		answer := routerContractAnswer{status: response.StatusCode, contentType: response.Header.Get("Content-Type")}
		var envelope struct {
			Error struct {
				Key     string `json:"key"`
				Message string `json:"message"`
				Hint    any    `json:"hint"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			answer.key, answer.message = envelope.Error.Key, envelope.Error.Message
			answer.hint, _ = envelope.Error.Hint.(string)
		}
		if answer.status != http.StatusServiceUnavailable || answer.key != "generation_pending" || !time.Now().Before(deadline) {
			return answer
		}
		select {
		case <-t.Context().Done():
			t.Fatal("router contract observation canceled")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// On the complete SQLite runtime an unsafe request to a path that is not a
// registered route gets the router's own 404 envelope, not a 503 that claims a
// command is unavailable, and the settings ownership routes that this runtime
// does not use say what to use instead (G21-05).
func TestRunServeSQLiteUnregisteredMutationsAndOwnershipRefusals(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	startStaticMount(t, cfg)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var status api.AuthStatus
	mountedFactorRequest(t, c, base, http.MethodGet, "/auth/status", "", nil, http.StatusOK, &status)
	var login api.SessionInfo
	mountedFactorRequest(t, c, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	if login.CSRF == "" {
		t.Fatal("mounted admin login lacks CSRF")
	}

	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/zones"},
		{http.MethodPut, "/zones/home"},
		{http.MethodPatch, "/zones/home/records"},
		{http.MethodPost, "/zones/home/records/www/A"},
		{http.MethodPost, "/zones/home/unregistered"},
		{http.MethodPost, "/unblockrequests"},
		{http.MethodPut, "/unblockrequests/default/request"},
		{http.MethodPost, "/unblockrequests/default/request/review"},
		{http.MethodPost, "/blocklists/default/list/sources/0/entries"},
		{http.MethodDelete, "/blocklists/default/list/sources/0/entries"},
	} {
		got := routerContractRequest(t, c, probe.method, base+probe.path, login.CSRF, "", map[string]any{})
		if got.status != http.StatusNotFound || got.key != api.KeyNotFound || !strings.HasPrefix(got.message, "no route "+probe.method+" ") || !strings.HasPrefix(got.contentType, "application/json") {
			t.Errorf("%s %s: status=%d key=%q message=%q, want the router's 404 not_found envelope", probe.method, probe.path, got.status, got.key, got.message)
		}
	}
	// A registered route of the same families still reaches its own command.
	if got := routerContractRequest(t, c, http.MethodDelete, base+"/zones/absent-contract-zone", login.CSRF, "", nil); got.status != http.StatusNotFound || strings.HasPrefix(got.message, "no route ") {
		t.Errorf("registered zone deletion: status=%d key=%q message=%q", got.status, got.key, got.message)
	}

	const ownershipHint = "this server stores cluster settings in its database: change them with `kudurru settings set` or PATCH /api/v1/settings; there is no file ownership to adopt or release"
	const exportHint = "this server stores cluster settings in its database: read them with `kudurru settings get` or GET /api/v1/settings"
	for _, refusal := range []struct {
		path, hint string
		body       any
	}{
		{"/settings/sections/upstreams/ownership/adopt", ownershipHint, map[string]string{"revision": "contract"}},
		{"/settings/sections/upstreams/ownership/release", ownershipHint, map[string]string{"revision": "contract"}},
		{"/settings/sections/upstreams/yaml", exportHint, map[string]any{}},
	} {
		got := routerContractRequest(t, c, http.MethodPost, base+refusal.path, login.CSRF, "", refusal.body)
		if got.status != http.StatusServiceUnavailable || got.key != api.KeyUnavailable || got.hint != refusal.hint {
			t.Errorf("POST %s: status=%d key=%q hint=%q", refusal.path, got.status, got.key, got.hint)
		}
	}

	// The CLI prints the refusal with its hint.
	var token api.CreatedToken
	mountedFactorRequest(t, c, base, http.MethodPost, "/auth/tokens", login.CSRF, api.CreateTokenRequest{Description: "router-contract"}, http.StatusCreated, &token)
	if got := routerContractRequest(t, &http.Client{Timeout: 10 * time.Second}, http.MethodGet, base+"/auth/me", "", token.Token, nil); got.status != http.StatusOK {
		t.Fatal("new token never became usable", got.status, got.key)
	}
	for _, command := range []struct {
		args []string
		want string
	}{
		{[]string{"settings", "adopt", "upstreams", "--revision", "contract"}, "unavailable: canonical settings do not use legacy ownership storage (hint: " + ownershipHint + ")"},
		{[]string{"settings", "release", "upstreams", "--revision", "contract"}, "unavailable: canonical settings do not use legacy ownership storage (hint: " + ownershipHint + ")"},
		{[]string{"settings", "yaml", "upstreams"}, "unavailable: canonical settings do not use legacy ownership storage (hint: " + exportHint + ")"},
	} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			o := &cliOpts{getenv: func(string) string { return "" }, stdin: strings.NewReader(""), now: time.Now, creds: &cli.Credentials{Keyring: &memKeyring{}}}
			root := newRootWith(o)
			var out, errOut syncBuffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			root.SetArgs(append([]string{"--cli-config", filepath.Join(t.TempDir(), "contexts.yaml"), "--server", "http://" + cfg.Listeners.API.Address, "--token", token.Token}, command.args...))
			err := root.ExecuteContext(t.Context())
			if apiclient.IsKey(err, "generation_pending") && time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			if err == nil || err.Error() != command.want {
				t.Errorf("kudurru %s:\n got %v\nwant %s", strings.Join(command.args, " "), err, command.want)
			}
			break
		}
	}
}
