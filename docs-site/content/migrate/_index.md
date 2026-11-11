---
title: Move over to kudurru
linkTitle: Migrate
description: Compare Pi-hole, AdGuard Home, Technitium and kudurru, then migrate lists, clients and zones with a tested cutover and rollback plan.
weight: 30
---

Start with the DNS server you run today. These guides describe working
features, configuration differences and the checks to make before changing
a network's DNS. The comparison below covers v0.2.0. A few features depend on
the store a node uses, SQLite or Kubernetes; the comparison and the linked
pages say where.

| Your current server | What the guide covers |
|---|---|
| [Pi-hole](/migrate/pihole/) | Teleporter backup, adlists, exact/wildcard/regex domains, groups, local DNS and a DHCP handoff |
| [AdGuard Home](/migrate/adguard-home/) | Configuration backup, filter compatibility, exceptions, persistent clients, rewrites and encrypted DNS |
| [Technitium](/migrate/technitium/) | Zone export/import, conditional forwarders, allowed/blocked zones, DNS Apps and features to retain elsewhere |
| [Blocky](/migrate/blocky/) | Offline YAML conversion, client policy selection, suffix-wide local mappings and explicit conversion losses |
| [Offline configuration conversion](/migrate/offline-config/) | AdGuard Home and Blocky preview, selection, tracked application and limits |
| [Cutover and rollback checklist](/migrate/cutover/) | Parallel installation, CLI setup, client identity, real DNS tests, IPv4/IPv6 rollout and recovery |

**Pi-hole v6 and Technitium have built-in API importers.** In **Tools**, choose
**Import from DNS server** to connect, preview and import directly, or use
`kudurru import pihole|technitium --source-url URL --apply`. Supported objects
and upstreams transfer automatically; the preview reports unsupported features.
`kudurru import adguard|blocky --file PATH` converts supported YAML
settings offline; it is a partial conversion with a loss report. kudurru cannot
restore a Pi-hole Teleporter archive, a product working directory or a Technitium backup. `kudurru import bind` imports standard DNS zone files.
Migration time
depends on your rules and services; a server that also provides DHCP,
DNSSEC or app-specific behavior needs more preparation than a basic blocker.

## Feature comparison

