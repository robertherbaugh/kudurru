package dnsserver

import (
	"github.com/robertherbaugh/kudurru/internal/clientid"

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

// dnsListener serves Do53 over UDP or TCP, or DoT (tcp-tls), on miekg/dns
// servers. A udp or tcp listener binds env.sockets sockets with
// SO_REUSEPORT (socket.go), one miekg server and read loop each; DoT binds
// one. With an enabled PROXYv2 policy every socket is wrapped so
// authenticated headers are stripped and forged ones dropped before miekg
// sees the bytes.
type dnsListener struct {
	spec  Spec
	env   *env
	proto pipeline.Proto
	tls   *tls.Config // DoT only

	mu      sync.Mutex
	srvs    []*dns.Server
	addr    net.Addr
	closer  func() error
	started chan struct{} // closed once every server has started
	pending atomic.Int32  // servers not yet started
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

// Sockets returns how many sockets the listener has bound (0 before Bind).
func (l *dnsListener) Sockets() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.srvs)
}

// socketCount is the number of sockets this listener binds.
func (l *dnsListener) socketCount() int {
	if l.spec.Protocol == ProtoDoT || l.env.sockets < 1 {
		return 1
	}
	return l.env.sockets
}

func (l *dnsListener) Bind() error {
	lim := l.env.limits
	srv := &dns.Server{
		Handler:      l,
		ReadTimeout:  lim.ReadTimeout,
		WriteTimeout: lim.WriteTimeout,
		// miekg's default UDP read buffer is 512 bytes, which truncates
		// EDNS queries (and a PROXYv2 header plus query) silently.
		UDPSize:      dns.DefaultMsgSize,
		TsigProvider: l.env.tsigProvider(),
	}
	proxied := l.env.proxy.Enabled()
	switch l.spec.Protocol {
	case ProtoUDP:
		pcs, err := listenPackets(l.spec.Address, l.socketCount())
		if err != nil {
			return err
		}
		srv.Net = "udp"
		if proxied {
			srv.UDPSize += proxyproto.MaxHeaderLen
		}
		srvs := make([]*dns.Server, len(pcs))
		for i, pc := range pcs {
			s := l.socketServer(srv)
			s.PacketConn = pc
			if proxied {
				s.PacketConn = &proxyproto.PacketConn{PacketConn: pc, Policy: l.env.proxy}
			}
			srvs[i] = s
		}
		l.setBound(srvs, pcs[0].LocalAddr(), closeAllFunc(pcs))
	case ProtoTCP, ProtoDoT:
		lns, err := listenStreams(l.spec.Address, l.socketCount())
		if err != nil {
			return err
		}
		srv.Net = "tcp"
		if l.spec.Protocol == ProtoDoT {
			srv.Net = "tcp-tls"
			srv.TLSConfig = l.tls
		}
		idle := lim.IdleTimeout
		srv.IdleTimeout = func() time.Duration { return idle }
		srv.MaxTCPQueries = -1
		if lim.MaxTCPQueries > 0 {
			srv.MaxTCPQueries = lim.MaxTCPQueries
		}
		srvs := make([]*dns.Server, len(lns))
		for i, ln := range lns {
			lis := ln
			if lim.StreamWriteTimeout > 0 {
				lis = writeDeadlineListener{Listener: lis, timeout: lim.StreamWriteTimeout}
			}
			if proxied {
				lis = &proxyproto.Listener{Listener: lis, Policy: l.env.proxy, HeaderTimeout: lim.ProxyHeaderTimeout}
			}
			if l.spec.Protocol == ProtoDoT {
				lis = tls.NewListener(lis, l.tls)
			}
			s := l.socketServer(srv)
			s.Listener = lis
			srvs[i] = s
		}
		l.setBound(srvs, lns[0].Addr(), closeAllFunc(lns))
	default:
		return errors.New("dnsserver: unsupported protocol " + string(l.spec.Protocol))
	}
	return nil
}

// writeDeadlineListener arms a write deadline of timeout
// (Limits.StreamWriteTimeout) before every Write on the connections it
// accepts. miekg/dns documents Server.WriteTimeout but never sets a write
// deadline, so a client that stops reading would hold a connection, and a
// zone transfer stream with it, until the kernel gives up (zero-window
// probes never do). Each DNS message is one Write (TLS records go through
// the same Write below the TLS layer), so the deadline bounds each
// message, not the whole transfer.
type writeDeadlineListener struct {
	net.Listener
	timeout time.Duration
}

