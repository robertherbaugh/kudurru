package recursor

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// rootHints are the root servers from IANA's named.root (last changed for
// b.root-servers.net in November 2023). Priming replaces them at run time.
var rootHints = []struct {
	name string
	v4   string
	v6   string
}{
	{"a.root-servers.net.", "198.41.0.4", "2001:503:ba3e::2:30"},
	{"b.root-servers.net.", "170.247.170.2", "2801:1b8:10::b"},
	{"c.root-servers.net.", "192.33.4.12", "2001:500:2::c"},
	{"d.root-servers.net.", "199.7.91.13", "2001:500:2d::d"},
	{"e.root-servers.net.", "192.203.230.10", "2001:500:a8::e"},
	{"f.root-servers.net.", "192.5.5.241", "2001:500:2f::f"},
	{"g.root-servers.net.", "192.112.36.4", "2001:500:12::d0d"},
	{"h.root-servers.net.", "198.97.190.53", "2001:500:1::53"},
	{"i.root-servers.net.", "192.36.148.17", "2001:7fe::53"},
	{"j.root-servers.net.", "192.58.128.30", "2001:503:c27::2:30"},
	{"k.root-servers.net.", "193.0.14.129", "2001:7fd::1"},
	{"l.root-servers.net.", "199.7.83.42", "2001:500:9f::42"},
	{"m.root-servers.net.", "202.12.27.33", "2001:dc3::35"},
}

// DefaultTrustAnchors are the root zone's key-signing keys as DS records
// from IANA's root-anchors.xml: KSK-2017 (20326) and KSK-2024 (38696),
// which signs the root DNSKEY set from 11 October 2026.
var DefaultTrustAnchors = []string{
	". IN DS 20326 8 2 E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D",
	". IN DS 38696 8 2 683D2D0ACB8C9B712A1948B27F741219298D0A450D612C483AF444A4C0FB2B16",
}

// ParseTrustAnchors parses DS records in presentation format; an owner
// name is optional and defaults to the root ("20326 8 2 E06D...").
func ParseTrustAnchors(in []string) ([]*dns.DS, error) {
	out := make([]*dns.DS, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		text := s
		if f := strings.Fields(s); len(f) > 0 && !strings.HasSuffix(f[0], ".") {
			text = ". IN DS " + s
		}
		rr, err := dns.NewRR(text)
		if err != nil {
			return nil, fmt.Errorf("recursor: trust anchor %q: %w", s, err)
		}
		ds, ok := rr.(*dns.DS)
		if !ok {
			return nil, fmt.Errorf("recursor: trust anchor %q is not a DS record", s)
		}
		if ds.Hdr.Name != "." {
			return nil, fmt.Errorf("recursor: trust anchor %q: only root trust anchors are supported", s)
		}
		out = append(out, ds)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("recursor: no trust anchors")
	}
	return out, nil
}

// ParseRootHints parses root server addresses: IPs, optionally with a
// port. Empty means the built-in IANA hints.
func ParseRootHints(in []string) ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if ap, err := netip.ParseAddrPort(s); err == nil {
			out = append(out, ap)
			continue
		}
		a, err := netip.ParseAddr(strings.Trim(s, "[]"))
		if err != nil {
			return nil, fmt.Errorf("recursor: root hint %q is not an IP address", s)
		}
		out = append(out, netip.AddrPortFrom(a, 53))
	}
	return out, nil
}

// hintCut is the root delegation from the hints. With custom hints the
// servers are named after their address.
func hintCut(custom []netip.AddrPort) *zoneCut {
	c := &zoneCut{name: ".", glue: map[string][]netip.AddrPort{}, hints: true}
	if len(custom) == 0 {
		for _, h := range rootHints {
			c.ns = append(c.ns, h.name)
			c.glue[h.name] = []netip.AddrPort{
				netip.AddrPortFrom(netip.MustParseAddr(h.v4), 53),
				netip.AddrPortFrom(netip.MustParseAddr(h.v6), 53),
			}
		}
		return c
	}
	for i, ap := range custom {
		name := fmt.Sprintf("hint%d.root.invalid.", i)
		c.ns = append(c.ns, name)
		c.glue[name] = []netip.AddrPort{ap}
	}
	return c
}

// haveIPv6Route reports whether the host has a route to the IPv6 root
// servers; connecting a UDP socket sends nothing.
func haveIPv6Route() bool {
	c, err := net.Dial("udp6", "[2001:503:ba3e::2:30]:53")
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
