package recursor

import (
	"strings"
	"time"

	"github.com/miekg/dns"
)

// vres is the outcome of validating one RRset.
type vres struct {
	sec Security
	ede uint16
	why string
	// wildcard is the closest encloser a wildcard expansion was made
	// from; the caller must prove the expanded name does not exist.
	wildcard string
	// signer is the zone whose key verified a secure RRset.
	signer string
}

func bogus(ede uint16, why string) vres { return vres{sec: SecBogus, ede: ede, why: why} }

// supportedAlgorithm reports whether signatures of alg can be checked.
// Others make a zone insecure (RFC 4035 §5.2, RFC 8624).
func supportedAlgorithm(alg uint8) bool {
	switch alg {
	case dns.RSASHA1, dns.RSASHA1NSEC3SHA1, dns.RSASHA256, dns.RSASHA512,
		dns.ECDSAP256SHA256, dns.ECDSAP384SHA384, dns.ED25519:
		return true
	}
	return false
}

func supportedDigest(t uint8) bool {
	switch t {
	case dns.SHA1, dns.SHA256, dns.SHA384:
		return true
	}
	return false
}

// validateRRset checks rrset (from the servers of zone) against sigs.
func (q *query) validateRRset(rrset []dns.RR, sigs []*dns.RRSIG, zone string) vres {
	owner := rrset[0].Header().Name
	if q.exception(owner) {
		q.exceptRRset(owner, rrset[0].Header().Rrtype)
		return vres{sec: SecInsecure}
	}
	if len(sigs) == 0 {
		sec, ede, why := q.unsignedSecurity(owner, zone)
		return vres{sec: sec, ede: ede, why: why}
	}
	now := q.now()
	failure := bogus(dns.ExtendedErrorCodeDNSBogus, "no signature of "+owner+" "+dns.TypeToString[rrset[0].Header().Rrtype]+" verifies")
	tried := map[string]bool{}
	for _, first := range sigs {
		signer := canon(first.SignerName)
		if tried[signer] || !isSubOrEqual(signer, owner) {
			continue
		}
		tried[signer] = true
		ks := q.keys(signer)
		switch ks.sec {
		case SecInsecure:
			// signer encloses owner: owner is in an insecure zone.
			return vres{sec: SecInsecure, ede: ks.ede, why: ks.why}
		case SecSecure:
		default:
			// Try the other signers: a junk signature listed first must
			// not decide the outcome.
			failure = bogus(ks.ede, ks.why)
			continue
		}
		checks := 0
		for _, sig := range sigs {
			if canon(sig.SignerName) != signer || !supportedAlgorithm(sig.Algorithm) || int(sig.Labels) > labels(owner) {
				continue
			}
			if !sig.ValidityPeriod(now) {
				if int64(sig.Inception)-now.Unix() > 0 && sigWindowAhead(sig, now) {
					failure = bogus(dns.ExtendedErrorCodeSignatureNotYetValid, "signature of "+owner+" not yet valid")
				} else {
					failure = bogus(dns.ExtendedErrorCodeSignatureExpired, "signature of "+owner+" expired")
				}
				continue
			}
			for _, key := range ks.keys {
				if key.Algorithm != sig.Algorithm || key.KeyTag() != sig.KeyTag {
					continue
				}
				checks++
				q.st.sigBudget--
				if checks > maxSigTries || q.st.sigBudget < 0 {
					return bogus(dns.ExtendedErrorCodeDNSBogus, "too many signatures to check for "+owner)
				}
				if sig.Verify(key, rrset) != nil {
					continue
				}
				clampTTL(rrset, sig, now)
				res := vres{sec: SecSecure, signer: signer}
				l := labels(owner)
				if strings.HasPrefix(owner, "*.") {
					l--
				}
				if int(sig.Labels) < l {
					res.wildcard = suffixLabels(owner, int(sig.Labels))
				}
				return res
			}
		}
	}
	return failure
}

// sigWindowAhead reports whether the inception lies in the future (serial
// arithmetic aside, a signature is either not yet valid or expired).
func sigWindowAhead(sig *dns.RRSIG, now time.Time) bool {
	return int64(sig.Inception) > now.Unix() && int64(sig.Expiration) > now.Unix()
}

