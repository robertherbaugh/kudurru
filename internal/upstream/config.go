package upstream

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/recursor"
)

// DefaultGroup is the group used when neither a Request.UpstreamGroup hint
// nor a conditional suffix matched. Config.Validate requires it.
const DefaultGroup = "default"

// Strategy selects which servers of a group are asked, and in which order.
type Strategy string

// Strategies (docs/architecture.md §3.3, §6.2).
const (
	// ParallelBest races two healthy servers picked at random and takes the
	// first usable answer; if both fail, the rest of the group is raced.
	// This is the default (blocky's parallel_best).
	ParallelBest Strategy = "parallel_best"
	// Strict asks the servers one after another in configuration order,
	// moving on only on failure. Servers in backoff go to the end.
	Strict Strategy = "strict"
	// Random asks one healthy server picked uniformly at random and moves
	// on to another random server on failure.
	Random Strategy = "random"
	// RTTWeighted picks the first server at random weighted by the inverse
	// of its smoothed round-trip time, then falls back in RTT order.
	RTTWeighted Strategy = "rtt_weighted"
)

// Valid reports whether s is a known strategy. The empty strategy is valid
// and means ParallelBest.
func (s Strategy) Valid() bool {
	switch s {
	case "", ParallelBest, Strict, Random, RTTWeighted:
		return true
	}
	return false
}

// ECSMode is the EDNS Client Subnet (RFC 7871) policy for upstream queries.
type ECSMode string

// ECS modes.
const (
	// ECSStrip removes any client subnet; upstreams never learn where the
	// client is. This is the default for privacy.
	ECSStrip ECSMode = "strip"
	// ECSForward passes the client's own ECS option, truncated to the
	// configured prefix lengths; nothing is added when the client sent none.
	ECSForward ECSMode = "forward"
	// ECSAdd behaves like ECSForward and, when the client sent no option,
	// derives one from the client address if that address is globally
	// routable (private, loopback and link-local addresses are never sent).
	ECSAdd ECSMode = "add"
)

// ECSConfig configures client subnet handling.
type ECSConfig struct {
	Mode ECSMode
	// IPv4Prefix and IPv6Prefix bound the source prefix length sent
	// upstream; zero means the defaults 24 and 56.
	IPv4Prefix int
	IPv6Prefix int
}

// HealthConfig tunes failure tracking. Zero values take the defaults.
type HealthConfig struct {
	// Failures is the number of consecutive failures that put a server into
	// backoff (default 3).
	Failures int
	// BackoffMin is the first backoff (default 2 s); every further failed
	// probe doubles it up to BackoffMax (default 1 min).
	BackoffMin time.Duration
	BackoffMax time.Duration
}

// GroupConfig is one named group.
type GroupConfig struct {
	// Servers are addresses in dnsproxy syntax: ip[:port], udp://, tcp://,
	// tls://, https://, quic://, h3:// and sdns:// stamps. An entry may be
	// prefixed with a display name, "cloudflare=tls://1.1.1.1"; the name
	// (else the address as written) is what metrics and the query log show.
	Servers  []string
	Strategy Strategy
	// Timeout bounds one exchange with one server (default 2 s); for a
	// recursive group the whole lookup (default 4 s).
	Timeout time.Duration
	// Recursive groups have no servers: the built-in resolver
	// (internal/recursor) walks the DNS tree from the root servers.
	Recursive bool
}

// Config describes every group of a node.
type Config struct {
	Groups map[string]GroupConfig
	// Bootstrap lists plain resolvers (ip[:port]) used to resolve server
	// host names. Empty means the system resolver.
	Bootstrap []string
	// Conditional maps a domain suffix to a group; the longest suffix wins.
	// Keys may use the dnsmasq form "[/a.example/b.example/]".
	Conditional map[string]string
	ECS         ECSConfig
	Health      HealthConfig
	// Recursion configures the resolver of recursive groups.
	Recursion recursor.Config
}

// DefaultTimeout is the per-server exchange timeout when a group sets none.
const DefaultTimeout = config.DefaultUpstreamTimeout

// DefaultRecursiveTimeout bounds one recursive lookup when a recursive
// group sets no timeout; it stays below the 5 s query deadline.
const DefaultRecursiveTimeout = config.DefaultRecursiveTimeout

// RecursiveServer is the server name of a recursive group in metrics, the
// query log ("<group>/recursive") and Status.
const RecursiveServer = "recursive"

