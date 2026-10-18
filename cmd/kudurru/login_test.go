package main

import (
	"os"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/cli"
)

func TestLoginWithTokenStoresItInTheKeyring(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("login", "--server", h.api.URL(), "--token", fakeToken)
	if !strings.Contains(out, "Logged in to") || !strings.Contains(out, "as admin (admin)") || !strings.Contains(out, "token in keyring") {
		t.Fatalf("unexpected login output: %q", out)
	}
	cfg := h.config()
	name := cli.ContextNameFor(h.api.URL())
	if cfg.CurrentContext != name {
		t.Fatalf("current context = %q, want %q", cfg.CurrentContext, name)
	}
	c, ok := cfg.Context(name)
	if !ok || c.Token != "" || c.TokenStore != cli.TokenStoreKeyring || c.User != "admin" {
		t.Fatalf("context = %+v", c)
	}
	if got, _ := h.keyring.Get(cli.KeyringService, name); got != fakeToken {
		t.Fatalf("keyring holds %q", got)
	}
	raw, _ := os.ReadFile(h.cfgPath)
	if strings.Contains(string(raw), fakeToken) {
		t.Fatal("the token leaked into the config file although the keyring works")
	}
	st, err := os.Stat(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v, want 0600", st.Mode().Perm())
	}
	// The saved context authenticates later commands.
	if out := h.mustRun("blocking", "status"); out != "blocking: enabled\n" {
		t.Fatalf("blocking status = %q", out)
	}
}

func TestLoginWithTokenOnStdin(t *testing.T) {
	h := newHarness(t)
	h.stdin = fakeToken + "\n"
	h.mustRun("login", "--server", h.api.URL(), "--token-stdin", "--name", "stdin")
	if got, _ := h.keyring.Get(cli.KeyringService, "stdin"); got != fakeToken {
		t.Fatalf("keyring holds %q", got)
	}
	h.stdin = ""
	if _, _, err := h.run("login", "--server", h.api.URL(), "--token-stdin"); err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("empty stdin: %v", err)
	}
}

func TestLoginFallsBackToTheFileWithoutAKeyring(t *testing.T) {
	h := newHarness(t)
	h.keyring.failing = true
	out, errOut, err := h.run("login", "--server", h.api.URL(), "--token", fakeToken, "--name", "lab")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "OS keyring unavailable") || !strings.Contains(out, "token in file") {
		t.Fatalf("no fallback warning: stdout %q stderr %q", out, errOut)
	}
	c, _ := h.config().Context("lab")
	if c.Token != fakeToken || c.TokenStore != cli.TokenStoreFile {
		t.Fatalf("context = %+v", c)
	}
	if st, _ := os.Stat(h.cfgPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("config with a token has mode %v", st.Mode().Perm())
	}
	if out := h.mustRun("blocking", "status"); out != "blocking: enabled\n" {
		t.Fatalf("file token not used: %q", out)
	}
}

