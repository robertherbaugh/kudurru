### Added

- Tier-B webhook integrations can receive hook events. A peer that advertises `hooks` and is given a separate `hmacKey` credential (32 to 4096 bytes) receives signed `POST /events` deliveries for `record.changed`, `zone.changed`, `blocking.toggled`, `integration.error` and `cluster.leader_changed`. Events are at-least-once with a stable `X-Kudurru-Delivery` ID, survive restarts and failover through a durable per-instance cursor, and are retried with backoff for up to 24 hours. Every hook instance receives events for all namespaces, so creating one is a cluster-level privilege.
- Reviewed `integrations sync` and `integrations act` commands now record one audit entry each (`integration.sync`, `integration.act`), including when the outcome is uncertain.

### Changed

- The Tier-B event body and the client and network inventory JSON now use lowercase field names (`{"topic","time","payload"}`); peers that send the old capitalized names still decode.
- Each webhook integration instance now has its own provider request budget instead of sharing one with every other webhook instance.

### Fixed

- A webhook delivery that timed out, or that met an exhausted request budget, is now retried instead of being dropped on its first attempt, and a busy budget no longer stops periodic webhook inventory polling.
- Dropped webhook events now set the integration Degraded (`EventDeadLettered`), and delivery records no longer accumulate: each hook instance keeps at most 384, its oldest delivered or dropped records are removed above 128, and the rest an hour after their 24-hour deadline.
- An inventory-only or hooks-only webhook peer is scheduled by what it negotiated: it never polls, publishes or drains for a capability it did not advertise.
