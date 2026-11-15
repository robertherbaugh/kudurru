### Added

- Tier-B webhook Integrations publish records: set `spec.webhook.domains` (and optionally `policy`) to publish the selected records of those domains to any external-dns webhook provider or another Kudurru's external-dns listener. Ownership is kept at the peer in a TXT registry (`_kudurru-<type>.<name>`), records of other owners and unowned records are never changed, and every change is confirmed by rereading the peer, never by its 204. Uncertain changes, a 204 without effect included, stay pending and are settled from a reread on the next sync, also after a restart or a failover. A record set this instance published stays its own after `domains` is narrowed: `policy: sync` deletes it, other policies keep it.
- `kudurru integrations webhook conformance --expect-endpoints-file` rereads the peer's record list and checks that given endpoints are present (`records_contain`) or absent (`records_absent`), so provider authors can check read-your-writes and TXT registry support. Record lists of up to 32,768 endpoints are accepted.

### Changed

- A webhook Integration whose peer offers only `records` is still refused unless `spec.webhook.domains` is set; a configured domain must be inside the domain filter the peer negotiates.
- A reviewed `integrations sync` of a webhook Integration with domains requires `--mode publish` and the preview digest, as for Cloudflare.
- Deleting a webhook Integration with `deletionPolicy: Delete` still needs no reachable peer unless it began publishing records; one that did waits for its peer and then deletes the record sets its registry rows name, including those outside narrowed domains, and leaves every other record at the peer.
