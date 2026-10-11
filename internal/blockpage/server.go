package blockpage

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/tlsreload"
)

// Settings are the hot blockPage.* keys the page reads per request.
type Settings struct {
	Title, Message string
	// Contact is blockPage.contact and ContactURL its link.
	Contact, ContactURL string
	ShowDetails         bool
	// DashboardURL is the dashboard base URL ("https://10.0.0.53:8080");
	// the page links to <DashboardURL>/queries?domain=<name>.
	DashboardURL string
	// RateLimit is requests per minute per client; 0 disables it.
	RateLimit int
}

// SettingsFrom maps the blockPage.* keys onto Settings, deriving the
// dashboard URL from the block page address and listeners.api when
// blockPage.dashboardURL is empty.
func SettingsFrom(c *config.Config) Settings {
	b := c.BlockPage
	s := Settings{Title: b.Title, Message: b.Message, Contact: strings.TrimSpace(b.Contact), ShowDetails: b.ShowDetails, RateLimit: b.RateLimit}
	s.ContactURL, _ = config.BlockPageContactURL(b.Contact)
	s.DashboardURL = strings.TrimSuffix(b.DashboardURL, "/")
	if s.DashboardURL == "" {
		s.DashboardURL = derivedDashboardURL(c)
	}
	return s
}

// derivedDashboardURL is the API listener as reached at the block page
// address (the block page and the API run on the same node), or "" when
// neither names a usable host.
func derivedDashboardURL(c *config.Config) string {
	host, port, err := net.SplitHostPort(c.Listeners.API.Address)
	if err != nil {
		return ""
	}
	a, err := netip.ParseAddr(host)
	if err != nil || a.IsUnspecified() {
		v4, v6 := c.BlockPageAddrs()
		switch {
		case v4.IsValid():
			a = v4
		case v6.IsValid():
			a = v6
		default:
			return ""
		}
	}
	scheme := "http"
	if c.Listeners.API.TLS.Enabled() {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(a.String(), port), Path: c.Listeners.API.PathPrefix}
	return strings.TrimSuffix(u.String(), "/")
}

