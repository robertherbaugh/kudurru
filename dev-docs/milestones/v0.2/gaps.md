# v0.2.0 implementation and validation gaps

Draft baseline: 2026-10-31, PR [#54](https://github.com/robertherbaugh/kudurru/pull/54),
commit `8c63961`. This is a working completion ledger, not release approval.
The [feature packages](README.md#feature-packages-and-acceptance-criteria),
[design record](../../design-record.md#132-v020--clay-copies-12-weeks),
[AD requirements](../../active-directory-dns.md), and
[follow-ups](../../followups.md) retain their scope.

“Confirmed” below means current code or a current test demonstrates the gap.
“Recorded” means the implementation tracker still lists the work as open;
reconcile it with code and tests before implementing it again. Existing unit
coverage does not by itself close production wiring, interoperability, UI,
upgrade, or failure-recovery acceptance.

## Current evidence

- Security workflow [41166682393](https://github.com/robertherbaugh/kudurru/actions/runs/41166682393)
  passes, including Semgrep, Go reachability analysis, checkout scanning, and
  both container scan jobs. The published-release scan is intentionally
  skipped on pull requests; this does not certify already published images.
- CI [41166682472](https://github.com/robertherbaugh/kudurru/actions/runs/41166682472)
  fails its dashboard browser suite: 135 tests pass and 12 fail. Other jobs,
  including both Go test shards and the dashboard unit/build job, pass.
- The browser failures cover eight allowlist/domain scenarios, three
  migration/import scenarios, and one unblock-request scenario. Several
  requests return `503 current_command_unavailable`. The production gate in
  [serving_routes.go](../../../cmd/kudurru/serving_routes.go) deliberately
  refuses these writes until they have complete current command admission.
- `VERSION` is `0.2.0`. Some historical planning text and the PR description
  still name `0.1.0` or older base commits; those statements are not evidence
  of current release readiness.

## First implementation batch

These are regressions in existing public workflows and take priority over
new feature breadth. Each row gets a separate implementation commit. Keep
the original raw credential, writer fencing, transactional ownership and
final expiry checks; removing the unavailable-route gate alone is not a fix.

| ID | Gap and evidence | Work | Required acceptance | Status |
|---|---|---|---|---|
| G01 | Confirmed: allowlist, denylist, domain and query-log convenience mutations lack current command admission; eight browser scenarios fail | Plan bounded list/policy edits in the concrete current transaction; authorize the complete operation, including no-ops; mount only supported routes | Existing domain API behavior; owner/permission/expiry/rollback/follower tests; real SQLite HTTP and DNS publication; `dashboard/e2e/allowlist.spec.ts` | Assigned: domain-command agent |
| G02 | Confirmed: migration apply/start/adopt/decommission remain gated; three migration browser scenarios fail | Carry original current authority through migration writes, durable reports and background work; preserve explicit partial-success semantics without unchecked retries | Preview/apply consistency, managed ownership, privilege revocation, cancellation/restart and journal integrity; `dashboard/e2e/migration.spec.ts` | Assigned: migration-command agent |
| G03 | Confirmed: unblock approval/denial is gated; block-page approval browser scenario fails | Admit request decisions and their list/policy effects as a complete current command; preserve decision-only permission and time-limited approval ownership | Approve/deny/revoke/expiry/no-op behavior, atomic failure, original credentials, restart and real DNS; `dashboard/e2e/requests.spec.ts` | Assigned: unblock-command agent |
| G04 | Confirmed: full CI is not green | Review and compose the three fixes, rerun existing browser acceptance and relevant Go/race checks, then inspect CI on the pushed commit | All existing checks pass without skipping failing scenarios or loosening admission | Pending integration |

## Remaining feature packages

The implementation claims here summarize the current tracker; none closes an
entire package. Split large rows into smaller commits when their work starts.

| ID / package | Existing foundation | Remaining work and validation |
|---|---|---|
| G05 / V2-ZONE | Secondary/stub acquisition, AXFR/IXFR, TCP/TLS/TSIG, signed copies, inbound NOTIFY, durable expiry and diagnostics | Recorded: remaining source/peer integration and full multi-replica acceptance. Reconcile BIND/Knot interoperability, fallback, serial wrap, corrupt transfers, restart, expiry, UI and CLI evidence against the final build. |
| G06 / V2-UPDATE | RFC 2136 engine, durable heads, SQLite lifecycle, native TSIG admission and durable NOTIFY | Recorded: Kubernetes barriers and HA forwarding. Prove atomic prerequisites and concurrent file/API/update writers, ownership, serial/NOTIFY behavior and recovery on both production drivers. |
| G07 / V2-DNS | DNSSEC/NTAs/EDE, raw cache provenance, synthesis, filtering, rewrites and contextual modifiers | Recorded: real edge cache isolation, complete modifier conformance and scale checks. Audit RFC 8198, RFC 5011, DNS cookies and other resolver follow-ups individually. Run black-box AdGuard conformance and matcher benchmarks; preserve AD/CD/DO and client-policy isolation across cache/reload paths. |
| G08 / V2-RPZ | Compiler, per-node authenticated acquisition, persisted expiry and policy runtime composition | Recorded: remaining public diagnostics/UI and final feed-update/expiry acceptance. Verify supported triggers/actions, precedence, unsupported-rule accounting and explanation output; RPZ publishing remains v1.0. |
| G09 / V2-INTEGRATIONS | Host, ledger, planner, Cloudflare adapter, UniFi read acquisition and inventory codecs | Recorded: production source transactions and durable allocations, optional UniFi writes, Tier-B webhook/conformance CLI, external-zone sources and administration UI. Prove preview/apply equivalence, foreign-record protection, split-horizon privacy, bounded retries and per-instance failover. |
| G10 / V2-IDENTITY | Immutable inventory, MAC/hostname matching, encrypted-transport IDs and authenticated PROXYv2 propagation | Recorded: complete provider fill, inventory UI and edge rendering. Validate stale/ambiguous assignments, collision stability, CIDR precedence and equivalent decisions across native and edge transports. |
| G11 / V2-HA | Complete replication images, atomic installation, authority fencing, authenticated transport and promotion primitives | Recorded: peer transition exclusion, join tokens/wizard/CLI, write forwarding and multi-process partition/rejoin acceptance. Prove deletes, wrong-cluster/schema rejection, conflicting promotions, primary loss and restart while DNS continues from valid local data. |
| G12 / V2-IAM | UID-bound credentials, IAM schemas, migration primitives, durable sessions/TOTP, browser OIDC/CEL and local account commands | Confirmed CLI supports local password/token login, not browser/device login. Recorded: remaining administrative writers, provisioning/group synchronization, RP logout and opt-in Kubernetes impersonation. Validate migration, revocation, scoped grants, restart and follower promotion for every supported route. |
| G13 / V2-OBS | Local query/audit storage, central shipping and sink interfaces | Recorded: bounded dnstap emit/listen, OTel/TRACEPARENT, ClickHouse/Loki exporters, privacy/loop protection, drop/error reporting and central retention follow-ups. Confirmed remote node-health route still returns 501. Demonstrate slow exporters cannot block DNS, local persistence or live tails. |
| G14 / V2-EDGE | Existing supervisors, rendered topology, local invalidation and VIP deployment | Recorded: console diff-apply, certificate reload, dynblock push, remote invalidation, host settings and VIP/provider event/ownership reporting. Reconcile already merged supervisor work before adding it again; validate with real dnsdist and VIP failover. |
| G15 / V2-OPS | Current object/settings/zone/source/cache commands, captured reads and Enterprise branding | G01–G03 restore existing workflows. Recorded: remaining route adoption, offline importers and loss reports, direct kubeconfig context, generated client adoption, CLI conformance, node-setting persistence/help and operational follow-ups. Validate retained branding settings and license fallback against final artifacts. |

## Active Directory DNS replacement

This scope was added to v0.2 by D222 and must not disappear because the older
11-package table predates it. The
[GSS-TSIG probe](../../spikes/ad-dns/README.md) is an isolated module outside
the production binary and deliberately refuses every UPDATE.

| ID | Remaining requirement | Evidence needed |
|---|---|---|
| G16 / AD1–AD2 | Discovery, metadata-aware import and ownership handoff | Real forest inventory, blocker/loss reports, shadow comparison and final synchronization |
| G17 / AD3–AD7 | Production authority/service discovery, GSS-TSIG/TKEY admission, principal permissions and record ownership | Actual Windows DC/member registrations, site/forest locator records, signed responses, rotation and atomic durable updates; shared-key TSIG tests alone are insufficient |
| G18 / AD8–AD11 | DHCP registration/name protection, aging/scavenging, metadata replication and administration | Windows DHCP lifecycle, static-record protection, preview/audit, restart, backup/restore and fenced failover |
| G19 / AD12 | Cutover, rollback and supported Windows matrix | AD health and lifecycle with Windows DNS stopped on every DC; rollback retaining post-cutover changes; documented tested OS/topology limits |

## Cross-cutting release gates

| ID | Gate | Closure rule |
|---|---|---|
| G20 | Reconcile the follow-up ledger | Classify every v0.2 row as implemented with evidence, still required, or explicitly retargeted with rationale. Historical deferrals must not override working code or silently reduce agreed scope. |
| G21 | Production wiring and public contracts | Every feature has its required API/CLI/UI, captured reads/current writes, generated schemas/clients, permission rules, operator docs and architecture updates. No unexplained 501/503 remains in supported workflows. |
| G22 | Upgrade and failure acceptance | Run supported-driver upgrades, mixed-version compatibility/rejection, outage startup, durable restart, promotion and partition/rejoin against the final built artifacts. |
| G23 | Interoperability and performance | Record real BIND/Knot/AdGuard/dnsdist/Windows results where required, bounded memory/queues and performance budgets; fixture-only evidence is labeled as such. |
| G24 | Deployment and release qualification | Complete the applicable [release checklist](../../release-checklist.md), including Compose/Kubernetes, two-VM keepalived failover, licensing and artifact validation. Do not publish or promote release branches merely to close this ledger. |
| G25 | Status and evidence consistency | Update this ledger, current roadmap progress, changelog and PR description with exact implementation commits, commands, outcomes and remaining limits. All 11 packages require explicit acceptance before claiming feature completion. |

## Completed work in this pass

Implementation commits and validation results will be recorded here as the
first batch is reviewed and integrated. A failed or unrun check stays visible;
an agent's completion report alone is not acceptance evidence.
