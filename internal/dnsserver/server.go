// Package dnsserver binds the UDP and TCP listeners and hands every message
// to the pipeline (docs/architecture.md §3.3). DoT/DoH listeners, PROXYv2
// parsing and multi-socket reuse port land in milestone M1.
package dnsserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Server serves one address over UDP and/or TCP.
type Server struct {
	Address   string
	Protocols []string // "udp", "tcp"
	Chain     *pipeline.Chain
	Logger    *slog.Logger
	// QueryTimeout bounds the whole pipeline for one query.
	QueryTimeout time.Duration

	mu      sync.Mutex
	servers []*dns.Server
}

// New returns a server for address with the given protocols.
func New(address string, protocols []string, chain *pipeline.Chain, logger *slog.Logger) *Server {
	if len(protocols) == 0 {
		protocols = []string{"udp", "tcp"}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Address: address, Protocols: protocols, Chain: chain, Logger: logger, QueryTimeout: 5 * time.Second}
}

// Start binds the listeners and serves until ctx is cancelled. It returns
// once every listener has shut down. Bind errors are returned immediately.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, len(s.Protocols))
	for _, proto := range s.Protocols {
		srv, err := s.listen(proto)
		if err != nil {
			s.shutdown()
			return err
		}
		s.mu.Lock()
		s.servers = append(s.servers, srv)
		s.mu.Unlock()
		go func() { errCh <- srv.ActivateAndServe() }()
	}
	s.Logger.Info("dns listener started", "address", s.Address, "protocols", s.Protocols)

	select {
	case <-ctx.Done():
		s.shutdown()
		return nil
	case err := <-errCh:
		s.shutdown()
		return err
	}
}

// Addrs returns the bound addresses (useful when the port was 0).
func (s *Server) Addrs() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, srv := range s.servers {
		if srv.PacketConn != nil {
			out["udp"] = srv.PacketConn.LocalAddr().String()
		}
		if srv.Listener != nil {
			out["tcp"] = srv.Listener.Addr().String()
		}
	}
	return out
}

func (s *Server) listen(proto string) (*dns.Server, error) {
	srv := &dns.Server{Handler: s, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
	switch proto {
	case "udp":
		pc, err := net.ListenPacket("udp", s.Address)
		if err != nil {
			return nil, err
		}
		srv.PacketConn = pc
		srv.Net = "udp"
	case "tcp":
		l, err := net.Listen("tcp", s.Address)
		if err != nil {
			return nil, err
		}
		srv.Listener = l
		srv.Net = "tcp"
	default:
		return nil, errors.New("dnsserver: unsupported protocol " + proto)
	}
	return srv, nil
}

func (s *Server) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.servers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.ShutdownContext(ctx)
		cancel()
	}
	s.servers = nil
}

// ServeDNS implements dns.Handler.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), s.QueryTimeout)
	defer cancel()

	req := &pipeline.Request{
		Ctx:      ctx,
		Msg:      r,
		Received: time.Now(),
		Client:   clientFrom(w.RemoteAddr()),
	}
	req.Deadline = req.Received.Add(s.QueryTimeout)

	resp, err := s.Chain.Serve(req)
	if err != nil || resp == nil || resp.Msg == nil {
		if err != nil && !errors.Is(err, pipeline.ErrNoAnswer) {
			s.Logger.Debug("pipeline error", "err", err, "q", req.Question().Name)
		}
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}
	m := resp.Msg
	m.Id = r.Id
	if req.Client.Proto == pipeline.ProtoUDP {
		size := dns.MinMsgSize
		if opt := r.IsEdns0(); opt != nil && int(opt.UDPSize()) > size {
			size = int(opt.UDPSize())
		}
		m.Truncate(size)
	}
	if err := w.WriteMsg(m); err != nil {
		s.Logger.Debug("write failed", "err", err)
	}
}

func clientFrom(addr net.Addr) pipeline.Client {
	c := pipeline.Client{Proto: pipeline.ProtoUDP}
	if addr == nil {
		return c
	}
	if addr.Network() == "tcp" {
		c.Proto = pipeline.ProtoTCP
	}
	if ap, err := netip.ParseAddrPort(addr.String()); err == nil {
		c.Addr = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return c
}
