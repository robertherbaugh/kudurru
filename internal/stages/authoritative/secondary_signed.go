package authoritative

import (
	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// minimalSignedANY keeps one complete ordinary RRset together with only its
// covering signatures. DNSSEC metadata cannot displace the ordinary RRset.
func minimalSignedANY(answer []dns.RR) []dns.RR {
	var owner string
	var typ uint16
	for _, rr := range answer {
		if rr.Header().Rrtype != dns.TypeRRSIG {
			owner = dns.CanonicalName(rr.Header().Name)
			typ = rr.Header().Rrtype
			break
		}
	}
	if owner == "" {
		return minimalANY(answer)
	}
	var out []dns.RR
	for _, rr := range answer {
		if dns.CanonicalName(rr.Header().Name) != owner {
			continue
		}
		if sig, ok := rr.(*dns.RRSIG); ok {
			if sig.TypeCovered == typ {
				out = append(out, rr)
			}
		} else if rr.Header().Rrtype == typ {
			out = append(out, rr)
		}
	}
	return out
}

func signedDO(req *pipeline.Request) bool { opt := req.Msg.IsEdns0(); return opt != nil && opt.Do() }

func (s *Stage) signedFailure(req *pipeline.Request, z *zone.Zone) *pipeline.Response {
	r := pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusServfailUnsynced)
	r.Upstream = "secondary:proof-unavailable:" + z.Name()
	r.Origin = pipeline.OriginFailure
	r.Msg.RecursionAvailable = s.RecursionAvailable
	r.Msg.AuthenticatedData = false
	r.Msg.CheckingDisabled = req.Msg.CheckingDisabled
	s.finish(req, r.Msg)
	return r
}

// appendSignedAuthority retains wildcard evidence from every local alias hop,
// including a final negative answer or referral from another local zone.
func appendSignedAuthority(prefix, suffix []dns.RR) []dns.RR {
	out := append([]dns.RR(nil), prefix...)
	for _, rr := range suffix {
		duplicate := false
		for _, existing := range out {
			if dns.IsDuplicate(existing, rr) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, rr)
		}
	}
	return out
}

func (s *Stage) signedLookup(req *pipeline.Request, set zone.Set, z *zone.Zone, name string, qtype uint16, hops int) (zone.Result, error) {
	return z.LookupSignedWithin(name, qtype, hops, func(target string) bool { return set.Match(target) == z }, signedDO(req), s.now())
}

func (s *Stage) signedSplice(req *pipeline.Request, z *zone.Zone, answer, authority []dns.RR, resp *pipeline.Response, err error) *pipeline.Response {
	r := s.splice(req, z, answer, resp, err)
	r.Msg.Ns = appendSignedAuthority(authority, r.Msg.Ns)
	if signedResponseOverBudget(r.Msg) {
		return s.signedFailure(req, z)
	}
	return r
}

// Bound combined signed replies across local zones and external alias splices.
// Ordinary unsigned responses retain the existing behavior.
func signedResponseOverBudget(m *dns.Msg) bool {
	signed := false
	type key struct {
		name string
		typ  uint16
	}
	sets := map[key]bool{}
	bytes := 0
	for _, section := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range section {
			signed = signed || rr.Header().Rrtype == dns.TypeRRSIG
			sets[key{dns.CanonicalName(rr.Header().Name), rr.Header().Rrtype}] = true
			bytes += dns.Len(rr)
		}
	}
	return signed && (len(sets) > 64 || bytes > 1<<20)
}
