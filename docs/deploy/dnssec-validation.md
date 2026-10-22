# DNSSEC validation

DNSSEC validation is opt-in. Enable `dnssec.enabled` in the configuration
file, Settings → Expert → DNSSEC, or with:

```sh
kudurru settings set dnssec.enabled=true
```

A forward zone's `spec.forward.dnssecValidation` overrides that default;
omit it to inherit the global setting. Validation uses the same captured
upstream route as the answer. Private forwarding routes do not send proof
queries to a public resolver as a fallback. Core DNSSEC validation does not
require a license key.

The validator checks DNSKEY/DS chains, signatures, aliases, wildcards and
authenticated negative answers. Bogus or incomplete proofs return SERVFAIL
with an extended DNS error. Authenticated unsigned delegations return normal
answers with an insecure validation state. Queries setting CD bypass checking
and use a separate cache context. An upstream's AD bit is never trusted.
Client DO controls whether proof records are returned, not whether validation
runs. AD is set only for an unmodified, locally secure answer with live proofs,
CD clear, and client DO or AD interest. The query log records the local result
even when the client's DO bit is clear.

## Trust anchors and rollover

`dnssec.useBuiltinAnchors: true` trusts the IANA root DS anchors shipped with
the binary. This version includes key tags 20326 and 38696, from IANA's
signed root-anchor publication verified on October 14, 2026. Software upgrades
and explicit operator changes maintain anchors. This version does not run an
RFC 5011 trust-anchor update worker and never promotes a fetched DNSKEY to a
trust anchor automatically.

`dnssec.trustAnchors` adds complete DS or DNSKEY records in DNS presentation
format. With `useBuiltinAnchors: false`, the supplied records replace the
built-in set; an empty trust set is rejected even when global validation is
off, because individual forward zones may enable it. Anchors are public key
material and are visible in settings. At most 64 explicit anchors are allowed.

For a private-zone rollover:

1. Obtain the new DS/DNSKEY through an authenticated channel independent of
   the resolver being validated. Confirm its zone, algorithm and digest.
2. Save the existing settings. Add the new anchor alongside the old one
   before the signer starts relying on it, preserving the public root anchors
   if this node resolves public names.
3. Query representative positive, negative and alias answers through every
   node. Inspect DNSSEC state and EDE in the query log, and AD with `dig +dnssec`.
4. After the signer's rollover and its propagation interval, remove the
   retired anchor. Trust changes replace the validation/cache context
   immediately; outstanding queries keep their captured plan but cannot
   populate the replacement cache generation.

To roll back, restore the last authenticated trust set and repeat the checks.
Do not restore a key known to be compromised. A newly required anchor must
remain installed for domains that have already completed their rollover.

## Temporary exceptions

Use a negative trust anchor only for a known broken signed domain while its
owner repairs it. An exception covers a domain and its descendants; aliases
leaving that scope still need validation. Every entry needs a reason and an
absolute RFC3339 expiry no more than seven days away. At most 128 entries are
allowed, with no duplicate canonical domains.

```yaml
dnssec:
  enabled: true
  useBuiltinAnchors: true
  negativeTrustAnchors:
    - domain: broken.example.
      reason: "Signer rollover incident; repair tracked by the zone owner"
      expiresAt: "2026-10-15T12:00:00Z"
```

Choose an expiry appropriate to the current incident; the example date is
not extended automatically. The dashboard displays and edits the instant in
UTC. Expired entries can remain in configuration but no longer bypass
validation. Cache hits, reload and restart respect the same absolute expiry.
Exceptions never establish secure status or AD. Remove the exception after
repair and verify the domain validates normally.

## Cache and failures

The shared response cache stores raw answers with their route, validation
context and proof deadlines. Client policy processing runs outside that
cache. Stale serving and minimum TTL settings cannot extend authenticated
data past its proof deadline. Validation bypasses, trust changes and NTA
expiry cannot reuse a secure/bypass entry from an incompatible context.

If validation fails, inspect the EDE and the DNSSEC state, verify the node's
clock, and check that its configured upstream can return DNSSEC material over
UDP and TCP. Increasing a cache TTL or trusting upstream AD does not repair a
bad signature. Proof queries, signature checks and NSEC3 work are bounded;
responses exceeding those limits fail closed.
