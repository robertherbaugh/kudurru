// Package apiclient is the hand-written Go client for the kudurru REST API
// (/api/v1, dev-docs/design-record.md §10). It is the transport the CLI uses.
//
// The surface is deliberately small so that a client generated from
// /api/v1/openapi.json can replace the transport underneath without the
// CLI changing: one request primitive (Do, Raw), the error type (*Error),
// list pagination (ListAll, Pages), the SSE subscription (Subscribe) and a
// handful of typed helpers for the auth flow (see auth.go).
//
// Authentication is either a bearer token (an API token kdr_<id>_<secret>
// or a JWT) or the session cookie set by POST /auth/login, in which case the
// client sends the X-Kudurru-CSRF header on mutating requests.
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BasePath is the API prefix every relative path is joined to.
const BasePath = "/api/v1"

// CSRFHeader carries the CSRF token on cookie-authenticated writes.
const CSRFHeader = "X-Kudurru-CSRF"

// MaxErrorBody caps how much of an error body is read.
const maxErrorBody = 1 << 20

// Client talks to one kudurru API server. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string

	mu    sync.RWMutex
	token string
	csrf  string
}

// Options configure New.
type Options struct {
	// Token is sent as "Authorization: Bearer <token>" when set.
	Token string
	// InsecureSkipVerify disables TLS verification (lab use only).
	InsecureSkipVerify bool
	// CAFile is a PEM bundle trusted in addition to the system roots.
	CAFile string
	// Timeout bounds every non-streaming request (default 30 s). SSE
	// subscriptions are bounded only by their context.
	Timeout time.Duration
	// UserAgent defaults to "kudurru-cli".
	UserAgent string
	// HTTPClient replaces the transport entirely (tests); Timeout, CAFile
	// and InsecureSkipVerify are then ignored.
	HTTPClient *http.Client
}

