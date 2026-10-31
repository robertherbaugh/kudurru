package externaldns

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

const relayTimeout = 15 * time.Second

// RelayOptions selects one operator-owned webhook. Namespace, ownership and
// mutation admission remain with that existing server.
type RelayOptions struct {
	Upstream, TokenFile, CAFile string
}

// Relay exposes only the external-dns protocol on a pod-local listener. It
// injects its startup credential; caller credentials and forwarding headers
// never become upstream authority. A failed operation is never replayed.
type Relay struct {
	upstream *url.URL
	token    string
	tls      *tls.Config
	active   chan struct{}
}

func (r *Relay) String() string { return "externaldns.Relay" }

// NewRelay validates configuration and captures bounded host credential/trust
// files. It makes no network requests. Restart the relay to rotate those files.
func NewRelay(o RelayOptions) (*Relay, error) {
	if len(o.Upstream) > 4096 {
		return nil, errors.New("externaldns relay: upstream URL exceeds bounds")
	}
	u, err := url.Parse(o.Upstream)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.Contains(u.Path, "\\") || strings.Contains(u.Host, "%") {
		return nil, errors.New("externaldns relay: upstream must be a canonical HTTP(S) URL without credentials, query or fragment")
	}
	if u.Path != "" && u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") {
		return nil, errors.New("externaldns relay: upstream path must be canonical")
	}
	ip, parseErr := netip.ParseAddr(u.Hostname())
	loopback := parseErr == nil && ip.Unmap().IsLoopback()
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return nil, errors.New("externaldns relay: upstream requires HTTPS; HTTP is limited to literal loopback development endpoints")
	}
	if o.TokenFile == "" && !loopback {
		return nil, errors.New("externaldns relay: remote upstream requires --upstream-token-file")
	}
	r := &Relay{upstream: u, active: make(chan struct{}, 2), tls: &tls.Config{MinVersion: tls.VersionTLS12}}
	if o.TokenFile != "" {
		raw, err := readRelayFile(o.TokenFile, maxTokenFileBytes)
		defer clear(raw)
		token := bytes.TrimSpace(raw)
		if err != nil || len(token) < 16 || !validBearerToken(token) {
			return nil, errors.New("externaldns relay: invalid upstream token file")
		}
		r.token = string(token)
	}
	if o.CAFile != "" {
		raw, err := readRelayFile(o.CAFile, 2<<20)
		if err != nil {
			return nil, errors.New("externaldns relay: invalid upstream CA file")
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			return nil, errors.New("externaldns relay: system CA trust unavailable")
		}
		if !roots.AppendCertsFromPEM(raw) {
			return nil, errors.New("externaldns relay: invalid upstream CA bundle")
		}
		r.tls.RootCAs = roots
	}
	return r, nil
}

func readRelayFile(name string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: explicit operator-owned file reference, never request input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o444 == 0 || info.Size() > limit {
		return nil, errors.New("invalid relay file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		clear(raw)
		return nil, errors.New("invalid relay file")
	}
	return raw, nil
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	allowed := req.Method == http.MethodGet && (req.URL.Path == "/" || req.URL.Path == "/records") || req.Method == http.MethodPost && (req.URL.Path == "/records" || req.URL.Path == "/adjustendpoints")
	if !allowed || req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.RawPath != "" {
		http.Error(w, "unsupported webhook operation", http.StatusNotFound)
		return
	}
	select {
	case r.active <- struct{}{}:
		defer func() { <-r.active }()
	default:
		http.Error(w, "webhook relay busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), relayTimeout)
	defer cancel()
	// Deadlines cover downstream body reads and writes as well as upstream I/O.
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, MaxBodyBytes))
	if err != nil {
		http.Error(w, "invalid or oversized webhook body", http.StatusRequestEntityTooLarge)
		return
	}
	if req.Method == http.MethodGet && len(body) != 0 {
		http.Error(w, "webhook reads do not accept a body", http.StatusBadRequest)
		return
	}
	u := *r.upstream
	u.Path = strings.TrimRight(u.Path, "/") + req.URL.Path
	out, err := http.NewRequestWithContext(ctx, req.Method, u.String(), io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		http.Error(w, "webhook upstream unavailable", http.StatusBadGateway)
		return
	}
	out.ContentLength = int64(len(body))
	out.Header.Set("Accept", MediaType)
	if req.Method == http.MethodPost {
		out.Header.Set("Content-Type", MediaType)
	}
	if r.token != "" {
		out.Header.Set("Authorization", "Bearer "+r.token)
	}
	// A new HTTP/1 transport per operation has no reused connection to retry.
	// No GetBody, proxy, redirects, HTTP/2 replay or caller headers participate.
	transport := &http.Transport{TLSClientConfig: r.tls.Clone(), DialContext: (&net.Dialer{Timeout: relayTimeout}).DialContext, DisableKeepAlives: true, TLSHandshakeTimeout: relayTimeout, ResponseHeaderTimeout: relayTimeout, MaxResponseHeaderBytes: 16 << 10, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: relayTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(out) //nolint:gosec // G704: fixed operator URL plus one of four literal protocol paths; request data cannot select an origin or query
	if err != nil {
		http.Error(w, "webhook upstream outcome unavailable; request was not replayed", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 && response.StatusCode < 400 {
		http.Error(w, "webhook upstream redirects are refused", http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
	if err != nil || len(data) > MaxBodyBytes || ctx.Err() != nil {
		http.Error(w, "webhook upstream response unavailable; request was not replayed", http.StatusBadGateway)
		return
	}
	if value := response.Header.Get("Content-Type"); value != "" {
		w.Header().Set("Content-Type", value)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(data)
}

// Serve owns a literal loopback listener and joins its server on cancellation.
func (r *Relay) Serve(ctx context.Context, ln net.Listener) error {
	addr, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil || !addr.Addr().Unmap().IsLoopback() {
		_ = ln.Close()
		return errors.New("externaldns relay: listener must be literal loopback")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := &http.Server{Handler: http.NewCrossOriginProtection().Handler(r), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: relayTimeout, WriteTimeout: relayTimeout, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	defer func() { _ = srv.Close() }()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	received := false
	select {
	case <-ctx.Done():
	case err = <-done:
		received = true
	}
	cancel()
	stop, release := context.WithTimeout(context.Background(), 2*time.Second)
	defer release()
	shutdownErr := srv.Shutdown(stop)
	if shutdownErr != nil {
		_ = srv.Close()
	}
	if !received {
		err = <-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, shutdownErr)
}
