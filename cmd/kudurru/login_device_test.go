package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

func cliDeviceFixture(t *testing.T, mode string) (*servingBrowserFixture, <-chan struct{}) {
	t.Helper()
	ready := make(chan struct{})
	f := newServingBrowserFixtureConfigured(t, func(p *servingBrowserOP) {
		p.cliPublic = true
		p.cliDevice = true
		p.mode = mode
		p.deviceReady = ready
	})
	return f, ready
}

func approveCLIDevice(t *testing.T, f *servingBrowserFixture, ready <-chan struct{}, decision string) <-chan bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		select {
		case <-ready:
		case <-t.Context().Done():
			done <- false
			return
		}
		r, e := http.NewRequestWithContext(t.Context(), "POST", f.op.Issuer("corp")+"/approve", strings.NewReader(url.Values{"user_code": {"ABCD-EFGH"}, "decision": {decision}}.Encode()))
		if e != nil {
			done <- false
			return
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, e := f.op.Server.Client().Do(r)
		if e != nil {
			done <- false
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode == http.StatusNoContent
	}()
	return done
}

func TestCLIDeviceActualTLSProductionRouterOriginalIDToken(t *testing.T) {
	f, ready := cliDeviceFixture(t, "")
	approved := approveCLIDevice(t, f, ready, "approve")
	before := f.security(t)
	o, path, text, e := runActualCLILogin(t, f, loginOIDCOptions{device: true}, nil, true)
	if e != nil {
		t.Fatalf("actual device login refused at %s", cliLoginErrorPhase(e))
	}
	if !<-approved {
		t.Fatal("actual OP approval failed")
	}
	cfg, e := cli.LoadConfig(path)
	if e != nil {
		t.Fatal("saved device context")
	}
	c, ok := cfg.Context("actual")
	if !ok {
		t.Fatal("device context missing")
	}
	raw, _, e := o.credentials().Token(*c)
	if e != nil {
		t.Fatal("stored device proof")
	}
	f.op.mu.Lock()
	original := f.op.lastToken
	code := ""
	for key := range f.op.devices {
		code = key
	}
	f.op.mu.Unlock()
	if raw == "" || raw != original || strings.Contains(text, raw) || strings.Contains(text, code) || strings.Contains(text, "cli-test-access") || strings.Contains(text, "NEVER-RETAIN") || !strings.Contains(text, "ABCD-EFGH") || !strings.Contains(text, "Approve only a request you initiated") {
		t.Fatal("device proof storage/display/redaction")
	}
	capi, e := apiclient.New(c.Server, apiclient.Options{Token: raw, CAFile: c.CAFile})
	if e != nil {
		t.Fatal("saved device API transport")
	}
	me, e := capi.Me(t.Context())
	if e != nil || me.Principal.Name != "corp:alice" {
		t.Fatal("device proof current raw admission")
	}
	after := f.security(t)
	if len(before.Sessions) != len(after.Sessions) || len(before.ExternalSessions) != len(after.ExternalSessions) || len(before.BrowserAttempts) != len(after.BrowserAttempts) || len(before.ExternalIdentities) != len(after.ExternalIdentities) || f.op.requests.Load() != 1 {
		t.Fatal("device flow created server authority or replayed poll")
	}
	// OP single consumption is observable on the actual TLS endpoint. Kudurru
	// does not claim a reusable source receipt is a one-time device grant.
	op, e := newCLIOPClient(cliTLSFile(t, f.op.CA))
	if e != nil {
		t.Fatal("OP client")
	}
	defer op.CloseIdleConnections()
	status, b, e := publicLoginResponse(t.Context(), op, f.op.Issuer("corp")+"/device-token", url.Values{"grant_type": {deviceGrantType}, "client_id": {"kudurru-cli"}, "device_code": {code}})
	defer clear(b)
	var reply publicLoginTokens
	if e != nil || status != http.StatusBadRequest || decodePublicLoginJSON(b, &reply) != nil || reply.Error != "invalid_grant" || reply.IDToken != "" {
		t.Fatal("consumed original OP grant accepted twice")
	}
}

