package zone

import (
	"fmt"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// BuildCommittedPrimary builds a primary from its complete committed data and
// frozen configuration. It never synthesizes apex data or imports desired
// endpoints. SOA fields, arbitrary RDATA and TTLs are copied; served RRset TTLs
// are normalized on private copies only. apexOverride records whether frozen
// ownership includes explicit apex data beyond generated SOA/NS. Signatures
// are data, never proof of AD.
func BuildCommittedPrimary(config *v1alpha1.DNSZone, records []dns.RR, serial uint32, expiry time.Time, apexOverride bool) (*Zone, error) {
	b, err := committedBuilder(config)
	if err != nil {
		return nil, err
	}
	z := b.z
	z.apexOverride = apexOverride
	if len(records) == 0 || len(records) > 1_000_000 {
		return nil, fmt.Errorf("committed record count exceeded")
	}
	seen := map[string]bool{}
	remaining := int64(64 << 20)
	for i, original := range records {
		rr, wire, err := copySecondaryRR(original) // wire validation helper, not secondary model construction
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		if int64(len(wire)) > remaining {
			return nil, fmt.Errorf("committed record bytes exceeded")
		}
		remaining -= int64(len(wire))
		h := rr.Header()
		key, err := canonicalKey(h.Name)
		if err != nil || !dns.IsFqdn(h.Name) || !isKeyPrefix(key, z.key) || h.Class != dns.ClassINET || secondaryPseudoType(h.Rrtype) {
			return nil, fmt.Errorf("record %d: invalid owner/class/type", i)
		}
		if err := dynamicupdate.ValidateData(rr); err != nil {
			return nil, err
		}
		id, err := dynamicupdate.Identity(rr)
		if err != nil || seen[id] {
			return nil, fmt.Errorf("record %d: duplicate/invalid RR", i)
		}
		seen[id] = true
		if soa, ok := rr.(*dns.SOA); ok {
			if key != z.key || z.soa != nil || soa.Serial != serial {
				return nil, fmt.Errorf("one apex SOA with committed serial required")
			}
			z.soa = soa
		}
		n := z.tree.search(key)
		if n == nil {
			n = b.insertName(key, nameFromKey(key))
		}
		set := n.get(h.Rrtype)
		if set == nil {
			n.sets = append(n.sets, RRset{Name: n.name, Type: h.Rrtype})
			set = &n.sets[len(n.sets)-1]
		}
		set.add(rr)
	}
	if z.soa == nil || (len(config.Spec.Nameservers) != 0 && z.apex.get(dns.TypeNS) == nil) {
		return nil, fmt.Errorf("committed primary requires apex SOA and configured NS")
	}
	var structural error
	z.index = make(map[string]*node, z.tree.count)
	z.tree.walk(func(n *node) bool {
		z.index[n.key] = n
		for _, typ := range []uint16{dns.TypeCNAME, dns.TypeDNAME} {
			if s := n.get(typ); s != nil && (len(s.RRs) != 1 || (typ == dns.TypeCNAME && n == z.apex)) {
				structural = fmt.Errorf("%s: invalid %s RRset", n.name, typeString(typ))
				return false
			}
		}
		if n.get(dns.TypeCNAME) != nil {
			for _, s := range n.sets {
				if s.Type != dns.TypeCNAME && s.Type != dns.TypeRRSIG && s.Type != dns.TypeNSEC {
					structural = fmt.Errorf("%s: CNAME coexists with data", n.name)
					return false
				}
			}
		}
		if d := b.dnameAbove(n.key); d != nil && !n.empty() {
			structural = fmt.Errorf("%s: data below DNAME", n.name)
			return false
		}
		sortSets(n.sets)
		for i := range n.sets {
			n.sets[i].normalise()
			z.records += len(n.sets[i].RRs)
		}
		if n != z.apex && (n.get(dns.TypeNS) != nil || n.get(dns.TypeDNAME) != nil) {
			z.cuts = true
		}
		return true
	})
	if structural != nil {
		return nil, structural
	}
	z.records--
	z.serial, z.nextExpiry = serial, expiry
	z.negSOA = dns.Copy(z.soa).(*dns.SOA)
	z.negSOA.Hdr.Ttl = min(z.soa.Hdr.Ttl, z.soa.Minttl)
	z.negNs = []dns.RR{z.negSOA}
	z.access.nsAddrs = append(z.access.nsAddrs, z.nameserverAddrs()...)
	z.transfer.nsAddrs = append(z.transfer.nsAddrs, z.nameserverAddrs()...)
	z.hash = contentHash(z)
	return z, nil
}

// BuildCommittedUnavailable preserves primary authority and frozen query ACLs
// with no data. Callers must check CommittedAvailableAt (or delegate AvailableAt).
func BuildCommittedUnavailable(config *v1alpha1.DNSZone) (*Zone, error) {
	b, err := committedBuilder(config)
	if err != nil {
		return nil, err
	}
	b.z.committedUnavailable = true
	b.z.index = map[string]*node{b.z.key: b.z.apex}
	return b.z, nil
}

func committedBuilder(config *v1alpha1.DNSZone) (*Builder, error) {
	if config == nil || config.Spec.Type != v1alpha1.ZoneTypePrimary || config.Spec.ZoneName == "" {
		return nil, fmt.Errorf("primary configuration required")
	}
	b, errs := NewBuilder(config.DeepCopy(), BuildOptions{})
	if len(errs) > 0 {
		return nil, errs.ToAggregate()
	}
	if len(b.PolicyErrors()) > 0 {
		return nil, b.PolicyErrors().ToAggregate()
	}
	b.z.committed = true
	return b, nil
}

// BuildCommittedFailureCoverage copies the frozen query ACL and primary
// authority boundary into a no-data model that fails closed at expiry.
func BuildCommittedFailureCoverage(committed *Zone) (*Zone, error) {
	if committed == nil || !committed.committed || committed.typ != v1alpha1.ZoneTypePrimary {
		return nil, fmt.Errorf("committed primary required")
	}
	z, err := BuildUnavailable(committed.name)
	if err != nil {
		return nil, err
	}
	z.typ = v1alpha1.ZoneTypePrimary
	z.committed, z.committedUnavailable = true, true
	z.access = committed.access
	return z, nil
}
