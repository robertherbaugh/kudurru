package blocking

import (
	"context"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/rpz"
)

type rpzContextKey struct{ stage *Stage }
type rpzCapture struct{ selection *rpz.Selection }

func (s *Stage) pinRPZ(req *pipeline.Request) (*rpz.Selection, error) {
	key := rpzContextKey{s}
	if req.Ctx != nil {
		if pinned, ok := req.Ctx.Value(key).(rpzCapture); ok {
			return pinned.selection, nil
		}
	}
	selected, err := s.opts.RPZ(req)
	if err != nil {
		return nil, err
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	req.Ctx = context.WithValue(ctx, key, rpzCapture{selected})
	return selected, nil
}

func rpzQuestion(ctx engine.Context) rpz.Context {
	return rpz.Context{Name: ctx.Name, Type: ctx.Type, Class: ctx.Class, ClientIP: ctx.ClientIP}
}

func rpzInspection(ctx engine.Context, m *dns.Msg) (rpz.Context, bool) {
	inspection := rpzQuestion(ctx)
	for _, rr := range m.Answer {
		switch r := rr.(type) {
		case *dns.CNAME:
			inspection.Names = append(inspection.Names, r.Target)
		case *dns.A:
			if a, ok := netip.AddrFromSlice(r.A.To4()); ok {
				inspection.Addresses = append(inspection.Addresses, a)
			}
		case *dns.AAAA:
			if a, ok := netip.AddrFromSlice(r.AAAA.To16()); ok {
				inspection.Addresses = append(inspection.Addresses, a)
			}
		}
		if len(inspection.Names)+len(inspection.Addresses) > maxInspect {
			return inspection, false
		}
	}
	return inspection, true
}

func rpzResolverFailure(r *pipeline.Response) bool {
	return r == nil || r.Msg == nil || r.Origin == pipeline.OriginFailure || r.Security.State == pipeline.ValidationBogus ||
		r.Msg.Rcode != dns.RcodeSuccess && r.Msg.Rcode != dns.RcodeNameError
}

func (s *Stage) serveRPZ(req *pipeline.Request, d *policy.Decision, ctx engine.Context, selected *rpz.Selection, next pipeline.Next) (*pipeline.Response, error) {
	authorization := d.Selection.EvaluateAuthorizationContext(ctx)
	if authorization.Action == engine.ActionAllow {
		s.metrics.decision(allowList, groupLabel(req))
		return next(req)
	}
	if authorization.Action == engine.ActionBlock {
		return s.block(req, d, authorization), nil
	}
	if preflight := selected.Evaluate(rpzQuestion(ctx), s.opts.Now()); preflight.Unavailable {
		return rpzFailure(req, preflight, "unavailable"), nil
	}
	raw, err := next(req)
	if err != nil || rpzResolverFailure(raw) {
		s.countQuestion(req, engine.Decision{})
		return raw, err
	}
	inspection, ok := rpzInspection(ctx, raw.Msg)
	if !ok {
		raw.Close()
		return rpzFailure(req, rpz.Decision{}, "inspection limit"), nil
	}
	decision := selected.Evaluate(inspection, s.opts.Now())
	if decision.Unavailable {
		raw.Close()
		return rpzFailure(req, decision, "unavailable"), nil
	}
	if decision.Action == rpz.ActionNone || decision.Action == rpz.ActionPassthru ||
		decision.Action == rpz.ActionTCPOnly && req.Client.Proto != pipeline.ProtoUDP {
		return s.ordinaryResolved(req, d, ctx, raw, next)
	}
	out, err := s.renderRPZ(req, d, decision, selected, inspection, raw, next)
	if out != nil && out != raw {
		out.AddCleanup(raw.Close)
	} else if err != nil {
		raw.Close()
	}
	if out != nil && out.Status == pipeline.StatusBlockedRPZ {
		s.metrics.decision(blockRPZ, groupLabel(req))
		s.metrics.listHit(out.List)
	}
	return out, err
}

// ordinaryResolved preserves existing modifier precedence and uses the already
// resolved original response. Any synthesized target is a distinct subquery.
func (s *Stage) ordinaryResolved(req *pipeline.Request, d *policy.Decision, ctx engine.Context, raw *pipeline.Response, next pipeline.Next) (*pipeline.Response, error) {
	bd := d.Selection.EvaluateContext(ctx)
	switch bd.Action {
	case engine.ActionBlock:
		raw.Close()
		return s.block(req, d, bd), nil
	case engine.ActionRewrite:
		raw.Close()
		return s.rewrite(req, d, bd, ctx, next)
	case engine.ActionAllow:
		if bd.Reason == engine.ReasonAllowList {
			s.metrics.decision(allowList, groupLabel(req))
			return raw, nil
		}
	}
	if d.SafeSearchOn() {
		if rw, ok := s.view(req).catalogue.RewriteFor(ctx.Name, d.SafeSearchMode); ok {
			raw.Close()
			return s.safeSearch(req, d, rw, ctx, next)
		}
	}
	if raw.Msg.Rcode == dns.RcodeSuccess && len(raw.Msg.Answer) > 0 {
		if response := s.inspectContext(d, ctx, raw.Msg); response.Action == engine.ActionBlock {
			raw.Close()
			return s.block(req, d, response), nil
		}
	}
	s.countQuestion(req, bd)
	return raw, nil
}

func rpzFailure(req *pipeline.Request, decision rpz.Decision, reason string) *pipeline.Response {
	out := pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusServfailUpstream)
	out.Origin, out.List, out.Rule = pipeline.OriginFailure, decision.List, "RPZ "+decision.Origin+" "+reason
	out.Msg.AuthenticatedData = false
	if opt := req.Msg.IsEdns0(); opt != nil {
		out.Msg.SetEdns0(ednsUDPSize, opt.Do())
		out.Msg.IsEdns0().Option = append(out.Msg.IsEdns0().Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeNotReady, ExtraText: out.Rule})
	}
	return out
}

