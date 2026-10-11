// Package authoritative is the pipeline stage that answers from kudurru's
// own zones (docs/architecture.md §7.1 step 6): longest-suffix zone match
// over a lock-free snapshot, queryAccess/queryACL enforcement, primary
// zone answers with wildcards, CNAME and DNAME chasing, forward zones by
// upstream-group hint, and SERVFAIL instead of denials while the zone
// cache has not synced.
//
// Authoritative answers are final: they are not passed to the blocking,
// cache or upstream stages (a zone cannot be blocked by a list). The one
// exception is a CNAME chain that leaves every local zone: with RD set the
// target is resolved by the rest of the chain and spliced after the local
// part of the answer.
package authoritative

import (
	"errors"
	"net/netip"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// Name is the fixed stage id.
const Name = "authoritative"

// UDPSize is the EDNS(0) buffer size advertised on authoritative replies
// (the DNS Flag Day 2020 value).
const UDPSize = 1232

// UpstreamUnsynced is the Response.Upstream value of SERVFAIL answers given
// while the zone cache has not synced, so the query log's "why" row shows
// the reason.
const UpstreamUnsynced = "zonecache:unsynced"

// ErrTransferNotImplemented is returned by Stage.Transfer until AXFR out
// lands in M2.
var ErrTransferNotImplemented = errors.New("authoritative: zone transfer is not implemented yet")

// Source supplies the current zone set. The zone cache implements it
// (zone.Holder is a ready-made implementation over an
// atomic.Pointer[zone.Snapshot]). Zones is called once per query and must
// not block; nil means nothing has been loaded yet, which the stage treats
// as unsynced.
type Source interface {
	Zones() zone.Set
}

// TransferProvider is the optional capability (discovered by type assertion,
// §3.3) through which the dnsserver streams AXFR/IXFR out. The stage
// defines the slot now; transfers are implemented in M2.
type TransferProvider interface {
	Transfer(zone string, serial uint32) (<-chan []dns.RR, error)
}

// Stage implements pipeline.Stage.
type Stage struct {
	// Source is required.
	Source Source
	// RecursionAvailable sets RA on authoritative replies. Set it when the
	// chain also resolves recursively (an upstream stage follows), so
	// clients that check RA keep using the server.
	RecursionAvailable bool
	// Notify and Update handle the NOTIFY and UPDATE opcodes. Nil answers
	// NOTIMP, the v0.1 behaviour; secondary zones and RFC 2136 (v0.2)
	// install handlers here.
	Notify, Update pipeline.Next
}

var (
	_ pipeline.Stage   = (*Stage)(nil)
	_ TransferProvider = (*Stage)(nil)
)

// New returns a stage reading zones from src.
func New(src Source) *Stage { return &Stage{Source: src, RecursionAvailable: true} }

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return Name }

// Ready reports whether the zone cache has synced (the Ready() capability).
func (s *Stage) Ready() bool {
	set := s.zones()
	return set != nil && set.Synced()
}

// Transfer is the AXFR/IXFR capability slot; it is not implemented in v0.1
// and AXFR/IXFR queries are answered NOTIMP.
func (s *Stage) Transfer(string, uint32) (<-chan []dns.RR, error) {
	return nil, ErrTransferNotImplemented
}

func (s *Stage) zones() zone.Set {
	if s.Source == nil {
		return nil
	}
	return s.Source.Zones()
}

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	m := req.Msg
	switch m.Opcode {
	case dns.OpcodeQuery:
	case dns.OpcodeNotify:
		return s.opcode(req, s.Notify)
	case dns.OpcodeUpdate:
		return s.opcode(req, s.Update)
	default:
		return next(req)
	}
	if len(m.Question) != 1 {
		return next(req)
	}
	q := m.Question[0]
	if q.Qclass != dns.ClassINET {
		return next(req)
	}

	set := s.zones()
	if set == nil {
		return s.unsynced(req), nil
	}
	synced := set.Synced()
	z := set.Match(q.Name)
	if z == nil {
		if !synced {
			return s.unsynced(req), nil
		}
		return next(req)
	}
	req.Zone = z
	client := clientAddr(req)
	if !z.QueryAllowed(client) {
		r := pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL)
		r.Upstream = upstreamName(z)
		s.finish(req, r.Msg)
		return r, nil
	}
	if q.Qtype == dns.TypeAXFR || q.Qtype == dns.TypeIXFR {
		// AXFR/IXFR out lands in M2 behind Transfer and transferACL.
		r := pipeline.Reply(req, dns.RcodeNotImplemented, pipeline.StatusAuthoritative)
		r.Upstream = upstreamName(z)
		s.finish(req, r.Msg)
		return r, nil
	}
	if z.Type() == v1alpha1.ZoneTypeForward {
		if !synced {
			return s.unsynced(req), nil
		}
		req.UpstreamGroup = z.UpstreamGroup()
		return next(req)
	}

	res := z.Lookup(q.Name, q.Qtype)
	if !synced && !res.Positive() {
		return s.unsynced(req), nil
	}
	if res.Kind == zone.CNAME {
		return s.chase(req, set, z, res, next)
	}
	answer := res.Answer
	if q.Qtype == dns.TypeANY && req.Client.Proto == pipeline.ProtoUDP {
		answer = minimalANY(answer)
	}
	return s.reply(req, z, &res, answer), nil
}

// minimalANY applies RFC 8482 §4.2 to ANY over UDP: answer with one RRset
// of the name instead of all of them, so ANY cannot be used to amplify.
// Clients that need everything ask over TCP.
func minimalANY(answer []dns.RR) []dns.RR {
	if len(answer) == 0 {
		return answer
	}
	t := answer[0].Header().Rrtype
	n := 1
	for n < len(answer) && answer[n].Header().Rrtype == t {
		n++
	}
	return answer[:n:n]
}

