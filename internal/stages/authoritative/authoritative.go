// Package authoritative is the pipeline stage that answers from kudurru's
// own zones (dev-docs/design-record.md §7.1 step 6): longest-suffix zone match
// over a lock-free snapshot, queryAccess/queryACL enforcement, primary
// zone answers with wildcards, CNAME and DNAME chasing, forward zones by
// upstream-group hint, zone transfers out (AXFR, IXFR by AXFR fallback;
// transfer.go), and SERVFAIL instead of answers the zone cache cannot
// vouch for (the D80 gate: everything while unsynced, names under primary
// zones while serving a stale local.db).
//
// Authoritative answers are final: they are not passed to the blocking,
// cache or upstream stages (a local record cannot be blocked by a list).
// With RD set, names absent from an overlay zone and alias targets outside
// local data continue through the rest of the chain. Mixed alias answers
// do not assert authority or whole-chain DNSSEC security.
package authoritative

import (
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

// UpstreamUnsynced and UpstreamStale are the Response.Upstream values of
// the SERVFAIL answers (Status servfail_unsynced) given while the zone
// cache has not synced, and while it serves a local.db snapshot older than
// maxAge, so the query log's "why" row shows the reason.
const (
	UpstreamUnsynced = "zonecache:unsynced"
	UpstreamStale    = "zonecache:stale"
)

// Source supplies the current zone set. The zone cache implements it
// (zone.Holder is a ready-made implementation over an
// atomic.Pointer[zone.Snapshot]). Zones is called once per query and must
// not block; nil means nothing has been loaded yet, which the stage treats
// as unsynced.
type Source interface {
	Zones() zone.Set
}

// LocalRecords resolves additional local namespaces outside specific configured
// zones. A catch-all forward root also permits it. Implementations read memory
// only and never block.
type LocalRecords interface {
	ResolveLocal(*pipeline.Request) (*pipeline.Response, bool)
}

// TransferProvider is the optional capability (discovered by type
// assertion, §3.3) that hands out a zone's records for a transfer: chunks
// of at most 64 KiB, SOA first and last, or the single SOA when serial is
// current. The stage implements it and uses the same chunking for its own
// AXFR/IXFR answers.
type TransferProvider interface {
	Transfer(zone string, serial uint32) (<-chan []dns.RR, error)
}

// Stage implements pipeline.Stage.
type Stage struct {
	// Source is required.
	Source Source
	// LocalRecords optionally supplies DHCP lease records outside every
	// specific configured zone. A catch-all forward root permits local records;
	// primary root and specific primary/forward zones retain precedence.
	LocalRecords LocalRecords
	// RecursionAvailable sets RA on authoritative replies. Set it when the
	// chain also resolves recursively (an upstream stage follows), so
	// clients that check RA keep using the server.
	RecursionAvailable bool
	// Notify and Update handle the NOTIFY and UPDATE opcodes. Nil answers
	// NOTIMP, the v0.1 behaviour; secondary zones and RFC 2136 (v0.2)
	// install handlers here.
	Notify, Update pipeline.Next
	// Metrics counts zone transfers; nil disables counting.
	Metrics *Metrics
	// ChunkBytes bounds the records per transfer message (uncompressed
	// wire size); zero means zone.DefaultChunkBytes (64 KiB minus room for
	// the header, OPT and TSIG).
	ChunkBytes int
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
		return s.unsynced(req, zone.GateUnsynced), nil
	}
	gate := set.Gate()
	z := set.Match(q.Name)
	if z == nil {
		if gate == zone.GateUnsynced {
			// A zone that has not loaded yet may cover the name.
			return s.unsynced(req, gate), nil
		}
		if s.LocalRecords != nil {
			if response, handled := s.LocalRecords.ResolveLocal(req); handled {
				s.finish(req, response.Msg)
				return response, nil
			}
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
		return s.transfer(req, gate, z), nil
	}
	if z.Type() == v1alpha1.ZoneTypeForward {
		if gate == zone.GateUnsynced {
			return s.unsynced(req, gate), nil
		}
		if z.Name() == "." && s.LocalRecords != nil {
			if response, handled := s.LocalRecords.ResolveLocal(req); handled {
				s.finish(req, response.Msg)
				return response, nil
			}
		}
		req.UpstreamGroup = z.UpstreamGroup()
		return next(req)
	}
	if gate == zone.GateStale {
		// Primary data older than maxAge is not served at all (§4.4).
		return s.unsynced(req, gate), nil
	}

	var res zone.Result
	if z.HasFallback() && gate == zone.GateOpen {
		res = z.LookupStep(q.Name, q.Qtype, 0)
	} else {
		res = z.Lookup(q.Name, q.Qtype)
	}
	if gate == zone.GateUnsynced && !res.Positive() {
		return s.unsynced(req, gate), nil
	}
	if res.Kind == zone.CNAME {
		return s.chase(req, set, z, res, next)
	}
	if res.Kind == zone.Fallback {
		return s.fallback(req, gate, z, res, next)
	}
	answer := res.Answer
	if q.Qtype == dns.TypeANY && req.Client.Proto == pipeline.ProtoUDP {
		answer = minimalANY(answer)
	}
	return s.reply(req, z, &res, answer), nil
}

// fallback continues after the authoritative stage so native recursion never
// reenters the local zone. A local name with a missing type is NODATA and never
// reaches this path. Snapshot gates and query ACLs still apply to every miss.
func (s *Stage) fallback(req *pipeline.Request, gate zone.Gate, z *zone.Zone, res zone.Result, next pipeline.Next) (*pipeline.Response, error) {
	if gate != zone.GateOpen {
		return s.unsynced(req, gate), nil
	}
	if !req.Msg.RecursionDesired || !s.RecursionAvailable {
		return s.refused(req, z), nil
	}
	if len(res.Answer) == 0 {
		req.UpstreamGroup = z.FallbackGroup()
		resp, err := next(req)
		if err != nil || resp == nil || resp.Msg == nil {
			return resp, err
		}
		out := *resp
		out.Msg = resp.Msg.Copy()
		out.Msg.Authoritative = false
		out.Msg.RecursionAvailable = s.RecursionAvailable
		return &out, nil
	}
	sub := subRequest(req, res.Target, req.Question().Qtype, req.Question().Qclass)
	sub.UpstreamGroup = z.FallbackGroup()
	resp, err := next(sub)
	return s.splice(req, z, res.Answer, resp, err), nil
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
			var r2 zone.Result
			if z2.HasFallback() && set.Gate() == zone.GateOpen {
				r2 = z2.LookupStep(target, q.Qtype, last.Hops)
			} else {
				r2 = z2.LookupFrom(target, q.Qtype, last.Hops)
			}
			switch r2.Kind {
			case zone.CNAME:
				answer = append(answer, r2.Answer...)
				last = &r2
				continue
			case zone.Delegation:
				// The target is served elsewhere: treat it as leaving.
			case zone.Fallback:
				r2.Answer = append(answer, r2.Answer...)
				return s.fallback(req, set.Gate(), z2, r2, next)
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
	out := in.Copy()
	out.Id = req.Msg.Id
	out.Question = append([]dns.Question(nil), req.Msg.Question...)
	out.RecursionDesired = req.Msg.RecursionDesired
	out.CheckingDisabled = req.Msg.CheckingDisabled
	out.Authoritative = false
	// A validated public target does not authenticate the local aliases.
	out.AuthenticatedData = false
	out.RecursionAvailable = s.RecursionAvailable || in.RecursionAvailable
	out.Answer = append(chain, in.Answer...)
	s.finish(req, out)
	security := pipeline.DNSSECUnknown
	if resp.DNSSEC == pipeline.DNSSECBogus {
		security = pipeline.DNSSECBogus
	}
	return &pipeline.Response{
		Msg:      out,
		Status:   resp.Status,
		Upstream: resp.Upstream,
		List:     resp.List,
		Rule:     resp.Rule,
		DNSSEC:   security,
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

// unsynced is the answer while the zone cache cannot vouch for the data
// (D80): SERVFAIL, never NXDOMAIN and never an upstream answer for a name
// a local zone may own.
func (s *Stage) unsynced(req *pipeline.Request, gate zone.Gate) *pipeline.Response {
	r := pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusServfailUnsynced)
	r.Upstream = UpstreamUnsynced
	if gate == zone.GateStale {
		r.Upstream = UpstreamStale
	}
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
