package replica

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ClientOptions pins peer trust, local identity and durable authority observation.
type ClientOptions struct {
	Credentials                     *internalapi.Credentials
	Self                            func() Peer
	Observer                        AuthorityObserver
	Tokens                          func() []string
	Timeout                         time.Duration
	MaxCommandBytes, MaxResultBytes int64
	// Transport must preserve TLS verification against the enrolled trust. Tests
	// may substitute it; insecure HTTP URLs and redirects remain rejected.
	Transport http.RoundTripper
}

// Client exchanges bounded images and commands with enrolled HTTPS peers.
type Client struct {
	opts ClientOptions
	http *http.Client
}

// NewClient requires an observer so rejected bodies cannot discard a higher term.
func NewClient(o ClientOptions) (*Client, error) {
	if o.Credentials == nil || o.Self == nil || o.Observer == nil {
		return nil, errors.New("replica: credentials, local member and authority observer required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.MaxCommandBytes <= 0 {
		o.MaxCommandBytes = DefaultMaxCommandBytes
	}
	if o.MaxResultBytes <= 0 {
		o.MaxResultBytes = DefaultMaxResultBytes
	}
	if o.Tokens == nil {
		o.Tokens = func() []string { return []string{o.Credentials.Token()} }
	}
	rt := o.Transport
	if rt == nil {
		d := &net.Dialer{Timeout: o.Timeout, KeepAlive: 30 * time.Second}
		rt = &http.Transport{DialContext: d.DialContext, TLSClientConfig: o.Credentials.ClientTLS(), TLSHandshakeTimeout: o.Timeout, ResponseHeaderTimeout: o.Timeout, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute}
	}
	return &Client{opts: o, http: &http.Client{Transport: rt, Timeout: o.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) address(peer Peer, path string) (string, error) {
	self := c.opts.Self()
	if !validID(peer.MemberID) || !validID(peer.ClusterID) || !validID(self.MemberID) || !validID(self.ClusterID) || peer.ClusterID != self.ClusterID {
		return "", replication.ErrWrongCluster
	}
	if peer.MemberID == self.MemberID {
		return "", ErrLoop
	}
	host, port, err := net.SplitHostPort(peer.Address)
	if err != nil || host == "" || port == "" {
		return "", ErrProtocol
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", ErrProtocol
	}
	u := url.URL{Scheme: "https", Host: peer.Address, Path: path}
	if u.Hostname() != host || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrProtocol
	}
	return u.String(), nil
}
func (c *Client) request(ctx context.Context, peer Peer, method, path string, body []byte, etag string) (*http.Response, error) {
	address, err := c.address(peer, path)
	if err != nil {
		return nil, err
	}
	tokens := c.opts.Tokens()
	if len(tokens) == 0 || len(tokens) > 4 {
		return nil, ErrUnauthorized
	}
	for _, token := range tokens {
		req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
		if err != nil {
			return nil, ErrProtocol
		}
		self := c.opts.Self()
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(HeaderMember, self.MemberID)
		req.Header.Set(HeaderCluster, self.ClusterID)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			var certificate *tls.CertificateVerificationError
			var unknownCA x509.UnknownAuthorityError
			var hostName x509.HostnameError
			if errors.As(err, &certificate) || errors.As(err, &unknownCA) || errors.As(err, &hostName) {
				return nil, ErrUntrusted
			}
			if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
				return nil, ctx.Err()
			}
			return nil, errors.Join(ErrUnavailable, err)
		}
		// A response rejected without returning it still carries authority. Observe
		// authenticated metadata before even draining its body; missing metadata
		// on the pre-authentication 401 cannot fence the local writer.
		redirect := resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified
		if (resp.StatusCode == http.StatusUnauthorized || redirect) && authenticated(resp, peer) {
			if _, err := c.observe(ctx, peer, resp); err != nil {
				closeResponse(resp)
				return nil, &ExchangeError{Reached: true, Err: err}
			}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			closeResponse(resp)
			continue
		}
		if redirect {
			closeResponse(resp)
			return nil, ErrRedirect
		}
		return resp, nil
	}
	return nil, ErrUnauthorized
}
func closeResponse(r *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
	_ = r.Body.Close()
}

// observe authenticates the responder and authority BEFORE any body decode.
func (c *Client) observe(ctx context.Context, peer Peer, r *http.Response) (replication.Term, error) {
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 || len(r.TLS.VerifiedChains) == 0 {
		return replication.Term{}, ErrUntrusted
	}
	if !authenticated(r, peer) {
		return replication.Term{}, ErrUnauthorized
	}
	if len(r.Header.Values(HeaderTerm)) != 1 {
		return replication.Term{}, ErrProtocol
	}
	t, err := decodeTerm(r.Header.Get(HeaderTerm))
	if err != nil {
		return t, err
	}
	if t.ClusterID != peer.ClusterID {
		return t, replication.ErrWrongCluster
	}
	_, err = c.opts.Observer.ObserveAuthority(ctx, t)
	return t, err
}
func peerProblem(r *http.Response) error {
	b, err := readBounded(r.Body, 64<<10)
	if err != nil {
		return err
	}
	var p Problem
	if err := decodeStrict(b, &p); err != nil {
		return ErrProtocol
	}
	return problemError(p.Code)
}

// SourceOptions selects the enrolled primary and the receiver's kind registry.
type SourceOptions struct {
	Resolve    PeerResolver
	Validation replication.Validation
}

