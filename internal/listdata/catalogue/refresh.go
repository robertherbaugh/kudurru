package catalogue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/downloadpolicy"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// Refresher defaults.
const (
	DefaultRefreshInterval = 24 * time.Hour
	MinRefreshInterval     = time.Minute
	// DefaultFallbackAfter is how long a replica waits for the leader
	// before downloading the catalogue itself (as for list bodies, D15).
	DefaultFallbackAfter = 10 * time.Minute
	// replicaPoll bounds how often a replica asks the leader.
	replicaPoll = 5 * time.Minute
	// retryAfter is the delay after a failed attempt.
	retryAfter  = 5 * time.Minute
	httpTimeout = time.Minute
)

// Files under RefresherOptions.Dir.
const (
	fileReceipt       = "catalogue.receipt.json"
	fileImportReceipt = "adguard-services.receipt.json"
)

// What a Refresher's URL serves (RefresherOptions.Kind).
const (
	// KindCatalogue: a kudurru catalogue document (blocking.catalogue.url).
	KindCatalogue = "catalogue"
	// KindAdGuard: AdGuard's HostlistsRegistry services document
	// (blocking.catalogue.adguardServicesURL), merged in as an import.
	KindAdGuard = "adguard"
)

// PullFunc fetches the catalogue the leader at addr serves
// (internalapi.Client.Catalogue): the document and the URL it came from.
type PullFunc func(ctx context.Context, addr string) (body []byte, source string, err error)

