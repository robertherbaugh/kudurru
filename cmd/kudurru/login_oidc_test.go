package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

func cliTLSFile(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trust.pem")
	if os.WriteFile(p, body, 0600) != nil {
		t.Fatal("write fixture trust")
	}
	return p
}
func cliPublicFixture(t *testing.T) *servingBrowserFixture {
	t.Helper()
	return newServingBrowserFixtureConfigured(t, func(p *servingBrowserOP) { p.cliPublic = true })
}
func runActualCLILogin(t *testing.T, f *servingBrowserFixture, options loginOIDCOptions, mutate func(*http.Request), failKeyring bool) (*cliOpts, string, string, error) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutate != nil {
			mutate(r)
		}
		f.server.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	apiCA := cliTLSFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	options.caFile = cliTLSFile(t, f.op.CA)
	if options.open == nil {
		options.open = func(ctx context.Context, target string) error {
			r, e := http.NewRequestWithContext(ctx, "GET", target, nil)
			if e != nil {
				return errors.New("browser request construction")
			}
			response, e := f.op.Server.Client().Do(r)
			if e != nil {
				return errors.New("browser navigation failed")
			}
			_ = response.Body.Close()
			if response.StatusCode != 200 {
				return errors.New("browser callback refused")
			}
			return nil
		}
	}
	o := &cliOpts{getenv: func(string) string { return "" }, stdin: strings.NewReader(""), now: time.Now, creds: &cli.Credentials{Keyring: &memKeyring{failing: failKeyring}}}
	root := newRootWith(o)
	for _, c := range root.Commands() {
		if c.Name() == "login" {
			root.RemoveCommand(c)
		}
	}
	cmd := newLoginCmdWithOIDC(o, options)
	cmd.GroupID = ""
	root.AddCommand(cmd)
	var out, errOut syncBuffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	config := filepath.Join(t.TempDir(), "contexts.yaml")
	root.SetArgs([]string{"--cli-config", config, "--ca-file", apiCA, "login", "--server", server.URL, "--name", "actual", "--provider", "corp", "--oidc-ca-file", options.caFile})
	if options.device {
		root.SetArgs([]string{"--cli-config", config, "--ca-file", apiCA, "login", "--server", server.URL, "--name", "actual", "--provider", "corp", "--oidc-ca-file", options.caFile, "--device"})
	}
	e := root.ExecuteContext(t.Context())
	return o, config, out.String() + errOut.String(), e
}
func TestCLIOIDCBrowserActualTLSProductionRouterStoresOriginalIDToken(t *testing.T) {
	f := cliPublicFixture(t)
	before := f.security(t)
	o, path, text, e := runActualCLILogin(t, f, loginOIDCOptions{}, nil, true)
	if e != nil {
		t.Fatalf("actual public login refused at %s with %d token requests", cliLoginErrorPhase(e), f.op.requests.Load())
	}
	cfg, e := cli.LoadConfig(path)
	if e != nil {
		t.Fatal("load saved context")
	}
	c, ok := cfg.Context("actual")
	if !ok {
		t.Fatal("context missing")
	}
	token, _, e := o.credentials().Token(*c)
	if e != nil {
		t.Fatal("saved credential missing")
	}
	f.op.mu.Lock()
	original := f.op.lastToken
	f.op.mu.Unlock()
	if token == "" || token != original || strings.HasPrefix(token, "kdr_") || strings.Contains(text, token) || strings.Contains(text, "cli-test-access") || strings.Contains(text, "NEVER-RETAIN") {
		t.Fatal("ID-token storage/redaction contract")
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file fallback mode")
	}
	apiClient, e := apiclient.New(c.Server, apiclient.Options{Token: token, CAFile: c.CAFile})
	if e != nil {
		t.Fatal("subsequent client")
	}
	me, e := apiClient.Me(t.Context())
	if e != nil || me.Principal.Name != "corp:alice" {
		t.Fatal("saved bearer failed current production admission")
	}
	after := f.security(t)
	if len(after.Sessions) != len(before.Sessions) || len(after.ExternalSessions) != len(before.ExternalSessions) || len(after.ExternalIdentities) != len(before.ExternalIdentities) || len(after.BrowserAttempts) != len(before.BrowserAttempts) || f.op.requests.Load() != 1 {
		t.Fatal("CLI flow created server credential authority or replayed exchange")
	}
}
func TestCLIOIDCBrowserActualTLSProofRefusals(t *testing.T) {
	for _, mode := range []string{"nonce", "at_hash", "audience", "azp", "multi", "expired", "access-only", "upstream-error", "credential"} {
		t.Run(mode, func(t *testing.T) {
			f := cliPublicFixture(t)
			f.op.mu.Lock()
			f.op.mode = mode
			f.op.mu.Unlock()
			_, path, text, e := runActualCLILogin(t, f, loginOIDCOptions{}, func(r *http.Request) {
				if mode == "credential" && strings.HasSuffix(r.URL.Path, "/verify") {
					p := f.s.opts.Config.Auth.IdentityProviders[0]["clientSecretFile"].(string)
					if os.WriteFile(p, []byte("rotated-CLI-fixture"), 0600) != nil {
						t.Error("rotate fixture secret")
					}
				}
			}, false)
			if e == nil {
				t.Fatal("invalid original proof admitted")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused proof persisted context")
			}
			if f.op.requests.Load() != 1 {
				t.Fatalf("exchange count %d at %s", f.op.requests.Load(), cliLoginErrorPhase(e))
			}
			if strings.Contains(text, "NEVER-EXPOSE") || strings.Contains(e.Error(), "NEVER-EXPOSE") {
				t.Fatal("OP diagnostics disclosed")
			}
			if len(f.security(t).ExternalSessions) != 0 {
				t.Fatal("refusal issued browser authority")
			}
		})
	}
}
func TestCLIOIDCListenerFallbackAndCancellation(t *testing.T) {
	first, e := net.Listen("tcp4", "127.0.0.1:8000")
	if e != nil {
		t.Fatal("fixture primary port")
	}
	defer func() { _ = first.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	l, e := listenCLILogin(ctx)
	if e != nil || l.Addr().String() != "127.0.0.1:18000" {
		t.Fatal("bounded fallback")
	}
	_, e = receiveCLILogin(ctx, l, randomLoginProof(), "https://op.test", func(context.Context, string) error { cancel(); return context.Canceled }, "https://op.test/authorize")
	if !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	second, e := net.Listen("tcp4", "127.0.0.1:18000")
	if e != nil {
		t.Fatal("canceled listener remained open")
	}
	defer func() { _ = second.Close() }()
	if l, e := listenCLILogin(t.Context()); e == nil {
		_ = l.Close()
		t.Fatal("busy ports bypassed")
	}
}
func TestCLIOIDCRejectsInsecureAPIWithoutOPRequest(t *testing.T) {
	o := &cliOpts{now: time.Now}
	for _, r := range []cli.Resolved{{Server: "http://127.0.0.1:8080"}, {Server: "https://api.test", InsecureSkipVerify: true}} {
		cmd := newLoginCmd(o)
		_, _, _, e := o.browserLogin(cmd, r, defaultLoginOIDC())
		if e == nil {
			t.Fatal("insecure API admitted")
		}
	}
}

func cliLoginErrorPhase(e error) string {
	if e == nil {
		return "none"
	}
	for _, p := range []string{"select OIDC provider", "prepare OIDC login", "exchange OIDC code", "verify OIDC proof", "configure OIDC API transport", "configure OP TLS", "receive OIDC callback", "select an advertised", "OIDC login requires", "CLI loopback"} {
		if strings.HasPrefix(e.Error(), p) {
			return p
		}
	}
	return "local command"
}

func TestCLIOIDCCallbackRejectsStateAndHostAndConsumesOnce(t *testing.T) {
	l, e := listenCLILogin(t.Context())
	if e != nil {
		t.Fatal("loopback listener")
	}
	state := randomLoginProof()
	code, e := receiveCLILogin(t.Context(), l, state, "https://op.test", func(ctx context.Context, _ string) error {
		target := "http://" + l.Addr().String() + "/callback?code=fixture-code&iss=https%3A%2F%2Fop.test&state=" + state
		for _, tc := range []struct {
			target, host string
			status       int
		}{{target + "x", "", 400}, {target, "attacker.test", 400}, {target, "", 200}, {target, "", 409}} {
			r, e := http.NewRequestWithContext(ctx, "GET", tc.target, nil)
			if e != nil {
				return errors.New("request")
			}
			if tc.host != "" {
				r.Host = tc.host
			}
			c := &http.Client{Timeout: time.Second}
			resp, e := c.Do(r)
			if e != nil {
				return errors.New("callback HTTP")
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				return errors.New("callback status")
			}
		}
		return nil
	}, "https://op.test/authorize")
	if e != nil || code != "fixture-code" {
		t.Fatal("callback state/host/single-consumption contract")
	}
}

func TestCLIOIDCPrepareAdmissionIndependentIPs(t *testing.T) {
	f := cliPublicFixture(t)
	q, _ := json.Marshal(clioidc.PrepareRequest{Mode: "browser", Nonce: randomLoginProof(), RedirectURI: "http://127.0.0.1:8000/callback"})
	call := func(ip string) int {
		r := httptest.NewRequest("POST", "http://kudurru.test/api/v1/auth/cli/corp/prepare", strings.NewReader(string(q)))
		r.RemoteAddr = ip + ":1234"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, r)
		return w.Code
	}
	for range 5 {
		if call("192.0.2.10") != 200 {
			t.Fatal("initial client admission")
		}
	}
	if call("192.0.2.10") != 429 {
		t.Fatal("per-client request cap")
	}
	if call("192.0.2.11") != 200 {
		t.Fatal("one client locked out independent client")
	}
	if f.h.userLimit.Len() != 0 || f.h.ipLimit.Len() != 0 {
		t.Fatal("preparation charged authentication failure buckets")
	}
}

