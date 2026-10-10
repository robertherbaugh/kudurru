// Package upstream is the terminal forwarding stage. The v0.1 design uses
// dnsproxy upstream groups with strategies (docs/architecture.md §3.3); this
// scaffold implements the "strict" strategy over miekg/dns so the chain is
// end-to-end testable. Replace the exchange with dnsproxy in milestone M1.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Stage implements pipeline.Stage.
type Stage struct {
	// Group is the upstream group name recorded in Response.Upstream.
	Group   string
	Servers []string
	Timeout time.Duration

	udp *dns.Client
	tcp *dns.Client
}

// New builds a strict-order forwarder. Servers are host:port; a missing port
// defaults to 53.
func New(group string, servers []string, timeout time.Duration) (*Stage, error) {
	if len(servers) == 0 {
		return nil, errors.New("upstream: at least one server is required")
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	norm := make([]string, len(servers))
	for i, s := range servers {
		s = strings.TrimPrefix(s, "udp://")
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, "53")
		}
		norm[i] = s
	}
	return &Stage{
		Group:   group,
		Servers: norm,
		Timeout: timeout,
		udp:     &dns.Client{Net: "udp", Timeout: timeout, UDPSize: dns.DefaultMsgSize},
		tcp:     &dns.Client{Net: "tcp", Timeout: timeout},
	}, nil
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "upstream" }

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	out := req.Msg.Copy()
	out.Id = dns.Id()
	out.RecursionDesired = true

	ctx := req.Ctx
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}

	var lastErr error
	for _, server := range s.Servers {
		in, err := s.exchange(ctx, out, server)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", server, err)
			continue
		}
		in.Id = req.Msg.Id
		return &pipeline.Response{
			Msg:       in,
			Status:    pipeline.StatusForwarded,
			Upstream:  s.Group + "/" + server,
			Cacheable: true,
		}, nil
	}

	status := pipeline.StatusServfailUpstream
	var nerr net.Error
	if errors.As(lastErr, &nerr) && nerr.Timeout() {
		status = pipeline.StatusTimeoutUpstream
	}
	r := pipeline.Reply(req, dns.RcodeServerFailure, status)
	r.Upstream = s.Group
	return r, nil
}

// exchange talks UDP first and retries over TCP when the answer is
// truncated. The client's own transport is irrelevant to the upstream hop.
func (s *Stage) exchange(ctx context.Context, m *dns.Msg, server string) (*dns.Msg, error) {
	in, _, err := s.udp.ExchangeContext(ctx, m, server)
	if err != nil {
		return nil, err
	}
	if in.Truncated {
		if tin, _, terr := s.tcp.ExchangeContext(ctx, m, server); terr == nil {
			return tin, nil
		}
	}
	return in, nil
}
