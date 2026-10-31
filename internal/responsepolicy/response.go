package responsepolicy

import (
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

const maxAddresses = 4096

// CloneModified creates a private synthetic response and removes proofs that
// no longer describe its RRsets. Security retains the source's diagnostic verdict.
func CloneModified(r *pipeline.Response, rule string) *pipeline.Response {
	out := *r
	out.Msg = r.Msg.Copy()
	out.Modified, out.Cacheable = true, false
	out.Msg.AuthenticatedData = false
	out.Msg.Authoritative = false
	out.Rule = rule
	for _, section := range []*[]dns.RR{&out.Msg.Answer, &out.Msg.Ns, &out.Msg.Extra} {
		rrs := (*section)[:0]
		for _, rr := range *section {
			switch rr.Header().Rrtype {
			case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeNSEC3PARAM, dns.TypeTSIG:
				continue
			}
			rrs = append(rrs, rr)
		}
		*section = rrs
	}
	return &out
}

func protected(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() || ip == netip.MustParseAddr("255.255.255.255")
}

// inspect covers every address, including SVCB/HTTPS hints. Exceeding its
// budget rejects the response rather than allowing unchecked tail records.
func inspect(sections [][]dns.RR, hit func(string, netip.Addr) bool) bool {
	work := 0
	check := func(owner string, ip net.IP) bool {
		work++
		a, ok := netip.AddrFromSlice(ip)
		return work > maxAddresses || !ok || hit(owner, a.Unmap())
	}
	for _, rrs := range sections {
		for _, rr := range rrs {
			work++
			if work > maxAddresses || rr == nil {
				return true
			}
			var values []dns.SVCBKeyValue
			switch r := rr.(type) {
			case *dns.A:
				if check(r.Hdr.Name, r.A) {
					return true
				}
			case *dns.AAAA:
				if check(r.Hdr.Name, r.AAAA) {
					return true
				}
			case *dns.SVCB:
				values = r.Value
			case *dns.HTTPS:
				values = r.Value
			}
			for _, v := range values {
				work++
				if work > maxAddresses {
					return true
				}
				var ips []net.IP
				switch hint := v.(type) {
				case *dns.SVCBIPv4Hint:
					ips = hint.Hint
				case *dns.SVCBIPv6Hint:
					ips = hint.Hint
				}
				for _, ip := range ips {
					if check(rr.Header().Name, ip) {
						return true
					}
				}
			}
		}
	}
	return false
}

// Guard applies rebinding protection after synthesis and response filtering.
// A private-domain exemption requires both the question and address owner to
// be in an exempt domain. A client-network bypass exempts the whole response.
func (s *Settings) Guard(req *pipeline.Request, r *pipeline.Response) *pipeline.Response {
	if s == nil || !s.rebinding || r == nil || r.Msg == nil || r.Status.Class() == pipeline.ClassBlocked || contains(s.bypassNetworks, req.Client.Addr.Addr()) {
		return r
	}
	switch r.Origin {
	case pipeline.OriginForwarded, pipeline.OriginSynthetic, pipeline.OriginMixed:
	default:
		return r
	}
	if r.Msg.Rcode != dns.RcodeSuccess || !inspect([][]dns.RR{r.Msg.Answer, r.Msg.Ns, r.Msg.Extra}, func(owner string, ip netip.Addr) bool {
		return protected(ip) && (!within(s.privateDomains, req.Question().Name) || !within(s.privateDomains, owner))
	}) {
		return r
	}
	out := CloneModified(r, "rebinding")
	out.Status, out.Origin = pipeline.StatusBlockedUpstream, pipeline.OriginSynthetic
	out.Msg.Rcode, out.Msg.Authoritative = dns.RcodeRefused, false
	out.Msg.Answer, out.Msg.Ns, out.Msg.Extra = nil, nil, nil
	if opt := req.Msg.IsEdns0(); opt != nil {
		out.Msg.SetEdns0(opt.UDPSize(), opt.Do())
		out.Msg.IsEdns0().Option = append(out.Msg.IsEdns0().Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeBlocked, ExtraText: "private address or response inspection limit"})
	}
	return out
}

