---
title: Coming from Pi-hole
linkTitle: From Pi-hole
description: Pi-hole's groups, lists, domains and clients map almost one to one onto kudurru's ClientGroup, Policy and BlockList objects.
weight: 10
---

The dashboard will feel familiar: stat tiles, the 24-hour charts, top lists,
a query log that says why each answer was given, and one-click allow and
deny. The difference is underneath. Pi-hole keeps its configuration in
`pihole.toml` and the gravity database; kudurru keeps it as typed objects
(the same YAML in Docker and in Kubernetes) plus a settings file, and every
object records who manages it.

There is no Pi-hole importer yet (`kudurru import pihole` is planned, see
the [migration overview](/migrate/)), so you write the objects once by hand.
The mapping below is the whole job for most installations.

## Concepts

| Pi-hole | kudurru | Notes |
|---|---|---|
| Group | a [`ClientGroup`](/reference/crds/clientgroup/) (who) plus a [`Policy`](/reference/crds/policy/) (what applies to them) | The Policy names the group in `clientGroupRefs` and the lists in `blockListRefs` and `allowListRefs` |
| Default group | `ClientGroup/default` and `Policy/default` | Created at the first start; they apply to clients in no other group |
| Client (IP, subnet, hostname) | an entry of `ClientGroup.spec.clients`: `ip`, `cidr`, `hostname`, `nameGlob` or `clientID` (DoT/DoH client ID) | MAC entries are accepted but match only once the v0.2 client inventory supplies addresses |
| List (adlist, allow list) | a [`BlockList`](/reference/crds/blocklist/) with `mode: block` or `mode: allow` and `url` sources | `format: auto` recognises hosts, domains and adblock lists |
| Domains: exact, wildcard and regex allow / deny | the dashboard's Allowlist and Denylist (per-entry switch, comment, group assignment, added date and origin), `kudurru lists allow <domain>`, `kudurru lists deny <domain>` (`--regex`), or one-click in the query log | Stored in the `ui-allow` / `ui-deny` lists (`ui-allow-<group>` / `ui-deny-<group>` for a group); see docs/deploy/allowlist.md. Up to 1,000 expressions per list |
| Disabling one domain of an adlist | the list's page → source → Entries: switch the entry off (`spec.sources[].exclude`) | Affects that list only; allow the name to let it through everywhere |
| Updating gravity (`pihole -g`) | automatic, every `spec.refreshInterval` (default [`blocking.refreshInterval`](/reference/configuration/#blocking.refreshInterval)); `kudurru lists refresh <name>` fetches now | The leader fetches, every replica pulls by digest |
| Disable blocking for a while | `kudurru blocking disable 5m`, or the dashboard toggle | `blocking enable` ends it early |
| Blocking mode (NULL, IP, NXDOMAIN, NODATA) | `blockMode` of the Policy, `response` of the BlockList, or [`blocking.defaultResponse`](/reference/configuration/#blocking.defaultResponse): `zeroIP` (default), `customIP`, `nxdomain`, `refused` | `customIP` with an empty address answers NODATA |
| CNAME inspection | always on: answers are checked as well, including CNAME targets, HTTPS/SVCB targets and the returned addresses | |
| Local DNS records | records in a primary [`DNSZone`](/reference/crds/dnszone/) of your own, for example `home.arpa.` | `kudurru records add`; a name needs a zone that contains it |
| Local CNAME records | `CNAME` records in that zone | Answers follow the chain inside the zone |
| Conditional forwarding | [`upstreams.conditional`](/reference/configuration/#upstreams.conditional) (suffix to upstream group, dnsmasq `[/a/b/]` form accepted), or a `DNSZone` of type `forward` | |
| Upstream DNS servers | [`upstreams.groups`](/reference/configuration/#upstreams.groups) | Several named groups with a strategy each; a Policy can pick one per client group |
| Rate limiting | [`ratelimit.*`](/reference/configuration/#ratelimit.enabled): queries per minute per IPv4 /32 and /24 (and IPv6 prefixes) | UDP only; TCP and the encrypted transports are not limited |
| Privacy level | [`querylog.privacy`](/reference/configuration/#querylog.privacy): `full`, `anonymize_client`, `hide_domains`, `hide_domains_clients`, `nothing` | Applied once, when a query is logged |
| Long-term data | rollups kept 35 days per minute, 400 days per hour and daily forever; raw rows for [`querylog.retention.raw`](/reference/configuration/#querylog.retention.raw) | The dashboard marks merged top lists as approximate |
| Web interface password | the local `admin` user (reset on the host with `kudurru admin reset-password admin`), other local users, or OIDC login | Roles: viewer, auditor, operator, zone-admin, admin |
| Pi-hole API | `/api/v1`, see the [REST API reference](/reference/api/) | A Pi-hole v6 compatible API is planned for v1.0 |
| Teleporter | `kudurru backup --out file.yaml` and `kudurru restore -f file.yaml` | kudurru's own format, not a Teleporter archive |
| `pihole.toml` | `kudurru.yaml` and the settings file, see the [settings reference](/reference/configuration/) | |
| DHCP server | none | Keep DHCP on your router or a DHCP server, and hand out kudurru's address as the DNS server |
| DNSSEC validation | not yet | Planned for v0.2 |

## A Pi-hole setup as kudurru objects

A household with the default group, a "Kids" group on its own VLAN, one
extra list and a local name:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: hagezi-pro, namespace: dns}
spec:
  mode: block
  sources:
    - url: https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/pro.txt
---
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: kids, namespace: dns}
spec:
  clients:
    - {cidr: 192.168.20.0/24, name: Kids VLAN}
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: kids, namespace: dns}
spec:
  clientGroupRefs: [kids]
  blockListRefs: [hagezi-pro]
---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: home-arpa}
spec:
  zoneName: home.arpa.
  type: primary
  soa: {mname: ns.home.arpa., rname: hostmaster.home.arpa.}
  nameservers: [ns.home.arpa.]
```

```sh
kudurru apply -f pihole-migration.yaml
kudurru records add home-arpa nas.home.arpa A 192.168.1.10
kudurru query doubleclick.net --client 192.168.20.15   # blocked, and by which list and rule
```

Write the objects into `objects.d/` instead of applying them if you want
the files to be the source of truth; they are then read-only in the
dashboard and re-applied whenever they change.

## Switching over

1. Run kudurru on another address next to Pi-hole and apply the objects.
2. Compare answers for a few clients with `kudurru query <name> --client <address>`.
3. Point the router's DHCP DNS option at kudurru. Clients move as their
   leases renew.
4. Keep Pi-hole until its query log has gone quiet.
