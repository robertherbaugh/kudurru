# DHCP scopes and network options

[DHCP guides](dhcp.md) · [Step-by-step setup](../guide/dhcp/README.md)

## Start in the dashboard (SQLite)

A fresh Linux/SQLite node needs no DHCP YAML. First provide its static host
addresses, persistent data volume and the [network privileges](dhcp-host.md#network-and-privileges)
in the host guide. The browser can select existing interfaces and addresses; it cannot
create a VLAN interface, assign a host address or grant container capabilities.

1. Open **DHCP → Scopes** on the node that will allocate addresses. All setup and
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
   the saved scopes. Follow [cutover checks](dhcp-cutover.md#first-cutover-and-acceptance),
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
the deployment. Probes require the [raw-network privileges](dhcp-host.md#network-and-privileges).

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
