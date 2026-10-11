package dnsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/tlsreload"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Protocol keys the listener registry. The values are the ones operators
// write in kudurru.yaml (listeners.dns[].protocols and the dot, doh, doq and
// doh3 sections; docs/architecture.md §6.2).
type Protocol string

// Listener protocols.
const (
	ProtoUDP  Protocol = "udp"
	ProtoTCP  Protocol = "tcp"
	ProtoDoT  Protocol = "dot"  // RFC 7858, tcp-tls, ALPN "dot"
	ProtoDoH  Protocol = "doh"  // RFC 8484 over HTTP/1.1 and HTTP/2
	ProtoDoQ  Protocol = "doq"  // RFC 9250, ALPN "doq"
	ProtoDoH3 Protocol = "doh3" // RFC 8484 over HTTP/3, ALPN "h3"
)

// Protocols returns every protocol the registry can build, in a stable
// order.
func Protocols() []Protocol {
	return []Protocol{ProtoUDP, ProtoTCP, ProtoDoT, ProtoDoH, ProtoDoQ, ProtoDoH3}
}

// Pipeline maps a listener protocol onto the transport recorded in
// pipeline.Client.Proto.
func (p Protocol) Pipeline() pipeline.Proto {
	switch p {
	case ProtoTCP:
		return pipeline.ProtoTCP
	case ProtoDoT:
		return pipeline.ProtoDoT
	case ProtoDoH:
		return pipeline.ProtoDoH
	case ProtoDoH3:
		return pipeline.ProtoDoH3
	case ProtoDoQ:
		return pipeline.ProtoDoQ
	default:
		return pipeline.ProtoUDP
	}
}

// TLS reports whether the protocol needs a certificate.
func (p Protocol) TLS() bool {
	return p == ProtoDoT || p == ProtoDoH || p == ProtoDoQ || p == ProtoDoH3
}

// Network is the socket type the protocol binds: "udp" or "tcp".
func (p Protocol) Network() string {
	switch p {
	case ProtoUDP, ProtoDoQ, ProtoDoH3:
		return "udp"
	default:
		return "tcp"
	}
}

// ALPN returns the application protocols the listener advertises.
func (p Protocol) ALPN() []string {
	switch p {
	case ProtoDoT:
		return []string{"dot"}
	case ProtoDoH:
		return []string{"h2", "http/1.1"}
	case ProtoDoQ:
		return []string{"doq"}
	case ProtoDoH3:
		return []string{"h3"}
	}
	return nil
}

// DefaultDoHPath is the RFC 8484 template path.
const DefaultDoHPath = "/dns-query"

// Spec describes one listener.
type Spec struct {
	Protocol Protocol
	Address  string
	// Path is the DoH/DoH3 request path; empty means DefaultDoHPath.
	Path string
	// CertFile and KeyFile are required for TLS protocols. Listeners that
	// name the same pair share one tlsreload.Reloader.
	CertFile string
	KeyFile  string
}

// Name identifies the listener in readiness output, e.g. "doq:0.0.0.0:853".
func (s Spec) Name() string { return string(s.Protocol) + ":" + s.Address }

func (s Spec) validate() error {
	if _, ok := factories[s.Protocol]; !ok {
		return fmt.Errorf("dnsserver: unknown protocol %q (want one of %v)", s.Protocol, Protocols())
	}
	if s.Address == "" {
		return fmt.Errorf("dnsserver: %s listener needs an address", s.Protocol)
	}
	if s.Protocol.TLS() && (s.CertFile == "" || s.KeyFile == "") {
		return fmt.Errorf("dnsserver: %s listener %s needs tls.certFile and tls.keyFile", s.Protocol, s.Address)
	}
	if s.Path != "" && !strings.HasPrefix(s.Path, "/") {
		return fmt.Errorf("dnsserver: %s listener %s: path %q must start with /", s.Protocol, s.Address, s.Path)
	}
	return nil
}

// Listener is one bound transport.
type Listener interface {
	// Spec returns the listener's configuration.
	Spec() Spec
	// Bind opens the socket and returns bind errors immediately.
	Bind() error
	// Serve answers queries until ctx is done, then drains in-flight
	// queries for at most Limits.ShutdownTimeout and releases the socket.
	// Bind must have succeeded.
	Serve(ctx context.Context) error
	// Addr returns the bound address, or nil before Bind.
	Addr() net.Addr
	// Ready reports whether the listener is bound and serving.
	Ready() bool
	// Close releases the socket of a listener that was bound but will not
	// be served. Serve releases it otherwise.
	Close() error
}

