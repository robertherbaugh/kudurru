// Package dnssec validates raw upstream responses before cache insertion.
package dnssec

import (
	"context"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

type Source interface {
	Bind(*pipeline.Request) (*upstream.QueryPlan, error)
}
type Stage struct{ source Source }

func New(source Source) *Stage { return &Stage{source: source} }
func (s *Stage) Name() string  { return "dnssec" }
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	if req.Msg == nil || req.Msg.Opcode != dns.OpcodeQuery || len(req.Msg.Question) != 1 || req.Msg.Question[0].Qclass != dns.ClassINET {
		return next(req)
	}
	plan, err := s.source.Bind(req)
	if err != nil {
		return next(req)
	}
	resp, err := next(req)
	if err != nil || resp == nil || resp.Msg == nil || resp.Origin != pipeline.OriginForwarded || resp.Msg.Truncated || (resp.Msg.Rcode != dns.RcodeSuccess && resp.Msg.Rcode != dns.RcodeNameError) {
		return resp, err
	}
	out := *resp
	out.Msg = resp.Msg.Copy()
	out.Msg.AuthenticatedData = false
	if plan.Validates() && !req.Msg.CheckingDisabled {
		v := plan.Validator()
		if v == nil {
			out.Security = pipeline.Security{State: pipeline.ValidationIndeterminate, Reason: "validator_unavailable"}
		} else {
			ctx := req.Ctx
			if ctx == nil {
				ctx = context.Background()
			}
			if !req.Deadline.IsZero() {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, req.Deadline)
				defer cancel()
			}
			r, _ := v.Validate(ctx, req.Question(), out.Msg, plan.Lookup)
			if r.Msg == nil {
				r.Msg = pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusDNSSECBogus).Msg
			}
			out.Msg, out.Security = r.Msg, r.Security
			if r.HasEDE {
				upstream.FinishReply(req.Msg, out.Msg, -1)
				upstream.AddEDE(out.Msg, r.EDE, r.Security.Reason)
			}
		}
		if out.Security.State == pipeline.ValidationBogus || out.Security.State == pipeline.ValidationIndeterminate {
			out.Msg.Rcode = dns.RcodeServerFailure
			out.Msg.Answer, out.Msg.Ns = nil, nil
			out.Status = pipeline.StatusDNSSECBogus
			if out.Security.State == pipeline.ValidationIndeterminate {
				out.Status = pipeline.StatusServfailUpstream
			}
			out.Origin = pipeline.OriginFailure
			out.Cacheable = false
			if out.Security.Reason == "validator_unavailable" {
				upstream.FinishReply(req.Msg, out.Msg, -1)
				upstream.AddEDE(out.Msg, dns.ExtendedErrorCodeNotReady, out.Security.Reason)
			}
		}
	} else if req.Msg.CheckingDisabled {
		out.Security.Reason = "cd_bypass"
	} else if !plan.Recursive() {
		out.Security.Reason = "validation_disabled"
	}
	// Recursive groups retain their native resolver's bounded provenance;
	// upstreams.recursion is independent of forwarded-answer validation.
	if !plan.RawResponse() {
		now := time.Now()
		if plan.Validator() != nil {
			now = plan.Validator().Now()
		}
		upstreamstage.Finalize(req.Msg, out.Msg, out.Security, out.Modified, now)
	}
	return &out, nil
}
