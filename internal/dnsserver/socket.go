package dnsserver

import (
	"context"
	"errors"
	"net"
	"runtime"
)

// Multi-socket listeners (dev-docs/design-record.md §13.1, an M1 carry-over). A
// udp, tcp or dot Spec binds N sockets on the same address with SO_REUSEPORT,
// each served by its own miekg/dns read loop sharing the Handler, so the
// kernel spreads datagrams and connections over N receive queues instead
// of one. The approach follows CoreDNS's multisocket plugin (Apache-2.0,
// NOTICE).
//
// SO_REUSEPORT load balancing is used on Linux only (the BSDs give the
// option different semantics); elsewhere every listener binds one socket.

// ReuseportSupported reports whether this platform binds more than one
// socket per UDP, TCP or DoT listener.
const ReuseportSupported = reuseportSupported

// SocketCount resolves Options.Sockets: zero or negative means
// runtime.GOMAXPROCS(0); platforms without SO_REUSEPORT always get 1.
func SocketCount(n int) int {
	if !reuseportSupported {
		return 1
	}
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	return max(n, 1)
}

// listenPackets binds n UDP sockets on address. The first bind is a plain
// one (no SO_REUSEPORT), so a port that is already taken, even by another
// process that set SO_REUSEPORT itself, fails here exactly as a
// single-socket bind would; the probe is then released and the group bound
// with SO_REUSEPORT on the probe's port (which also resolves port 0).
func listenPackets(address string, n int) ([]net.PacketConn, error) {
	probe, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, err
	}
	if n <= 1 {
		return []net.PacketConn{probe}, nil
	}
	bound := rebindAddress(address, probe.LocalAddr())
	_ = probe.Close()
	lc := net.ListenConfig{Control: reuseportControl}
	out := make([]net.PacketConn, 0, n)
	for range n {
		pc, err := lc.ListenPacket(context.Background(), "udp", bound)
		if err != nil {
			_ = closeAllFunc(out)()
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

// listenStreams is listenPackets for TCP listeners. A TCP socket that was
// listening without connections leaves no TIME_WAIT state behind, so the
// group can take the probe's port immediately.
func listenStreams(address string, n int) ([]net.Listener, error) {
	probe, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	if n <= 1 {
		return []net.Listener{probe}, nil
	}
	bound := rebindAddress(address, probe.Addr())
	_ = probe.Close()
	lc := net.ListenConfig{Control: reuseportControl}
	out := make([]net.Listener, 0, n)
	for range n {
		ln, err := lc.Listen(context.Background(), "tcp", bound)
		if err != nil {
			_ = closeAllFunc(out)()
			return nil, err
		}
		out = append(out, ln)
	}
	return out, nil
}

// rebindAddress keeps the configured host (so "0.0.0.0:53" stays IPv4-only
// and ":53" dual-stack) and takes the port the probe actually bound.
func rebindAddress(address string, bound net.Addr) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return bound.String()
	}
	_, port, err := net.SplitHostPort(bound.String())
	if err != nil {
		return bound.String()
	}
	return net.JoinHostPort(host, port)
}

// closeAllFunc returns one closer for every socket of a listener, joining
// their errors.
func closeAllFunc[T interface{ Close() error }](xs []T) func() error {
	return func() error {
		var errs []error
		for _, x := range xs {
			if err := x.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}
