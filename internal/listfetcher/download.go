package listfetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/downloadpolicy"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// Download defaults (blocky's loading.downloads, dev-docs/research §2.6).
const (
	DefaultMaxHTTPSize        = 64 << 20
	DefaultAttempts           = 3
	DefaultRetryBackoff       = time.Second
	DefaultMaxErrorsPerSource = 5
	DefaultHTTPTimeout        = 5 * time.Minute
)

// Download errors. A body rejected by validation keeps the previous body.
var (
	ErrEmptyList       = errors.New("listfetcher: source holds no rules")
	ErrTooManyErrors   = errors.New("listfetcher: source is mostly invalid lines")
	ErrUnsupported     = errors.New("listfetcher: source type not supported here")
	ErrUnsupportedURL  = errors.New("listfetcher: only http and https list URLs are supported")
	errNotModifiedMiss = errors.New("listfetcher: 304 Not Modified without a cached body")
)

// ConfigMapFunc reads a configMapRef source body. Only the kube store
// driver can resolve ConfigMaps; without one, configMapRef sources report
// ErrUnsupported in status.
type ConfigMapFunc func(ctx context.Context, namespace, name string) (io.ReadCloser, error)

// ConfigMapKey is the cache key of a configMapRef source.
func ConfigMapKey(namespace, name string) string { return "configmap://" + namespace + "/" + name }

