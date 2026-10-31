# v0.2.0 implementation and validation gaps

Baseline: 2026-10-31, PR [#54](https://github.com/robertherbaugh/kudurru/pull/54),
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

## Baseline evidence

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
- `VERSION` is `0.2.0`. At baseline, historical planning text and the PR
  description still named `0.1.0` or older base commits; those statements are
  not evidence of current release readiness.

## First implementation batch

These were regressions in existing public workflows and take priority over
new feature breadth. Each row gets a separate implementation commit. Keep
the original raw credential, writer fencing, transactional ownership and
final expiry checks; removing the unavailable-route gate alone is not a fix.

| ID | Baseline gap and evidence | Work | Required acceptance | Status |
|---|---|---|---|---|
| G01 | Confirmed: allowlist, denylist, domain and query-log convenience mutations lack current command admission; eight browser scenarios fail | Plan bounded list/policy edits in the concrete current transaction; authorize the complete operation, including no-ops; mount only supported routes | Existing domain API behavior; owner/permission/expiry/rollback/follower tests; real SQLite HTTP and DNS publication; `dashboard/e2e/allowlist.spec.ts` | Implemented in `4db982a`; local command, DNS/restart and browser acceptance pass |
| G02 | Confirmed: migration apply/start/adopt/decommission remain gated; three migration browser scenarios fail | Carry original current authority through migration writes, durable reports and background work; preserve explicit partial-success semantics without unchecked retries | Preview/apply consistency, managed ownership, privilege revocation, cancellation/restart and journal integrity; `dashboard/e2e/migration.spec.ts` | Implemented across both batches: atomic reviewed adoption and canonical imported settings now extend `1e2c2d0`; composed validation is recorded below |
| G03 | Confirmed: unblock approval/denial is gated; block-page approval browser scenario fails | Admit request decisions and their list/policy effects as a complete current command; preserve decision-only permission and time-limited approval ownership | Approve/deny/revoke/expiry/no-op behavior, atomic failure, original credentials, restart and real DNS; `dashboard/e2e/requests.spec.ts` | Implemented in `63f2e53`; local command, DNS/restart and browser acceptance pass |
| G04 | Confirmed: full CI is not green | Review and compose the three fixes, rerun existing browser acceptance and relevant Go/race checks, then inspect CI on the pushed commit | All existing checks pass without skipping failing scenarios or loosening admission | First batch green on `629146f` (CI `41170330357`, security `41170330451`); rerun required for this batch |

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
| G21 | Production wiring and public contracts | Domain and unblock captured GETs are completed in `1f3b0a4`; the broader gate remains open. Every feature has its required API/CLI/UI, captured reads/current writes, generated schemas/clients, permission rules, operator docs and architecture updates. No unexplained 501/503 remains in supported workflows. |
| G22 | Upgrade and failure acceptance | Run supported-driver upgrades, mixed-version compatibility/rejection, outage startup, durable restart, promotion and partition/rejoin against the final built artifacts. |
| G23 | Interoperability and performance | Record real BIND/Knot/AdGuard/dnsdist/Windows results where required, bounded memory/queues and performance budgets; fixture-only evidence is labeled as such. |
| G24 | Deployment and release qualification | Complete the applicable [release checklist](../../release-checklist.md), including Compose/Kubernetes, two-VM keepalived failover, licensing and artifact validation. Do not publish or promote release branches merely to close this ledger. |
| G25 | Status and evidence consistency | Update this ledger, current roadmap progress, changelog and PR description with exact implementation commits, commands, outcomes and remaining limits. All 11 packages require explicit acceptance before claiming feature completion. |

## First batch implementation and validation

Three isolated subagents produced separate signed-off commits, reviewed and
composed on the existing PR branch:

- `1e2c2d0`: current migration apply/start and authority cutover; each destination
  step and durable outcome commit together. History uses the original complete
  image. Object steps support DNSZone, DNSRecordSet, BlockList, ClientGroup and
  Policy. Adoption remains gated. Imported settings after successful object
  steps produce an explicit durable unavailable error; after object failures
  they remain `not_applied`. Revocation, cancellation and writer loss preserve
  the last authorized journal state and never trigger privileged cleanup or
  automatic replay.
- `63f2e53`: atomic unblock approval, denial, deletion and elected cleanup;
  original credential and license deadlines, shared-entry ownership, canonical
  retention settings, and cache invalidation across every scope.
- `4db982a`: current domain/allowlist/denylist/query actions and list exceptions;
  bounded detached plans preserve delegated permissions, managed ownership,
  batch item isolation and original source-body receipts. Existing domain GET
  projection work is unchanged and remains part of G21.
- `608e2f0`: composed route acceptance and design decision D223.
- `ffde215`: lint cleanup for the new adapters and removal of the obsolete
  cache query-type list.

Combined validation on the implementation commits:

- All 13 existing `allowlist.spec.ts`, `migration.spec.ts` and `requests.spec.ts`
  scenarios pass against the rebuilt SQLite binary (1.1 minutes), including
  every previously failing scenario. No browser assertions were changed.
- Full affected-package races pass for `internal/api`, `internal/domainlist`,
  `internal/unblock`, `internal/servinghttp` and `internal/servingbuild`
  (`GOMAXPROCS=3 go test -race -p 2`, fresh run). The scoped current-command and
  mounted DNS/restart runtime races pass in 123.317 seconds with `licensetest`.
- Scoped `go vet` and CGO-disabled compilation pass for those packages and
  `cmd/kudurru`. The dashboard build and CGO-disabled test-license binary build
  pass; the binary is 73,568,416 bytes against the 100,000,000-byte budget.
  The tracked dashboard placeholder was restored.
- After `ffde215`, scoped golangci-lint reports zero issues and the route test
  passes. The final mounted unblock DNS/restart race also passes (8.610 seconds).
- Security [41169879571](https://github.com/robertherbaugh/kudurru/actions/runs/41169879571)
  passes all applicable PR jobs on integrated commit `608e2f0`, including
  SAST, Go vulnerability reachability, filesystem and both image scans.
- Full existing browser acceptance passes: **147/147** in 3.9 minutes with two
  Chrome workers. The hosted dashboard browser job also passes in
  [CI 41169879456](https://github.com/robertherbaugh/kudurru/actions/runs/41169879456).
  That run predates lint cleanup `ffde215`; the final hosted CI result remains
  G04. Check the PR's current commit for the final rerun.

These checks do not close the remaining feature packages or release gates.

## Second batch: remaining migration flows and captured reads

Three isolated subagents supplied separate signed-off commits:

- `f4e3416`: selected imported settings merge against current canonical state
  and commit with the final durable outcome. Ordinary PATCH shares validation
  and persistence without merging resolved map values twice. Null resets, map
  deletion, host/node/env restrictions, original branding entitlement, no-ops
  and final credential checks remain enforced. Object failures leave settings
  `not_applied`; validation errors remain durable and settings stay unchanged.
  Background reports retain restart requirements in Settings item hints.
- `1f3b0a4`: domain, Allowlist, Denylist and list-exception GETs project original
  objects and compiled lists; unblock GET/list uses captured workflow rows,
  canonical pending expiry and the original finite entitlement. Cursors bind
  the complete publication and normalized query/page size. Missing capabilities,
  expired authority and stale cursors never fall back to live state.
- `d702f96`: one bounded current adoption command validates every selected prior
  API import's UID, resourceVersion, manager and authority, then authenticates
  once for settings and every zone scope. All run annotations and the completed
  journal commit together. Records, specs and authority remain unchanged.
  The existing-import listing now includes UID; generated clients and saved
  review payloads carry it through adoption.

D224 records these decisions. Operator guidance and the roadmap now describe
the resulting behavior; this batch does not close the eleven feature packages,
AD replacement, or release qualification. G21 also remains open beyond these
convenience routes.

Captured domain projections retain the existing 10,000-object/32 MiB bounds.
Old external list bodies may be evicted before a held request opens them,
returning unavailable. Pagination must restart after publication changes.
Adoption accepts at most 1,000 zones and preserves the 1 MiB journal limit.
Interrupted background imports retain the last admitted report until their
existing interruption deadline; they do not automatically replay refused work.

Validation recorded during second-batch integration:

- The final first-batch commit `629146f` passes all hosted CI jobs in
  [41170330357](https://github.com/robertherbaugh/kudurru/actions/runs/41170330357)
  and all applicable security scans in
  [41170330451](https://github.com/robertherbaugh/kudurru/actions/runs/41170330451).
- Full `internal/servingbuild` race tests pass in 131.736 seconds
  (`GOMAXPROCS=2 go test -race -p 2 ./internal/servingbuild -count=1`).
- The new settings-only browser scenario passes against the rebuilt binary,
  including durable report reload, persisted readback and restart hints. An
  earlier run stopped at fixture login with a 503 before entering the workflow;
  no handler failure was retried or assertion weakened.

- Both new browser scenarios pass on the integrated binary (2/2, 14.7 seconds):
  historical import adoption, record/spec/UID preservation and real DNS authority
  cutover; settings-only background completion, saved-value readback, restart
  hints and report reload. The previous binary failed both workflows as expected.
- Regenerated OpenAPI 3.0/3.1, Go and TypeScript clients add only the optional
  migration UID. Dashboard typecheck, scoped ESLint and all 10 migration/history
  component tests pass. The dashboard bundle is 301.1 KiB gzip on the entry
  path (600 KiB budget), and the CGO-disabled embedded binary is 73,724,064 bytes
  (100,000,000-byte budget). Tracked build output is restored.

- Composed runtime races pass in 97.116 seconds with `licensetest`, covering
  `TestServing(ImportSettings|Migration|AdoptionActualHTTPPreservesDataRestartAndCutover|CanonicalSettings|BrandingFinalClock|MutationRoutes|Domain|Unblock)`.
  Agent adoption races also pass all mounted adoption/gate cases in 122.202
  seconds, including simultaneous requests, restart/promotion/cutover, final
  expiry and complete authorization. Its fixture waits for actual primary
  admission/publication; no refused mutation is replayed.
- Scoped vet and CGO-disabled compilation pass for API, servingauth,
  servingbuild and cmd/kudurru. Scoped golangci-lint reports zero issues.
  Generated reference/client tests and dashboard embedding tests pass.

- Full `internal/api` and `internal/servingauth` race suites pass in 154.112
  and 32.085 seconds respectively (`GOMAXPROCS=3 go test -race -p 2
  ./internal/api ./internal/servingauth -count=1`), including OpenAPI drift
  and the composed public read/write contracts.

- Full expanded browser acceptance passes: **149/149** in 3.5 minutes, with
  two Chrome workers against the integrated SQLite binary. Existing assertions
  and production admission checks remain intact.
- A separate bounded static review of production changes `629146f..16ccdee`
  found no actionable defects across adoption, settings and captured-read
  interactions. Hosted CI/security must still validate each pushed head; see
  PR #54 for the current result.
