package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/spf13/cobra"
)

type loginOIDCOptions struct {
	provider, caFile string
	device           bool
	open             func(context.Context, string) error
}

func defaultLoginOIDC() loginOIDCOptions { return loginOIDCOptions{open: openLoginBrowser} }
func openLoginBrowser(ctx context.Context, target string) error {
	name, args := "xdg-open", []string{target}
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", target}
	}
	// #nosec G204 -- fixed platform executable; URL is one argument without a shell.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return errors.New("browser could not be opened; start again with --device on a headless machine")
	}
	return nil
}
func randomLoginProof() string {
	var b [32]byte
	_, e := rand.Read(b[:])
	if e != nil {
		panic("cryptographic random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func listenCLILogin(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	for _, address := range []string{"127.0.0.1:8000", "127.0.0.1:18000"} {
		l, e := lc.Listen(ctx, "tcp4", address)
		if e == nil {
			return l, nil
		}
		if !errors.Is(e, syscall.EADDRINUSE) {
			return nil, clioidc.ErrInvalid
		}
	}
	return nil, errors.New("CLI loopback ports 8000 and 18000 are unavailable")
}

type loginCode struct {
	code   string
	denied bool
}

func receiveCLILogin(ctx context.Context, l net.Listener, state, issuer string, open func(context.Context, string) error, authorization string) (string, error) {
	result := make(chan loginCode, 1)
	var consumed atomic.Bool
	server := &http.Server{ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 8 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		peer, _, e := net.SplitHostPort(r.RemoteAddr)
		if e != nil || net.ParseIP(peer) == nil || !net.ParseIP(peer).IsLoopback() || r.Method != "GET" || r.Host != l.Addr().String() || r.URL.Path != "/callback" || len(r.URL.RawQuery) > 16<<10 {
			w.WriteHeader(400)
			return
		}
		q, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil || len(q) > 8 {
			w.WriteHeader(400)
			return
		}
		for k, v := range q {
			if len(v) != 1 || len(v[0]) > 4096 {
				w.WriteHeader(400)
				return
			}
			switch k {
			case "state", "code", "iss", "error", "error_description", "error_uri", "session_state":
			default:
				w.WriteHeader(400)
				return
			}
		}
		if !constantLoginState(q.Get("state"), state) || q.Has("iss") && q.Get("iss") != issuer || q.Get("code") == "" && q.Get("error") == "" || q.Get("code") != "" && q.Get("error") != "" {
			w.WriteHeader(400)
			return
		}
		if !consumed.CompareAndSwap(false, true) {
			w.WriteHeader(409)
			return
		}
		result <- loginCode{q.Get("code"), q.Get("error") != ""}
		_, _ = io.WriteString(w, "Login response received. You may close this window.\n")
	})
	done := make(chan struct{})
	go func() { _ = server.Serve(l); close(done) }()
	defer func() { _ = server.Close(); <-done }()
	// The bounded listener is active before the browser can reach its redirect.
	if e := open(ctx, authorization); e != nil {
		return "", e
	}
	select {
	case r := <-result:
		if r.denied {
			return "", errors.New("provider denied login")
		}
		return r.code, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func constantLoginState(got, want string) bool {
	return clioidc.RandomProof(got) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// A separate OP client has no API cookies, bearer, credentials, proxy inheritance,
// redirect following, HTTP2/reused-connection replay, or auth-style probing.
func newCLIOPClient(caFile string) (*http.Client, error) {
	roots, e := x509.SystemCertPool()
	if e != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		// #nosec G304 -- explicit local trust-file flag, never an HTTP-provided path.
		b, e := os.ReadFile(caFile)
		if e != nil || len(b) > 2<<20 || !roots.AppendCertsFromPEM(b) {
			return nil, clioidc.ErrInvalid
		}
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, MaxResponseHeaderBytes: 16 << 10, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type publicLoginTokens struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

func (publicLoginTokens) String() string     { return "private public-client token proof" }
func (p publicLoginTokens) GoString() string { return p.String() }
func publicLoginPost(ctx context.Context, c *http.Client, target string, form url.Values, out any) error {
	status, b, e := publicLoginResponse(ctx, c, target, form)
	defer clear(b)
	if e != nil || status != http.StatusOK || decodePublicLoginJSON(b, out) != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return clioidc.ErrInvalid
	}
	return nil
}

// Each call is one explicit protocol operation. Only the device polling caller
// may interpret a pre-response timeout under RFC8628; other callers refuse it.
func publicLoginResponse(ctx context.Context, c *http.Client, target string, form url.Values) (int, []byte, error) {
	if !clioidc.HTTPS(target) {
		return 0, nil, clioidc.ErrInvalid
	}
	r, e := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(form.Encode()))
	if e != nil {
		return 0, nil, clioidc.ErrInvalid
	}
	r.GetBody = nil
	r.Close = true
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "application/json")
	resp, e := c.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		var n net.Error
		if errors.As(e, &n) && n.Timeout() {
			return 0, nil, errDeviceConnectionTimeout
		}
		return 0, nil, clioidc.ErrInvalid
	}
	defer func() { _ = resp.Body.Close() }()
	media, _, e := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if e != nil || media != "application/json" || resp.ContentLength > 64<<10 {
		return 0, nil, clioidc.ErrInvalid
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	// A body-read failure may follow successful grant consumption. It is terminal.
	if e != nil || len(b) > 64<<10 {
		clear(b)
		return 0, nil, clioidc.ErrInvalid
	}
	return resp.StatusCode, b, nil
}

// Reject duplicate top-level members before decoding consumed protocol fields.
// Unknown extension members remain compatible; the bounded body caps nesting.
func decodePublicLoginJSON(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	first, e := d.Token()
	if e != nil || first != json.Delim('{') {
		return clioidc.ErrInvalid
	}
	seen := map[string]bool{}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return clioidc.ErrInvalid
		}
		name, ok := key.(string)
		if !ok || seen[name] || len(seen) >= 64 {
			return clioidc.ErrInvalid
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return clioidc.ErrInvalid
		}
	}
	if _, e = d.Token(); e != nil || d.Decode(new(any)) != io.EOF {
		return clioidc.ErrInvalid
	}
	if json.Unmarshal(b, out) != nil {
		return clioidc.ErrInvalid
	}
	return nil
}
func cliAuthorization(p clioidc.Preparation, q clioidc.PrepareRequest, state, verifier string) (string, error) {
	if !clioidc.HTTPS(p.Issuer) || !clioidc.HTTPS(p.AuthorizationURL) || !clioidc.HTTPS(p.TokenURL) || p.ClientID == "" || p.Receipt == "" || len(p.Scopes) > 64 {
		return "", clioidc.ErrInvalid
	}
	u, e := url.Parse(p.AuthorizationURL)
	if e != nil {
		return "", clioidc.ErrInvalid
	}
	values, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return "", clioidc.ErrInvalid
	}
	challenge, e := browseroidc.S256(verifier)
	if e != nil {
		return "", e
	}
	for k, v := range map[string]string{"response_type": "code", "client_id": p.ClientID, "redirect_uri": q.RedirectURI, "state": state, "nonce": q.Nonce, "code_challenge": challenge, "code_challenge_method": "S256", "scope": strings.Join(p.Scopes, " ")} {
		if values.Has(k) {
			return "", clioidc.ErrInvalid
		}
		values.Set(k, v)
	}
	u.RawQuery = values.Encode()
	return u.String(), nil
}
func (o *cliOpts) browserLogin(cmd *cobra.Command, r cli.Resolved, options loginOIDCOptions) (string, *apiclient.Me, time.Time, error) {
	if !clioidc.HTTPS(r.Server) || r.InsecureSkipVerify {
		return "", nil, time.Time{}, errors.New("OIDC login requires an HTTPS API with verified TLS")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), clioidc.Lifetime)
	defer cancel()
	c, id, e := o.selectCLILogin(ctx, r, options)
	if e != nil {
		return "", nil, time.Time{}, e
	}
	l, e := listenCLILogin(ctx)
	if e != nil {
		return "", nil, time.Time{}, e
	}
	defer func() { _ = l.Close() }()
	q := clioidc.PrepareRequest{Mode: "browser", Nonce: randomLoginProof(), RedirectURI: "http://" + l.Addr().String() + "/callback"}
	p, e := c.PrepareCLILogin(ctx, id, q)
	if e != nil {
		return "", nil, time.Time{}, loginStageError("prepare OIDC login", e)
	}
	if !p.ExpiresAt.After(o.now()) || p.ExpiresAt.After(o.now().Add(clioidc.Lifetime)) || p.Provider != id {
		return "", nil, time.Time{}, clioidc.ErrInvalid
	}
	ctx, deadline := context.WithDeadline(ctx, p.ExpiresAt)
	defer deadline()
	op, e := newCLIOPClient(options.caFile)
	if e != nil {
		return "", nil, time.Time{}, loginStageError("configure OP TLS", e)
	}
	defer op.CloseIdleConnections()
	state, verifier := randomLoginProof(), randomLoginProof()
	authorization, e := cliAuthorization(p, q, state, verifier)
	if e != nil {
		return "", nil, time.Time{}, e
	}
	code, e := receiveCLILogin(ctx, l, state, p.Issuer, options.open, authorization)
	if e != nil {
		return "", nil, time.Time{}, loginStageError("receive OIDC callback", e)
	}
	var tokens publicLoginTokens
	e = publicLoginPost(ctx, op, p.TokenURL, url.Values{"grant_type": {"authorization_code"}, "client_id": {p.ClientID}, "code": {code}, "redirect_uri": {q.RedirectURI}, "code_verifier": {verifier}}, &tokens)
	if e != nil || tokens.IDToken == "" || tokens.Error != "" {
		return "", nil, time.Time{}, loginStageError("exchange OIDC code", e)
	}
	return o.completeCLILogin(ctx, c, id, p, tokens)
}
func (o *cliOpts) completeCLILogin(ctx context.Context, c *apiclient.Client, id string, p clioidc.Preparation, tokens publicLoginTokens) (string, *apiclient.Me, time.Time, error) {
	c.SetToken(tokens.IDToken)
	verified, e := c.VerifyCLILogin(ctx, id, clioidc.VerifyRequest{Receipt: p.Receipt, AccessToken: tokens.AccessToken})
	if e != nil {
		return "", nil, time.Time{}, loginStageError("verify OIDC proof", e)
	}
	if ctx.Err() != nil || !o.now().Before(verified.ExpiresAt) {
		return "", nil, time.Time{}, clioidc.ErrInvalid
	}
	return tokens.IDToken, &verified.Me, verified.ExpiresAt, nil
}

func (o *cliOpts) selectCLILogin(ctx context.Context, r cli.Resolved, options loginOIDCOptions) (*apiclient.Client, string, error) {
	if !clioidc.HTTPS(r.Server) || r.InsecureSkipVerify {
		return nil, "", errors.New("OIDC login requires an HTTPS API with verified TLS")
	}
	c, e := o.newClient(r)
	if e != nil {
		return nil, "", loginStageError("configure OIDC API transport", e)
	}
	providers, e := c.CLILoginProviders(ctx)
	if e != nil {
		return nil, "", loginStageError("select OIDC provider", e)
	}
	eligible := []string{}
	for _, p := range providers {
		if options.device && p.Device || !options.device && p.Browser {
			eligible = append(eligible, p.ID)
		}
	}
	id := options.provider
	if id == "" && len(eligible) == 1 {
		id = eligible[0]
	}
	for _, candidate := range eligible {
		if candidate == id {
			return c, id, nil
		}
	}
	return nil, "", errors.New("select an advertised public CLI provider with --provider; configure CLIClientID and support for the selected flow")
}