func (s *Stage) renderRPZ(req *pipeline.Request, d *policy.Decision, decision rpz.Decision, selected *rpz.Selection, inspection rpz.Context, raw *pipeline.Response, next pipeline.Next) (*pipeline.Response, error) {
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.RecursionAvailable = true
	m.Rcode = decision.Rcode()
	out := &pipeline.Response{Msg: m, Status: pipeline.StatusBlockedRPZ, List: decision.List, Rule: decision.Owner + " " + string(decision.Action), Origin: pipeline.OriginSynthetic, Modified: true}
	switch decision.Action {
	case rpz.ActionDrop:
		out.Msg = nil
		return out, pipeline.ErrDrop
	case rpz.ActionTCPOnly:
		m.Truncated = true
	case rpz.ActionLocalData:
		ttl := d.BlockTTL
		if ttl <= 0 {
			ttl = s.view(req).defaults.blockTTL
		}
		capTTL := ttlSeconds(ttl)
		name := req.Question().Name
		var prefix []dns.RR
		if decision.Trigger == rpz.TriggerQName && !strings.EqualFold(decision.Matched, name) {
			prefix = rpzAliasPrefix(raw.Msg.Answer, name, decision.Matched)
			if len(prefix) > 0 {
				name = decision.Matched
			}
		}
		records, err := decision.Records(name, req.Question().Qtype, capTTL)
		if err != nil {
			return rpzFailure(req, decision, "invalid local data"), nil
		}
		m.Answer = append(prefix, records...)
		var target string
		for _, rr := range records {
			if cname, ok := rr.(*dns.CNAME); ok {
				target = cname.Target
				break
			}
		}
		if target != "" && req.Question().Qtype != dns.TypeCNAME {
			sub, loop := s.targetRequest(req, target)
			if loop {
				return rpzFailure(req, decision, "rewrite loop"), nil
			}
			resolve := s.opts.ResolveTarget
			if resolve == nil {
				resolve = func(r *pipeline.Request) (*pipeline.Response, error) { return s.Serve(r, next) }
			}
			dependency, err := resolve(sub)
			if err != nil {
				// A dropped or failed target still owns its diagnostic metadata and
				// serving lease. Let the caller close the returned dependency.
				if dependency != nil && dependency.Msg != nil {
					return failedRewriteTarget(req, out, dependency), err
				}
				return dependency, err
			}
			out.AddCleanup(dependency.Close)
			if dependency == nil || dependency.Msg == nil {
				out.Close()
				return nil, pipeline.ErrNoAnswer
			}
			if rpzResolverFailure(dependency) {
				failed := failedRewriteTarget(req, out, dependency)
				if failed.Msg.Rcode == dns.RcodeSuccess {
					failed.Msg.Rcode = dns.RcodeServerFailure
				}
				failed.AddCleanup(out.Close)
				return failed, nil
			}
			m.Rcode = dependency.Msg.Rcode
			if dependency.Msg.Truncated {
				// A target's TCP-only action requires the original client to retry
				// over a stream too. Never turn it into an empty positive alias.
				m.Truncated, m.Answer = true, nil
				if dependency.Status == pipeline.StatusBlockedRPZ {
					out.List, out.Rule = dependency.List, dependency.Rule
				}
			} else {
				m.Answer = append(m.Answer, rewriteCopies(dependency.Msg.Answer)...)
				m.Ns = rewriteCopies(dependency.Msg.Ns)
				m.Extra = dnsExtraWithoutOPT(rewriteCopies(dependency.Msg.Extra))
			}
			out.Origin, out.Upstream = pipeline.OriginMixed, dependency.Upstream
		}
		// Target I/O can cross the feed's absolute expiry. Recheck the same pinned
		// selection and rematerialize policy TTLs at the final response time.
		final := selected.Evaluate(inspection, s.opts.Now())
		if final.Unavailable {
			out.Close()
			return rpzFailure(req, final, "unavailable"), nil
		}
		// Even a type-missing local-data action must cap its retained alias
		// prefix to the source lifetime. ANY supplies the policy RRset's TTLs.
		current, err := final.Records(name, dns.TypeANY, capTTL)
		if err != nil {
			out.Close()
			return rpzFailure(req, final, "invalid local data"), nil
		}
		for _, rr := range current {
			capTTL = min(capTTL, rr.Header().Ttl)
		}
		for _, section := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
			for _, rr := range section {
				rr.Header().Ttl = min(rr.Header().Ttl, capTTL)
			}
		}
	}
	finishRewrite(req, m, out.Rule)
	return out, nil
}

// A CNAME-tail policy retains only the authenticated/raw chain prefix linking
// the original question to the matched target. Unrelated aliases are omitted.
func rpzAliasPrefix(records []dns.RR, name, target string) []dns.RR {
	var prefix []dns.RR
	for range maxRewriteHops {
		found := false
		for _, rr := range records {
			cname, ok := rr.(*dns.CNAME)
			if !ok || !strings.EqualFold(cname.Hdr.Name, name) {
				continue
			}
			prefix = append(prefix, dns.Copy(cname))
			name, found = cname.Target, true
			break
		}
		if strings.EqualFold(name, target) {
			return prefix
		}
		if !found {
			return nil
		}
	}
	return nil
}