func TestCLIOIDCTokenResponseDuplicateFieldsRefused(t *testing.T) {
	for _, raw := range []string{`{"id_token":"first","id_token":"second"}`, `{"access_token":"first","access_token":"second"}`, `[]`, `{} {}`} {
		var proof publicLoginTokens
		if decodePublicLoginJSON([]byte(raw), &proof) == nil {
			t.Fatal("ambiguous token response")
		}
	}
	var proof publicLoginTokens
	if decodePublicLoginJSON([]byte(`{"id_token":"original","extension":{"value":true}}`), &proof) != nil || proof.IDToken != "original" {
		t.Fatal("bounded extension compatibility")
	}
}

func TestCLIOIDCVerifyFinalActualClockRegressionRefused(t *testing.T) {
	f := cliPublicFixture(t)
	// The HTTP adapter and runtime owners retain their real clocks. Only this
	// handler's response-time observer changes; no background clock is mutated.
	runtime := &servingRuntime{opts: f.s.opts, publisher: f.s.publisher, primary: f.s.primary}
	f.h.opts.Runtime = runtime
	var calls atomic.Int64
	var active atomic.Bool
	base := time.Now()
	runtime.opts.Now = func() time.Time {
		if active.Load() {
			calls.Add(1)
		}
		return base
	}
	var body []byte
	var bearer string
	_, _, _, e := runActualCLILogin(t, f, loginOIDCOptions{}, func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/verify") {
			base = time.Now() // Observe after the actual OP issued its strict-time proof.
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			bearer = r.Header.Get("Authorization")
			calls.Store(0)
			active.Store(true)
		}
	}, false)
	if e != nil {
		t.Fatal("baseline actual verification")
	}
	final := calls.Load()
	if final < 3 {
		t.Fatal("final observation not reached")
	}
	calls.Store(0)
	runtime.opts.Now = func() time.Time {
		if calls.Add(1) >= final {
			return base.Add(-time.Nanosecond)
		}
		return base
	}
	r := httptest.NewRequest("POST", "http://kudurru.test/api/v1/auth/cli/corp/verify", strings.NewReader(string(body)))
	r.Header.Set("Authorization", bearer)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != 401 || calls.Load() < final {
		t.Fatal("final actual-time regression admitted")
	}
}
