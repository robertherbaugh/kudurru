// Package integrationconformance probes an explicitly selected external webhook.
// It grants no Kudurru runtime authority and never retries a remote effect.
package integrationconformance

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
	"os"
	"strings"
	"time"
)

// Wire bounds apply independently to every captured input and response.
const (
	MaxBody  = 32 << 20
	MaxItems = 4096
	// MaxRecordItems bounds one GET /records list, as the webhook publisher
	// bounds it: the planner's 10,000 record sets, a registry row for each,
	// and room for foreign registries.
	MaxRecordItems   = 32768
	ExternalDNSMedia = "application/external.dns.webhook+json;version=1"
	IntegrationMedia = "application/kudurru.integration+json;version=1"
)

var errInput = errors.New("invalid conformance input")

// Config names only explicit target inputs; no ambient API context is consulted.
// Files are captured once before any protocol request. Mutation bodies are sent
// exactly as read, including whitespace used by the event signature.
type Config struct {
	URL                                          string
	CAFile, BearerFile, CertFile, KeyFile        string
	Timeout, RequestTimeout                      time.Duration
	AdjustFile, ChangesFile, EventFile, HMACFile string
	// ExpectFile names a read-back expectation (D380): endpoints the record
	// list must contain and endpoints it must not. It is a read probe and
	// needs no AllowMutations.
	ExpectFile         string
	Action, ParamsFile string
	AllowMutations     bool
}

type runner struct {
	base                                    *url.URL
	client                                  *http.Client
	token                                   string
	requestTimeout                          time.Duration
	media                                   string
	adjust, changes, event, hmacKey, params []byte
	expect                                  *expectation
	action                                  string
	explicitAdjust                          bool
	report                                  Report
}

func readFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errInput
	}
	// #nosec G304 -- This is an explicitly selected local operator input, checked
	// as a bounded regular file before and after open; no peer supplies the path.
	f, err := os.Open(path)
	if err != nil {
		return nil, errInput
	}
	defer func() { _ = f.Close() }()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errInput
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if err != nil || len(b) == 0 || int64(len(b)) > limit {
		return nil, errInput
	}
	return b, nil
}

func prepare(ctx context.Context, c Config) (*runner, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.EscapedPath() != u.Path || strings.ContainsAny(u.Path, "\\\x00\r\n") {
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
	if (c.CertFile == "") != (c.KeyFile == "") || c.BearerFile != "" && c.CertFile != "" {
		return nil, errInput
	}
	if !c.AllowMutations && (c.ChangesFile != "" || c.EventFile != "" || c.Action != "" || c.ParamsFile != "" || c.HMACFile != "") {
		return nil, errInput
	}
	if (c.EventFile == "") != (c.HMACFile == "") || (c.Action == "") != (c.ParamsFile == "") || c.Action != "" && !actionName(c.Action) {
		return nil, errInput
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, e := readFile(ctx, c.CAFile, 1<<20)
		if e != nil {
			return nil, e
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(pem) {
			return nil, errInput
		}
	}
	if c.CertFile != "" {
		cert, e := readFile(ctx, c.CertFile, 1<<20)
		if e != nil {
			return nil, e
		}
		key, e := readFile(ctx, c.KeyFile, 1<<20)
		if e != nil {
			return nil, e
		}
		pair, e := tls.X509KeyPair(cert, key)
		if e != nil {
			return nil, errInput
		}
		tlsConfig.Certificates = []tls.Certificate{pair}
	}
	r := &runner{base: u, requestTimeout: c.RequestTimeout, adjust: []byte("[]"), action: c.Action, explicitAdjust: c.AdjustFile != ""}
	if c.BearerFile != "" {
		b, e := readFile(ctx, c.BearerFile, 16<<10)
		if e != nil {
			return nil, e
		}
		r.token = strings.TrimSpace(string(b))
		if r.token == "" || strings.ContainsAny(r.token, " \t\r\n\x00") {
			return nil, errInput
		}
	}
	for _, input := range []struct {
		path  string
		dst   *[]byte
		limit int64
	}{
		{c.AdjustFile, &r.adjust, MaxBody}, {c.ChangesFile, &r.changes, MaxBody},
		{c.EventFile, &r.event, MaxBody}, {c.ParamsFile, &r.params, 64 << 10}, {c.HMACFile, &r.hmacKey, 4096},
	} {
		if input.path != "" {
			b, e := readFile(ctx, input.path, input.limit)
			if e != nil {
				return nil, e
			}
			*input.dst = b
		}
	}
	if c.ExpectFile != "" {
		b, e := readFile(ctx, c.ExpectFile, MaxBody)
		if e != nil {
			return nil, e
		}
		if r.expect = decodeExpectation(b); r.expect == nil {
			return nil, errInput
		}
	}
	if !validEndpoints(r.adjust) || r.changes != nil && !validChanges(r.changes) || r.event != nil && (!validEvent(r.event) || len(r.hmacKey) < 32) || r.params != nil && !jsonObject(r.params) {
		return nil, errInput
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: c.RequestTimeout}).DialContext,
		TLSClientConfig: tlsConfig, TLSHandshakeTimeout: c.RequestTimeout,
		ResponseHeaderTimeout: c.RequestTimeout, MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives: true, DisableCompression: true, Protocols: protocols,
	}
	r.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return r, nil
}

func mediaType(header string) string {
	typ, params, err := mime.ParseMediaType(header)
	if err != nil || len(params) != 1 || params["version"] != "1" {
		return ""
	}
	switch typ {
	case "application/external.dns.webhook+json":
		return ExternalDNSMedia
	case "application/kudurru.integration+json":
		return IntegrationMedia
	default:
		return ""
	}
}

// request makes exactly one attempt. In particular a mutating body has no
// GetBody, redirects are returned, and each attempt uses a fresh HTTP/1 connection.
func (r *runner) request(ctx context.Context, method, path string, body []byte, headers http.Header) ([]byte, int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.requestTimeout)
	defer cancel()
	u := *r.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	var reader io.Reader
	if body != nil {
		reader = io.LimitReader(bytes.NewReader(body), int64(len(body)))
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, 0, "", errInput
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	req.Close = true
	for key, values := range headers {
		req.Header[key] = append([]string(nil), values...)
	}
	req.Header.Set("Accept", r.media)
	if r.media == "" {
		req.Header.Set("Accept", IntegrationMedia+", "+ExternalDNSMedia)
	}
	if body != nil {
		req.Header.Set("Content-Type", r.media)
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, "", errors.New("transport")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, "", errors.New("status")
	}
	if resp.ContentLength > MaxBody {
		return nil, resp.StatusCode, "", errors.New("body_limit")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, resp.StatusCode, "", errors.New("body")
	}
	if len(b) > MaxBody {
		return nil, resp.StatusCode, "", errors.New("body_limit")
	}
	if ctx.Err() != nil {
		return nil, resp.StatusCode, "", errors.New("deadline")
	}
	if len(resp.Header.Values("Content-Type")) > 1 {
		return nil, resp.StatusCode, "", errors.New("media")
	}
	return b, resp.StatusCode, mediaType(resp.Header.Get("Content-Type")), nil
}
