// Package recursor is kudurru's own recursive resolver: it answers a
// query by walking the DNS tree from the root servers instead of
// forwarding to a public resolver, and validates the answers with DNSSEC
// (docs/architecture.md D170, docs/architecture/upstreams-and-cache.md
// "Recursive groups").
//
// An upstream group with mode "recursive" sends its queries here; the
// Resolver implements dnsproxy's upstream interface, so group health,
// metrics, the query log, the response cache, conditional forwarding,
// forward zones and policies treat it like any other upstream server.
//
// Resolution:
//
//   - Built-in IANA root hints, refreshed from the roots themselves
//     (priming, RFC 9609).
//   - Iterative resolution with a delegation cache, glue limited to the
//     delegating zone (bailiwick), lazily resolved name-server addresses,
//     CNAME and DNAME chains across zones and TCP after truncation.
//   - QNAME minimization (RFC 9156, relaxed): a server sees only one label
//     more than the zone it serves until the full name is needed; an
//     NXDOMAIN for a minimized name is retried with the full name once.
//   - Privacy: no client subnet (ECS) is ever sent, and names that must
//     never leave the network (RFC 6303 private reverse zones, home.arpa,
//     local, internal and the other special-use names) are answered here.
//   - Anti-spoofing: random query IDs and source ports, random letter case
//     in query names (0x20, with a per-server fallback), strict question
//     matching and bailiwick scrubbing of every section.
//   - Limits: per client query a budget of outgoing queries, a nesting
//     depth, a referral and CNAME count and a bound on glueless name
//     servers per delegation (NXNS, NRDelegation); a cap on signature
//     checks per RRset (KeyTrap) and NSEC3 iterations (RFC 9276).
//
// Validation (RFC 4033-4035, 5155, 6840):
//
//   - The root trust anchors (KSK-2017 and KSK-2024) are built in and may
//     be replaced in the settings.
//   - The chain of trust is built lazily, only for zones whose data is
//     validated: DS records from referrals are kept and checked when the
//     zone's keys are first needed; otherwise DS is queried at the parent.
//   - Positive answers, wildcard expansions, NXDOMAIN and NODATA (NSEC and
//     NSEC3, opt-out) are proven; unsigned data below a secure zone is
//     insecure only when a delegation without DS is proven, else bogus.
//   - Bogus answers become SERVFAIL with an extended DNS error (RFC 8914)
//     unless the client set CD; AD is set on secure answers for clients that
//     asked with DO or AD.
//
// miekg/dns types are allowed here (docs/architecture.md §2).
package recursor