// clampTTL limits the TTLs of a validated RRset to the signature's original
// TTL and to its expiry (RFC 4035 §5.3.3).
func clampTTL(rrset []dns.RR, sig *dns.RRSIG, now time.Time) {
	left := int64(sig.Expiration) - now.Unix()
	for _, rr := range rrset {
		h := rr.Header()
		h.Ttl = min(h.Ttl, sig.OrigTtl)
		if left >= 0 && int64(h.Ttl) > left {
			h.Ttl = uint32(left) //nolint:gosec // bounded by h.Ttl
		}
	}
}

// unsignedSecurity decides what unsigned data at owner from zone's servers
// is: insecure when zone is, or when a delegation without DS lies between
// zone and owner (servers often host child zones too); else bogus.
func (q *query) unsignedSecurity(owner, zone string) (Security, uint16, string) {
	ks := q.keys(zone)
	if ks.sec != SecSecure {
		return ks.sec, ks.ede, ks.why
	}
	n := zone
	for range maxCutProbes {
		if equalName(n, owner) || !isBelow(n, owner) {
			break
		}
		n = childToward(n, owner)
		d := q.dsFor(n)
		switch {
		case d.sec == SecBogus:
			return SecBogus, d.ede, d.why
		case d.sec == SecInsecure:
			return SecInsecure, 0, ""
		case len(d.ds) == 0 && d.cut:
			return SecInsecure, 0, ""
		}
	}
	return SecBogus, dns.ExtendedErrorCodeRRSIGsMissing, "no signatures for " + owner + " in signed zone " + zone
}

// keys returns the validated DNSKEY set of zone, building the chain of
// trust from the root as needed.
func (q *query) keys(zone string) *keyState {
	zone = canon(zone)
	if q.exception(zone) {
		return &keyState{sec: SecInsecure}
	}
	now := q.now()
	if v, ok := q.r.cache.get(q.key(kKeys, zone), now); ok {
		return v.(*keyState)
	}
	if q.st.inKeys[zone] || q.depth > maxDepth {
		q.st.tooDeep = q.st.tooDeep || q.depth > maxDepth
		return &keyState{sec: SecBogus, ede: dns.ExtendedErrorCodeDNSKEYMissing, why: "chain of trust for " + zone + " loops"}
	}
	q.st.inKeys[zone] = true
	defer delete(q.st.inKeys, zone)
	sub := q.nested(true)
	var st *keyState
	var ttl time.Duration
	if zone == "." {
		st, ttl = sub.fetchKeys(".", q.s.anchors)
	} else {
		d := sub.dsFor(zone)
		switch {
		case d.sec == SecBogus:
			st, ttl = &keyState{sec: SecBogus, ede: d.ede, why: d.why}, failTTL
		case d.sec == SecInsecure || len(d.ds) == 0 && d.cut:
			st, ttl = &keyState{sec: SecInsecure}, d.ttl
		case len(d.ds) == 0:
			st, ttl = &keyState{sec: SecBogus, ede: dns.ExtendedErrorCodeDNSKEYMissing, why: zone + " signs data but is not a delegation"}, failTTL
		default:
			st, ttl = sub.fetchKeys(zone, d.ds)
		}
	}
	if st.sec == SecBogus {
		ttl = failTTL
	}
	if st.sec != SecBogus || !q.limited() {
		q.r.cache.set(q.key(kKeys, zone), st, ttl, now)
	}
	return st
}