// DownloadOptions configure a Downloader.
type DownloadOptions struct {
	// Client performs the requests; default an http.Client with
	// DefaultHTTPTimeout, the proxy from the environment and, unless
	// AllowLocalAddresses is set, a dialer that refuses local addresses.
	Client *http.Client
	// HTTPSOnly rejects HTTP initial requests and redirects before transport.
	// Cached bodies and ConfigMap sources keep their existing behavior.
	HTTPSOnly bool
	// AllowLocalAddresses lets list URLs reach loopback, link-local
	// (169.254.0.0/16 and fe80::/10: cloud metadata services), unspecified,
	// multicast and the known cloud metadata addresses
	// (blocking.allowLocalListURLs). Off by default: a BlockList writer
	// (the operator role) must not turn the fetching node into a proxy for
	// node-local services (SSRF). Private LAN ranges are always allowed.
	// With a caller-supplied Client only literal addresses in the URL are
	// checked.
	AllowLocalAddresses bool
	// MaxHTTPSize caps a body; default DefaultMaxHTTPSize.
	MaxHTTPSize int64
	// Attempts per download for transient failures (network errors, 429,
	// 5xx); default DefaultAttempts.
	Attempts int
	// RetryBackoff is the first delay between attempts, doubled each time,
	// with ±50 % jitter; default DefaultRetryBackoff.
	RetryBackoff time.Duration
	// MaxErrorsPerSource rejects a body with more invalid lines than this
	// when the invalid lines also outnumber its rules (a page that is not
	// a list); negative disables the check. Default
	// DefaultMaxErrorsPerSource. Browser filter lists full of cosmetic
	// rules stay accepted because their DNS rules outnumber them.
	MaxErrorsPerSource int
	// UserAgent; default "kudurru/<version>".
	UserAgent string
	// ConfigMaps resolves configMapRef sources; nil means unsupported.
	ConfigMaps ConfigMapFunc
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Downloader fetches list bodies into a DiskCache with conditional GET,
// retries and validation.
type Downloader struct {
	opts  DownloadOptions
	cache *DiskCache
	sleep func(ctx context.Context, d time.Duration) error
}

// NewDownloader returns a Downloader writing into cache.
func NewDownloader(cache *DiskCache, opts DownloadOptions) *Downloader {
	if opts.Client == nil {
		if opts.AllowLocalAddresses {
			opts.Client = &http.Client{Timeout: DefaultHTTPTimeout, Transport: http.DefaultTransport}
		} else {
			opts.Client = guardedClient()
		}
	}
	if opts.HTTPSOnly {
		opts.Client = downloadpolicy.HTTPSOnly(opts.Client)
	}
	if opts.MaxHTTPSize <= 0 {
		opts.MaxHTTPSize = DefaultMaxHTTPSize
	}
	if opts.Attempts <= 0 {
		opts.Attempts = DefaultAttempts
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = DefaultRetryBackoff
	}
	if opts.MaxErrorsPerSource == 0 {
		opts.MaxErrorsPerSource = DefaultMaxErrorsPerSource
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "kudurru/" + version.Get().Version
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Downloader{opts: opts, cache: cache, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Result reports one download.
type Result struct {
	Entry Entry
	// Changed is true when a new body replaced the cached one (or none was
	// cached).
	Changed bool
	// NotModified is true for an HTTP 304 revalidation.
	NotModified bool
}

// Source describes what to download.
type Source struct {
	// Key is the URL or ConfigMapKey.
	Key    string
	Format parser.Format
	// ConfigMap is set for configMapRef sources.
	ConfigMap *ConfigMapRef
}

// ConfigMapRef names a ConfigMap source.
type ConfigMapRef struct{ Namespace, Name string }

// Fetch downloads src into the cache, recording origin. On error the
// cached body (if any) is untouched.
func (d *Downloader) Fetch(ctx context.Context, src Source, origin string) (Result, error) {
	if src.ConfigMap != nil {
		return d.fetchConfigMap(ctx, src, origin)
	}
	u, err := url.Parse(src.Key)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Result{}, fmt.Errorf("%w: %q", ErrUnsupportedURL, src.Key)
	}
	if !d.opts.AllowLocalAddresses {
		if err := checkURLHost(u); err != nil {
			return Result{}, fmt.Errorf("listfetcher: GET %s: %w", redact(src.Key), err)
		}
	}
	backoff := d.opts.RetryBackoff
	var last error
	for attempt := 1; attempt <= d.opts.Attempts; attempt++ {
		res, retry, err := d.fetchOnce(ctx, src, origin)
		if err == nil {
			return res, nil
		}
		last = err
		if !retry || attempt == d.opts.Attempts || ctx.Err() != nil {
			break
		}
		jitter := time.Duration(rand.Int64N(int64(backoff))) - backoff/2 //nolint:gosec // jitter, not a secret
		if err := d.sleep(ctx, backoff+jitter); err != nil {
			return Result{}, errors.Join(last, err)
		}
		backoff *= 2
	}
	return Result{}, last
}

// fetchOnce makes one conditional GET. retry reports whether the failure
// is transient.
func (d *Downloader) fetchOnce(ctx context.Context, src Source, origin string) (Result, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.Key, nil)
	if err != nil {
		return Result{}, false, err
	}
	req.Header.Set("User-Agent", d.opts.UserAgent)
	req.Header.Set("Accept", "text/plain, */*;q=0.5")
	prev, hasPrev := d.cache.Lookup(src.Key)
	if hasPrev {
		if prev.ETag != "" {
			req.Header.Set("If-None-Match", prev.ETag)
		}
		if prev.LastModified != "" {
			req.Header.Set("If-Modified-Since", prev.LastModified)
		}
	}
	resp, err := d.opts.Client.Do(req)
	if err != nil {
		if errors.Is(err, downloadpolicy.ErrHTTPSRequired) {
			// net/http wraps errors with the URL, which can contain tokens.
			return Result{}, false, downloadpolicy.ErrHTTPSRequired
		}
		if errors.Is(err, ErrLocalAddress) {
			return Result{}, false, fmt.Errorf("listfetcher: GET %s: %w", redact(src.Key), err)
		}
		return Result{}, true, fmt.Errorf("listfetcher: GET %s: %w", redact(src.Key), err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	now := d.opts.Now()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		if !hasPrev {
			return Result{}, false, errNotModifiedMiss
		}
		e, err := d.cache.Touch(src.Key, now, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"))
		if err != nil {
			return Result{}, false, err
		}
		return Result{Entry: e, NotModified: true}, false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return Result{}, true, fmt.Errorf("listfetcher: GET %s: %s", redact(src.Key), resp.Status)
	case resp.StatusCode != http.StatusOK:
		return Result{}, false, fmt.Errorf("listfetcher: GET %s: %s", redact(src.Key), resp.Status)
	}
	if resp.ContentLength > d.opts.MaxHTTPSize {
		return Result{}, false, fmt.Errorf("%w: Content-Length %d > %d", ErrTooLarge, resp.ContentLength, d.opts.MaxHTTPSize)
	}
	e, changed, err := d.cache.Put(src.Key, resp.Body, PutOptions{
		MaxSize:      d.opts.MaxHTTPSize,
		Validate:     d.validator(src.Format),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Origin:       origin,
		Fetched:      now,
	})
	if err != nil {
		// A body cut by the network is transient; content problems are not.
		transient := !errors.Is(err, ErrTooLarge) && !isContentError(err)
		return Result{}, transient, fmt.Errorf("listfetcher: %s: %w", redact(src.Key), err)
	}
	return Result{Entry: e, Changed: changed}, false, nil
}

func (d *Downloader) fetchConfigMap(ctx context.Context, src Source, origin string) (Result, error) {
	if d.opts.ConfigMaps == nil {
		return Result{}, fmt.Errorf("%w: configMapRef %s/%s needs the kube store driver", ErrUnsupported, src.ConfigMap.Namespace, src.ConfigMap.Name)
	}
	rc, err := d.opts.ConfigMaps(ctx, src.ConfigMap.Namespace, src.ConfigMap.Name)
	if err != nil {
		return Result{}, fmt.Errorf("listfetcher: configMapRef %s/%s: %w", src.ConfigMap.Namespace, src.ConfigMap.Name, err)
	}
	defer func() { _ = rc.Close() }()
	e, changed, err := d.cache.Put(src.Key, rc, PutOptions{
		MaxSize: d.opts.MaxHTTPSize, Validate: d.validator(src.Format), Origin: origin, Fetched: d.opts.Now(),
	})
	if err != nil {
		return Result{}, fmt.Errorf("listfetcher: configMapRef %s/%s: %w", src.ConfigMap.Namespace, src.ConfigMap.Name, err)
	}
	return Result{Entry: e, Changed: changed}, nil
}

// validator parses the temporary body: HTML and binary bodies are refused
// by the parser's sniffing (parser.ErrHTML, parser.ErrBinary), empty lists
// and pages that are mostly invalid lines by the counts.
func (d *Downloader) validator(format parser.Format) func(string) (int, int, error) {
	maxErrs := d.opts.MaxErrorsPerSource
	return func(path string) (int, int, error) {
		f, err := os.Open(path) //nolint:gosec // a temporary file in the list cache
		if err != nil {
			return 0, 0, err
		}
		defer func() { _ = f.Close() }()
		res, err := parser.Parse(f, parser.Options{Format: format, KeepUnsupported: -1, KeepInvalid: -1}, nil)
		if err != nil {
			return 0, 0, err
		}
		switch {
		case res.Rules == 0 && res.Unsupported == 0:
			return res.Rules, res.Invalid, fmt.Errorf("%w (%d lines, %d invalid)", ErrEmptyList, res.Lines, res.Invalid)
		case maxErrs >= 0 && res.Invalid > maxErrs && res.Invalid > res.Rules:
			return res.Rules, res.Invalid, fmt.Errorf("%w: %d invalid lines, %d rules", ErrTooManyErrors, res.Invalid, res.Rules)
		}
		return res.Rules, res.Invalid, nil
	}
}

func isContentError(err error) bool {
	for _, e := range []error{parser.ErrHTML, parser.ErrBinary, parser.ErrUnknownFormat, ErrEmptyList, ErrTooManyErrors} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// redact drops user info and the query string (tokens in list URLs) from
// a URL for logs and status.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	return strings.TrimSuffix(u.String(), "?")
}
