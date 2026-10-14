package dnssec

import (
	"context"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type LookupFunc func(context.Context, string, uint16) (*dns.Msg, error)
type Result struct {
	Msg      *dns.Msg
	Security pipeline.Security
	EDE      uint16
	HasEDE   bool
}

type fault struct {
	reason string
	code   uint16
	state  pipeline.ValidationState
}

func (e *fault) Error() string               { return "dnssec: " + e.reason }
func bogus(reason string, code uint16) error { return &fault{reason, code, pipeline.ValidationBogus} }
func incomplete(reason string) error {
	return &fault{reason, dns.ExtendedErrorCodeDNSSECIndeterminate, pipeline.ValidationIndeterminate}
}

type rrkey struct {
	name string
	typ  uint16
}
type records struct {
	sets map[rrkey][]dns.RR
	sigs map[rrkey][]*dns.RRSIG
}

func collect(rrs []dns.RR) records {
	r := records{map[rrkey][]dns.RR{}, map[rrkey][]*dns.RRSIG{}}
	for _, rr := range rrs {
		h := rr.Header()
		if sig, ok := rr.(*dns.RRSIG); ok {
			k := rrkey{dns.CanonicalName(h.Name), sig.TypeCovered}
			r.sigs[k] = append(r.sigs[k], sig)
		} else if h.Rrtype != dns.TypeOPT {
			k := rrkey{dns.CanonicalName(h.Name), h.Rrtype}
			r.sets[k] = append(r.sets[k], rr)
		}
	}
	return r
}

type authentication struct {
	keys     []*dns.DNSKEY
	insecure bool
	until    time.Time
}
type validation struct {
	v                             *Validator
	ctx                           context.Context
	lookup                        LookupFunc
	now                           time.Time
	queries, wire, rrsets, hashes int
	busy                          map[string]bool
	keys                          map[string]authentication
	until                         time.Time
	insecure                      bool
	selected                      map[rrkey]*dns.RRSIG
}

// Validate authenticates complete answers, aliases, wildcard expansion and
// negative proofs. Inputs are copied. No upstream AD bit is trusted.
func (v *Validator) Validate(ctx context.Context, q dns.Question, answer *dns.Msg, lookup LookupFunc) (result Result, returned error) {
	defer func() {
		if returned != nil && result.Security.State == pipeline.ValidationUnknown {
			f, ok := returned.(*fault)
			if !ok {
				f = &fault{"lookup_failure", dns.ExtendedErrorCodeNetworkError, pipeline.ValidationIndeterminate}
			}
			result.Security.State, result.Security.Reason = f.state, f.reason
			result.EDE, result.HasEDE = f.code, true
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	s := &validation{v: v, ctx: ctx, lookup: lookup, now: v.now(), busy: map[string]bool{}, keys: map[string]authentication{}, selected: map[rrkey]*dns.RRSIG{}}
	r := Result{Security: pipeline.Security{ContextID: v.ActiveContextID(), ValidatedAt: s.now}}
	if answer == nil {
		return r, incomplete("missing_answer")
	}
	for _, part := range [][]dns.RR{answer.Answer, answer.Ns, answer.Extra} {
		for _, rr := range part {
			if nilRR(rr) {
				return r, bogus("nil_record", dns.ExtendedErrorCodeInvalidData)
			}
		}
	}
	r.Msg = answer.Copy()
	r.Msg.AuthenticatedData = false
	// RRSIG RRsets are not themselves signed. Explicit meta queries cannot
	// acquire AD, but may still return the requested records for diagnostics.
	if q.Qtype == dns.TypeRRSIG {
		if !answer.Response || answer.Truncated || len(answer.Question) != 1 || !strings.EqualFold(answer.Question[0].Name, q.Name) || answer.Question[0].Qtype != q.Qtype || answer.Question[0].Qclass != q.Qclass {
			return r, incomplete("invalid_answer")
		}
		if _, err := s.checkMessage(r.Msg); err != nil {
			return r, err
		}
		r.Security.Reason = "explicit_rrsig_query"
		return r, nil
	}
	err := s.validate(q, r.Msg)
	completed := v.now()
	if err == nil && (!s.until.IsZero() && !completed.Before(s.until) || completed.Before(s.now)) {
		err = bogus("validation_expired", dns.ExtendedErrorCodeSignatureExpired)
	}
	r.Security.State = pipeline.ValidationSecure
	if s.insecure {
		r.Security.State = pipeline.ValidationInsecure
		r.Security.Reason = "insecure_delegation_or_nta"
	}
	r.Security.ValidUntil = s.until
	if err != nil {
		f, ok := err.(*fault)
		if !ok {
			f = &fault{"lookup_failure", dns.ExtendedErrorCodeNetworkError, pipeline.ValidationIndeterminate}
		}
		r.Security.State, r.Security.Reason = f.state, f.reason
		r.EDE, r.HasEDE = f.code, true
		return r, err
	}
	return r, nil
}

func (s *validation) limit() error {
	if err := s.ctx.Err(); err != nil {
		return incomplete("deadline")
	}
	if s.queries > s.v.limits.Queries || s.wire > s.v.limits.WireBytes || s.rrsets > s.v.limits.RRsets || s.hashes > s.v.limits.HashWork || len(s.busy) > s.v.limits.Hops {
		return incomplete("work_limit")
	}
	return nil
}
func (s *validation) deadline(t time.Time) {
	if !t.IsZero() && (s.until.IsZero() || t.Before(s.until)) {
		s.until = t
	}
}
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

func (s *validation) fetch(name string, typ uint16) (*dns.Msg, error) {
	s.queries++
	if err := s.limit(); err != nil {
		return nil, err
	}
	if s.lookup == nil {
		return nil, incomplete("lookup_unavailable")
	}
	m, err := s.lookup(s.ctx, name, typ)
	if err != nil {
		return nil, err
	}
	if m == nil || !m.Response || m.Truncated || m.Opcode != dns.OpcodeQuery || len(m.Question) != 1 || !strings.EqualFold(m.Question[0].Name, name) || m.Question[0].Qtype != typ || m.Question[0].Qclass != dns.ClassINET || (m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError) {
		return nil, incomplete("invalid_proof_reply")
	}
	return s.checkMessage(m)
}
func (s *validation) checkMessage(m *dns.Msg) (*dns.Msg, error) {
	for _, part := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range part {
			if nilRR(rr) || (rr.Header().Rrtype != dns.TypeOPT && rr.Header().Class != dns.ClassINET) {
				return nil, bogus("invalid_record", dns.ExtendedErrorCodeInvalidData)
			}
		}
	}
	w, err := m.Pack()
	if err != nil {
		return nil, bogus("malformed_answer", dns.ExtendedErrorCodeInvalidData)
	}
	s.wire += len(w)
	s.rrsets += len(collect(append(append(append([]dns.RR(nil), m.Answer...), m.Ns...), m.Extra...)).sets)
	if err := s.limit(); err != nil {
		return nil, err
	}
	return m.Copy(), nil
}

func (s *validation) validate(q dns.Question, m *dns.Msg) error {
	if q.Qclass != dns.ClassINET || !m.Response || m.Truncated || m.Opcode != dns.OpcodeQuery || len(m.Question) != 1 || !strings.EqualFold(q.Name, m.Question[0].Name) || q.Qtype != m.Question[0].Qtype || q.Qclass != m.Question[0].Qclass || (m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError) {
		return incomplete("invalid_answer")
	}
	if _, err := s.checkMessage(m); err != nil {
		return err
	}
	a := collect(m.Answer)
	ns := collect(m.Ns)
	name := dns.CanonicalName(q.Name)
	used := map[rrkey]bool{}
	seen := map[string]bool{}
	for hops := 0; ; hops++ {
		if hops >= s.v.limits.Hops || seen[name] {
			return bogus("alias_loop", dns.ExtendedErrorCodeDNSBogus)
		}
		seen[name] = true
		k := rrkey{name, q.Qtype}
		if rs := a.sets[k]; len(rs) > 0 && q.Qtype != dns.TypeCNAME {
			if m.Rcode != dns.RcodeSuccess {
				return bogus("positive_nxdomain", dns.ExtendedErrorCodeInvalidData)
			}
			if err := s.answerSet(k, a, ns); err != nil {
				return err
			}
			used[k] = true
			break
		}
		ck := rrkey{name, dns.TypeCNAME}
		if rs := a.sets[ck]; len(rs) > 0 {
			if len(rs) != 1 {
				return bogus("multiple_cname", dns.ExtendedErrorCodeInvalidData)
			}
			// A DNAME's derived CNAME has no signature of its own.
			derived := false
			for dk, ds := range a.sets {
				if dk.typ == dns.TypeDNAME && dk.name != name && dns.IsSubDomain(dk.name, name) {
					if len(ds) != 1 {
						return bogus("multiple_dname", dns.ExtendedErrorCodeInvalidData)
					}
					target := strings.TrimSuffix(name, dk.name) + dns.CanonicalName(ds[0].(*dns.DNAME).Target)
					if !strings.EqualFold(rs[0].(*dns.CNAME).Target, target) {
						return bogus("invalid_dname_alias", dns.ExtendedErrorCodeDNSBogus)
					}
					if err := s.answerSet(dk, a, ns); err != nil {
						return err
					}
					used[dk] = true
					derived = true
					break
				}
			}
			if !derived {
				if err := s.answerSet(ck, a, ns); err != nil {
					return err
				}
			}
			used[ck] = true
			if q.Qtype == dns.TypeCNAME {
				break
			}
			name = dns.CanonicalName(rs[0].(*dns.CNAME).Target)
			continue
		}
		var soa *dns.SOA
		for key, rs := range ns.sets {
			if key.typ == dns.TypeSOA && dns.IsSubDomain(key.name, name) {
				if soa != nil || len(rs) != 1 {
					return bogus("invalid_negative_soa", dns.ExtendedErrorCodeInvalidData)
				}
				soa = rs[0].(*dns.SOA)
			}
		}
		if soa == nil {
			return bogus("missing_negative_soa", dns.ExtendedErrorCodeNSECMissing)
		}
		sk := rrkey{dns.CanonicalName(soa.Hdr.Name), dns.TypeSOA}
		insecure, err := s.validateSet(sk, ns)
		if err != nil {
			return err
		}
		if !insecure {
			if err := s.proofSets(ns, sk.name); err != nil {
				return err
			}
			if err := s.denial(ns, sk.name, name, q.Qtype, m.Rcode == dns.RcodeNameError, false); err != nil {
				return err
			}
		}
		break
	}
	for key := range a.sets {
		if !used[key] {
			return bogus("unrelated_answer_rrset", dns.ExtendedErrorCodeInvalidData)
		}
	}
	// AD covers the retained authority RRsets as well as the answer.
	for k := range ns.sets {
		if _, err := s.validateSet(k, ns); err != nil {
			return err
		}
	}
	// Unauthenticated glue/additional data must not acquire a Secure assertion.
	extra := m.Extra[:0]
	for _, rr := range m.Extra {
		if rr.Header().Rrtype == dns.TypeOPT {
			extra = append(extra, rr)
		}
	}
	m.Extra = extra
	return s.limit()
}

func (s *validation) answerSet(k rrkey, a, ns records) error {
	insecure, err := s.validateSet(k, a)
	if err != nil || insecure {
		return err
	}
	if sig := s.selected[k]; sig != nil {
		if int(sig.Labels) < dns.CountLabel(k.name) && !(strings.HasPrefix(k.name, "*.") && int(sig.Labels) == dns.CountLabel(k.name)-1) {
			if err := s.proofSets(ns, dns.CanonicalName(sig.SignerName)); err != nil {
				return err
			}
			labels := dns.SplitDomainName(k.name)
			ce := dns.Fqdn(strings.Join(labels[len(labels)-int(sig.Labels):], "."))
			return s.wildcard(ns, dns.CanonicalName(sig.SignerName), k.name, ce)
		}
	}
	return nil
}

func (s *validation) validateSet(k rrkey, r records) (bool, error) {
	if err := s.limit(); err != nil {
		return false, err
	}
	if until, ok := s.v.exception(k.name, s.now); ok {
		s.insecure = true
		s.deadline(until)
		return true, nil
	}
	rs := r.sets[k]
	sigs := r.sigs[k]
	if len(sigs) == 0 {
		auth, err := s.unsigned(k.name)
		if err != nil {
			return false, err
		}
		if !auth.insecure {
			return false, bogus("missing_rrsig", dns.ExtendedErrorCodeRRSIGsMissing)
		}
		s.insecure = true
		s.deadline(auth.until)
		return true, nil
	}
	if len(sigs) > s.v.limits.Signatures {
		return false, incomplete("signature_limit")
	}
	var last error = bogus("invalid_signature", dns.ExtendedErrorCodeDNSBogus)
	for _, sig := range sigs {
		zone := dns.CanonicalName(sig.SignerName)
		if !dns.IsSubDomain(zone, k.name) || k.typ != dns.TypeDS && !dns.IsSubDomain(s.v.anchorBoundary(k.name), zone) {
			continue
		}
		auth, err := s.authenticate(zone)
		if err != nil {
			last = err
			continue
		}
		if auth.insecure {
			s.insecure = true
			s.deadline(auth.until)
			return true, nil
		}
		until, err := s.verify(rs, []*dns.RRSIG{sig}, auth.keys)
		if err != nil {
			last = err
			continue
		}
		s.deadline(auth.until)
		s.deadline(until)
		s.selected[k] = sig
		return false, nil
	}
	return false, last
}

func (s *validation) unsigned(owner string) (authentication, error) {
	m, err := s.fetch(owner, dns.TypeSOA)
	if err != nil {
		return authentication{}, err
	}
	var name string
	for _, rr := range append(m.Answer, m.Ns...) {
		if soa, ok := rr.(*dns.SOA); ok && dns.IsSubDomain(soa.Hdr.Name, owner) {
			if name != "" && !strings.EqualFold(name, soa.Hdr.Name) {
				return authentication{}, bogus("ambiguous_zone", dns.ExtendedErrorCodeInvalidData)
			}
			name = dns.CanonicalName(soa.Hdr.Name)
		}
	}
	if name == "" {
		return authentication{}, bogus("zone_apex_missing", dns.ExtendedErrorCodeDNSKEYMissing)
	}
	if !dns.IsSubDomain(s.v.anchorBoundary(owner), name) {
		return authentication{}, bogus("zone_above_anchor", dns.ExtendedErrorCodeDNSBogus)
	}
	return s.authenticate(name)
}

func (s *validation) authenticate(zone string) (authentication, error) {
	if a, ok := s.keys[zone]; ok {
		return a, nil
	}
	if until, ok := s.v.exception(zone, s.now); ok {
		return authentication{insecure: true, until: until}, nil
	}
	if s.busy[zone] {
		return authentication{}, bogus("trust_chain_loop", dns.ExtendedErrorCodeDNSBogus)
	}
	s.busy[zone] = true
	defer delete(s.busy, zone)
	if err := s.limit(); err != nil {
		return authentication{}, err
	}
	anchors := s.v.anchors[zone]
	var ds []dns.RR
	var parent authentication
	if len(anchors) == 0 {
		if zone == "." {
			return authentication{}, bogus("no_anchor", dns.ExtendedErrorCodeDNSKEYMissing)
		}
		m, err := s.fetch(zone, dns.TypeDS)
		if err != nil {
			return authentication{}, err
		}
		r := collect(append(m.Answer, m.Ns...))
		dk := rrkey{zone, dns.TypeDS}
		if len(r.sets[dk]) > 0 {
			if m.Rcode != dns.RcodeSuccess {
				return authentication{}, bogus("positive_ds_nxdomain", dns.ExtendedErrorCodeInvalidData)
			}
			var err error
			parent, err = s.parentFor(zone, r.sigs[dk])
			if err != nil {
				return authentication{}, err
			}
			if parent.insecure {
				s.keys[zone] = parent
				return parent, nil
			}
			until, err := s.verify(r.sets[dk], r.sigs[dk], parent.keys)
			if err != nil {
				return authentication{}, err
			}
			parent.until = earlier(parent.until, until)
			parent.until = earlier(parent.until, s.ttlDeadline(r.sets[dk]))
			ds = r.sets[dk]
		} else {
			var pk rrkey
			for k := range r.sets {
				if k.typ == dns.TypeSOA && k.name != zone && dns.IsSubDomain(k.name, zone) {
					pk = k
					break
				}
			}
			if pk.name == "" {
				return authentication{}, bogus("missing_ds_denial", dns.ExtendedErrorCodeNSECMissing)
			}
			if !dns.IsSubDomain(s.v.anchorBoundary(zone), pk.name) {
				return authentication{}, bogus("parent_above_anchor", dns.ExtendedErrorCodeDNSBogus)
			}
			var err error
			parent, err = s.authenticate(pk.name)
			if err != nil {
				return authentication{}, err
			}
			if !parent.insecure {
				for k, rs := range r.sets {
					if k.typ != dns.TypeSOA && k.typ != dns.TypeNSEC && k.typ != dns.TypeNSEC3 {
						continue
					}
					until, err := s.verify(rs, r.sigs[k], parent.keys)
					if err != nil {
						return authentication{}, err
					}
					parent.until = earlier(parent.until, until)
					parent.until = earlier(parent.until, s.ttlDeadline(rs))
				}
				if err := s.denial(r, pk.name, zone, dns.TypeDS, false, true); err != nil {
					return authentication{}, err
				}
			}
			parent.insecure = true
			parent.keys = nil
			s.keys[zone] = parent
			return parent, nil
		}
	} else {
		ds = anchors
	}
	if len(anchors) == 0 {
		usable := false
		for _, rr := range ds {
			if d, ok := rr.(*dns.DS); ok && supported(d.Algorithm) && (d.DigestType == dns.SHA256 || d.DigestType == dns.SHA384) {
				usable = true
			}
		}
		if !usable {
			parent.insecure = true
			parent.keys = nil
			s.keys[zone] = parent
			return parent, nil
		}
	}
	m, err := s.fetch(zone, dns.TypeDNSKEY)
	if err != nil {
		return authentication{}, err
	}
	r := collect(m.Answer)
	k := rrkey{zone, dns.TypeDNSKEY}
	var keys, matched []*dns.DNSKEY
	for _, rr := range r.sets[k] {
		key, ok := rr.(*dns.DNSKEY)
		if !ok || key.Protocol != 3 || key.Flags&dns.ZONE == 0 || key.Flags&dns.REVOKE != 0 || !supported(key.Algorithm) || len(key.PublicKey) > 4096 || !validPublicKey(key) {
			continue
		}
		keys = append(keys, key)
		for _, d := range ds {
			switch anchor := d.(type) {
			case *dns.DS:
				got := key.ToDS(anchor.DigestType)
				if got != nil && got.KeyTag == anchor.KeyTag && got.Algorithm == anchor.Algorithm && strings.EqualFold(got.Digest, anchor.Digest) {
					matched = append(matched, key)
				}
			case *dns.DNSKEY:
				if key.Flags == anchor.Flags && key.Protocol == anchor.Protocol && key.Algorithm == anchor.Algorithm && key.PublicKey == anchor.PublicKey {
					matched = append(matched, key)
				}
			}
		}
	}
	if len(keys) > s.v.limits.Signatures || len(ds) > s.v.limits.Signatures {
		return authentication{}, incomplete("key_limit")
	}
	if len(matched) == 0 {
		return authentication{}, bogus("ds_key_mismatch", dns.ExtendedErrorCodeDNSKEYMissing)
	}
	until, err := s.verify(r.sets[k], r.sigs[k], matched)
	if err != nil {
		return authentication{}, err
	}
	a := authentication{keys: keys, until: earlier(earlier(parent.until, until), s.ttlDeadline(r.sets[k]))}
	s.keys[zone] = a
	return a, nil
}

func (s *validation) parentFor(child string, sigs []*dns.RRSIG) (authentication, error) {
	for _, sig := range sigs {
		p := dns.CanonicalName(sig.SignerName)
		if p != child && dns.IsSubDomain(p, child) && dns.IsSubDomain(s.v.anchorBoundary(child), p) {
			return s.authenticate(p)
		}
	}
	return authentication{}, bogus("missing_parent_signature", dns.ExtendedErrorCodeRRSIGsMissing)
}

func (s *validation) verify(rs []dns.RR, sigs []*dns.RRSIG, keys []*dns.DNSKEY) (time.Time, error) {
	if len(rs) == 0 || len(sigs) == 0 {
		return time.Time{}, bogus("missing_rrsig", dns.ExtendedErrorCodeRRSIGsMissing)
	}
	if len(sigs) > s.v.limits.Signatures || len(keys) > s.v.limits.Signatures {
		return time.Time{}, incomplete("signature_limit")
	}
	last := bogus("invalid_signature", dns.ExtendedErrorCodeDNSBogus)
	for _, sig := range sigs {
		if !supported(sig.Algorithm) {
			continue
		}
		// RFC serial arithmetic maps the 32-bit expiration into this epoch.
		remaining := int64(int32(sig.Expiration - uint32(s.now.Unix())))
		if remaining <= 0 || !sig.ValidityPeriod(s.now) {
			code := uint16(dns.ExtendedErrorCodeSignatureExpired)
			if int32(sig.Inception-uint32(s.now.Unix())) > 0 {
				code = dns.ExtendedErrorCodeSignatureNotYetValid
			}
			last = bogus("signature_time", code)
			continue
		}
		for _, key := range keys {
			if sig.Verify(key, rs) == nil {
				deadline := s.now.Truncate(time.Second).Add(time.Duration(remaining) * time.Second)
				lifetime := uint32(deadline.Sub(s.now) / time.Second)
				for _, rr := range rs {
					ttl := min(rr.Header().Ttl, sig.OrigTtl, sig.Hdr.Ttl, lifetime)
					rr.Header().Ttl = ttl
				}
				sig.Hdr.Ttl = min(sig.Hdr.Ttl, sig.OrigTtl, lifetime)
				return deadline, nil
			}
		}
	}
	return time.Time{}, last
}

func (s *validation) proofSets(r records, zone string) error {
	found := false
	for k := range r.sets {
		if k.typ == dns.TypeNSEC || k.typ == dns.TypeNSEC3 {
			if !dns.IsSubDomain(zone, k.name) {
				return bogus("proof_outside_zone", dns.ExtendedErrorCodeDNSBogus)
			}
			for _, sig := range r.sigs[k] {
				if !strings.EqualFold(sig.SignerName, zone) {
					return bogus("proof_wrong_signer", dns.ExtendedErrorCodeDNSBogus)
				}
			}
			insecure, err := s.validateSet(k, r)
			if err != nil {
				return err
			}
			if insecure {
				return bogus("unauthenticated_denial", dns.ExtendedErrorCodeNSECMissing)
			}
			found = true
			s.deadline(s.ttlDeadline(r.sets[k]))
		}
	}
	if !found {
		return bogus("missing_denial", dns.ExtendedErrorCodeNSECMissing)
	}
	return nil
}

func (s *validation) ttlDeadline(rs []dns.RR) time.Time {
	var until time.Time
	for _, rr := range rs {
		until = earlier(until, s.now.Add(time.Duration(rr.Header().Ttl)*time.Second))
	}
	return until
}
