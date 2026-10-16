# DHCPv4 server

DHCPv4 is a **v0.2.0 development feature**, disabled by default. It supports
persistent leases and reservations, multiple subnets and trusted relays,
live API/dashboard management, lease import/export and automatic A/PTR
records. It is not available in v0.1.0.

Use one Linux host with `store.driver: sqlite`, fixed interface addresses and
a persistent `node.dataDir`. Only one allocator may manage each pool. DHCPv6
and multi-node failover remain deferred. The Kubernetes `kube` store is
rejected; do not copy enabled DHCP configuration to the other nodes of a DNS
HA deployment. Continue to manage IPv6 router and DNS advertisements on your
router.

## Start in the dashboard

A fresh Linux/SQLite node needs no DHCP YAML. First provide its static host
addresses, persistent data volume and the [network privileges](#network-and-privileges)
below. The browser can select existing interfaces and addresses; it cannot
create a VLAN interface, assign a host address or grant container capabilities.

1. Open **DNS → DHCP** on the node that will allocate addresses. All setup and
   lease operations apply to that connected node.
2. Choose **New scope**. Enter a stable name, interface, server IPv4 address,
   client subnet and any trusted relay pairs. Interface discovery lists local
   IPv4 addresses without sending DHCP traffic.
3. Complete the address pool, router, DNS servers, lease duration and optional
   DNS registration, then review and save the scope. Open its detail page to
   add initial reservations or change saved setup.
4. Turn on **Enable DHCP after restart**. The restart notice describes the
   saved desired state; saving does not start a listener or change a running
   allocator. Every saved scope must be complete even while DHCP is disabled.
5. Stop any competing DHCP service before restarting this node to activate
   the saved scopes. Follow [cutover checks](#first-cutover-and-acceptance),
   then confirm that the scope is running and inspect its leases.

New scopes, enabled state and topology changes take effect after a node
restart. For an active API-managed scope, its ordinary network option and
reservation editors apply changes immediately. **Setup for next restart**
stages a complete scope instead. Disabling DHCP also takes effect only after
restart. Up to 256 scopes and a 4 MiB saved setup document are supported;
incomplete drafts cannot be persisted.

### Optional file configuration

The legacy flat form configures a scope named `default`. Replace these
example addresses and interface with your LAN settings. `serverIP` must
already be assigned to the interface. Range endpoints are inclusive.

```yaml
store:
  driver: sqlite
dhcp:
  enabled: true
  management: api
  interface: eth0
  serverIP: 192.0.2.2
  subnet: 192.0.2.0/24
  rangeStart: 192.0.2.100
  rangeEnd: 192.0.2.199
  router: 192.0.2.1
  dnsServers: [192.0.2.2]
  domainName: lan.home.arpa
  leaseDuration: 12h
  registerDNS: true
  dnsTTL: 30s
  reservations:
    - mac: "02:00:00:00:00:10"
      ip: 192.0.2.10
      hostname: printer
```

The pool, router and reservations must fit the subnet. Server, router,
network, broadcast and trusted relay gateway addresses cannot be allocated.
A reservation may be outside the dynamic pool but must be inside the subnet.
Reservations use unicast Ethernet MAC addresses. Keep unmanaged static
addresses outside the pool: the server does not probe for conflicts before
making an offer. A matching client's DECLINE quarantines an address for ten
minutes.

`leaseDuration` defaults to 12 hours and accepts whole seconds from one
minute to 30 days. `dnsTTL` defaults to 30 seconds and accepts one second to
one hour. Ensure advertised DNS servers are reachable on UDP and TCP port 53.
`domainName` is optional unless DNS registration is enabled.

## Multiple subnets and relays

Use `dhcp.scopes` instead of flat scope fields to serve several networks.
Names are stable lowercase filename components; keep them unchanged while
leases remain active. This example serves a local subnet and a remote subnet
through a trusted relay on the same server-facing interface:

```yaml
dhcp:
  enabled: true
  scopes:
    - name: lan
      management: api
      interface: eth0
      serverIP: 192.0.2.2
      subnet: 192.0.2.0/24
      rangeStart: 192.0.2.100
      rangeEnd: 192.0.2.199
      router: 192.0.2.1
      dnsServers: [192.0.2.2]
      domainName: lan.home.arpa
      registerDNS: true
    - name: guest
      management: file
      interface: eth0
      serverIP: 192.0.2.2
      subnet: 198.51.100.0/24
      relayOnly: true
      trustedRelays:
        - sourceIP: 192.0.2.1
          gatewayIP: 198.51.100.1
      rangeStart: 198.51.100.100
      rangeEnd: 198.51.100.199
      router: 198.51.100.1
      dnsServers: [192.0.2.2]
      domainName: guest.home.arpa
      registerDNS: true
```

Subnets cannot overlap. There is one listener per interface, at most one
direct scope per interface, and a shared server address for scopes on that
interface. Additional direct subnets need their own interfaces, such as VLAN
interfaces. Flat fields and `scopes` cannot be combined.

Configure the relay to send to the server address, preserving its client-side
gateway in `giaddr`. Both the source IP and gateway IP must match the configured
pair on the receiving interface. Relay requests and replies use UDP port 67;
allow a route back to the gateway. Routed unicast renewals use the client's
existing address to choose its scope. Unknown relays are ignored. Ordinary
option 82 information is echoed; link-selection, server-ID-override and
alternate-port extensions are unsupported and packets carrying them are
dropped.

## Dashboard and API ownership

Open **DNS → DHCP** to see saved scopes and whether they are running or have
pending changes, then open a scope for setup, live options and lease inventory.
These pages and `/api/v1/node/dhcp/v4` are local to the connected node; no
peer forwarding or cluster aggregation occurs.

An existing host-file setup remains file-owned. Choose **Manage in dashboard**
and confirm **Adopt setup** to copy its effective scopes and delegate setup
management. This leaves the host YAML untouched. Saved dashboard setup takes
precedence after restart, and adoption requires that restart even when the
network values are unchanged. Adopted scopes use API management. A disabled,
incomplete file setup remains readable. Choose **Complete and adopt setup** to
fill in every incomplete scope in the wizard, review the full setup and adopt
it in one save. It stays disabled until you explicitly enable it.

Without adoption, `management: file` is the default: options and reservations
stay read-only. A file-configured `management: api` scope supports live edits;
its file options seed the first start and later starts preserve database
values. Changing back to file mode validates host values against live
allocations. A saved UI registry continues to take precedence over host seeds.

Any DHCP environment override, including `KUDURRU_DHCP__ENABLED=false`, locks
dashboard setup. Remove all `KUDURRU_DHCP__*` overrides and restart before
adopting or saving setup. Starting with both a saved UI registry and DHCP
environment overrides fails with a conflict. File mode retains environment
precedence. For enabled file-configured API scopes, mutable environment
overrides are rejected; topology, enabled and management overrides remain
allowed while the host owns setup. Generic Settings writes cannot change DHCP;
the dedicated setup API is the explicit management path.

Live pool, DNS option and reservation edits are durable and preserve existing
lease ownership. A conflicting binding, offer or quarantine rejects a live
edit. Setup reads show those current effective values unless that scope has
a pending setup edit; then they show the pending desired values instead.
An enable toggle, unrelated scope edit or ordinary restart does not replay old
seed options over later live edits.

| Route under `/api/v1/node/dhcp/v4` | Purpose |
|---|---|
| `GET /setup` | Desired/effective scope view, pending scopes, setup ETag and per-scope revisions |
| `PUT /setup` | Save complete setup for restart; required `If-Match` and `scopeRevisions`; optional `adopt` |
| `GET /interfaces` | Local IPv4 interfaces and addresses; no network probes |
| `GET /status` | Node identity, enabled/available state and scopes |
| `GET /scopes/{scope}/config` | Effective topology, options, reservations, ownership and ETag |
| `PUT /scopes/{scope}/config` | Replace mutable options with required `If-Match` |
| `GET /scopes/{scope}/leases` | Live inventory; `q`, `state`, `limit`, `continue` |
| `GET /scopes/{scope}/reservations` | Reservation inventory |
| `PUT`, `DELETE /scopes/{scope}/reservations/{mac}` | Change a reservation with required `If-Match` |
| `GET /scopes/{scope}/export` | Native lease document in the `document` response field |
| `POST /scopes/{scope}/import` | Atomic merge of `{format, data}` |

Use the exact quoted `etag` from a config response as `If-Match`. A stale
revision returns 412; reload before retrying. File ownership and live
allocation conflicts return 409. Config revisions change only for management
edits or staged setup application, not renewals. Removing a reservation does
not revoke its active lease; there is no administrative lease-delete endpoint.

Setup has a separate ETag. Send its exact quoted `etag` as `If-Match` and echo
the full `scopeRevisions` map from `GET /setup`; values are decimal strings,
including retained database names. A new scope without a database uses `"0"`.
Another setup save or live option/reservation edit can cause 412. The dashboard
refreshes the setup metadata and retains the draft until you choose to reload
and review it. Review the latest effective and pending values before retrying;
do not replace revision guards with a wildcard. Save checks a read-only live
snapshot; startup checks its expected revision and live allocations again in
the transaction that applies the staged scope.

### Repair a setup that could not start

If dashboard-owned DHCP cannot open a scope or bind its listener, DNS and the
API stay available. The DHCP page shows **DHCP could not start** with the
runtime error and a restart notice; running status reports DHCP disabled.
Setup remains editable. Correct the interface, server address, privileges,
occupied port or allocation conflict, save if setup changed, and restart the
node. Nothing retries a listener bind automatically after a save.

A stale staged database revision can also block startup. Reload setup to
obtain current `scopeRevisions`, review the pending desired scope, explicitly
save again, then restart. This refreshes that scope's apply guard. An edit
that would invalidate live leases must still be corrected or delayed until
those leases expire. A retry after a later bind failure preserves any staged
scope already committed and subsequent live edits.

File-owned DHCP startup errors remain fatal. Corrupt/unsupported
`dhcp-settings.json` or environment conflicts that prevent opening setup also
fail startup; those require host-side repair rather than the dashboard.

Viewers can read configuration. Auditors and operators can also read and
export leases. By default, only administrators can edit configuration or
reservations and import leases. Custom roles use `dhcp.read/write/delete`
and `dhcpleases.read/export/import`. Existing session, CSRF and audit rules
apply.

### Diagnose packet failures after startup

After DHCP starts successfully, a lease-handler or storage failure is logged
at ERROR and only the affected packet is dropped. No successful lease reply
is sent for the failed operation. The listener continues handling later
requests and client retries in both file- and dashboard-owned scopes, so a
DHCP storage failure does not stop core DNS. Check the server logs, disk space,
database filesystem availability and write permissions; restore storage access
and let clients retry.

Network timeouts and ICMP errors retain their existing per-packet handling.
Unexpected socket failures remain fatal. Startup rules above are unchanged:
file-owned startup failures remain fatal, and recoverable dashboard-owned
activation failures leave the repair UI available. The setup `runtimeError`
field reports startup activation failures only, not live packet/storage health.

## Automatic lease DNS

Set `registerDNS: true` and use a dedicated domain such as `lan.home.arpa`.
A bound lease named `laptop` answers at `laptop.lan.home.arpa` and has a PTR
record. Reservation hostnames override client-supplied names. Offers,
quarantined or expired addresses have no records. Duplicate hostnames are
suppressed; a reserved hostname is protected from claims by a different MAC,
even when the reserved device is offline.

DHCP names and MAC addresses are unauthenticated. A second client can obtain
a lease with the same unreserved name and deny service to that name: all
claimants lose their A/PTR records and hostname/nameGlob policy matches until
the conflict clears, including across scopes sharing a domain. Clients can
also claim unused names. Use reservation hostnames for stable names, but MAC
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

## Network and privileges

The server receives UDP port 67 on the selected interface and sends replies
to UDP port 68. Unconfigured clients need broadcast replies on that LAN.
A published Docker UDP port or a Kubernetes LoadBalancer does not provide
the required broadcast connectivity.

For a Linux container, use host networking, keep the data directory on a
persistent volume and grant `NET_BIND_SERVICE` and `NET_RAW` to both the
executable and the runtime. The latter capability permits binding the socket
to the selected interface. Adding `--cap-add NET_RAW` alone to the existing
non-root DNS image does not give its executable that effective capability.

Build the opt-in development image from this checkout:

```sh
make docker-dhcp
```

This builds `kudurru:dhcp-development` with the executable's file capabilities
`cap_net_bind_service,cap_net_raw=ep`. The normal images keep their existing
capabilities. The underlying build option is `DHCP_CAPABILITIES=true` in
`images/hostnetwork/Dockerfile`; it defaults to `false`.

Prepare the secret files described in the [Docker guide](docker.md#docker-run).
For fresh dashboard setup, run from the directory containing `secrets/`;
no YAML mount is required:

```sh
docker run -d --name kudurru --restart unless-stopped \
  --network host --user 65532:65532 --read-only \
  --security-opt no-new-privileges \
  --cap-drop ALL --cap-add NET_BIND_SERVICE --cap-add NET_RAW \
  --tmpfs /run/kudurru:uid=65532,gid=65532 --tmpfs /tmp \
  -v kudurru-data:/var/lib/kudurru \
  -v "$PWD/secrets:/run/secrets:ro" \
  -e KUDURRU_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key \
  -e KUDURRU_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
  kudurru:dhcp-development
```

For optional host-file setup, also mount
`$PWD/kudurru.yaml:/etc/kudurru/kudurru.yaml:ro` and append
`serve --config /etc/kudurru/kudurru.yaml`. Mounted configuration and secrets
must be readable by uid 65532. A privileged container is not required. Review
every listener when using host networking because configured addresses now
refer to the host network.

For the packaged systemd service, add a drop-in with `systemctl edit kudurru`:

```ini
[Service]
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_RAW
```

Restart the service after changing its configuration and capabilities. The
default unit and deployment templates do not enable DHCP or grant its extra
capability. A missing interface, a server address absent from that interface,
an occupied UDP port 67 or insufficient permissions prevents DHCP from
starting; dashboard-owned setup retains the repair path described above.

## Lease durability and backups

The default scope uses `dhcp.db`; named scopes use `dhcp-<name>.db`, under
`node.dataDir`, separately from configuration objects in `state.db`. Each
allocation commits before its OFFER or ACK. WAL/full synchronization and
exclusive database and directory ownership protect restart recovery.
Preserve these files across restarts. API-managed options and reservations
also live in the scope databases. Dashboard desired setup and apply plans live
in `dhcp-settings.json`, protected by `dhcp-settings.lock` for the process
lifetime even when DHCP is disabled. Saves use atomic replacement and file
and directory synchronization; they do not rewrite YAML or mutate live lease
databases.

Listing and export share the store lock with allocations and rebuild the DNS
snapshot. Large lease sets or frequent refreshes can delay allocation and
renewal; large-pool capacity has not been benchmarked. Validate your expected
lease count and request rate before deployment.

Lease schema version 3 stores the last applied setup token. At startup, a new
token must match its expected database revision and preserve live ownership;
the settings, revision and token commit together. Matching tokens retain later
live edits instead of reapplying saved options. Earlier lease schemas migrate
transactionally. The JSON setup document has its own version; schema v3 refers
to the lease database, not the JSON export or setup format.

Shortening `leaseDuration` never shortens retention of an existing binding:
a renewal can advertise the new duration while the database holds the address
through its earlier expiry, in case the client missed the ACK. Configuration
changes are checked against live state before committing. Removing or renaming
a scope retains its database and can block the next DHCP startup while that
retired database has live allocations. A renamed scope uses a different
database; allocations are not moved to its new name. Keep the original name
until leases, offers and quarantines drain. To recover a failed rename/removal,
restore the original scope in saved setup using the observed revision from
its retained database, then restart. Removing the last scope stages disabling
DHCP; it still does not delete lease state.

The ordinary `kudurru backup` exports configuration objects, not these lease
databases or `dhcp-settings.json`. Native lease exports cover allocations only,
not saved setup, managed options or reservations. Stop the node before taking
or restoring a filesystem backup of the data directory, including the setup
registry and SQLite WAL files. Do not delete a scope database or restore an
old copy while clients hold leases absent from it.

## Lease import and export

The dashboard exports native version 1 JSON preserving scope, subnet,
client identity, address, hostname, absolute expiry and offered/bound/declined
state. When using the API directly, save the `document` field, not its response
wrapper. Native imports require a matching scope and subnet. Client IDs are
opaque bytes encoded as base64 with an explicit identity kind; do not rewrite
them as MAC addresses.

The import dialog accepts native JSON or a dnsmasq **lease file**, pasted as
text, up to 4 MiB. The API takes `format: native|dnsmasq` and `data` containing
the file text. An import merges all valid records in one transaction, reporting
imported, unchanged and expired/skipped counts. It cannot replace live
ownership, shorten expiration or conflict with reservations. Any invalid row
rejects the entire import. Unsupported infinite leases are rejected. Import
works in file management mode because it changes runtime allocations only.

dnsmasq hostname fields follow the same normalization as option 12: trailing
NULs are stripped and unusable names are omitted. Native JSON stays strict and
lossless, rejecting invalid hostnames instead of normalizing them. Other
invalid records still reject the entire import in either format.

The public export uses native JSON; it preserves holds and quarantine that a
dnsmasq lease file cannot represent. Import does not configure network options
or reservations. Save and recreate those separately before cutover.

## First cutover and acceptance

The opt-in [isolated test harness](../../test/dhcp/README.md) has passed with a
stock dhcpcd client on two direct subnets and through a relay: acquisition,
reserved addresses, distinct clients, exhaustion, release/reuse, restart,
routed renewal and expiration/reacquisition. It runs the server as uid 65532
with `NET_BIND_SERVICE` and `NET_RAW` plus `no-new-privileges`. It sends no
traffic on a live LAN. Test your interfaces, router and relay hardware before
moving clients.

1. Save the old server's options, reservations and leases. Prepare compatible
   pools and reservations. Stop the old DHCP service before importing its
   final lease snapshot; avoid having both allocators issue leases during
   cutover. Without a supported export, use a nonoverlapping pool or wait for
   old leases to expire.
2. Configure fixed host addresses and deployment privileges. In the dashboard,
   save complete scopes, reservations and DNS domain boundaries, then stage
   enabling DHCP. For optional file setup, check configuration loading with
   `kudurru --config /etc/kudurru/kudurru.yaml config show > /dev/null`;
   that command does not validate the saved UI registry or bind a listener.
3. Start kudurru and import leases before allowing client DHCP traffic to it.
   Retain the old configuration for rollback.
4. Verify one client's address, mask, gateway, DNS servers, domain, duration
   and A/PTR resolution. For relays, verify the source/giaddr mapping and
   routed renewal path.
5. Renew, restart kudurru, renew again, then test a second and a reserved
   client. Check each subnet independently.
6. Exercise release, expiration and a full pool on an isolated LAN before
   moving the remaining clients.

For rollback, stop kudurru DHCP before restoring the old service. Account
for every still-valid lease issued by kudurru so the old server cannot reuse
those addresses prematurely.
