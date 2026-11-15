# Global DNS controls

Use `control.allowedNetworks` to limit client networks and
`control.blockedQTypes` to refuse DNS query types. Both are cluster settings,
available through the generic settings API, CLI and dashboard. Changes apply
without restarting. Empty lists preserve unrestricted defaults:

```yaml
control:
  allowedNetworks: [192.0.2.0/24, "2001:db8::/32"]
  blockedQTypes: [HINFO, TYPE65280]
```

The network list accepts at most 1024 IPv4 or IPv6 CIDRs. Host bits are masked.
IPv4-mapped IPv6 prefixes with at least 96 prefix bits are interpreted as IPv4;
shorter mapped prefixes are rejected because they do not describe only IPv4.
Client addresses are unmapped before comparison. Loopback clients always pass
the network check, preserving local tools and health checks. They still obey
the blocked-type list.

The type list accepts at most 256 case-insensitive DNS mnemonics or explicit
`TYPE1` through `TYPE65535` names, including private types. Unknown mnemonics,
type zero, bare numbers and out-of-range values are rejected. Invalid settings
are rejected before adoption; an invalid file reload keeps the last valid rules.

Controls run after ingress and identification and before local records,
rewrites, raw-cache hits and upstream answers. Ingress keeps its existing
special-query and malformed-message behavior. The control stage silently drops
malformed queries. Other denied queries receive REFUSED (`refused_acl`), with
EDE 18 (Prohibited) when the request has EDNS. DNS64's additional A lookup and
rewrite dependencies obey the same controls; an allowed AAAA request cannot
synthesize an answer from a prohibited A lookup. Blocking a transfer type also
prevents that type from reaching authoritative handling. These rules complement
zone permissions and ad blocking; they do not grant write or transfer authority.

For example, send a JSON merge patch to `PATCH /api/v1/settings`:

```json
{"control":{"allowedNetworks":["192.0.2.0/24"],"blockedQTypes":["HINFO"]}}
```

SQLite stores the accepted settings in the canonical singleton. The background
complete publisher prepares the replacement; persistence acknowledgment can
precede its publication. Kubernetes uses the existing settings
ConfigMap/file ownership path. Each request retains the exact immutable rules
it captured, including synchronous subqueries; a request already in flight
does not adopt replacement rules. Fresh requests see the new complete view.
The local edge resolver cache is invalidated on changes. Independently managed
remote edges need their own cache invalidation or TTL expiry; keep their packet
caches disabled when policy isolation is not established.
