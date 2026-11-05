// Package webhook implements the captured transport for Tier-B integrations.
// It performs no election, source publication, or ownership acknowledgment.
package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

const (
	maxBody          = 32 << 20
	externalMedia    = "application/external.dns.webhook+json;version=1"
	integrationMedia = "application/kudurru.integration+json;version=1"
)

var (
	errInput    = errors.New("webhook: invalid captured input")
	errProtocol = errors.New("webhook: invalid protocol response")
)

// transportConfig contains detached values supplied by the lifecycle adapter's
// named credential resolver. The transport never opens a credential file or
// reads environment variables. Exactly one authentication mode is required.
type transportConfig struct {
	endpoint                     string
	timeout                      time.Duration
	bearer, ca, certificate, key []byte
}

type transport struct {
	base    url.URL
	host    host.RequestHost
	client  *http.Client
	timeout time.Duration
	token   string
	life    context.Context
	cancel  context.CancelFunc
	stop    sync.Once
}

func newTransport(h host.RequestHost, c transportConfig) (*transport, error) {
	if h == nil || len(c.endpoint) == 0 || len(c.endpoint) > 2048 {
		return nil, errInput
	}
	u, err := url.Parse(c.endpoint)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || u.EscapedPath() != u.Path || strings.ContainsAny(u.Path, "\\\x00\r\n") {
		return nil, errInput
	}
	ip, _ := netip.ParseAddr(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || !ip.IsLoopback()) {
		return nil, errInput
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, errInput
		}
	}
	if c.timeout == 0 {
		c.timeout = 10 * time.Second
	}
	if c.timeout <= 0 || c.timeout > 2*time.Minute || len(c.bearer) > 16<<10 || len(c.ca) > 1<<20 || len(c.certificate) > 1<<20 || len(c.key) > 1<<20 {
		return nil, errInput
	}
	hasBearer, hasCert, hasKey := len(c.bearer) != 0, len(c.certificate) != 0, len(c.key) != 0
	if hasCert != hasKey || hasBearer == hasCert || u.Scheme != "https" && hasCert {
		return nil, errInput
	}
	token := string(c.bearer)
	if hasBearer && !validBearer(token) {
		return nil, errInput
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(c.ca) != 0 {
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(c.ca) {
			return nil, errInput
		}
	}
	if hasCert {
		pair, err := tls.X509KeyPair(c.certificate, c.key)
		if err != nil {
			return nil, errInput
		}
		tlsConfig.Certificates = []tls.Certificate{pair}
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	base := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: c.timeout}).DialContext,
		TLSClientConfig: tlsConfig, TLSHandshakeTimeout: c.timeout,
		ResponseHeaderTimeout: c.timeout, MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives: true, DisableCompression: true, Protocols: protocols,
	}
	life, cancel := context.WithCancel(context.Background())
	return &transport{base: *u, host: h, timeout: c.timeout, token: token, life: life, cancel: cancel,
		client: &http.Client{Transport: base, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func validBearer(s string) bool {
	if s == "" {
		return false
	}
	padding := false
	for _, c := range s {
		if c == '=' {
			padding = true
			continue
		}
		if padding {
			return false
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("-._~+/", c):
		default:
			return false
		}
	}
	return s[0] != '='
}

func (t *transport) close() {
	if t != nil {
		t.stop.Do(func() { t.cancel(); t.client.CloseIdleConnections() })
	}
}

type response struct {
	body    []byte
	status  int
	media   string
	partial bool // explicit pagination is unsupported by complete-list adapters
}

// request is exactly one charged HTTP attempt. Redirects, body replay, ambient
// proxies/cookies and reused HTTP connections are disabled. A 2xx response is
// only wire evidence; the caller must separately validate its exact contract.
func (t *transport) request(ctx context.Context, method, path, media string, body []byte, headers http.Header) (response, error) {
	if t == nil || ctx == nil || len(body) > maxBody || (media != "" && media != externalMedia && media != integrationMedia) || method == http.MethodPost && media == "" || !validPath(method, path) {
		return response{}, errInput
	}
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	if t.life.Err() != nil {
		return response{}, host.ErrUnavailable
	}
	for key, values := range headers {
		if key != "X-Kudurru-Signature" && key != "X-Kudurru-Delivery" || len(values) != 1 || len(values[0]) > 256 || strings.ContainsAny(values[0], "\r\n\x00") {
			return response{}, errInput
		}
	}
	body = slices.Clone(body)
	headers = headers.Clone()
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	stop := context.AfterFunc(t.life, cancel)
	defer stop()
	var out response
	mutation := method == http.MethodPost && path != "/adjustendpoints"
	err := t.host.Request(ctx, "webhook", "all", mutation, func(bound context.Context) error {
		// The callback must retain the original operation lifetime even if a
		// future host implementation accidentally supplies a detached context.
		if bound == nil {
			return host.ErrFenced
		}
		deadline, ok := bound.Deadline()
		original, _ := ctx.Deadline()
		if !ok || deadline.After(original) {
			return host.ErrFenced
		}
		bound, release := context.WithCancel(bound)
		defer release()
		stopOriginal := context.AfterFunc(ctx, release)
		defer stopOriginal()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := bound.Err(); err != nil {
			return err
		}
		u := t.base
		u.Path = strings.TrimRight(u.Path, "/") + path
		var reader io.Reader
		if body != nil {
			reader = io.LimitReader(bytes.NewReader(body), int64(len(body)))
		}
		req, err := http.NewRequestWithContext(bound, method, u.String(), reader)
		if err != nil {
			return errInput
		}
		req.Close = true
		if body != nil {
			req.ContentLength = int64(len(body))
			req.Header.Set("Content-Type", media)
		}
		req.Header.Set("Accept", media)
		if media == "" {
			req.Header.Set("Accept", integrationMedia+", "+externalMedia)
		}
		if t.token != "" {
			req.Header.Set("Authorization", "Bearer "+t.token)
		}
		for key, values := range headers {
			req.Header[key] = append([]string(nil), values...)
		}
		resp, err := t.client.Do(req)
		if err != nil {
			return transportError(bound, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return statusError(resp.StatusCode, resp.Header.Get("Retry-After"))
		}
		if resp.ContentLength > maxBody || resp.Header.Get("Content-Encoding") != "" || len(resp.Header.Values("Content-Type")) > 1 {
			return errProtocol
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return transportError(bound, err)
		}
		if len(b) > maxBody {
			return errProtocol
		}
		if err := bound.Err(); err != nil {
			return err
		}
		partial := false
		for _, name := range []string{"Link", "Content-Range", "X-Next-Page", "X-Next-Cursor", "X-Continuation-Token", "X-Page", "X-Page-Count", "X-Total-Count"} {
			partial = partial || len(resp.Header.Values(name)) != 0
		}
		out = response{body: b, status: resp.StatusCode, media: parseMedia(resp.Header.Get("Content-Type")), partial: partial}
		return nil
	})
	if ctx.Err() != nil {
		return response{}, ctx.Err()
	}
	if err != nil {
		return response{}, err
	}
	return out, nil
}

func validPath(method, path string) bool {
	switch method {
	case http.MethodGet:
		return path == "/" || path == "/records" || path == "/clients" || path == "/networks" || path == "/actions"
	case http.MethodPost:
		return path == "/records" || path == "/adjustendpoints" || path == "/events" || strings.HasPrefix(path, "/actions/") && actionName(strings.TrimPrefix(path, "/actions/"))
	}
	return false
}

func actionName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func parseMedia(header string) string {
	typ, params, err := mime.ParseMediaType(header)
	if err != nil || len(params) != 1 || params["version"] != "1" {
		return ""
	}
	switch typ {
	case "application/external.dns.webhook+json":
		return externalMedia
	case "application/kudurru.integration+json":
		return integrationMedia
	}
	return ""
}

func statusError(code int, retry string) error {
	f := &host.Failure{Kind: host.FailurePermanent}
	if code >= 500 && code <= 599 {
		f.Kind = host.FailureTemporary
		if seconds, err := strconv.ParseUint(retry, 10, 32); err == nil {
			f.RetryAfter = time.Duration(min(seconds, 900)) * time.Second
		}
	}
	return f
}

func transportError(ctx context.Context, _ error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return &host.Failure{Kind: host.FailureTemporary}
}
