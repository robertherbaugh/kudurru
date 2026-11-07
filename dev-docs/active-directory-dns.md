# v0.2: replace Windows DNS in a single-domain AD forest

**Status:** scope agreed with the project owner on 2026-10-15; functional
outline for implementation planning. This document describes requirements,
not features that already work. See decision D222 in the
[design record](design-record.md#16-decision-log).

## 1. Outcome and supported topology

kudurru becomes the authoritative DNS server and normal resolver for an
existing single-domain Active Directory Domain Services (AD DS) forest.
After migration, the Windows DNS service can remain stopped on every domain
controller (DC). All migrated records are stored and maintained in kudurru;
serving them does not depend on Windows DNS or AD's DNS directory partitions.

The initial topology includes multiple writable DCs, global catalog servers,
multiple AD sites and subnets, domain members, Windows DHCP registrations,
and IPv4/IPv6. A single domain does not imply a single DC or site. Exact
Windows Server and client versions require a tested support matrix.

AD DS continues to supply directory services, Kerberos, LDAP, Group Policy,
SYSVOL, and directory replication. DNS answers tell clients where those
services are; their traffic goes directly to the DCs. Kudurru may use AD for
its DNS service identity and update authorization, but takes over DNS storage,
replication, and administration. DHCP remains a separate service.

In the completed deployment there is no forwarding back to a DC for the
migrated AD zones, including `_msdcs` and reverse zones. Ordinary forwarding
to public resolvers or explicitly configured external namespaces continues.

## 2. Functional inventory

Every row is part of the replacement goal. Existing DNS primitives provide
a foundation; each still needs validation with real Windows systems.

| ID | Function | Required behavior |
|---|---|---|
| AD1 | Discovery and assessment | Inventory every hosted zone, delegation, DNS server, registration source, and relevant Windows DNS setting; report migration blockers. |
| AD2 | Import and ownership handoff | Import records and available metadata, compare with the source, perform a final sync, and transfer write ownership to kudurru. |
| AD3 | Authoritative service | Serve domain, forest locator, reverse, and other migrated zones with correct SOA, NS, glue, positive and negative answers, and UDP/TCP behavior. |
| AD4 | AD service discovery | Serve DC locator, Kerberos, LDAP, global catalog, PDC, site-specific SRV, DC GUID CNAME, and associated address records. |
| AD5 | Dynamic update protocol | Accept RFC 2136 prerequisites and atomic updates, including adds, deletes, replacements, and refreshes. |
| AD6 | Secure Windows registration | Accept Windows Kerberos/GSS-TSIG updates, with TKEY negotiation, server identity provisioning, credential rotation, and signed responses. |
| AD7 | Update authorization | Limit authenticated machines, DCs, DHCP identities, and administrators to permitted names, types, and operations; preserve record ownership safely. |
| AD8 | DHCP interoperability | Support client/server registration responsibilities, forward/reverse records, lease changes, cleanup, shared DHCP credentials, and name-protection behavior. |
| AD9 | Aging and scavenging | Track dynamic record refreshes, protect static records, and preview and audit removal of stale data. |
| AD10 | Replication and recovery | Persist accepted updates, distribute records and security metadata, support secondary transfers, fence writers, and recover through restart and failover. |
| AD11 | DNS administration and diagnostics | Expose records, ownership, refresh state, update permissions, registration failures, migration state, backup, and restore through kudurru tools. |
| AD12 | Cutover and rollback | Change authority and resolver settings in a controlled sequence, verify AD behavior with Windows DNS stopped, and reconcile changes if rolling back. |

## 3. Zones, records, and discovery

Use `corp.example` as the example forest-root domain. Assess and migrate:

- The domain's forward zone, its apex records, and any other hosted primary
  zones needed by the environment.
- `_msdcs.corp.example` if separately hosted, or its records inside the
  domain zone if that is the existing arrangement. Preserve the actual zone
  boundaries and delegations.
- IPv4 `in-addr.arpa` and IPv6 `ip6.arpa` reverse zones, including delegated
  reverse ranges and glue where applicable.
- Service registrations beneath `_tcp`, `_udp`, `_sites`, and `_msdcs`:
  domain-wide and site-specific LDAP/Kerberos discovery, global catalog and
  PDC discovery, DC GUID aliases, and their A/AAAA targets.
- Application and infrastructure records such as MX, TXT, CNAME, SRV, and
  supported uncommon types; preserve TTLs, SRV priority/weight/port, and
  multiple values. Preserve unknown standard wire types where supported.

Netlogon remains responsible for registering DC service records. Kudurru
accepts those registrations rather than generating a fixed list from a
one-time directory snapshot. DC promotion, demotion, address changes, site
changes, and global catalog/PDC role changes must keep working.

Discovery needs an operator-assisted export path and may add authenticated
remote discovery. Inventory domain/forest DNS directory partitions and
legacy storage where used. Capture zone settings, forwarders, conditional
forwarders, update permissions, aging settings, and dependencies on Windows
management tools. A DNS `ANY` query is not a record inventory mechanism.

Assess Windows DNS policies/views, DNSSEC signing, WINS/WINS-R behavior,
custom ACLs, secondary/stub zones, and special zones separately. Do not
silently flatten views, copy trust-anchor configuration as ordinary zone
data, or claim unsupported behavior was migrated. Existing DNSSEC signing
plans remain v1.0; a signed source zone needs a supported transition before
cutover. Feature gaps block migration of affected configurations rather
than leaving a hidden Windows DNS dependency.

## 4. Import and the transition to writable authority

Support AXFR when explicitly permitted and a Windows export path when it is
not. Zone transfers and text exports provide record data, not a complete
copy of AD security descriptors, ownership, aging timestamps, or DNS server
configuration. Structured exports can supply additional metadata; report
exactly what was obtained and what needs operator review.

The importer must provide a dry run, normalized record comparisons,
conflict reports, repeatable synchronization, and a rollback checkpoint.
Validate zone boundaries and data before publishing a complete snapshot.
Ignore only explicitly explained differences, such as SOA/NS changes at
cutover. Do not accept a partial import as an empty authoritative zone.

Define the handoff from importer-managed data to dynamic registration and
normal administration. The existing `managedBy` rule cannot leave imported
records permanently unwritable by their legitimate registrants. Conversely,
an import or later UniFi/Kubernetes sync must not overwrite a machine-owned
name. Import provenance and DNS update ownership are separate concepts.

Use an explicit write transition and final reconciliation to avoid changes
being lost while clients switch servers. A Windows secondary or an AXFR copy
is not a multi-writer replica. Do not assume updates received by a Windows
server during overlap automatically reach kudurru.

## 5. Secure updates and permissions

RFC 2136 support must implement prerequisite checking and atomic changes
within a zone. A multi-record update either commits in full or changes
nothing. Forward and reverse zones are separate transactions; retry and
partial-success behavior must reflect that boundary. Successful updates
invalidate affected caches and become visible through the serving snapshot.

Windows registration requires a **server-side Kerberos/GSS-TSIG acceptor**,
including TKEY negotiation and signed update replies. Shared HMAC-TSIG keys
alone do not satisfy this requirement. Refusing an initial unsigned update
must allow a legitimate Windows client to retry securely; unsigned writes
remain disabled by default.

Provision DNS service principal names (SPNs) and secret-referenced key
material for the names clients actually contact: the SOA primary, published
server names, and any service aliases/VIP names. Define SPN uniqueness,
Kerberos realm/KDC discovery without circular DNS dependencies, supported
encryption types, key versions and rotation, clock requirements, and
credential recovery. A KDC outage must not stop ordinary DNS answers.

Authorization must distinguish:

- Domain members managing authorized hostnames and associated address data.
- DC identities maintaining their own and legitimately shared locator data,
  including records with contributions from multiple DCs.
- DHCP identities managing registrations on behalf of clients, including
  multiple DHCP servers using a shared registration account.
- Administrators managing static/reserved names and approved exceptions.

Authentication alone must not permit arbitrary zone writes. Model stable
principal identity, name/type/operation constraints, first registration,
replacement, deletion, name reuse, and transfer of ownership. Cross-host,
cross-zone, and reserved-name changes must be refused unless authorized.
Review ownership at the DNS name and record/value level where shared names
require it; a permission to add one DC's value must not grant removal of
another DC's contribution.

Migrating AD ACLs requires a documented translation or an explicitly
reviewed replacement policy. AXFR cannot supply those ACLs. Do not widen
permissions silently or let the first post-import writer claim protected
records. DNS update authorization is distinct from dashboard/API RBAC.

## 6. DHCP, aging, and record lifecycle

Windows DHCP and direct Windows client registration both belong in the
initial test matrix. Exercise FQDN option 81 negotiation, the resulting
client/server split of A/AAAA and PTR updates, DHCP credentials, lease
renewal, address changes, lease release/expiration, and cleanup. Test
configured DHCP failover and shared ownership. Detect name protection and
support its DHCID/prerequisite behavior before migrating an installation
that uses it. DHCP credentials do not imply blanket ownership of static or
DC records.

Track static versus dynamic state, last permitted refresh, no-refresh and
refresh intervals, and eligibility for scavenging. A successful same-data
refresh can matter to aging even when the RRset and SOA serial do not
change. TTL expiration removes cached answers; it does not delete the
authoritative record.

Static/imported records must not become eligible for deletion merely
because timestamps were unavailable. Adopt imported dynamic records through
reviewed metadata or a controlled grace/adoption process. Preserve Windows
static-record behavior deliberately, and document any approved differences.
Expose a scavenging preview, exclusions, audit, and recovery. Apply one
consistent lifecycle policy across replicas so stale data cannot reappear
after failover or restore.

## 7. Storage, replication, and operations

The AD integration handles discovery, assessment, import, and migration
coordination. Ongoing authoritative answers and secure dynamic updates are
DNS server capabilities. Disabling the importer after handoff must not stop
registrations or restore AD as the source of truth.

Normal answers continue to use in-memory snapshots without a live store,
LDAP, or KDC lookup. Update authentication, authorization, persistence, and
replication form a separate write path.

An update success response means the change is durably committed according
to the advertised deployment's recovery guarantee. Define that guarantee
before shipping: local persistence does not imply that asynchronous follower
promotion preserves every acknowledged update. Replicate ownership, aging,
and authorization metadata as well as record content; fence stale writers
and prevent rollback to older security state. Measure registration bursts,
replica lag, and convergence after updates.

The kube store has no multi-object transaction for RFC 2136. The design
record also reserves large/imported zone data outside CRDs. Resolve atomic
updates, writable-zone storage, replication, and backup for each advertised
deployment; placing records in per-pod `local.db` alone does not establish a
durable authoritative source. A mode is supported for AD replacement only
after its write and recovery semantics pass the acceptance gates.

Publish a reachable update target through SOA/NS and define follower
behavior for update/TKEY traffic. GSS contexts must survive the chosen
routing or fail with a recoverable renegotiation; blindly forwarding a
signed request under another server identity is insufficient. Validate
dnsdist, VIP, TCP retry, and failover behavior as deployed. Standard
AXFR/IXFR and NOTIFY replicate DNS data to secondaries; they do not carry
kudurru's ownership and security metadata.

Administration needs migration previews, record editing, registration
history, owner and aging inspection, permission explanations, and backup /
restore including security and lifecycle state. Diagnostics distinguish
reachability, missing SPNs, clock skew, authentication failure, denied
ownership, failed prerequisites, and persistence/replication failure.
Log auditable identities and changes without key material or Kerberos
tokens. Windows DNS Manager/RPC tooling is not automatically compatible;
identify and replace dependent administrative scripts.

## 8. Migration sequence and rollback

1. **Assess and checkpoint.** Inventory topology and settings, export zone
   data and available metadata, identify unsupported features, and save a
   recoverable source configuration. Produce a zone-by-zone plan.
2. **Shadow and compare.** Load copies into kudurru without changing client
   authority. Compare record sets and representative answers, including
   locator, delegation, reverse, and negative answers. Keep imports current.
3. **Prove registrations.** Provision service identities and permissions;
   exercise real DC, member, and DHCP updates against controlled test zones
   or an isolated lab. Validate the writer and failover path.
4. **Transfer authority.** Quiesce or otherwise account for concurrent
   source writes, reconcile final changes, adopt ownership, and change
   SOA/NS/delegations, DHCP resolver settings, DC NIC settings, and static
   client settings. Account for cached data and DNS settings. Temporary
   forwarding/delegation to explicitly retained child zones is allowed
   during migration, with visible remaining dependencies.
5. **Verify with Windows DNS stopped.** Exercise the acceptance matrix
   with all DC DNS services stopped and firewall/log evidence that DNS
   queries and updates are not falling back to them. Reboot DCs and clients
   and test fresh registrations as well as cached logons.
6. **Retire and monitor.** Disable the retired DNS services after the
   verification window, remove stale DNS server references, and monitor
   registration and AD health. Keep the tested rollback material.

At every phase expose which server owns writes. Never forward arbitrary
missing names from a locally authoritative zone back to Windows DNS.

Rollback after cutover must preserve updates accepted by kudurru: stop or
fence writes, export/reconcile the changed records and applicable ownership
into the chosen Windows authority, then restore authority and resolver
settings. Restarting an old Windows DNS copy alone is not a valid rollback.

## 9. Acceptance gates

Run these against real Windows DCs, members, and DHCP; protocol unit tests
and a Samba-only lab are insufficient to claim Windows compatibility.
Record tested OS versions, topology, deployment mode, workload, convergence
time, and recovery behavior. All functional gates run with Windows DNS
stopped on every DC after cutover.

| Gate | Evidence required |
|---|---|
| Authority | Domain, `_msdcs`, reverse, and other migrated zones answer correctly; no DNS forwarding/update proxy to the DCs remains. |
| Domain operations | Fresh domain join, uncached logon, password change, secure-channel verification, and GPO/SYSVOL access succeed. |
| DC discovery | DC locator returns correct LDAP/Kerberos/GC/PDC and site-specific results; GUID aliases resolve; AD replication health remains good. |
| DC lifecycle | DC reboot, service re-registration, promotion without installing Windows DNS, demotion, site changes, GC changes, and PDC role transfer maintain correct registrations and cleanup. |
| Member lifecycle | Boot, forced registration, rename, address change, multihoming, and configured IPv4/IPv6 paths work without stale or unauthorized replacements. |
| DHCP | Negotiated client/server updates, credential use, forward/reverse registration, renewal, release/expiration, failover, and enabled name protection behave as specified. |
| Security | Correct identities succeed; unsigned, cross-host, protected-name, replayed, tampered, and stale-key updates fail appropriately; credential rotation and expired-context recovery work. |
| Atomicity | Failed prerequisites, authorization, or persistence leave no partial record changes; retries do not corrupt ownership or shared locator records. |
| Aging | Valid refreshes preserve dynamic records, static records stay protected, stale records are removed only when eligible, and preview matches execution. |
| Availability | Restart, writer loss, VIP/backend change, KDC outage, store outage, and fenced promotion meet documented answer/update and data-loss guarantees. |
| Recovery | Backup/restore and rollback retain post-cutover changes and their ownership/lifecycle state without enabling competing writers. |
| Diagnostics and scale | Windows tools such as `dcdiag /test:dns`, `repadmin`, `nltest`, and forced registration provide expected results; tool-specific Windows-DNS assumptions are explained; measured registration bursts meet published limits. |

## 10. Implementation order and unresolved design

Started 2026-10-22: the isolated [GSS-TSIG probe](spikes/ad-dns/README.md)
implements the first protocol experiment, with an ephemeral MIT Kerberos
realm and independent `nsupdate` client over UDP/TCP. It verifies identities
and signs refusals; it does not accept or persist changes. Real Windows
DC/member/DHCP qualification, SPN rotation and deployment routing remain
open. There is no Windows lab available yet.

1. **Windows interoperability spike:** prove server-side GSS-TSIG/TKEY,
   SPN/key provisioning and rotation, and Netlogon/member/DHCP registrations.
   Confirm dependency licensing and `CGO_ENABLED=0` compatibility. This is
   a feasibility gate, not an already selected library or implementation.
2. **Write-path design:** settle identity/ownership policies, atomic zone
   storage, durability, replication, routing, and credential recovery for
   each supported deployment mode.
3. **Assessment and import:** implement source inventory, metadata reports,
   comparisons, synchronization, adoption, and the migration/rollback plan.
4. **Operational lifecycle:** implement DHCP compatibility, aging/scavenging,
   diagnostics, administration, backup, and failover behavior.
5. **Windows qualification:** automate the acceptance matrix and document
   tested versions, topology, scale, and operational limits before claiming
   complete DNS replacement.

The accepted goal is complete DNS replacement for a single-domain forest.
Specific libraries, schemas, AD ACL translation, supported Windows versions,
and per-mode durability guarantees still need design and experimental
evidence. These are required work, not grounds to call a read-only import or
permanent DC forwarder complete. Multiple domains/forests, cross-forest
trusts, and RODC-specific registration behavior require later qualification.
Replacing GPO, LDAP, Kerberos, AD DS itself, or the DHCP service is outside
this DNS scope.

## Sources and existing seams

Primary sources checked on 2026-10-15; no Windows interoperability spike has
been run for this outline.

- [Microsoft: DNS and AD DS](https://learn.microsoft.com/en-us/windows-server/identity/ad-ds/plan/dns-and-ad-ds).
- [Microsoft: dynamic updates](https://learn.microsoft.com/en-us/windows-server/networking/dns/dynamic-update), including SOA discovery, TKEY/Kerberos, and DHCP ownership.
- [Microsoft: DNS zone types](https://learn.microsoft.com/en-us/windows-server/networking/dns/zone-types), including AD integration, transfers, and name reservations.
- [Microsoft: aging and scavenging](https://learn.microsoft.com/en-us/windows-server/networking/dns/aging-scavenging).
- [Microsoft: Export-DnsServerZone](https://learn.microsoft.com/en-us/powershell/module/dnsserver/export-dnsserverzone?view=windowsserver2025-ps) and [Get-DnsServerResourceRecord](https://learn.microsoft.com/en-us/powershell/module/dnsserver/get-dnsserverresourcerecord?view=windowsserver2025-ps).
- [RFC 2136: dynamic updates](https://www.rfc-editor.org/rfc/rfc2136), [RFC 2930: TKEY](https://www.rfc-editor.org/rfc/rfc2930), and [RFC 3645: GSS-TSIG](https://www.rfc-editor.org/rfc/rfc3645).
- [RFC 4701: DHCID](https://www.rfc-editor.org/rfc/rfc4701), [RFC 4702: DHCP client FQDN option](https://www.rfc-editor.org/rfc/rfc4702), and [RFC 4703: conflict resolution](https://www.rfc-editor.org/rfc/rfc4703).
- Existing [zone/update types](../api/v1alpha1/dnszone_types.go), [authoritative opcode hooks](../internal/stages/authoritative/authoritative.go), [TSIG seam](../internal/dnsserver/tsig.go), [store contract](../internal/store/store.go), and [integration lifecycle](../ee/integration/integration.go).
