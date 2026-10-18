package dnsdist

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

// ErrNoLocalEdge is returned by CacheFlusher.FlushResolver when no dnsdist
// runs next to this kudurru (the API key file `kudurru edge run
// --api-key-out` publishes does not exist). Callers treat it as "nothing
// to flush".
var ErrNoLocalEdge = errors.New("dnsdist: no local edge (API key file missing)")

// DefaultAPIKeyFile and DefaultAPIURL are where `kudurru edge run`
// publishes its API (the chart, compose files and §3.6 use them).
const (
	DefaultAPIKeyFile = "/run/kudurru/dnsdist-api.key" //nolint:gosec // G101: a file path, not a credential
	DefaultAPIURL     = "http://127.0.0.1:8083"
)

// maxScheduledFlush bounds the delayed flush at the end of a temporary
// disable; longer (or Forever) disables wait for the explicit enable.
const maxScheduledFlush = 366 * 24 * time.Hour

// CacheFlusher empties the resolver pool's packet cache of the dnsdist
// that shares this kudurru's host or pod (sidecar, compose single-edge and
// HA nodes: the API on loopback, its key on the shared /run/kudurru
// volume). The resolver pool caches blocked and unblocked answers up to
// their TTL, so a blocking toggle or a list refresh is not visible through
// the edge until those entries expire; the integrator calls
// BlockingChanged from the toggle's change path (POST /dns/blocking and
// the replicated Toggle.Apply) and FlushResolver after a list refresh.
// Remote edges (edge Deployments in Kubernetes) cannot be reached this
// way; see dev-docs/followups.md.
type CacheFlusher struct {
	// API is the local dnsdist API; KeyFile is read on every flush
	// because the key changes on every dnsdist start.
	API *APIClient
	// Log receives flush failures; default slog.Default().
	Log *slog.Logger
	// Timeout bounds one flush; default 2 s.
	Timeout time.Duration
	// Now is the clock; default time.Now.
	Now func() time.Time

	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
	wg      sync.WaitGroup
}

// NewLocalCacheFlusher returns a flusher for the dnsdist API at apiURL
// with its key in keyFile; empty arguments use DefaultAPIURL and
// DefaultAPIKeyFile.
func NewLocalCacheFlusher(apiURL, keyFile string, log *slog.Logger) *CacheFlusher {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	if keyFile == "" {
		keyFile = DefaultAPIKeyFile
	}
	return &CacheFlusher{API: &APIClient{BaseURL: apiURL, KeyFile: keyFile}, Log: log}
}

func (f *CacheFlusher) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

func (f *CacheFlusher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// FlushResolver expunges every entry of the resolver pool's packet cache
// (DELETE /api/v1/cache?pool=resolver&name=.&suffix=true). It returns
// ErrNoLocalEdge when no edge publishes its API key here.
func (f *CacheFlusher) FlushResolver(ctx context.Context) error {
	if f.API.Key == "" && f.API.KeyFile != "" {
		if _, err := os.Stat(f.API.KeyFile); errors.Is(err, os.ErrNotExist) {
			return ErrNoLocalEdge
		}
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return f.API.FlushCache(ctx, edge.PoolResolver, ".", true)
}

// BlockingChanged reacts to a blocking toggle change: it flushes the
// resolver pool now (in the background, never blocking the caller) and,
// for a temporary disable that ends at until, once more when blocking
// resumes. A zero until means blocking is enabled again. A later call
// replaces the pending delayed flush.
func (f *CacheFlusher) BlockingChanged(until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	f.flushAsync("blocking toggled")
	if d := until.Sub(f.now()); !until.IsZero() && d > 0 && d <= maxScheduledFlush {
		f.timer = time.AfterFunc(d, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.stopped {
				f.flushAsync("blocking resumed")
			}
		})
	}
}

// ListsChanged reacts to a compiled list snapshot (a list refresh or an
// edited BlockList): it flushes the resolver pool in the background so the
// edge stops serving answers the old lists produced.
func (f *CacheFlusher) ListsChanged() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		f.flushAsync("lists changed")
	}
}

// BlockResponsesChanged reacts to a change of the synthesised block
// answers (blocking.defaultResponse, blockTTL, customIP, the block page
// addresses): it flushes the resolver pool in the background so the edge
// stops serving the old answers before their block TTL ends.
func (f *CacheFlusher) BlockResponsesChanged() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		f.flushAsync("block responses changed")
	}
}

// flushAsync runs FlushResolver on its own goroutine; f.mu is held.
func (f *CacheFlusher) flushAsync(why string) {
	f.wg.Go(func() {
		err := f.FlushResolver(context.Background())
		switch {
		case err == nil:
			f.log().Info("flushed the edge resolver cache", "reason", why)
		case errors.Is(err, ErrNoLocalEdge):
			f.log().Debug("no local edge to flush", "reason", why)
		default:
			f.log().Warn("edge resolver cache flush failed; cached answers expire with their TTL", "reason", why, "err", err)
		}
	})
}

// Stop cancels the pending delayed flush and waits for running flushes.
func (f *CacheFlusher) Stop() {
	f.mu.Lock()
	f.stopped = true
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	f.mu.Unlock()
	f.wg.Wait()
}