// chase continues a chain that left zone z: into other local zones without
// recursion, then (with RD) into the rest of the pipeline.
func (s *Stage) chase(req *pipeline.Request, set zone.Set, z *zone.Zone, res zone.Result, next pipeline.Next) (*pipeline.Response, error) {
	q := req.Msg.Question[0]
	client := clientAddr(req)
	answer := append([]dns.RR(nil), res.Answer...)
	last := &res
	for {
		target := last.Target
		z2 := set.Match(target)
		if z2 != nil && z2.Type() == v1alpha1.ZoneTypePrimary && z2.QueryAllowed(client) {
			r2 := z2.LookupFrom(target, q.Qtype, last.Hops)
			switch r2.Kind {
			case zone.CNAME:
				answer = append(answer, r2.Answer...)
				last = &r2
				continue
			case zone.Delegation:
				// The target is served elsewhere: treat it as leaving.
			default:
				answer = append(answer, r2.Answer...)
				return s.reply(req, z2, &r2, answer), nil
			}
		}
		if !req.Msg.RecursionDesired || (z2 != nil && !z2.QueryAllowed(client)) {
			// No recursion requested (or the target zone refuses this
			// client): the partial chain is the answer.
			return s.reply(req, z, last, answer), nil
		}
		sub := subRequest(req, target, q.Qtype, q.Qclass)
		if z2 != nil && z2.Type() == v1alpha1.ZoneTypeForward {
			sub.UpstreamGroup = z2.UpstreamGroup()
		}
		resp, err := next(sub)
		return s.splice(req, z, answer, resp, err), nil
	}
}

// subRequest builds the query for a chain target that leaves local data.
func subRequest(req *pipeline.Request, target string, qtype, qclass uint16) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion(target, qtype)
	m.Question[0].Qclass = qclass
	m.RecursionDesired = true
	m.CheckingDisabled = req.Msg.CheckingDisabled
	if opt := req.Msg.IsEdns0(); opt != nil {
		m.SetEdns0(opt.UDPSize(), opt.Do())
	}
	sub := *req
	sub.Msg = m
	sub.Zone = nil
	sub.UpstreamGroup = ""
	return &sub
}

// splice appends the rest of the pipeline's answer for a chain target to
// the local part of the chain. The result is not authoritative (it holds
// data kudurru does not own) and takes the status of the inner answer.
func (s *Stage) splice(req *pipeline.Request, z *zone.Zone, chain []dns.RR, resp *pipeline.Response, err error) *pipeline.Response {
	if err != nil || resp == nil || resp.Msg == nil {
		r := pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusServfailUpstream)
		r.Upstream = upstreamName(z)
		r.Msg.Answer = chain
		s.finish(req, r.Msg)
		return r
	}
	in := resp.Msg
	out := new(dns.Msg)
	out.SetRcode(req.Msg, in.Rcode)
	out.Authoritative = false
	out.RecursionAvailable = s.RecursionAvailable || in.RecursionAvailable
	out.Answer = append(chain, in.Answer...)
	if len(in.Answer) == 0 {
		out.Ns = in.Ns
	}
	s.finish(req, out)
	return &pipeline.Response{
		Msg:      out,
		Status:   resp.Status,
		Upstream: resp.Upstream,
		List:     resp.List,
		Rule:     resp.Rule,
	}
}

// reply renders a lookup result.
func (s *Stage) reply(req *pipeline.Request, z *zone.Zone, res *zone.Result, answer []dns.RR) *pipeline.Response {
	m := new(dns.Msg)
	m.SetRcode(req.Msg, res.Rcode())
	m.Authoritative = res.Authoritative()
	m.RecursionAvailable = s.RecursionAvailable
	if res.Kind != zone.Loop {
		m.Answer = answer
		m.Ns = res.Ns
		m.Extra = res.Extra
	}
	s.finish(req, m)
	return &pipeline.Response{Msg: m, Status: pipeline.StatusAuthoritative, Upstream: upstreamName(z)}
}

// unsynced is the answer while the zone cache cannot vouch for denials.
func (s *Stage) unsynced(req *pipeline.Request) *pipeline.Response {
	r := pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusServfailUpstream)
	r.Upstream = UpstreamUnsynced
	r.Msg.RecursionAvailable = s.RecursionAvailable
	s.finish(req, r.Msg)
	return r
}

// opcode routes NOTIFY and UPDATE.
func (s *Stage) opcode(req *pipeline.Request, h pipeline.Next) (*pipeline.Response, error) {
	if h != nil {
		return h(req)
	}
	r := pipeline.Reply(req, dns.RcodeNotImplemented, pipeline.StatusAuthoritative)
	s.finish(req, r.Msg)
	return r, nil
}

// finish adds an OPT record when the query had one (RFC 6891 §7), copying
// the DO bit (RFC 3225 §3).
func (s *Stage) finish(req *pipeline.Request, m *dns.Msg) {
	if opt := req.Msg.IsEdns0(); opt != nil && m.IsEdns0() == nil {
		m.SetEdns0(UDPSize, opt.Do())
	}
}

func clientAddr(req *pipeline.Request) netip.Addr {
	return req.Client.Addr.Addr().Unmap()
}

// upstreamName is the dictionary name recorded for authoritative answers
// (dict_upstream "zone:<name>", §11.2).
func upstreamName(z *zone.Zone) string { return "zone:" + z.Name() }
