package internalapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
)

// Routes.
const (
	PathStatus = "/internal/status"
	PathLists  = "/internal/lists/"
)

// Response headers of GET /internal/lists/{digest}.
const (
	HeaderDigest = "X-Kudurru-Digest"
	// HeaderSource names the cache key (the list URL, or
	// configmap://<ns>/<name>) the body was fetched from, so a replica can
	// refuse a body published for a different source under the same name.
	HeaderSource = "X-Kudurru-Source"
)

// Server defaults.
const (
	DefaultAddress          = "0.0.0.0:8082"
	DefaultRateLimit        = 20 // requests per second per client address
	DefaultBurst            = 40
	DefaultMaxDownloads     = 4
	DefaultMaxListBytes     = 256 << 20
	maxHeaderBytes          = 16 << 10
	statusWriteTimeout      = 10 * time.Second
	listWriteTimeoutMinimum = 2 * time.Minute
	limiterIdle             = 5 * time.Minute
	limiterPruneAbove       = 1024
)

// ErrListNotFound is returned by a ListOpener for unknown digests and by
// Client.OpenList for a 404.
var ErrListNotFound = errors.New("internalapi: list not found")

// Status is the body of GET /internal/status.
type Status struct {
	Node    string       `json:"node"`
	Version string       `json:"version"`
	Leader  string       `json:"leader"`
	State   string       `json:"state"`
	Lists   []ListStatus `json:"lists"`
	// Ready is absent on older peers, whose readiness is unknown.
	Ready    *bool    `json:"ready,omitempty"`
	Unready  []string `json:"unready,omitempty"`
	Snapshot string   `json:"snapshotAge,omitempty"`
}

// ListStatus reports the source digests one node has compiled for a list.
type ListStatus struct {
	Name    string   `json:"name"`
	Digests []string `json:"digests"`
}

// ListBody is one cached list body.
type ListBody struct {
	Body io.ReadCloser
	Size int64
	// Source is the cache key the body was fetched from (URL or
	// configmap://<ns>/<name>).
	Source string
}

// ListOpener serves cached list bodies by sha256 digest (lowercase hex);
// unknown digests return ErrListNotFound.
type ListOpener interface {
	OpenList(digest string) (ListBody, error)
}

// ServerOptions configures a Server.
type ServerOptions struct {
	// Address is listeners.internal.address; default DefaultAddress.
	Address string
	// Credentials are required.
	Credentials *Credentials
	// Status fills GET /internal/status; nil serves an empty status.
	Status func(ctx context.Context) Status
	// Lists serves GET /internal/lists/{digest}; nil answers 404.
	Lists ListOpener
	// Zones fills GET /internal/zones (the primary and secondary zones
	// this node serves with their serials); nil answers 404, which
	// Client.Zones reports as ErrZonesUnsupported.
	Zones func(ctx context.Context) ZoneList
	// Catalogue serves GET /internal/catalogue (the blocked-services and
	// safe-search catalogue in service); nil answers 404.
	Catalogue func() CatalogueBody
	// CatalogueImport serves GET /internal/catalogue/adguard (the AdGuard
	// services document this node imported, when
	// blocking.catalogue.adguardServicesURL is set); false answers 404.
	CatalogueImport func() (CatalogueBody, bool)
	// RateLimit and Burst bound requests per client address; defaults
	// DefaultRateLimit and DefaultBurst.
	RateLimit rate.Limit
	Burst     int
	// MaxDownloads bounds concurrent list downloads; further requests get
	// 503 with Retry-After. Default DefaultMaxDownloads.
	MaxDownloads int
	// MaxListBytes is the largest body served; default DefaultMaxListBytes.
	MaxListBytes int64
	// Registerer receives kudurru_internal_requests_total{route,code}.
	Registerer prometheus.Registerer
	Logger     *slog.Logger
}

// Server is the internal listener.
type Server struct {
	opts      ServerOptions
	log       *slog.Logger
	handler   http.Handler
	downloads chan struct{}
	requests  *prometheus.CounterVec

	mu       sync.Mutex
	limiters map[netip.Addr]*limiterEntry
}

type limiterEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewServer builds the listener; Run binds it.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Credentials == nil {
		return nil, errors.New("internalapi: server needs credentials")
	}
	if opts.Address == "" {
		opts.Address = DefaultAddress
	}
	if opts.RateLimit <= 0 {
		opts.RateLimit = DefaultRateLimit
	}
	if opts.Burst <= 0 {
		opts.Burst = DefaultBurst
	}
	if opts.MaxDownloads <= 0 {
		opts.MaxDownloads = DefaultMaxDownloads
	}
	if opts.MaxListBytes <= 0 {
		opts.MaxListBytes = DefaultMaxListBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{
		opts:      opts,
		log:       opts.Logger.With("component", "internal"),
		downloads: make(chan struct{}, opts.MaxDownloads),
		limiters:  map[netip.Addr]*limiterEntry{},
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_internal_requests_total",
			Help: "Requests to the internal listener by route and HTTP status code.",
		}, []string{"route", "code"}),
	}
	if opts.Registerer != nil {
		if err := opts.Registerer.Register(s.requests); err != nil {
			return nil, err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, s.handleStatus)
	mux.HandleFunc("GET "+PathLists+"{digest}", s.handleList)
	if opts.Zones != nil {
		mux.HandleFunc("GET "+PathZones, s.handleZones)
	}
	if opts.Catalogue != nil {
		mux.HandleFunc("GET "+PathCatalogue, s.handleCatalogue)
	}
	if opts.CatalogueImport != nil {
		mux.HandleFunc("GET "+PathCatalogueAdGuard, s.handleCatalogueImport)
	}
	s.handler = s.guard(mux)
	return s, nil
}

