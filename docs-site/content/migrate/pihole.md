---
title: Move from Pi-hole to kudurru
linkTitle: From Pi-hole
description: Import Pi-hole v6 lists, domain rules, groups, upstreams and local DNS through the dashboard wizard or CLI, then validate before switching clients.
weight: 10
---

Move Pi-hole's adlists, domain rules and local DNS into explicit kudurru
objects. The dashboard provides query explanations and allow/deny actions;
configuration uses the same object format in standalone and Kubernetes
installations. This guide targets Pi-hole v6 concepts and kudurru v0.1.0.

**Before you start:** complete [preparation and CLI setup](/migrate/cutover/#prepare).
Keep Pi-hole serving the network. Use the built-in API importer below to
move supported configuration directly. The later examples show how to
recreate or adjust individual objects. Teleporter archives remain backups
for Pi-hole; kudurru does not restore them.

## Import directly through the API

In **Tools**, choose **Import from DNS server**, select **Pi-hole v6**, and
enter its URL and an **application password** generated in Pi-hole's
**Settings** > **Web interface / API**. The kudurru server must be able to
reach that URL. Review the preview and conversion warnings, then choose
**Import configuration**. No configuration files need to be copied or edited.

Select **Update nameservers during import** to replace apex NS and SOA primary
with kudurru's DNS hostnames, or keep source authority during validation and
use **Migration history → Review run → Decommission source nameservers** later.
CLI imports accept repeatable `--nameserver` flags. Previous API imports can
be tracked without reimporting records. See the
[authority cutover flow](/migrate/cutover/#replace-imported-authoritative-nameservers).

The importer transfers adlist URLs, exact and compatible RE2 domain rules,
enabled switches, domain comments, group assignments, IP/CIDR clients,
upstreams and local A/AAAA/CNAME records under the selected local suffixes.
It uses Pi-hole's configured local domain by default; add other local
suffixes in the wizard if needed. These suffixes become authoritative, so
missing names beneath them stop forwarding upstream. MAC/hostname clients,
Pi-hole regex extensions, DHCP and custom dnsmasq behavior require review.

Pi-hole v6 uses an application password to create a temporary API session,
rather than a permanent v5 API key. kudurru closes that session afterward
and does not save the password. Application passwords can read settings
without enabling Pi-hole's application configuration writes.

The CLI provides the same conversion and a direct apply option. Save the
application password in a protected file, or set `PIHOLE_APP_PASSWORD`:

```sh
kudurru import pihole --source-url http://pi.hole \
  --source-key-file ./pihole-app-password --local-zone home.arpa
kudurru import pihole --source-url http://pi.hole \
  --source-key-file ./pihole-app-password --local-zone home.arpa --apply
```

The first command previews; the second writes to the kudurru server selected
by your CLI context or `--server`. Use `-n` for your default policy's
namespace. Import replaces matching object names, including `Policy/default`,
and the default upstream group. Additional policies require an appropriate
license key. Inspect all results: object writes are not transactional, and
settings are applied only after all objects succeed. A settings result with
`persisted: false` needs a [settings file](/reference/configuration/) to
survive a restart. Complete the validation and cutover checklist afterward.

## Compare the features you use

| Pi-hole | kudurru v0.1.0 | Migration action |
|---|---|---|
| Adlists and group assignments | `BlockList` objects referenced by `Policy` objects | Copy enabled source URLs and record which groups use each list |
| Default group | Seeded `ClientGroup/default` and `Policy/default` | Update the existing policy in its namespace |
| Custom groups | `ClientGroup` plus `Policy` | Reference all baseline lists explicitly; additional policies require a key |
| Clients by IP, network, MAC or hostname | Production matching by IP/CIDR | Reserve addresses and add IPv6 prefixes where needed |
| Exact, wildcard and regex allow/deny | Structured list entries or **Filtering** > **Allowlist** / **Denylist** | Translate regex syntax and extensions before applying |
| Local DNS and CNAME records | Primary zones and records | Create the containing zone, then copy records |
| Conditional forwarding | Named upstream groups and suffix rules, or forward zones | Recreate both local-domain and reverse-DNS routes |
| Temporary blocking pause | Dashboard toggle or `kudurru blocking disable 5m` | Pause is node-local in v0.1.0; check the node you manage |
| DHCP | Opt-in DHCPv4 on Linux with the SQLite or Kubernetes store; no DHCPv6 | Configure kudurru or another DHCP server and preserve leases and reservations before retiring Pi-hole |
| DNSSEC validation | No local validation | Retain a validating upstream if required; this does not add validation to kudurru |
| Pi-hole-specific API integrations | kudurru REST API and CLI | Rewrite integrations; no Pi-hole API compatibility layer in v0.1.0 |
| Teleporter and historical query database | kudurru object backup and separate logs | Keep the source backup/history separately; neither is directly importable |

See the [four-product comparison](/migrate/#feature-comparison) for resolution,
encrypted DNS, multiple nodes and licensing.

## 1. Back up and inventory Pi-hole

1. In Pi-hole's web interface, open **Settings** > **Teleporter** and export
   a backup. Keep the archive untouched for recovery.
2. Record the enabled adlist URLs, their group assignments, allow/deny
   domains, domain types, comments and disabled entries. Preserve the
   original configuration and data volume with a consistent backup as well.
3. Record client/group membership, upstreams, conditional forwarding,
   blocking mode, local A/AAAA and CNAME records, privacy settings and
   retention. Record any custom dnsmasq settings separately.
4. If Pi-hole serves DHCP, save the lease range, active leases, reservations,
   gateway, search domain and lease duration. Plan the [DHCP handoff](#dhcp-handoff).
5. Capture queries for a known allowed name, blocked name, exception, local
   alias and reverse lookup from each group. These become your acceptance tests.

Read the [Pi-hole documentation](https://docs.pi-hole.net/) for the backup
and settings pages in your version. Do not edit the live gravity database
to perform this migration.

## 2. Recreate lists and the default policy

Save this as `pihole-migration.yaml`. Replace the sample source URL with
**your enabled adlist URLs**; add one source for each URL. The inline
`tracker.example` rule gives you a predictable migration test. The allow
entry demonstrates a name that must resolve despite a blocking match.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: migrated-ads, namespace: default}
spec:
  mode: block
  sources:
    - name: adlist
      url: https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
      format: hosts
    - name: custom-domains
      entries:
        - {value: '*.tracker.example', type: wildcard, origin: migrated}
        - {value: disabled.example, type: exact, enabled: false, origin: migrated}
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: migrated-allow, namespace: default}
spec:
  mode: allow
  sources:
    - name: exceptions
      entries:
        - value: needed.tracker.example
          type: exact
          comment: Keep the required endpoint available
          origin: migrated
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: default, namespace: default}
spec:
  clientGroupRefs: [default]
  blockListRefs: [migrated-ads]
  allowListRefs: [migrated-allow]
  blockMode: zeroIP
```

```sh
kudurru apply -f pihole-migration.yaml
kudurru get blocklists -o yaml
kudurru query tracker.example A --client 192.0.2.10
kudurru query needed.tracker.example A --client 192.0.2.10
```

The first test must block. The second must show an allow-list decision;
since `.example` has no public records, an upstream NXDOMAIN is expected
unless you replace it with a real exception. An allow decision does not
create a DNS record.

This updates the seeded default policy and replaces its starter-list
references. Preserve any other settings you already need on that policy.
No additional policy is necessary for a single filtering profile.
If your installation uses another namespace, change the examples as
[described in the checklist](/migrate/cutover/#use-the-right-namespace-and-owner).

### Translate domain rules deliberately

| Pi-hole domain type | kudurru entry | Detail |
|---|---|---|
| Exact allow/deny | `type: exact`, `value: ads.example` | Matches only that name |
| Wildcard allow/deny | `type: wildcard`, `value: '*.ads.example'` | Matches the apex and its subdomains |
| Regex allow/deny | `type: regex`, `value: '^ads[0-9]+\.'` | Uses RE2; test every migrated expression |
| Disabled domain | `enabled: false` | Retains the entry without applying it |

Pi-hole regex extensions such as `;querytype=` and `;reply=` do not carry
over. kudurru counts these extensions as unsupported in regex sources; it
rejects regex constructs that RE2 cannot compile. Do not remove an extension
and assume the broader resulting rule is equivalent. Rebuild its intended
behavior where supported, or retain the original service for that behavior.

Structured entries on your own lists appear in those lists' source entry
browser. The dedicated Allowlist and Denylist pages manage `ui-*` lists;
use those pages if you want the same workflow as Pi-hole's Domains page.
See [allow/deny behavior](/deploy/allowlist/), including per-list exclusions.

## 3. Recreate custom groups

For a group on a kids' VLAN, save the following as `pihole-kids.yaml`.
Install a suitable [key](/project/licensing/) first: a policy beyond
`default` is refused with `license_required` without one. Applying a file
can create some objects before a later object fails; inspect the result.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: kids, namespace: default}
spec:
  clients:
    - {cidr: 192.168.20.0/24, name: Kids VLAN}
    - {ip: 192.168.1.40, name: Reserved tablet address}
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: kids, namespace: default}
spec:
  clientGroupRefs: [kids]
  blockListRefs: [migrated-ads]
  allowListRefs: [migrated-allow]
  blockMode: zeroIP
```

```sh
kudurru apply -f pihole-kids.yaml
kudurru query tracker.example A --client 192.168.20.15
kudurru query needed.tracker.example A --client 192.168.20.15
```

Add group-specific lists to `kids` as needed. The explicit baseline
references matter: this group's policy does not inherit the default
policy. Exact-IP membership also takes precedence over subnet membership;
check overlapping groups and add the corresponding IPv6 matchers.

## 4. Move local names and upstreams

Create the existing local zone in **DNS** > **Zones** > **New zone**, choosing
**Primary**, or save and apply `pihole-local-zone.yaml`:

```yaml
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
kudurru apply -f pihole-local-zone.yaml
kudurru records add home-arpa ns.home.arpa A 192.0.2.54
kudurru records add home-arpa nas.home.arpa A 192.168.1.10
kudurru records add home-arpa files.home.arpa CNAME nas.home.arpa.
kudurru query files.home.arpa A
```

Use your current local suffix and addresses. Add AAAA and PTR records where
you have them. A primary zone answers authoritatively for its entire suffix;
it does not forward missing names to public DNS, and its answers precede
blocking. Do not create a broad public zone merely to override one hostname.

In **DNS** > **Upstreams**, set the `default` group to the upstreams Pi-hole
used. If Pi-hole used a separate Unbound resolver, you can keep it as an
upstream, or set the `default` group's **Mode** to **Recursive**: kudurru
then resolves from the root servers itself, with QNAME minimization and
DNSSEC validation, and needs outbound UDP and TCP port 53
([trade-offs](/start/home/#7-resolve-names-yourself-optional)). Recreate conditional forwarding for your local suffix and the
correct `in-addr.arpa` / `ip6.arpa` reverse suffixes. Do not forward back to a
router whose own DNS forwards to kudurru. See
[zones and upstreams](/guide/admin/07-zones-and-upstreams/).

Map the blocking response deliberately: Pi-hole's NULL response corresponds
to `zeroIP`; `nxdomain`, `refused` and `customIP` are also available. Set
query-log privacy and retention in **Settings** before moving real traffic.

## DHCP handoff

kudurru v0.1.0 includes an opt-in DHCPv4 server on Linux with either store,
with dashboard setup, durable leases and reservations, trusted relays and
optional three-node quorum failover on SQLite. On Kubernetes, one elected
replica serves clients that your router relays to it. Follow the [DHCPv4 deployment guide](/deploy/dhcp/)
to prepare it. You can also keep DHCP on your router or another server.
Keep IPv6 router and DNS advertisements on your router; kudurru does not
serve DHCPv6.

If Pi-hole serves DHCP, prepare kudurru or your chosen DHCP server with
the same subnet, gateway, reservations and search domain. Account for active
leases before choosing the new allocation pool; import leases where
supported or exclude addresses still leased by Pi-hole. kudurru supports
dnsmasq lease import; recreate reservations separately. Set kudurru as DNS.

Disable Pi-hole's DHCP service before enabling the replacement on the same
LAN. Keep Pi-hole's DNS running during this handoff. Renew one client's lease
and confirm its address, gateway, search domain and DNS. Recreate any dynamic
local names previously derived from Pi-hole leases using kudurru's optional
lease A/PTR records in a dedicated DHCP domain, the other DHCP server's DNS
integration, conditional forwarding or explicit records.

## Switching over

Complete the [validation checklist](/migrate/cutover/#validate), testing at
least one default client and each custom group. Include UDP/TCP, A/AAAA,
exceptions, local aliases and reverse DNS. Then follow
[network cutover](/migrate/cutover/#switch-over), including IPv6 DNS
advertisements. Keep Pi-hole, its Teleporter backup and the saved DHCP plan
available until the observation period ends. If checks fail, follow
[rollback](/migrate/cutover/#roll-back).