// Options configures a Server.
type Options struct {
	// Node is this node's name, shown on the page.
	Node string
	// HTTP and HTTPS are the listener addresses; empty skips one.
	HTTP, HTTPS string
	// CertFile and KeyFile are the HTTPS certificate (the caller generates
	// a self-signed pair when none is configured).
	CertFile, KeyFile string
	// Finder looks up why a name was blocked; nil shows no reason.
	Finder Finder
	// Policy names the policy that applies to a client now; nil omits it.
	Policy func(client netip.Addr, now time.Time) string
	// Settings are the initial page settings (Reconfigure replaces them).
	Settings Settings
	// Registerer receives the metrics; nil leaves them unregistered.
	Registerer prometheus.Registerer
	Logger     *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Server renders the block page and serves it on the block page
// listeners. Every path of every Host answers the page (GET and HEAD 200,
// other methods 405) except /favicon.ico, /robots.txt (disallow all) and
// /health; nothing is ever proxied or fetched.
type Server struct {
	opts     Options
	settings atomic.Pointer[Settings]
	limiter  *limiter
	certs    *tlsreload.Store
	cert     *tlsreload.Reloader

	requests    *prometheus.CounterVec
	rateLimited *prometheus.CounterVec

	httpLn, httpsLn net.Listener
}

// New returns a Server and registers its metrics. It binds nothing; call
// Listen before Run.
func New(o Options) (*Server, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	o.Logger = o.Logger.With("component", "blockpage")
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{
		opts:    o,
		limiter: newLimiter(o.Settings.RateLimit, o.Now),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_blockpage_requests_total",
			Help: "Requests to the block page listeners, by scheme (http, https).",
		}, []string{"scheme"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_blockpage_rate_limited_total",
			Help: "Block page requests answered 429 because the client exceeded blockPage.rateLimit, by scheme.",
		}, []string{"scheme"}),
	}
	settings := o.Settings
	s.settings.Store(&settings)
	if o.Registerer != nil {
		for _, c := range []prometheus.Collector{s.requests, s.rateLimited} {
			if err := o.Registerer.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// Reconfigure replaces the page settings; requests in flight finish with
// the old ones.
func (s *Server) Reconfigure(st Settings) {
	s.settings.Store(&st)
	s.limiter.setRate(st.RateLimit)
}

// Listen binds the configured listeners (fail fast on port conflicts) and
// loads the HTTPS certificate.
func (s *Server) Listen() error {
	if s.opts.HTTP == "" && s.opts.HTTPS == "" {
		return errors.New("blockpage: no listener configured")
	}
	if s.opts.HTTPS != "" {
		s.certs = tlsreload.NewStore(s.opts.Logger)
		r, err := s.certs.Get(s.opts.CertFile, s.opts.KeyFile)
		if err != nil {
			return fmt.Errorf("blockpage: https certificate: %w", err)
		}
		s.cert = r
	}
	var lc net.ListenConfig
	if s.opts.HTTP != "" {
		ln, err := lc.Listen(context.Background(), "tcp", s.opts.HTTP)
		if err != nil {
			return fmt.Errorf("blockpage: %w", err)
		}
		s.httpLn = ln
	}
	if s.opts.HTTPS != "" {
		ln, err := lc.Listen(context.Background(), "tcp", s.opts.HTTPS)
		if err != nil {
			if s.httpLn != nil {
				_ = s.httpLn.Close()
			}
			return fmt.Errorf("blockpage: %w", err)
		}
		s.httpsLn = ln
	}
	return nil
}

// Addrs returns the bound HTTP and HTTPS addresses ("" when not bound).
func (s *Server) Addrs() (httpAddr, httpsAddr string) {
	if s.httpLn != nil {
		httpAddr = s.httpLn.Addr().String()
	}
	if s.httpsLn != nil {
		httpsAddr = s.httpsLn.Addr().String()
	}
	return httpAddr, httpsAddr
}

// Run serves the bound listeners until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if s.httpLn == nil && s.httpsLn == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	errCh := make(chan error, 3)
	var servers []*http.Server
	start := func(ln net.Listener, scheme string, tlsCfg *tls.Config) {
		srv := &http.Server{
			Handler:           s.Handler(scheme),
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          slog.NewLogLogger(s.opts.Logger.Handler(), slog.LevelDebug),
		}
		servers = append(servers, srv)
		go func() {
			var err error
			if tlsCfg != nil {
				err = srv.ServeTLS(ln, "", "")
			} else {
				err = srv.Serve(ln)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		s.opts.Logger.Info("block page listener started", "address", ln.Addr().String(), "scheme", scheme)
	}
	if s.httpLn != nil {
		start(s.httpLn, "http", nil)
	}
	if s.httpsLn != nil {
		start(s.httpsLn, "https", s.cert.TLSConfig("h2", "http/1.1"))
		go func() {
			if err := s.certs.Run(ctx); err != nil {
				s.opts.Logger.Warn("block page certificate watch stopped", "err", err)
			}
		}()
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	return runErr
}

// robots disallows every crawler.
const robots = "User-agent: *\nDisallow: /\n"

// Handler is the block page for one scheme (the metrics label).
func (s *Server) Handler(scheme string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.WithLabelValues(scheme).Inc()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Kudurru-Block", "1")
		// Never Strict-Transport-Security: the page answers for the blocked
		// names, and HSTS would pin them to HTTPS in the browser.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/health" {
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		client := remoteAddr(r)
		if !s.limiter.allow(client) {
			s.rateLimited.WithLabelValues(scheme).Inc()
			h.Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		switch r.URL.Path {
		case "/robots.txt":
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write([]byte(robots))
			return
		case "/favicon.ico":
			h.Set("Content-Type", "image/x-icon")
			h.Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(favicon)
			return
		}
		domain := requestDomain(r)
		body, reason := s.render(r.Context(), domain, client, false)
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Frame-Options", "DENY")
		// The page differs per name and client: no shared caching, and a
		// later allow must not keep showing it.
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body) //nolint:gosec // html/template output: every value is escaped (TestHandlerHostileHost)
		}
		kind := ""
		if reason != nil {
			kind = reason.Kind
		}
		s.opts.Logger.Debug("block page served", "scheme", scheme, "domain", domain, "client", client.String(), "reason", kind)
	})
}

// Preview renders the page as the admin preview with the current
// settings: for domain (default blocked.example) and client (the caller),
// with an example reason. It reads neither the query log nor the policies,
// so the preview reveals nothing the settings.read permission does not.
func (s *Server) Preview(ctx context.Context, domain string, client netip.Addr) []byte {
	d := normalizeDomain(domain)
	if d == "" {
		d = "blocked.example"
	}
	body, _ := s.render(ctx, d, client, true)
	return body
}

// previewReason is the example reason of the preview.
func previewReason(domain string) *Reason {
	return &Reason{Kind: KindList, List: "default/example-list", Rule: "||" + domain + "^", Policy: "default/example-policy", Status: "blocked_list"}
}

// render builds the page for domain and client.
func (s *Server) render(ctx context.Context, domain string, client netip.Addr, preview bool) ([]byte, *Reason) {
	st := s.settings.Load()
	now := s.opts.Now()
	p := Page{
		Title: st.Title, Message: st.Message, Domain: domain, Node: s.opts.Node, Time: now,
		ShowDetails: st.ShowDetails, Contact: st.Contact, ContactURL: st.ContactURL, Preview: preview,
	}
	if client.IsValid() {
		p.Client = client.String()
	}
	if st.ShowDetails {
		switch {
		case domain == "":
			p.ReasonNote = noteNoDomain
		case preview:
			p.Reason = previewReason(domain)
		case s.opts.Finder != nil:
			lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			r, ok, note := s.opts.Finder.Find(lctx, client, domain, now)
			cancel()
			if ok {
				if r.Policy == "" && s.opts.Policy != nil && client.IsValid() {
					r.Policy = s.opts.Policy(client, now)
				}
				p.Reason = &r
			} else {
				p.ReasonNote = note
			}
		}
		if st.DashboardURL != "" && domain != "" {
			p.DashboardURL = st.DashboardURL + "/queries?" + url.Values{"domain": {domain}}.Encode()
		}
	}
	body, err := RenderBytes(p)
	if err != nil {
		// The template is static and every value is a string; this does
		// not happen, but never serve a half page.
		s.opts.Logger.Error("block page render failed", "err", err)
		return []byte("blocked\n"), p.Reason
	}
	return body, p.Reason
}

// remoteAddr is the transport peer. Forwarded headers are ignored: the
// block page is reached directly by the clients DNS sent to it.
func remoteAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// requestDomain is the blocked name: the Host header, else the TLS server
// name, normalised; "" when neither is a domain name.
func requestDomain(r *http.Request) string {
	if d := normalizeDomain(r.Host); d != "" {
		return d
	}
	if r.TLS != nil {
		return normalizeDomain(r.TLS.ServerName)
	}
	return ""
}

// normalizeDomain returns host (an optional :port removed) lower-cased
// without the trailing dot when it is a DNS name of letters, digits,
// hyphens and underscores in labels of 1-63 characters, at most 253 in
// total, with a non-numeric last label. IP literals and anything else give
// "".
func normalizeDomain(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || strings.HasPrefix(host, "[") {
		return ""
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		port := host[i+1:]
		if strings.Count(host, ":") > 1 || port == "" || len(port) > 5 || strings.Trim(port, "0123456789") != "" {
			return "" // a bare IPv6 literal, or not a port
		}
		host = host[:i]
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || len(host) > 253 {
		return ""
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return ""
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return ""
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return ""
	}
	return host
}
