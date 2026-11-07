package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type loginTOTPTransport func(*http.Request) (*http.Response, error)

func (f loginTOTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCLILoginTOTPInput(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"two buffered lines", "test-password\n012345\n", true},
		{"crlf", "test-password\r\n012345\r\n", true},
		{"code eof", "test-password\n012345", true},
		{"missing code", "test-password\n", false},
		{"missing delimiter", "test-password", false},
		{"empty password", "\n012345\n", false},
		{"long password", strings.Repeat("x", 1025) + "\n012345\n", false},
		{"max password", strings.Repeat("x", 1024) + "\r\n012345\r\n", true},
		{"long code", "test-password\n0123456\n", false},
		{"space", "test-password\n 12345\n", false},
		{"unicode", "test-password\n１２３４５６\n", false},
		{"embedded cr", "test-password\n01\r345\n", false},
		{"cr eof", "test-password\n012345\r", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &cliOpts{stdin: strings.NewReader(tc.input)}
			cmd := newLoginCmd(o)
			cmd.SetContext(t.Context())
			password, code, err := o.readLoginProof(cmd, loginFactorOptions{username: "local", passwordStdin: true, stdin: true})
			if (err == nil) != tc.valid || (tc.valid && (code != "012345" || len(password) == 0)) {
				t.Fatal("bounded buffered input contract failed")
			}
		})
	}
	// A required code line must not require EOF from the producer.
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { _, _ = io.WriteString(w, "test-password\n012345\n"); close(done) }()
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	o := &cliOpts{stdin: r}
	cmd := newLoginCmd(o)
	cmd.SetContext(t.Context())
	_, code, err := o.readLoginProof(cmd, loginFactorOptions{username: "local", passwordStdin: true, stdin: true})
	if err != nil || code != "012345" {
		t.Fatal("line input waited for EOF or lost prefetched code")
	}
	<-done
}

func TestCLILoginTOTPHiddenInputAndCancellation(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	values := [][]byte{[]byte("test-password"), []byte("012345")}
	password, code, err := readTerminalLoginProof(t.Context(), &out, "", func() ([]byte, error) {
		value := values[calls]
		calls++
		return value, nil
	})
	if err != nil || password != "test-password" || code != "012345" || calls != 2 || strings.Contains(out.String(), password) || strings.Contains(out.String(), code) {
		t.Fatal("hidden proof prompt contract failed")
	}
	for _, value := range values {
		if bytes.Count(value, []byte{0}) != len(value) {
			t.Fatal("owned prompt byte buffer retained proof")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = readTerminalLoginProof(ctx, io.Discard, "", func() ([]byte, error) {
		t.Error("canceled prompt read proof")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("prompt cancellation lost")
	}
	_, _, err = readTerminalLoginProof(t.Context(), io.Discard, "test-password", func() ([]byte, error) {
		return nil, errors.New("sensitive-marker")
	})
	if err == nil || strings.Contains(err.Error(), "sensitive-marker") {
		t.Fatal("terminal error reflected secret")
	}
	o := &cliOpts{stdin: strings.NewReader("012345\n")}
	cmd := newLoginCmd(o)
	cmd.SetContext(t.Context())
	if _, _, err = o.readLoginProof(cmd, loginFactorOptions{username: "local", password: "test-password", prompt: true}); err == nil {
		t.Fatal("nonterminal prompt silently used visible input")
	}
}

func TestCLILoginTOTPInvalidFlagsAndProofNeverSend(t *testing.T) {
	for _, args := range [][]string{
		{"--username", "local", "--totp", "--totp-stdin"},
		{"--totp-stdin", "--password-stdin"},
		{"--username", "local", "--totp", "--password-stdin"},
		{"--username", "local", "--totp-stdin"},
		{"--username", "local", "--totp-stdin", "--password-stdin", "--password", "test-password"},
		{"--username", "local", "--totp-stdin", "--password-stdin", "--token-stdin"},
		{"--username", "local", "--totp-stdin", "--password-stdin", "--token", "fixture"},
		{"--username", "local", "--totp-stdin", "--password-stdin"}, // invalid code below
	} {
		h := newHarness(t)
		h.stdin = "test-password\n12345x\n"
		o := h.opts()
		var calls atomic.Int32
		o.httpClient = &http.Client{Transport: loginTOTPTransport(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected network")
		})}
		_, _, err := runLoginTOTP(t, o, h.cfgPath, append([]string{"--server", "https://fixture.invalid"}, args...)...)
		if err == nil || calls.Load() != 0 || len(h.keyring.m) != 0 {
			t.Fatal("invalid proof/flags reached network or keyring")
		}
		if _, err := os.Stat(h.cfgPath); !os.IsNotExist(err) {
			t.Fatal("invalid input changed config")
		}
	}
}

func TestCLILoginTOTPSafeStageErrors(t *testing.T) {
	const marker = "reflected-sensitive-marker"
	for _, stage := range []string{"login", "me", "mint", "verify"} {
		t.Run(stage, func(t *testing.T) {
			var posts, mints, reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fail := false
				switch r.URL.Path {
				case "/api/v1/auth/login":
					posts.Add(1)
					fail = stage == "login"
				case "/api/v1/auth/tokens":
					mints.Add(1)
					fail = stage == "mint"
					if !fail {
						_, _ = io.WriteString(w, `{"token":"kdr_fixture_value"}`)
						return
					}
				case "/api/v1/auth/me":
					n := reads.Add(1)
					fail = stage == "me" || (stage == "verify" && n == 2)
				}
				if fail {
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":{"key":"generation_pending","message":"`+marker+`","hint":"`+marker+`"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"principal":{"name":"local"},"csrf":"fixture"}`)
			}))
			defer srv.Close()
			h := newHarness(t)
			h.stdin = "test-password\n012345\n"
			out, errOut, err := runLoginTOTP(t, h.opts(), h.cfgPath, "--server", srv.URL, "--username", "local", "--password-stdin", "--totp-stdin")
			if err == nil || strings.Contains(err.Error()+out+errOut, marker) || posts.Load() != 1 || mints.Load() > 1 || len(h.keyring.m) != 0 {
				t.Fatal("stage failure reflected proof, retried or persisted")
			}
			if _, err := os.Stat(h.cfgPath); !os.IsNotExist(err) {
				t.Fatal("failed credential chain saved config")
			}
		})
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(loginStageError("login", cause), cause) {
			t.Fatal("safe stage projection lost context category")
		}
	}
}

