# Response processing

Forwarded DNS answers can be rewritten, synthesized for NAT64, filtered, or
checked for rebinding. All settings are under `response` in the configuration,
the CLI settings commands, and **Settings → Expert → Response processing**.
Changes apply without restarting. The defaults leave answers unchanged.

```yaml
response:
  dns64:
    prefix: "64:ff9b::/96"
    excludeDomains: [internal.example]
    excludeIPv4: [192.168.0.0/16]
  filterAAAA: false
  bogusNXDOMAIN: [203.0.113.66/32]
  nxdomainIPv4: []
  nxdomainIPv6: []
  ttl: 300
  rebinding:
    enabled: true
    privateDomains: [internal.example]
    bypassNetworks: []
```

DNS64 requires an existing NAT64 translator on the client network. Use its
canonical IPv6 /32, /40, /48, /56, /64 or /96 prefix; an empty prefix disables
synthesis. An AAAA NODATA answer triggers at most one A lookup through the same
captured resolver and validation settings. Only matching alias targets are
combined. A successful synthetic AAAA answer cannot outlive the A data, the
original negative SOA TTL, or `response.ttl`. Upstream validation failures stay
failures. Queries with DO set retain their original response so clients can
validate their own denial; CD alone does not disable synthesis. Exclusions
apply to both question and terminal alias domain, and to each IPv4 address.

`filterAAAA` removes IPv6 addresses and IPv6 SVCB/HTTPS hints. A matching AAAA
answer becomes NODATA. This takes precedence over DNS64 and also filters
configured policy rewrites. `bogusNXDOMAIN` turns matching answer addresses or
service hints into NXDOMAIN, for upstreams that redirect nonexistent domains.
This generated negative is never immediately overridden. The `nxdomainIPv4`
and `nxdomainIPv6` lists replace an actual upstream NXDOMAIN for the respective
question family, using `response.ttl`. Empty lists preserve NXDOMAIN.

Rebinding protection refuses unexpected private, loopback, link-local,
unspecified and multicast addresses, including IPv4-mapped IPv6 and service
hints in every response section. It also checks DNS64's IPv4 inputs before
embedding them. Rejection returns REFUSED with EDE Blocked when the client
uses EDNS. Exceeding the bounded inspection budget also refuses the response.
An allowed private domain must cover both the original question and the
address owner; an alias to another domain does not inherit its exemption.
Client CIDRs in `bypassNetworks` bypass the whole check. Your authoritative
zones, stub routes and intentional block responses keep their existing behavior.

## Per-policy choices and rewrites

Open a policy's **Response processing** section to edit client overrides and
rewrites. They are also available in the creation wizard's **How** step:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata:
  name: nat64-clients
  namespace: default
spec:
  clientGroupRefs: [nat64]
  priority: 100
  response:
    dns64Prefix: "64:ff9b::/96"
    filterAAAA: false
  rewrites:
    - name: application.example
      targets: [192.0.2.8, "2001:db8::8"]
    - name: "*.alias.example"
      cname: application.example.
```

Each nullable response field uses the highest-priority policy that sets it.
Omission inherits; an explicit empty `dns64Prefix` or `filterAAAA: false`
disables that inherited behavior. DNS64 exclusions and rebinding exceptions
remain global settings.

Rewrites match an exact name or descendants of `*.suffix` (excluding the
suffix itself). The highest policy priority wins, then the most specific rule
within that policy, then input order. An omitted type infers A/AAAA from
address targets. Explicit types support A, AAAA, CNAME, PTR, TXT, MX, SRV,
HTTPS, SVCB, CAA, NS and NAPTR with DNS presentation-format targets. A CNAME
returns an alias; its subsequent client query follows ordinary zone and policy
boundaries. Other query families get NODATA when the rule has no matching
type. Rules have at most 16 targets and a policy has at most 128 rules.

Configured rewrites remain active while ad blocking is paused. Adblock
`dnsrewrite` and safe-search rules follow the blocking toggle. An invalid
compiled response policy reports `InvalidResponsePolicy` and fails its
recursive queries with SERVFAIL until corrected. Unavailable authoritative
zone coverage cannot be replaced by a rewrite or by NXDOMAIN handling.

## Cache and DNSSEC behavior

Validation runs before raw cache insertion. Every client gets a private copy
and its current policy; synthesized answers never enter the shared raw cache.
Changes take effect on existing raw-cache hits, while a query already in
flight retains its captured response settings. Response blocking inspects
synthesized data, followed by rebinding and final wire flags.

Changed answers clear AD and AA and remove signatures and denial proofs that
no longer describe them. Configured rewrites are local data, not authenticated
upstream answers. Enabling NXDOMAIN replacement or AAAA filtering therefore
deliberately changes the result even when the source validated securely.

Core cache isolation does not make a shared edge packet cache safe for
client-specific answers. Keep an external edge's packet cache disabled until
the edge's client-policy isolation is verified. The full v0.2 edge package
tracks the managed-edge acceptance work separately.
