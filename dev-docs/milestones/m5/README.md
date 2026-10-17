# Milestone M5: hardening

These reports record hardening and release preparation before v0.1.0.
Their measurements and acceptance items describe the reviewed commits,
not the status of every later release or deployment. For the release's
behavior and limitations, read the [architecture tour](../../../docs/architecture/README.md)
and [changelog](../../../CHANGELOG.md).

Four work packages, merged with two integration fixes (the superseded lameduck client dropped; the chart now runs `kudurru lameduck` in preStop and sets `KUDURRU_SETTINGS_FILE`). Decisions D106 to D118 in [design-record.md](../../design-record.md) record what changed.

| Package | Report | Headline |
|---|---|---|
| Final v0.1 feature review | [features-v0.1.md](features-v0.1.md) | Shipping feature matrix, Talos client-address evidence, Clients scope and outage-startup fixes, acceptance items recorded at review time |
| Security review | [security.md](security.md) | 11 confirmed findings fixed with regression tests (4 medium: SSRF via list URLs, inherited credentials after account re-creation, proxy-keyed login lockouts, webhook on all interfaces); SECURITY.md |
| Security review, v0.1 round | [security-v0.1-round.md](security-v0.1-round.md) | 17 findings on the block page, unblock requests, allowlists, catalogue fetchers, entries APIs and the release pipeline, fixed with regression tests (3 medium: a branch shadowing the release tag, a signing identity matching any workflow, a quadratic entries PATCH) |
| Measurements, v0.1 | [measurements-v0.1.md](measurements-v0.1.md) | In Docker on 4 CPUs: 142k q/s cached, 72k mix, 40k all-forwarded with no loss; 82 µs at 10k q/s; ready in 0.4 s, a 1M-entry list enforced in 2.6 s; upgrade from the oldest data format 16/16; five bugs fixed (Docker volume permissions, query-log memory under unique-name floods, a stale auth cache entry, a record write race, Cloudflare DoT refusals); soak status recorded in the report |
| End-to-end and fuzzing | [e2e.md](e2e.md) | BIND 9.18 secondary and primary interop, two-node internal listener, SIGTERM/restart/config/port resilience, fuzz CI; five bugs recorded as KnownBug skips |
| Server burn-down | [burndown-server.md](burndown-server.md) | Settings hot reload, lameduck drain, `kudurru.io/enabled`, `/internal/zones` + serial metrics, refresh/purge routes, EndpointSlice leader lookup, doctor API, controller flakes fixed at the root |
| Edge, CLI, chart, release | [burndown-edge-release.md](burndown-edge-release.md) | Chart flags that would have crash-looped edge pods fixed, generated aggregated ClusterRoles, self-hosted API docs, `health --vrrp`, CHANGELOG, release checklist, 0.1.0-rc.1 |

The original M5 report left five e2e KnownBug rows, password-route
forwarding and database file permissions open. Those product bugs were
fixed in the v0.1 fix round; the saved-snapshot outage-startup fix and
later acceptance checks are recorded in [features-v0.1.md](features-v0.1.md).
The current backlog is in [followups.md](../../followups.md).
