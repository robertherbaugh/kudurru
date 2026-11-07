// Package responsepolicy compiles immutable response transformations. It does
// not resolve names, access a store, or alter a raw cache entry.
package responsepolicy

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// Options is the operator configuration for forwarded responses.
type Options struct {
	DNS64Prefix         string
	DNS64ExcludeDomains []string
	DNS64ExcludeIPv4    []string
	FilterAAAA          bool
	BogusNXDOMAIN       []string
	NXDomainIPv4        []string
	NXDomainIPv6        []string
	TTL                 uint32
	Rebinding           bool
	PrivateDomains      []string
	BypassNetworks      []string
}

// Settings is a compiled immutable response plan. A nil plan disables changes.
type Settings struct {
	prefix         netip.Prefix
	excludeDomains []string
	excludeIPv4    []netip.Prefix
	filterAAAA     bool
	bogus          []netip.Prefix
	nxdomain       []netip.Addr
	ttl            uint32
	rebinding      bool
	privateDomains []string
	bypassNetworks []netip.Prefix
}

// Compile validates options before a plan can be published to queries.
func Compile(o Options) (*Settings, error) {
	s := &Settings{filterAAAA: o.FilterAAAA, ttl: o.TTL, rebinding: o.Rebinding}
	if s.ttl == 0 {
		s.ttl = 300
	}
	if s.ttl > 86400 {
		return nil, fmt.Errorf("synthetic TTL must not exceed 86400 seconds")
	}
	var err error
	if s.prefix, err = ParseDNS64Prefix(o.DNS64Prefix); err != nil {
		return nil, err
	}
	for _, field := range []struct {
		name string
		in   []string
		out  *[]string
	}{
		{"dns64.excludeDomains", o.DNS64ExcludeDomains, &s.excludeDomains},
		{"rebinding.privateDomains", o.PrivateDomains, &s.privateDomains},
	} {
		if len(field.in) > 256 {
			return nil, fmt.Errorf("%s: at most 256 domains", field.name)
		}
		for _, name := range field.in {
			if !validName(name) || name == "." || strings.Contains(name, "*") {
				return nil, fmt.Errorf("%s: invalid domain %q", field.name, name)
			}
			*field.out = append(*field.out, dns.CanonicalName(name))
		}
	}
	for _, field := range []struct {
		name string
		in   []string
		out  *[]netip.Prefix
		v4   bool
	}{
		{"dns64.excludeIPv4", o.DNS64ExcludeIPv4, &s.excludeIPv4, true},
		{"bogusNXDOMAIN", o.BogusNXDOMAIN, &s.bogus, false},
		{"rebinding.bypassNetworks", o.BypassNetworks, &s.bypassNetworks, false},
	} {
		if len(field.in) > 256 {
			return nil, fmt.Errorf("%s: at most 256 networks", field.name)
		}
		for _, text := range field.in {
			p, err := netip.ParsePrefix(text)
			if err != nil || p.Addr().Is4In6() || p != p.Masked() || field.v4 && !p.Addr().Is4() {
				return nil, fmt.Errorf("%s: invalid canonical network %q", field.name, text)
			}
			*field.out = append(*field.out, p)
		}
	}
	for _, field := range []struct {
		name string
		in   []string
		v4   bool
	}{{"nxdomainIPv4", o.NXDomainIPv4, true}, {"nxdomainIPv6", o.NXDomainIPv6, false}} {
		if len(field.in) > 16 {
			return nil, fmt.Errorf("%s: at most 16 addresses", field.name)
		}
		for _, text := range field.in {
			ip, err := netip.ParseAddr(text)
			if err != nil || ip.Zone() != "" || ip.Is4In6() || ip.Is4() != field.v4 {
				return nil, fmt.Errorf("%s: invalid address %q", field.name, text)
			}
			s.nxdomain = append(s.nxdomain, ip)
		}
	}
	return s, nil
}

// ParseDNS64Prefix accepts only the RFC 6052 layouts; empty disables DNS64.
func ParseDNS64Prefix(text string) (netip.Prefix, error) {
	if text == "" {
		return netip.Prefix{}, nil
	}
	p, err := netip.ParsePrefix(text)
	if err == nil && p.Addr().Is6() && !p.Addr().Is4In6() && p == p.Masked() {
		switch p.Bits() {
		case 32, 40, 48, 56, 64, 96:
			if p.Addr().IsGlobalUnicast() {
				return p, nil
			}
		}
	}
	return netip.Prefix{}, fmt.Errorf("dns64.prefix: expected a canonical IPv6 /32, /40, /48, /56, /64 or /96 prefix")
}

func validName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n\t ") {
		return false
	}
	_, ok := dns.IsDomainName(dns.Fqdn(name))
	return ok
}

func within(names []string, name string) bool {
	for _, suffix := range names {
		if dns.IsSubDomain(suffix, dns.CanonicalName(name)) {
			return true
		}
	}
	return false
}

func contains(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

// Overrides holds optional client-policy values. Its fields are private so a
// compiled policy cannot mutate the query's global plan.
type Overrides struct {
	prefix     *netip.Prefix
	filterAAAA *bool
}

// CompileOverrides compiles nullable fields; an explicit empty prefix disables
// inherited DNS64 and an explicit false disables inherited AAAA filtering.
func CompileOverrides(prefix *string, filterAAAA *bool) (*Overrides, error) {
	o := &Overrides{}
	if prefix != nil {
		p, err := ParseDNS64Prefix(*prefix)
		if err != nil {
			return nil, err
		}
		o.prefix = &p
	}
	if filterAAAA != nil {
		v := *filterAAAA
		o.filterAAAA = &v
	}
	return o, nil
}

// MergeOverrides chooses the first explicitly configured field, in priority order.
func MergeOverrides(ordered []*Overrides) *Overrides {
	o := &Overrides{}
	for _, v := range ordered {
		if v == nil {
			continue
		}
		if o.prefix == nil {
			o.prefix = v.prefix
		}
		if o.filterAAAA == nil {
			o.filterAAAA = v.filterAAAA
		}
	}
	return o
}

// With returns an immutable plan with the client's explicit policy overrides.
func (s *Settings) With(o *Overrides) *Settings {
	if o == nil || o.prefix == nil && o.filterAAAA == nil {
		return s
	}
	n := Settings{ttl: 300}
	if s != nil {
		n = *s
	}
	if o.prefix != nil {
		n.prefix = *o.prefix
	}
	if o.filterAAAA != nil {
		n.filterAAAA = *o.filterAAAA
	}
	return &n
}
