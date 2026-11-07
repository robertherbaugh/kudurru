package dnsserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// doh3Listener serves DoH over HTTP/3 with quic-go/http3, sharing the RFC
// 8484 handler with the HTTP/1.1 and HTTP/2 listener. The setup follows
// CoreDNS core/dnsserver/server_https3.go and dnsproxy
// proxy/serverhttps.go (Apache-2.0; see NOTICE): ListenEarly with ALPN h3,
// 256 streams per connection, 16 KiB header cap, optional connection cap.
type doh3Listener struct {
	spec    Spec
	env     *env
	handler *dohHandler
	qcfg    *quic.Config

	mu    sync.Mutex
	pc    net.PacketConn
	tr    *quic.Transport
	ln    *quic.EarlyListener
	srv   *http3.Server
	ready atomic.Bool
}

func newDoH3Listener(spec Spec, e *env) (Listener, error) {
	if _, err := e.certs.Get(spec.CertFile, spec.KeyFile); err != nil {
		return nil, err
	}
	lim := e.limits
	l := &doh3Listener{
		spec:    spec,
		env:     e,
		handler: &dohHandler{h: e.handler, env: e, path: spec.Path, clientIDs: spec.ClientIDs, proto: ProtoDoH3.Pipeline(), maxBody: lim.MaxBodyBytes},
		qcfg: &quic.Config{
			MaxIdleTimeout:     lim.QUICIdleTimeout,
			MaxIncomingStreams: lim.QUICMaxStreams,
			// HTTP/3 clients open three unidirectional streams (control and
			// two QPACK) plus the occasional GREASE stream.
			MaxIncomingUniStreams: 16,
			Allow0RTT:             true,
		},
	}
	l.srv = &http3.Server{
		Handler:        l.handler,
		MaxHeaderBytes: lim.MaxHeaderBytes,
		IdleTimeout:    lim.QUICIdleTimeout,
		Logger:         e.logger,
	}
	return l, nil
}

func (l *doh3Listener) Spec() Spec  { return l.spec }
func (l *doh3Listener) Ready() bool { return l.ready.Load() }

func (l *doh3Listener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pc == nil {
		return nil
	}
	return l.pc.LocalAddr()
}

func (l *doh3Listener) Bind() error {
	r, err := l.env.certs.Get(l.spec.CertFile, l.spec.KeyFile)
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", l.spec.Address)
	if err != nil {
		return err
	}
	tr := &quic.Transport{Conn: pc}
	// ConfigureTLSConfig sets ALPN h3; the clone keeps GetCertificate, so
	// rotations still flow through the shared reloader.
	ln, err := tr.ListenEarly(http3.ConfigureTLSConfig(r.TLSConfig()), l.qcfg)
	if err != nil {
		_ = pc.Close()
		return err
	}
	l.mu.Lock()
	l.pc, l.tr, l.ln = pc, tr, ln
	l.mu.Unlock()
	return nil
}

func (l *doh3Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pc == nil {
		return nil
	}
	_ = l.ln.Close()
	_ = l.tr.Close()
	err := l.pc.Close()
	l.pc = nil
	return err
}

func (l *doh3Listener) Serve(ctx context.Context) error {
	l.mu.Lock()
	ln := l.ln
	l.mu.Unlock()
	if ln == nil {
		return errors.New("dnsserver: Serve before Bind")
	}
	var qln http3.QUICListener = ln
	if n := l.env.limits.QUICMaxConns; n > 0 {
		qln = &limitQUICListener{QUICListener: ln, sem: make(chan struct{}, n), code: quic.ApplicationErrorCode(http3.ErrCodeExcessiveLoad)}
	}
	errCh := make(chan error, 1)
	go func() { errCh <- l.srv.ServeListener(qln) }()
	l.ready.Store(true)
	defer l.ready.Store(false)

	select {
	case <-ctx.Done():
		l.ready.Store(false)
		sctx, cancel := context.WithTimeout(context.Background(), l.env.limits.ShutdownTimeout)
		defer cancel()
		_ = l.srv.Shutdown(sctx)
		_ = l.Close()
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, quic.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		_ = l.Close()
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, quic.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// limitQUICListener caps concurrent connections; excess connections are
// closed with code as soon as they are accepted.
type limitQUICListener struct {
	http3.QUICListener
	sem  chan struct{}
	code quic.ApplicationErrorCode
}

func (l *limitQUICListener) Accept(ctx context.Context) (*quic.Conn, error) {
	for {
		conn, err := l.QUICListener.Accept(ctx)
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			go func() {
				<-conn.Context().Done()
				<-l.sem
			}()
			return conn, nil
		default:
			_ = conn.CloseWithError(l.code, "too many connections")
		}
	}
}
