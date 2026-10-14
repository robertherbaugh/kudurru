// Package postprocess wraps the answering stages so client response flags
// are finalized after validation, cache lookup and any policy synthesis.
package postprocess

import (
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
)

// Stage applies the final representation to a private response copy. It
// cannot establish authenticity: only the validator supplies that evidence.
type Stage struct {
	Now func() time.Time
}

// Name implements pipeline.Stage.
func (*Stage) Name() string { return "postprocess" }

// Serve implements pipeline.Stage. Placement before all answering stages
// makes this run on response unwinding, including authoritative and cache hits.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	resp, err := next(req)
	if err != nil || resp == nil || resp.Msg == nil || req.Msg == nil || req.Msg.Opcode != dns.OpcodeQuery || len(req.Msg.Question) != 1 {
		return resp, err
	}
	qt := req.Msg.Question[0].Qtype
	if qt == dns.TypeAXFR || qt == dns.TypeIXFR {
		return resp, err
	}
	out := *resp
	out.Msg = resp.Msg.Copy()
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	// Unknown, local and mixed answers cannot inherit an upstream AD bit.
	security := out.Security
	if out.Origin != pipeline.OriginForwarded {
		security = pipeline.Security{}
	}
	upstreamstage.Finalize(req.Msg, out.Msg, security, out.Modified, now)
	return &out, nil
}