// New returns a client for server, an http(s) URL such as
// https://dns.example or http://127.0.0.1:8080. A path in the URL is kept
// as a prefix (for servers behind a path-prefix proxy).
func New(server string, opts Options) (*Client, error) {
	if server == "" {
		return nil, errors.New("apiclient: no server URL (use --server, KUDURRU_SERVER or kudurru login)")
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("apiclient: server URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("apiclient: server URL scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("apiclient: server URL %q has no host", server)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""

	hc := opts.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.InsecureSkipVerify} //nolint:gosec // opt-in flag for lab servers with self-signed certificates
		if opts.CAFile != "" {
			pem, err := os.ReadFile(opts.CAFile)
			if err != nil {
				return nil, fmt.Errorf("apiclient: caFile: %w", err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("apiclient: caFile %s holds no PEM certificates", opts.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
		tr.TLSClientConfig = tlsCfg
		jar, _ := cookiejar.New(nil)
		timeout := opts.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		hc = &http.Client{Transport: tr, Jar: jar, Timeout: timeout}
	} else if hc.Jar == nil {
		// Copy so the caller's client is not mutated; the login flow needs
		// a cookie jar.
		cp := *hc
		cp.Jar, _ = cookiejar.New(nil)
		hc = &cp
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "kudurru-cli"
	}
	return &Client{base: u, http: hc, userAgent: ua, token: opts.Token}, nil
}

// Server returns the server URL the client was built for.
func (c *Client) Server() string { return c.base.String() }

// SetToken replaces the bearer token (empty removes it).
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

// Token returns the bearer token in use.
func (c *Client) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// SetCSRF sets the CSRF token sent on mutating cookie-authenticated
// requests (the csrf field of GET /auth/me).
func (c *Client) SetCSRF(csrf string) {
	c.mu.Lock()
	c.csrf = csrf
	c.mu.Unlock()
}

// URL resolves an API path. Paths starting with "/api/" are taken as is;
// any other path is joined to /api/v1. The path must already be escaped
// (use Seg for user-supplied segments); an inline "?query" is merged with
// query.
func (c *Client) URL(path string, query url.Values) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasPrefix(path, "/api/") {
		path = BasePath + path
	}
	p, inline, _ := strings.Cut(path, "?")
	q := url.Values{}
	if inline != "" {
		if parsed, err := url.ParseQuery(inline); err == nil {
			for k, vs := range parsed {
				q[k] = append(q[k], vs...)
			}
		}
	}
	for k, vs := range query {
		q[k] = append(q[k], vs...)
	}
	s := strings.TrimSuffix(c.base.String(), "/") + p
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// Seg escapes one path segment (a zone, namespace or object name).
func Seg(s string) string { return url.PathEscape(s) }

// Request is one API call for Raw.
type Request struct {
	Method string
	// Path is relative to /api/v1 unless it starts with /api/.
	Path  string
	Query url.Values
	// Body is sent as is; ContentType defaults to application/json when
	// Body is non-nil.
	Body        io.Reader
	ContentType string
	Accept      string
	Header      http.Header
	// NoTimeout runs the request with no client-side timeout (SSE).
	NoTimeout bool
}

// Raw sends req and returns the response without interpreting it. The
// caller closes the body. Transport errors are returned; HTTP errors are
// not (see CheckResponse).
func (c *Client) Raw(ctx context.Context, req Request) (*http.Response, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	hr, err := http.NewRequestWithContext(ctx, method, c.URL(req.Path, req.Query), req.Body)
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	if req.Body != nil {
		ct := req.ContentType
		if ct == "" {
			ct = "application/json"
		}
		hr.Header.Set("Content-Type", ct)
	}
	accept := req.Accept
	if accept == "" {
		accept = "application/json"
	}
	if hr.Header.Get("Accept") == "" {
		hr.Header.Set("Accept", accept)
	}
	hr.Header.Set("User-Agent", c.userAgent)
	c.mu.RLock()
	token, csrf := c.token, c.csrf
	c.mu.RUnlock()
	if token != "" && hr.Header.Get("Authorization") == "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	if csrf != "" && mutating(method) {
		hr.Header.Set(CSRFHeader, csrf)
	}
	hc := c.http
	if req.NoTimeout && hc.Timeout != 0 {
		cp := *hc
		cp.Timeout = 0
		hc = &cp
	}
	return hc.Do(hr)
}

func mutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// Do sends a JSON request and decodes a JSON response into out (nil
// discards the body). in is marshalled as JSON unless it is an io.Reader
// or []byte, which are sent verbatim as application/json. Any non-2xx
// response is returned as *Error.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	req := Request{Method: method, Path: path, Query: query}
	if in != nil {
		switch v := in.(type) {
		case io.Reader:
			req.Body = v
		case []byte:
			req.Body = bytes.NewReader(v)
		default:
			b, err := json.Marshal(in)
			if err != nil {
				return fmt.Errorf("apiclient: encode %s %s: %w", method, path, err)
			}
			req.Body = bytes.NewReader(b)
		}
	}
	resp, err := c.Raw(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := CheckResponse(resp); err != nil {
		return err
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err := io.Copy(w, resp.Body)
		return err
	}
	if b, ok := out.(*[]byte); ok {
		*b, err = io.ReadAll(resp.Body)
		return err
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("apiclient: decode %s %s: %w", method, path, err)
	}
	return nil
}

// Get is Do with GET.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.Do(ctx, http.MethodGet, path, query, nil, out)
}

// Post is Do with POST.
func (c *Client) Post(ctx context.Context, path string, query url.Values, in, out any) error {
	return c.Do(ctx, http.MethodPost, path, query, in, out)
}

// Delete is Do with DELETE.
func (c *Client) Delete(ctx context.Context, path string, query url.Values) error {
	return c.Do(ctx, http.MethodDelete, path, query, nil, nil)
}

// Missing names the permission a 403 lacked.
type Missing struct {
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
	Scope    string `json:"scope"`
}

// Error is a non-2xx API response: {error:{key,message,hint}, took_ms}.
type Error struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Key is the stable machine key: invalid, unauthenticated, forbidden,
	// not_found, conflict, managed_by, read_only, host_only, ...
	Key string
	// Message is human readable; for 400 it names the field path.
	Message string
	// Hint is the free-text hint; for managed_by it is the owner (e.g.
	// file:objects.d/zones.yaml).
	Hint string
	// Missing is set on 403 when the hint carries {resource,verb,scope}.
	Missing *Missing
	// RetryAfter is the parsed Retry-After header of a 429 or 503.
	RetryAfter time.Duration
	// TookMS is the server's took_ms.
	TookMS float64
}

// Error formats "key: message (hint)".
func (e *Error) Error() string {
	var b strings.Builder
	key := e.Key
	if key == "" {
		key = strings.ToLower(strings.ReplaceAll(http.StatusText(e.StatusCode), " ", "_"))
	}
	if key == "" {
		key = strconv.Itoa(e.StatusCode)
	}
	b.WriteString(key)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	switch {
	case e.Missing != nil:
		fmt.Fprintf(&b, " (missing %s.%s on %s)", e.Missing.Resource, e.Missing.Verb, orCluster(e.Missing.Scope))
	case e.Key == "managed_by" && e.Hint != "":
		fmt.Fprintf(&b, " (managed by %s)", e.Hint)
	case e.Hint != "":
		fmt.Fprintf(&b, " (hint: %s)", e.Hint)
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, " (retry after %s)", e.RetryAfter)
	}
	return b.String()
}

