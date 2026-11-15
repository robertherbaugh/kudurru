---
title: Move from Technitium to kudurru
linkTitle: From Technitium
description: Import Technitium primary zones, blocking and forwarders through the dashboard wizard or CLI, review unsupported features, and validate before a DNS cutover.
weight: 30
---

Technitium combines authoritative DNS, recursive resolution, blocking, DHCP,
clustering and DNS Apps. kudurru v0.2.0 covers primary, secondary and stub
zones, RFC 2136 updates, forwarding, validating recursion, filtering and a
dashboard, with native Kubernetes objects. Start by deciding
which Technitium responsibilities can move and which need to stay elsewhere.

## Import directly through the API

In **Tools**, choose **Import from DNS server**, select **Technitium**, and
enter its console URL and an API token created in Technitium. Review the
preview and warnings, then choose **Import configuration**. The kudurru
server reads the source API and writes supported configuration directly;
no backup files or hand-written manifests are needed.

The review step lets you select individual objects and settings sections, with
**Select all** and **Clear selection** controls. Select referenced dependencies
too, or make sure they already exist. Starting the import opens a migration run
page with live progress. You can leave or close the browser: the server continues
in the background. Return through **Tools → Migration history → Review run**.
The report shows a green check for successful items, a red X for errors and full
error details. Unselected items are recorded as skipped. A failed item does not
roll back successful changes; selected settings wait until every selected object
succeeds. Imports interrupted by a server restart are not resumed automatically;
check any unconfirmed outcomes before retrying.