// Handler returns the routes behind the rate limiter and the token check,
// without TLS (for tests and embedding).
func (s *Server) Handler() http.Handler { return s.handler }

// Run listens on Address with TLS and serves until ctx ends. It is an
// always-on runnable for cluster.Runner.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.opts.Address)
	if err != nil {
		return fmt.Errorf("internalapi: listen %s: %w", s.opts.Address, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves TLS on ln until ctx ends, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		TLSConfig:         s.opts.Credentials.ServerTLS(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	tln := tls.NewListener(ln, srv.TLSConfig)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(tln) }()
	s.log.Info("internal listener started", "address", ln.Addr().String())
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			_ = srv.Close()
		}
		<-errCh
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// guard applies, in order: the per-address rate limit, the no-body rule
// and the bearer token.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := routeOf(r.URL.Path)
		rw := &codeWriter{ResponseWriter: w, code: http.StatusOK}
		defer func() { s.requests.WithLabelValues(route, strconv.Itoa(rw.code)).Inc() }()
		if !s.allow(r.RemoteAddr) {
			rw.Header().Set("Retry-After", "1")
			http.Error(rw, "rate limited", http.StatusTooManyRequests)
			return
		}
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			http.Error(rw, "request bodies are not accepted", http.StatusBadRequest)
			return
		}
		tok, ok := bearer(r.Header.Get("Authorization"))
		if !ok || !s.opts.Credentials.ValidToken(tok) {
			rw.Header().Set("WWW-Authenticate", `Bearer realm="kudurru-internal"`)
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

func routeOf(p string) string {
	switch {
	case p == PathStatus:
		return "status"
	case p == PathZones:
		return "zones"
	case p == PathCatalogue, p == PathCatalogueAdGuard:
		return "catalogue"
	case len(p) > len(PathLists) && p[:len(PathLists)] == PathLists:
		return "lists"
	}
	return "other"
}

func bearer(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !equalFoldASCII(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// allow applies the token bucket of the request's source address.
func (s *Server) allow(remote string) bool {
	ap, err := netip.ParseAddrPort(remote)
	addr := ap.Addr().Unmap()
	if err != nil {
		addr = netip.Addr{} // unknown peers share one bucket
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.limiters[addr]
	if e == nil {
		if len(s.limiters) >= limiterPruneAbove {
			for a, x := range s.limiters {
				if now.Sub(x.seen) > limiterIdle {
					delete(s.limiters, a)
				}
			}
		}
		e = &limiterEntry{lim: rate.NewLimiter(s.opts.RateLimit, s.opts.Burst)}
		s.limiters[addr] = e
	}
	e.seen = now
	return e.lim.AllowN(now, 1)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(statusWriteTimeout))
	st := Status{Lists: []ListStatus{}}
	if s.opts.Status != nil {
		st = s.opts.Status(r.Context())
		if st.Lists == nil {
			st.Lists = []ListStatus{}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(st)
}

func (s *Server) handleZones(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(statusWriteTimeout))
	zl := s.opts.Zones(r.Context())
	if zl.Zones == nil {
		zl.Zones = []Zone{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(zl)
}

// ValidDigest reports whether d is a lowercase hex sha256.
func ValidDigest(d string) bool {
	if len(d) != 64 {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	if !ValidDigest(digest) {
		http.Error(w, "digest must be 64 lowercase hex characters", http.StatusBadRequest)
		return
	}
	if s.opts.Lists == nil {
		http.Error(w, "no list cache on this node", http.StatusNotFound)
		return
	}
	select {
	case s.downloads <- struct{}{}:
		defer func() { <-s.downloads }()
	default:
		w.Header().Set("Retry-After", "2")
		http.Error(w, "too many concurrent list downloads", http.StatusServiceUnavailable)
		return
	}
	body, err := s.opts.Lists.OpenList(digest)
	switch {
	case errors.Is(err, ErrListNotFound):
		http.Error(w, "list not found", http.StatusNotFound)
		return
	case err != nil:
		s.log.Warn("internal: open list", "digest", digest, "err", err)
		http.Error(w, "list unavailable", http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Body.Close() }()
	if body.Size > s.opts.MaxListBytes {
		http.Error(w, "list exceeds the size cap", http.StatusRequestEntityTooLarge)
		return
	}
	// Allow at least 1 MiB/s plus a floor for slow links.
	deadline := listWriteTimeoutMinimum + time.Duration(body.Size>>20)*time.Second
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(deadline))
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.FormatInt(body.Size, 10))
	h.Set("ETag", `"`+digest+`"`)
	h.Set(HeaderDigest, digest)
	h.Set(HeaderSource, body.Source)
	h.Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, io.LimitReader(body.Body, body.Size)); err != nil {
		s.log.Debug("internal: list download interrupted", "digest", digest, "err", err)
	}
}

// codeWriter records the status code for the request metric.
type codeWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (w *codeWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.code, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *codeWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *codeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
