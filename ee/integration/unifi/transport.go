package unifi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
)

type pollBudget struct{ attempts, pages, bytes int }
type responseError struct {
	code    int
	failure *host.Failure
}

func (e *responseError) Error() string { return "unifi: provider response rejected" }
func (e *responseError) Unwrap() error { return e.failure }

func fingerprint(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "sha256:") || len(s) != 71 {
		return nil, ErrInvalid
	}
	b, err := hex.DecodeString(s[7:])
	if err != nil {
		return nil, ErrInvalid
	}
	return b, nil
}
func (p *Provider) httpClient(c public.UniFiIntegration, origin string) (*http.Client, error) {
	transport := p.transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		transport = transport.Clone()
	}
	transport.Proxy = nil
	transport.DisableKeepAlives = true // no transparent reuse retry outside a host loan
	transport.DisableCompression = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	// nosemgrep: problem-based-packs.insecure-transport.go-stdlib.bypass-tls-verification.bypass-tls-verification -- explicit certificate pin replaces CA trust; VerifyConnection still checks the exact fingerprint, hostname and validity period
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if transport.TLSClientConfig != nil {
		config = transport.TLSClientConfig.Clone()
		config.MinVersion = tls.VersionTLS12
	}
	config.InsecureSkipVerify = false
	config.VerifyConnection = nil
	if c.TLSFingerprint != "" {
		pin, err := fingerprint(c.TLSFingerprint)
		if err != nil {
			return nil, err
		}
		u, _ := url.Parse(origin)
		config.InsecureSkipVerify = true // pin replaces CA trust; hostname/time remain mandatory below
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return ErrInvalid
			}
			leaf := state.PeerCertificates[0]
			sum := sha256.Sum256(leaf.Raw)
			now := p.now()
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.VerifyHostname(u.Hostname()) != nil {
				return ErrInvalid
			}
			return nil
		}
	}
	transport.TLSClientConfig = config
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func (p *Provider) get(ctx context.Context, budget *pollBudget, path string) ([]byte, error) {
	return p.getPath(ctx, budget, p.prefix+"/v1"+path)
}

func (p *Provider) getPath(ctx context.Context, budget *pollBudget, path string) ([]byte, error) {
	p.mu.RLock()
	ready := p.host != nil && !p.stopped
	p.mu.RUnlock()
	if !ready {
		return nil, host.ErrUnavailable
	}
	if budget.attempts >= maxAttempts {
		return nil, ErrCapacity
	}
	budget.attempts++
	// Build only the supported read request. Network I/O remains inside the
	// original host loan below; no login, background context or retry is added.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.origin+path, nil)
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("X-API-Key", p.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	var body []byte
	err = p.host.Request(ctx, "unifi", p.bucket, false, func(loan context.Context) error {
		requestCtx, cancel := context.WithCancel(loan)
		defer cancel()
		stop := make(chan struct{})
		go func() {
			select {
			case <-p.stop:
				cancel()
			case <-stop:
			}
		}()
		defer close(stop)
		response, err := p.client.Do(req.Clone(requestCtx))
		if err != nil {
			if requestCtx.Err() != nil {
				return requestCtx.Err()
			}
			return &host.Failure{Kind: host.FailureTemporary}
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return &responseError{response.StatusCode, statusFailure(response.StatusCode, response.Header.Get("Retry-After"), p.now())}
		}
		compressed, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
		if err != nil {
			return &host.Failure{Kind: host.FailureTemporary}
		}
		if len(compressed) > maxResponse {
			return ErrCapacity
		}
		encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
		if encoding != "" && encoding != "identity" && encoding != "gzip" {
			return ErrInvalid
		}
		body = compressed
		if encoding == "gzip" || len(compressed) >= 2 && compressed[0] == 0x1f && compressed[1] == 0x8b {
			reader, err := gzip.NewReader(bytes.NewReader(compressed))
			if err != nil {
				return ErrInvalid
			}
			defer func() { _ = reader.Close() }()
			body, err = io.ReadAll(io.LimitReader(reader, maxResponse+1))
			if err != nil {
				return ErrInvalid
			}
			if len(body) > maxResponse {
				return ErrCapacity
			}
		}
		budget.bytes += len(body)
		if budget.bytes > maxSnapshot {
			return ErrCapacity
		}
		return requestCtx.Err()
	})
	if errors.Is(err, ledger.ErrBudget) {
		return nil, &host.Failure{Kind: host.FailureRateLimited, RetryAfter: 5 * time.Minute}
	}
	return body, err
}

func statusFailure(code int, retry string, now time.Time) *host.Failure {
	kind := host.FailurePermanent
	if code == 429 {
		kind = host.FailureRateLimited
	} else if code == 408 || code == 409 || code >= 500 {
		kind = host.FailureTemporary
	}
	after := time.Duration(0)
	if seconds, err := strconv.ParseUint(retry, 10, 32); err == nil {
		after = time.Duration(min(seconds, 900)) * time.Second
	} else if at, err := http.ParseTime(retry); err == nil {
		after = max(0, min(15*time.Minute, at.Sub(now)))
	}
	return &host.Failure{Kind: kind, RetryAfter: after}
}
