package recursor

import (
	"slices"
	"strings"

	"github.com/miekg/dns"
)

// Authenticated denial of existence: NSEC (RFC 4035 §5.4, RFC 6840 §4)
// and NSEC3 (RFC 5155 §8). The records passed in are already validated.

func hasType(bitmap []uint16, t uint16) bool { return slices.Contains(bitmap, t) }

// nsecCovers reports whether n proves that name does not exist: name sorts
// strictly between n's owner and next name (the last NSEC of a zone wraps
// to the apex).
func nsecCovers(n *dns.NSEC, name string) bool {
	owner, next := n.Hdr.Name, n.NextDomain
	if canonicalCompare(owner, name) >= 0 {
		return false
	}
	if canonicalCompare(owner, next) < 0 {
		return canonicalCompare(name, next) < 0
	}
	return isSubOrEqual(next, name) // the last NSEC: next is the apex
}

// delegatesAbove reports whether n sits at a delegation or DNAME above
// name, where it cannot speak for names below (RFC 6840 §4.1).
func delegatesAbove(n *dns.NSEC, name string) bool {
	if !isBelow(n.Hdr.Name, name) {
		return false
	}
	return hasType(n.TypeBitMap, dns.TypeDNAME) ||
		hasType(n.TypeBitMap, dns.TypeNS) && !hasType(n.TypeBitMap, dns.TypeSOA)
}

// nsecCover returns an NSEC that covers name and may speak for it.
func nsecCover(name string, nsecs []*dns.NSEC) *dns.NSEC {
	for _, n := range nsecs {
		if nsecCovers(n, name) && !delegatesAbove(n, name) {
			return n
		}
	}
	return nil
}

// nsecClosestEncloser derives the closest encloser of name from the NSEC
// that covers it.
func nsecClosestEncloser(name string, cover *dns.NSEC) string {
	a := commonAncestor(name, cover.Hdr.Name)
	b := commonAncestor(name, cover.NextDomain)
	if labels(b) > labels(a) {
		return b
	}
	return a
}

func wildcardOf(ce string) string {
	if ce == "." {
		return "*."
	}
	return "*." + ce
}

// nsecProveNX proves NXDOMAIN: name is covered and so is the wildcard at
// its closest encloser.
func nsecProveNX(name string, nsecs []*dns.NSEC) bool {
	cover := nsecCover(name, nsecs)
	if cover == nil {
		return false
	}
	return nsecCover(wildcardOf(nsecClosestEncloser(name, cover)), nsecs) != nil
}

// nsecProveNoData proves NODATA for name/qtype: an NSEC at name without the
// type (or CNAME), an empty non-terminal, or a wildcard without the type.
// For DS it reports whether name is a delegation (NS in the bitmap).
func nsecProveNoData(name string, qtype uint16, nsecs []*dns.NSEC) (ok, cut bool) {
	for _, n := range nsecs {
		if !equalName(n.Hdr.Name, name) {
			continue
		}
		bm := n.TypeBitMap
		if hasType(bm, qtype) || hasType(bm, dns.TypeCNAME) {
			return false, false
		}
		if qtype == dns.TypeDS {
			if hasType(bm, dns.TypeSOA) && name != "." {
				return false, false // the child's apex NSEC cannot deny DS
			}
			return true, hasType(bm, dns.TypeNS)
		}
		if hasType(bm, dns.TypeNS) && !hasType(bm, dns.TypeSOA) {
			return false, false // parent side of a delegation
		}
		return true, false
	}
	cover := nsecCover(name, nsecs)
	if cover == nil {
		return false, false
	}
	if isBelow(name, cover.NextDomain) {
		return true, false // empty non-terminal
	}
	wc := wildcardOf(nsecClosestEncloser(name, cover))
	for _, n := range nsecs {
		if equalName(n.Hdr.Name, wc) && !hasType(n.TypeBitMap, qtype) && !hasType(n.TypeBitMap, dns.TypeCNAME) {
			return true, false
		}
	}
	return false, false
}

// nsec3Set is a validated set of NSEC3 records with one parameter set.
type nsec3Set struct {
	recs   []*dns.NSEC3
	alg    uint8
	iter   uint16
	salt   string
	hashes map[string]string
	budget *int
}

// newNSEC3Set keeps the records it can use: those of zone (owned directly
// under its apex), SHA-1, known flags and the parameters of the first such
// record. insecure is true when the iterations exceed maxNSEC3Iter (RFC
// 9276 §3.2); a nil set means nothing was usable.
func newNSEC3Set(recs []*dns.NSEC3, zone string, budget *int) (*nsec3Set, bool) {
	s := &nsec3Set{hashes: map[string]string{}, budget: budget}
	for _, r := range recs {
		if r.Hash != dns.SHA1 || r.Flags&^1 != 0 || !equalName(parentName(r.Hdr.Name), zone) {
			continue
		}
		if len(s.recs) == 0 {
			s.alg, s.iter, s.salt = r.Hash, r.Iterations, r.Salt
		} else if r.Iterations != s.iter || !strings.EqualFold(r.Salt, s.salt) {
			continue
		}
		s.recs = append(s.recs, r)
	}
	if len(s.recs) == 0 {
		return nil, false
	}
	if s.iter > maxNSEC3Iter {
		return nil, true
	}
	return s, false
}