// fetchKeys gets zone's DNSKEY set and checks it against ds: a key that
// matches a DS record must sign the set.
func (q *query) fetchKeys(zone string, ds []*dns.DS) (*keyState, time.Duration) {
	var usable []*dns.DS
	for _, d := range ds {
		if supportedDigest(d.DigestType) && supportedAlgorithm(d.Algorithm) {
			usable = append(usable, d)
		}
	}
	if len(usable) == 0 {
		return &keyState{sec: SecInsecure, ede: dns.ExtendedErrorCodeUnsupportedDNSKEYAlgorithm, why: zone + " uses only unsupported DNSSEC algorithms"}, ttlOf(dsRRs(ds)...)
	}
	res, err := q.iterate(zone, dns.TypeDNSKEY)
	if err != nil {
		return &keyState{sec: SecBogus, ede: dns.ExtendedErrorCodeDNSKEYMissing, why: "DNSKEY of " + zone + ": " + err.Error()}, failTTL
	}
	set := rrsetOf(res.msg.Answer, zone, dns.TypeDNSKEY)
	if len(set) == 0 {
		return &keyState{sec: SecBogus, ede: dns.ExtendedErrorCodeDNSKEYMissing, why: "no DNSKEY for " + zone}, failTTL
	}
	sigs := sigsFor(res.msg.Answer, zone, dns.TypeDNSKEY)
	now := q.now()
	failure := &keyState{sec: SecBogus, ede: dns.ExtendedErrorCodeDNSBogus, why: "no DNSKEY of " + zone + " matching its DS signs the key set"}
	checks := 0
	for _, d := range usable {
		for _, rr := range set {
			k := rr.(*dns.DNSKEY)
			if k.Algorithm != d.Algorithm || k.KeyTag() != d.KeyTag || k.Flags&dns.ZONE == 0 || k.Flags&dns.REVOKE != 0 || k.Protocol != 3 {
				continue
			}
			kd := k.ToDS(d.DigestType)
			if kd == nil || !strings.EqualFold(kd.Digest, d.Digest) {
				continue
			}
			for _, sig := range sigs {
				if sig.KeyTag != d.KeyTag || sig.Algorithm != k.Algorithm || canon(sig.SignerName) != zone {
					continue
				}
				if !sig.ValidityPeriod(now) {
					failure = &keyState{sec: SecBogus, ede: dns.ExtendedErrorCodeSignatureExpired, why: "DNSKEY signature of " + zone + " is outside its validity period"}
					continue
				}
				checks++
				q.st.sigBudget--
				if checks > maxSigTries || q.st.sigBudget < 0 {
					return &keyState{sec: SecBogus, ede: dns.ExtendedErrorCodeDNSBogus, why: "too many DNSKEY signatures to check for " + zone}, failTTL
				}
				if sig.Verify(k, set) == nil {
					clampTTL(set, sig, now)
					st := &keyState{sec: SecSecure}
					for _, rr := range set {
						if key := rr.(*dns.DNSKEY); key.Flags&dns.ZONE != 0 && key.Flags&dns.REVOKE == 0 && key.Protocol == 3 {
							st.keys = append(st.keys, key)
						}
					}
					return st, min(ttlOf(set...), ttlOf(dsRRs(ds)...))
				}
			}
		}
	}
	return failure, failTTL
}

func dsRRs(ds []*dns.DS) []dns.RR {
	out := make([]dns.RR, len(ds))
	for i, d := range ds {
		out[i] = d
	}
	return out
}

// dsResult is what the parent proves about name's DS RRset.
type dsResult struct {
	sec Security // secure: the proof below is signed; insecure: the parent is unsigned
	ds  []*dns.DS
	cut bool // name is (or, under NSEC3 opt-out, may be) a delegation
	ede uint16
	why string
	ttl time.Duration
}