// RefresherOptions configure a Refresher.
type RefresherOptions struct {
	// Kind is what URL serves: KindCatalogue (default) or KindAdGuard.
	Kind string
	// URL is blocking.catalogue.url (or adguardServicesURL); required (an
	// empty URL needs no refresher: the embedded catalogue serves).
	URL string
	// RefreshInterval is blocking.catalogue.refreshInterval; default
	// DefaultRefreshInterval, at least MinRefreshInterval.
	RefreshInterval time.Duration
	// Holder receives every new catalogue; required.
	Holder *Holder
	// Dir keeps the last good download across restarts; "" disables it.
	Dir string
	// Membership finds the leader; default cluster.Self (single node).
	Membership cluster.Membership
	// Pull asks the leader; nil makes every node download itself.
	Pull PullFunc
	// Client downloads URL; default a plain client with a 1 m timeout.
	// The wiring passes the list fetcher's guarded client.
	Client *http.Client
	// HTTPSOnly restricts new external downloads, including redirects.
	// Persisted data and authenticated peer pulls keep their existing semantics.
	HTTPSOnly bool
	// FallbackAfter; default DefaultFallbackAfter.
	FallbackAfter time.Duration
	Logger        *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Status reports the refresher's last attempt, for the API.
type Status struct {
	// Kind is KindCatalogue or KindAdGuard.
	Kind string `json:"kind"`
	// URL is blocking.catalogue.url.
	URL string `json:"url"`
	// LastAttempt and LastSuccess are zero before the first attempt.
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	// LastError is the error of the last attempt; empty after a success.
	LastError string `json:"lastError,omitempty"`
}

// Refresher keeps the catalogue in service current with
// blocking.catalogue.url, following the list fetcher's "fetch once, serve
// everywhere" (D15): the leader downloads the document (conditional GET)
// on the refresh interval, every replica pulls the leader's copy over the
// internal listener (GET /internal/catalogue) and downloads it itself only
// when the leader has been unreachable for FallbackAfter. A document is
// served only after it parses and validates with this build's schema
// version; on any failure the catalogue in service stays (the last good
// download, kept in Dir across restarts, else the embedded one).
//
// It runs on every node (NeedLeaderElection false).
type Refresher struct {
	opts RefresherOptions
	log  *slog.Logger

	mu           sync.Mutex
	etag         string
	lastModified string
	status       Status
	leaderSince  time.Time // leader unreachable since
}

// NewRefresher returns a Refresher. It does no I/O.
func NewRefresher(opts RefresherOptions) (*Refresher, error) {
	switch {
	case opts.URL == "":
		return nil, errors.New("catalogue: RefresherOptions.URL is required")
	case len(opts.URL) > maxSourceBytes:
		return nil, errors.New("catalogue: source URL exceeds bound")
	case !strings.HasPrefix(opts.URL, "https://") && !strings.HasPrefix(opts.URL, "http://"):
		return nil, fmt.Errorf("catalogue: blocking.catalogue.url %q is not an http or https URL", opts.URL)
	case opts.Holder == nil:
		return nil, errors.New("catalogue: RefresherOptions.Holder is required")
	case opts.Kind == "":
		opts.Kind = KindCatalogue
	case opts.Kind != KindCatalogue && opts.Kind != KindAdGuard:
		return nil, fmt.Errorf("catalogue: unknown RefresherOptions.Kind %q", opts.Kind)
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	opts.RefreshInterval = max(opts.RefreshInterval, MinRefreshInterval)
	if opts.FallbackAfter <= 0 {
		opts.FallbackAfter = DefaultFallbackAfter
	}
	if opts.Membership == nil {
		opts.Membership = cluster.Self{}
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: httpTimeout}
	}
	if opts.HTTPSOnly {
		opts.Client = downloadpolicy.HTTPSOnly(opts.Client)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Refresher{opts: opts, log: opts.Logger.With("component", "catalogue", "kind", opts.Kind), status: Status{Kind: opts.Kind, URL: opts.URL}}, nil
}

// NeedLeaderElection marks the Refresher as always-on: the leader
// downloads, replicas pull.
func (r *Refresher) NeedLeaderElection() bool { return false }

// Status returns the last attempt's outcome.
func (r *Refresher) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Run serves the persisted download at once, then refreshes until ctx
// ends.
func (r *Refresher) Run(ctx context.Context) error {
	r.loadPersisted()
	for {
		wait := r.Refresh(ctx)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// Refresh runs one attempt and returns the delay until the next one.
func (r *Refresher) Refresh(ctx context.Context) time.Duration {
	now := r.opts.Now()
	leader, err := r.opts.Membership.Leader(ctx)
	switch {
	case err == nil && leader.Self:
		return r.download(ctx, now)
	case err == nil && r.opts.Pull != nil && leader.Address != "":
		body, source, perr := r.opts.Pull(ctx, leader.Address)
		if perr == nil {
			perr = r.apply(body, source)
		}
		if perr == nil {
			r.mu.Lock()
			r.leaderSince = time.Time{}
			r.mu.Unlock()
			r.succeeded(now)
			return min(r.opts.RefreshInterval, replicaPoll)
		}
		err = fmt.Errorf("leader %s: %w", leader.Name, perr)
	case err == nil && r.opts.Pull == nil:
		// No internal listener: every node downloads itself.
		return r.download(ctx, now)
	case err == nil:
		err = fmt.Errorf("leader %s has no internal address", leader.Name)
	}
	r.mu.Lock()
	if r.leaderSince.IsZero() {
		r.leaderSince = now
	}
	since := r.leaderSince
	r.mu.Unlock()
	if now.Sub(since) >= r.opts.FallbackAfter {
		r.log.Info("catalogue: the leader is unreachable; downloading directly", "err", err)
		return r.download(ctx, now)
	}
	r.failed(now, err)
	return min(retryAfter, r.opts.FallbackAfter)
}

// download fetches URL with a conditional GET.
func (r *Refresher) download(ctx context.Context, now time.Time) time.Duration {
	if err := r.fetch(ctx); err != nil {
		r.failed(now, err)
		return min(retryAfter, r.opts.RefreshInterval)
	}
	r.succeeded(now)
	return r.opts.RefreshInterval
}

func (r *Refresher) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.opts.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "kudurru/"+version.Get().Version)
	req.Header.Set("Accept", "application/json")
	r.mu.Lock()
	if r.etag != "" {
		req.Header.Set("If-None-Match", r.etag)
	}
	if r.lastModified != "" {
		req.Header.Set("If-Modified-Since", r.lastModified)
	}
	r.mu.Unlock()
	resp, err := r.opts.Client.Do(req)
	if err != nil {
		if errors.Is(err, downloadpolicy.ErrHTTPSRequired) {
			return downloadpolicy.ErrHTTPSRequired
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("GET %s: %s", r.opts.URL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(r.maxBytes())+1))
	if err != nil {
		return fmt.Errorf("GET %s: %w", r.opts.URL, err)
	}
	if err := r.apply(body, r.opts.URL); err != nil {
		return err
	}
	r.mu.Lock()
	r.etag, r.lastModified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	r.mu.Unlock()
	return nil
}

func (r *Refresher) maxBytes() int {
	if r.opts.Kind == KindAdGuard {
		return MaxAdGuardBytes
	}
	return MaxBytes
}

// apply validates body and serves it; an unchanged document is a no-op.
func (r *Refresher) apply(body []byte, source string) error {
	// Validate provenance before the digest no-op, including identical bodies
	// sent by a leader still configured for an older source.
	if source != r.opts.URL || len(body) > r.maxBytes() {
		return errors.New("catalogue: source mismatch or document bound")
	}
	if r.opts.Kind == KindAdGuard {
		return r.applyImport(body, source)
	}
	if r.opts.Holder.Base().Digest() == digestOf(body) {
		return nil
	}
	c, err := Parse(body, source)
	if err != nil {
		return err
	}
	if r.opts.Holder.Set(c) {
		r.log.Info("catalogue updated", "version", c.Version, "source", source, "services", len(c.Services), "safeSearch", len(c.SafeSearch), "digest", c.Digest())
		r.persist(c.raw, c.Origin())
	}
	return nil
}

// applyImport is apply for KindAdGuard.
func (r *Refresher) applyImport(body []byte, source string) error {
	if cur := r.opts.Holder.Import(); cur != nil && cur.Digest() == digestOf(body) {
		return nil
	}
	if len(body) > MaxAdGuardBytes {
		return fmt.Errorf("catalogue: AdGuard services document is over the %d byte cap", MaxAdGuardBytes)
	}
	imp, err := ParseAdGuard(body, source)
	if err != nil {
		return err
	}
	if r.opts.Holder.SetImport(imp) {
		_, shadowed := r.opts.Holder.Current().Imported()
		r.log.Info("AdGuard services imported", "source", source, "services", len(imp.Services), "shadowed", shadowed,
			"skipped", imp.Skipped, "droppedRules", imp.DroppedRules, "digest", imp.Digest())
		r.persist(imp.raw, imp.origin)
	}
	return nil
}

func (r *Refresher) failed(now time.Time, err error) {
	r.mu.Lock()
	r.status.LastAttempt, r.status.LastError = now, err.Error()
	r.mu.Unlock()
	if r.opts.Kind == KindAdGuard {
		kept := "none"
		if imp := r.opts.Holder.Import(); imp != nil {
			kept = imp.Digest()
		}
		r.log.Warn("AdGuard services refresh failed; keeping the import in service", "url", r.opts.URL, "err", err, "digest", kept)
		return
	}
	keep := r.opts.Holder.Base()
	r.log.Warn("catalogue refresh failed; keeping the catalogue in service", "url", r.opts.URL, "err", err,
		"version", keep.Version, "source", keep.Origin())
}

func (r *Refresher) succeeded(now time.Time) {
	r.mu.Lock()
	r.status.LastAttempt, r.status.LastSuccess, r.status.LastError = now, now, ""
	r.mu.Unlock()
}
