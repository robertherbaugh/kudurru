package dnsserver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// DoQ error codes (RFC 9250 §4.3), used both as QUIC application error
// codes on CONNECTION_CLOSE and as stream error codes on RESET_STREAM /
// STOP_SENDING.
const (
	DoQNoError          = 0x0
	DoQInternalError    = 0x1
	DoQProtocolError    = 0x2
	DoQRequestCancelled = 0x3
	DoQExcessiveLoad    = 0x4
	DoQUnspecifiedError = 0x5
)

// errDoQProtocol marks RFC 9250 §4.3.3 protocol errors, which close the
// whole connection with DOQ_PROTOCOL_ERROR.
var errDoQProtocol = errors.New("doq: protocol error")

// doqListener serves DNS over QUIC (RFC 9250): one query per
// client-initiated bidirectional stream, each message prefixed with its
// 2-byte length, message ID 0, STREAM FIN after the query and after the
// response. The structure follows CoreDNS core/dnsserver/server_quic.go and
// dnsproxy proxy/serverquic.go (both Apache-2.0; see NOTICE): a stream
// worker pool shared by all connections, a per-connection stream cap, and
// connection-level DOQ_PROTOCOL_ERROR for malformed streams.
type doqListener struct {
	spec Spec
	env  *env
	qcfg *quic.Config

	mu     sync.Mutex
	pc     net.PacketConn
	tr     *quic.Transport
	ln     *quic.Listener
	active map[*quic.Conn]struct{}

	workers chan struct{}
	conns   chan struct{} // nil when unlimited
	ready   atomic.Bool
	streams sync.WaitGroup
}

func newDoQListener(spec Spec, e *env) (Listener, error) {
	if _, err := e.certs.Get(spec.CertFile, spec.KeyFile); err != nil {
		return nil, err
	}
	lim := e.limits
	l := &doqListener{
		spec: spec,
		env:  e,
		qcfg: &quic.Config{
			MaxIdleTimeout:     lim.QUICIdleTimeout,
			MaxIncomingStreams: lim.QUICMaxStreams,
			// DoQ never uses unidirectional streams.
			MaxIncomingUniStreams: -1,
			// Queries are replay-safe; non-QUERY opcodes are refused by the
			// ingress stage before anything acts on them.
			Allow0RTT: true,
		},
		active:  map[*quic.Conn]struct{}{},
		workers: make(chan struct{}, lim.QUICWorkers),
	}
	if lim.QUICMaxConns > 0 {
		l.conns = make(chan struct{}, lim.QUICMaxConns)
	}
	return l, nil
}

func (l *doqListener) Spec() Spec  { return l.spec }
func (l *doqListener) Ready() bool { return l.ready.Load() }

func (l *doqListener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pc == nil {
		return nil
	}
	return l.pc.LocalAddr()
}

func (l *doqListener) Bind() error {
	r, err := l.env.certs.Get(l.spec.CertFile, l.spec.KeyFile)
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", l.spec.Address)
	if err != nil {
		return err
	}
	tr := &quic.Transport{Conn: pc}
	ln, err := tr.Listen(r.TLSConfig(ProtoDoQ.ALPN()...), l.qcfg)
	if err != nil {
		_ = pc.Close()
		return err
	}
	l.mu.Lock()
	l.pc, l.tr, l.ln = pc, tr, ln
	l.mu.Unlock()
	return nil
}

func (l *doqListener) Close() error {
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

func (l *doqListener) Serve(ctx context.Context) error {
	l.mu.Lock()
	ln := l.ln
	l.mu.Unlock()
	if ln == nil {
		return errors.New("dnsserver: Serve before Bind")
	}
	l.ready.Store(true)

	var conns sync.WaitGroup
	var serveErr error
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, quic.ErrServerClosed) {
				serveErr = err
			}
			break
		}
		if l.conns != nil {
			select {
			case l.conns <- struct{}{}:
			default:
				_ = conn.CloseWithError(DoQExcessiveLoad, "too many connections")
				continue
			}
		}
		l.track(conn, true)
		conns.Go(func() {
			l.serveConn(ctx, conn)
			if l.conns != nil {
				<-l.conns
			}
		})
	}

	// Shutdown: stop accepting connections and streams, let in-flight
	// streams finish (bounded), then close every connection with
	// DOQ_NO_ERROR.
	l.ready.Store(false)
	_ = ln.Close()
	conns.Wait()
	drained := make(chan struct{})
	go func() { l.streams.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(l.env.limits.ShutdownTimeout):
	}
	l.mu.Lock()
	for c := range l.active {
		_ = c.CloseWithError(DoQNoError, "")
	}
	clear(l.active)
	l.mu.Unlock()
	_ = l.Close()
	return serveErr
}

func (l *doqListener) track(c *quic.Conn, add bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if add {
		l.active[c] = struct{}{}
	} else {
		delete(l.active, c)
	}
}

