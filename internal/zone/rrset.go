package zone

import (
	"bytes"
	"slices"
	"strconv"

	"github.com/miekg/dns"
)

// RRset is every record of one owner name and type. It is the boundary type
// of the zone model: callers outside the miekg-confined packages (§2) see
// RRsets through zone methods rather than building dns.RR values.
//
// RRs returned from a built Zone are shared by every reader of the
// snapshot and must be treated as read-only; copy them (dns.Copy) before
// changing a header, for example to decrement a TTL.
type RRset struct {
	Name string // lowercased absolute owner name
	Type uint16
	TTL  uint32
	RRs  []dns.RR
}

// TypeString returns the presentation mnemonic of the set's type.
func (s *RRset) TypeString() string { return typeString(s.Type) }

// add appends rr to the set unless an identical record is present, and
// lowers the set TTL to the record TTL when that is smaller (RFC 2181 §5.2
// requires one TTL per RRset).
func (s *RRset) add(rr dns.RR) bool {
	for _, have := range s.RRs {
		if dns.IsDuplicate(have, rr) {
			return false
		}
	}
	s.RRs = append(s.RRs, rr)
	if ttl := rr.Header().Ttl; len(s.RRs) == 1 || ttl < s.TTL {
		s.TTL = ttl
	}
	return true
}

// normalise gives every record the set TTL and the shared owner string and
// sorts the records into canonical rdata order.
func (s *RRset) normalise() {
	for _, rr := range s.RRs {
		h := rr.Header()
		h.Ttl = s.TTL
		h.Name = s.Name
	}
	if len(s.RRs) > 1 {
		type keyed struct {
			rdata []byte
			rr    dns.RR
		}
		ks := make([]keyed, len(s.RRs))
		for i, rr := range s.RRs {
			ks[i] = keyed{canonicalRdata(rr), rr}
		}
		slices.SortStableFunc(ks, func(a, b keyed) int { return bytes.Compare(a.rdata, b.rdata) })
		for i := range ks {
			s.RRs[i] = ks[i].rr
		}
	}
}

// canonicalRdata returns the uncompressed wire rdata of rr with domain names
// lowercased where RFC 4034 §6.2 requires it. It works on a copy.
func canonicalRdata(rr dns.RR) []byte {
	c := dns.Copy(rr)
	h := c.Header()
	h.Name = "."
	if lowercaseRdataNames(h.Rrtype) {
		lowercaseNames(c)
	}
	buf := make([]byte, dns.Len(c)+16)
	off, err := dns.PackRR(c, buf, 0, nil, false)
	if err != nil {
		return []byte(c.String())
	}
	// Skip the header: root name (1) + type, class, ttl, rdlength (10).
	return buf[11:off]
}

// lowercaseRdataNames reports whether the canonical form of type t
// lowercases embedded domain names (RFC 4034 §6.2 item 3, RFC 6840 §5.1).
func lowercaseRdataNames(t uint16) bool {
	switch t {
	case dns.TypeNS, dns.TypeCNAME, dns.TypeSOA, dns.TypePTR, dns.TypeMX,
		dns.TypeDNAME, dns.TypeSRV, dns.TypeNAPTR:
		return true
	}
	return false
}

// lowercaseNames lowercases the domain names inside rr in place.
func lowercaseNames(rr dns.RR) {
	switch r := rr.(type) {
	case *dns.NS:
		r.Ns = canonicalName(r.Ns)
	case *dns.CNAME:
		r.Target = canonicalName(r.Target)
	case *dns.DNAME:
		r.Target = canonicalName(r.Target)
	case *dns.PTR:
		r.Ptr = canonicalName(r.Ptr)
	case *dns.MX:
		r.Mx = canonicalName(r.Mx)
	case *dns.SRV:
		r.Target = canonicalName(r.Target)
	case *dns.NAPTR:
		r.Replacement = canonicalName(r.Replacement)
	case *dns.SOA:
		r.Ns = canonicalName(r.Ns)
		r.Mbox = canonicalName(r.Mbox)
	}
}

// typeString is dns.TypeToString with the RFC 3597 fallback.
func typeString(t uint16) string {
	if s, ok := dns.TypeToString[t]; ok {
		return s
	}
	return "TYPE" + strconv.Itoa(int(t))
}