func runLoginTOTP(t *testing.T, o *cliOpts, configPath string, args ...string) (string, string, error) {
	t.Helper()
	root := newRootWith(o)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--cli-config", configPath, "login"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// Wait before dispatch, never retry a proof. There are no background fixture
// mutations; join the actual published generation after each preceding command.
func loginTOTPCurrentClient(t *testing.T, f *servingHTTPFixture) *http.Client {
	t.Helper()
	return &http.Client{Transport: loginTOTPTransport(func(r *http.Request) (*http.Response, error) {
		awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
			image, err := f.s.opts.Store.ReadGeneration(t.Context())
			return err == nil && v.Base().Same(image.Generation()) && f.s.publisher.Status().ProtectedReady
		})
		return http.DefaultTransport.RoundTrip(r)
	})}
}

func TestCLILoginTOTPActualSQLiteOnceAndPersistence(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	seed := factorInstall(t, f)
	var posts, mints, logouts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/auth/login":
			posts.Add(1)
		case "POST /api/v1/auth/tokens":
			mints.Add(1)
		case "POST /api/v1/auth/logout":
			logouts.Add(1)
		}
		f.server.ServeHTTP(w, r)
	}))
	defer srv.Close()
	h := newHarness(t)
	code := servingHTTPFactorCode(t, seed, time.Now())
	h.stdin = "initial-password\n" + code + "\n"
	o := h.opts()
	o.httpClient = loginTOTPCurrentClient(t, f)
	// An ambient token must not override explicit local username resolution.
	h.env[cli.EnvToken] = "ignored-ambient-token"
	out, errOut, err := runLoginTOTP(t, o, h.cfgPath, "--server", srv.URL, "--username", "root-command", "--password-stdin", "--totp-stdin", "--name", "factor")
	if err != nil || posts.Load() != 1 || mints.Load() != 1 || logouts.Load() != 1 {
		t.Fatal("actual CLI credential chain failed or repeated")
	}
	cfg := h.config()
	entry, ok := cfg.Context("factor")
	if !ok {
		t.Fatal("successful login did not save context")
	}
	token, _, err := o.credentials().Token(*entry)
	if err != nil || token == "" || entry.Token != "" || strings.Contains(out+errOut, code) || strings.Contains(out+errOut, "initial-password") || strings.Contains(out+errOut, token) {
		t.Fatal("credential persistence/output boundary failed")
	}
	raw, err := os.ReadFile(h.cfgPath)
	if err != nil || bytes.Contains(raw, []byte(code)) || bytes.Contains(raw, []byte("initial-password")) || bytes.Contains(raw, []byte(token)) || len(h.keyring.m) != 1 {
		t.Fatal("config/keyring retained wrong material")
	}
	c, err := apiclient.New(srv.URL, apiclient.Options{Token: token, HTTPClient: loginTOTPCurrentClient(t, f)})
	if err != nil {
		t.Fatal("verification client failed")
	}
	me, err := c.Me(t.Context())
	if err != nil || me.Principal.Name != "root-command" {
		t.Fatal("persisted personal token failed actual authentication")
	}
	assertLoginTOTPState(t, f, seed, code, true, 1, true)
	id, parsed := tokens.Parse(token)
	var issued internalv1.APIToken
	if !parsed || f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &issued) != nil || issued.UID == "" || issued.Spec.SecretHash != tokens.Hash(token) {
		t.Fatal("persisted token does not match exact durable token incarnation")
	}
	// A separate explicit attempt with the same proof is durable replay, even
	// after the local admission bucket is reset for this fixture assertion.
	factorResetAdmission(f)
	c, _ = apiclient.New(srv.URL, apiclient.Options{HTTPClient: loginTOTPCurrentClient(t, f)})
	if _, err = c.LoginWithTOTP(t.Context(), "root-command", "initial-password", code); !apiclient.IsStatus(err, 401) {
		t.Fatal("replayed code authenticated")
	}
	assertLoginTOTPState(t, f, seed, code, true, 1, true)
}

