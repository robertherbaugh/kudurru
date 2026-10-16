package dhcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

const (
	serverPort    = 67
	clientPort    = 68
	maxPacketSize = 4096
	packetTimeout = 2 * time.Second
)

// leaseBackend keeps protocol tests independent of the persistent lease store.
type leaseBackend interface {
	Offer(context.Context, Client, netip.Addr) (Lease, error)
	Acknowledge(context.Context, Client, netip.Addr, RequestMode) (Lease, error)
	Release(context.Context, Client, netip.Addr) error
	Decline(context.Context, Client, netip.Addr) error
}

// Server serves one DHCPv4 scope, directly or through trusted relays. Listen
// must succeed before Run is called. The caller owns the lease store's lifetime.
type Server struct {
	cfg    Config
	leases leaseBackend
	logger *slog.Logger

	processMu sync.Mutex // packets and managed configuration commits
	dispatch  func(context.Context, []byte, *net.UDPAddr) ([]byte, *net.UDPAddr, error)

	mu       sync.Mutex
	conn     net.PacketConn
	listen   func(Config) (net.PacketConn, error)
	running  bool
	closed   bool
	closeErr error
}

// NewServer validates configuration without binding a socket.
func NewServer(cfg Config, leases *LeaseStore, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("dhcp: configuration: %w", err)
	}
	if leases == nil {
		return nil, errors.New("dhcp: lease store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg = normalizedConfig(cfg)
	return &Server{cfg: cfg, leases: leases, logger: logger, listen: listenDHCP}, nil
}

// Listen verifies the interface and server address and exclusively binds UDP 67.
// It is idempotent until Close. No packets are processed before Run.
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if s.conn != nil {
		return nil
	}
	conn, err := s.listen(s.cfg)
	if err != nil {
		return fmt.Errorf("dhcp: listen: %w", err)
	}
	s.conn = conn
	return nil
}

// Run processes packets serially, bounding memory, storage concurrency, and
// each packet's processing time. Cancellation closes the socket to unblock I/O.
// Handler failures drop only the affected packet and are logged; unexpected
// socket failures remain fatal to the supervising process.
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		return net.ErrClosed
	}
	if s.running {
		s.mu.Unlock()
		return errors.New("dhcp: server is already running")
	}
	if s.conn == nil {
		s.mu.Unlock()
		return errors.New("dhcp: Listen must succeed before Run")
	}
	s.running = true
	conn := s.conn
	s.mu.Unlock()
	defer func() { _ = s.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()

	buf := make([]byte, maxPacketSize+1)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, source, err := conn.ReadFrom(buf)
		if err != nil {
			if s.stopped(ctx) {
				return nil
			}
			// An ICMP error from an earlier UDP reply can be reported
			// on ReadFrom. It is not a failure of the listening socket.
			if packetNetworkError(err) {
				continue
			}
			return fmt.Errorf("dhcp: read: %w", err)
		}
		if n <= 0 || n > maxPacketSize {
			continue
		}
		peer, ok := source.(*net.UDPAddr)
		if !ok {
			continue
		}
		packetCtx, cancel := context.WithTimeout(ctx, packetTimeout)
		handler := s.handlePacket
		if s.dispatch != nil {
			handler = s.dispatch
		}
		reply, destination, err := handler(packetCtx, buf[:n], peer)
		if err != nil {
			cancel()
			if s.stopped(ctx) {
				return nil
			}
			level := slog.LevelError
			if errors.Is(err, context.DeadlineExceeded) {
				level = slog.LevelDebug
			}
			if s.logger != nil {
				s.logger.Log(ctx, level, "DHCP packet dropped", "interface", s.cfg.Interface, "error", err)
			}
			continue
		}
		if reply != nil && packetCtx.Err() == nil {
			deadline, _ := packetCtx.Deadline()
			if err = conn.SetWriteDeadline(deadline); err == nil {
				var written int
				written, err = conn.WriteTo(reply, destination)
				if err == nil && written != len(reply) {
					err = errors.New("short datagram write")
				}
			}
		}
		cancel()
		if err != nil {
			if s.stopped(ctx) {
				return nil
			}
			if errors.Is(err, context.DeadlineExceeded) || packetNetworkError(err) {
				if s.logger != nil {
					s.logger.Debug("DHCP packet dropped", "error", err)
				}
				continue
			}
			return fmt.Errorf("dhcp: process packet: %w", err)
		}
	}
}

// UDP may report asynchronous ICMP errors from an earlier reply, including
// EACCES for an administratively prohibited destination. Those errors belong
// to that datagram and must not terminate the listener.
func packetNetworkError(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout() ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EMSGSIZE) || errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPROTO)
}

func (s *Server) stopped(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ctx.Err() != nil || s.closed
}

// Close is idempotent, interrupts Run, and does not close the caller's store.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.conn != nil {
			s.closeErr = s.conn.Close()
		}
	}
	return s.closeErr
}
