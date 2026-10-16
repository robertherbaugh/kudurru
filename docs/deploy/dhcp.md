# DHCPv4 server

DHCPv4 is included in **v0.1.0**, disabled by default. It supports
persistent leases and reservations, multiple subnets and trusted relays,
live API/dashboard management, lease import/export and automatic A/PTR
records, conflict detection, network boot settings, classless routes, NTP and
search domains.

Use one Linux host, or the three-node quorum setup below, with
`store.driver: sqlite`, fixed interface addresses and a persistent
`node.dataDir` on each node. Independent allocators must never share a pool.
The Kubernetes `kube` store is rejected. DHCPv6 is excluded; continue to manage
IPv6 router and DNS advertisements on your router.

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
addresses outside the pool. Before new offers, the server probes candidate
addresses and durably quarantines detected conflicts for ten minutes. This
reduces collisions without proving that a quiet host is absent. A matching
client's DECLINE uses the same ten-minute hold. Relay server-ID overrides and
all configured failover server addresses are also excluded from allocation.

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

Configure the relay to send to the server address, preserving its return
address in `giaddr`. Both source IP and gateway IP must match the configured
pair on the receiving interface; allow a route back to `giaddr`. Ports default
to UDP 67. Unknown relays and direct clients carrying option 82 are ignored.
Routed unicast renewals use the client's existing address to choose its scope.

Relays supporting the following extensions need explicit authorization in
each trusted mapping. For example, a relay may use a transit `giaddr` while
selecting the guest link:

```yaml
trustedRelays:
  - sourceIP: 192.0.2.1
    gatewayIP: 192.0.2.1
    sourcePort: 1067
    linkSelection: 198.51.100.1
    serverIdentifier: 198.51.100.1
```

This is a mapping within a guest scope with `subnet: 198.51.100.0/24`.
`linkSelection` authorizes the exact RFC 3527 option-82 suboption-5 address;
it must be usable within that scope. Several scopes may share a relay source
and `giaddr` when their selectors differ. `serverIdentifier` authorizes the
exact RFC 5107 suboption-11 value used as option 54 in offers and replies. The
relay must handle subsequent client traffic sent to that override address.
The server's source IP stays its local `serverIP`.

A non-67 `sourcePort` requires RFC 8357's zero-length suboption 19 and must
match the actual UDP source port. Replies go to `giaddr` at that same port;
they never use `linkSelection` as the destination. Port 68 is forbidden.
Configured link selection and server identifier values must be present exactly;
unset fields reject those extensions. Duplicate control suboptions, malformed
TLVs and port mismatches are dropped. Valid option 82 is echoed whole, or
omitted whole if it cannot fit the client's reply limit. If a transit address
also falls within another configured scope, that scope must exclude it through
its own immutable topology; merely setting it as a mutable router is insufficient.

### Conflict detection

`conflictDetection` accepts `arp`, `icmp` or `disabled`. By default, a directly
attached scope without trusted relays uses an ARP probe. A relay-only scope or
any scope with trusted relays uses a routed ICMP echo. ARP is rejected for
relay-configured scopes; choose ICMP or explicitly disable probes instead.

`probeTimeout` defaults to `200ms` and accepts integral milliseconds between
`50ms` and `500ms`. An offer hold commits before probing. A positive response
causes a durable ten-minute quarantine before another candidate is tried, up
to three candidates per packet. Existing bound clients are not probed again
when rediscovering their lease. Probe socket, route and cancellation errors
drop that packet without an offer; inspect logs and retry after correcting
the deployment. Probes require the raw-network privileges below.

ICMP only detects hosts that answer it through the configured server interface.
Firewalls, sleeping devices and competing external allocators can hide conflicts.
Continue to reserve DHCP pools exclusively and keep other static addresses
outside them. `disabled` is an explicit operator opt-out; it does not disable
client DECLINE quarantine.

### NTP, search domains, routes and boot settings

These optional fields work in the flat form or within a named scope. The
following fields extend the local-scope example above:

```yaml
ntpServers: [192.0.2.1]
domainSearch: [lan.home.arpa, lab.home.arpa]
classlessRoutes:
  - destination: 0.0.0.0/0
    router: 192.0.2.1
  - destination: 10.20.0.0/16
    router: 192.0.2.1
bootServer: 192.0.2.3
bootServerName: boot.lan.home.arpa
bootFile: netboot.efi
bootProfiles:
  - architecture: 7
    bootServer: 192.0.2.3
    bootServerName: boot.lan.home.arpa
    bootFile: http://boot.lan.home.arpa/boot.efi
```

NTP option 42 permits up to 63 unicast IPv4 servers. Search option 119 uses
validated DNS labels with at most 255 encoded bytes. Classless routes use
option 121 and Microsoft option 249 when requested; destinations must be
canonical IPv4 prefixes and next hops usable within the scope. The complete
route list must fit 255 encoded bytes. Clients receiving classless routes
ignore option 3, so include a `0.0.0.0/0` route explicitly for a default gateway.

