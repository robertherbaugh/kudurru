package internalapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"time"
)

// Client errors.
var (
	ErrUnauthorized   = errors.New("internalapi: peer rejected the token")
	ErrDigestMismatch = errors.New("internalapi: list body does not match its digest")
	ErrTooLarge       = errors.New("internalapi: list body exceeds the size cap")
)

// ClientOptions tune a Client.
type ClientOptions struct {
	// Timeout bounds connecting, the TLS handshake and the response
	// header; bodies are bounded by the request context. Default 10 s.
	Timeout time.Duration
	// MaxListBytes caps a downloaded list; default DefaultMaxListBytes.
	MaxListBytes int64
	// Transport replaces the HTTP transport (tests); its TLS configuration
	// must come from Credentials.ClientTLS.
	Transport http.RoundTripper
}

// Client talks to peers' internal listeners.
type Client struct {
	creds *Credentials
	http  *http.Client
	max   int64
}

// NewClient returns a Client using creds for TLS and the bearer token.
func NewClient(creds *Credentials, opts ClientOptions) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.MaxListBytes <= 0 {
		opts.MaxListBytes = DefaultMaxListBytes
	}
	rt := opts.Transport
	if rt == nil {
		d := &net.Dialer{Timeout: opts.Timeout, KeepAlive: 30 * time.Second}
		rt = &http.Transport{
			DialContext:           d.DialContext,
			TLSClientConfig:       creds.ClientTLS(),
			TLSHandshakeTimeout:   opts.Timeout,
			ResponseHeaderTimeout: opts.Timeout,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	return &Client{creds: creds, http: &http.Client{
		Transport: rt,
		// Membership selects the peer. Redirects must not send its token or
		// a write/hint body to a different endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, max: opts.MaxListBytes}
}

// Status fetches GET /internal/status from addr (host:port).
func (c *Client) Status(ctx context.Context, addr string) (Status, error) {
	resp, err := c.get(ctx, addr, PathStatus)
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var st Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&st); err != nil {
		return Status{}, fmt.Errorf("internalapi: decode status from %s: %w", addr, err)
	}
	return st, nil
}

// ListInfo describes a list body being downloaded.
type ListInfo struct {
	Size   int64
	Source string
}

// OpenList starts downloading the body with the given digest from addr.
// The returned reader verifies the sha256 while it is read: the read that
// reaches the end returns ErrDigestMismatch instead of io.EOF when the body
// does not match, and ErrTooLarge once more than MaxListBytes arrive, so a
// caller that commits only after io.EOF never keeps a bad body. A 404 is
// ErrListNotFound.
func (c *Client) OpenList(ctx context.Context, addr, digest string) (io.ReadCloser, ListInfo, error) {
	if !ValidDigest(digest) {
		return nil, ListInfo{}, fmt.Errorf("internalapi: invalid digest %q", digest)
	}
	resp, err := c.get(ctx, addr, PathLists+digest)
	if err != nil {
		return nil, ListInfo{}, err
	}
	if resp.ContentLength > c.max {
		_ = resp.Body.Close()
		return nil, ListInfo{}, ErrTooLarge
	}
	info := ListInfo{Size: resp.ContentLength, Source: resp.Header.Get(HeaderSource)}
	return &verifyingReader{rc: resp.Body, h: sha256.New(), want: digest, left: c.max}, info, nil
}

func (c *Client) get(ctx context.Context, addr, path string) (*http.Response, error) {
	var last error
	for _, tok := range c.creds.tokensToTry() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusOK:
			return resp, nil
		case http.StatusUnauthorized:
			drain(resp)
			last = ErrUnauthorized
			continue // during a key rotation the peer may know only the other token
		case http.StatusNotFound:
			drain(resp)
			return nil, ErrListNotFound
		default:
			drain(resp)
			return nil, fmt.Errorf("internalapi: GET %s from %s: %s", path, addr, resp.Status)
		}
	}
	return nil, last
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

type verifyingReader struct {
	rc   io.ReadCloser
	h    hash.Hash
	want string
	left int64
	err  error
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	n, err := v.rc.Read(p)
	v.h.Write(p[:n])
	v.left -= int64(n)
	if v.left < 0 {
		v.err = ErrTooLarge
		return n, v.err
	}
	if errors.Is(err, io.EOF) {
		if hex.EncodeToString(v.h.Sum(nil)) != v.want {
			v.err = ErrDigestMismatch
			return n, v.err
		}
		v.err = io.EOF
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.rc.Close() }
