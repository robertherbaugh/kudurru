// Package tlsreload is the single certificate-reload path shared by every
// TLS listener (DoT, DoH, DoQ, DoH3; decision D67). A Reloader holds the
// current certificate for one cert/key file pair behind an atomic pointer;
// tls.Config.GetCertificate reads it, so a rotation is picked up by the
// next handshake without restarting listeners. A Store hands out one
// Reloader per distinct file pair, so listeners that share a certificate
// share its watcher.
//
// Changes are detected with fsnotify on the files' parent directories (which
// also catches the atomic "..data" symlink swap Kubernetes uses for Secret
// volumes) and, as a fallback for filesystems without inotify, by polling
// modification time and size. A pair that fails to load (for example the
// certificate was replaced before the key) keeps the previous certificate
// and is retried on the next event or poll.
package tlsreload

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultPollInterval is the fallback stat interval.
const DefaultPollInterval = 30 * time.Second

// debounce coalesces the burst of events one rotation produces.
const debounce = 100 * time.Millisecond

// Reloader serves the current certificate for one cert/key pair.
type Reloader struct {
	certFile, keyFile string
	logger            *slog.Logger

	cert     atomic.Pointer[tls.Certificate]
	digest   atomic.Pointer[[sha256.Size]byte]
	loadedAt atomic.Int64 // unix nanos of the last successful load

	reloads  atomic.Uint64
	failures atomic.Uint64

	mu      sync.Mutex // serialises Reload
	stamp   fileStamp
	onSwap  []func(*tls.Certificate)
	lastErr error
}

type fileStamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
}

// New loads the pair once and returns a Reloader; it fails if the files are
// missing, unreadable or do not form a valid pair.
func New(certFile, keyFile string, logger *slog.Logger) (*Reloader, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("tlsreload: certFile and keyFile are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Reloader{certFile: certFile, keyFile: keyFile, logger: logger}
	if _, err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Files returns the certificate and key paths.
func (r *Reloader) Files() (certFile, keyFile string) { return r.certFile, r.keyFile }

// Certificate returns the current certificate.
func (r *Reloader) Certificate() *tls.Certificate { return r.cert.Load() }

// GetCertificate implements tls.Config.GetCertificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cert.Load(), nil
}

// LoadedAt returns when the current certificate was loaded.
func (r *Reloader) LoadedAt() time.Time { return time.Unix(0, r.loadedAt.Load()) }

// NotAfter returns the expiry of the current leaf certificate.
func (r *Reloader) NotAfter() time.Time {
	if c := r.cert.Load(); c != nil && c.Leaf != nil {
		return c.Leaf.NotAfter
	}
	return time.Time{}
}

// Stats returns the number of certificate swaps after the initial load and
// the number of failed reload attempts.
func (r *Reloader) Stats() (reloads, failures uint64) { return r.reloads.Load(), r.failures.Load() }

// LastError returns the error of the most recent failed reload, or nil when
// the last attempt succeeded.
func (r *Reloader) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// OnSwap registers fn to run after every certificate swap (not the initial
// load). Listeners do not need it; it exists for metrics and tests.
func (r *Reloader) OnSwap(fn func(*tls.Certificate)) {
	r.mu.Lock()
	r.onSwap = append(r.onSwap, fn)
	r.mu.Unlock()
}

// TLSConfig returns a server configuration that reads the current
// certificate on every handshake and advertises nextProtos (ALPN). TLS 1.2
// is the floor; QUIC listeners get TLS 1.3 from quic-go regardless.
func (r *Reloader) TLSConfig(nextProtos ...string) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.GetCertificate,
		NextProtos:     append([]string(nil), nextProtos...),
	}
}

// Reload re-reads the pair and swaps it in when its content changed. It
// reports whether a swap happened. On error the previous certificate stays.
func (r *Reloader) Reload() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	swapped, err := r.reloadLocked()
	if err != nil {
		r.lastErr = err
		if r.cert.Load() != nil {
			r.failures.Add(1)
			r.logger.Warn("tls certificate reload failed; keeping the previous certificate",
				"cert", r.certFile, "key", r.keyFile, "err", err)
		}
		return false, err
	}
	r.lastErr = nil
	return swapped, nil
}

