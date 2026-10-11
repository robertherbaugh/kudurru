# Milestone M5: hardening

Four work packages, merged with two integration fixes (the superseded lameduck client dropped; the chart now runs `kudurru lameduck` in preStop and sets `KUDURRU_SETTINGS_FILE`). Decisions D106 to D118 in [architecture.md](../architecture.md) record what changed.

| Package | Report | Headline |
|---|---|---|
| Security review | [security.md](security.md) | 11 confirmed findings fixed with regression tests (4 medium: SSRF via list URLs, inherited credentials after account re-creation, proxy-keyed login lockouts, webhook on all interfaces); SECURITY.md |
| End-to-end and fuzzing | [e2e.md](e2e.md) | BIND 9.18 secondary and primary interop, two-node internal listener, SIGTERM/restart/config/port resilience, fuzz CI; five bugs recorded as KnownBug skips |
| Server burn-down | [burndown-server.md](burndown-server.md) | Settings hot reload, lameduck drain, `kudurru.io/enabled`, `/internal/zones` + serial metrics, refresh/purge routes, EndpointSlice leader lookup, doctor API, controller flakes fixed at the root |
| Edge, CLI, chart, release | [burndown-edge-release.md](burndown-edge-release.md) | Chart flags that would have crash-looped edge pods fixed, generated aggregated ClusterRoles, self-hosted API docs, `health --vrrp`, CHANGELOG, release checklist, 0.1.0-rc.1 |

Open after M5 (docs/followups.md): the five e2e KnownBug rows (conditional forwarding bypassed by the root forward zone's hint, readiness before the first live snapshot, no lameduck window setting, keepalived path braces, a misleading key-generation log line), `POST /auth/password` not forwarded by the API, world-readable local.db/querylog.db, and everything that needs Docker, kind or two VMs.
