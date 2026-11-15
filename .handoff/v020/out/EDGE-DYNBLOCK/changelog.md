### Added

- Operator edge blocks: drop one client address or prefix at every dnsdist edge until an expiry (one minute to seven days). `POST /api/v1/edge/blocks`, `GET /api/v1/edge/blocks` and `DELETE /api/v1/edge/blocks/{id}` with the new `edgeblocks` permission (operator and admin roles; auditors can read), and the CLI verbs `kudurru edge block <prefix> [duration] [--until] [--reason]`, `kudurru edge blocks` and `kudurru edge unblock <id>`. Every create and revoke is audited. IPv4 prefixes must be /16 or longer and IPv6 prefixes /32 or longer; at most 256 blocks are active.
- Edge supervisors pull the blocks from every backend's authenticated internal listener (`GET /internal/edge/blocks`) and apply them with dnsdist's dynamic-block table on both the YAML and Lua renderers. Blocks survive dnsdist and supervisor restarts until they expire (the set is kept in the edge's configuration directory) and dnsdist expires each block itself. Each edge remembers the newest revision of the set it applied, also across restarts, so a lagging replica cannot bring back a revoked block or wipe a new one while the newer backend misses polls; a lower revision that every poll has reported for five minutes is taken as a restored backup. `/status` gains an `edgeBlocks` section (counts only) and the metrics `kudurru_edge_blocks` and `kudurru_edge_block_applies_total`.

### Changed

- `kudurru edge run` now uses the internal listener credentials whenever `--internal-port` is set, also when `--poll-zones=false`, so it can pull operator edge blocks.
- Revoking an edge block clears dnsdist's dynamic block table and reinstalls the remaining operator blocks; clients the `--dynblocks` rate rules had blocked are let through until they exceed the rate again.