func TestCLILoginTOTPActualSQLiteDenialDoesNotConsume(t *testing.T) {
	for _, withFactor := range []bool{false, true} {
		name := "missing-factor"
		if withFactor {
			name = "wrong-password"
		}
		t.Run(name, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			seed := factorInstall(t, f)
			srv := httptest.NewServer(f.server)
			defer srv.Close()
			h := newHarness(t)
			code := servingHTTPFactorCode(t, seed, time.Now())
			h.stdin = "initial-password\n"
			args := []string{"--server", srv.URL, "--username", "root-command", "--password-stdin"}
			if withFactor {
				h.stdin = "wrong-password\n" + code + "\n"
				args = append(args, "--totp-stdin")
			}
			o := h.opts()
			o.httpClient = loginTOTPCurrentClient(t, f)
			_, _, err := runLoginTOTP(t, o, h.cfgPath, args...)
			if !apiclient.IsStatus(err, 401) || len(h.keyring.m) != 0 {
				t.Fatal("denied proof authenticated or persisted")
			}
			if _, err := os.Stat(h.cfgPath); !os.IsNotExist(err) {
				t.Fatal("denied login saved config")
			}
			assertLoginTOTPState(t, f, seed, code, false, 0, false)
		})
	}
}

func TestCLILoginTOTPActualSQLiteLostResponseNeverRetries(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	seed := factorInstall(t, f)
	var posts, others atomic.Int32
	var committed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/login" {
			others.Add(1)
			f.server.ServeHTTP(w, r)
			return
		}
		posts.Add(1)
		recorded := httptest.NewRecorder()
		f.server.ServeHTTP(recorded, r)
		committed.Store(recorded.Code == 200)
		// Drop the response only AFTER the real handler committed enrollment and
		// obtained its postcommit seal. No proof escapes in failure output.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error("could not drop committed login response")
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	h := newHarness(t)
	code := servingHTTPFactorCode(t, seed, time.Now())
	h.stdin = "initial-password\n" + code + "\n"
	o := h.opts()
	o.httpClient = loginTOTPCurrentClient(t, f)
	// Reuse an established connection, where transports can otherwise consider
	// replay on connection loss. This observation carries no credential proof.
	response, err := o.httpClient.Get(srv.URL + "/api/v1/auth/status")
	if err != nil {
		t.Fatal("could not establish observation connection")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("fixture status not ready")
	}
	others.Store(0)
	out, errOut, err := runLoginTOTP(t, o, h.cfgPath, "--server", srv.URL, "--username", "root-command", "--password-stdin", "--totp-stdin")
	if err == nil || !committed.Load() || posts.Load() != 1 || others.Load() != 0 || len(h.keyring.m) != 0 || strings.Contains(out+errOut+err.Error(), code) {
		t.Fatal("lost response retried, persisted or exposed proof")
	}
	if _, err := os.Stat(h.cfgPath); !os.IsNotExist(err) {
		t.Fatal("unknown login outcome saved config")
	}
	assertLoginTOTPState(t, f, seed, code, true, 1, false)
}

