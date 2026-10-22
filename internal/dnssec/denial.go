package dnssec

import (
	"bytes"
	"encoding/base32"
	"encoding/hex"
	"strings"

	"github.com/miekg/dns"
)

func bitmap(bits []uint16, t uint16) bool {
	for _, b := range bits {
		if b == t {
			return true
		}
	}
	return false
}
func missingType(bits []uint16, t uint16) bool {
	return !bitmap(bits, t) && !bitmap(bits, dns.TypeCNAME) && (t == dns.TypeDS || !delegation(bits))
}
func delegation(bits []uint16) bool {
	return bitmap(bits, dns.TypeNS) && !bitmap(bits, dns.TypeSOA) && !bitmap(bits, dns.TypeDS)
}

// compareNames implements RFC 4034 canonical ordering, including escaped
// label octets. DNS textual lexicographic order is not NSEC order.
func compareNames(a, b string) int {
	labels := func(n string) [][]byte {
		buf := make([]byte, 255)
		end, err := dns.PackDomainName(n, buf, 0, nil, false)
		if err != nil {
			return nil
		}
		var out [][]byte
		for p := 0; p < end && buf[p] != 0; {
			size := int(buf[p])
			p++
			l := buf[p : p+size]
			for i, c := range l {
				if c >= 'A' && c <= 'Z' {
					l[i] = c + 32
				}
			}
			out = append(out, l)
			p += size
		}
		return out
	}
	aa, bb := labels(a), labels(b)
	for i, j := len(aa)-1, len(bb)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if n := bytes.Compare(aa[i], bb[j]); n != 0 {
			return n
		}
	}
	if len(aa) < len(bb) {
		return -1
	}
	if len(aa) > len(bb) {
		return 1
	}
	return 0
}
func interval(owner, next, name string, compare func(string, string) int) bool {
	if compare(owner, name) == 0 || compare(next, name) == 0 {
		return false
	}
	if compare(owner, next) < 0 {
		return compare(owner, name) < 0 && compare(name, next) < 0
	}
	return compare(owner, name) < 0 || compare(name, next) < 0
}
func parents(name string) []string {
	labels := dns.SplitDomainName(name)
	var out []string
	for i := range labels {
		out = append(out, dns.Fqdn(strings.Join(labels[i:], ".")))
	}
	return append(out, ".")
}
func nextCloser(name, ce string) string {
	p := parents(name)
	for i := 0; i < len(p)-1; i++ {
		if strings.EqualFold(p[i+1], ce) {
			return p[i]
		}
	}
	return name
}

type proofs struct {
	nsec  []*dns.NSEC
	nsec3 []*dns.NSEC3
}

func (s *validation) proofs(r records, zone string) (proofs, error) {
	var p proofs
	var params *dns.NSEC3
	for k, rs := range r.sets {
		if k.typ != dns.TypeNSEC && k.typ != dns.TypeNSEC3 {
			continue
		}
		if len(rs) != 1 || !dns.IsSubDomain(zone, k.name) {
			return p, bogus("invalid_denial_owner", dns.ExtendedErrorCodeDNSBogus)
		}
		labels := dns.CountLabel(k.name)
		if strings.HasPrefix(k.name, "*.") {
			labels--
		}
		for _, sig := range r.sigs[k] {
			if int(sig.Labels) != labels {
				return p, bogus("expanded_denial_owner", dns.ExtendedErrorCodeDNSBogus)
			}
		}
		switch v := rs[0].(type) {
		case *dns.NSEC:
			if !dns.IsSubDomain(zone, v.NextDomain) {
				return p, bogus("nsec_outside_zone", dns.ExtendedErrorCodeDNSBogus)
			}
			p.nsec = append(p.nsec, v)
		case *dns.NSEC3:
			if v.Hash != 1 || v.Flags & ^uint8(1) != 0 || v.Iterations > s.v.limits.NSEC3Iterations {
				return p, incomplete("nsec3_parameters")
			}
			if salt, err := hex.DecodeString(v.Salt); err != nil || len(salt) > 32 {
				return p, incomplete("nsec3_salt")
			}
			labels := dns.SplitDomainName(v.Hdr.Name)
			if len(labels) < 1 || !strings.EqualFold(dns.Fqdn(strings.Join(labels[1:], ".")), zone) || !validHash(labels[0]) || !validHash(v.NextDomain) {
				return p, bogus("invalid_nsec3_owner", dns.ExtendedErrorCodeDNSBogus)
			}
			if params != nil && (params.Hash != v.Hash || params.Iterations != v.Iterations || !strings.EqualFold(params.Salt, v.Salt)) {
				return p, bogus("mixed_nsec3_parameters", dns.ExtendedErrorCodeDNSBogus)
			}
			params = v
			p.nsec3 = append(p.nsec3, v)
		}
	}
	if len(p.nsec) > 0 && len(p.nsec3) > 0 {
		return p, bogus("mixed_denial_types", dns.ExtendedErrorCodeDNSBogus)
	}
	return p, nil
}

