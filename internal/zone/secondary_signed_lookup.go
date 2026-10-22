package zone

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// LookupSignedWithin serves immutable transferred proof material using the
// caller's captured authority boundary and clock. It never validates trust.
// Unsigned zones retain their ordinary lookup behavior.
func (z *Zone) LookupSignedWithin(qname string, qtype uint16, hops int, owns func(string) bool, do bool, now time.Time) (Result, error) {
	if z.secondaryProofs != nil {
		return z.secondaryProofs.LookupWithin(qname, qtype, hops, owns, do, now)
	}
	if z.typ == v1alpha1.ZoneTypeSecondary && z.hasSignatures {
		return Result{}, fmt.Errorf("signed secondary proof index is unavailable")
	}
	return z.LookupWithin(qname, qtype, hops, owns), nil
}

type secondaryAssembly struct {
	p               *SecondaryProofs
	qtype           uint16
	do              bool
	now             time.Time
	rrsets          map[secondaryRRKey]bool
	bytes, hashWork int
}

// LookupWithin assembles applicable transferred signatures and denial proofs
// while following the ordinary lookup's captured-zone/alias boundaries. It
// returns private response records; raw immutable transfer data is untouched.
func (p *SecondaryProofs) LookupWithin(qname string, qtype uint16, hops int, owns func(string) bool, do bool, now time.Time) (Result, error) {
	if p == nil {
		return Result{}, fmt.Errorf("signed secondary proof index is required")
	}
	z := p.z
	if !z.AvailableAt(now) {
		return Result{}, fmt.Errorf("signed secondary has expired")
	}
	a := &secondaryAssembly{p: p, qtype: qtype, do: do, now: now, rrsets: map[secondaryRRKey]bool{}}
	r := Result{Hops: hops}
	start := hops
	for {
		key, err := canonicalKey(qname)
		if err != nil || !isKeyPrefix(key, z.key) {
			return Result{}, fmt.Errorf("invalid signed secondary query name")
		}
		part := Result{}
		next, done := z.step(&part, qname, []byte(key), qtype)
		if secondaryDNSSECType(qtype) && part.Kind == Delegation {
			if n := z.index[key]; n != nil && n.get(dns.TypeNS) != nil {
				if stored := n.get(qtype); stored != nil {
					part = Result{Kind: Success, Answer: stored.RRs}
					next, done = "", true
				}
			}
		}
		source := ""
		if part.Wildcard {
			source = wildcardName(a.closestEncloser(qname))
		}
		synthesized := false
		for _, rr := range part.Answer {
			if rr.Header().Rrtype == dns.TypeDNAME {
				synthesized = true
				break
			}
		}
		answer, err := a.section(part.Answer, source, synthesized, false, ^uint32(0))
		if err != nil {
			return Result{}, err
		}
		r.Answer = append(r.Answer, answer...)
		r.Wildcard = r.Wildcard || part.Wildcard
		if do && part.Wildcard {
			proofs, err := a.wildcardProof(qname)
			if err != nil {
				return Result{}, err
			}
			r.Ns = append(r.Ns, proofs...)
		}
		if done {
			r.Kind = part.Kind
			switch part.Kind {
			case NXDomain, NoData:
				authority, err := a.section(part.Ns, "", false, false, z.negSOA.Hdr.Ttl)
				if err != nil {
					return Result{}, err
				}
				r.Ns = append(r.Ns, authority...)
				if do {
					proofs, err := a.denial(qname, qtype, part.Kind == NXDomain, part.Wildcard)
					if err != nil {
						return Result{}, err
					}
					r.Ns = append(r.Ns, proofs...)
				}
			case Delegation:
				if r.Hops > start {
					r.Kind, r.Target = CNAME, qname
					return r, nil
				}
				authority, err := a.referral(part.Ns)
				if err != nil {
					return Result{}, err
				}
				r.Ns = append(r.Ns, authority...)
				additional, err := a.section(part.Extra, "", false, true, ^uint32(0))
				if err != nil {
					return Result{}, err
				}
				r.Extra = additional
			default:
				additional, err := a.section(part.Extra, "", false, true, ^uint32(0))
				if err != nil {
					return Result{}, err
				}
				r.Extra = additional
			}
			r.Ns = dedupSecondaryRecords(r.Ns)
			return r, nil
		}
		r.Hops++
		if r.Hops > MaxChain {
			return Result{Kind: Loop, Hops: r.Hops}, nil
		}
		if owns != nil && !owns(next) {
			r.Kind, r.Target = CNAME, next
			return r, nil
		}
		if !z.Contains(next) {
			r.Kind, r.Target = CNAME, next
			return r, nil
		}
		qname = next
	}
}

