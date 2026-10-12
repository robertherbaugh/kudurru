package config

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

// The ratelimit keys are checked by their metadata alone (schema.go): the
// bounds of slip and maxBuckets, the prefix lengths and rates of the ipv4
// and ipv6 maps and the exempt CIDRs.