func orCluster(s string) string {
	if s == "" {
		return "cluster"
	}
	return s
}

// IsKey reports whether err is an *Error with the given key.
func IsKey(err error, key string) bool {
	var e *Error
	return errors.As(err, &e) && e.Key == key
}

// IsStatus reports whether err is an *Error with the given HTTP status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == status
}

// errorEnvelope is the wire form of an error body. Hint is raw because the
// 403 hint carries an object ({missing:{...}}) while others carry a string.
type errorEnvelope struct {
	Error *struct {
		Key     string          `json:"key"`
		Message string          `json:"message"`
		Hint    json.RawMessage `json:"hint"`
		Missing *Missing        `json:"missing"`
	} `json:"error"`
	TookMS float64 `json:"took_ms"`
}

// CheckResponse returns nil for 2xx and an *Error otherwise. It reads (and
// does not close) the body of an error response.
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := &Error{StatusCode: resp.StatusCode}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			e.RetryAfter = time.Until(t).Round(time.Second)
		}
	}
	var env errorEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		e.Key, e.Message, e.TookMS = env.Error.Key, env.Error.Message, env.TookMS
		e.Missing = env.Error.Missing
		decodeHint(e, env.Error.Hint)
		return e
	}
	// Not the envelope (a proxy, a 404 from the wrong server): keep the
	// first line of the body as the message.
	msg := strings.TrimSpace(string(body))
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		msg = resp.Status
	}
	e.Message = msg
	return e
}

func decodeHint(e *Error, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		e.Hint = s
		// A hint may itself be a JSON object encoded as a string.
		if strings.HasPrefix(strings.TrimSpace(s), "{") {
			decodeHintObject(e, json.RawMessage(s))
		}
		return
	}
	decodeHintObject(e, raw)
	if e.Hint == "" && e.Missing == nil {
		e.Hint = string(raw)
	}
}

func decodeHintObject(e *Error, raw json.RawMessage) {
	var obj struct {
		Missing *Missing `json:"missing"`
		Owner   string   `json:"owner"`
		Hint    string   `json:"hint"`
		Message string   `json:"message"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	if obj.Missing != nil && e.Missing == nil {
		e.Missing = obj.Missing
	}
	switch {
	case obj.Owner != "":
		e.Hint = obj.Owner
	case obj.Hint != "":
		e.Hint = obj.Hint
	case obj.Message != "":
		e.Hint = obj.Message
	case e.Missing != nil:
		e.Hint = ""
	}
}
