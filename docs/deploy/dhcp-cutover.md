# DHCP cutover checks

[DHCP guides](dhcp.md) · [Step-by-step setup](../guide/dhcp/README.md)

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