Select **Update nameservers during import** to replace apex NS and SOA primary
with kudurru's DNS hostnames, or keep source authority during validation and
use **Migration history → Review run → Decommission source nameservers** later.
CLI imports accept repeatable `--nameserver` flags. Previous API imports can
be tracked without reimporting records. See the
[authority cutover flow](/migrate/cutover/#replace-imported-authoritative-nameservers).

The importer transfers allowed/blocked names (including subdomains), list
subscriptions, supported default forwarders and unsigned primary zones
through Technitium's standard zone export API. Primary zones in a cluster
or catalog are imported as independent primary zones. The importer reads
the catalog's query access mode and ordered network ACL when the member
inherits them, and preserves a member's explicit override. Catalog membership
and replication are not migrated. Use a token with access to Settings,
Allowed, Blocked, Zones, each zone to migrate and any catalog supplying its
query access policy; reading zone options also requires the
Zones Modify permission in Technitium. Tokens are used as bearer credentials
and are never stored by kudurru. The API flow was verified with Technitium
15.6.1; older versions without bearer-token authentication are not supported.

Disabled and signed zones, non-primary zones and zones containing disabled,
expiring or app-specific records are skipped with warnings. This avoids
enabling disabled records, publishing private
app data or silently dropping DNSSEC. A member is also skipped if its
effective query access policy cannot be read or converted; local defaults
are never substituted for an unreadable inherited policy. Catalog zones
themselves remain unsupported. Blank lines and comments beginning with `#`
in list subscriptions are ignored, including those following an allow-list
`!` prefix. Zone transfers, NOTIFY, dynamic updates, DNS Apps,
Advanced Blocking profiles, outbound proxies and conditional forwarders require separate
review: the importer does not move them, even where kudurru has an equivalent
feature.
Keep those services on Technitium until their replacements are ready.

The CLI supports previews and direct imports too. Save the token in a
protected file, or set `TECHNITIUM_API_TOKEN`:

```sh
kudurru import technitium --source-url http://dns.local:5380 \
  --source-key-file ./technitium-token
kudurru import technitium --source-url http://dns.local:5380 \
  --source-key-file ./technitium-token --apply
```

The first command previews; `--apply` starts a background import on your selected kudurru CLI
context and returns its run ID. Add `--wait` to wait for the final report, or run
`kudurru import runs RUN_ID` later. Use `-n` for the namespace of your existing default policy. Matching
object names and the default upstream group are replaced. Read every result:
the import can partially succeed across objects, and settings change only after
every selected object succeeds. On SQLite, each object commits with its durable
result; selected settings commit with the final report in one transaction.
Settings errors leave settings unchanged. Review any restart requirements in
the Settings result before restarting the destination. Use repeatable
`--select-object Kind/[namespace/]name` and
`--select-setting SECTION` flags to import only those items, or `--skip-settings`
to import objects while leaving settings unchanged. Complete the validation and cutover checklist afterward. The
sections below cover manual adjustments and features needing a separate plan.

**Before you start:** complete [preparation and CLI setup](/migrate/cutover/#prepare).
Keep Technitium serving the network. Its server backups, cluster state and
DNS Apps are not importable. Standard primary-zone files are the supported
interchange format.

## Compare the features you use

| Technitium | kudurru v0.2.0 | Migration action |
|---|---|---|
| Primary zones and standard DNS records | Primary `DNSZone` and `DNSRecordSet` | Export zone files, import and compare records |
| Conditional Forwarder zones / `FWD` behavior | Forward `DNSZone` or suffix-to-upstream-group rule | Recreate forwarding explicitly; `FWD` is not a portable record |
| Full recursive resolution | Upstream group with `mode: recursive` (QNAME minimization) | Set `default` (or a Policy's group) to recursive; allow outbound UDP/TCP 53 |
| DNSSEC validation and signing | Validation in recursive groups; validation of forwarded answers when you turn it on; no signing | Retain a signing service for signed zones; turn on [`dnssec.enabled`](/deploy/dnssec-validation/) if forwarded answers must be validated |
| Secondary and stub zones | Secondary `DNSZone` (AXFR/IXFR in over TCP or TLS, TSIG, NOTIFY in) and stub `DNSZone`; every node transfers or discovers for itself | Create them by hand, as in [section 3](#3-recreate-secondary-and-stub-zones): the importer skips them. Allow every kudurru node in the primary's transfer ACL |
| Catalog zones | Not served | Keep the existing service for catalog distribution. The `catalog` and `secondaryCatalog` types are in the schema only: [do not apply them](/migrate/#feature-comparison) |
| Zone transfer and NOTIFY from a primary | Outbound AXFR with ACLs and TSIG, and NOTIFY; an IXFR request for an older serial gets the full zone | Recreate ACLs, keys and destinations |
| RFC 2136 dynamic updates | Primary zones accept TSIG-signed updates over UDP, TCP and DoT, under grants by key, name and record type | Create a key and an update policy, then point the updater at kudurru: [dynamic updates](/deploy/dynamic-updates/). Technitium's update ACLs do not transfer |
| Allowed/blocked zones and URL lists | Allow/block `BlockList` objects and policies | Convert entries and attach lists explicitly |
| Advanced Blocking DNS App | Client groups, policy lists, schedules, service blocking and safe search | Rebuild supported behavior; app configuration is not imported |
| `APP`, `ANAME`, split-horizon or other app-specific behavior | No Technitium app runtime. A policy can rewrite forwarded answers for its client groups ([response processing](/deploy/response-processing/#per-policy-choices-and-rewrites)); answers from kudurru's own zones are the same for every client | Replace each dependency explicitly before moving affected zones |
| DHCP | Opt-in DHCPv4 on Linux with the SQLite or Kubernetes store; no DHCPv6 | Configure scopes and reservations, preserve active leases and review lease DNS behavior separately |
| Clustering | Kubernetes shared store, an enrolled SQLite cluster (one primary and its followers), or file-managed Compose HA | Choose a kudurru deployment; Technitium cluster membership does not transfer |
| OIDC, LDAP, local users and permissions | Local users, OIDC and scoped roles; no native LDAP | Recreate credentials and grants; OIDC/scoped roles require a key |

Technitium already offers clustering, OIDC and advanced authoritative DNS.
Moving to kudurru trades some of that breadth for its object model,
Kubernetes integration and operating workflow. Compare against your actual
dependencies; the presence of a field in a CRD does not make a feature
operational. See the [full comparison](/migrate/#feature-comparison)
and [Technitium's feature list](https://technitium.com/dns/).

## 1. Back up and classify the workload

1. Use Technitium's backup function to save the server configuration, zones,
   allowed/blocked data, app settings and any DHCP data you need. Retain
   certificates and TSIG material securely for recovery.
2. List every zone with its type, records, query/transfer/update ACLs,
   NOTIFY destinations, DNSSEC state and dependent clients or secondaries.
3. Export each primary zone in standard zone-file format from the web
   console. Keep the original exports unchanged and work on copies.
4. List DNS Apps and records that invoke them. Record the behavior each app
   provides, including blocking, split-horizon answers, aliases or DNS64.
5. Record forwarders, recursive settings, blocked-list URLs, allowed/blocked
   zones, client subnets, encrypted endpoints, users and cluster membership.

Classify each item as **move now**, **retain as a separate service** or
**defer the migration**. Do not change a public delegation or a signed zone
until you have a plan for DNSSEC and all authoritative dependencies.

## 2. Import an unsigned primary zone

Start with a private, unsigned primary zone such as `home.arpa.`. An export
must be a standard zone file, not Technitium's binary backup or internal
zone files. If your installed version cannot export the required zone in
that format, recreate its records through the dashboard or API instead.

Inspect a working copy of the export. Standard records such as A, AAAA,
CNAME, MX, TXT, SRV and PTR are portable. `APP`, `FWD`, `ANAME` and other
Technitium-specific records need explicit replacements; they do not become
working app behavior when placed in a zone file. Preserve unsupported data
in your source backup and resolve the dependency before omitting it.

A minimal test file, `home.arpa.zone`, looks like this. Substitute your real
records and addresses; the server address is a documentation placeholder.

```dns
$ORIGIN home.arpa.
$TTL 300
@ IN SOA ns.home.arpa. hostmaster.home.arpa. (
  2026101501 3600 600 86400 300
)
@     IN NS    ns.home.arpa.
ns    IN A     192.0.2.54
nas   IN A     192.168.1.10
files IN CNAME nas.home.arpa.
```

Import into a **new** object name, allowing the CLI to create the primary
zone with `--origin`:

```sh
kudurru import bind home-arpa -f home.arpa.zone --origin home.arpa.
kudurru zones get home-arpa
kudurru records list home-arpa
kudurru zones export home-arpa --out home.arpa.kudurru.zone
kudurru query nas.home.arpa A
kudurru query files.home.arpa A
```

Read the import summary and every warning. If a command directly after the
import answers `unavailable`, run it again
([apply results](/migrate/cutover/#apply-results)). The
[Technitium export API](https://github.com/TechnitiumSoftware/DnsServer/blob/master/APIDOCS.md#export-zone)
documents the standard zone-file export format. Compare the exported result
against your source by owner name, type, targets, TTL, apex SOA and NS;
text order and serial formatting may differ. A re-import replaces that import's record-set chunks, but leaves records
owned by other record sets in place and does not update an existing zone's
SOA/NS configuration. Review existing records before importing again.
There must be only one active zone object for each apex.

Set the zone's query/transfer/NOTIFY policy separately in the dashboard or
manifest. An imported zone file does not carry Technitium ACLs, permissions,
TSIG secrets, app settings or DNSSEC key state. Configure a valid NS address
and serial/NOTIFY behavior before using kudurru as a replacement primary for
existing secondaries. Test transfers from those real secondary clients.

Reverse zones migrate as primary zones too: export the existing
`in-addr.arpa.` or `ip6.arpa.` zone with its PTR records and import it using
its real apex. A local A/AAAA record does not automatically create a PTR.

### Signed and public zones

kudurru cannot sign a zone or maintain a DNSSEC key lifecycle.
Zone import skips DNSSEC records with warnings; it does not migrate
signing state. Keep signed
authority on a signer, or plan a separate DNSSEC transition that accounts
for parent DS records and cached signatures before changing authority.
kudurru can serve a signed zone as a [secondary](#3-recreate-secondary-and-stub-zones)
of that signer: it serves the transferred signatures and does not sign.
A public move also requires reachable UDP/TCP port 53, correct NS/glue,
delegation and tested transfers; a LAN DHCP change alone does not move a
public authoritative zone.

## 3. Recreate secondary and stub zones

The importer skips Technitium's secondary and stub zones. Create each one in
**DNS** > **Zones** > **New zone**, or apply a manifest. Save this as
`technitium-secondary.yaml`, with the apex and the address of the server that
stays primary for the zone:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: partner-example}
spec:
  zoneName: partner.example.
  type: secondary
  secondary:
    primaries: ["192.0.2.53"]
```

```sh
kudurru apply -f technitium-secondary.yaml
kudurru zones get partner-example
kudurru query www.partner.example A
```

Every kudurru node transfers the zone for itself, so allow the address of
each node in the primary's transfer ACL, and send NOTIFY to the nodes if you
want changes to arrive before the next SOA refresh. Add
`tsigKeyRef: <key name>` when the primary requires a TSIG key that the nodes
know, and `transferProtocol: tls` for a transfer over TLS. A node answers
SERVFAIL for the zone until its first transfer succeeds. Later it keeps its
last copy until the zone's SOA expiry and then answers SERVFAIL again until
a transfer succeeds. Records of a secondary are read-only in kudurru.

A stub zone has `type: stub` and `stub: {nameservers: ["192.0.2.53"]}`
instead: each node discovers the zone's name servers from those bootstrap
servers and sends queries for the zone directly to them. See
[zones and upstreams](/guide/admin/07-zones-and-upstreams/#create-a-secondary-zone)
for both types.

## 4. Recreate conditional forwarding

Create the upstream group first. For file configuration, merge this into
the installed `kudurru.yaml` before startup, retaining its other settings;
or use **DNS** > **Upstreams** in the dashboard:

```yaml
upstreams:
  groups:
    default:
      servers: ["9.9.9.9:53", "1.1.1.1:53"]
      strategy: parallel_best
    corp:
      servers: ["192.168.50.10:53", "192.168.50.11:53"]
      strategy: strict
```

Replace these with your approved resolvers. If you depended on Technitium
for full recursion, make `default` recursive instead and keep the `corp`
group for the private suffix:

```yaml
upstreams:
  groups:
    default: { mode: recursive, timeout: 4s }   # no servers: resolves from the root, validates DNSSEC
    corp:
      servers: ["192.168.50.10:53", "192.168.50.11:53"]
      strategy: strict
```

Recursion needs outbound UDP and TCP port 53 to any address. kudurru
validates DNSSEC only in recursive groups; a forwarding group passes on what
its upstream returns, so an upstream validator enforces its own policy. In
either mode, a SERVFAIL that carries a DNSSEC extended DNS error (6 to 12)
is logged as `dnssec_bogus`. See
[resolving names yourself](/start/home/#7-resolve-names-yourself-optional)
for the privacy trade-offs.

Save this as `technitium-forward.yaml` for a private corporate suffix:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: corp-example}
spec:
  zoneName: corp.example.
  type: forward
  forward:
    upstreamGroup: corp
```

```sh
kudurru apply -f technitium-forward.yaml
kudurru query host.corp.example A
```

Alternatively, use `upstreams.conditional` for suffix-to-group rules.
Recreate reverse-DNS suffix routes as needed. A Technitium `FWD` record
with special proxy, DNSSEC or app behavior needs separate evaluation;
a forward zone only recreates the supported forwarding path. Avoid loops
through the old server. See [upstreams](/guide/admin/07-zones-and-upstreams/).

## 5. Move allowed and blocked names

Allowed/blocked zones are filtering rules, not authoritative DNS zone
exports. Recreate them as list entries. The wildcard form below covers the
apex and descendants; use `type: exact` for a single name.

Save as `technitium-filtering.yaml` and add your existing enabled list URLs
as additional sources on `migrated-blocked`:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: migrated-blocked, namespace: default}
spec:
  mode: block
  sources:
    - name: blocked-zones
      entries:
        - {value: '*.tracker.example', type: wildcard, origin: migrated}
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: migrated-allowed, namespace: default}
spec:
  mode: allow
  sources:
    - name: allowed-zones
      entries:
        - {value: '*.needed.tracker.example', type: wildcard, origin: migrated}
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: default, namespace: default}
spec:
  clientGroupRefs: [default]
  blockListRefs: [migrated-blocked]
  allowListRefs: [migrated-allowed]
  blockMode: zeroIP
```

```sh
kudurru apply -f technitium-filtering.yaml
kudurru get blocklists -o yaml
kudurru query tracker.example A --client 192.0.2.10
kudurru query needed.tracker.example A --client 192.0.2.10
```

If `kudurru apply` refuses every document with `unavailable: current
writable serving generation unavailable` or `generation_pending`, nothing was
applied: run it again after a few seconds
([apply results](/migrate/cutover/#apply-results)). A list whose URL kudurru
cannot download, or does not accept as a list, is stored and blocks nothing
until a download succeeds: `kudurru lists list` shows `0` entries for it and
`kudurru get blocklists -o yaml` shows the download error under `Degraded`
([what counts as a failed download](/migrate/#feature-comparison)).

The first query must block: its `kudurru status` line names the list and the
rule. The second must not: the allow list wins, so the status is `forwarded`
or `cached` and names no list.
Its upstream can still return NXDOMAIN because `.example` names have no
public records. Check real allowed names too. Match Technitium's configured
block response deliberately instead of assuming the default is identical.

These examples replace the seeded default policy's starter references.
Use the actual [namespace and ownership path](/migrate/cutover/#use-the-right-namespace-and-owner)
for your installation. Authoritative answers from primary zones precede
filtering; check any overlap between local authority and blocked names.

For Advanced Blocking app profiles, create an IP/CIDR `ClientGroup` and
`Policy` for each supported profile, following the
[group example](/migrate/pihole/#3-recreate-custom-groups). Install a suitable
[key](/project/licensing/) before adding policies. Include all baseline
lists explicitly: custom policies do not inherit the default policy.
Translate regular expressions to RE2 and inspect skipped rules. Rebuild
service blocks and safe search with the
[policy catalog](/deploy/blocked-services/); do not copy DNS App configuration.

## 6. Rebuild operating dependencies

- **DHCP:** move service, active leases, reservations and dynamic local-name
  behavior using the [DHCP handoff](/migrate/pihole/#dhcp-handoff). Keep DNS
  running throughout the handoff.
- **Access:** create new users/tokens and least-privilege grants. Configure
  OIDC if needed; LDAP requires an external identity-provider bridge.
  Source credentials and grants do not migrate from the backup.
- **Encrypted clients:** configure listeners and certificates using
  [encrypted transports](/deploy/docker/#encrypted-transports), then test
  the actual client hostname, path, trust and protocol.
- **Multiple servers:** choose [Helm](/deploy/helm/) for shared CRDs, an
  [enrolled SQLite cluster](/deploy/sqlite-cluster/) for one primary with
  followers that copy it, or [Compose HA](/deploy/compose-ha/) for
  file-managed independent nodes. SQLite nodes that are neither enrolled nor
  managed from the same files do not synchronize.
- **Automation:** replace Technitium-specific API calls with the
  [kudurru API/CLI](/start/api-cli/). An updater that speaks RFC 2136 can keep
  the protocol with a TSIG key and a grant in the zone
  ([dynamic updates](/deploy/dynamic-updates/)). Retain existing services for
  DNS App and catalog-zone responsibilities.

## 7. Validate and switch

Complete the [validation checklist](/migrate/cutover/#validate). In addition,
compare SOA/NS, every record type in use, wildcard answers, negative answers,
PTR zones, conditional forwarding and authorized/unauthorized query access.
Test transfers and NOTIFY if secondaries depend on this primary. Confirm
there is a working replacement for each DNS App and other retained service.

For LAN resolver clients, use the [cutover procedure](/migrate/cutover/#switch-over)
and keep Technitium and its backup available for
[rollback](/migrate/cutover/#roll-back). Move public authority separately,
using your delegation and DNSSEC plan. Retire Technitium only when its
remaining authoritative, recursive, DHCP and app responsibilities have all
been accounted for.
