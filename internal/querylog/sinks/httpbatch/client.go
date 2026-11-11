// Package httpbatch is the HTTP client the batch sinks of querylog.sinks
// share (ClickHouse and Loki, D371). It uses the standard library client
// only: one POST per batch, on its own connection, with the credential read
// from a reference and the answer sorted into the three outcomes the sink
// host knows.
//
//   - Stored: a 2xx answer.
//   - May be sent again (querylog.Retryable): no connection was
//     established, so nothing was sent; or the destination answered 408, 429
//     or 5xx. A 5xx is not proof that nothing was stored (a gateway's 502 or
//     504, a ClickHouse insert that failed part way); the repeat is accepted
//     because the documented ClickHouse table and Loki drop the duplicate,
//     and attempts: 1 turns it off. A 401 or 403 is in this group once,
//     when reading the credential reference again returned a different
//     value.
//   - Dropped: every other answer, and a request that was sent and got no
//     answer. Whether that batch was stored is unknown, and an unknown
//     outcome is never sent twice.
//
// Errors name the destination, the status and the transport failure. They
// never contain a credential, the response body or anything from a query.
package httpbatch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

const (
	// maxCredentialBytes bounds a password or token; maxCABytes a bundle.
	// maxCABytes is the bound of the node's credential resolver
	// (internal/credentials.MaxBytes), which refuses a larger value before
	// this client sees it: a caRef names the private CA, not a system bundle.
	maxCredentialBytes = 16 << 10
	maxCABytes         = 64 << 10

	// expectContinueBytes is the body size from which a request asks for
	// "100 Continue" before it sends the body, and expectContinueWait how
	// long it waits for that answer before it sends the body anyway (a
	// server that ignores the header). A destination that refuses a request
	// at its headers (a wrong password, 429, 503) without "100 Continue"
	// then never gets a body it would not read; one that sends "100
	// Continue" first, as ClickHouse does, is handled by answerConn. Small
	// bodies fit the socket buffers and are sent without the round trip.
	expectContinueBytes = 64 << 10
	expectContinueWait  = 2 * time.Second

	// wroteWait bounds how long a success waits for the transport to report
	// that the body was written. It has normally reported before the answer.
	wroteWait = 5 * time.Second
)

// Resolver reads the current value of a credential reference: a Secret key,
// an allowed file or an allowed environment variable. Its errors must not
// contain the value.
type Resolver func(ctx context.Context, ref config.CredentialRef) ([]byte, error)

// Options configures New. Username with Password is HTTP basic
// authentication, Bearer a bearer token; at most one of the two.
type Options struct {
	// Destination names the kind of server in errors and logs, "ClickHouse".
	Destination string
	// URL is the complete request URL. It carries no credential.
	URL *url.URL
	// Header is sent with every request.
	Header http.Header
	// Username, Password and Bearer authenticate the requests.
	Username string
	Password *config.CredentialRef
	Bearer   *config.CredentialRef
	// CA names a PEM bundle that replaces the system trust store.
	CA *config.CredentialRef
	// Gzip compresses request bodies.
	Gzip bool
	// Resolve reads Password, Bearer and CA; required when one is set.
	Resolve Resolver
	Logger  *slog.Logger
	// Detail returns a short description of a refusal from the response
	// headers, or "". It never sees the body.
	Detail func(http.Header) string
	// Now is the clock for Retry-After dates; nil means time.Now.
	Now func() time.Time
}

// StatusError is an answer other than 2xx.
type StatusError struct {
	Destination string
	Status      int
	// Detail is what Options.Detail made of the response headers.
	Detail string
	// Reloaded is set when the destination refused the credential and the
	// reference then held a different value.
	Reloaded bool
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s answered HTTP %d", e.Destination, e.Status)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	if e.Reloaded {
		msg += "; the credential was read again"
	}
	return msg
}

// Client posts batches to one destination. It is used by one goroutine, the
// sink's.
type Client struct {
	opts   Options
	target string // scheme://host, for messages

	http      *http.Client
	transport *http.Transport
	// authorization is the Authorization header value and secret the
	// credential it was made from; neither is ever logged.
	authorization string
	secret        []byte

	gz *gzip.Writer
}

