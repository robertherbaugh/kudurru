// Package ingress is the first pipeline stage: the per-prefix QPM rate
// limiter (ratelimit.go), message sanity checks and the locally answered
// CHAOS and health names (dev-docs/design-record.md §7.1 step 2).
package ingress

import (
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/nodehealth"
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
	// GroupExempt identifies an eligible UDP client from the request's captured
	// policy and inventory owners. Nil grants no group exemption. The callback
	// must retain that identification for the normal identify stage.
	GroupExempt func(*pipeline.Request) (bool, error)
	// Lameduck reports whether the node is draining (D51): the health name
	// then answers SERVFAIL, so dnsdist's must-resolve health check takes
	// the node out of every backend pool while it still serves the queries
	// already routed to it. nil never drains.
	Lameduck func() bool
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "ingress" }

func (l *RateLimiter) requestExempt(req *pipeline.Request) bool {
	if req.Client.Proto != pipeline.ProtoUDP || l.Exempt(req.Client.Addr.Addr()) {
		return true
	}
	m := req.Msg
	return m != nil && len(m.Question) == 1 && nodehealth.IsName(m.Question[0].Name)
}

// Check returns the limiter's verdict for req: UDP queries only, never for
// exempt clients (loopback, ratelimit.exempt) or the health name. The
// client is req.Client.Addr, the PROXYv2-attributed address when dnsdist
// forwarded the query.
func (l *RateLimiter) Check(req *pipeline.Request) Verdict {
	if l.requestExempt(req) {
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
		exempt := false
		if s.GroupExempt != nil && s.Limiter.Enabled() && !s.Limiter.requestExempt(req) {
			var err error
			exempt, err = s.GroupExempt(req)
			if err != nil {
				return nil, err
			}
		}
		verdict := VerdictAllow
		if !exempt {
			verdict = s.Limiter.Check(req)
		}
		switch verdict {
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
	if m.Opcode != dns.OpcodeQuery && m.Opcode != dns.OpcodeNotify && m.Opcode != dns.OpcodeUpdate {
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
	if nodehealth.Reserved(q.Name) {
		if q.Qclass == dns.ClassINET && s.Lameduck != nil && s.Lameduck() {
			return pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusSpecialDomain), nil
		}
		if m.Opcode != dns.OpcodeQuery || q.Qclass != dns.ClassINET || q.Qtype != dns.TypeTXT || !nodehealth.Matches(q.Name, s.NodeID) {
			response := pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusSpecialDomain)
			response.Msg.Authoritative = true
			return response, nil
		}
		var epoch uint64
		if s.Epoch != nil {
			epoch = s.Epoch()
		}
		return s.txt(req, q, fmt.Sprintf("%s %d", s.NodeID, epoch)), nil
	}
	// Protocol operations must reach authoritative's dedicated handlers.
	// Their SOA zone section is not a CHAOS/health/ANY query.
	if m.Opcode != dns.OpcodeQuery {
		return next(req)
	}
	if q.Qtype == dns.TypeANY {
		return pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL), nil
	}
	if q.Qclass == dns.ClassCHAOS && q.Qtype == dns.TypeTXT {
		if txt, ok := s.chaos(q.Name); ok {
			return s.txt(req, q, txt), nil
		}
	}
	if q.Qclass == dns.ClassINET && strings.EqualFold(q.Name, HealthName) && s.Lameduck != nil && s.Lameduck() {
		return pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusSpecialDomain), nil
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
