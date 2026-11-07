package dynamicupdate

import "github.com/miekg/dns"

// Parse performs bounded framing/prescan validation and copies its input.
// Additional data is bounded but never installed as zone data or trusted for
// authentication. Only the listener's verified identity may authorize Plan.
func Parse(msg *dns.Msg, limits Limits) (*Transaction, *ProtocolError) {
	l := limits.Defaults()
	if msg == nil || msg.Response || msg.Opcode != dns.OpcodeUpdate || len(msg.Question) != 1 || msg.Question[0].Qtype != dns.TypeSOA || msg.Truncated {
		return nil, failure(dns.RcodeFormatError, "invalid UPDATE zone section or header")
	}
	q := msg.Question[0]
	if q.Qclass != dns.ClassINET {
		return nil, failure(dns.RcodeNotAuth, "only IN zones are hosted")
	}
	apex, err := CanonicalName(q.Name)
	if err != nil {
		return nil, failure(dns.RcodeFormatError, "invalid zone name")
	}
	if len(msg.Answer) > l.MaxUpdateRecords || len(msg.Ns) > l.MaxUpdateRecords-len(msg.Answer) || len(msg.Extra) > l.MaxUpdateRecords-len(msg.Answer)-len(msg.Ns) {
		return nil, failure(dns.RcodeFormatError, "UPDATE record limit exceeded")
	}
	t := &Transaction{apex: apex}
	bytes := int64(12 + len(apex) + 6)
	groups := map[string]int{}
	for section, records := range [][]dns.RR{msg.Answer, msg.Ns, msg.Extra} {
		for _, rr := range records {
			wire, err := Wire(rr)
			if err != nil {
				return nil, failure(dns.RcodeFormatError, "invalid RR encoding")
			}
			if int64(len(wire)) > l.MaxUpdateBytes-bytes {
				return nil, failure(dns.RcodeFormatError, "UPDATE byte limit exceeded")
			}
			bytes += int64(len(wire))
			if section == 2 {
				continue
			}
			h := rr.Header()
			name, err := CanonicalName(h.Name)
			if err != nil {
				return nil, failure(dns.RcodeFormatError, "invalid owner")
			}
			if !dns.IsSubDomain(apex, name) {
				return nil, failure(dns.RcodeNotZone, "owner outside zone")
			}
			rdlen, err := rdataLength(wire)
			if err != nil {
				return nil, failure(dns.RcodeFormatError, err.Error())
			}
			if section == 0 {
				if h.Ttl != 0 || (!dataType(h.Rrtype) && h.Rrtype != dns.TypeANY) {
					return nil, failure(dns.RcodeFormatError, "invalid prerequisite TTL/type")
				}
				p := prerequisite{name: name, typ: h.Rrtype, class: h.Class}
				switch h.Class {
				case dns.ClassANY, dns.ClassNONE:
					if rdlen != 0 {
						return nil, failure(dns.RcodeFormatError, "prerequisite requires empty RDATA")
					}
				case dns.ClassINET:
					if !dataType(h.Rrtype) {
						return nil, failure(dns.RcodeFormatError, "value prerequisite requires data type")
					}
					if err := ValidateData(rr); err != nil {
						return nil, failure(dns.RcodeFormatError, err.Error())
					}
					key, err := Identity(rr)
					if err != nil {
						return nil, failure(dns.RcodeFormatError, err.Error())
					}
					group := name + "\x00" + dns.Type(h.Rrtype).String()
					if i, ok := groups[group]; ok {
						t.prerequisites[i].values = append(t.prerequisites[i].values, key)
						continue
					}
					groups[group] = len(t.prerequisites)
					p.values = []string{key}
				default:
					return nil, failure(dns.RcodeFormatError, "invalid prerequisite class")
				}
				t.prerequisites = append(t.prerequisites, p)
				continue
			}
			switch h.Class {
			case dns.ClassINET:
				if !dataType(h.Rrtype) {
					return nil, failure(dns.RcodeFormatError, "invalid addition type")
				}
			case dns.ClassANY:
				if h.Ttl != 0 || rdlen != 0 || (!dataType(h.Rrtype) && h.Rrtype != dns.TypeANY) {
					return nil, failure(dns.RcodeFormatError, "invalid deletion")
				}
			case dns.ClassNONE:
				if h.Ttl != 0 || !dataType(h.Rrtype) {
					return nil, failure(dns.RcodeFormatError, "invalid exact deletion")
				}
			default:
				return nil, failure(dns.RcodeFormatError, "invalid update class")
			}
			if h.Class != dns.ClassANY {
				if err := ValidateData(rr); err != nil {
					return nil, failure(dns.RcodeFormatError, err.Error())
				}
			}
			key, err := Identity(rr)
			if err != nil {
				return nil, failure(dns.RcodeFormatError, err.Error())
			}
			t.mutations = append(t.mutations, mutation{rr: Copy(rr), name: name, key: key})
		}
	}
	if bytes > l.MaxUpdateBytes {
		return nil, failure(dns.RcodeFormatError, "UPDATE byte limit exceeded")
	}
	return t, nil
}
