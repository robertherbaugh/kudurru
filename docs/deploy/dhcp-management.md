# DHCP ownership and diagnosis

[DHCP guides](dhcp.md) · [Step-by-step setup](../guide/dhcp/README.md)

## Dashboard and API ownership

Open **DHCP → Scopes** to see saved scopes and whether they are running or have
pending changes, then open a scope for setup, live options and reservations.
Use **DHCP → Leases** for the lease inventory.
With the SQLite store, these pages and `/api/v1/node/dhcp/v4` are local to
the connected node; no peer forwarding or cluster aggregation occurs. With the
Kubernetes store the same routes read and write `DHCPScope` and `DHCPLease`
objects and answer the same on every replica.

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
