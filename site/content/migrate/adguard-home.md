---
title: Move from AdGuard Home to kudurru
linkTitle: From AdGuard Home
description: Back up AdGuard Home, check filter-rule compatibility, migrate clients and exceptions, rebuild rewrites and upstreams, and test a DNS cutover.
weight: 20
---

Bring your DNS filters and client settings into kudurru's lists, groups,
policies and zones. kudurru understands common DNS adblock rules, but
v0.1.0 does not implement AdGuard Home's full filtering language. Treat this
as a configuration migration with explicit behavior checks.

**Before you start:** complete [preparation and CLI setup](/migrate/cutover/#prepare).
Keep AdGuard Home serving the network. There is no `AdGuardHome.yaml` or
working-directory importer.

## Compare the features you use

| AdGuard Home | kudurru v0.1.0 | Migration action |
|---|---|---|
| DNS blocklists and allowlists | `BlockList` with `mode: block` or `mode: allow` | Copy URLs; attach the lists to policies |
| Custom filtering rules | Inline sources or structured entries | Check modifiers and exception scope before copying rules |
| Persistent clients and per-client settings | IP/CIDR `ClientGroup` plus `Policy` | Reserve addresses; additional policies need a key |
| ClientID, MAC and hostname identification | No production source for these matchers | Use real IPv4/IPv6 addresses or subnets |
| DNS rewrites | Primary zones and records | Recreate supported local records; general per-policy rewrites are deferred |
| Blocked services and safe search | Policy service/category blocking and safe search | Match catalog IDs and supported engines explicitly |
| Upstreams and bootstrap servers | Named groups, strategies, bootstrap and suffix rules | Recreate configuration; retest private reverse DNS |
| DoT, DoH and DoQ | Native listeners; also DoH3 | Configure listener certificates and client endpoints separately |
| DHCP | No DHCP server | Hand off DHCP before retiring AdGuard Home |
| Safe browsing / parental-control lookup services | No equivalent remote lookup services | Select suitable block lists; do not assume identical coverage |
| Query history and users | Separate kudurru logs and accounts | Preserve old data separately; create new credentials and API tokens |

See the [full feature comparison](/migrate/#feature-comparison), including
DNSSEC and license differences.

## 1. Back up and inventory AdGuard Home

1. Save `AdGuardHome.yaml` and its working/data directory, including filters
   and any history you need. Use a consistent snapshot or a brief service
   stop for the copy, then keep AdGuard Home serving the network.
2. Record enabled blocklist/allowlist URLs, custom rules and DNS rewrites.
   Keep disabled rules and comments in your inventory.
3. Record persistent client identifiers, per-client upstreams, service
   blocks, safe-search settings, schedules, cache settings and log privacy.
4. Save upstream/bootstrap/private reverse-DNS settings and encrypted DNS
   hostnames, ports, certificates and paths. Save DHCP leases and reservations
   if this server provides DHCP.

See the upstream [configuration reference](https://github.com/AdguardTeam/AdGuardHome/wiki/Configuration).
The source backup is for recovery and inventory; do not rename it to
`kudurru.yaml` or send it to `kudurru restore`.

## 2. Audit filtering rules before moving them

| Source rule | kudurru v0.1.0 behavior | Migration action |
|---|---|---|
| `\|\|tracker.example^` | Domain and all subdomains | Supported |
| `@@\|\|cdn.example^` | Exception within its own block-list object | Use a separate allow-mode list when it must override other lists |
| `0.0.0.0 tracker.example` or plain `tracker.example` | Exact-name match by default | Use a wildcard/suffix entry if you need subdomains |
| `*.tracker.example` | Matches the apex as well as subdomains | Check that this matches your intent |
| `/^ads[0-9]+\./` | RE2 regular expression, up to 1,000 per list | Check regex compatibility |
| `$important`, `$badfilter`, `$dnstype`, `$client`, `$ctag`, `$denyallow`, `$dnsrewrite` | Rule is counted as unsupported and skipped | Rebuild supported behavior as groups, policies or zone records |
| Hosts lines returning a non-blocking address | Counted as unsupported rewrites | Create explicit zone records where appropriate |
| Cosmetic filters and URL/path rules | Invalid for DNS filtering and skipped | Use a DNS-specific list instead |

**Exception scope changes.** AdGuard Home merges filtering rules; a custom
exception can cancel a block from a downloaded filter. In kudurru, `@@`
in one block-list object only cancels matches from that object, including
its other sources. To make an exception win across the selected lists,
put a positive domain rule in a `mode: allow` list and reference it from
`allowListRefs`.

Inspect both `status.unsupported` and `status.invalid`, plus each source's
fetch status. A successful download does not prove equivalent protection.
Do not treat a partially understood filter as a complete migration.

## 3. Create the baseline lists and policy

Save as `adguard-migration.yaml`. Replace the sample filter URL with your
current enabled sources. This example deliberately puts a global exception
in a separate allow list.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: migrated-filter, namespace: default}
spec:
  mode: block
  sources:
    - name: dns-filter
      url: https://adguardteam.github.io/HostlistsRegistry/assets/filter_1.txt
      format: adblock
    - name: custom-rules
      format: adblock
      inline:
        - '||tracker.example^'
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: migrated-exceptions, namespace: default}
spec:
  mode: allow
  sources:
    - name: global-exceptions
      entries:
        - value: '*.needed.tracker.example'
          type: wildcard
          comment: Allow this endpoint and its subdomains across selected lists
          origin: migrated
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: default, namespace: default}
spec:
  clientGroupRefs: [default]
  blockListRefs: [migrated-filter]
  allowListRefs: [migrated-exceptions]
  blockMode: zeroIP
