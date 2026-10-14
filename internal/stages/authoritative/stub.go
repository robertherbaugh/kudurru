package authoritative

import (
	"context"
	"fmt"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// stubReply uses only the request's captured route. RD=0 never invokes the
// exchange callback; RD=1 asks that authority with RD=0 and returns child
// referrals without following them. Configuration/store discovery is absent.
func (s *Stage) stubReply(req *pipeline.Request, set zone.Set, z *zone.Zone, next pipeline.Next, hops int) (*pipeline.Response, error) {
	if !z.RoutingAvailableAt(s.now()) {
		return s.secondaryUnavailable(req, z), nil
	}
	if !req.Msg.RecursionDesired {
		out := new(dns.Msg)
		out.SetReply(req.Msg)
		out.RecursionAvailable = s.RecursionAvailable
		do := req.Msg.IsEdns0() != nil && req.Msg.IsEdns0().Do()
		out.Ns, out.Extra = z.StubReferral(s.now(), do)
		s.finish(req, out)
		return &pipeline.Response{Msg: out, Status: pipeline.StatusAuthoritative, Upstream: "stub:" + z.Name(), Origin: pipeline.OriginLocal, Security: pipeline.Security{State: pipeline.ValidationUnknown}}, nil
	}
	if s.StubQuery == nil {
		return s.stubFailure(req, z), nil
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Bound the entire stub/CNAME continuation, even for callers outside a
	// listener. A listener's earlier captured deadline takes precedence.
	var cancel context.CancelFunc
	if !req.Deadline.IsZero() && req.Deadline.Before(time.Now().Add(2*time.Second)) {
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
	} else {
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
	}
	defer cancel()
	captured := *req
	captured.Ctx = ctx
	req = &captured
	query := req.Msg.Copy()
	query.RecursionDesired = false
	reply, err := s.StubQuery(ctx, z, query)
	if err != nil || reply == nil || reply.Truncated {
		return s.stubFailure(req, z), nil
	}
	// Honor most-specific configured coverage when the raw answer's CNAME
	// leaves this stub; discard remote records for the overridden target.
	chain, target, count, err := stubBoundary(reply, req.Msg.Question[0], set, z, hops)
	if err != nil {
		return s.stubFailure(req, z), nil
	}
	if target != "" {
		res := zone.Result{Kind: zone.CNAME, Answer: chain, Target: target, Hops: count}
		resp, err := s.chase(req, set, z, res, next)
		if err != nil || resp == nil {
			return s.stubFailure(req, z), nil
		}
		resp.Msg.Authoritative, resp.Msg.AuthenticatedData = false, false
		if resp.Origin != pipeline.OriginFailure {
			resp.Origin, resp.Modified = pipeline.OriginMixed, true
		}
		resp.Cacheable = false
		resp.Security = pipeline.Security{State: pipeline.ValidationUnknown}
		return resp, nil
	}
	out := reply.Copy()
	out.Id = req.Msg.Id
	out.Question = append([]dns.Question(nil), req.Msg.Question...)
	out.RecursionDesired, out.CheckingDisabled = req.Msg.RecursionDesired, req.Msg.CheckingDisabled
	out.Authoritative, out.AuthenticatedData = false, false
	out.RecursionAvailable = s.RecursionAvailable
	extra := out.Extra[:0]
	for _, rr := range out.Extra {
		if rr.Header().Rrtype != dns.TypeOPT && rr.Header().Rrtype != dns.TypeTSIG {
			extra = append(extra, rr)
		}
	}
	out.Extra = extra
	s.finish(req, out)
	status := pipeline.StatusAuthoritative
	if out.Rcode == dns.RcodeServerFailure {
		status = pipeline.StatusServfailUpstream
	}
	return &pipeline.Response{Msg: out, Status: status, Upstream: "stub:" + z.Name(), Origin: pipeline.OriginForwarded, Security: pipeline.Security{State: pipeline.ValidationUnknown}, Cacheable: false}, nil
}

func (s *Stage) stubFailure(req *pipeline.Request, z *zone.Zone) *pipeline.Response {
	r := pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusServfailUpstream)
	r.Upstream = "stub:failed:" + z.Name()
	r.Origin = pipeline.OriginFailure
	r.Msg.RecursionAvailable = s.RecursionAvailable
	s.finish(req, r.Msg)
	return r
}

func stubBoundary(reply *dns.Msg, q dns.Question, set zone.Set, z *zone.Zone, hops int) ([]dns.RR, string, int, error) {
	if q.Qtype == dns.TypeCNAME || q.Qtype == dns.TypeANY {
		return nil, "", hops, nil
	}
	name := dns.CanonicalName(q.Name)
	seen := map[string]bool{}
	owners := map[string]bool{}
	for {
		if seen[name] {
			return nil, "", hops, fmt.Errorf("stub CNAME loop")
		}
		seen[name] = true
		var cname *dns.CNAME
		for _, rr := range reply.Answer {
			if r, ok := rr.(*dns.CNAME); ok && dns.CanonicalName(r.Hdr.Name) == name {
				if cname != nil && dns.CanonicalName(cname.Target) != dns.CanonicalName(r.Target) {
					return nil, "", hops, fmt.Errorf("conflicting stub CNAME")
				}
				cname = r
			}
		}
		if cname == nil {
			return nil, "", hops, nil
		}
		hops++
		if hops > zone.MaxChain {
			return nil, "", hops, fmt.Errorf("stub CNAME chain exceeds limit")
		}
		owners[name] = true
		name = dns.CanonicalName(cname.Target)
		if set.Match(name) != z {
			var chain []dns.RR
			for _, rr := range reply.Answer {
				owner := dns.CanonicalName(rr.Header().Name)
				if owners[owner] {
					switch r := rr.(type) {
					case *dns.CNAME:
						chain = append(chain, rr)
					case *dns.RRSIG:
						if r.TypeCovered == dns.TypeCNAME {
							chain = append(chain, rr)
						}
					}
				}
				// Preserve a DNAME underlying a synthesized CNAME on this path.
				if r, ok := rr.(*dns.DNAME); ok {
					for owner := range owners {
						if owner != dns.CanonicalName(r.Hdr.Name) && dns.IsSubDomain(r.Hdr.Name, owner) {
							chain = append(chain, rr)
							break
						}
					}
				}
			}
			return chain, name, hops, nil
		}
	}
}