// New checks the options. It does no I/O.
func New(o Options) (*Client, error) {
	switch {
	case o.URL == nil || o.URL.Hostname() == "" || (o.URL.Scheme != "http" && o.URL.Scheme != "https") || o.URL.User != nil:
		return nil, errors.New("httpbatch: an http or https URL without user information is required")
	case o.Bearer != nil && (o.Password != nil || o.Username != ""):
		return nil, errors.New("httpbatch: basic and bearer authentication exclude each other")
	case o.Password != nil && o.Username == "":
		return nil, errors.New("httpbatch: a password needs a username")
	case (o.Password != nil || o.Bearer != nil || o.CA != nil) && o.Resolve == nil:
		return nil, errors.New("httpbatch: credential references need a resolver")
	}
	if o.Destination == "" {
		o.Destination = "destination"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{opts: o, target: o.URL.Scheme + "://" + o.URL.Host}, nil
}

// Open reads the credential and the CA bundle and builds the transport. It
// contacts nothing but the credential source, so a destination that is down
// does not keep a sink from opening; an unreadable reference does, and the
// host tries again.
func (c *Client) Open(ctx context.Context) error {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ClientSessionCache: tls.NewLRUClientSessionCache(2)}
	if c.opts.CA != nil {
		pem, err := c.opts.Resolve(ctx, *c.opts.CA)
		if err != nil {
			return fmt.Errorf("%s: CA certificates: %w", c.opts.Destination, err)
		}
		pool := x509.NewCertPool()
		if len(pem) > maxCABytes {
			return fmt.Errorf("%s: CA certificates: the reference holds more than %d KiB; name a file with only the private CA", c.opts.Destination, maxCABytes>>10)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("%s: CA certificates: the reference holds no PEM certificate", c.opts.Destination)
		}
		tlsConfig.RootCAs = pool
	}
	if _, err := c.loadCredential(ctx); err != nil {
		return err
	}
	// One connection per batch: with no reused connection, an error before
	// the connection exists means nothing was sent, and every other error
	// means the outcome is unknown. A server closing an idle connection
	// under a request cannot blur the two. The session cache keeps the TLS
	// handshakes short.
	dialer := &answerDialer{dialer: net.Dialer{Timeout: 30 * time.Second}}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	c.transport = &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ExpectContinueTimeout:  expectContinueWait,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		Protocols:              protocols,
	}
	// A redirect is an answer like any other: the credential and the batch
	// are never sent to a second host.
	c.http = &http.Client{Transport: c.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c.authorization != "" && c.opts.URL.Scheme == "http" && !loopback(c.opts.URL.Hostname()) {
		c.opts.Logger.Warn("query log sink sends its credential over plain HTTP", "destination", c.opts.Destination, "url", c.target)
	}
	return nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// loadCredential reads the credential reference and reports whether the
// value differs from the one in use.
func (c *Client) loadCredential(ctx context.Context) (changed bool, err error) {
	ref, what := c.opts.Password, "password"
	if c.opts.Bearer != nil {
		ref, what = c.opts.Bearer, "bearer token"
	}
	if ref == nil {
		if c.opts.Username != "" {
			// A user without a password, as ClickHouse's default user.
			c.authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.opts.Username+":"))
		}
		return false, nil
	}
	value, err := c.opts.Resolve(ctx, *ref)
	if err != nil {
		return false, fmt.Errorf("%s: %s: %w", c.opts.Destination, what, err)
	}
	// Secret files usually end in a newline.
	value = bytes.TrimRight(value, "\r\n")
	if len(bytes.TrimSpace(value)) == 0 || len(value) > maxCredentialBytes {
		return false, fmt.Errorf("%s: %s: the reference holds no usable value", c.opts.Destination, what)
	}
	if bytes.Equal(value, c.secret) {
		return false, nil
	}
	if c.opts.Bearer != nil {
		if !validBearer(value) {
			return false, fmt.Errorf("%s: %s: the reference does not hold a bearer token", c.opts.Destination, what)
		}
		c.authorization = "Bearer " + string(value)
	} else {
		c.authorization = "Basic " + base64.StdEncoding.EncodeToString(append([]byte(c.opts.Username+":"), value...))
	}
	c.secret = value
	return true, nil
}

// validBearer accepts the token68 syntax of RFC 7235, which keeps a token
// out of any other header.
func validBearer(token []byte) bool {
	padding := false
	for i, b := range token {
		switch {
		case b == '=':
			if i == 0 {
				return false
			}
			padding = true
		case padding:
			return false
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', strings.IndexByte("-._~+/", b) >= 0:
		default:
			return false
		}
	}
	return len(token) > 0
}

