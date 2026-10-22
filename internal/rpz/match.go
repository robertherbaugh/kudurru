package rpz

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

func canonicalName(name string) (string, error) {
	if name == "" || strings.TrimSpace(name) != name {
		return "", fmt.Errorf("rpz: empty or whitespace domain name")
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return "", fmt.Errorf("rpz: invalid domain name %q", name)
	}
	// Normalize equivalent escaped presentation names without penalizing the
	// ordinary ASCII query path with a DNS pack/unpack allocation.
	if strings.ContainsRune(name, '\\') {
		var buf [256]byte
		end, err := dns.PackDomainName(dns.Fqdn(name), buf[:], 0, nil, false)
		if err != nil {
			return "", fmt.Errorf("rpz: invalid escaped domain: %w", err)
		}
		if end > 255 {
			return "", fmt.Errorf("rpz: escaped domain exceeds wire length limit")
		}
		decoded, _, err := dns.UnpackDomainName(buf[:], 0)
		if err != nil {
			return "", fmt.Errorf("rpz: invalid escaped domain: %w", err)
		}
		name = decoded
	} else if len(dns.Fqdn(name)) > 254 {
		return "", fmt.Errorf("rpz: domain name exceeds wire length limit")
	}
	return dns.CanonicalName(name), nil
}

func (z *Zone) match(ctx Context) (*rule, string) {
	client := ctx.ClientIP.Unmap()
	if r := prefixMatch(z.clients, client); r != nil {
		return r, client.String()
	}
	if r, matched := z.nameMatch(ctx.Name); r != nil {
		return r, matched
	}
	for _, name := range ctx.Names {
		if r, matched := z.nameMatch(name); r != nil {
			return r, matched
		}
	}
	var best *rule
	var matched netip.Addr
	for _, addr := range ctx.Addresses {
		addr = addr.Unmap()
		if r := prefixMatch(z.addresses, addr); r != nil &&
			(best == nil || r.prefix.Bits() > best.prefix.Bits() ||
				r.prefix.Bits() == best.prefix.Bits() && addr.Compare(matched) < 0) {
			best, matched = r, addr
		}
	}
	if best != nil {
		return best, matched.String()
	}
	return nil, ""
}

func prefixMatch(index map[netip.Prefix]*rule, addr netip.Addr) *rule {
	if !addr.IsValid() || addr.Zone() != "" || len(index) == 0 {
		return nil
	}
	for bits := addr.BitLen(); bits > 0; bits-- {
		if r := index[netip.PrefixFrom(addr, bits).Masked()]; r != nil {
			return r
		}
	}
	return nil
}

func (z *Zone) nameMatch(name string) (*rule, string) {
	name, err := canonicalName(name)
	if err != nil {
		return nil, ""
	}
	if r := z.qnames[name]; r != nil {
		return r, name
	}
	if z.nodes[name] {
		return nil, ""
	}
	parent := name
	for parent != "." {
		parent = parentName(parent)
		if z.nodes[parent] {
			return z.qnames["*."+parent], name
		}
	}
	return nil, ""
}

func parentName(name string) string {
	// NextLabel respects escaped dots. Its end result has no parent label.
	i, end := dns.NextLabel(name, 0)
	if end || i >= len(name) {
		return "."
	}
	return name[i:]
}
