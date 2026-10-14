// Package rewrite transforms private forwarded answers after raw cache lookup
// and before the blocking stage inspects the resulting response.
package rewrite

import (
	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
)

// Stage applies the captured response-processing plan.
type Stage struct{}

// Name implements pipeline.Stage.
func (*Stage) Name() string { return "rewrite" }

// Serve implements pipeline.Stage. Additional A queries inherit the original
// resolver/trust snapshot and pass through cache and validation once each.
func (*Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	if req.Msg == nil || req.Msg.Opcode != dns.OpcodeQuery || len(req.Msg.Question) != 1 || req.Question().Qtype == dns.TypeAXFR || req.Question().Qtype == dns.TypeIXFR {
		return next(req)
	}
	s, _ := req.ResponseSettings.(*responsepolicy.Settings)
	if d := policy.FromRequest(req); d != nil {
		if d.ResponseInvalid {
			r := pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusServfailUpstream)
			r.Origin, r.Rule = pipeline.OriginFailure, "invalid_response_policy"
			return r, nil
		}
		s = s.With(d.Response)
		if r := s.RewriteAnswer(req, d.Rewrites); r != nil {
			return r, nil
		}
	}
	r, err := next(req)
	if err != nil {
		return r, err
	}
	r = s.Transform(req, r)
	if !s.WantsDNS64(req, r) {
		return r, nil
	}
	sub := *req
	sub.Msg = req.Msg.Copy()
	sub.Msg.Question[0].Qtype = dns.TypeA
	a, err := next(&sub)
	if err != nil {
		return nil, err
	}
	if a != nil && a.Msg != nil && a.Msg.Rcode == dns.RcodeServerFailure {
		out := *a
		out.Msg = a.Msg.Copy()
		out.Msg.Question = append([]dns.Question(nil), req.Msg.Question...)
		return &out, nil
	}
	return s.DNS64(req, r, a), nil
}
