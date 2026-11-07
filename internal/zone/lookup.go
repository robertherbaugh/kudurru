package zone

import (
	"github.com/miekg/dns"
)

// MaxChain caps the number of CNAME and DNAME hops followed for one query,
// inside a zone and across zones (dev-docs/design-record.md §7.1 step 6).
const MaxChain = 20

// Kind classifies a lookup result.
type Kind uint8

// Result kinds.
const (
	// Success: the answer section holds the requested RRset, possibly
	// preceded by an in-zone CNAME/DNAME chain.
	Success Kind = iota
	// NoData: the name exists but has no records of the type; the
	// authority section holds the SOA.
	NoData
	// NXDomain: the name does not exist (and no wildcard covers it); the
	// authority section holds the SOA.
	NXDomain
	// Delegation: the name is at or below a zone cut; the authority section
	// holds the child NS RRset and the additional section in-zone glue.
	Delegation
	// CNAME: the answer is a CNAME/DNAME chain whose last target lies
	// outside the zone or below a zone cut. Target is the name to continue
	// with; the caller resolves it elsewhere (another local zone or the
	// rest of the pipeline) and splices the result after Answer.
	CNAME
	// Loop: the chain exceeded MaxChain hops; answer SERVFAIL.
	Loop
	// YXDomain: a DNAME substitution produced a name longer than 255
	// octets (RFC 6672 §2.2); answer YXDOMAIN with the DNAME.
	YXDomain
	// Fallback: the name is absent from an opt-in overlay zone. Target is
	// the unresolved name, possibly after local CNAME/DNAME records in Answer.
	Fallback
)

var kindNames = [...]string{"success", "nodata", "nxdomain", "delegation", "cname", "loop", "yxdomain", "fallback"}

// String returns a lower-case name for logs and tests.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "unknown"
}

// Result is the outcome of Zone.Lookup: the three message sections and
// what they mean. The RRs are shared with the zone (except records
// synthesised from wildcards and DNAMEs) and must not be modified.
type Result struct {
	Kind   Kind
	Answer []dns.RR
	Ns     []dns.RR
	Extra  []dns.RR
	// Target is set for Kind CNAME or Fallback: the name to resolve next.
	Target string
	// Hops counts the CNAME/DNAME hops taken, including any the caller
	// passed to LookupFrom.
	Hops int
	// Wildcard reports that an answer was synthesised from a wildcard.
	Wildcard bool
}

// Rcode returns the response code for the result.
func (r *Result) Rcode() int {
	switch r.Kind {
	case NXDomain:
		return dns.RcodeNameError
	case Loop:
		return dns.RcodeServerFailure
	case YXDomain:
		return dns.RcodeYXDomain
	}
	return dns.RcodeSuccess
}

// Authoritative reports whether the result can carry AA. Referrals,
// unresolved fallback and alias loops do not assert authority.
func (r *Result) Authoritative() bool {
	return r.Kind != Delegation && r.Kind != Loop && r.Kind != Fallback
}

// Positive reports whether the result is complete data the zone positively
// asserts (an answer or a referral), as opposed to a denial or a chain that
// must be continued elsewhere. The authoritative stage serves only positive
// results while the zone cache is unsynced.
func (r *Result) Positive() bool {
	return r.Kind == Success || r.Kind == Delegation
}

// Lookup answers qname/qtype from the zone (RFC 1034 §4.3.2 with RFC 4592
// wildcards and RFC 6672 DNAME): zone cuts first, then exact match, CNAME
// chasing inside the zone, then the closest encloser's wildcard. qname must
// be at or below the zone apex; case does not matter. Lookup on a forward
// zone returns Kind CNAME with Target qname: continue elsewhere.
func (z *Zone) Lookup(qname string, qtype uint16) Result { return z.LookupFrom(qname, qtype, 0) }

// LookupFrom is Lookup for the continuation of a chain that already took
// hops hops in other zones; the MaxChain budget is shared.
func (z *Zone) LookupFrom(qname string, qtype uint16, hops int) Result {
	return z.LookupWithin(qname, qtype, hops, nil)
}

// LookupStep stops at the first alias target so the caller can select a
// more-specific zone before continuing the chain.
func (z *Zone) LookupStep(qname string, qtype uint16, hops int) Result {
	return z.LookupWithin(qname, qtype, hops, func(string) bool { return false })
}