`bootServer` sets BOOTP `siaddr`. `bootServerName` sets option 66 and, when at
most 63 bytes, `sname`; its limit is 253 bytes. `bootFile` sets both BOOTP `file`
and option 67, with a 127-byte limit. Names and files accept printable ASCII
without spaces. Up to 128 unique `bootProfiles` select a complete replacement
set of those three fields by client option-93 architecture. The first matching
architecture in the client's list wins; an unmatched client uses scope defaults.
Blank profile fields stay blank. Use an external TFTP or HTTP boot server;
kudurru does not serve boot files or provide proxyDHCP on UDP 4011.

Optional options follow the client's parameter-request list and are omitted
whole when the reply-size limit cannot accommodate them. Client FQDN option 81
accepts DNS-label and legacy ASCII names and is preferred over option 12 as an
advisory hostname. Its reply signals no server-driven RFC 2136 DNS UPDATE;
`registerDNS` controls local A/PTR publication independently. Malformed FQDN
flags/names and malformed architecture lists are rejected before allocation.

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

## Network and privileges

The server receives UDP port 67 on the selected interface and sends replies
to UDP port 68. Unconfigured clients need broadcast replies on that LAN.
A published Docker UDP port or a Kubernetes LoadBalancer does not provide
the required broadcast connectivity.

For a Linux container, use host networking, keep the data directory on a
persistent volume and grant `NET_BIND_SERVICE` and `NET_RAW` to both the
executable and the runtime. `NET_RAW` permits binding to the selected
interface and opening the ARP/ICMP probe sockets. Adding `--cap-add NET_RAW` alone to the existing
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
# Reset the packaged allowlist, then include ARP packet sockets.
RestrictAddressFamilies=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK AF_PACKET
```

Restart the service after changing its configuration and capabilities. The
default unit and deployment templates do not enable DHCP or grant its extra
capability. A missing interface, a server address absent from that interface,
an occupied UDP port 67 or insufficient permissions prevents DHCP from
starting; dashboard-owned setup retains the repair path described above.

## Three-node automatic failover

Configure exactly three Linux/SQLite nodes. Two voters must be reachable to
allocate or renew; losing one voter permits an election, while losing two
stops successful DHCP replies until quorum returns. Every node needs its own
persistent data directory and fixed server-facing interface addresses, including
each directly served client link. A relay-only scope can use an off-subnet
local server address. Relay
forwarding must reach all three servers. A replacement leader uses its own
server identifier: a client whose original server is down reaches it at T2
rebinding. Existing leases remain usable until their expiry. This does not
require a shared VIP and does not replicate DNS configuration objects.

1. Provision interfaces, UDP 67 access and the capabilities described above on
   all nodes. Permit a chosen TCP peer port between the three nodes, such as
   7447. Keep host clocks synchronized and monitor clock health; forward clock
   jumps can prematurely expire leases even when replicas agree.
2. Create the same scope names, subnets, relay trust, conflict-detection settings
   and initial options on all three dashboards. Each scope's interface and
   server IP are local to that node. Keep DHCP disabled until setup is complete.
3. Open **Automatic failover → Configure failover**. Set the same cluster ID
   and three voter IDs/advertised peer addresses everywhere, and select this
   node's distinct ID and listen address. For every scope, enter all three
   server addresses, with exactly the same mapping on every node. Those
   addresses are excluded from allocations. An optional dashboard URL is only
   a navigation reference; replication and forwarded management use peer TCP.
4. Choose credentials: the same existing `KUDURRU_ENCRYPTION_KEY` on every host
   derives authenticated peer certificates, or provide CA, certificate and
   private-key file paths on each host. Explicit certificates need both client
   and server authentication EKUs, CN equal to the voter ID, and a SAN matching
   the advertised peer host. Provision secrets outside the browser; the UI
   stores paths only. All three TLS paths are required together.
5. Select **Bootstrap this new cluster** on the alphabetically first voter
   only. That is the only node allowed to adopt existing standalone leases.
   Other voters must have empty lease state or their own existing state from
   this same cluster. Never copy another node's directory or bootstrap an
   independent cluster to recover from loss of quorum.
6. Save failover and enabled state for restart on each node, then restart all
   three. The dashboard reports voter role, leader, quorum readiness, applied
   and committed indexes. No DHCP reply succeeds before the initial seed is
   committed by a majority. Verify acquisition and renewals, then stop one
   voter and verify the remaining majority elects/retains a serving leader.

For file-managed deployment, the same fields live under `dhcp.failover`.
This example names the first node; the other two change `nodeID`,
`listenAddress`, their local scope interface/server IP, and set `bootstrap: false`:

```yaml
dhcp:
  failover:
    enabled: true
    clusterID: home-dhcp
    nodeID: stone-1
    listenAddress: 0.0.0.0:7447
    peers:
      - {id: stone-1, address: '192.0.2.2:7447'}
      - {id: stone-2, address: '192.0.2.3:7447'}
      - {id: stone-3, address: '192.0.2.4:7447'}
    bootstrap: true
```

Repeat the mapping on all replicas of a scope (or the legacy flat `dhcp`
scope), using that scope's server addresses. Different scopes can use different
mappings:

```yaml
failoverServers:
  - {nodeID: stone-1, address: 192.0.2.2}
  - {nodeID: stone-2, address: 192.0.2.3}
  - {nodeID: stone-3, address: 192.0.2.4}