// serveConn accepts streams until the connection or the listener ends.
// Each stream waits for a worker from the listener-wide pool before it is
// read, so at most Limits.QUICWorkers queries are in flight per listener.
func (l *doqListener) serveConn(ctx context.Context, conn *quic.Conn) {
	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() == nil {
				// Peer closed, idle timeout or a protocol error we raised.
				l.track(conn, false)
			}
			return
		}
		select {
		case l.workers <- struct{}{}:
		case <-ctx.Done():
			st.CancelRead(DoQRequestCancelled)
			st.CancelWrite(DoQRequestCancelled)
			return
		case <-conn.Context().Done():
			l.track(conn, false)
			return
		}
		l.streams.Go(func() {
			defer func() { <-l.workers }()
			l.serveStream(conn, st)
		})
	}
}

func (l *doqListener) serveStream(conn *quic.Conn, st *quic.Stream) {
	lim := l.env.limits
	_ = st.SetReadDeadline(time.Now().Add(lim.ReadTimeout))
	wire, err := readDoQQuery(st)
	if err != nil {
		l.protocolError(conn, err)
		return
	}
	req := new(dns.Msg)
	if err := req.Unpack(wire); err != nil {
		l.protocolError(conn, fmt.Errorf("%w: unpack: %w", errDoQProtocol, err))
		return
	}
	if err := validDoQQuery(req); err != nil {
		l.protocolError(conn, err)
		return
	}

	resp := l.env.handler.resolveFreshID(st.Context(), req, clientFrom(conn.RemoteAddr(), pipeline.ProtoDoQ))
	out, err := resp.Pack()
	if err != nil {
		fail := new(dns.Msg)
		fail.SetRcode(req, dns.RcodeServerFailure)
		fail.Id = 0
		if out, err = fail.Pack(); err != nil {
			st.CancelWrite(DoQInternalError)
			return
		}
	}
	buf := make([]byte, 2+len(out))
	binary.BigEndian.PutUint16(buf, uint16(len(out))) //nolint:gosec // Pack never exceeds 65535 bytes
	copy(buf[2:], out)
	_ = st.SetWriteDeadline(time.Now().Add(lim.WriteTimeout))
	if _, err := st.Write(buf); err != nil {
		st.CancelWrite(DoQInternalError)
		return
	}
	// STREAM FIN: no more data on this stream (RFC 9250 §4.2).
	_ = st.Close()
}

// protocolError closes the connection with DOQ_PROTOCOL_ERROR (RFC 9250
// §4.3.3). Reads that fail because the connection is already gone are not
// protocol errors and need no action.
func (l *doqListener) protocolError(conn *quic.Conn, err error) {
	if conn.Context().Err() != nil {
		return
	}
	l.env.logger.Debug("doq: closing connection", "remote", conn.RemoteAddr().String(), "err", err)
	_ = conn.CloseWithError(DoQProtocolError, "")
	l.track(conn, false)
}

// readDoQQuery reads one length-prefixed message and then requires the
// STREAM FIN (RFC 9250 §4.2 and §4.3.3): a zero length, a FIN before the
// declared length, trailing data or no FIN before the read deadline are
// protocol errors.
func readDoQQuery(r io.Reader) ([]byte, error) {
	var prefix [2]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, fmt.Errorf("%w: reading length: %w", errDoQProtocol, err)
	}
	n := binary.BigEndian.Uint16(prefix[:])
	if n == 0 {
		return nil, fmt.Errorf("%w: zero-length message (pre-RFC draft client?)", errDoQProtocol)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("%w: reading %d-byte message: %w", errDoQProtocol, n, err)
	}
	var extra [1]byte
	m, err := r.Read(extra[:])
	switch {
	case m > 0:
		return nil, fmt.Errorf("%w: data after the query", errDoQProtocol)
	case errors.Is(err, io.EOF):
		return buf, nil
	case err == nil:
		// A zero-byte read without EOF; wait for the FIN once more.
		if m, err = r.Read(extra[:]); m == 0 && errors.Is(err, io.EOF) {
			return buf, nil
		}
		return nil, fmt.Errorf("%w: missing STREAM FIN", errDoQProtocol)
	default:
		return nil, fmt.Errorf("%w: missing STREAM FIN: %w", errDoQProtocol, err)
	}
}

// validDoQQuery applies the message-level rules of RFC 9250 §4.2.1 and
// §5.5.2: the ID must be 0 and edns-tcp-keepalive must not be present.
func validDoQQuery(m *dns.Msg) error {
	if m.Id != 0 {
		return fmt.Errorf("%w: message ID %d, must be 0", errDoQProtocol, m.Id)
	}
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if o.Option() == dns.EDNS0TCPKEEPALIVE {
				return fmt.Errorf("%w: edns-tcp-keepalive option", errDoQProtocol)
			}
		}
	}
	return nil
}