// hash returns name's NSEC3 hash, "" when the budget is spent.
func (s *nsec3Set) hash(name string) string {
	name = canon(name)
	if h, ok := s.hashes[name]; ok {
		return h
	}
	if *s.budget <= 0 {
		return ""
	}
	*s.budget--
	h := dns.HashName(name, s.alg, s.iter, s.salt)
	s.hashes[name] = h
	return h
}

// ownerHash splits an NSEC3 owner into its hash label and zone.
func ownerHash(r *dns.NSEC3) (string, string) {
	owner := r.Hdr.Name
	off, end := dns.NextLabel(owner, 0)
	if end {
		return "", "."
	}
	return strings.ToUpper(owner[:off-1]), owner[off:]
}

// match returns the record whose owner hash is name's hash.
func (s *nsec3Set) match(name string) *dns.NSEC3 {
	h := s.hash(name)
	if h == "" {
		return nil
	}
	for _, r := range s.recs {
		if oh, zone := ownerHash(r); oh == h && isSubOrEqual(zone, name) {
			return r
		}
	}
	return nil
}

// cover returns a record whose hash interval contains name's hash.
func (s *nsec3Set) cover(name string) *dns.NSEC3 {
	h := s.hash(name)
	if h == "" {
		return nil
	}
	for _, r := range s.recs {
		oh, zone := ownerHash(r)
		if !isSubOrEqual(zone, name) {
			continue
		}
		next := strings.ToUpper(r.NextDomain)
		var in bool
		switch {
		case oh < next:
			in = oh < h && h < next
		case oh == next: // a single record covers everything else
			in = h != oh
		default: // the last record wraps around
			in = h > oh || h < next
		}
		if in {
			return r
		}
	}
	return nil
}

// closestEncloser finds the closest encloser of name that has a matching
// record, and the next closer name below it ("" when name itself matches).
func (s *nsec3Set) closestEncloser(name string) (ce, nextCloser string, ok bool) {
	prev := ""
	for n := canon(name); ; n = parentName(n) {
		if s.match(n) != nil {
			return n, prev, true
		}
		if n == "." || *s.budget <= 0 {
			return "", "", false
		}
		prev = n
	}
}

// proveNX proves NXDOMAIN (RFC 5155 §8.4): the closest encloser matches,
// the next closer name and the wildcard at the closest encloser are
// covered. optOut is set when the next closer's cover has opt-out, which
// leaves the answer insecure.
func (s *nsec3Set) proveNX(name string) (ok, optOut bool) {
	ce, nc, found := s.closestEncloser(name)
	if !found || nc == "" {
		return false, false
	}
	if !enclosesBelow(s.match(ce)) {
		return false, false
	}
	c := s.cover(nc)
	if c == nil || s.cover(wildcardOf(ce)) == nil {
		return false, false
	}
	return true, c.Flags&1 == 1
}

// proveNoData proves NODATA (RFC 5155 §8.5-8.7). For DS, cut reports
// whether name is or may be (opt-out) a delegation.
func (s *nsec3Set) proveNoData(name string, qtype uint16) (ok, cut, optOut bool) {
	if m := s.match(name); m != nil {
		bm := m.TypeBitMap
		if hasType(bm, qtype) || hasType(bm, dns.TypeCNAME) {
			return false, false, false
		}
		if qtype == dns.TypeDS {
			if hasType(bm, dns.TypeSOA) && name != "." {
				return false, false, false
			}
			return true, hasType(bm, dns.TypeNS), false
		}
		if hasType(bm, dns.TypeNS) && !hasType(bm, dns.TypeSOA) {
			return false, false, false
		}
		return true, false, false
	}
	ce, nc, found := s.closestEncloser(name)
	if !found || nc == "" || !enclosesBelow(s.match(ce)) {
		return false, false, false
	}
	c := s.cover(nc)
	if c == nil {
		return false, false, false
	}
	if qtype == dns.TypeDS && c.Flags&1 == 1 {
		return true, true, true // an unsigned delegation may hide under opt-out
	}
	if w := s.match(wildcardOf(ce)); w != nil && !hasType(w.TypeBitMap, qtype) && !hasType(w.TypeBitMap, dns.TypeCNAME) {
		return true, false, c.Flags&1 == 1
	}
	return false, false, false
}

// enclosesBelow reports whether the closest encloser m may speak for names
// below it: not a delegation in the parent (NS without SOA) and not a DNAME
// (RFC 5155 §8.3).
func enclosesBelow(m *dns.NSEC3) bool {
	if m == nil {
		return false
	}
	bm := m.TypeBitMap
	return !hasType(bm, dns.TypeDNAME) && (!hasType(bm, dns.TypeNS) || hasType(bm, dns.TypeSOA))
}
