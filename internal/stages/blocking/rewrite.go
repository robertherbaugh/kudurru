package blocking

import (
	"context"
	"strings"

	"github.com/miekg/dns"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

type rewriteContextKey struct{}
type rewriteTrace struct{ names []string }

const maxRewriteHops = 16

func (s *Stage) rewrite(req *pipeline.Request, d *policy.Decision, bd engine.Decision, ctx engine.Context, next pipeline.Next) (*pipeline.Response, error) {
	q := req.Question()
	ttl := d.BlockTTL
	if ttl <= 0 {
		ttl = s.live.Load().blockTTL
	}
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.RecursionAvailable = true
	m.Rcode = bd.Rewrite.Rcode()
	out := &pipeline.Response{Msg: m, Status: pipeline.StatusLocalRecord, List: bd.List, Rule: bd.Rule(), Origin: pipeline.OriginSynthetic, Modified: true}
	if q.Qclass == dns.ClassINET {
		m.Answer = bd.Rewrite.Records(q.Name, q.Qtype, q.Qclass, ttlSeconds(ttl))
	}
	target := bd.Rewrite.Target()
	if target != "" && q.Qtype != dns.TypeCNAME && q.Qclass == dns.ClassINET {
		sub, loop := s.targetRequest(req, target)
		if loop {
			m.Rcode, m.Answer = dns.RcodeServerFailure, nil
			out.Status, out.Origin = pipeline.StatusServfailUpstream, pipeline.OriginFailure
			finishRewrite(req, m, "rewrite loop")
			return out, nil
		}
		resolve := s.opts.ResolveTarget
		if resolve == nil {
			resolve = func(r *pipeline.Request) (*pipeline.Response, error) { return s.Serve(r, next) }
		}
		resp, err := resolve(sub)
		if err != nil {
			return nil, err
		}
		if resp == nil || resp.Msg == nil {
			return nil, pipeline.ErrNoAnswer
		}
		m.Rcode = resp.Msg.Rcode
		// A failed target must remain failed, including expired/unavailable local
		// authority. Never turn its SERVFAIL into a positive synthetic CNAME.
		if m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError {
			return failedRewriteTarget(req, out, resp), nil
		}
		m.Answer = append(m.Answer, rewriteCopies(resp.Msg.Answer)...)
		m.Ns = rewriteCopies(resp.Msg.Ns)
		m.Extra = rewriteCopies(resp.Msg.Extra)
		// OPT belongs to the synthesized request/response, not the target.
		m.Extra = dnsExtraWithoutOPT(m.Extra)
		out.Origin = pipeline.OriginMixed
		out.Upstream = resp.Upstream
		out.Security = resp.Security
	}
	finishRewrite(req, m, bd.List)
	if m.Rcode == dns.RcodeSuccess {
		if rd := s.inspectContext(d, ctx, m); rd.Action == engine.ActionBlock {
			return s.block(req, d, rd), nil
		}
	}
	return out, nil
}

// Preserve the failing dependency's EDE and security diagnostics while keeping
// the original question and removing a misleading positive alias.
func failedRewriteTarget(req *pipeline.Request, synthetic, dependency *pipeline.Response) *pipeline.Response {
	out := *dependency
	out.Msg = dependency.Msg.Copy()
	out.Msg.Id = req.Msg.Id
	out.Msg.Question = append([]dns.Question(nil), req.Msg.Question...)
	out.Msg.Answer, out.Msg.Ns = nil, nil
	out.Msg.Authoritative, out.Msg.AuthenticatedData = false, false
	out.Msg.CheckingDisabled = req.Msg.CheckingDisabled
	out.Origin, out.Modified, out.Cacheable = pipeline.OriginFailure, true, false
	out.List, out.Rule = synthetic.List, synthetic.Rule
	return &out
}
func finishRewrite(req *pipeline.Request, m *dns.Msg, text string) {
	m.AuthenticatedData = false
	m.CheckingDisabled = req.Msg.CheckingDisabled
	if opt := req.Msg.IsEdns0(); opt != nil {
		m.SetEdns0(ednsUDPSize, opt.Do())
		m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeForgedAnswer, ExtraText: text})
	}
}

// A target question retains request lifetime/configuration snapshots but drops
// name-specific authority selections before ordinary authority/policy dispatch.
func (s *Stage) targetRequest(req *pipeline.Request, target string) (*pipeline.Request, bool) {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	trace, _ := ctx.Value(rewriteContextKey{}).(rewriteTrace)
	if len(trace.names) >= maxRewriteHops || strings.EqualFold(req.Question().Name, target) {
		return nil, true
	}
	for _, name := range trace.names {
		if strings.EqualFold(name, target) {
			return nil, true
		}
	}
	trace.names = append(append([]string(nil), trace.names...), req.Question().Name)
	sub := *req
	sub.Msg = req.Msg.Copy()
	sub.Msg.Question[0].Name = target
	sub.Ctx = context.WithValue(ctx, rewriteContextKey{}, trace)
	if s.opts.ResolveTarget != nil {
		sub.Zone, sub.DNSSECValidation, sub.UpstreamGroup = nil, nil, ""
	}
	return &sub, false
}

func rewriteCopies(rrs []dns.RR) []dns.RR {
	var out []dns.RR
	for _, rr := range rrs {
		switch rr.Header().Rrtype {
		case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3:
			continue
		}
		out = append(out, dns.Copy(rr))
	}
	return out
}

func dnsExtraWithoutOPT(rrs []dns.RR) []dns.RR {
	out := rrs[:0]
	for _, rr := range rrs {
		if rr.Header().Rrtype != dns.TypeOPT && rr.Header().Rrtype != dns.TypeTSIG {
			out = append(out, rr)
		}
	}
	return out
}
