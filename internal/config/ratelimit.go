package config

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
)

// RateLimit configures the ingress per-prefix QPM limiter
// (docs/architecture.md §7.1 step 2): UDP queries only; TCP, DoT, DoH,
// DoQ and DoH3 are never limited.
type RateLimit struct {
	// Enabled turns the limiter on.
	Enabled bool `koanf:"enabled" json:"enabled"`
	// IPv4 and IPv6 map a prefix length ("32", "24"; "128", "64", "56") to
	// the queries per minute allowed per prefix of that length. Maps merge
	// with the defaults key by key, so a level is switched off with 0.
	IPv4 map[string]int `koanf:"ipv4" json:"ipv4"`
	IPv6 map[string]int `koanf:"ipv6" json:"ipv6"`
	// Slip is the percentage (0-100) of over-limit queries answered with an
	// empty TC=1 response so legitimate clients retry over TCP; the rest
	// are dropped.
	Slip int `koanf:"slip" json:"slip"`
	// MaxBuckets bounds the number of tracked prefixes (LRU).
	MaxBuckets int `koanf:"maxBuckets" json:"maxBuckets"`
	// Exempt lists CIDRs or addresses that are never limited; loopback
	// always is.
	Exempt []string `koanf:"exempt" json:"exempt"`
}

// validate checks the limiter settings.
func (r *RateLimit) validate() []error {
	var errs []error
	if r.Slip < 0 || r.Slip > 100 {
		errs = append(errs, fmt.Errorf("ratelimit.slip: %d is outside 0-100", r.Slip))
	}
	if r.MaxBuckets < 0 {
		errs = append(errs, fmt.Errorf("ratelimit.maxBuckets: %d is negative", r.MaxBuckets))
	}
	check := func(family string, m map[string]int, maxBits int) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			bits, err := strconv.Atoi(k)
			if err != nil || bits < 0 || bits > maxBits {
				errs = append(errs, fmt.Errorf("ratelimit.%s: prefix length %q is not a number in 0-%d", family, k, maxBits))
			}
			if m[k] < 0 {
				errs = append(errs, fmt.Errorf("ratelimit.%s[%s]: %d queries per minute is negative", family, k, m[k]))
			}
		}
	}
	check("ipv4", r.IPv4, 32)
	check("ipv6", r.IPv6, 128)
	for _, e := range r.Exempt {
		if _, err := netip.ParsePrefix(e); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(e); err != nil {
			errs = append(errs, fmt.Errorf("ratelimit.exempt: %q is neither a CIDR nor an address", e))
		}
	}
	return errs
}