Built-in capabilities unless stated otherwise; planned features are labeled
explicitly. The Pi-hole, AdGuard Home and Technitium columns were reviewed
October 16, 2026 against the [Pi-hole documentation](https://docs.pi-hole.net/),
[AdGuard Home configuration reference](https://github.com/AdguardTeam/AdGuardHome/wiki/Configuration)
and [Technitium feature list](https://technitium.com/dns/). The kudurru column
was reviewed November 10, 2026 against the v0.2.0 code and the
[kudurru changelog](/project/changelog/). Other projects evolve independently;
check the version you run.

| Feature | Pi-hole | AdGuard Home | Technitium | kudurru v0.2.0 |
|---|---|---|---|---|
| Filtering | Hosts, domains, regex and groups | Adblock DNS syntax, allowlists and client settings | Allowed/blocked zones and lists; Advanced Blocking app | Hosts, domains, adblock rules with DNS modifiers and RE2; client-group policies |
| Local names and authority | Local DNS and CNAME records | DNS rewrites | Primary, secondary, stub, forwarder and catalog zones | Primary, forward, secondary and stub zones; RFC 2136 updates; per-policy answer rewrites; BIND import/export; AXFR out, AXFR/IXFR in |
| Resolution | Forwarding; recursion with a separate resolver | Forwarding | Full recursion or forwarding | Forwarding or built-in recursion (QNAME minimization) per named upstream group; conditional forwarding |
| DNSSEC | Optional validation | Requests DNSSEC data from upstreams | Validation and authoritative signing | Validation in recursive groups; opt-in validation of forwarded answers; no signing |
| DHCP | Built in | Built in | Built in | Opt-in DHCPv4 on Linux; dashboard setup; three-node quorum failover on SQLite or an elected replica behind a relay on Kubernetes; no DHCPv6 |
| Encrypted DNS listeners | Separate proxy | DoT, DoH, DoQ | DoT, DoH, DoQ | DoT, DoH, DoQ, DoH3 |
| Multiple servers | Separate instances; external sync tools | Separate instances; external sync tools | Built-in clustering | Shared Kubernetes store; enrolled SQLite cluster with one primary and its followers; Compose HA uses shared object files |
| Kubernetes configuration | Container configuration; no native CRDs | Container configuration; no native CRDs | Container configuration; no native CRDs | Native CRDs, Helm, external-dns webhook |
| Identity | Local dashboard login | Local users | Roles, OIDC, LDAP | Local users, with authenticator codes on the SQLite store; OIDC and scoped roles with a key |
| License | Open source; component licenses apply | GPL-3.0 | GPL-3.0 | Source available; eligible community use free, commercial production by agreement |

AdGuard Home's DNSSEC option requests DNSSEC records from its upstreams;
it does not make it a local validating resolver. kudurru validates DNSSEC
in upstream groups set to recursive mode, which resolve from the root servers
instead of forwarding. It validates answers from forwarding groups only when
you turn that on: the setting `dnssec.enabled` is off by default, and a
forward zone can set its own choice ([DNSSEC validation](/deploy/dnssec-validation/)).
It does not sign zones. Forwarding stays the default; see
[resolving names yourself](/start/home/#7-resolve-names-yourself-optional).
A schema field for a future feature does not mean that feature is active.
In particular, the zone types `external`, `catalog` and `secondaryCatalog`
and zone signing (`spec.dnssec`) are in the schema and are not served.

**Do not apply a zone of the type `external`, `catalog` or
`secondaryCatalog`, and do not apply a block-list URL that the kudurru host
cannot download.** In this version a node with the SQLite store accepts such
a zone, or a `BlockList` with an enabled URL source that has never been
downloaded, and then cannot publish its configuration. This happens even
when no policy refers to the list. The node stops answering for its own zones
and refuses every API request, including the deletion of that object.
Forwarded names still answer until the node restarts. **Do not restart it**:
after a restart it answers SERVFAIL for every name, forwarded names too.

A list recovers by itself when a later download attempt succeeds; the
attempts are minutes apart. A zone does not. To recover otherwise, stop the
node and put back a copy of its data directory from before the object was
applied. Before you apply a list URL, download it once from the kudurru host
(for example with `curl -fsS -o /dev/null URL`), apply lists with URLs one
at a time, and keep a copy of the data directory.

Since v0.2.0, secondary and stub zones, RFC 2136 updates
([dynamic updates](/deploy/dynamic-updates/)), validation of forwarded answers,
adblock modifiers and per-policy answer rewrites
([response processing](/deploy/response-processing/)) are served on both
stores. Some other features behave differently on the two stores or need one
of them, for example [background integrations](/deploy/integrations/),
transferred RPZ feeds and authenticator codes. The architecture overview keeps
[the table](/architecture/#what-depends-on-the-store-driver); read it for your
store before you plan around one of these.

### DHCPv4 support

Built-in DHCPv4 has been included since v0.1.0 and is disabled by default. It supports
standalone Linux/SQLite nodes or a three-node quorum, the
[Kubernetes store](/deploy/dhcp/#kubernetes) (`DHCPScope` and `DHCPLease`
objects, one elected replica serving relayed clients), durable leases and
reservations, complete dashboard setup, multiple subnets and trusted relays,
conflict checks, network/boot options, live management, lease import/export
and persistent local DNS ownership. See the [DHCPv4 deployment guide](/deploy/dhcp/)
for setup and cutover.

You can keep your existing DHCP server or move DHCPv4 to kudurru. Stop the
old allocator before activating its replacement. DHCPv6 is not supported;
keep IPv6 router and DNS advertisements on your router.

### License and deployment choices

Without a key, kudurru runs one node, `Policy/default` and the built-in
administrator. Additional policies, OIDC, scoped roles, audit storage,
statistics older than 24 hours, integrations and the edge supervisors need
the applicable entitlements. A free key for eligible community use unlocks these features
and up to three nodes. Business and government production use requires a
commercial agreement; see [licensing](/project/licensing/) and
[request a key](https://kudurru.io/license/).

For multiple nodes, choose [Kubernetes](/deploy/helm/) for a shared
configuration store, an [enrolled SQLite cluster](/deploy/sqlite-cluster/) in
which followers copy the data of one primary, or the
[Compose HA layout](/deploy/compose-ha/) for independent SQLite nodes managed
from the same object files. In an enrolled cluster nothing is elected: when
the primary is lost, an operator promotes a follower. Docker containers that
are neither enrolled nor managed from the same files do not synchronize their
configuration. There is no Raft store.

## How to approach the move

1. Complete [preparation](/migrate/cutover/#prepare) and take a source backup.
2. Follow your product's guide to recreate lists, groups, upstreams and zones.
3. Complete the [validation checklist](/migrate/cutover/#validate), including one real client from each group.
4. [Switch DNS advertisements](/migrate/cutover/#switch-over) only after the checks pass.
5. Keep the old server available through the observation period and use the
   [rollback procedure](/migrate/cutover/#roll-back) if needed.

Query history, statistics, passwords, API tokens and source-specific plugins
stay with the original server. Export history separately if you need to keep it.