```

```sh
kudurru apply -f adguard-migration.yaml
kudurru get blocklists -o yaml
kudurru query tracker.example A --client 192.0.2.10
kudurru query needed.tracker.example A --client 192.0.2.10
```

The first name must block; the second must show an allow-list decision.
The allowed `.example` name can still get NXDOMAIN from the upstream because
it has no public DNS record. Repeat the test with real exceptions.

Use the seeded policy's namespace, as explained in
[object ownership](/migrate/cutover/#use-the-right-namespace-and-owner).
Updating this policy replaces its original list references; preserve other
settings you need. Structured entries on custom lists live in their source
entry browser; use **Filtering** > **Allowlist** or **Denylist** for entries
managed through the dedicated domain pages.

## 4. Recreate per-client behavior

For a TV that used its own persistent-client settings, save this as
`adguard-tv.yaml`. Install a suitable [key](/project/licensing/) before
creating the additional policy. Use a reserved address and add the
corresponding IPv6 matcher if the device uses IPv6 DNS.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: living-room-tv, namespace: default}
spec:
  clients:
    - {ip: 192.168.1.40, name: Living room TV}
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: living-room-tv, namespace: default}
spec:
  clientGroupRefs: [living-room-tv]
  blockListRefs: [migrated-filter]
  allowListRefs: [migrated-exceptions]
  upstreamGroup: default
  blockedServices: [tiktok]
  safeSearch: true
```

```sh
kudurru apply -f adguard-tv.yaml
kudurru query tracker.example A --client 192.168.1.40
kudurru query needed.tracker.example A --client 192.168.1.40
```

A specific group's policy does not inherit the default policy's lists.
When multiple policies apply, list references merge and the highest
priority decides individual settings. Client-group matching uses tiers,
so an exact IP takes precedence over a broader subnet group.

Map blocked services by reviewing **Filtering** > **Policies**, rather than
assuming AdGuard's IDs and domain coverage are identical. kudurru ships its
own catalog, supports category exceptions and safe search, and offers an
optional runtime AdGuard service catalog download. See
[blocked services and safe search](/deploy/blocked-services/).

For schedules, use `Policy.spec.schedule` with weekdays, start/end and an
IANA time zone. A policy applies **inside** that window; the default policy
is the fallback when no candidate is active. Recheck both sides of a window
instead of assuming an AdGuard blocking-pause schedule has the same meaning.

## 5. Recreate upstreams, local rewrites and encryption

In **DNS** > **Upstreams**, configure `default`, any client-specific groups,
bootstrap servers and conditional forwarding. If you configure files,
merge this example into the installed `kudurru.yaml` before starting the
server; do not replace its listener, store or credential settings:

```yaml
upstreams:
  groups:
    default:
      servers: ["tls://1.1.1.1", "tls://9.9.9.9"]
      strategy: parallel_best
    lan:
      servers: ["192.168.1.1:53"]
      strategy: strict
  conditional:
    "[/home.arpa/1.168.192.in-addr.arpa/]": lan
  bootstrap: ["1.1.1.1:53", "9.9.9.9:53"]
```

Use the suffixes and private reverse resolver for your actual network; add
IPv6 reverse suffixes when applicable. `parallel_best` is the default;
`strict`, `random` and `rtt_weighted` are alternatives. Preserve deliberate
upstream filtering and ECS choices. Avoid a loop through a router that
forwards DNS back to kudurru. See [upstream settings](/reference/configuration/#upstreams.groups).
Persistent settings-file and environment overrides can take precedence over
the base file; check effective values in the dashboard.

For a local rewrite such as `nas.home.arpa → 192.168.1.10`, create a primary
`home.arpa.` zone and add the record, following
[local zone creation](/migrate/pihole/#4-move-local-names-and-upstreams).
Recreate CNAME and AAAA records too. A primary zone owns the entire suffix
and answers before filtering: it does not preserve arbitrary wildcard,
record-type-specific or per-client AdGuard rewrite behavior. General
`$dnsrewrite` and `Policy.spec.rewrites` processing is deferred in v0.1.0.

Encrypted **upstream** URLs do not enable encrypted **client listeners**.
Configure DoT/DoH/DoQ/DoH3 listeners, certificates and firewall rules using
[encrypted transports](/deploy/docker/#encrypted-transports). Test a client
with its real hostname/path and certificate trust. Do not rely on AdGuard
ClientID paths or SNI values for client matching in this release.

## 6. Validate and switch

Set query-log privacy/retention and check cache sizing before moving traffic.
If AdGuard Home serves DHCP, follow the [DHCP handoff](/migrate/pihole/#dhcp-handoff)
with your saved leases and reservations; keep DNS running during the handoff.

Complete the [validation checklist](/migrate/cutover/#validate), especially
cross-list exceptions, every custom client, local rewrites, private reverse
DNS and actual encrypted endpoints. Then use the
[cutover procedure](/migrate/cutover/#switch-over). Keep AdGuard Home and its
backup until the observation period ends; use
[rollback](/migrate/cutover/#roll-back) if required.
