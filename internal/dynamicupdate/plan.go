package dynamicupdate

import (
	"sort"
	"time"

	"github.com/miekg/dns"
)

type recordIndex struct {
	records map[string]map[uint16]map[string]dns.RR
	count   int
	bytes   int64
	limits  Limits
}

func (z *recordIndex) rrset(name string, typ uint16) map[string]dns.RR { return z.records[name][typ] }
func (z *recordIndex) remove(name string, typ uint16, key string) {
	set := z.rrset(name, typ)
	if rr, ok := set[key]; ok {
		z.count--
		z.bytes -= int64(dns.Len(rr))
		delete(set, key)
	}
	if len(set) == 0 {
		delete(z.records[name], typ)
	}
	if len(z.records[name]) == 0 {
		delete(z.records, name)
	}
}
func (z *recordIndex) put(name string, typ uint16, key string, rr dns.RR) *ProtocolError {
	old := z.rrset(name, typ)[key]
	count, size := z.count+1, z.bytes+int64(dns.Len(rr))
	if old != nil {
		count--
		size -= int64(dns.Len(old))
	}
	if count > z.limits.MaxRecords || size > z.limits.MaxBytes {
		return failure(dns.RcodeServerFailure, "candidate zone capacity exceeded")
	}
	if z.records[name] == nil {
		z.records[name] = map[uint16]map[string]dns.RR{}
	}
	if z.records[name][typ] == nil {
		z.records[name][typ] = map[string]dns.RR{}
	}
	z.records[name][typ][key] = rr
	z.count = count
	z.bytes = size
	return nil
}

func load(s State, l Limits) (*recordIndex, uint32, *ProtocolError) {
	if len(s.Records) > l.MaxRecords {
		return nil, 0, failure(dns.RcodeServerFailure, "committed zone capacity exceeded")
	}
	z := &recordIndex{records: map[string]map[uint16]map[string]dns.RR{}, limits: l}
	var serial uint32
	soaCount := 0
	for _, rr := range s.Records {
		if err := ValidateData(rr); err != nil {
			return nil, 0, failure(dns.RcodeServerFailure, err.Error())
		}
		key, err := Identity(rr)
		if err != nil {
			return nil, 0, failure(dns.RcodeServerFailure, "invalid committed record")
		}
		h := rr.Header()
		name, err := CanonicalName(h.Name)
		if err != nil || h.Class != dns.ClassINET || !dataType(h.Rrtype) || !dns.IsSubDomain(s.Apex, name) {
			return nil, 0, failure(dns.RcodeServerFailure, "invalid committed owner/class/type")
		}
		if h.Rrtype == dns.TypeSOA {
			soa, ok := rr.(*dns.SOA)
			if !ok || name != s.Apex {
				return nil, 0, failure(dns.RcodeServerFailure, "invalid committed SOA")
			}
			serial = soa.Serial
			soaCount++
		}
		if z.rrset(name, h.Rrtype)[key] != nil {
			return nil, 0, failure(dns.RcodeServerFailure, "duplicate committed record")
		}
		if e := z.put(name, h.Rrtype, key, Copy(rr)); e != nil {
			return nil, 0, e
		}
	}
	if soaCount != 1 || len(z.rrset(s.Apex, dns.TypeNS)) == 0 {
		return nil, 0, failure(dns.RcodeServerFailure, "committed zone requires apex SOA and NS")
	}
	return z, serial, nil
}

func prerequisites(t *Transaction, z *recordIndex) *ProtocolError {
	for _, p := range t.prerequisites {
		set := z.rrset(p.name, p.typ)
		exists := len(set) > 0
		if p.typ == dns.TypeANY {
			exists = len(z.records[p.name]) > 0
		}
		switch p.class {
		case dns.ClassANY:
			if !exists {
				if p.typ == dns.TypeANY {
					return failure(dns.RcodeNameError, "name not in use")
				}
				return failure(dns.RcodeNXRrset, "RRset not in use")
			}
		case dns.ClassNONE:
			if exists {
				if p.typ == dns.TypeANY {
					return failure(dns.RcodeYXDomain, "name in use")
				}
				return failure(dns.RcodeYXRrset, "RRset in use")
			}
		case dns.ClassINET:
			values := map[string]bool{}
			for _, key := range p.values {
				values[key] = true
			}
			if len(values) != len(set) {
				return failure(dns.RcodeNXRrset, "RRset values differ")
			}
			for key := range values {
				if set[key] == nil {
					return failure(dns.RcodeNXRrset, "RRset values differ")
				}
			}
		}
	}
	return nil
}

// RFC1982Greater rejects equal, backward and half-range ambiguous serials.
func RFC1982Greater(next, old uint32) bool { d := next - old; return d != 0 && d < 1<<31 }

// AutoSerial follows the primary max(previous+1, Unix time) rule when that
// value advances under RFC 1982. Clock jumps cannot create backward or
// ambiguous serials; automatic wrap skips zero (RFC 2136 section 7.11).
func AutoSerial(old uint32, now time.Time) uint32 {
	next := old + 1
	u := now.Unix()
	if u > 0 {
		clock := uint32(uint64(u) % (1 << 32))
		if clock > next && RFC1982Greater(clock, old) {
			next = clock
		}
	}
	if next == 0 {
		next = 1
	}
	return next
}