func secondaryDNSSECType(t uint16) bool {
	switch t {
	case dns.TypeRRSIG, dns.TypeDNSKEY, dns.TypeDS, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeNSEC3PARAM, dns.TypeSIG, dns.TypeKEY, dns.TypeNXT:
		return true
	}
	return false
}

// section preserves RRset order while attaching only covered signatures.
// Additional/glue is optional; unsigned delegation addresses stay unsigned.
func (a *secondaryAssembly) section(records []dns.RR, source string, synthesized, optional bool, capTTL uint32) ([]dns.RR, error) {
	groups := map[secondaryRRKey][]dns.RR{}
	var order []secondaryRRKey
	stagedBytes := 0
	for _, rr := range records {
		t := rr.Header().Rrtype
		if t == dns.TypeRRSIG && a.qtype != dns.TypeRRSIG {
			continue
		}
		if !a.do && secondaryDNSSECType(t) && t != a.qtype {
			continue
		}
		stagedBytes += dns.Len(rr)
		if stagedBytes > secondaryMaxProofBytes-a.bytes {
			return nil, fmt.Errorf("signed secondary response exceeds byte budget")
		}
		k := secondaryRRKey{dns.CanonicalName(rr.Header().Name), t}
		if len(groups[k]) == 0 {
			order = append(order, k)
			if len(order) > secondaryMaxProofRRsets {
				return nil, fmt.Errorf("signed secondary response exceeds RRset budget")
			}
		}
		groups[k] = append(groups[k], rr)
	}
	var out []dns.RR
	for _, key := range order {
		group := groups[key]
		original := key
		if source != "" {
			original.name = dns.CanonicalName(source)
		}
		ttl := group[0].Header().Ttl
		for _, rr := range group {
			ttl = min(ttl, rr.Header().Ttl)
		}
		ttl = min(ttl, a.zoneTTL())
		if capTTL != ^uint32(0) {
			ttl = min(ttl, capTTL)
		}
		var signatures []*dns.RRSIG
		// Explicitly requested stored DNSSEC material is diagnostic raw data.
		// It does not require live signatures covering the requested record.
		diagnostic := secondaryDNSSECType(key.typ) && key.typ == a.qtype
		if a.do && key.typ != dns.TypeRRSIG && (!synthesized || key.typ != dns.TypeCNAME) {
			for _, sig := range a.p.signatures[original] {
				if secondarySignatureLive(sig, a.now) {
					signatures = append(signatures, sig)
				}
			}
			if len(signatures) == 0 && !optional && !diagnostic {
				return nil, fmt.Errorf("missing live applicable signature for %s %s", original.name, dns.TypeToString[original.typ])
			}
			// Any attached signature must remain useful for the advertised TTL.
			for _, sig := range signatures {
				ttl = min(ttl, sig.Hdr.Ttl, sig.OrigTtl, a.signatureTTL(sig))
			}
		}
		if err := a.budget(key, group); err != nil {
			return nil, err
		}
		for _, rr := range group {
			c := dns.Copy(rr)
			c.Header().Ttl = ttl
			out = append(out, c)
		}
		for _, sig := range signatures {
			c := dns.Copy(sig).(*dns.RRSIG)
			c.Hdr.Name = group[0].Header().Name
			c.Hdr.Ttl = ttl
			if err := a.budget(secondaryRRKey{key.name, dns.TypeRRSIG}, []dns.RR{c}); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	if synthesized {
		ttl := ^uint32(0)
		for _, rr := range out {
			if rr.Header().Rrtype == dns.TypeDNAME {
				ttl = min(ttl, rr.Header().Ttl)
			}
		}
		for _, rr := range out {
			if rr.Header().Rrtype == dns.TypeCNAME {
				rr.Header().Ttl = min(rr.Header().Ttl, ttl)
			}
		}
	}
	return out, nil
}

func (a *secondaryAssembly) zoneTTL() uint32 {
	seconds := a.p.z.nextExpiry.Sub(a.now) / time.Second
	if seconds <= 0 {
		return 0
	}
	if seconds > time.Duration(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(seconds) //nolint:gosec // Bounded above and nonnegative.
}
func (a *secondaryAssembly) signatureTTL(sig *dns.RRSIG) uint32 {
	// Expiration is a 32-bit serial timestamp. Interpret the nearest future
	// epoch only inside the valid, less-than-half-range signature window.
	now := uint32(a.now.Unix()) //nolint:gosec // Intentional RFC serial timestamp wrap.
	remaining := sig.Expiration - now
	if remaining == 0 || remaining >= 1<<31 {
		return 0
	}
	if a.now.Nanosecond() != 0 {
		remaining--
	}
	return remaining
}
func (a *secondaryAssembly) budget(key secondaryRRKey, records []dns.RR) error {
	a.rrsets[key] = true
	if len(a.rrsets) > secondaryMaxProofRRsets {
		return fmt.Errorf("signed secondary response exceeds RRset budget")
	}
	for _, rr := range records {
		a.bytes += dns.Len(rr)
	}
	if a.bytes > secondaryMaxProofBytes {
		return fmt.Errorf("signed secondary response exceeds byte budget")
	}
	return nil
}

func (a *secondaryAssembly) closestEncloser(name string) string {
	z := a.p.z
	key, err := canonicalKey(name)
	if err != nil {
		return z.name
	}
	for {
		if n := z.index[key]; n != nil && n.get(dns.TypeNSEC3) == nil {
			return n.name
		}
		if key == z.key || len(key) == 0 {
			return z.name
		}
		key = strings.TrimSuffix(key, "\x00")
		pos := strings.LastIndexByte(key, keySep)
		if pos < 0 {
			key = ""
		} else {
			key = key[:pos+1]
		}
	}
}
func wildcardName(ce string) string {
	if ce == "." {
		return "*."
	}
	return "*." + ce
}
func secondaryNextCloser(name, ce string) string {
	labels := dns.SplitDomainName(name)
	want := dns.CountLabel(ce) + 1
	if len(labels) <= want {
		return dns.CanonicalName(name)
	}
	return dns.CanonicalName(dns.Fqdn(strings.Join(labels[len(labels)-want:], ".")))
}
func secondaryHasType(bits []uint16, t uint16) bool { return slices.Contains(bits, t) }
func secondaryMissing(bits []uint16, t uint16) bool {
	return !secondaryHasType(bits, t) && !secondaryHasType(bits, dns.TypeCNAME)
}

func (a *secondaryAssembly) hash(name string) (string, error) {
	key, err := canonicalKey(name)
	if err != nil {
		return "", err
	}
	if h, ok := a.p.hashes[key]; ok {
		return h, nil
	}
	if a.p.params == nil {
		return "", fmt.Errorf("missing NSEC3 parameters")
	}
	a.hashWork += int(a.p.params.Iterations) + 1
	if a.hashWork > secondaryMaxNSEC3HashWork {
		return "", fmt.Errorf("signed secondary NSEC3 hash work exceeds budget")
	}
	return strings.ToUpper(dns.HashName(name, a.p.params.Hash, a.p.params.Iterations, a.p.params.Salt)), nil
}
func (a *secondaryAssembly) n3(name string, exact, optout bool) (*dns.NSEC3, error) {
	h, err := a.hash(name)
	if err != nil {
		return nil, err
	}
	rr := a.p.nsec3ForHash(h, exact)
	if rr != nil && !exact && rr.Flags&1 != 0 && !optout {
		return nil, fmt.Errorf("opt-out cannot prove ordinary name absence")
	}
	return rr, nil
}
func (a *secondaryAssembly) proofRecords(records ...dns.RR) ([]dns.RR, error) {
	var selected []dns.RR
	seen := map[secondaryRRKey]bool{}
	for _, rr := range records {
		if rr == nil || reflect.ValueOf(rr).IsNil() {
			return nil, fmt.Errorf("required signed secondary denial proof is missing")
		}
		key := secondaryRRKey{dns.CanonicalName(rr.Header().Name), rr.Header().Rrtype}
		if seen[key] {
			continue
		}
		seen[key] = true
		selected = append(selected, rr)
	}
	return a.section(selected, "", false, false, a.p.z.negSOA.Hdr.Ttl)
}
func (a *secondaryAssembly) wildcardProof(name string) ([]dns.RR, error) {
	ce := a.closestEncloser(name)
	nc := secondaryNextCloser(name, ce)
	if len(a.p.nsec) > 0 {
		return a.proofRecords(a.p.coveringNSEC(nc))
	}
	// RFC 5155 section 8.8: the positive wildcard's RRSIG Labels already
	// identifies the closest encloser. Only next-closer coverage is needed.
	cover, err := a.n3(nc, false, false)
	if err != nil {
		return nil, err
	}
	return a.proofRecords(cover)
}

func (a *secondaryAssembly) denial(name string, typ uint16, nxdomain, wild bool) ([]dns.RR, error) {
	if len(a.p.nsec) > 0 {
		if !nxdomain && !wild {
			if n := a.p.exactNSEC(name); n != nil {
				if !secondaryMissing(n.TypeBitMap, typ) {
					return nil, fmt.Errorf("NSEC contradicts NODATA")
				}
				return a.proofRecords(n)
			}
			// NSEC omits empty nonterminals. Coverage plus the following descendant
			// proves existence of the empty name and absence of an RRset at it.
			if a.p.z.index[keyForProof(name)] != nil {
				n := a.p.coveringNSEC(name)
				if n == nil || !dns.IsSubDomain(name, n.NextDomain) {
					return nil, fmt.Errorf("missing empty-nonterminal NSEC proof")
				}
				return a.proofRecords(n)
			}
		}
		ce := a.closestEncloser(name)
		nc := secondaryNextCloser(name, ce)
		w := wildcardName(ce)
		cover := a.p.coveringNSEC(nc)
		if nxdomain {
			return a.proofRecords(cover, a.p.coveringNSEC(w))
		}
		exact := a.p.exactNSEC(w)
		if exact == nil || !secondaryMissing(exact.TypeBitMap, typ) {
			return nil, fmt.Errorf("missing wildcard NODATA bitmap")
		}
		return a.proofRecords(cover, exact)
	}
	if !nxdomain && !wild {
		exact, err := a.n3(name, true, false)
		if err != nil {
			return nil, err
		}
		if exact != nil {
			if !secondaryMissing(exact.TypeBitMap, typ) {
				return nil, fmt.Errorf("NSEC3 contradicts NODATA")
			}
			return a.proofRecords(exact)
		}
		if typ == dns.TypeDS {
			return a.insecureNSEC3(name)
		}
	}
	ce := a.closestEncloser(name)
	nc := secondaryNextCloser(name, ce)
	w := wildcardName(ce)
	exact, err := a.n3(ce, true, false)
	if err != nil {
		return nil, err
	}
	cover, err := a.n3(nc, false, false)
	if err != nil {
		return nil, err
	}
	if nxdomain {
		absent, err := a.n3(w, false, false)
		if err != nil {
			return nil, err
		}
		return a.proofRecords(exact, cover, absent)
	}
	wildcard, err := a.n3(w, true, false)
	if err != nil {
		return nil, err
	}
	if wildcard == nil || !secondaryMissing(wildcard.TypeBitMap, typ) {
		return nil, fmt.Errorf("missing NSEC3 wildcard NODATA bitmap")
	}
	return a.proofRecords(exact, cover, wildcard)
}
func keyForProof(name string) string { key, _ := canonicalKey(name); return key }

func (a *secondaryAssembly) insecureNSEC3(name string) ([]dns.RR, error) {
	ce := name
	var exact *dns.NSEC3
	for exact == nil && ce != a.p.z.name {
		labels := dns.SplitDomainName(ce)
		ce = dns.Fqdn(strings.Join(labels[1:], "."))
		var err error
		exact, err = a.n3(ce, true, false)
		if err != nil {
			return nil, err
		}
	}
	if exact == nil {
		var err error
		exact, err = a.n3(ce, true, false)
		if err != nil {
			return nil, err
		}
	}
	cover, err := a.n3(secondaryNextCloser(name, ce), false, true)
	if err != nil {
		return nil, err
	}
	if cover == nil || cover.Flags&1 == 0 {
		return nil, fmt.Errorf("insecure delegation lacks NSEC3 opt-out proof")
	}
	return a.proofRecords(exact, cover)
}
func (a *secondaryAssembly) referral(ns []dns.RR) ([]dns.RR, error) {
	out, err := a.section(ns, "", false, true, ^uint32(0))
	if err != nil || !a.do || len(ns) == 0 {
		return out, err
	}
	owner := dns.CanonicalName(ns[0].Header().Name)
	n := a.p.z.index[keyForProof(owner)]
	if n == nil {
		return nil, fmt.Errorf("missing delegation owner")
	}
	if ds := n.get(dns.TypeDS); ds != nil {
		records, err := a.section(ds.RRs, "", false, false, ^uint32(0))
		return append(out, records...), err
	}
	records, err := a.denial(owner, dns.TypeDS, false, false)
	return append(out, records...), err
}

func dedupSecondaryRecords(records []dns.RR) []dns.RR {
	out := make([]dns.RR, 0, len(records))
	seen := map[string]bool{}
	for _, rr := range records {
		key := dns.CanonicalName(rr.Header().Name) + fmt.Sprint(rr.Header().Rrtype) + string(canonicalRdata(rr))
		if !seen[key] {
			out = append(out, rr)
			seen[key] = true
		}
	}
	return out
}

func secondarySignatureLive(sig *dns.RRSIG, now time.Time) bool {
	current := uint32(now.Unix()) //nolint:gosec // Intentional RFC serial-time wrap.
	span := sig.Expiration - sig.Inception
	return span > 0 && span < 1<<31 && current-sig.Inception < span
}