func TestLoginWithPasswordMintsAToken(t *testing.T) {
	h := newHarness(t)
	h.stdin = "hunter2\n"
	out := h.mustRun("login", "--server", h.api.URL(), "--username", "admin", "--password-stdin", "--name", "pw", "--token-name", "laptop")
	if !strings.Contains(out, "as admin") {
		t.Fatalf("login output %q", out)
	}
	// POST /auth/tokens carried the session's CSRF token.
	if got := h.api.lastHeader("POST", "/api/v1/auth/tokens").Get("X-Kudurru-CSRF"); got != fakeCSRF {
		t.Fatalf("CSRF header on token mint = %q", got)
	}
	if body := string(h.api.lastBody("POST", "/api/v1/auth/tokens")); !strings.Contains(body, `"description":"laptop"`) {
		t.Fatalf("token request body %s", body)
	}
	if len(h.api.requested("POST /api/v1/auth/logout")) != 1 {
		t.Fatal("the login session was not closed")
	}
	tok, _ := h.keyring.Get(cli.KeyringService, "pw")
	if !strings.HasPrefix(tok, "kdr_minted000001_") {
		t.Fatalf("minted token not stored: %q", tok)
	}
	// The minted token, not the session, authenticates.
	h.mustRun("get", "zones")
	if auth := h.api.lastHeader("GET", "/api/v1/objects/dnszones").Get("Authorization"); auth != "Bearer "+tok {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestLoginFailures(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"bad token", []string{"login", "--server", h.api.URL(), "--token", "kdr_nope_nope"}, "", "unauthenticated: invalid token"},
		{"bad password", []string{"login", "--server", h.api.URL(), "-u", "admin", "-p", "wrong"}, "", "invalid username or password"},
		{"no credentials", []string{"login", "--server", h.api.URL()}, "", "pass --token"},
		{"no server", []string{"login", "--token", fakeToken}, "", "--server is required"},
		{"both", []string{"login", "--server", h.api.URL(), "--token", fakeToken, "-u", "admin"}, "", "not both"},
		{"empty stdin", []string{"login", "--server", h.api.URL(), "-u", "admin", "--password-stdin"}, "", "empty password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.stdin = tc.stdin
			_, _, err := h.run(tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := os.Stat(h.cfgPath); !os.IsNotExist(err) {
		t.Fatal("a failed login wrote the config file")
	}
}

func TestContextSwitching(t *testing.T) {
	h := newHarness(t)
	other := newFakeAPI(t)
	h.mustRun("login", "--server", h.api.URL(), "--token", fakeToken, "--name", "home")
	h.mustRun("login", "--server", other.URL(), "--token", fakeToken, "--name", "lab", "--default-namespace", "apps")

	out := h.mustRun("config", "get-contexts")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "CURRENT NAME SERVER USER NAMESPACE TOKEN" ||
		strings.Join(strings.Fields(lines[2]), " ") != "* lab "+other.URL()+" admin apps keyring" {
		t.Fatalf("get-contexts:\n%s", out)
	}
	out = h.mustRun("config", "get-contexts", "-o", "yaml")
	golden(t, h, "get-contexts", strings.ReplaceAll(out, other.URL(), "OTHER"))
	if got := h.mustRun("config", "current-context"); got != "lab\n" {
		t.Fatalf("current-context = %q", got)
	}

	// The current context (lab) is used, with its default namespace.
	h.mustRun("get", "rrset")
	if len(other.requested("GET /api/v1/objects/dnsrecordsets?namespace=apps")) != 1 {
		t.Fatalf("lab requests: %v", other.requested("GET"))
	}
	// --context picks another one for a single command.
	h.mustRun("--context", "home", "get", "zones")
	if len(h.api.requested("GET /api/v1/objects/dnszones")) != 1 {
		t.Fatal("--context home did not reach the home server")
	}
	// use-context switches for good.
	if out := h.mustRun("config", "use-context", "home"); !strings.Contains(out, `Switched to context "home"`) {
		t.Fatalf("use-context output %q", out)
	}
	h.mustRun("get", "pol")
	if len(h.api.requested("GET /api/v1/objects/policies?namespace=default")) != 1 {
		t.Fatal("use-context home not honoured")
	}
	// KUDURRU_CONTEXT selects for one process.
	h.env[cli.EnvContext] = "lab"
	h.mustRun("get", "cg")
	if len(other.requested("GET /api/v1/objects/clientgroups")) != 1 {
		t.Fatal("KUDURRU_CONTEXT not honoured")
	}
	delete(h.env, cli.EnvContext)

	if _, _, err := h.run("config", "use-context", "nope"); err == nil || !strings.Contains(err.Error(), "home, lab") {
		t.Fatalf("use-context nope: %v", err)
	}
	if _, _, err := h.run("--context", "nope", "get", "zones"); err == nil || !strings.Contains(err.Error(), `no context "nope"`) {
		t.Fatalf("--context nope: %v", err)
	}

	h.mustRun("config", "delete-context", "lab")
	if _, ok := h.config().Context("lab"); ok {
		t.Fatal("lab still present")
	}
	if _, err := h.keyring.Get(cli.KeyringService, "lab"); err == nil {
		t.Fatal("the lab token stayed in the keyring")
	}
}

func TestEnvironmentAndFlagOverrides(t *testing.T) {
	h := loggedIn(t)
	other := newFakeAPI(t)
	other.tokens = map[string]string{"kdr_other_token": "ci"}

	// KUDURRU_SERVER pointing elsewhere must not receive the context token.
	h.env[cli.EnvServer] = other.URL()
	if _, _, err := h.run("get", "zones"); err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("expected 401 from the other server, got %v", err)
	}
	if auth := other.lastHeader("GET", "/api/v1/objects/dnszones").Get("Authorization"); auth != "" {
		t.Fatalf("context token leaked to another server: %q", auth)
	}
	h.env[cli.EnvToken] = "kdr_other_token"
	h.mustRun("get", "zones")
	if auth := other.lastHeader("GET", "/api/v1/objects/dnszones").Get("Authorization"); auth != "Bearer kdr_other_token" {
		t.Fatalf("KUDURRU_TOKEN not used: %q", auth)
	}
	// Flags beat the environment.
	h.mustRun("--server", h.api.URL(), "--token", fakeToken, "get", "zones")
	if len(h.api.requested("GET /api/v1/objects/dnszones")) != 1 {
		t.Fatal("--server/--token not honoured over the environment")
	}
	// --server equal to the context's server keeps the context token.
	h.env = map[string]string{}
	h.mustRun("--server", h.api.URL()+"/", "get", "zones")
	if len(h.api.requested("GET /api/v1/objects/dnszones")) != 2 {
		t.Fatal("--server matching the context lost its token")
	}
}

func TestLogoutForgetsTheToken(t *testing.T) {
	h := loggedIn(t)
	if out := h.mustRun("logout"); !strings.Contains(out, `Logged out of context "test"`) {
		t.Fatalf("logout output %q", out)
	}
	if _, err := h.keyring.Get(cli.KeyringService, "test"); err == nil {
		t.Fatal("token still in the keyring")
	}
	if _, _, err := h.run("get", "zones"); err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("after logout: %v", err)
	}
	h.mustRun("logout", "test", "--remove")
	if len(h.config().Contexts) != 0 {
		t.Fatal("--remove kept the context")
	}
}

func TestSetContextWithoutLogin(t *testing.T) {
	h := newHarness(t)
	h.mustRun("--server", h.api.URL(), "--token", fakeToken, "config", "set-context", "ci", "--default-namespace", "apps")
	c, ok := h.config().Context("ci")
	if !ok || c.Server != h.api.URL() || c.Namespace != "apps" || c.TokenStore != cli.TokenStoreKeyring {
		t.Fatalf("context = %+v", c)
	}
	if _, _, err := h.run("config", "set-context", "empty"); err == nil {
		t.Fatal("a new context without --server must fail")
	}
}

func TestNoServerConfigured(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.run("get", "zones")
	if err == nil || !strings.Contains(err.Error(), "kudurru login") {
		t.Fatalf("err = %v", err)
	}
}
