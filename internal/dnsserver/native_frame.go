package dnsserver

import (
	"bytes"
	"crypto/tls"
	"net"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Raw carriers own at most one bounded frame per datagram/serial connection.
// They never acquire serving leases: rejected or ignored frames cannot leak one.
type nativeFrame struct {
	net.Addr
	wire     []byte
	received time.Time
}

type nativeSession struct {
	net.Addr
	session *dns.SessionUDP
}

type sessionPacket struct {
	net.PacketConn
	udp *net.UDPConn
}

func (c *sessionPacket) ReadFrom(b []byte) (int, net.Addr, error) {
	n, session, err := dns.ReadFromSessionUDP(c.udp, b)
	if err != nil {
		return n, nil, err
	}
	return n, &nativeSession{Addr: session.RemoteAddr(), session: session}, nil
}
func (c *sessionPacket) WriteTo(b []byte, addr net.Addr) (int, error) {
	session, ok := addr.(*nativeSession)
	if !ok {
		return c.PacketConn.WriteTo(b, addr)
	}
	return dns.WriteToSessionUDP(c.udp, b, session.session)
}

// Preserve ancillary destination data even though miekg sees a generic carrier.
func preserveUDPSession(pc net.PacketConn) net.PacketConn {
	if udp, ok := pc.(*net.UDPConn); ok {
		return &sessionPacket{PacketConn: pc, udp: udp}
	}
	return pc
}

type nativePacket struct{ net.PacketConn }

func (c *nativePacket) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if err != nil {
		return n, addr, err
	}
	return n, &nativeFrame{Addr: addr, wire: bytes.Clone(b[:n]), received: time.Now()}, nil
}
func (c *nativePacket) WriteTo(b []byte, addr net.Addr) (int, error) {
	if frame, ok := addr.(*nativeFrame); ok {
		addr = frame.Addr
	}
	return c.PacketConn.WriteTo(b, addr)
}

type nativeConn struct {
	net.Conn
	frame atomic.Pointer[nativeFrame]
}

func (c *nativeConn) RemoteAddr() net.Addr {
	if frame := c.frame.Load(); frame != nil {
		return frame
	}
	return c.Conn.RemoteAddr()
}
func (c *nativeConn) ConnectionState() tls.ConnectionState {
	if tlsConn, ok := c.Conn.(interface{ ConnectionState() tls.ConnectionState }); ok {
		return tlsConn.ConnectionState()
	}
	return tls.ConnectionState{}
}

type nativeListener struct{ net.Listener }

func (l nativeListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &nativeConn{Conn: conn}, nil
}

type nativeReader struct{ dns.Reader }

func (r nativeReader) ReadTCP(conn net.Conn, timeout time.Duration) ([]byte, error) {
	wire, err := r.Reader.ReadTCP(conn, timeout)
	if framed, ok := conn.(*nativeConn); ok {
		framed.frame.Store(nil)
		if err == nil {
			framed.frame.Store(&nativeFrame{Addr: framed.Conn.RemoteAddr(), wire: bytes.Clone(wire), received: time.Now()})
		}
	}
	return wire, err
}
func (r nativeReader) ReadPacketConn(pc net.PacketConn, timeout time.Duration) ([]byte, net.Addr, error) {
	return r.Reader.(dns.PacketConnReader).ReadPacketConn(pc, timeout)
}