// dsFor finds out whether name has a DS RRset: from a referral's evidence
// when one was kept, else by asking the parent.
func (q *query) dsFor(name string) *dsResult {
	name = canon(name)
	now := q.now()
	if v, ok := q.r.cache.get(q.key(kDS, name), now); ok {
		return v.(*dsResult)
	}
	guard := "ds|" + name
	if q.st.inKeys[guard] || q.depth > maxDepth {
		q.st.tooDeep = q.st.tooDeep || q.depth > maxDepth
		return &dsResult{sec: SecBogus, ede: dns.ExtendedErrorCodeDNSBogus, why: "DS lookup for " + name + " loops"}
	}
	q.st.inKeys[guard] = true
	defer delete(q.st.inKeys, guard)
	var d *dsResult
	if v, ok := q.r.cache.get(q.key(kRefD, name), now); ok {
		ev := v.(*referralDS)
		recs := make([]dns.RR, len(ev.records)) // shared via the cache; validation clamps TTLs
		for i, rr := range ev.records {
			recs[i] = dns.Copy(rr)
		}
		if r := q.dsFromResponse(name, dsAnswerFromReferral(&dns.Msg{Ns: recs}, ev.zone, name)); r.sec != SecBogus {
			d = r
		}
	}
	if d == nil {
		res, err := q.iterate(name, dns.TypeDS)
		if err != nil {
			d = &dsResult{sec: SecBogus, ede: dns.ExtendedErrorCodeNoReachableAuthority, why: "DS of " + name + ": " + err.Error(), ttl: failTTL}
		} else {
			d = q.dsFromResponse(name, res)
		}
	}
	ttl := d.ttl
	if d.sec == SecBogus {
		ttl = failTTL
	}
	if d.sec != SecBogus || !q.limited() {
		q.r.cache.set(q.key(kDS, name), d, ttl, now)
	}
	return d
}

// dsFromResponse validates the parent's answer to name DS.
func (q *query) dsFromResponse(name string, res *response) *dsResult {
	m := res.msg
	if set := rrsetOf(m.Answer, name, dns.TypeDS); len(set) > 0 {
		v := q.validateRRsetAbove(set, sigsFor(m.Answer, name, dns.TypeDS), res.zone, name)
		switch v.sec {
		case SecSecure:
			d := &dsResult{sec: SecSecure, cut: true, ttl: ttlOf(set...)}
			for _, rr := range set {
				d.ds = append(d.ds, rr.(*dns.DS))
			}
			return d
		case SecInsecure:
			return &dsResult{sec: SecInsecure, cut: true, ttl: ttlOf(set...)}
		}
		return &dsResult{sec: SecBogus, ede: v.ede, why: v.why}
	}
	sec, cut, ede, why := q.proveDenial(m, res.zone, name, dns.TypeDS, name)
	return &dsResult{sec: sec, cut: cut, ede: ede, why: why, ttl: ttlOf(m.Ns...)}
}

// validateRRsetAbove is validateRRset for data that must be signed by a
// zone strictly above name (DS and the parent's denial records), which
// keeps a child from vouching for itself.
func (q *query) validateRRsetAbove(rrset []dns.RR, sigs []*dns.RRSIG, zone, name string) vres {
	var keep []*dns.RRSIG
	for _, s := range sigs {
		if name == "." || isBelow(canon(s.SignerName), name) {
			keep = append(keep, s)
		}
	}
	if len(keep) == 0 && len(sigs) > 0 {
		return bogus(dns.ExtendedErrorCodeDNSBogus, "DS data for "+name+" signed by the child zone")
	}
	if zone == name && name != "." {
		zone = parentName(name)
	}
	if len(keep) == 0 {
		// Unsigned: fine only when the parent is unsigned. No probing
		// below it, which would ask for this very DS again.
		ks := q.keys(zone)
		if ks.sec == SecSecure {
			return bogus(dns.ExtendedErrorCodeRRSIGsMissing, "unsigned DS data for "+name+" in signed zone "+zone)
		}
		return vres{sec: ks.sec, ede: ks.ede, why: ks.why}
	}
	return q.validateRRset(rrset, keep, zone)
}

// validateNegative validates an NXDOMAIN or NODATA answer for name/qtype.
func (q *query) validateNegative(res *response, name string, qtype uint16) (Security, uint16, string) {
	if q.exception(name) {
		for _, rr := range res.msg.Ns {
			if sig, ok := rr.(*dns.RRSIG); ok {
				q.exceptRRset(sig.Hdr.Name, sig.TypeCovered)
			}
		}
		return SecInsecure, 0, ""
	}
	sec, _, ede, why := q.proveDenial(res.msg, res.zone, name, qtype, "")
	if sec == SecSecure {
		return SecSecure, 0, ""
	}
	return sec, ede, why
}