// ConfigFrom converts the settings section into a Config.
func ConfigFrom(c config.Upstreams) Config {
	out := Config{
		Groups:      make(map[string]GroupConfig, len(c.Groups)),
		Bootstrap:   append([]string(nil), c.Bootstrap...),
		Conditional: make(map[string]string, len(c.Conditional)),
		Recursion:   RecursionFrom(c.Recursion),
		ECS: ECSConfig{
			Mode:       ECSMode(c.ECS.Mode),
			IPv4Prefix: c.ECS.IPv4Prefix,
			IPv6Prefix: c.ECS.IPv6Prefix,
		},
	}
	for name, g := range c.Groups {
		out.Groups[name] = GroupConfig{
			Servers:   append([]string(nil), g.Servers...),
			Strategy:  Strategy(g.Strategy),
			Timeout:   g.Timeout.Std(),
			Recursive: g.Mode == config.UpstreamModeRecursive,
		}
	}
	for k, v := range c.Conditional {
		out.Conditional[k] = v
	}
	return out
}

// RecursionFrom converts the upstreams.recursion settings.
func RecursionFrom(c config.Recursion) recursor.Config {
	rc := recursor.DefaultConfig()
	rc.DNSSEC = c.DNSSEC
	rc.QNAMEMinimization = c.QNAMEMinimization
	if c.IPv6 != "" {
		rc.IPv6 = recursor.IPv6Mode(c.IPv6)
	}
	rc.TrustAnchors = append([]string(nil), c.TrustAnchors...)
	rc.RootHints = append([]string(nil), c.RootHints...)
	return rc
}

// withDefaults fills zero values.
func (c Config) withDefaults() Config {
	if c.ECS.Mode == "" {
		c.ECS.Mode = ECSStrip
	}
	if c.ECS.IPv4Prefix == 0 {
		c.ECS.IPv4Prefix = 24
	}
	if c.ECS.IPv6Prefix == 0 {
		c.ECS.IPv6Prefix = 56
	}
	if c.Health.Failures <= 0 {
		c.Health.Failures = 3
	}
	if c.Health.BackoffMin <= 0 {
		c.Health.BackoffMin = 2 * time.Second
	}
	if c.Health.BackoffMax < c.Health.BackoffMin {
		c.Health.BackoffMax = max(time.Minute, c.Health.BackoffMin)
	}
	return c
}

// Validate checks the configuration without dialing anything. Server
// addresses are parsed by New.
func (c Config) Validate() error {
	var errs []error
	if _, ok := c.Groups[DefaultGroup]; !ok {
		errs = append(errs, fmt.Errorf("upstream: group %q is required", DefaultGroup))
	}
	names := make([]string, 0, len(c.Groups))
	for n := range c.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g := c.Groups[n]
		if n == "" {
			errs = append(errs, errors.New("upstream: group name is empty"))
		}
		switch {
		case g.Recursive && len(g.Servers) > 0:
			errs = append(errs, fmt.Errorf("upstream: recursive group %q lists servers", n))
		case !g.Recursive && len(g.Servers) == 0:
			errs = append(errs, fmt.Errorf("upstream: group %q has no servers", n))
		}
		if !g.Strategy.Valid() {
			errs = append(errs, fmt.Errorf("upstream: group %q: unknown strategy %q", n, g.Strategy))
		}
		if g.Timeout < 0 {
			errs = append(errs, fmt.Errorf("upstream: group %q: negative timeout", n))
		}
	}
	switch c.ECS.Mode {
	case "", ECSStrip, ECSForward, ECSAdd:
	default:
		errs = append(errs, fmt.Errorf("upstream: unknown ECS mode %q", c.ECS.Mode))
	}
	if c.ECS.IPv4Prefix < 0 || c.ECS.IPv4Prefix > 32 || c.ECS.IPv6Prefix < 0 || c.ECS.IPv6Prefix > 128 {
		errs = append(errs, errors.New("upstream: ECS prefix lengths out of range"))
	}
	if _, err := newSuffixMap(c.Conditional, c.Groups); err != nil {
		errs = append(errs, err)
	}
	if c.hasRecursive() {
		if err := c.Recursion.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hasRecursive reports whether any group resolves recursively.
func (c Config) hasRecursive() bool {
	for _, g := range c.Groups {
		if g.Recursive {
			return true
		}
	}
	return false
}