func assertLoginTOTPState(t *testing.T, f *servingHTTPFixture, seed *totp.Seed, code string, consumed bool, sessions int, revoked bool) {
	t.Helper()
	user, state := factorState(t, f)
	if len(state.TOTP) != 1 || state.TOTP[0].HasConsumed != consumed || len(state.Sessions) != sessions {
		t.Fatal("unexpected durable replay/enrollment state")
	}
	account, _, _ := seed.Identity()
	if account.UID != string(user.UID) || state.TOTP[0].Account != account {
		t.Fatal("replay state bound to wrong account incarnation")
	}
	if consumed {
		step, err := totp.Match(seed, code, time.Now())
		if err != nil || state.TOTP[0].LastConsumedStep != step || state.Sessions[0].Revoked != revoked || state.Sessions[0].Account.UID != account.UID {
			t.Fatal("consume/session state mismatch")
		}
	}
	objects, err := f.s.opts.Store.List(t.Context(), "APIToken", store.ListOptions{})
	if err != nil {
		t.Fatal("could not inspect token inventory")
	}
	found := 0
	for _, object := range objects.Items {
		token := object.(*internalv1.APIToken)
		if token.Name == strings.Split(f.token, "_")[1] {
			continue // Existing fixture admission token.
		}
		found++
		if !revoked || token.UID == "" || token.Spec.Owner.UID != account.UID || token.Spec.Owner.ID != "local:root-command" {
			t.Fatal("CLI token has wrong owner/UID or unexpected issuance")
		}
	}
	if (revoked && found != 1) || (!revoked && found != 0) {
		t.Fatal("unexpected personal-token issuance count")
	}
}

func TestCLILoginTOTPExplicitPasswordStdinCode(t *testing.T) {
	o := &cliOpts{stdin: strings.NewReader("012345\n")}
	cmd := newLoginCmd(o)
	cmd.SetContext(t.Context())
	password, code, err := o.readLoginProof(cmd, loginFactorOptions{username: "local", password: "test-password", stdin: true})
	if err != nil || password != "test-password" || code != "012345" {
		t.Fatal("explicit password with stdin code failed")
	}
}

type reflectingLoginKeyring struct{ *memKeyring }

func (*reflectingLoginKeyring) Set(_, _, secret string) error { return errors.New(secret) }

func TestCLILoginTOTPSafeKeyringWarning(t *testing.T) {
	h := newHarness(t)
	h.stdin = "hunter2\n012345\n"
	o := h.opts()
	o.creds = &cli.Credentials{Keyring: &reflectingLoginKeyring{h.keyring}}
	out, errOut, err := runLoginTOTP(t, o, h.cfgPath, "--server", h.api.URL(), "--username", "admin", "--password-stdin", "--totp-stdin", "--name", "fallback")
	if err != nil || !strings.Contains(errOut, "OS keyring unavailable") {
		t.Fatal("safe keyring fallback failed")
	}
	entry, ok := h.config().Context("fallback")
	if !ok || entry.Token == "" || strings.Contains(out+errOut, entry.Token) || strings.Contains(out+errOut, "012345") || strings.Contains(out+errOut, "hunter2") {
		t.Fatal("keyring error reflected credentials")
	}
	stat, err := os.Stat(h.cfgPath)
	if err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatal("fallback config permissions changed")
	}
}