```

These are fragments of the complete setup above, not standalone configurations.
Use `management: api` for replicated live network-option and reservation edits.
Adopt file-managed scopes before initializing failover if you want that live
UI ownership; an established file-managed cluster cannot change management mode
in place.
Mutations made through any healthy node's API forward to the current leader and
wait for the connected node's committed SQLite state before reporting success.
A timed-out write may still commit; reload current state and its revision before
retrying. Pool ownership is never delegated to independent local callbacks. DNS answers
on followers use their last committed snapshots, so a disconnected follower
may serve a previous lease name until expiry or catch-up.

Once a cluster identity exists, the UI freezes shared scope registry/topology,
voter IDs/addresses, cluster ID and per-scope server mappings. Change network
options and reservations through the live scope editors; imports also replicate.
Local interface, peer listen/TLS references and node enable/disable may be saved
for restart. Stop a voter by disabling DHCP while retaining failover settings.
Deleting failover settings does not convert an established replica into a
standalone allocator. Enabling failover on the initial bootstrap can apply its
staged seed once; other node-local setup tokens cannot overwrite cluster state.

Preserve the entire `dhcp-raft` directory, including `identity.json`, Bolt log
and snapshots, with the lease databases and setup registry. Restart an intact
failed node with the same identity and reachable peer address; it replays logs
or receives a snapshot. If its SQLite apply failed, correct the storage problem
before restarting it. The failed replica stops voting; the remaining two can
continue. A node with lost Raft state and a persisted initialized identity
refuses automatic bootstrap. Do not erase that guard.

Changing shared topology or voter identity requires a coordinated migration:
stop all old allocators, retain complete backups and export current leases,
prepare compatible scopes on fresh directories with a new cluster ID, and
start the sole bootstrap and two empty voters with DHCP client ingress blocked.
Wait for quorum, import ownership through the live API, and verify committed
state on all replicas before exposing DHCP traffic. Imports require quorum. Preserve every live allocation/quarantine, reservation and network
option; use a nonoverlapping pool or drain leases when topology is incompatible.
Never start old and migrated clusters against the same clients. There is no
online membership-reconfiguration or force-new-cluster recovery endpoint.

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

Listing and export read committed cached state and do not rebuild DNS snapshots.
They still copy/sort the live set under the store lock. Renewal updates affected
rows only; durable SQLite writes remain mandatory. Comparative 1,000/10,000-lease
[benchmarks](../architecture/dhcp.md#persistent-state-and-transfer) show the
improvement, but do not establish production throughput. Validate your expected
lease count, storage and request rates before deployment.

Lease schema version 4 retains the applied setup token and adds persistent DNS
claim ranks and replica progress. At startup, a new
token must match its expected database revision and preserve live ownership;
the settings, revision and token commit together. Matching tokens retain later
live edits instead of reapplying saved options. Earlier lease schemas migrate
transactionally. The JSON setup document has its own version; schema v4 refers
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
registry, `.dhcp-dns-claims.sqlite`, SQLite WAL files and, for quorum nodes,
the entire `dhcp-raft` directory. The claim registry establishes cross-scope
name order and must stay with its lease databases. Do not delete a scope database or restore an
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

The [isolated test harness](../../test/dhcp/README.md) uses stock dhcpcd on
two direct subnets and through a relay. It covers acquisition, reservations,
distinct clients, exhaustion, release/reuse, restart, routed renewal and
expiration/reacquisition. Static hosts provoke actual ARP and routed ICMP
conflicts, whose durable quarantines must survive restart. Relay traffic uses
link selection, server identifier override and alternate source port 1067.
The client consumes NTP, search domains, classless routes and boot settings,
and supplies client FQDN.

The fixture runs as uid 65532 with exactly `NET_BIND_SERVICE` and `NET_RAW`
and `no-new-privileges`. It checks the effective, permitted and bounding
capabilities and opens an ARP socket in the disconnected namespace. Both
systemd-style ambient capabilities and container-style executable file
capabilities are checked; the server restarts using the file-capability profile.
This validates process permissions, not actual systemd unit activation or a
Docker daemon. The `dhcp-acceptance` CI job installs Ubuntu's stock `dhcpcd-base`
package and retains logs, lease databases, script snapshots and packet captures.
It sends no traffic on a live LAN. Test your actual unit/container deployment,
interfaces, router and relay hardware before moving clients.

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
4. Verify one client's address, mask, gateway, DNS servers, domains, duration,
   optional NTP/routes/boot settings and A/PTR resolution. For relays, verify
   the exact source/giaddr/port/extension mapping and routed renewal path.
5. Renew, restart kudurru, renew again, then test a second and a reserved
   client. Check each subnet independently.
6. Exercise release, expiration and a full pool on an isolated LAN before
   moving the remaining clients.

For rollback, stop kudurru DHCP before restoring the old service. Account
for every still-valid lease issued by kudurru so the old server cannot reuse
those addresses prematurely.