func validHash(value string) bool {
	b, err := base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
	return err == nil && len(b) == 20 && len(value) == 32
}
func (s *validation) hash(name string, n *dns.NSEC3) (string, error) {
	s.hashes += int(n.Iterations) + 1
	if err := s.limit(); err != nil {
		return "", err
	}
	h := dns.HashName(name, n.Hash, n.Iterations, n.Salt)
	if h == "" {
		return "", bogus("invalid_nsec3_hash", dns.ExtendedErrorCodeDNSBogus)
	}
	return strings.ToUpper(h), nil
}
func (s *validation) n3exact(p proofs, name string) (*dns.NSEC3, error) {
	if len(p.nsec3) == 0 {
		return nil, nil
	}
	h, err := s.hash(name, p.nsec3[0])
	if err != nil {
		return nil, err
	}
	for _, n := range p.nsec3 {
		if strings.EqualFold(dns.SplitDomainName(n.Hdr.Name)[0], h) {
			return n, nil
		}
	}
	return nil, nil
}
func (s *validation) n3cover(p proofs, name string, optout bool) (bool, error) {
	if len(p.nsec3) == 0 {
		return false, nil
	}
	h, err := s.hash(name, p.nsec3[0])
	if err != nil {
		return false, err
	}
	for _, n := range p.nsec3 {
		if (n.Flags&1 == 0 || optout) && interval(strings.ToUpper(dns.SplitDomainName(n.Hdr.Name)[0]), strings.ToUpper(n.NextDomain), h, strings.Compare) {
			return true, nil
		}
	}
	return false, nil
}
func ncover(p proofs, name string) bool {
	for _, n := range p.nsec {
		if interval(n.Hdr.Name, n.NextDomain, name, compareNames) {
			return true
		}
	}
	return false
}
func nexact(p proofs, name string) *dns.NSEC {
	for _, n := range p.nsec {
		if compareNames(n.Hdr.Name, name) == 0 {
			return n
		}
	}
	return nil
}
func (s *validation) closest(p proofs, zone, name string) (string, error) {
	for _, a := range parents(name) {
		if !dns.IsSubDomain(zone, a) {
			break
		}
		if len(p.nsec3) > 0 {
			n, err := s.n3exact(p, a)
			if err != nil {
				return "", err
			}
			if n != nil {
				return a, nil
			}
		} else {
			if strings.EqualFold(a, zone) {
				return a, nil
			}
			for _, n := range p.nsec {
				if dns.IsSubDomain(a, n.Hdr.Name) || dns.IsSubDomain(a, n.NextDomain) {
					return a, nil
				}
			}
		}
	}
	return "", bogus("closest_encloser_missing", dns.ExtendedErrorCodeNSECMissing)
}
func (s *validation) wildcard(r records, zone, name, ce string) error {
	p, err := s.proofs(r, zone)
	if err != nil {
		return err
	}
	if !dns.IsSubDomain(ce, name) || strings.EqualFold(ce, name) {
		return bogus("invalid_wildcard_labels", dns.ExtendedErrorCodeDNSBogus)
	}
	nc := nextCloser(name, ce)
	if len(p.nsec) > 0 && ncover(p, nc) {
		return nil
	}
	if len(p.nsec3) > 0 {
		// RFC 5155 section 8.8: the verified wildcard answer's RRSIG
		// establishes the closest encloser. Only next-closer coverage is
		// required here; an exact closest-encloser NSEC3 is not required in
		// a positive wildcard response (and BIND omits it).
		covered, err := s.n3cover(p, nc, false)
		if err != nil {
			return err
		}
		if covered {
			return nil
		}
	}
	return bogus("wildcard_proof_missing", dns.ExtendedErrorCodeNSECMissing)
}