// proveDenial validates the denial records in m and checks that they prove
// name/qtype does not exist (NXDOMAIN) or has no data (NODATA). For DS it
// also reports whether name is a delegation. above, when set, requires
// the records to be signed by a zone strictly above it.
//
// Only the records of one zone count: the deepest signer that encloses
// name and lies at or below the answering servers' zone. Records signed by
// any other zone are ignored, so a spoofed answer cannot borrow another
// zone's signatures, NSEC3 chain or insecurity.
func (q *query) proveDenial(m *dns.Msg, zone, name string, qtype uint16, above string) (Security, bool, uint16, string) {
	nx := m.Rcode == dns.RcodeNameError
	if above != "" && zone == above && above != "." {
		zone = parentName(above)
	}
	signer := denialSigner(m.Ns, zone, name, above)
	if signer == "" {
		// Nothing signed by a zone that may speak for name: fine for an
		// unsigned zone only.
		var sec Security
		var ede uint16
		var why string
		if above != "" {
			// The parent's own answer about a DS: no probing below it.
			ks := q.keys(zone)
			sec, ede, why = ks.sec, ks.ede, ks.why
		} else {
			sec, ede, why = q.unsignedSecurity(name, zone)
		}
		if sec == SecSecure || sec == SecBogus && ede == dns.ExtendedErrorCodeRRSIGsMissing {
			return SecBogus, false, dns.ExtendedErrorCodeNSECMissing, "no NSEC or NSEC3 records deny " + name
		}
		return sec, false, ede, why
	}
	nsecs, nsec3s, _, v := q.denialRecords(m.Ns, signer, zone)
	switch v.sec {
	case SecSecure:
	case SecInsecure:
		// signer encloses name, so name is in an insecure zone.
		return SecInsecure, qtype == dns.TypeDS, v.ede, v.why
	default:
		return SecBogus, false, v.ede, v.why
	}
	missing := func() (Security, bool, uint16, string) {
		what := "no data"
		if nx {
			what = "name error"
		}
		return SecBogus, false, dns.ExtendedErrorCodeNSECMissing, "no valid proof of " + what + " for " + name + " " + dns.TypeToString[qtype]
	}
	if len(nsecs) > 0 {
		if nx {
			if nsecProveNX(name, nsecs) {
				return SecSecure, false, 0, ""
			}
			return missing()
		}
		ok, cut := nsecProveNoData(name, qtype, nsecs)
		if !ok && qtype == dns.TypeDS && nsecProveNX(name, nsecs) {
			ok, cut = true, false // a name that does not exist is no cut
		}
		if ok {
			return SecSecure, cut, 0, ""
		}
		return missing()
	}
	if len(nsec3s) == 0 {
		return missing()
	}
	set, insecure := newNSEC3Set(nsec3s, signer, &q.st.hashBudget)
	if insecure {
		return SecInsecure, qtype == dns.TypeDS, dns.ExtendedErrorCodeUnsupportedNSEC3IterValue, "NSEC3 parameters too expensive to check"
	}
	if set == nil {
		return missing()
	}
	if nx {
		ok, optOut := set.proveNX(name)
		switch {
		case !ok:
			return missing()
		case optOut:
			return SecInsecure, false, 0, ""
		}
		return SecSecure, false, 0, ""
	}
	ok, cut, optOut := set.proveNoData(name, qtype)
	if !ok && qtype == dns.TypeDS {
		if nxok, _ := set.proveNX(name); nxok {
			ok = true
		}
	}
	switch {
	case !ok:
		return missing()
	case optOut && qtype != dns.TypeDS:
		return SecInsecure, false, 0, ""
	}
	return SecSecure, cut, 0, ""
}

// denialSigner picks the zone whose denial records count for name: among
// the signers of the SOA, NSEC and NSEC3 records in ns, the deepest one
// that is at or below zone (the servers' zone) and encloses name; with
// above set, it must be strictly above that name (the parent of a DS).
func denialSigner(ns []dns.RR, zone, name, above string) string {
	best := ""
	for _, rr := range ns {
		sig, ok := rr.(*dns.RRSIG)
		if !ok {
			continue
		}
		switch sig.TypeCovered {
		case dns.TypeSOA, dns.TypeNSEC, dns.TypeNSEC3:
		default:
			continue
		}
		s := canon(sig.SignerName)
		if !isSubOrEqual(s, name) || !isSubOrEqual(zone, s) {
			continue
		}
		if above != "" && above != "." && !isBelow(s, above) {
			continue
		}
		if best == "" || labels(s) > labels(best) {
			best = s
		}
	}
	return best
}

