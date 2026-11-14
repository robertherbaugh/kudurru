### Added

- UniFi optional writes, each off by default and run only as an explicit reviewed command (SQLite): `push.mirrorRecords` mirrors selected A and CNAME records into the site's DNS policies through a preview and a digest-bound `integrations sync --mode publish`, changes only policies the Integration itself created, never touches operator policies and reports drift; polling never writes.
- UniFi `dhcp-dns-servers` action (`push.dhcpDNSServers` with `push.dnsServers` and `push.networks`): preview the DHCP DNS server override per network, then apply it once with the preview digest.
- UniFi `block-client` and `unblock-client` actions (`push.clientActions`, requires `legacy: true`), each one legacy station command.
- `integrations act` returns an acknowledged action's answer in `result`, and the action's audit row records the SHA-256 of its canonical parameters (the same request with other whitespace or key order gives the same digest).

### Changed

- The integration manifests list no refused UniFi fields on SQLite; on Kubernetes an Integration that enables a UniFi write is refused with reason `UniFiPushUnsupportedOnKubernetes`.
- A UniFi Integration now polls at the shorter of `spec.unifi.poll` and `spec.resync` (default ten minutes); `spec.resync` was ignored before, so a poll longer than ten minutes now runs every ten minutes unless `resync` is raised.
- A review or command whose captured input still names a record set or zone deleted since answers `409` (review changed) instead of `404 integration not found`.
- An integration intent whose acknowledged provider effects have all been removed, with the destination back at its baseline, is now settled at the next poll for publishers that cannot resume an intent (the UniFi record mirror), instead of staying pending.
- An Integration that enables `push.dhcpDNSServers` must now name `dnsServers` and `networks`, and `push.mirrorRecords.enabled` requires `domains`; both were refused outright before.
