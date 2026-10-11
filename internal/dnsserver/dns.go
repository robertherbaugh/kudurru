package dnsserver

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// dnsListener serves Do53 over UDP or TCP, or DoT (tcp-tls), on a
// miekg/dns server. With an enabled PROXYv2 policy the socket is wrapped so
// authenticated headers are stripped and forged ones dropped before miekg
// sees the bytes.
type dnsListener struct {
	spec  Spec
	env   *env
	proto pipeline.Proto
	tls   *tls.Config // DoT only

	mu      sync.Mutex
	srv     *dns.Server
	addr    net.Addr
	closer  func() error
	started chan struct{}
	ready   atomic.Bool
}

func newDNSListener(spec Spec, e *env) (Listener, error) {
	l := &dnsListener{spec: spec, env: e, proto: spec.Protocol.Pipeline(), started: make(chan struct{})}
	if spec.Protocol == ProtoDoT {
		r, err := e.certs.Get(spec.CertFile, spec.KeyFile)
		if err != nil {
			return nil, err
		}
		l.tls = r.TLSConfig(ProtoDoT.ALPN()...)
	}
	return l, nil
}

func (l *dnsListener) Spec() Spec { return l.spec }

func (l *dnsListener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.addr
}

func (l *dnsListener) Ready() bool { return l.ready.Load() }

func (l *dnsListener) Bind() error {
	lim := l.env.limits
	srv := &dns.Server{
		Handler:      l,
		ReadTimeout:  lim.ReadTimeout,
		WriteTimeout: lim.WriteTimeout,
		// miekg's default UDP read buffer is 512 bytes, which truncates
		// EDNS queries (and a PROXYv2 header plus query) silently.
		UDPSize:           dns.DefaultMsgSize,
		NotifyStartedFunc: func() { close(l.started) },
		TsigProvider:      l.env.tsigProvider(),
	}
	proxied := l.env.proxy.Enabled()
	switch l.spec.Protocol {
	case ProtoUDP:
		pc, err := net.ListenPacket("udp", l.spec.Address)
		if err != nil {
			return err
		}
		srv.Net = "udp"
		srv.PacketConn = pc
		if proxied {
			srv.PacketConn = &proxyproto.PacketConn{PacketConn: pc, Policy: l.env.proxy}
			srv.UDPSize += proxyproto.MaxHeaderLen
		}
		l.setBound(srv, pc.LocalAddr(), pc.Close)
	case ProtoTCP, ProtoDoT:
		ln, err := net.Listen("tcp", l.spec.Address)
		if err != nil {
			return err
		}
		lis := ln
		if proxied {
			lis = &proxyproto.Listener{Listener: ln, Policy: l.env.proxy, HeaderTimeout: lim.ProxyHeaderTimeout}
		}
		srv.Net = "tcp"
		if l.spec.Protocol == ProtoDoT {
			srv.Net = "tcp-tls"
			srv.TLSConfig = l.tls
			lis = tls.NewListener(lis, l.tls)
		}
		idle := lim.IdleTimeout
		srv.IdleTimeout = func() time.Duration { return idle }
		srv.MaxTCPQueries = -1
		if lim.MaxTCPQueries > 0 {
			srv.MaxTCPQueries = lim.MaxTCPQueries
		}
		srv.Listener = lis
		l.setBound(srv, ln.Addr(), ln.Close)
	default:
		return errors.New("dnsserver: unsupported protocol " + string(l.spec.Protocol))
	}
	return nil
}

func (l *dnsListener) setBound(srv *dns.Server, addr net.Addr, closer func() error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.srv, l.addr, l.closer = srv, addr, closer
}

func (l *dnsListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closer == nil {
		return nil
	}
	err := l.closer()
	l.closer = nil
	return err
}

func (l *dnsListener) Serve(ctx context.Context) error {
	l.mu.Lock()
	srv := l.srv
	l.mu.Unlock()
	if srv == nil {
		return errors.New("dnsserver: Serve before Bind")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ActivateAndServe() }()

	select {
	case <-l.started:
		l.ready.Store(true)
	case err := <-errCh:
		return err
	}
	defer l.ready.Store(false)

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), l.env.limits.ShutdownTimeout)
		defer cancel()
		_ = srv.ShutdownContext(sctx)
		<-errCh
		_ = l.Close()
		return nil
	case err := <-errCh:
		_ = l.Close()
		return err
	}
}

// ServeDNS implements dns.Handler.
func (l *dnsListener) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	serveMiekg(l.env.handler, l.env, l.proto, w, r)
}

// serveMiekg answers one message delivered by a miekg/dns server. Requests
// whose TSIG fails are answered NOTAUTH without running the pipeline;
// replies to signed requests are signed (tsig.go); multi-message answers
// (AXFR/IXFR) are streamed on TCP and DoT.
func serveMiekg(h *Handler, e *env, proto pipeline.Proto, w dns.ResponseWriter, r *dns.Msg) {
	client := clientFrom(w.RemoteAddr(), proto)
	sig, ok := verifyTSIG(h, e, w, r, &client)
	if !ok {
		return
	}
	m, stream := h.resolve(context.Background(), r, client)
	if stream != nil && proto != pipeline.ProtoUDP {
		writeStream(h, w, stream, sig)
		return
	}
	if proto == pipeline.ProtoUDP {
		size := dns.MinMsgSize
		if opt := r.IsEdns0(); opt != nil && int(opt.UDPSize()) > size {
			size = int(opt.UDPSize())
		}
		if sig.signed() {
			size -= tsigReserve
		}
		m.Truncate(size)
	}
	sig.sign(m)
	if err := w.WriteMsg(m); err != nil {
		h.logger().Debug("write failed", "err", err, "proto", proto.String())
	}
}