// LookupWithin stops a CNAME/DNAME chain before a target belongs to another
// configured zone. owns is evaluated for alias targets, not ordinary lookups;
// nil keeps the standalone LookupFrom behavior. The caller then checks the
// target zone's access policy and availability before continuing the chain.
func (z *Zone) LookupWithin(qname string, qtype uint16, hops int, owns func(string) bool) Result {
	r := Result{Hops: hops}
	if z.apex == nil {
		r.Kind, r.Target = CNAME, qname
		return r
	}
	z.resolve(&r, qname, qtype, owns)
	return r
}

func (z *Zone) resolve(r *Result, qname string, qtype uint16, owns func(string) bool) {
	var kb [256]byte
	start := r.Hops
	for {
		key, err := appendKey(kb[:0], qname)
		if err != nil {
			r.Kind, r.Ns = NXDomain, z.negNs
			return
		}
		if !hasKeyPrefix(key, z.key) {
			r.Kind, r.Target = CNAME, qname
			return
		}
		// The generated SOA/NS describes local zone management, not an apex
		// hostname override. Keep those types local while allowing public apex
		// addresses and mail records until the operator defines an apex record.
		if z.fallback && len(key) == len(z.key) && !z.apexOverride && qtype != dns.TypeSOA && qtype != dns.TypeNS && qtype != dns.TypeANY {
			r.Kind, r.Target = Fallback, qname
			return
		}
		next, done := z.step(r, qname, key, qtype)
		if done {
			if r.Kind == NXDomain && z.fallback {
				r.Kind, r.Target, r.Ns, r.Extra = Fallback, qname, nil, nil
			}
			if r.Kind == Delegation && r.Hops > start {
				// A chain ran into a zone cut: the target is served by
				// another server, so it leaves the zone.
				r.Kind, r.Target, r.Ns, r.Extra = CNAME, qname, nil, nil
			}
			return
		}
		r.Hops++
		if r.Hops > MaxChain {
			r.Kind, r.Ns, r.Extra = Loop, nil, nil
			return
		}
		if owns != nil && !owns(next) {
			r.Kind, r.Target = CNAME, next
			return
		}
		qname = next
	}
}

// step resolves one name. It returns done, or the next name of a chain.
func (z *Zone) step(r *Result, qname string, key []byte, qtype uint16) (string, bool) {
	n := z.apex
	ce := z.apex
	if len(key) > len(z.key) {
		if d := n.get(dns.TypeDNAME); d != nil {
			return z.dname(r, qname, d)
		}
	}
	if !z.cuts {
		return z.stepFlat(r, qname, key, qtype)
	}
	for i := len(z.key); i < len(key); i++ {
		if key[i] != keySep {
			continue
		}
		n = z.find(key[:i+1])
		if n == nil {
			return z.wildcard(r, qname, ce, qtype)
		}
		full := i+1 == len(key)
		if ns := n.get(dns.TypeNS); ns != nil && (!full || qtype != dns.TypeDS) {
			z.referral(r, ns)
			return "", true
		}
		if !full {
			if d := n.get(dns.TypeDNAME); d != nil {
				return z.dname(r, qname, d)
			}
		}
		ce = n
	}
	return z.answer(r, n, qname, qtype, false)
}

// stepFlat is step for zones without delegations or DNAMEs below the apex:
// nothing above the query name can change the outcome, so one search
// answers existing names, and only a miss walks back up to find the
// closest encloser for wildcard synthesis.
func (z *Zone) stepFlat(r *Result, qname string, key []byte, qtype uint16) (string, bool) {
	if len(key) == len(z.key) {
		return z.answer(r, z.apex, qname, qtype, false)
	}
	if n := z.find(key); n != nil {
		return z.answer(r, n, qname, qtype, false)
	}
	// key ends with a separator; every earlier separator ends the key of
	// an ancestor. The apex is the encloser of last resort.
	for i := len(key) - 2; i >= len(z.key); i-- {
		if key[i] != keySep {
			continue
		}
		if ce := z.find(key[:i+1]); ce != nil {
			return z.wildcard(r, qname, ce, qtype)
		}
	}
	return z.wildcard(r, qname, z.apex, qtype)
}

