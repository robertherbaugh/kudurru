package proxyproto

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// DefaultHeaderTimeout bounds how long a stream connection may take to
// deliver its header (or its first bytes) before it is dropped.
const DefaultHeaderTimeout = 5 * time.Second

// Addr is the remote address of a datagram or connection whose client
// address came from an authenticated header. String reports the client;
// writes go to Proxy.
type Addr struct {
	// Client is the original client from the header.
	Client netip.AddrPort
	// Proxy is the transport peer (dnsdist) the bytes arrived from.
	Proxy net.Addr
}

// Network returns the network of the transport peer ("udp" or "tcp").
func (a *Addr) Network() string { return a.Proxy.Network() }

// String returns the client address.
func (a *Addr) String() string { return a.Client.String() }

// AddrPort converts a net.Addr from a UDP or TCP socket into a
// netip.AddrPort with IPv4-mapped addresses unmapped. Unknown address types
// return the zero value.
func AddrPort(a net.Addr) netip.AddrPort {
	var ap netip.AddrPort
	switch v := a.(type) {
	case *net.UDPAddr:
		ap = v.AddrPort()
	case *net.TCPAddr:
		ap = v.AddrPort()
	case *Addr:
		return v.Client
	case nil:
		return netip.AddrPort{}
	default:
		ap, _ = netip.ParseAddrPort(a.String())
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// PacketConn wraps a UDP socket. ReadFrom strips authenticated headers and
// reports the client as an *Addr; dropped datagrams are counted by the
// Policy and never returned. WriteTo sends replies for an *Addr to the
// proxy it came from.
type PacketConn struct {
	net.PacketConn
	Policy *Policy
}

// ReadFrom implements net.PacketConn.
func (c *PacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(b)
		if err != nil {
			return n, addr, err
		}
		payload, client, proxied, derr := c.Policy.Datagram(AddrPort(addr), b[:n])
		if derr != nil {
			// Returning the error would make the server loop exit; a forged
			// datagram must cost the attacker, not the listener.
			continue
		}
		if len(payload) != n {
			n = copy(b, payload)
		}
		if proxied {
			return n, &Addr{Client: client, Proxy: addr}, nil
		}
		return n, addr, nil
	}
}

// WriteTo implements net.PacketConn.
func (c *PacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if a, ok := addr.(*Addr); ok {
		addr = a.Proxy
	}
	return c.PacketConn.WriteTo(b, addr)
}

// Listener wraps a stream listener. Accepted connections read and check the
// header on their first Read or RemoteAddr call, in the connection's own
// goroutine, so a slow client never blocks Accept.
type Listener struct {
	net.Listener
	Policy *Policy
	// HeaderTimeout bounds the header read; zero means DefaultHeaderTimeout.
	HeaderTimeout time.Duration
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	t := l.HeaderTimeout
	if t <= 0 {
		t = DefaultHeaderTimeout
	}
	return &Conn{Conn: c, policy: l.Policy, timeout: t}, nil
}

// Conn is a stream connection whose header is consumed lazily.
type Conn struct {
	net.Conn
	policy  *Policy
	timeout time.Duration

	once    sync.Once
	err     error
	pending []byte
	remote  net.Addr

	mu           sync.Mutex
	userDeadline time.Time
}

// Read implements net.Conn. If the header was refused every Read returns
// the *DropError.
func (c *Conn) Read(b []byte) (int, error) {
	c.once.Do(c.readHeader)
	if c.err != nil {
		return 0, c.err
	}
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// RemoteAddr returns the client: an *Addr when an authenticated header
// named one, otherwise the transport peer.
func (c *Conn) RemoteAddr() net.Addr {
	c.once.Do(c.readHeader)
	return c.remote
}

// Proxied reports whether the client address came from a header.
func (c *Conn) Proxied() bool {
	_, ok := c.RemoteAddr().(*Addr)
	return ok
}

// SetDeadline implements net.Conn, remembering the read deadline so the
// header read can restore it.
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.userDeadline = t
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

// SetReadDeadline implements net.Conn, remembering the deadline so the
// header read can restore it.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.userDeadline = t
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

func (c *Conn) readHeader() {
	peer := c.Conn.RemoteAddr()
	c.remote = peer

	c.mu.Lock()
	user := c.userDeadline
	c.mu.Unlock()
	d := time.Now().Add(c.timeout)
	if !user.IsZero() && user.Before(d) {
		d = user
	}
	_ = c.Conn.SetReadDeadline(d)

	consumed, client, proxied, err := c.policy.Stream(AddrPort(peer), c.Conn)

	c.mu.Lock()
	_ = c.Conn.SetReadDeadline(c.userDeadline)
	c.mu.Unlock()

	if err != nil {
		c.err = err
		return
	}
	c.pending = consumed
	if proxied {
		c.remote = &Addr{Client: client, Proxy: peer}
	}
}