// Limits are per-listener resource bounds. Zero fields take the defaults
// noted on each field.
type Limits struct {
	// ReadTimeout bounds reading the first query of a TCP/DoT connection,
	// a DoQ stream and DoH request headers. Default 2s.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing a response. Default 2s.
	WriteTimeout time.Duration
	// IdleTimeout closes idle TCP, DoT and DoH connections. Default 10s.
	IdleTimeout time.Duration
	// MaxTCPQueries caps the queries served on one TCP or DoT connection;
	// zero means unlimited, as in CoreDNS. (miekg/dns's own default of 128
	// makes long-lived DoT clients such as Android Private DNS reconnect.)
	MaxTCPQueries int
	// QUICIdleTimeout is the QUIC max idle timeout for DoQ and DoH3.
	// Default 30s.
	QUICIdleTimeout time.Duration
	// QUICMaxStreams caps concurrent bidirectional streams per QUIC
	// connection. Default 256 (CoreDNS DefaultMaxQUICStreams).
	QUICMaxStreams int64
	// QUICWorkers caps concurrently processed DoQ streams per listener;
	// streams beyond it wait, which back-pressures clients through QUIC
	// flow control. Default 1024 (CoreDNS DefaultQUICStreamWorkers).
	QUICWorkers int
	// QUICMaxConns caps concurrent QUIC connections per DoQ/DoH3 listener;
	// excess connections are closed with DOQ_EXCESSIVE_LOAD /
	// H3_EXCESSIVE_LOAD. Zero means unlimited.
	QUICMaxConns int
	// MaxBodyBytes caps a DoH request body or decoded ?dns= parameter.
	// Default and maximum 65535, the largest DNS message.
	MaxBodyBytes int
	// MaxHeaderBytes caps DoH/DoH3 request headers. Default 16 KiB.
	MaxHeaderBytes int
	// ProxyHeaderTimeout bounds the PROXYv2 header read on stream
	// connections. Default proxyproto.DefaultHeaderTimeout.
	ProxyHeaderTimeout time.Duration
	// ShutdownTimeout bounds draining on shutdown. Default 5s.
	ShutdownTimeout time.Duration
}

// Default limits.
const (
	DefaultReadTimeout     = 2 * time.Second
	DefaultWriteTimeout    = 2 * time.Second
	DefaultIdleTimeout     = 10 * time.Second
	DefaultQUICIdleTimeout = 30 * time.Second
	DefaultQUICMaxStreams  = 256
	DefaultQUICWorkers     = 1024
	DefaultMaxHeaderBytes  = 16 << 10
	DefaultShutdownTimeout = 5 * time.Second
	maxDNSMessage          = 65535
)

func (l Limits) withDefaults() Limits {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&l.ReadTimeout, DefaultReadTimeout)
	def(&l.WriteTimeout, DefaultWriteTimeout)
	def(&l.IdleTimeout, DefaultIdleTimeout)
	def(&l.QUICIdleTimeout, DefaultQUICIdleTimeout)
	def(&l.ProxyHeaderTimeout, proxyproto.DefaultHeaderTimeout)
	def(&l.ShutdownTimeout, DefaultShutdownTimeout)
	if l.QUICMaxStreams <= 0 {
		l.QUICMaxStreams = DefaultQUICMaxStreams
	}
	if l.QUICWorkers <= 0 {
		l.QUICWorkers = DefaultQUICWorkers
	}
	if l.QUICMaxConns < 0 {
		l.QUICMaxConns = 0
	}
	if l.MaxBodyBytes <= 0 || l.MaxBodyBytes > maxDNSMessage {
		l.MaxBodyBytes = maxDNSMessage
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	return l
}

// Options configures a Registry.
type Options struct {
	// Handler is the shared query path; required.
	Handler *Handler
	Logger  *slog.Logger
	// Certs is the shared certificate store; one is created when nil.
	Certs *tlsreload.Store
	// Proxy authenticates PROXYv2 headers on the udp, tcp and dot
	// listeners. Nil or disabled means headers are not inspected.
	Proxy  *proxyproto.Policy
	Limits Limits
	// Sockets is how many SO_REUSEPORT sockets every udp and tcp listener
	// binds, each with its own read loop (listeners.sockets). Zero means
	// runtime.GOMAXPROCS(0) and 1 a single plain socket; platforms other
	// than Linux always bind one. See SocketCount.
	Sockets int
}

// env is what every listener constructor receives.
type env struct {
	handler *Handler
	logger  *slog.Logger
	certs   *tlsreload.Store
	proxy   *proxyproto.Policy
	limits  Limits
	sockets int // resolved Options.Sockets, at least 1
}

// factories is the protocol-keyed constructor table behind the registry.
var factories = map[Protocol]func(Spec, *env) (Listener, error){
	ProtoUDP:  newDNSListener,
	ProtoTCP:  newDNSListener,
	ProtoDoT:  newDNSListener,
	ProtoDoH:  newDoHListener,
	ProtoDoQ:  newDoQListener,
	ProtoDoH3: newDoH3Listener,
}