// answer produces the answer at an existing node n. For wildcard nodes the
// records are copied with qname as owner.
func (z *Zone) answer(r *Result, n *node, qname string, qtype uint16, wildcard bool) (string, bool) {
	if qtype == dns.TypeANY && !n.empty() {
		for i := range n.sets {
			r.Answer = appendSet(r.Answer, &n.sets[i], qname, wildcard)
		}
		r.Kind = Success
		return "", true
	}
	if s := n.get(qtype); s != nil {
		r.Answer = appendSet(r.Answer, s, qname, wildcard)
		r.Kind = Success
		z.additional(r, s)
		return "", true
	}
	if c := n.get(dns.TypeCNAME); c != nil {
		r.Answer = appendSet(r.Answer, c, qname, wildcard)
		return c.RRs[0].(*dns.CNAME).Target, false
	}
	r.Kind, r.Ns = NoData, z.negNs
	return "", true
}

// wildcard applies RFC 4592: the source of synthesis is "*" below the
// closest encloser ce.
func (z *Zone) wildcard(r *Result, qname string, ce *node, qtype uint16) (string, bool) {
	var wb [256]byte
	wk := append(append(wb[:0], ce.key...), '*', keySep)
	w := z.find(wk)
	if w == nil {
		r.Kind, r.Ns = NXDomain, z.negNs
		return "", true
	}
	r.Wildcard = true
	return z.answer(r, w, qname, qtype, true)
}

// dname adds the DNAME and the synthesised CNAME (RFC 6672 §3.1).
func (z *Zone) dname(r *Result, qname string, d *RRset) (string, bool) {
	dn := d.RRs[0].(*dns.DNAME)
	r.Answer = append(r.Answer, dn)
	target, ok := replaceSuffix(qname, d.Name, dn.Target)
	if !ok {
		r.Kind = YXDomain
		return "", true
	}
	r.Answer = append(r.Answer, &dns.CNAME{
		Hdr:    dns.RR_Header{Name: qname, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: d.TTL},
		Target: target,
	})
	return target, false
}

// referral fills a delegation: child NS in authority, glue in additional.
func (z *Zone) referral(r *Result, ns *RRset) {
	r.Kind = Delegation
	r.Ns = append(r.Ns, ns.RRs...)
	for _, rr := range ns.RRs {
		r.Extra = z.appendAddrs(r.Extra, rr.(*dns.NS).Ns)
	}
}

// additional adds in-zone addresses for the targets of NS, MX and SRV
// answers (RFC 1035 §3.3, RFC 2782).
func (z *Zone) additional(r *Result, s *RRset) {
	for _, rr := range s.RRs {
		switch t := rr.(type) {
		case *dns.NS:
			r.Extra = z.appendAddrs(r.Extra, t.Ns)
		case *dns.MX:
			r.Extra = z.appendAddrs(r.Extra, t.Mx)
		case *dns.SRV:
			r.Extra = z.appendAddrs(r.Extra, t.Target)
		}
	}
}

// appendAddrs appends the A and AAAA RRsets of an in-zone host.
func (z *Zone) appendAddrs(extra []dns.RR, host string) []dns.RR {
	var kb [256]byte
	key, err := appendKey(kb[:0], host)
	if err != nil || !hasKeyPrefix(key, z.key) {
		return extra
	}
	n := z.find(key)
	if n == nil {
		return extra
	}
	for _, t := range [...]uint16{dns.TypeA, dns.TypeAAAA} {
		if s := n.get(t); s != nil {
			if len(extra) > 0 && extra[len(extra)-1] == s.RRs[len(s.RRs)-1] {
				continue // already present (two NS records naming one host)
			}
			extra = append(extra, s.RRs...)
		}
	}
	return extra
}

// appendSet appends the records of s; wildcard records are copied with
// owner as their name.
func appendSet(dst []dns.RR, s *RRset, owner string, wildcard bool) []dns.RR {
	if !wildcard {
		return append(dst, s.RRs...)
	}
	for _, rr := range s.RRs {
		c := dns.Copy(rr)
		c.Header().Name = owner
		dst = append(dst, c)
	}
	return dst
}

// find returns the node with key k, or nil. The map index conversion
// string(k) does not allocate.
func (z *Zone) find(k []byte) *node { return z.index[string(k)] }

// hasKeyPrefix reports whether key starts with prefix without allocating.
func hasKeyPrefix(key []byte, prefix string) bool {
	return len(key) >= len(prefix) && string(key[:len(prefix)]) == prefix
}
