// Package dnsserver owns the DNS listeners and hands every message to the
// pipeline (docs/architecture.md §3.3).
//
// A Registry holds the listeners of a node keyed by protocol (udp, tcp,
// dot, doh, doq, doh3) and shares one certificate-reload path
// (tlsreload) between all TLS listeners (decision D67). Do53 and DoT run on
// miekg/dns servers, DoH on net/http (HTTP/1.1 and HTTP/2), DoQ and DoH3 on
// quic-go; all of them feed the same pipeline.Chain through Handler and set
// pipeline.Client.Proto. The udp, tcp and dot listeners accept PROXY
// protocol v2 headers only from listeners.proxyProtocolFrom peers carrying
// the shared-secret TLV (proxyproto; D45, D56).
//
// miekg/dns types stay inside this package (§2); quic-go types never leave
// it.
package dnsserver

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Server serves one address over UDP and/or TCP. It predates Registry and
// is kept for callers that only need Do53; it runs the same listeners.
type Server struct {
	Address   string
	Protocols []string // "udp", "tcp"
	Chain     *pipeline.Chain
	Logger    *slog.Logger
	// QueryTimeout bounds the whole pipeline for one query.
	QueryTimeout time.Duration
	// Proxy, when enabled, authenticates PROXYv2 headers on these sockets.
	Proxy *proxyproto.Policy

	mu        sync.Mutex
	listeners []*dnsListener
}

// New returns a server for address with the given protocols.
func New(address string, protocols []string, chain *pipeline.Chain, logger *slog.Logger) *Server {
	if len(protocols) == 0 {
		protocols = []string{"udp", "tcp"}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Address: address, Protocols: protocols, Chain: chain, Logger: logger, QueryTimeout: DefaultQueryTimeout}
}

func (s *Server) handler() *Handler {
	return &Handler{Chain: s.Chain, Logger: s.Logger, QueryTimeout: s.QueryTimeout}
}

// Start binds the listeners and serves until ctx is cancelled. It returns
// once every listener has shut down. Bind errors are returned immediately.
func (s *Server) Start(ctx context.Context) error {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	e := &env{handler: s.handler(), logger: logger, proxy: s.Proxy, limits: Limits{}.withDefaults()}
	var ls []*dnsListener
	for _, p := range s.Protocols {
		if p != string(ProtoUDP) && p != string(ProtoTCP) {
			closeAll(ls)
			return errors.New("dnsserver: unsupported protocol " + p)
		}
		l, err := newDNSListener(Spec{Protocol: Protocol(p), Address: s.Address}, e)
		if err != nil {
			closeAll(ls)
			return err
		}
		dl := l.(*dnsListener)
		if err := dl.Bind(); err != nil {
			closeAll(ls)
			return err
		}
		ls = append(ls, dl)
	}
	s.mu.Lock()
	s.listeners = ls
	s.mu.Unlock()
	logger.Info("dns listener started", "address", s.Address, "protocols", s.Protocols)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, len(ls))
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Go(func() {
			if err := l.Serve(ctx); err != nil {
				errCh <- err
				cancel()
			}
		})
	}
	wg.Wait()
	close(errCh)
	return <-errCh
}

func closeAll(ls []*dnsListener) {
	for _, l := range ls {
		_ = l.Close()
	}
}

// Addrs returns the bound addresses (useful when the port was 0).
func (s *Server) Addrs() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, l := range s.listeners {
		if a := l.Addr(); a != nil {
			out[string(l.spec.Protocol)] = a.String()
		}
	}
	return out
}

// ServeDNS implements dns.Handler.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	proto := pipeline.ProtoUDP
	if a := w.RemoteAddr(); a != nil && a.Network() == "tcp" {
		proto = pipeline.ProtoTCP
	}
	serveMiekg(s.handler(), proto, w, r)
}