func (r *Reloader) reloadLocked() (bool, error) {
	stamp, err := statPair(r.certFile, r.keyFile)
	if err != nil {
		return false, err
	}
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return false, fmt.Errorf("tlsreload: %w", err)
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return false, fmt.Errorf("tlsreload: %w", err)
	}
	h := sha256.New()
	h.Write(certPEM)
	h.Write(keyPEM)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	if prev := r.digest.Load(); prev != nil && *prev == sum {
		r.stamp = stamp
		return false, nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// The stamp is left alone so the next poll retries.
		return false, fmt.Errorf("tlsreload: %s / %s: %w", r.certFile, r.keyFile, err)
	}
	r.stamp = stamp
	initial := r.cert.Load() == nil
	r.cert.Store(&cert)
	r.digest.Store(&sum)
	r.loadedAt.Store(time.Now().UnixNano())
	if initial {
		return false, nil
	}
	r.reloads.Add(1)
	r.logger.Info("tls certificate reloaded", "cert", r.certFile, "notAfter", r.NotAfter())
	for _, fn := range r.onSwap {
		fn(&cert)
	}
	return true, nil
}

func statPair(certFile, keyFile string) (fileStamp, error) {
	ci, err := os.Stat(certFile)
	if err != nil {
		return fileStamp{}, fmt.Errorf("tlsreload: %w", err)
	}
	ki, err := os.Stat(keyFile)
	if err != nil {
		return fileStamp{}, fmt.Errorf("tlsreload: %w", err)
	}
	return fileStamp{certMod: ci.ModTime(), keyMod: ki.ModTime(), certSize: ci.Size(), keySize: ki.Size()}, nil
}

// changed reports whether a stat differs from the one recorded at the last
// load attempt.
func (r *Reloader) changed() bool {
	s, err := statPair(r.certFile, r.keyFile)
	if err != nil {
		return true // let Reload report and count it
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return s != r.stamp
}

// Store deduplicates Reloaders by file pair and runs their watchers.
type Store struct {
	// PollInterval is the fallback stat interval; zero means
	// DefaultPollInterval, negative disables polling.
	PollInterval time.Duration
	Logger       *slog.Logger

	mu        sync.Mutex
	reloaders map[[2]string]*Reloader
	noWatch   bool // tests: exercise the poller alone
}

// NewStore returns an empty store.
func NewStore(logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{Logger: logger, reloaders: map[[2]string]*Reloader{}}
}

// Get returns the Reloader for the pair, loading it on first use. Paths are
// cleaned so "./a" and "a" share one Reloader.
func (s *Store) Get(certFile, keyFile string) (*Reloader, error) {
	key := [2]string{filepath.Clean(certFile), filepath.Clean(keyFile)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.reloaders[key]; ok {
		return r, nil
	}
	r, err := New(key[0], key[1], s.Logger)
	if err != nil {
		return nil, err
	}
	s.reloaders[key] = r
	return r, nil
}

// Reloaders returns the loaded pairs.
func (s *Store) Reloaders() []*Reloader {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Reloader, 0, len(s.reloaders))
	for _, r := range s.reloaders {
		out = append(out, r)
	}
	return out
}

// Run watches every pair obtained so far until ctx is done. Pairs added
// after Run started are covered by polling only.
func (s *Store) Run(ctx context.Context) error {
	reloaders := s.Reloaders()
	if len(reloaders) == 0 {
		<-ctx.Done()
		return nil
	}
	dirs := map[string][]*Reloader{}
	for _, r := range reloaders {
		for _, f := range []string{r.certFile, r.keyFile} {
			d := filepath.Dir(f)
			if !containsReloader(dirs[d], r) {
				dirs[d] = append(dirs[d], r)
			}
		}
	}

	var events <-chan fsnotify.Event
	var errs <-chan error
	w, err := fsnotify.NewWatcher()
	if s.noWatch && err == nil {
		_ = w.Close()
		w, err = nil, errors.New("disabled")
	}
	if err != nil {
		s.Logger.Warn("fsnotify unavailable; certificate reload falls back to polling", "err", err)
	} else {
		defer func() { _ = w.Close() }()
		for d := range dirs {
			if err := w.Add(d); err != nil {
				s.Logger.Warn("cannot watch certificate directory; relying on polling", "dir", d, "err", err)
			}
		}
		events, errs = w.Events, w.Errors
	}

	var poll <-chan time.Time
	if iv := s.PollInterval; iv >= 0 {
		if iv == 0 {
			iv = DefaultPollInterval
		}
		t := time.NewTicker(iv)
		defer t.Stop()
		poll = t.C
	}

	pending := map[*Reloader]bool{}
	var fire <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			for _, r := range dirs[filepath.Dir(ev.Name)] {
				pending[r] = true
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				timer.Reset(debounce)
			}
			fire = timer.C
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			s.Logger.Warn("certificate watcher error", "err", err)
		case <-fire:
			fire = nil
			for r := range pending {
				_, _ = r.Reload()
				delete(pending, r)
			}
		case <-poll:
			for _, r := range s.Reloaders() {
				if r.changed() {
					_, _ = r.Reload()
				}
			}
		}
	}
}

func containsReloader(list []*Reloader, r *Reloader) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}