func (z *recordIndex) apply(t *Transaction) *ProtocolError {
	for _, m := range t.mutations {
		h := m.rr.Header()
		typ := h.Rrtype
		set := z.rrset(m.name, typ)
		switch h.Class {
		case dns.ClassANY:
			if typ == dns.TypeANY {
				for rt, set := range z.records[m.name] {
					if m.name == t.apex && (rt == dns.TypeSOA || rt == dns.TypeNS) {
						continue
					}
					for key := range set {
						z.remove(m.name, rt, key)
					}
				}
			} else if m.name != t.apex || (typ != dns.TypeSOA && typ != dns.TypeNS) {
				for key := range set {
					z.remove(m.name, typ, key)
				}
			}
		case dns.ClassNONE:
			if m.name == t.apex && (typ == dns.TypeSOA || (typ == dns.TypeNS && len(set) == 1)) {
				continue
			}
			z.remove(m.name, typ, m.key)
		case dns.ClassINET:
			if typ == dns.TypeCNAME {
				conflict := false
				for rt := range z.records[m.name] {
					if rt != dns.TypeCNAME && rt != dns.TypeRRSIG && rt != dns.TypeNSEC {
						conflict = true
						break
					}
				}
				if conflict {
					continue
				}
			} else if len(z.rrset(m.name, dns.TypeCNAME)) > 0 && typ != dns.TypeRRSIG && typ != dns.TypeNSEC {
				continue
			}
			if typ == dns.TypeSOA {
				soa, ok := m.rr.(*dns.SOA)
				if !ok {
					return failure(dns.RcodeFormatError, "invalid SOA data")
				}
				if len(set) == 0 {
					continue
				}
				var old uint32
				for _, rr := range set {
					old = rr.(*dns.SOA).Serial
				}
				if !RFC1982Greater(soa.Serial, old) {
					continue
				}
			}
			// WKS (type 11) is opaque in miekg/dns. Its first five RDATA
			// octets are the IPv4 address and protocol replacement tuple.
			for key := range set {
				if typ == dns.TypeSOA || typ == dns.TypeCNAME || (typ == 11 && wksTuple(key) != "" && wksTuple(key) == wksTuple(m.key)) {
					z.remove(m.name, typ, key)
				}
			}
			rr := Copy(m.rr)
			rr.Header().Class = dns.ClassINET
			if e := z.put(m.name, typ, m.key, rr); e != nil {
				return e
			}
		}
	}
	return nil
}

func wksTuple(key string) string {
	n, err := rdataLength([]byte(key))
	if err != nil || n < 5 {
		return ""
	}
	return key[len(key)-n : len(key)-n+5]
}

func (z *recordIndex) sorted() []dns.RR {
	keys := make([]string, 0, z.count)
	byKey := make(map[string]dns.RR, z.count)
	for _, types := range z.records {
		for _, set := range types {
			for key, rr := range set {
				keys = append(keys, key)
				byKey[key] = rr
			}
		}
	}
	sort.Strings(keys)
	out := make([]dns.RR, 0, len(keys))
	for _, key := range keys {
		out = append(out, Copy(byKey[key]))
	}
	return out
}

// Plan evaluates current credentials, whole-RRset prerequisites, ownership
// and ordered mutations against one committed revision. It never performs I/O.
func Plan(s State, t *Transaction, principal Principal, now time.Time, limits Limits) (*Result, *ProtocolError) {
	l := limits.Defaults()
	apex, err := CanonicalName(s.Apex)
	if err != nil || t == nil || t.apex != apex {
		return nil, failure(dns.RcodeNotAuth, "exact hosted primary zone required")
	}
	s.Apex = apex
	p, e := currentPolicy(s, principal, l)
	if e != nil {
		return nil, e
	}
	z, oldSerial, e := load(s, l)
	if e != nil {
		return nil, e
	}
	if e = prerequisites(t, z); e != nil {
		return nil, e
	}
	if e = p.authorize(t, z); e != nil {
		return nil, e
	}
	before := map[string]uint32{}
	for _, types := range z.records {
		for _, set := range types {
			for key, rr := range set {
				before[key] = rr.Header().Ttl
			}
		}
	}
	if e = z.apply(t); e != nil {
		return nil, e
	}
	changed := len(before) != z.count
	for _, types := range z.records {
		for _, set := range types {
			for key, rr := range set {
				ttl, ok := before[key]
				if !ok || ttl != rr.Header().Ttl {
					changed = true
				}
			}
		}
	}
	serial := oldSerial
	if changed {
		for key, rr := range z.rrset(apex, dns.TypeSOA) {
			soa := rr.(*dns.SOA)
			serial = soa.Serial
			if serial == oldSerial {
				serial = AutoSerial(oldSerial, now)
				z.remove(apex, dns.TypeSOA, key)
				soa.Serial = serial
				nextKey, err := Identity(soa)
				if err != nil {
					return nil, failure(dns.RcodeServerFailure, err.Error())
				}
				if e := z.put(apex, dns.TypeSOA, nextKey, soa); e != nil {
					return nil, e
				}
			}
		}
	}
	data := z.sorted()
	if !changed {
		// Keep the prior raw bytes on a canonical no-op (e.g. case-only add).
		data = make([]dns.RR, len(s.Records))
		for i, rr := range s.Records {
			data[i] = Copy(rr)
		}
	}
	return &Result{Records: data, Serial: serial, Changed: changed, AuthEpoch: s.AuthEpoch, CredentialVersion: principal.Version}, nil
}
