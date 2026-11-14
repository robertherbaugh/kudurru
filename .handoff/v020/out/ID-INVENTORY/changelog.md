### Added

- `GET /api/v1/inventory` and `kudurru inventory` list every device the client inventory knows, including devices that never sent a DNS query: inventory-integration devices (UniFi, webhook) and the answering node's DHCP leases, with MACs, names, tags, expiry, a `stale` flag and the state of each address (assigned, expired, ambiguous, superseded, withdrawn). Filters: source, kind, stale and a text search; keyset pages. Requires the new `inventory.read` permission (auditor, operator and admin roles).
- The built-in DHCP server now supplies the MAC address of a leased client, so `ClientGroup` `mac` matchers work without an inventory integration, in scopes with or without DNS registration, on both the SQLite and Kubernetes store drivers. An inventory integration's MAC for the same address takes precedence. On a SQLite cluster the lease MAC applies only to queries answered by the node whose DHCP server holds the lease; on Kubernetes every replica applies it.

### Changed

- UniFi: when two clients of one poll claim the same address with the same observation time, the inventory now lists that address as `ambiguous` on both devices instead of dropping it silently, and an older claim of another inventory source can no longer take over the address. Neither device identifies queries from it, and no DNS record is published for it, as before.
- Kubernetes RBAC: the auditor and editor ClusterRoles (chart and kustomize) gain read access (`get`, `list`, `watch`) to the virtual resource `inventories`, which backs `inventory.read`.