func (l writeDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &writeDeadlineConn{Conn: c, timeout: l.timeout}, nil
}

type writeDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *writeDeadlineConn) Write(b []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// socketServer returns a fresh miekg server for one socket with the
// configuration of tmpl. miekg servers carry per-instance state (a lock,
// the connection set, a buffer pool), so each socket needs its own; every
// exported setting is copied so options set on tmpl reach all sockets.
func (l *dnsListener) socketServer(tmpl *dns.Server) *dns.Server {
	l.pending.Add(1)
	return &dns.Server{
		Net:            tmpl.Net,
		TLSConfig:      tmpl.TLSConfig,
		Handler:        tmpl.Handler,
		UDPSize:        tmpl.UDPSize,
		ReadTimeout:    tmpl.ReadTimeout,
		WriteTimeout:   tmpl.WriteTimeout,
		IdleTimeout:    tmpl.IdleTimeout,
		TsigProvider:   tmpl.TsigProvider,
		TsigSecret:     tmpl.TsigSecret,
		DecorateReader: tmpl.DecorateReader,
		DecorateWriter: tmpl.DecorateWriter,
		MaxTCPQueries:  tmpl.MaxTCPQueries,
		MsgAcceptFunc:  tmpl.MsgAcceptFunc,
		MsgInvalidFunc: tmpl.MsgInvalidFunc,
		NotifyStartedFunc: func() {
			if l.pending.Add(-1) == 0 {
				close(l.started)
			}
		},
	}
}

func (l *dnsListener) setBound(srvs []*dns.Server, addr net.Addr, closer func() error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.srvs, l.addr, l.closer = srvs, addr, closer
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
	srvs := l.srvs
	l.mu.Unlock()
	if len(srvs) == 0 {
		return errors.New("dnsserver: Serve before Bind")
	}
	errCh := make(chan error, len(srvs))
	for _, srv := range srvs {
		go func() { errCh <- srv.ActivateAndServe() }()
	}
	running := len(srvs)
	// stop shuts every server down, waits for the read loops still
	// running and releases the sockets; first is the error that ended
	// serving, if any.
	stop := func(first error) error {
		sctx, cancel := context.WithTimeout(context.Background(), l.env.limits.ShutdownTimeout)
		defer cancel()
		var wg sync.WaitGroup
		for _, srv := range srvs {
			wg.Go(func() { _ = srv.ShutdownContext(sctx) })
		}
		wg.Wait()
		for ; running > 0; running-- {
			if err := <-errCh; first == nil {
				first = err
			}
		}
		_ = l.Close()
		return first
	}

	select {
	case <-l.started:
		l.ready.Store(true)
	case err := <-errCh:
		running--
		return stop(err)
	}
	defer l.ready.Store(false)

	select {
	case <-ctx.Done():
		return stop(nil)
	case err := <-errCh:
		running--
		return stop(err)
	}
}

// ServeDNS implements dns.Handler.
func (l *dnsListener) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	serveMiekg(l.env.handler, l.env, l.proto, w, r, l.spec.ClientIDDomain)
}

// serveMiekg answers one message delivered by a miekg/dns server. Requests
// whose TSIG fails are answered NOTAUTH without running the pipeline;
// replies to signed requests are signed (tsig.go); multi-message answers
// (AXFR/IXFR) are streamed on TCP and DoT. A pipeline drop sends nothing:
// over UDP nothing is written, over TCP and DoT the connection is closed
// (other queries pipelined on it are abandoned, as a client that is being
// dropped deserves).
func serveMiekg(h *Handler, e *env, proto pipeline.Proto, w dns.ResponseWriter, r *dns.Msg, clientIDDomain ...string) {
	client := clientFrom(w.RemoteAddr(), proto)
	if client.ClientID == "" && proto == pipeline.ProtoDoT && len(clientIDDomain) > 0 {
		if cs, ok := w.(dns.ConnectionStater); ok {
			if state := cs.ConnectionState(); state != nil {
				client.ClientID = clientid.FromSNI(state.ServerName, clientIDDomain[0])
			}
		}
	}
	sig, ok := verifyTSIG(h, e, w, r, &client)
	if !ok {
		return
	}
	m, stream := h.resolve(context.Background(), r, client)
	if m == nil {
		if proto != pipeline.ProtoUDP {
			_ = w.Close()
		}
		return
	}
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