// Post sends one batch and returns nil when the destination stored it, an
// error marked querylog.Retryable when it did not and the same batch may be
// sent again, and any other error when the batch must be dropped.
func (c *Client) Post(ctx context.Context, body []byte) error {
	if c.http == nil {
		return fmt.Errorf("%s: the sink is not open", c.opts.Destination)
	}
	payload, err := c.payload(body)
	if err != nil {
		return err
	}
	var connected atomic.Bool
	var conn atomic.Pointer[answerConn]
	wrote := make(chan error, 1)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			connected.Store(true)
			conn.Store(unwrapConn(info.Conn))
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			select {
			case wrote <- info.Err:
			default:
			}
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.URL.String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%s: %s: invalid request", c.opts.Destination, c.target)
	}
	for name, values := range c.opts.Header {
		req.Header[name] = values
	}
	if c.opts.Gzip {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if len(payload) >= expectContinueBytes {
		req.Header.Set("Expect", "100-continue")
	}
	if c.authorization != "" {
		req.Header.Set("Authorization", c.authorization)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error repeats the request URL; the cause is enough.
		var wrapped *url.Error
		if errors.As(err, &wrapped) {
			err = wrapped.Err
		}
		if !connected.Load() {
			return querylog.Retryable(fmt.Errorf("%s: no connection to %s, nothing was sent: %w", c.opts.Destination, c.target, err))
		}
		return fmt.Errorf("%s: %s gave no answer after the batch was sent, whether it was stored is unknown: %w", c.opts.Destination, c.target, err)
	}
	// The status is the answer. The body is never read: it can quote the
	// batch, and the connection is not reused.
	_ = resp.Body.Close()
	status := resp.StatusCode
	if status >= 200 && status < 300 {
		// The transport returns an answer that arrives while the body is
		// still being written. A success is believed only for a body that
		// was written whole on a connection the destination did not reset:
		// closing the response above ends a write still in progress, which
		// then reports its error here.
		var writeErr error
		timer := time.NewTimer(wroteWait)
		select {
		case writeErr = <-wrote:
		case <-timer.C:
			writeErr = errors.New("still writing")
		case <-ctx.Done():
			writeErr = ctx.Err()
		}
		timer.Stop()
		if writeErr != nil || (conn.Load() != nil && conn.Load().cut.Load()) {
			return fmt.Errorf("%s: %s answered HTTP %d before it had the whole batch, whether it was stored is unknown", c.opts.Destination, c.target, status)
		}
		return nil
	}
	refusal := &StatusError{Destination: c.opts.Destination, Status: status}
	if c.opts.Detail != nil {
		refusal.Detail = c.opts.Detail(resp.Header)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// The destination refused the credential, so it stored nothing. A
		// rotated password or token is picked up here: when the reference
		// now holds another value, the batch may be sent again with it.
		if changed, err := c.loadCredential(ctx); err == nil && changed {
			refusal.Reloaded = true
			return querylog.Retryable(refusal)
		}
		return refusal
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return querylog.RetryableAfter(refusal, retryAfter(resp.Header.Get("Retry-After"), c.opts.Now()))
	}
	return refusal
}

// answerDialer dials connections that keep an answer readable when the
// destination resets the connection under the request body.
//
// A destination may answer a request it refuses at its headers (a wrong
// password, 429, 503) and close the connection without reading the rest
// of the body. ClickHouse does so even after it has sent "100 Continue".
// The kernel then refuses the next write with EPIPE or ECONNRESET, while
// the answer already sits in the receive buffer; Go's transport returns
// the write error and never reads it. The batch would be dropped as an
// unknown outcome, a 401 or 403 would not read the credential again and
// the log would name the reset instead of the status. answerConn hides
// such a write error and discards the rest of the body, so the transport
// goes on to read the answer, or fails to read one: a reset connection
// without an answer is still an unknown outcome.
type answerDialer struct{ dialer net.Dialer }

func (d *answerDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := d.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &answerConn{Conn: conn}, nil
}

// answerConn is a connection whose writes, after the peer reset it,
// succeed without sending anything; cut records that it happened.
type answerConn struct {
	net.Conn
	cut atomic.Bool
}

func (c *answerConn) Write(p []byte) (int, error) {
	if c.cut.Load() {
		return len(p), nil
	}
	n, err := c.Conn.Write(p)
	if err != nil && (errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)) {
		c.cut.Store(true)
		return len(p), nil
	}
	return n, err
}

// unwrapConn returns the answerConn under a transport connection, plain or
// TLS; nil for one this client did not dial (a proxy's is still one).
func unwrapConn(conn net.Conn) *answerConn {
	for conn != nil {
		switch c := conn.(type) {
		case *answerConn:
			return c
		case interface{ NetConn() net.Conn }:
			conn = c.NetConn()
		default:
			return nil
		}
	}
	return nil
}

// payload returns the request body: a copy of body, or its gzip form. The
// request owns the result. The transport can still be reading a request
// body after Do has returned with an error, so a buffer the sink or this
// client fills again for the next batch must never be handed to it.
func (c *Client) payload(body []byte) ([]byte, error) {
	if !c.opts.Gzip {
		return bytes.Clone(body), nil
	}
	var buf bytes.Buffer
	buf.Grow(len(body)/8 + 64)
	if c.gz == nil {
		c.gz, _ = gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	} else {
		c.gz.Reset(&buf)
	}
	if _, err := c.gz.Write(body); err != nil {
		return nil, fmt.Errorf("%s: compress: %w", c.opts.Destination, err)
	}
	if err := c.gz.Close(); err != nil {
		return nil, fmt.Errorf("%s: compress: %w", c.opts.Destination, err)
	}
	return buf.Bytes(), nil
}

// retryAfter parses a Retry-After value: seconds or an HTTP date. The sink
// host caps the wait.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// Close releases the transport and forgets the credential.
func (c *Client) Close() {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	clear(c.secret)
	c.secret, c.authorization = nil, ""
}
