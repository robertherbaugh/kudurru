package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HeaderRequestID carries the request id; an incoming value from a
// trusted proxy is kept, otherwise one is generated.
const HeaderRequestID = "X-Request-Id"

// reqState is the mutable per-request record shared by the outer
// net/http middleware and the huma middleware/handlers. Values that inner
// layers learn (the route pattern, the authorizer path) flow back out
// through it, because a context set inside cannot be seen outside.
type reqState struct {
	start    time.Time
	id       string
	clientIP netip.Addr
	// route is the operation pattern ("GET /zones/{zone}") once routed.
	route string
	// authz is the authorizer path that granted or denied.
	authz string
	// clusterAllowed records that the route's declared permission
	// (allowedResource.allowedVerb) was granted at cluster scope, so
	// per-object checks of the same permission can be skipped.
	clusterAllowed  bool
	allowedResource string
	allowedVerb     string
	// method, path and userAgent are kept for audit records.
	method    string
	path      string
	userAgent string
}

type stateKey struct{}

func withState(ctx context.Context, st *reqState) context.Context {
	return context.WithValue(ctx, stateKey{}, st)
}

// stateFrom returns the request state; a fresh one when the request did
// not pass the outer chain (tests calling a handler directly).
func stateFrom(ctx context.Context) *reqState {
	if st, ok := ctx.Value(stateKey{}).(*reqState); ok {
		return st
	}
	return &reqState{start: time.Now()}
}

// tookMS is the request duration so far in milliseconds.
func tookMS(ctx context.Context) float64 {
	return float64(time.Since(stateFrom(ctx).start).Microseconds()) / 1000
}

// RequestID returns the request id of ctx, or "".
func RequestID(ctx context.Context) string { return stateFrom(ctx).id }

// statusWriter records the status and size and passes Flush, Hijack and
// Unwrap through (SSE needs Flush; http.ResponseController uses Unwrap).
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("api: response writer cannot hijack")
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// metrics are the API instruments (§11.1).
type metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_api_requests_total", Help: "API requests by method, route pattern and status code.",
		}, []string{"method", "route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kudurru_api_request_duration_seconds", Help: "API request duration by route pattern.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route"}),
	}
	if reg != nil {
		reg.MustRegister(m.requests, m.duration)
	}
	return m
}

// chain applies middleware so that the first one listed runs first.
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		if mw[i] != nil {
			h = mw[i](h)
		}
	}
	return h
}

// recoverMW turns a panic into a 500 envelope and a log line (§8.8 first).
func recoverMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
						panic(v)
					}
					logger.ErrorContext(r.Context(), "api: panic", "panic", fmt.Sprint(v), "path", r.URL.Path, "stack", string(debug.Stack()))
					writeError(w, r, newError(http.StatusInternalServerError, KeyInternal, "internal error", "the server logged the failure with the request id"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestIDMW creates the request state and assigns the request id.
func requestIDMW(trusted *trustedProxies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := &reqState{start: time.Now(), method: r.Method, path: r.URL.Path, userAgent: truncate(r.UserAgent(), 256)}
			id := r.Header.Get(HeaderRequestID)
			if id == "" || len(id) > 64 || !trusted.trusts(remoteAddr(r)) || strings.ContainsAny(id, "\r\n\" ") {
				id = newRequestID()
			}
			st.id = id
			w.Header().Set(HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(withState(r.Context(), st)))
		})
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// trustedProxies decides whether X-Forwarded-* headers are believed.
type trustedProxies struct {
	prefixes []netip.Prefix
}

func (t *trustedProxies) trusts(a netip.Addr) bool {
	if t == nil || !a.IsValid() {
		return false
	}
	for _, p := range t.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		a, _ := netip.ParseAddr(r.RemoteAddr)
		return a.Unmap()
	}
	return ap.Addr().Unmap()
}

// realIPMW derives the client address: the peer, or for a trusted proxy
// the right-most untrusted X-Forwarded-For entry. X-Forwarded-Proto from
// a trusted proxy marks the request as HTTPS for HSTS and cookies.
func realIPMW(trusted *trustedProxies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := stateFrom(r.Context())
			peer := remoteAddr(r)
			st.clientIP = peer
			if trusted.trusts(peer) {
				if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
					parts := strings.Split(strings.Join(xff, ","), ",")
					for i := len(parts) - 1; i >= 0; i-- {
						a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
						if err != nil {
							break
						}
						st.clientIP = a.Unmap()
						if !trusted.trusts(st.clientIP) {
							break
						}
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedHTTPS reports whether the request reached kudurru over TLS or
// through a trusted proxy that terminated TLS.
func forwardedHTTPS(r *http.Request, trusted *trustedProxies) bool {
	if r.TLS != nil {
		return true
	}
	return trusted.trusts(remoteAddr(r)) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ClientIP returns the client address derived by the RealIP step.
func ClientIP(ctx context.Context) netip.Addr { return stateFrom(ctx).clientIP }

// securityHeadersMW is §8.8 SecurityHeaders: CSP default-src 'self',
// framing denied, nosniff, same-origin referrer, HSTS when TLS terminates
// here. The docs page replaces the CSP with its own (it loads Scalar).
func securityHeadersMW(trusted *trustedProxies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			if forwardedHTTPS(r, trusted) {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// accessLogMW logs one line per request and feeds the metrics. The route
// label is the operation pattern, never the raw path (bounded
// cardinality).
func accessLogMW(logger *slog.Logger, m *metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			st := stateFrom(r.Context())
			route := st.route
			if route == "" {
				route = "other"
			}
			code := sw.code()
			d := time.Since(st.start)
			m.requests.WithLabelValues(r.Method, route, strconv.Itoa(code)).Inc()
			m.duration.WithLabelValues(route).Observe(d.Seconds())
			level := slog.LevelDebug
			if code >= 500 {
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "api request",
				slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("route", route),
				slog.Int("status", code), slog.Int64("bytes", sw.bytes), slog.Duration("duration", d),
				slog.String("client", st.clientIP.String()), slog.String("request_id", st.id))
		})
	}
}

// crossOriginMW is Go 1.25 http.CrossOriginProtection over every unsafe
// request (§8.2): browsers attach Sec-Fetch-Site/Origin, so a cross-site
// form or fetch is refused while non-browser clients pass.
func crossOriginMW(cop *http.CrossOriginProtection) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := cop.Check(r); err != nil {
				writeError(w, r, newError(http.StatusForbidden, KeyCSRF, "cross-origin request rejected: "+err.Error(),
					"send the request from the kudurru UI origin or a non-browser client"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// onPrefix applies mw only to requests whose path starts with prefix.
func onPrefix(prefix string, mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	if mw == nil {
		return nil
	}
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, prefix) {
				wrapped.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeError writes the envelope outside huma (middleware, SPA mux).
func writeError(w http.ResponseWriter, r *http.Request, e *Error) {
	e.TookMS = tookMS(r.Context())
	for k, vs := range e.headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	writeJSON(w, e.Status, e)
}
