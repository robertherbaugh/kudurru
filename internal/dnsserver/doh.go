package dnsserver

import (
	"github.com/robertherbaugh/kudurru/internal/clientid"

	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// dohMIME is the RFC 8484 media type.
const dohMIME = "application/dns-message"

// dohHandler implements RFC 8484 for both DoH (HTTP/1.1, HTTP/2) and DoH3:
// GET with ?dns=<base64url> and POST with an application/dns-message body.
// The response carries Cache-Control: max-age=<smallest TTL>.
type dohHandler struct {
	h         *Handler
	env       *env
	path      string
	proto     pipeline.Proto
	maxBody   int
	clientIDs bool
	// altSvc is fixed at registry bind and observes the matching H3 listener.
	altSvc func(*http.Request) string
}

func (d *dohHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, matches := clientid.FromPath(r.URL.Path, d.path, d.clientIDs)
	if !matches || r.URL.EscapedPath() != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	var wire []byte
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		q := r.URL.Query().Get("dns")
		if q == "" {
			http.Error(w, "missing dns query parameter", http.StatusBadRequest)
			return
		}
		// RFC 8484 §6 forbids padding; tolerate it anyway.
		q = strings.TrimRight(q, "=")
		if base64.RawURLEncoding.DecodedLen(len(q)) > d.maxBody {
			http.Error(w, "dns parameter too large", http.StatusRequestURITooLong)
			return
		}
		b, err := base64.RawURLEncoding.DecodeString(q)
		if err != nil {
			http.Error(w, "dns parameter is not base64url", http.StatusBadRequest)
			return
		}
		wire = b
	case http.MethodPost:
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != dohMIME {
			http.Error(w, "content type must be "+dohMIME, http.StatusUnsupportedMediaType)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, int64(d.maxBody)+1))
		if err != nil {
			http.Error(w, "reading body failed", http.StatusBadRequest)
			return
		}
		if len(b) > d.maxBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		wire = b
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req := new(dns.Msg)
	if err := req.Unpack(wire); err != nil {
		http.Error(w, "malformed DNS message", http.StatusBadRequest)
		return
	}
	client := clientFromHTTP(r, d.proto)
	client.ClientID = id
	out, err := d.h.resolveEncrypted(r.Context(), d.env, wire, req, client)
	if err != nil {
		http.Error(w, "response could not be encoded", http.StatusInternalServerError)
		return
	}
	if out == nil {
		// A pipeline drop: abort the exchange without a response (HTTP/2
		// and HTTP/3 reset the stream, HTTP/1.1 closes the connection).
		panic(http.ErrAbortHandler)
	}
	hdr := w.Header()
	if d.altSvc != nil {
		if alternate := d.altSvc(r); alternate != "" {
			hdr.Set("Alt-Svc", alternate)
		}
	}
	hdr.Set("Content-Type", dohMIME)
	// Every reply can depend on client policy, even without an explicit ID.
	// Shared HTTP caches must never reuse one client's answer for another.
	hdr.Set("Cache-Control", "private, "+out.cache)
	hdr.Set("Content-Length", strconv.Itoa(len(out.wire)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out.wire) //nolint:gosec // G705: a packed DNS message served as application/dns-message, never HTML
	}
}

// clientFromHTTP takes the client from the connection, never from
// X-Forwarded-For: an HTTP header is not an authenticated channel.
func clientFromHTTP(r *http.Request, proto pipeline.Proto) pipeline.Client {
	c := pipeline.Client{Proto: proto}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		c.Addr = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return c
}

// cacheControl implements RFC 8484 §5.1: the freshness lifetime is the
// smallest TTL in the response (for a negative answer the SOA TTL capped by
// its MINIMUM, RFC 2308). Errors and record-less answers are not cacheable.
func cacheControl(m *dns.Msg) string {
	if m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError {
		return "no-store"
	}
	ttl, ok := minTTL(m)
	if !ok {
		return "max-age=0"
	}
	return "max-age=" + strconv.FormatUint(uint64(ttl), 10)
}

func minTTL(m *dns.Msg) (uint32, bool) {
	var lowest uint32
	found := false
	for _, section := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range section {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			ttl := rr.Header().Ttl
			if soa, ok := rr.(*dns.SOA); ok && soa.Minttl < ttl {
				ttl = soa.Minttl
			}
			if !found || ttl < lowest {
				lowest, found = ttl, true
			}
		}
	}
	return lowest, found
}

// dohListener serves DoH over HTTP/1.1 and HTTP/2 with net/http.
type dohListener struct {
	spec    Spec
	env     *env
	handler *dohHandler

	mu    sync.Mutex
	ln    net.Listener
	srv   *http.Server
	ready atomic.Bool
}

func newDoHListener(spec Spec, e *env) (Listener, error) {
	r, err := e.certs.Get(spec.CertFile, spec.KeyFile)
	if err != nil {
		return nil, err
	}
	l := &dohListener{spec: spec, env: e, handler: &dohHandler{h: e.handler, env: e, path: spec.Path, clientIDs: spec.ClientIDs, proto: ProtoDoH.Pipeline(), maxBody: e.limits.MaxBodyBytes}}
	lim := e.limits
	l.srv = &http.Server{
		Handler:           l.handler,
		TLSConfig:         r.TLSConfig(ProtoDoH.ALPN()...),
		ReadHeaderTimeout: lim.ReadTimeout,
		ReadTimeout:       lim.ReadTimeout + lim.WriteTimeout,
		WriteTimeout:      lim.ReadTimeout + lim.WriteTimeout + e.handler.timeout(),
		IdleTimeout:       lim.IdleTimeout,
		MaxHeaderBytes:    lim.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(e.logger.Handler(), slog.LevelDebug),
	}
	return l, nil
}

func (l *dohListener) Spec() Spec  { return l.spec }
func (l *dohListener) Ready() bool { return l.ready.Load() }

func (l *dohListener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln == nil {
		return nil
	}
	return l.ln.Addr()
}

func (l *dohListener) Bind() error {
	ln, err := net.Listen("tcp", l.spec.Address)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.ln = ln
	l.mu.Unlock()
	return nil
}

func (l *dohListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln == nil {
		return nil
	}
	return l.ln.Close()
}

func (l *dohListener) Serve(ctx context.Context) error {
	l.mu.Lock()
	ln := l.ln
	l.mu.Unlock()
	if ln == nil {
		return errors.New("dnsserver: Serve before Bind")
	}
	errCh := make(chan error, 1)
	// ServeTLS clones TLSConfig and enables HTTP/2 because "h2" is in
	// NextProtos; GetCertificate still reads the shared reloader.
	go func() { errCh <- l.srv.ServeTLS(ln, "", "") }()
	l.ready.Store(true)
	defer l.ready.Store(false)

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), l.env.limits.ShutdownTimeout)
		defer cancel()
		if err := l.srv.Shutdown(sctx); err != nil {
			_ = l.srv.Close()
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
