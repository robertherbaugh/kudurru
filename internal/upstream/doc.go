// Package upstream forwards queries to upstream resolvers in named groups
// (dev-docs/design-record.md §3.3 "internal/upstream", §6.2 "upstreams").
//
// Transports come from dnsproxy's upstream package (plain UDP with TCP
// fallback on truncation, TCP, DoT, DoH over HTTP/1.1, HTTP/2 and HTTP/3,
// DoQ, DNSCrypt and sdns:// stamps, with bootstrap resolution of host
// names); the strategies, health tracking, conditional routing and ECS
// policy are kudurru's own:
//
//   - parallel_best (default) races two healthy servers chosen at random
//     and takes the first usable answer, then races the rest of the group if
//     both fail (the strategy name and idea come from blocky);
//   - strict tries servers in configuration order;
//   - random tries one random server, then another;
//   - rtt_weighted picks the first server weighted by 1/RTT (smoothed) and
//     falls back in RTT order.
//
// A server that fails Health.Failures times in a row (timeouts, network
// errors, REFUSED) goes into exponential backoff and is tried only after
// every healthy server; one successful probe restores it. SERVFAIL is passed
// over to the next server but does not count against health, because it is
// usually about the name rather than the server.
//
// Conditional forwarding maps domain suffixes to groups (longest suffix on
// label boundaries wins) and accepts dnsmasq-style "[/a/b/]" keys;
// ImportDnsmasq reads whole dnsmasq/dnsproxy rule files. pipeline.Request's
// UpstreamGroup hint, set by the authoritative stage for forward zones,
// takes precedence over the map. Answers routed by a hint, or by a suffix
// that maps to a group other than "default", are conditional and must not
// be cached (Set.Route reports which).
//
// ECS (RFC 7871) is stripped by default; forward and add modes are opt-in
// and truncate subnets to /24 and /56.
//
// miekg/dns types are confined to this package, the upstream and cache
// stages, dnsserver, zone and dnssec (§2).
package upstream