// Source adapts authenticated full-image exchanges to follower.Source.
type Source struct {
	client *Client
	opts   SourceOptions
}

// NewSource copies the receiver registry and requires an enrolled peer resolver.
func NewSource(client *Client, o SourceOptions) (*Source, error) {
	if client == nil || o.Resolve == nil || o.Validation.SchemaVersion <= 0 || len(o.Validation.Kinds) == 0 {
		return nil, errors.New("replica: client, enrolled resolver and receiver registry required")
	}
	o.Validation.Kinds = append([]replication.Kind(nil), o.Validation.Kinds...)
	return &Source{client: client, opts: o}, nil
}

var _ follower.Source = (*Source)(nil)

// Fetch observes authenticated authority before decoding even a rejected image.
func (s *Source) Fetch(ctx context.Context, etag string) (follower.Response, error) {
	peer, err := s.opts.Resolve(ctx)
	if err != nil {
		return follower.Response{}, err
	}
	r, err := s.client.request(ctx, peer, http.MethodGet, PathImage, nil, etag)
	if err != nil {
		return follower.Response{}, err
	}
	defer func() { _ = r.Body.Close() }()
	term, err := s.client.observe(ctx, peer, r)
	if err != nil {
		return follower.Response{}, &ExchangeError{Reached: authenticated(r, peer), Err: err}
	}
	reject := func(err error) (follower.Response, error) {
		return follower.Response{}, &ExchangeError{Reached: true, Err: err}
	}
	switch r.StatusCode {
	case http.StatusNotModified:
		if etag == "" || r.Header.Get("ETag") != etag || term.PrimaryID != peer.MemberID {
			return reject(ErrProtocol)
		}
		return follower.Response{Term: term, NotModified: true, ETag: etag}, nil
	case http.StatusOK:
		if term.PrimaryID != peer.MemberID || r.Header.Get("Content-Type") != "application/json" {
			return reject(ErrProtocol)
		}
		limit := s.opts.Validation.Limits.Defaults().MaxBytes
		if r.ContentLength > limit {
			return reject(replication.ErrTooLarge)
		}
		image, err := replication.Decode(r.Body, s.opts.Validation)
		if err != nil {
			return reject(err)
		}
		i := image.Image()
		if !i.Term.Same(term) || r.Header.Get("ETag") != i.Generation().ETag() {
			return reject(ErrProtocol)
		}
		return follower.Response{Term: term, Image: &i, ETag: r.Header.Get("ETag")}, nil
	default:
		return reject(peerProblem(r))
	}
}
func authenticated(r *http.Response, p Peer) bool {
	return r.TLS != nil && r.TLS.Version >= tls.VersionTLS13 && len(r.TLS.VerifiedChains) > 0 && len(r.Header.Values(HeaderMember)) == 1 && len(r.Header.Values(HeaderCluster)) == 1 && r.Header.Get(HeaderMember) == p.MemberID && r.Header.Get(HeaderCluster) == p.ClusterID
}

// Write never automatically retries an ambiguous exchange. Caller retains the
// immutable command ID/body/lifetime and may retry explicitly after promotion.
func (c *Client) Write(ctx context.Context, peer Peer, command Command) (Outcome, error) {
	if IsForwarded(ctx) || sqlite.HasWriterToken(ctx) {
		return Outcome{}, ErrLoop
	}
	now := time.Now()
	if err := command.validate(now, DefaultCommandLifetime); err != nil {
		return Outcome{}, err
	}
	if command.ExpectedTerm.ClusterID != peer.ClusterID {
		return Outcome{}, replication.ErrWrongCluster
	}
	if command.ExpectedTerm.PrimaryID != peer.MemberID {
		return Outcome{}, replication.ErrCAS
	}
	b, err := json.Marshal(command)
	if err != nil {
		return Outcome{}, ErrProtocol
	}
	if int64(len(b)) > c.opts.MaxCommandBytes {
		return Outcome{}, replication.ErrTooLarge
	}
	r, err := c.request(ctx, peer, http.MethodPost, PathWrite, b, "")
	if err != nil {
		if errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Outcome{}, errors.Join(ErrOutcomeUnknown, err)
		}
		return Outcome{}, err
	}
	defer func() { _ = r.Body.Close() }()
	term, err := c.observe(ctx, peer, r)
	if err != nil {
		if r.StatusCode == http.StatusOK {
			err = errors.Join(ErrOutcomeUnknown, err)
		}
		return Outcome{}, &ExchangeError{Reached: authenticated(r, peer), Err: err}
	}
	if r.StatusCode != http.StatusOK {
		return Outcome{}, &ExchangeError{Reached: true, Err: peerProblem(r)}
	}
	if !term.Same(command.ExpectedTerm) || term.PrimaryID != peer.MemberID {
		return Outcome{}, errors.Join(ErrOutcomeUnknown, ErrProtocol)
	}
	if r.ContentLength > c.opts.MaxResultBytes {
		return Outcome{}, errors.Join(ErrOutcomeUnknown, replication.ErrTooLarge)
	}
	b, err = readBounded(r.Body, c.opts.MaxResultBytes)
	if err != nil {
		return Outcome{}, errors.Join(ErrOutcomeUnknown, err)
	}
	var out Outcome
	if err := decodeStrict(b, &out); err != nil {
		return Outcome{}, errors.Join(ErrOutcomeUnknown, err)
	}
	if err := out.validate(c.opts.MaxResultBytes); err != nil {
		return Outcome{}, errors.Join(ErrOutcomeUnknown, err)
	}
	return out, nil
}
