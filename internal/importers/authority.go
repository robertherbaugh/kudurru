package importers

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Nameservers validates host names and normalizes their DNS spelling.
func Nameservers(in []string) ([]string, error) {
	if len(in) > 16 {
		return nil, fmt.Errorf("nameservers: at most 16 names are allowed")
	}
	var out []string
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
