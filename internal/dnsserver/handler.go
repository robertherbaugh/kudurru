package dnsserver

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// DefaultQueryTimeout bounds the whole pipeline for one query.
const DefaultQueryTimeout = 5 * time.Second

// Handler runs one DNS message through the pipeline on behalf of any
// transport. Every listener (Do53, DoT, DoH, DoQ, DoH3) funnels into
// Resolve, so stages see the same Request shape whatever the transport.
type Handler struct {
	Chain  *pipeline.Chain
	Logger *slog.Logger
	// QueryTimeout bounds the pipeline for one query; zero means
	// DefaultQueryTimeout.
	QueryTimeout time.Duration
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func (h *Handler) timeout() time.Duration {
	if h.QueryTimeout > 0 {
		return h.QueryTimeout
	}
	return DefaultQueryTimeout
}

// Resolve answers r on behalf of client. It never returns nil: when the
// chain fails or produces nothing the answer is SERVFAIL. The response ID
// equals r.Id. ctx carries transport cancellation (an HTTP/2 or QUIC stream
// reset); Resolve adds the query deadline.
//
// A stage may ask the transport to send nothing at all: the ingress rate
// limiter's drop (dev-docs/design-record.md §7.1 step 2) answers with a Response
// whose Msg is nil and whose Status is pipeline.StatusDropped or
// pipeline.StatusBlockedRateLimit, the control stage returns
// pipeline.ErrDrop. The listeners honour both: udp writes nothing, tcp and
// dot close the connection, doq resets the stream (DOQ_REQUEST_CANCELLED)
// and doh/doh3 abort the HTTP exchange. Resolve itself, which must return
// a message, answers SERVFAIL.
func (h *Handler) Resolve(ctx context.Context, r *dns.Msg, client pipeline.Client) *dns.Msg {
	if m, _ := h.resolve(ctx, r, client); m != nil {
		return m
	}
	return servfail(r)
}

// IsDrop reports whether resp asks the transport to send nothing (see
// Resolve; pipeline.ErrDrop is the error form).
func IsDrop(resp *pipeline.Response) bool {
	return resp != nil && resp.Msg == nil &&
		(resp.Status == pipeline.StatusDropped || resp.Status == pipeline.StatusBlockedRateLimit)
}

func servfail(r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	return m
}

// resolve is Resolve that returns nil when the pipeline dropped the query
// and, for stream transports, the rest of a multi-message answer
// (pipeline.Response.Transfer, an AXFR/IXFR stream). When the stream is
// non-nil it yields every message of the answer, starting with the
// returned one, and the caller writes those instead.
func (h *Handler) resolve(ctx context.Context, r *dns.Msg, client pipeline.Client) (*dns.Msg, iter.Seq[*dns.Msg]) {
	timeout := h.timeout()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := &pipeline.Request{Ctx: ctx, Msg: r, Received: time.Now(), Client: client}
	req.Deadline = req.Received.Add(timeout)

	resp, err := h.Chain.Serve(req)
	if (err == nil && IsDrop(resp)) || errors.Is(err, pipeline.ErrDrop) {
		return nil, nil
	}
	if err != nil || resp == nil || resp.Msg == nil {
		if err != nil && !errors.Is(err, pipeline.ErrNoAnswer) {
			h.logger().Debug("pipeline error", "err", err, "q", req.Question().Name, "proto", client.Proto.String())
		}
		return servfail(r), nil
	}
	m := resp.Msg
	m.Id = r.Id
	if resp.Transfer == nil {
		return m, nil
	}
	id := r.Id
	return m, func(yield func(*dns.Msg) bool) {
		for x := range resp.Transfer {
			x.Id = id
			if !yield(x) {
				return
			}
		}
	}
}

// resolveFreshID is Resolve for transports whose clients send a fixed
// message ID: 0 is mandatory on DoQ (RFC 9250 §4.2.1) and recommended on
// DoH (RFC 8484 §4.1). The pipeline sees a random ID so nothing downstream
// (a forwarder bridging to UDP, a cache key, the query log) can reuse the
// client's fixed ID, the CVE-2026-47703 class [go-dns-stack §4.1]; the
// response carries the client's ID again.
//
// It returns nil when the pipeline dropped the query; the caller then sends
// nothing (Resolve).
func (h *Handler) resolveFreshID(ctx context.Context, r *dns.Msg, client pipeline.Client) *dns.Msg {
	orig := r.Id
	r.Id = dns.Id()
	m, _ := h.resolve(ctx, r, client)
	r.Id = orig
	if m != nil {
		m.Id = orig
	}
	return m
}

// clientFrom describes the peer of a socket-level transport. An address
// produced by an authenticated PROXYv2 header becomes the client, and the
// proxy that sent it becomes ProxiedFrom.
func clientFrom(addr net.Addr, proto pipeline.Proto) pipeline.Client {
	c := pipeline.Client{Proto: proto}
	if pa, ok := addr.(*proxyproto.Addr); ok {
		c.Addr = pa.Client
		c.ProxiedFrom = proxyproto.AddrPort(pa.Proxy).Addr()
		c.ClientID = pa.ClientID
		return c
	}
	c.Addr = proxyproto.AddrPort(addr)
	return c
}
