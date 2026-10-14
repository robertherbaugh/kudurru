package responsepolicy

import (
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// WantsDNS64 reports whether this AAAA NODATA answer permits an A lookup.
// DO clients receive the original denial so they can validate it themselves.
func (s *Settings) WantsDNS64(req *pipeline.Request, r *pipeline.Response) bool {
	if s == nil || !s.prefix.IsValid() || s.filterAAAA || r == nil || r.Msg == nil || r.Origin != pipeline.OriginForwarded || r.Modified || r.Msg.Rcode != dns.RcodeSuccess || r.Msg.Truncated || !req.Msg.RecursionDesired || req.Question().Qtype != dns.TypeAAAA || req.Question().Qclass != dns.ClassINET || within(s.excludeDomains, req.Question().Name) {
		return false
	}
	if opt := req.Msg.IsEdns0(); opt != nil && opt.Do() {
		return false
	}
	name, _, ok := terminal(req.Question().Name, r.Msg.Answer)
	if !ok || within(s.excludeDomains, name) {
		return false
	}
	for _, rr := range r.Msg.Answer {
		switch rr.Header().Rrtype {
		case dns.TypeCNAME, dns.TypeRRSIG, dns.TypeDNAME:
		default:
			return false
		}
	}
	return true
}

// DNS64 synthesizes only a successful A answer for the same canonical target
// as the AAAA denial. Input address protection happens before IPv4 embedding.
func (s *Settings) DNS64(req *pipeline.Request, denial, addresses *pipeline.Response) *pipeline.Response {
	if !s.WantsDNS64(req, denial) || addresses == nil || addresses.Msg == nil || addresses.Origin != pipeline.OriginForwarded || addresses.Modified || addresses.Msg.Rcode != dns.RcodeSuccess || addresses.Msg.Truncated {
		return denial
	}
	if guarded := s.Guard(req, addresses); guarded != addresses {
		return guarded
	}
	if transformed := s.Transform(req, addresses); transformed != addresses {
		return transformed
	}
	name, _, ok := terminal(req.Question().Name, denial.Msg.Answer)
	atarget, aliases, aok := terminal(req.Question().Name, addresses.Msg.Answer)
	if !ok || !aok || name != atarget || within(s.excludeDomains, name) {
		return denial
	}
	ttl := s.ttl
	for _, rr := range denial.Msg.Ns {
		if soa, ok := rr.(*dns.SOA); ok {
			ttl = min(ttl, soa.Hdr.Ttl, soa.Minttl)
		}
	}
	var records []dns.RR
	for _, rr := range addresses.Msg.Answer {
		a, ok := rr.(*dns.A)
		if !ok || !strings.EqualFold(a.Hdr.Name, name) {
			continue
		}
		ip, ok := netip.AddrFromSlice(a.A)
		if !ok || !ip.Unmap().Is4() || contains(s.excludeIPv4, ip) {
			continue
		}
		v6 := embed(s.prefix, ip.Unmap())
		h := a.Hdr
		h.Rrtype, h.Ttl = dns.TypeAAAA, min(a.Hdr.Ttl, ttl)
		records = append(records, &dns.AAAA{Hdr: h, AAAA: net.IP(v6.AsSlice())})
	}
	if len(records) == 0 {
		return denial
	}
	out := CloneModified(denial, "dns64")
	out.Msg.Answer, out.Msg.Ns, out.Msg.Extra = append(aliases, records...), nil, nil
	for _, rr := range out.Msg.Answer {
		rr.Header().Ttl = min(rr.Header().Ttl, ttl)
	}
	if out.Security.State != addresses.Security.State {
		out.Security.State = pipeline.ValidationUnknown
		out.Security.Reason = "mixed_synthesis_dependencies"
	}
	if until := addresses.Security.ValidUntil; !until.IsZero() && (out.Security.ValidUntil.IsZero() || until.Before(out.Security.ValidUntil)) {
		out.Security.ValidUntil = until
	}
	return out
}

// embed implements RFC 6052 section 2.2, including the reserved u octet.
func embed(prefix netip.Prefix, ip netip.Addr) netip.Addr {
	b := prefix.Addr().As16()
	v4 := ip.As4()
	at := prefix.Bits() / 8
	for _, octet := range v4 {
		if at == 8 {
			at++
		}
		b[at] = octet
		at++
	}
	return netip.AddrFrom16(b)
}
