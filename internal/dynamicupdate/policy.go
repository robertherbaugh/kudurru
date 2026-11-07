package dynamicupdate

import (
	"strings"

	"github.com/miekg/dns"
)

type policy struct {
	grants       []Grant
	reservations map[string]*reservedOwner
	key          string
}
type reservedOwner struct {
	types   map[uint16]bool
	subtree bool
}

func currentPolicy(s State, p Principal, l Limits) (*policy, *ProtocolError) {
	if !s.UpdateEnabled || p.Key == "" || p.Version == "" {
		return nil, failure(dns.RcodeRefused, "verified current TSIG identity required")
	}
	if len(p.Key) > MaxNameBytes || len(p.Version) > MaxIdentityBytes {
		return nil, failure(dns.RcodeRefused, "TSIG identity capacity exceeded")
	}
	if _, err := PolicyAllocation(s.Credentials, s.Grants, s.Reservations, l.MaxPolicyBytes); err != nil {
		return nil, failure(dns.RcodeServerFailure, "policy capacity exceeded")
	}
	key, err := CanonicalName(p.Key)
	if err != nil {
		return nil, failure(dns.RcodeRefused, "invalid key name")
	}
	active := false
	seen := map[string]bool{}
	for _, c := range s.Credentials {
		name, err := CanonicalName(c.Key)
		if err != nil || c.Version == "" || seen[name] {
			return nil, failure(dns.RcodeRefused, "invalid committed credential references")
		}
		seen[name] = true
		if name == key && c.Version == p.Version {
			active = true
		}
	}
	if !active {
		return nil, failure(dns.RcodeRefused, "credential version is not current")
	}
	out := &policy{key: key, reservations: map[string]*reservedOwner{}}
	for _, g := range s.Grants {
		name, err := CanonicalName(g.Key)
		if err != nil {
			return nil, failure(dns.RcodeRefused, "invalid grant key")
		}
		pattern := g.Owner
		wild := strings.HasPrefix(pattern, "*.")
		if wild {
			pattern = strings.TrimPrefix(pattern, "*.")
		}
		owner, err := CanonicalName(pattern)
		if err != nil || strings.Contains(owner, "*") {
			return nil, failure(dns.RcodeRefused, "invalid owner grant")
		}
		if len(g.Types) > 65535 {
			return nil, failure(dns.RcodeRefused, "grant type capacity exceeded")
		}
		for _, typ := range g.Types {
			if !dataType(typ) && typ != dns.TypeANY {
				return nil, failure(dns.RcodeRefused, "invalid grant type")
			}
		}
		g.Key = name
		g.Owner = owner
		if wild {
			g.Owner = "*." + owner
		}
		out.grants = append(out.grants, g)
	}
	for _, r := range s.Reservations {
		name, err := CanonicalName(r.Name)
		if err != nil || (!dataType(r.Type) && r.Type != 0) || r.Source == "" {
			return nil, failure(dns.RcodeRefused, "invalid ownership reservation")
		}
		if out.reservations[name] == nil {
			out.reservations[name] = &reservedOwner{types: map[uint16]bool{}}
		}
		out.reservations[name].types[r.Type] = true
		out.reservations[name].subtree = out.reservations[name].subtree || r.Subtree
	}
	return out, nil
}

func (p *policy) allows(name string, typ uint16) bool {
	for _, g := range p.grants {
		if g.Key != p.key {
			continue
		}
		owner := g.Owner
		matches := name == owner
		if strings.HasPrefix(owner, "*.") {
			suffix := strings.TrimPrefix(owner, "*.")
			matches = name != suffix && dns.IsSubDomain(suffix, name)
		}
		if !matches {
			continue
		}
		if len(g.Types) == 0 {
			return true
		}
		for _, t := range g.Types {
			if t == dns.TypeANY || t == typ {
				return true
			}
		}
	}
	return false
}

func (p *policy) protects(m mutation) bool {
	typ := m.rr.Header().Rrtype
	if r := p.reservations[m.name]; r != nil {
		if r.types[0] || typ == dns.TypeANY || r.types[typ] {
			return true
		}
		// A managed CNAME reserves the entire owner against incompatible data.
		// An incoming CNAME likewise cannot occlude another managed type.
		if m.rr.Header().Class == dns.ClassINET && (r.types[dns.TypeCNAME] || typ == dns.TypeCNAME) {
			return true
		}
	}
	for name := m.name; name != "."; {
		i, end := dns.NextLabel(name, 0)
		if end {
			break
		}
		name = name[i:]
		if r := p.reservations[name]; r != nil && r.subtree {
			return true
		}
	}
	return false
}

func (p *policy) authorize(t *Transaction, z *recordIndex) *ProtocolError {
	for _, m := range t.mutations {
		if p.protects(m) {
			return failure(dns.RcodeRefused, "source-owned identity is protected")
		}
		typ := m.rr.Header().Rrtype
		if typ != dns.TypeANY {
			if !p.allows(m.name, typ) {
				return failure(dns.RcodeRefused, "owner/type not granted")
			}
			continue
		}
		// Delete-name is one operation, never a partial deletion of allowed types.
		rrsets := z.records[m.name]
		if len(rrsets) == 0 && !p.allows(m.name, dns.TypeANY) {
			return failure(dns.RcodeRefused, "delete-name not granted")
		}
		for rt := range rrsets {
			if !p.allows(m.name, rt) {
				return failure(dns.RcodeRefused, "delete-name includes unauthorized type")
			}
		}
	}
	return nil
}
