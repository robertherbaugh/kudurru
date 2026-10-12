---
title: Coming from AdGuard Home
linkTitle: From AdGuard Home
description: AdGuard Home's filters, custom rules, persistent clients and upstream settings, and where each one lives in kudurru.
weight: 20
---

AdGuard Home keeps everything in `AdGuardHome.yaml`; kudurru splits it into
settings (upstreams, cache, blocking defaults, query log) and objects
(lists, client groups, policies, zones) that use the same YAML in Docker
and in Kubernetes. kudurru reads the adblock rule syntax with its own
parser (AdGuard Home's code is GPL and none of it is used), so most filter
lists work unchanged.

There is no AdGuard Home importer yet (`kudurru import adguard` is planned,
see the [migration overview](/migrate/)), so you write the objects once by
hand.

## Concepts

| AdGuard Home | kudurru | Notes |
|---|---|---|
| DNS blocklists | [`BlockList`](/reference/crds/blocklist/) objects with `mode: block` and `url` sources | Refreshed every `spec.refreshInterval`; `kudurru lists refresh <name>` fetches now |
| DNS allowlists | `BlockList` objects with `mode: allow`, referenced from a Policy's `allowListRefs` | Allow wins across every list that applies |
| Custom filtering rules | an inline source (`inline:` lines) with `format: adblock`, or `kudurru lists allow` / `deny` for single domains | See the rule support below |
| Persistent clients (IP, CIDR, ClientID) | entries of a [`ClientGroup`](/reference/crds/clientgroup/): `ip`, `cidr`, `clientID`, `hostname`, `nameGlob` | MAC entries are accepted but match only once the v0.2 client inventory supplies addresses |
| Per-client settings (filters, upstreams) | a [`Policy`](/reference/crds/policy/) for that group: `blockListRefs`, `allowListRefs`, `upstreamGroup`, `blockMode`, `queryLog` | The highest `priority` wins per setting when several policies match |
| Client tags | none | Use client groups |
| Schedules (pause per client) | `Policy.spec.schedule`: weekdays, start and end time, time zone | The policy applies only inside its window |
| Upstream DNS servers | [`upstreams.groups`](/reference/configuration/#upstreams.groups) | Named groups; a Policy or a forward zone picks one |
| `[/example.org/]upstream` lines | [`upstreams.conditional`](/reference/configuration/#upstreams.conditional), which accepts the same `[/a/b/]` form as keys | Longest suffix wins |
| Upstream mode: load balancing, parallel | `strategy: rtt_weighted`, `parallel_best` (the default), `strict` (in order), `random` | |
| Bootstrap DNS servers | [`upstreams.bootstrap`](/reference/configuration/#upstreams.bootstrap) | |
| Private reverse DNS servers | `upstreams.conditional` entries for the `in-addr.arpa.` and `ip6.arpa.` suffixes | |
| EDNS Client Subnet | [`upstreams.ecs.mode`](/reference/configuration/#upstreams.ecs.mode): `strip` (default), `forward`, `add` | |
| Rate limit | [`ratelimit.*`](/reference/configuration/#ratelimit.enabled), queries per minute per prefix | UDP only |
| Blocking mode (default, NXDOMAIN, REFUSED, custom IP, null IP) | `blockMode` (Policy), `response` (BlockList) or [`blocking.defaultResponse`](/reference/configuration/#blocking.defaultResponse): `zeroIP` (default), `nxdomain`, `refused`, `customIP` | |
| Blocked response TTL | [`blocking.blockTTL`](/reference/configuration/#blocking.blockTTL) or `Policy.spec.blockTTL` | |
| Cache size, optimistic caching | [`cache.maxEntries`](/reference/configuration/#cache.maxEntries), `cache.prefetch.*`, `cache.serveStale.*` | Serve-stale follows RFC 8767 |
| DNS rewrites | records in a primary [`DNSZone`](/reference/crds/dnszone/) of your own (`kudurru records add`) | Per-policy answer rewrites (`Policy.spec.rewrites`) are planned for v0.2 |
| Blocked services, safe search | the kudurru-owned catalogue of decision D60 | Not every build has it yet: check your version's changelog. Without it, list a service's domains in a BlockList |
| Parental control, safe browsing web services | none | Use blocklists |
| Query log and statistics | the dashboard; [`querylog.privacy`](/reference/configuration/#querylog.privacy) and `querylog.retention.*` | Per node, with rollups for long-term views (reading them needs a [license key](/project/licensing/)) |
| Encryption settings (DoT, DoH, DoQ) | `listeners.dot`, `doh`, `doq`, `doh3` with a certificate, or dnsdist in front | See [encrypted transports](/deploy/docker/#encrypted-transports) |
| DHCP server | none | Keep DHCP on your router or a DHCP server |
| DNSSEC | not yet | Validation is planned for v0.2 |

## Rule support

| Rule | Example | v0.1 |
|---|---|---|
| Domain and subdomains | `\|\|example.org^` | yes (`\|\|x` is read as `\|\|x^`) |
| Exception | `@@\|\|example.org^` | yes, within its own list |
| Hosts lines | `0.0.0.0 example.org` | yes; lines pointing at other addresses are rewrites and counted as unsupported |
| Plain domains, `*.example.org` | `example.org` | yes; `*.x` also matches `x` |
| Regular expressions | `/^ad[0-9]+\./` | yes, up to 1,000 per list |
| Modifiers | `$important`, `$badfilter`, `$dnstype`, `$dnsrewrite`, `$client`, `$ctag`, `$denyallow` | counted as unsupported and ignored; planned for v0.2 |

A list's `status.invalid` and the dashboard show how many rules were
skipped, so a browser-oriented list is safe to load.

## An AdGuard Home setup as kudurru objects

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: adguard-dns-filter, namespace: dns}
spec:
  mode: block
  sources:
    - url: https://adguardteam.github.io/HostlistsRegistry/assets/filter_1.txt
      format: adblock
    - name: custom-rules
      format: adblock
      inline:
        - "||tracker.example^"
        - "@@||cdn.example^"
---
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: living-room-tv, namespace: dns}
spec:
  clients:
    - {ip: 192.168.1.40, name: TV}
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: living-room-tv, namespace: dns}
spec:
  clientGroupRefs: [living-room-tv]
  blockListRefs: [adguard-dns-filter]
  upstreamGroup: default
```

The upstream configuration goes into the settings of `kudurru.yaml`:

```yaml
upstreams:
  groups:
    default:
      servers: ["tls://1.1.1.1", "tls://9.9.9.9"]
      strategy: parallel_best
    lan: {servers: ["192.168.1.1:53"]}
  conditional:
    "[/lan/168.192.in-addr.arpa/]": lan
  bootstrap: ["1.1.1.1:53", "9.9.9.9:53"]
```

Then switch over as with [Pi-hole](/migrate/pihole/#switching-over): run both,
compare with `kudurru query <name> --client <address>`, and change the DHCP
DNS option.