// Registry owns every DNS listener of a node, keyed by protocol, plus the
// certificate store they share. It is the dnsserver runnable of
// docs/architecture.md §3.1: Run binds everything, serves until the context
// ends and reports per-listener readiness.
type Registry struct {
	env *env

	mu        sync.Mutex
	listeners []Listener
	bound     bool
}

// NewRegistry returns an empty registry.
func NewRegistry(opts Options) (*Registry, error) {
	if opts.Handler == nil || opts.Handler.Chain == nil {
		return nil, errors.New("dnsserver: Options.Handler with a Chain is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = opts.Handler.logger()
	}
	certs := opts.Certs
	if certs == nil {
		certs = tlsreload.NewStore(logger)
	}
	return &Registry{env: &env{
		handler: opts.Handler,
		logger:  logger,
		certs:   certs,
		proxy:   opts.Proxy,
		limits:  opts.Limits.withDefaults(),
		sockets: SocketCount(opts.Sockets),
	}}, nil
}

// Add validates specs and creates their listeners. TLS listeners load their
// certificate here, so a bad pair fails before anything binds. Duplicate
// protocol/address pairs are rejected.
func (r *Registry) Add(specs ...Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bound {
		return errors.New("dnsserver: cannot add listeners after Bind")
	}
	seen := map[string]bool{}
	for _, l := range r.listeners {
		seen[l.Spec().Name()] = true
	}
	var built []Listener
	for _, s := range specs {
		if s.Path == "" && (s.Protocol == ProtoDoH || s.Protocol == ProtoDoH3) {
			s.Path = DefaultDoHPath
		}
		if err := s.validate(); err != nil {
			return err
		}
		if seen[s.Name()] {
			return fmt.Errorf("dnsserver: duplicate listener %s", s.Name())
		}
		seen[s.Name()] = true
		l, err := factories[s.Protocol](s, r.env)
		if err != nil {
			return fmt.Errorf("dnsserver: %s: %w", s.Name(), err)
		}
		built = append(built, l)
	}
	r.listeners = append(r.listeners, built...)
	return nil
}

// Listeners returns the listeners in the order they were added.
func (r *Registry) Listeners() []Listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Listener(nil), r.listeners...)
}

// Lookup returns the listeners of one protocol.
func (r *Registry) Lookup(p Protocol) []Listener {
	var out []Listener
	for _, l := range r.Listeners() {
		if l.Spec().Protocol == p {
			out = append(out, l)
		}
	}
	return out
}

// Addr returns the bound address of the first listener of protocol p, or
// nil (useful when the port was 0).
func (r *Registry) Addr(p Protocol) net.Addr {
	for _, l := range r.Lookup(p) {
		if a := l.Addr(); a != nil {
			return a
		}
	}
	return nil
}

// Certs returns the shared certificate store.
func (r *Registry) Certs() *tlsreload.Store { return r.env.certs }

// Proxy returns the PROXYv2 policy (possibly nil) so metrics can read its
// drop counters.
func (r *Registry) Proxy() *proxyproto.Policy { return r.env.proxy }

// Bind binds every listener, or none: on the first error the ones already
// bound are closed and the error returned. Run calls Bind when the caller
// has not.
func (r *Registry) Bind() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bound {
		return nil
	}
	for i, l := range r.listeners {
		if err := l.Bind(); err != nil {
			for _, b := range r.listeners[:i] {
				_ = b.Close()
			}
			return fmt.Errorf("dnsserver: bind %s: %w", l.Spec().Name(), err)
		}
	}
	r.bound = true
	return nil
}

// Run binds (if needed) and serves every listener and the certificate
// watcher until ctx is done or a listener fails; it then shuts every
// listener down and returns the first failure, or nil.
func (r *Registry) Run(ctx context.Context) error {
	if err := r.Bind(); err != nil {
		return err
	}
	listeners := r.Listeners()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(listeners)+1)
	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Go(func() {
			if err := l.Serve(ctx); err != nil {
				errCh <- fmt.Errorf("dnsserver: %s: %w", l.Spec().Name(), err)
				cancel()
			}
		})
	}
	wg.Go(func() {
		if err := r.env.certs.Run(ctx); err != nil {
			errCh <- err
			cancel()
		}
	})
	names := make([]string, len(listeners))
	for i, l := range listeners {
		names[i] = l.Spec().Name()
	}
	r.env.logger.Info("dns listeners started", "listeners", names)
	wg.Wait()
	close(errCh)
	return <-errCh
}

// Ready reports overall readiness and the names of listeners that are not
// serving, for /healthz/ready.
func (r *Registry) Ready() (bool, []string) {
	var bad []string
	for _, l := range r.Listeners() {
		if !l.Ready() {
			bad = append(bad, l.Spec().Name())
		}
	}
	sort.Strings(bad)
	return len(bad) == 0, bad
}
