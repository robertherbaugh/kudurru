# DHCP lease names in DNS

[DHCP guides](dhcp.md) · [Step-by-step setup](../guide/dhcp/README.md)

## Automatic lease DNS

Set `registerDNS: true` and use a dedicated domain such as `lan.home.arpa`.
A bound lease named `laptop` answers at `laptop.lan.home.arpa` and has a PTR
record. Reservation hostnames override client-supplied names. Offers,
quarantined or expired addresses have no records. The earliest still-live
bound claimant owns an unreserved name, across scopes sharing a domain. Later
duplicates cannot suppress it; the earliest waiting claimant takes over when
the owner ends. A reserved hostname is protected from claims by a different MAC,
even when the reserved device is offline.

Claim order survives renewal and restart. Release, expiry, rename, DNS disable
or domain reassignment ends the old claim. Public imports cannot take ownership
from an existing live name. Conflicting configured reservation names suppress
that name until corrected. DHCP names and MAC addresses are unauthenticated;
clients can still claim unused names or impersonate a MAC. Use reservation hostnames for stable names, but MAC
reservations do not prevent impersonation. Enforce required restrictions with
IP/network policies and a restrictive default policy; hostname matching is
advisory, and a missing match can select a fallback policy.

Option 12 hostnames have trailing NUL bytes stripped. Empty or unusable names
are ignored, so an otherwise valid DHCP request can still obtain a lease.
Malformed option framing and invalid configured reservation names are rejected.

**Do not create a configured zone covering the DHCP domain.** Specific
primary or forward zones retain precedence, including their negative answers.
A primary root also takes precedence. The default root **forward** zone is
the exception: its catch-all route allows DHCP names to resolve locally.
For example, a configured `home.arpa` zone hides DHCP names under
`lan.home.arpa`; use a separate domain or reorganize the zones. Specific
reverse zones likewise take precedence over generated PTRs.

The DHCP domain returns local negative answers for missing names, so dedicate
it to leases. Unrelated DNS names continue through normal routing. Generated
records do not appear in zone record editors, modify zone serials or export
through zone transfers. TTL is the smaller of `dnsTTL` and remaining lease
life. Server answers reflect committed changes immediately, while clients
may retain an earlier answer until its TTL expires. Lease hostnames are also
available to hostname/nameGlob client policy matching.
