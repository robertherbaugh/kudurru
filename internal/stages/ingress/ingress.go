// Package ingress is the first pipeline stage: the per-prefix QPM rate
// limiter (ratelimit.go), message sanity checks and the locally answered
// CHAOS and health names (docs/architecture.md §7.1 step 2).
package ingress

import (
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// HealthName is answered locally with TXT "<node-id> <serial-epoch>" and is
// used by dnsdist health checks, keepalived and the liveness self-query. It
// is never logged.
const HealthName = "health.kudurru.internal."

// Stage implements pipeline.Stage.
type Stage struct {
	NodeID   string
	Hostname string
	Version  string
	// Epoch is reported in the health TXT; the serial epoch of the zonecache
	// once that exists.
	Epoch func() uint64
	// Limiter is the per-prefix QPM limiter (ratelimit.go); nil disables
	// rate limiting.
	Limiter *RateLimiter
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "ingress" }

// Check returns the limiter's verdict for req: UDP queries only, never for
// exempt clients (loopback, ratelimit.exempt) or the health name. The
// client is req.Client.Addr, the PROXYv2-attributed address when dnsdist
// forwarded the query.
func (l *RateLimiter) Check(req *pipeline.Request) Verdict {
	if req.Client.Proto != pipeline.ProtoUDP || l.Exempt(req.Client.Addr.Addr()) {
		return VerdictAllow
	}
	if m := req.Msg; m != nil && len(m.Question) == 1 && strings.EqualFold(m.Question[0].Name, HealthName) {
		return VerdictAllow
	}
	now := req.Received
	if l.now != nil {
		now = l.now()
	} else if now.IsZero() {
		now = time.Now()
	}
	return l.Allow(req.Client.Addr.Addr(), now)
}

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	if s.Limiter != nil {
		switch s.Limiter.Check(req) {
		case VerdictSlip:
			r := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusBlockedRateLimit)
			r.Msg.Truncated = true
			return r, nil
		case VerdictDrop:
			// A nil message asks the udp listener to send nothing
			// (dnsserver.Handler).
			return &pipeline.Response{Status: pipeline.StatusBlockedRateLimit}, nil
		}
	}
	m := req.Msg
	if m.Opcode != dns.OpcodeQuery {
		return pipeline.Reply(req, dns.RcodeNotImplemented, pipeline.StatusDropped), nil
	}
	if len(m.Question) != 1 {
		return pipeline.Reply(req, dns.RcodeFormatError, pipeline.StatusDropped), nil
	}
	if opt := m.IsEdns0(); opt != nil && opt.Version() != 0 {
		r := pipeline.Reply(req, dns.RcodeBadVers, pipeline.StatusDropped)
		r.Msg.SetEdns0(dns.MinMsgSize, false)
		r.Msg.IsEdns0().SetVersion(0)
		return r, nil
	}
	q := m.Question[0]
	if q.Qtype == dns.TypeANY {
		return pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL), nil
	}
	if q.Qclass == dns.ClassCHAOS && q.Qtype == dns.TypeTXT {
		if txt, ok := s.chaos(q.Name); ok {
			return s.txt(req, q, txt), nil
		}
	}
	if q.Qclass == dns.ClassINET && q.Qtype == dns.TypeTXT && strings.EqualFold(q.Name, HealthName) {
		var epoch uint64
		if s.Epoch != nil {
			epoch = s.Epoch()
		}
		return s.txt(req, q, fmt.Sprintf("%s %d", s.NodeID, epoch)), nil
	}
	return next(req)
}

func (s *Stage) chaos(name string) (string, bool) {
	switch strings.ToLower(name) {
	case "id.server.", "hostname.bind.":
		if s.Hostname != "" {
			return s.Hostname, true
		}
		return s.NodeID, true
	case "version.server.", "version.bind.":
		return "kudurru " + s.Version, true
	}
	return "", false
}

func (s *Stage) txt(req *pipeline.Request, q dns.Question, value string) *pipeline.Response {
	r := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusSpecialDomain)
	r.Msg.Authoritative = true
	r.Msg.Answer = []dns.RR{&dns.TXT{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: q.Qclass, Ttl: 0},
		Txt: []string{value},
	}}
	return r
}