func TestCLIDeviceActualTLSRefusals(t *testing.T) {
	for _, mode := range []string{"denied", "grant-expired", "access-only", "at_hash", "audience", "azp", "multi", "expired", "credential"} {
		t.Run(mode, func(t *testing.T) {
			f, ready := cliDeviceFixture(t, mode)
			var approved <-chan bool
			if mode != "grant-expired" {
				decision := "approve"
				if mode == "denied" {
					decision = "deny"
				}
				approved = approveCLIDevice(t, f, ready, decision)
			}
			_, path, text, e := runActualCLILogin(t, f, loginOIDCOptions{device: true}, func(r *http.Request) {
				if mode == "credential" && strings.HasSuffix(r.URL.Path, "/verify") {
					p := f.s.opts.Config.Auth.IdentityProviders[0]["clientSecretFile"].(string)
					if os.WriteFile(p, []byte("device-rotated-test-only"), 0600) != nil {
						t.Error("rotate device fixture credential")
					}
				}
			}, false)
			if approved != nil && !<-approved {
				t.Fatal("OP approval fixture")
			}
			if e == nil {
				t.Fatal("refused device flow accepted")
			}
			if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("refused device proof persisted")
			}
			if strings.Contains(text, "cli-test-access") || strings.Contains(text, "NEVER-RETAIN") {
				t.Fatal("device diagnostics disclosed proof")
			}
			expected := int64(1)
			if mode == "grant-expired" {
				expected = 0
			}
			if f.op.requests.Load() != expected || len(f.security(t).ExternalSessions) != 0 {
				t.Fatal("refusal retried or issued server authority")
			}
		})
	}
}

func TestCLIDeviceActualTLSPollProtocol(t *testing.T) {
	for _, mode := range []string{"pending", "slowdown", "timeout", "body-timeout", "denied", "refused", "redirect", "cancel", "original-deadline"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var calls []time.Time
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls = append(calls, time.Now())
				index := len(calls)
				mu.Unlock()
				if r.ParseForm() != nil || r.Header.Get("Authorization") != "" || r.PostForm.Get("grant_type") != deviceGrantType || r.PostForm.Get("device_code") != "same-original-grant" || r.PostForm.Get("client_id") != "public-client" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "body-timeout" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if mode == "timeout" && index == 1 {
					<-r.Context().Done()
					return
				}
				if mode == "redirect" {
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				if mode == "refused" {
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
					return
				}
				if mode == "denied" {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
					return
				}
				if index == 1 && (mode == "pending" || mode == "slowdown") {
					code := "authorization_pending"
					if mode == "slowdown" {
						code = "slow_down"
					}
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"id_token": "protocol-proof"})
			}))
			defer server.Close()
			ca := cliTLSFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
			op, e := newCLIOPClient(ca)
			if e != nil {
				t.Fatal("poll TLS")
			}
			defer op.CloseIdleConnections()
			if mode == "timeout" || mode == "body-timeout" {
				op.Timeout = 100 * time.Millisecond
			}
			start := time.Now()
			deadline := start.Add(12 * time.Second)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			if mode == "original-deadline" {
				deadline = start.Add(100 * time.Millisecond)
			}
			proof, e := pollDeviceLogin(ctx, op, clioidc.Preparation{ClientID: "public-client", TokenURL: server.URL}, "same-original-grant", deadline, time.Second, time.Now)
			mu.Lock()
			times := append([]time.Time(nil), calls...)
			mu.Unlock()
			success := mode == "pending" || mode == "slowdown" || mode == "timeout"
			if (e == nil) != success || success && proof.IDToken != "protocol-proof" {
				t.Fatal("explicit device protocol outcome")
			}
			expected := 1
			if success {
				expected = 2
			}
			if mode == "cancel" || mode == "original-deadline" {
				expected = 0
			}
			if len(times) != expected {
				t.Fatal("refusal retried or wrong explicit poll count")
			}
			if len(times) > 0 && times[0].Sub(start) < time.Second {
				t.Fatal("initial poll too early")
			}
			if len(times) > 1 {
				minimum := time.Second
				if mode == "slowdown" {
					minimum = 6 * time.Second
				}
				if mode == "timeout" {
					minimum = 2 * time.Second
				}
				if times[1].Sub(times[0]) < minimum {
					t.Fatal("pending/slowdown/timeout frequency increased")
				}
			}
		})
	}
}

func TestCLIDeviceUnsupportedAndModeFlags(t *testing.T) {
	f := cliPublicFixture(t)
	_, _, _, e := runActualCLILogin(t, f, loginOIDCOptions{device: true}, nil, false)
	if e == nil || f.op.requests.Load() != 0 {
		t.Fatal("unadvertised device flow started")
	}
	for _, args := range [][]string{{"--device", "--username", "admin"}, {"--device", "--token-stdin"}, {"--device", "--token-name", "substitute"}} {
		o := &cliOpts{getenv: func(string) string { return "" }, stdin: strings.NewReader(""), now: time.Now}
		cmd := newLoginCmd(o)
		cmd.SetArgs(args)
		if cmd.ExecuteContext(t.Context()) == nil {
			t.Fatal("device/local credential modes mixed")
		}
	}
}

