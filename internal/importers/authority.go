package importers

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// NameserverAddresses normalizes serving IPs, including supervisor VIP CIDRs.
func NameserverAddresses(in []string) ([]string, error) {
	if len(in) > 16 {
		return nil, fmt.Errorf("nameserverAddresses: at most 16 addresses are allowed")
	}
	out := []string{}
	for _, value := range in {
		value = strings.TrimSpace(value)
		a, err := netip.ParseAddr(value)
		if prefix, e := netip.ParsePrefix(value); e == nil {
			a, err = prefix.Addr(), nil
		}
		a = a.Unmap()
		if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
			return nil, fmt.Errorf("nameserverAddresses: enter unicast IPv4 or IPv6 addresses")
		}
		value = a.String()
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out, nil
}

// Authority derives stable, in-zone names for serving addresses. An IP cannot
// be used literally as NS RDATA or SOA MNAME. Encoding the address also keeps
// the names stable when the user reorders or adds VIPs.
func Authority(zoneName string, names, addresses []string) ([]string, []v1alpha1.Endpoint, error) {
	ns, err := Nameservers(names)
	if err != nil {
		return nil, nil, err
	}
	ips, err := NameserverAddresses(addresses)
	if err != nil {
		return nil, nil, err
	}
	if len(ns) > 0 && len(ips) > 0 {
		return nil, nil, fmt.Errorf("choose nameservers or nameserverAddresses, not both")
	}
	var records []v1alpha1.Endpoint
	for _, ip := range ips {
		a := netip.MustParseAddr(ip)
		n := "ns-" + hex.EncodeToString(a.AsSlice()) + "." + strings.TrimSuffix(strings.ToLower(zoneName), ".")
		canonical, err := Nameservers([]string{n})
		if err != nil {
			return nil, nil, fmt.Errorf("cannot generate a nameserver name within zone %s: %w", zoneName, err)
		}
		typ := "AAAA"
		if a.Is4() {
			typ = "A"
		}
		ns = append(ns, canonical[0])
		records = append(records, v1alpha1.Endpoint{DNSName: canonical[0], RecordType: typ, Targets: []string{ip}, RecordTTL: 300})
	}
	return ns, records, nil
}

// Nameservers validates host names and normalizes their DNS spelling.
func Nameservers(in []string) ([]string, error) {
	if len(in) > 16 {
		return nil, fmt.Errorf("nameservers: at most 16 names are allowed")
	}
	out := []string{}
	for _, value := range in {
		n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if _, err := netip.ParseAddr(n); err == nil {
			return nil, fmt.Errorf("nameservers: use DNS names, not IP addresses")
		}
		if len(validation.IsDNS1123Subdomain(n)) != 0 {
			return nil, fmt.Errorf("nameservers: enter valid DNS host names")
		}
		n += "."
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// IsImportedZone recognizes the exact stable names used by API imports before
// migration journals existed. It is only a candidate filter; adoption is explicit.
func IsImportedZone(name, domain string) bool {
	for _, candidate := range []string{domain, strings.TrimSuffix(domain, ".")} {
		for _, source := range []string{"technitium", "pihole"} {
			if name == zoneName(source, candidate) {
				return true
			}
		}
	}
	return false
}