func (s *validation) denial(r records, zone, name string, typ uint16, nxdomain, ds bool) error {
	p, err := s.proofs(r, zone)
	if err != nil {
		return err
	}
	if n := nexact(p, name); n != nil {
		if !nxdomain && ((!ds && missingType(n.TypeBitMap, typ)) || (ds && delegation(n.TypeBitMap))) {
			return nil
		}
		return bogus("denial_type_exists", dns.ExtendedErrorCodeDNSBogus)
	}
	if n, err := s.n3exact(p, name); err != nil {
		return err
	} else if n != nil {
		if !nxdomain && ((!ds && missingType(n.TypeBitMap, typ)) || (ds && delegation(n.TypeBitMap))) {
			return nil
		}
		return bogus("denial_type_exists", dns.ExtendedErrorCodeDNSBogus)
	}
	ce, err := s.closest(p, zone, name)
	if err != nil {
		return err
	}
	if strings.EqualFold(ce, name) {
		if !nxdomain && !ds && ncover(p, name) {
			for _, n := range p.nsec {
				if !strings.EqualFold(name, n.NextDomain) && dns.IsSubDomain(name, n.NextDomain) {
					return nil
				}
			}
		}
		return bogus("denial_name_exists", dns.ExtendedErrorCodeDNSBogus)
	}
	if ds {
		// RFC 5155 opt-out can prove an insecure delegation only with a
		// closest-encloser proof and opt-out coverage of the next closer.
		if len(p.nsec3) > 0 {
			nc := nextCloser(name, ce)
			h, err := s.hash(nc, p.nsec3[0])
			if err != nil {
				return err
			}
			for _, n := range p.nsec3 {
				if n.Flags&1 != 0 && interval(strings.ToUpper(dns.SplitDomainName(n.Hdr.Name)[0]), strings.ToUpper(n.NextDomain), h, strings.Compare) {
					return nil
				}
			}
		}
		return bogus("insecure_delegation_unproven", dns.ExtendedErrorCodeNSECMissing)
	}
	wild := "*." + strings.TrimSuffix(ce, ".")
	if ce != "." {
		wild += "."
	}
	if len(p.nsec) > 0 {
		if !ncover(p, nextCloser(name, ce)) {
			return bogus("name_denial_missing", dns.ExtendedErrorCodeNSECMissing)
		}
		if nxdomain && ncover(p, wild) {
			return nil
		}
		if !nxdomain {
			if n := nexact(p, wild); n != nil && missingType(n.TypeBitMap, typ) {
				return nil
			}
		}
	} else if len(p.nsec3) > 0 {
		cover, err := s.n3cover(p, nextCloser(name, ce), false)
		if err != nil {
			return err
		}
		if cover {
			if nxdomain {
				cover, err := s.n3cover(p, wild, false)
				if err != nil {
					return err
				}
				if cover {
					return nil
				}
			} else {
				n, err := s.n3exact(p, wild)
				if err != nil {
					return err
				}
				if n != nil && missingType(n.TypeBitMap, typ) {
					return nil
				}
			}
		}
	}
	return bogus("wildcard_denial_missing", dns.ExtendedErrorCodeNSECMissing)
}