func TestCLIDeviceResponseOriginalBounds(t *testing.T) {
	now := time.Now()
	p := publicDeviceGrant{DeviceCode: "private", UserCode: "ABCD-EFGH", VerificationURI: "https://provider.test/approve", ExpiresIn: 60}
	deadline, interval, e := p.bounds(now, now.Add(30*time.Second))
	if e != nil || interval != 5*time.Second || !deadline.Equal(now.Add(30*time.Second)) {
		t.Fatal("original receipt/default interval")
	}
	for _, bad := range []publicDeviceGrant{{DeviceCode: "private", UserCode: "escape\x1b", VerificationURI: p.VerificationURI, ExpiresIn: 60}, {DeviceCode: "private", UserCode: p.UserCode, VerificationURI: "http://provider.test", ExpiresIn: 60}, {DeviceCode: "private", UserCode: p.UserCode, VerificationURI: p.VerificationURI, ExpiresIn: 0}} {
		if _, _, e := bad.bounds(now, deadline); e == nil {
			t.Fatal("unbounded device response")
		}
	}
}

func TestCLIDeviceActualTLSConcurrentOriginalGrantConsumption(t *testing.T) {
	op := newServingBrowserOP(t)
	op.cliDevice = true
	client, e := newCLIOPClient(cliTLSFile(t, op.CA))
	if e != nil {
		t.Fatal("device peer trust")
	}
	defer client.CloseIdleConnections()
	var grant publicDeviceGrant
	if publicLoginPost(t.Context(), client, op.Issuer("corp")+"/device", url.Values{"client_id": {"kudurru-cli"}, "scope": {"openid"}}, &grant) != nil {
		t.Fatal("original device authorization")
	}
	request, e := http.NewRequestWithContext(t.Context(), "POST", op.Issuer("corp")+"/approve", strings.NewReader(url.Values{"user_code": {grant.UserCode}, "decision": {"approve"}}.Encode()))
	if e != nil {
		t.Fatal("approval construction")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reply, e := client.Do(request)
	if e != nil {
		t.Fatal("approval")
	}
	_ = reply.Body.Close()
	if reply.StatusCode != http.StatusNoContent {
		t.Fatal("approval refused")
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			status, b, e := publicLoginResponse(t.Context(), client, op.Issuer("corp")+"/device-token", url.Values{"grant_type": {deviceGrantType}, "client_id": {"kudurru-cli"}, "device_code": {grant.DeviceCode}})
			clear(b)
			if e != nil {
				status = 0
			}
			results <- status
		}()
	}
	a, b := <-results, <-results
	if min(a, b) != http.StatusOK || max(a, b) != http.StatusBadRequest {
		t.Fatal("OP grant consumption was not single use")
	}
}

func TestCLIDeviceExplicitModeDoesNotUseAmbientBearer(t *testing.T) {
	var mu sync.Mutex
	paths := []string{}
	authSeen := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		authSeen = authSeen || r.Header.Get("Authorization") != ""
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	ca := cliTLSFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	o := &cliOpts{getenv: func(k string) string {
		if k == cli.EnvToken {
			return "ambient-test-only"
		}
		return ""
	}, stdin: strings.NewReader(""), now: time.Now}
	root := newRootWith(o)
	root.SetArgs([]string{"--cli-config", filepath.Join(t.TempDir(), "config.yaml"), "--ca-file", ca, "login", "--server", server.URL, "--device"})
	var out syncBuffer
	root.SetOut(&out)
	root.SetErr(&out)
	if root.ExecuteContext(t.Context()) == nil {
		t.Fatal("missing device capability accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/api/v1/auth/cli/providers" || authSeen {
		t.Fatal("explicit device mode reused ambient bearer")
	}
}

func TestCLIDeviceActualTLSInFlightCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "deadline" {
				var finish context.CancelFunc
				ctx, finish = context.WithTimeout(ctx, 3*time.Second)
				defer finish()
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			if mode == "cancel" {
				go func() {
					select {
					case <-entered:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			ca := cliTLSFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
			op, e := newCLIOPClient(ca)
			if e != nil {
				t.Fatal("cancellation fixture trust")
			}
			defer op.CloseIdleConnections()
			_, e = pollDeviceLogin(ctx, op, clioidc.Preparation{ClientID: "client", TokenURL: server.URL}, "original-code", time.Now().Add(5*time.Second), time.Second, time.Now)
			want := context.Canceled
			if mode == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(e, want) {
				t.Fatal("in-flight cancellation lost original context outcome")
			}
			select {
			case <-entered:
			default:
				t.Fatal("OP request not in flight")
			}
		})
	}
}