// denialRecords validates the SOA, NSEC and NSEC3 RRsets in ns that belong
// to signer's zone (owned inside it, an NSEC3 owned directly under it) using
// only signer's signatures, and returns the NSEC and NSEC3 records with the
// signatures for DO clients. Other records are ignored.
func (q *query) denialRecords(ns []dns.RR, signer, zone string) ([]*dns.NSEC, []*dns.NSEC3, []dns.RR, vres) {
	var nsecs []*dns.NSEC
	var nsec3s []*dns.NSEC3
	var proof []dns.RR
	res := vres{sec: SecSecure}
	done := map[string]bool{}
	for _, rr := range ns {
		h := rr.Header()
		switch h.Rrtype {
		case dns.TypeSOA, dns.TypeNSEC, dns.TypeNSEC3:
		default:
			continue
		}
		k := h.Name + "|" + dns.TypeToString[h.Rrtype]
		if done[k] || !isSubOrEqual(signer, h.Name) {
			continue
		}
		done[k] = true
		if h.Rrtype == dns.TypeNSEC3 && !equalName(parentName(h.Name), signer) {
			continue
		}
		var sigs []*dns.RRSIG
		for _, s := range sigsFor(ns, h.Name, h.Rrtype) {
			if canon(s.SignerName) == signer {
				sigs = append(sigs, s)
			}
		}
		if len(sigs) == 0 {
			continue // not signed by this zone: not part of its proof
		}
		set := rrsetOf(ns, h.Name, h.Rrtype)
		v := q.validateRRset(set, sigs, zone)
		if v.sec != SecSecure {
			return nil, nil, nil, v
		}
		for _, x := range set {
			switch r := x.(type) {
			case *dns.NSEC:
				if isSubOrEqual(signer, r.NextDomain) {
					nsecs = append(nsecs, r)
				}
			case *dns.NSEC3:
				nsec3s = append(nsec3s, r)
			}
		}
		if h.Rrtype != dns.TypeSOA {
			proof = append(proof, set...)
			for _, s := range sigs {
				proof = append(proof, s)
			}
		}
	}
	return nsecs, nsec3s, proof, res
}

// proveWildcard checks that owner, answered from a wildcard at ce of
// signer's zone, does not exist itself, and returns the proof records.
func (q *query) proveWildcard(owner, ce, signer string, ns []dns.RR, zone string) ([]dns.RR, bool) {
	nsecs, nsec3s, proof, v := q.denialRecords(ns, signer, zone)
	if v.sec != SecSecure {
		return nil, false
	}
	for _, n := range nsecs {
		if nsecCovers(n, owner) && !delegatesAbove(n, owner) && equalName(nsecClosestEncloser(owner, n), ce) {
			return proof, true
		}
	}
	if len(nsec3s) > 0 {
		set, insecure := newNSEC3Set(nsec3s, signer, &q.st.hashBudget)
		if set != nil && !insecure && set.cover(childToward(ce, owner)) != nil {
			return proof, true
		}
	}
	return nil, false
}

// exception records the authorization deadline on the whole alias/proof walk.
// Each RRset/target is matched independently, so aliases cannot escape scope.
func (q *query) exception(name string) bool {
	until, ok := q.s.exceptions.Match(name, q.now())
	if ok && (q.st.exceptionUntil.IsZero() || until.Before(q.st.exceptionUntil)) {
		q.st.exceptionUntil = until
	}
	return ok
}

func (q *query) exceptRRset(name string, typ uint16) {
	if q.st.excepted == nil {
		q.st.excepted = make(map[RRset]struct{})
	}
	q.st.excepted[RRset{Name: canon(name), Type: typ}] = struct{}{}
}