// Transform applies address filtering and NXDOMAIN handling to a raw forwarded
// answer. DNS64 is separate because it may require one additional A query.
func (s *Settings) Transform(req *pipeline.Request, r *pipeline.Response) *pipeline.Response {
	if s == nil || r == nil || r.Msg == nil || r.Origin != pipeline.OriginForwarded || r.Modified || req.Question().Qclass != dns.ClassINET {
		return r
	}
	if r.Msg.Rcode == dns.RcodeSuccess && len(s.bogus) > 0 && inspect([][]dns.RR{r.Msg.Answer}, func(_ string, ip netip.Addr) bool { return contains(s.bogus, ip) }) {
		out := CloneModified(r, "bogus_nxdomain")
		out.Msg.Rcode = dns.RcodeNameError
		out.Msg.Answer, out.Msg.Ns, out.Msg.Extra = nil, nil, nil
		return out // Never immediately override the negative we just synthesized.
	}
	if r.Msg.Rcode == dns.RcodeNameError && len(s.nxdomain) > 0 {
		name, aliases, ok := terminal(req.Question().Name, r.Msg.Answer)
		if !ok {
			return r
		}
		var records []dns.RR
		for _, ip := range s.nxdomain {
			h := dns.RR_Header{Name: name, Class: dns.ClassINET, Ttl: s.ttl}
			if ip.Is4() && req.Question().Qtype == dns.TypeA {
				h.Rrtype = dns.TypeA
				records = append(records, &dns.A{Hdr: h, A: net.IP(ip.AsSlice())})
			} else if ip.Is6() && req.Question().Qtype == dns.TypeAAAA && !s.filterAAAA {
				h.Rrtype = dns.TypeAAAA
				records = append(records, &dns.AAAA{Hdr: h, AAAA: net.IP(ip.AsSlice())})
			}
		}
		if len(records) > 0 {
			out := CloneModified(r, "nxdomain_override")
			out.Msg.Rcode = dns.RcodeSuccess
			out.Msg.Answer, out.Msg.Ns, out.Msg.Extra = append(aliases, records...), nil, nil
			return out
		}
	}
	return s.filterIPv6(r)
}

func (s *Settings) filterIPv6(r *pipeline.Response) *pipeline.Response {
	if s != nil && s.filterAAAA && r.Msg.Rcode == dns.RcodeSuccess {
		out := CloneModified(r, "filter_aaaa")
		changed := false
		for _, section := range []*[]dns.RR{&out.Msg.Answer, &out.Msg.Ns, &out.Msg.Extra} {
			rrs := (*section)[:0]
			for _, rr := range *section {
				if rr.Header().Rrtype == dns.TypeAAAA {
					changed = true
					continue
				}
				var values *[]dns.SVCBKeyValue
				switch r := rr.(type) {
				case *dns.SVCB:
					values = &r.Value
				case *dns.HTTPS:
					values = &r.Value
				}
				if values != nil {
					kept := (*values)[:0]
					for _, v := range *values {
						if v.Key() == dns.SVCB_IPV6HINT {
							changed = true
							continue
						}
						if mandatory, ok := v.(*dns.SVCBMandatory); ok {
							codes := mandatory.Code[:0]
							for _, key := range mandatory.Code {
								if key != dns.SVCB_IPV6HINT {
									codes = append(codes, key)
								}
							}
							mandatory.Code = codes
							if len(codes) == 0 {
								continue
							}
						}
						kept = append(kept, v)
					}
					*values = kept
				}
				rrs = append(rrs, rr)
			}
			*section = rrs
		}
		if changed {
			return out
		}
	}
	return r
}

// terminal returns the terminal CNAME owner and private alias copies. Unexpected
// data, conflicting targets, cycles and excessive chains cannot be synthesized.
func terminal(name string, rrs []dns.RR) (string, []dns.RR, bool) {
	name = dns.CanonicalName(name)
	seen := map[string]bool{}
	var aliases []dns.RR
	for range 21 {
		if seen[name] {
			return "", nil, false
		}
		seen[name] = true
		var alias *dns.CNAME
		for _, rr := range rrs {
			if c, ok := rr.(*dns.CNAME); ok && strings.EqualFold(c.Hdr.Name, name) {
				if alias != nil && !strings.EqualFold(alias.Target, c.Target) {
					return "", nil, false
				}
				alias = c
			}
		}
		if alias == nil {
			return name, aliases, true
		}
		aliases = append(aliases, dns.Copy(alias))
		name = dns.CanonicalName(alias.Target)
	}
	return "", nil, false
}
