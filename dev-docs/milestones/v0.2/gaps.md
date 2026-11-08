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
| G04 | Confirmed: full CI is not green | Review and compose the three fixes, rerun existing browser acceptance and relevant Go/race checks, then inspect CI on the pushed commit | All existing checks pass without skipping failing scenarios or loosening admission | First batch green on `629146f`; later CI `41174991520` fails three readiness-sensitive Go tests, corrected in separate commits below. Current-head hosted validation remains the closure gate |

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
| G13 / V2-OBS | Local query/audit storage, central shipping and sink interfaces | Recorded: bounded dnstap emit/listen, OTel/TRACEPARENT, ClickHouse/Loki exporters, privacy/loop protection, drop/error reporting and central retention follow-ups. Remote node health is implemented in `49f9e8b`; other observability and final deployment acceptance remain open. Demonstrate slow exporters cannot block DNS, local persistence or live tails. |
| G14 / V2-EDGE | Existing supervisors, rendered topology, local invalidation and VIP deployment | Recorded: console diff-apply, certificate reload, dynblock push, remote invalidation, host settings and VIP/provider event/ownership reporting. Reconcile already merged supervisor work before adding it again; validate with real dnsdist and VIP failover. |
| G15 / V2-OPS | Current object/settings/zone/source/cache commands, captured reads and Enterprise branding | G01–G03 restore existing workflows. Offline AdGuard/Blocky converters and loss reports plus audit verification CLI are implemented in this pass. Remaining: route adoption, direct kubeconfig context, generated client adoption, broader CLI conformance, node-setting persistence/help and operational follow-ups; use the item-level audit for current closure evidence. Validate retained branding settings and license fallback against final artifacts. |

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
| G20 | Reconcile the follow-up ledger | Completed by `b6f8d6e`: [75 original targeted rows, 130 subrequirements](followup-audit.md), with evidence and remaining acceptance; subsequent closures are tracked separately. No requirement was silently retargeted. |
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


## Third pass: reconcile the entire original review

The previous five-commit batch covered three remaining migration/read gaps,
plus tests and documentation; it did not complete the original feature review.
`b6f8d6e` adds the [full follow-up audit](followup-audit.md). At its `b42fa24`
baseline, 130 subrequirements were classified as 18 implemented, 25 partial,
76 missing and 11 external/decision. These are baseline counts, not current
release totals. Duplicate source rows share completion evidence. AD replacement
and all G21–G24 gates remain in scope.

| Item | Separate implementation commits | Completion and limits |
|---|---|---|
| G04: obsolete mounted route assertion | `7a28a44` | Adoption now reaches request-body validation. Focused actual serve/restart race passes. |
| G04: authority-cutover publication stall | `f5624e7` | An active member whose original desired source/frozen configuration is obsolete returns superseded, allowing the serial worker to reconcile. Never acknowledges old data or retries a refused HTTP write. |
| F33 / G13: remote node health | `49f9e8b` | Membership-only authenticated peer health, two-second total deadline, 1 MiB cap, strict peer identity and redacted failures. Older peers return explicit unsupported errors. |
| F60a / G15: offline import commands | `48b11dd`, `32f9e81`, `f01d4db`, `54fcf48`, `01f35ed`, `39b13dc` | Shared reviewed plan machinery, separate AdGuard Home and Blocky adapters. Bounded YAML, no source fetching, deterministic selection and explicit losses. Supported subsets are intentional, not full source-product migration parity. |
| F75b / G13/G15: audit verification CLI | `a097836` | Existing verifier endpoint with human/JSON output and nonzero tamper/unavailable/denied results. Real-router SQLite tamper, permission/license and cancellation tests pass. |
| F24a / G07/G13: slip metric | `661174c` | Counts UDP truncated-response decisions separately from drops, survives reconfiguration, no client labels. Native UDP/TCP race acceptance passes. Group exemptions remain F24b. |
| F74 / G13: central Grafana panels | `60a3af1` | Central availability, query shipping lag and audit spool drops; source/Helm JSON agree and existing dashboard/metrics/alerts tests pass. Missing central series are not reported as failures. |
| F20b/F68a / G07/G23: AdGuard conformance execution | `f5d5316` | CI runs the existing first-party corpus against the checksum-pinned official v0.107.79 executable. Actual race comparator passes; two known deviations stay explicit. No copyleft source/data/executable is vendored. Full corpus/scale qualification is separate. |
| F35c/F49 / G09/G12: webhook authentication | `5fc97d7` | Optional host-only bearer token file, bounded startup read, constant-time check before dispatch and restart-required rotation. Actual listener, configured startup failure and existing write-gate races pass. Default disabled; upstream v0.23.0 requires a trusted proxy to inject the header. |
| F57 / G15: nested settings help | `d37f4ad` | Recursive schema help for list items, map keys/values and nested objects, preserving explicit field help. Accessible rendering passes 59 focused UI tests; Go metadata/serialization and existing rejection cases pass. |
| F20c / G15: benchmark gate location | `a2bb3bf` | Unchanged gate moved to `hack/benchgate`; existing races and baseline self-check pass. Historical measured outputs are preserved; no new performance claim. |

Validation of the publication fix:

- Hosted CI `41171975087` on `b42fa24` failed the stale adoption assertion and
  the new cutover browser case (148/149 passed). Its trace shows a successful
  cutover response followed by repeated `SERVFAIL / servfail_unsynced`.
- The old binary reproduces the browser failure in 1 of 10 repeated scenarios.
  Four deterministic Go cases reproduce waiting on a changed nameserver source,
  status update, deletion and UID replacement before the fix.
- After the fix, all publication races pass, the full primaryruntime race
  passes with actual Kubernetes envtest in 55.305 seconds, the driver race
  passes in 17.713 seconds and the full servingbuild race passes in 141.841
  seconds. The first primary run lacked envtest binaries; `make envtest`
  installed the pinned assets and the complete package was rerun successfully.
- Rebuilt-binary migration browser acceptance passes **10/10** with two workers
  and five repetitions. Assertions, 10-second expectations and 30-second test
  deadlines remain unchanged. Source publication lint reports zero issues.
- `make test-adguard` executes the external comparator successfully under the
  race detector. Benchgate relocation passes its race suite and unchanged
  captured baseline self-comparison. Dashboard JSON generation/metric existence
  and Prometheus rule checks pass.

The final combined checks and hosted results for this pass are recorded below.
The separate Cloudflare docs check is understood and excluded from this pass
at the user's direction. `origin/main` was fetched and remains fully merged.


Combined validation in this pass:

- Generated OpenAPI 3.0/3.1, Go/TypeScript clients and CLI/settings references
  are synchronized in `26fa782`; contract/reference drift checks pass.
- Dashboard typecheck, full ESLint/theme checks and all 59 focused field/settings
  tests pass. The rebuilt dashboard entry is 301.1 KiB gzip against the 600 KiB
  budget. The local Hugo build passes for the changed migration/operator docs;
  this does not resolve or claim a result for the excluded Cloudflare check.
- Composed Go lint over CLI/API/config/importers, cluster health, primary runtime,
  serving builder, ingress/DNS listeners, external-dns and benchgate reports zero
  issues.
- A separate static review found two concrete converter defects: forward-mode
  omission when merging over recursive destination groups, and Blocky inline
  regex entries exceeding the destination schema bound. `01f35ed` explicitly
  emits forward mode, with actual canonical migration over existing recursive
  groups; `39b13dc` applies the schema character bound, with ASCII and Unicode
  1,024/1,025-character preview and actual-admission tests. Unrelated groups
  and valid entries are preserved; incomplete filtering groups remain detached.

- Full browser acceptance passes **149/149 in 4.4 minutes** against the rebuilt
  SQLite binary after `5fc97d7`, including migration cutover, settings, access,
  secondary/stub expiration and real DNS answers. Assertions, timeouts and
  retries are unchanged. The first launch lacked the required KUDURRU_BIN and
  was stopped at fixture setup; the complete rerun supplies the absolute binary
  path. The embedded CGO-disabled test-license binary is 73,875,616 bytes,
  below the 100,000,000-byte limit; tracked build output is restored.

- The new webhook listener test reproduced a client-cleanup race (15/50 old
  runs timed out during shutdown). `ce07354` drains response bodies and closes
  its dedicated transport before server cancellation. It passes 200 listener
  race repetitions and 20 shuffled full package runs. Production shutdown
  deadlines, authentication and dispatch are unchanged.

- Offline CLI races initially exhausted the existing three report-read attempts
  while complete generations continued to publish (installed/served 9/8,
  13/9, 17/15); the accepted job converged without replay 597 ms later.
  A separate DNS assertion observed the legitimately served earlier primary
  head before the imported RecordSet receipt. `c12dd3e` observes terminal
  durable work, exact publication/primary acknowledgment and the committed
  RecordSet UID/version/intent before one-shot CLI readback and DNS assertions.
  It preserves selected/settings `--wait`, actual partial/nonzero outcomes,
  cancellation and bounded-poll/no-replay coverage. Production code and
  deadlines are unchanged. Five repeated full/current/cancellation races pass
  in 125.274 seconds; the final partial `--wait` case passes ten repetitions
  in 46.260 seconds; final scoped offline/CLI races pass in 43.501 seconds.
- Both static-review fixes pass importer/API races; the integrated importer/API
  selection passes in 1.159/3.673 seconds. Tests use the actual canonical
  migration and embedded schema, including the distinct Unicode character
  bound for direct object imports.

- Final composed CLI/webhook races on the integrated converters and fixtures
  pass (49.234/1.327 seconds): offline AdGuard/Blocky conversion/application,
  selected/settings/partial completion, cancellation, no replay, audit verify,
  webhook authentication and actual invalid-startup coverage. The final embedded
  binary rebuild passes and remains 73,875,616 bytes. Hosted CI/security on the
  pushed head remains the authoritative final runner result.

### Hosted validation and readiness corrections

On `9e754cb`, all applicable [security jobs](https://github.com/robertherbaugh/kudurru/actions/runs/41174991522)
pass, including SAST, Go vulnerability reachability, filesystem and both image
scans. [CI 41174991520](https://github.com/robertherbaugh/kudurru/actions/runs/41174991520)
passes browser acceptance, BIND/Dex interoperability, the mandatory AdGuard
comparator, lint/vet, build, licenses, Helm, PostgreSQL 16/18, DHCP and all seven
fuzz shards. Go shard 2 passes. Go shard 1 fails three tests; this is not a green
CI result:

- `TestPrimaryRuntimeHeldNativeRequestCannotAdoptNewKeyring`: the fresh UPDATE
  after restoring the original keys receives SERVFAIL.
- `TestServingAdoptionActualHTTPPreservesDataRestartAndCutover`: the single
  decommission POST receives 503, current writable serving generation unavailable.
- `TestServingZoneNotifyRejectsFallbackWithoutIntent`: the fallback POST
  receives 503 before the expected semantic rejection, 422.

The adoption reproduction observes a current elected writer and committed
records, but a pending native NOTIFY acknowledgment at the old helper's exact
successful readiness boundary. Even with no targets, acknowledging intent
commits later metadata and can supersede a captured HTTP request. Fallback
configuration also changes the content hash and queues this acknowledgment.
Restore readiness alone does not establish completion of this background work.
These observations do not justify weakening the original request proof or
replaying a refused mutation.

`532c2b8` waits for the exact fallback source, admitted route, head without
pending intent and matching driver acknowledgment before the single POST. It
retains the 15-second fixture deadline, expects exactly 422 and verifies no
durable write. Twenty race repetitions pass in 62.531 seconds; focused normal
execution and scoped lint pass. The original hosted fallback failure did not
naturally reproduce in 30 local baseline/diagnostic repetitions, so this is
source/acknowledgment evidence rather than a claimed local before/after failure.

`da54ede` settles adoption and offline-import fixtures against the same complete
image: current live primary membership, committed RecordSet UID/intent, cleared
notification intent, exact driver head acknowledgment and current elected
writer. The existing 100 ms interval now tracks the full publication identity;
the two-minute fixture bound and one-shot requests remain unchanged. Historical
or orphaned heads and records outside current membership do not create false
waits. All adoption and offline CLI consumers pass under races in 149.523
seconds; scoped lint reports zero issues. The focused restart/cutover case also
passes ten repetitions before the final historical-head scoping refinement.

`994f655` fixes a distinct native-keyring fixture boundary. Two of 30 diagnostic
race attempts reproduce the fresh UPDATE refusal while the restored transport
keys are current but the same image's durable UPDATE policy still references
the intervening keyring. No NOTIFY intent is pending in those samples. The
fixture now verifies the exact published member/head and digest-verified policy
chunks from one matching complete image, then checks the original writer lease
before its single fresh UPDATE. The held request and stale-listener proof still
fail, and the held record must remain absent. All 30 repeated one/two-CPU race
attempts pass in 60.776 seconds; scoped lint passes. Failure output now contains
only numeric response codes and transport-error presence, not DNS/TSIG messages.
No production admission code, retry behavior or deadline is changed.

After composing all three fixes, the combined primary-runtime, adoption,
offline CLI and zone-NOTIFY race selection passes in 172.369 seconds (62 test
and subtest results, zero failures or skips). Composed CLI-package lint reports
zero issues, and `git diff --check` passes. This confirms local composition;
the PR's current-head hosted run remains the final CI result.

Compose, Edge, kind/MetalLB, GitHub docs, development image, license server,
website and the isolated AD probe workflows also pass on `9e754cb`. The probe
does not certify production AD support. The excluded Cloudflare check remains
separate.

### VIP cutover browser follow-up

[Security 41177243778](https://github.com/robertherbaugh/kudurru/actions/runs/41177243778)
passes all applicable checks on `a05a7cc`. Its
[CI run](https://github.com/robertherbaugh/kudurru/actions/runs/41177243742)
exposes a separate VIP cutover browser failure: the single decommission POST
returns 409, `A zone changed after the preview`, instead of completing the
nameserver update. The hosted preview and submitted payloads are identical.

Two isolated diagnostic repetitions reproduce the same refusal with the
original reviewed plan. Only the zone's resourceVersion differs; UID, spec,
metadata generation, name, namespace, error and generated address records are
unchanged. Public primary status advances the resourceVersion while the new
zone becomes ready. A second read-only preview supplies comparison evidence;
it never replaces the original plan and the refused write is never repeated.
The hosted trace does not include the fresh transaction payload, so its exact
apply-side resourceVersion is unknown.

`47b5e7c` waits for the imported primary's Ready condition and observed
generation before opening the VIP cutover preview. The reviewed plan, one-shot
UI submission, success message, DNS assertions, console-error checks and
deadlines are preserved. Ten VIP repetitions pass in 57.6 seconds; all three
migration-file scenarios pass in 14.6 seconds. Dashboard typecheck and scoped
ESLint pass.

The full CLI-package race passes on `a05a7cc` in 788.663 seconds, including the
three earlier Go regressions. The same hosted Go shard fails
`internal/dhcpha.TestSnapshotCatchupForOfflineVoter`: an offline replica does
not reach identical committed lease/configuration/claim state within the
existing eight-second consistency bound. This is investigated separately below; the previous fixture corrections do
not claim to fix it. Browser acceptance
on that head is 148/149. All other workflows, including kind/MetalLB, pass.

The DHCP investigation identifies an unsupported fixture deadline. The pinned
Raft v1.7.3 independently backs off failed log replication for up to 10.24
seconds. Holding the voter offline for eleven seconds before the original
single offers/ACKs reproduces the eight-second consistency failure twice.
At that deadline the returning voter has applied index zero, no storage failure
and no leases; only applied index and lease rows differ. Full equality then
recovers at 9.201 and 9.279 seconds after restart, with all three replicas
initialized, healthy, at index 123 and holding 90 leases. This establishes late
healthy recovery rather than corruption at equal indexes. The original hosted
failure lacks those detailed indexes, so its exact retry phase is not known.

`356474b` gives only the snapshot-rejoin fixture a derived 18.24-second budget:
one maximum 10.24-second retry delay plus its existing eight-second convergence
margin. Ordinary consistency checks stay at eight seconds. Complete state
equality, compaction, single offers/ACKs, current authority and production
command deadlines remain enforced. Both controlled long-outage cases pass
with that bound in 46.824 seconds. Operator and architecture guidance now
distinguish peer connectivity, replication catch-up and command deadlines.

Final DHCP acceptance preserves the original assertions and adds safe terminal
progress diagnostics. Ten focused one/two-CPU race runs pass in 37.555 seconds;
the complete DHCP HA package race passes in 22.098 seconds and scoped lint
reports zero issues. Intermediate extra snapshot/FSM-view assertions were
removed during review because they imposed unrelated recovery/publication
requirements; they are absent from the committed patch. The final browser and
DHCP changes touch separate test files and compose without conflicts. Latest
`origin/main` (`d281123`) remains fully merged. Current-head hosted results are
recorded on the PR after this push.

### Atomic cutover completion

[Security 41179171856](https://github.com/robertherbaugh/kudurru/actions/runs/41179171856)
passes all applicable checks on `d313092`. The same head's
[CI run](https://github.com/robertherbaugh/kudurru/actions/runs/41179172005)
exposes a distinct failure in the explicit-nameserver migration-history browser
case. Its single decommission request returns HTTP 200 with a committed zone
update and an explicit journal error: the separate terminal report command was
refused because the current writable serving generation was unavailable. This
establishes the completion boundary failure; the trace does not identify which
publication advanced or show that the refusal itself was incorrect.

`f17473e` (D229) moves cutover completion into the last selected zone's existing SQLite
transaction. The zone, generated VIP records, outcome and completed/partial
status now commit together. Earlier zones retain their committed outcomes when
a later command fails. The same settings/zone/record authorization, original
request proof, elected writer and final credential/license checks remain.
Import finalization and the general current-command protocol are unchanged.

The new regression reads the durable journal after every commit. Before the
fix, all six single/multi-zone, earlier/last-error, unchanged and VIP cases
deterministically expose all zone outcomes while the report remains running
without a finish time. Refused commands, final commit-check failure and journal
failure must roll back the last zone, generated records and report together,
preserving earlier outcomes and never replaying the refused command. A mounted
runtime regression also expires the original credential at the final commit
check for the last zone, covering both VIP writes and unchanged authority.

Focused API cutover races pass in 9.752 seconds. Three repetitions of the
mounted final-expiry races pass in 40.339 seconds, including the original
initialization-expiry case. Scoped API/CLI lint reports zero issues. Against
the rebuilt embedded application, the formerly failing history scenario passes
ten repetitions in 52.0 seconds, and all three migration-file browser scenarios
pass in 15.8 seconds. Assertions, deadlines and one-shot mutation behavior are
unchanged. An earlier browser launch stopped at fixture setup because the
binary embedded the placeholder dashboard; it did not exercise cutover.

Both hosted Go shards, including DHCP snapshot catch-up, pass on `d313092`.
Its only failing CI test job is the browser case corrected here. All separate
GitHub workflows pass, including security, kind, Compose and Edge. The excluded
Cloudflare check remains separate. Current-head combined and hosted results
follow in the PR validation record.

Final local composition on `f17473e` passes all **149 browser scenarios** in
3.9 minutes against the rebuilt application. Combined current-cutover,
migration and adoption races pass 61 test/subtest results with zero failures or
skips (`internal/api`: 10.347 seconds; `cmd/kudurru`: 166.190 seconds, with
`licensetest`). The CGO-disabled embedded binary is 73,785,504 bytes against the
100,000,000-byte limit; dashboard entry gzip is 301.1 KiB against 600 KiB.
The local documentation build and whitespace checks pass. This closes the
local cutover regression, not the remaining feature packages or release gates.

### Browser startup login follow-up

[Security 41180620042](https://github.com/robertherbaugh/kudurru/actions/runs/41180620042)
passes all applicable checks on `2048b1b`. Its
[CI run](https://github.com/robertherbaugh/kudurru/actions/runs/41180619994)
passes every migration browser scenario but exposes two distinct login/setup
failures (147/149 browser scenarios pass). Shared worker setup receives a 503
outside the permitted pre-handler `generation_pending` category, before the
branding test body starts. The community policy-limit test's single login
returns `503 unavailable`, current writable serving generation unavailable.
Neither refused login is replayed. The retained community server log belongs
to a parallel sibling: their identical filenames allowed the later teardown
to overwrite the failing server's evidence.

Ten controlled local starts all log in successfully, so they do not reproduce
the hosted refusal. However, six return from the old ops-ready check without
current zone status, and eight have subsequent zone/lifecycle metadata writes.
Independent review also identifies the seeded record set's status queue as a
separate startup writer. The hosted artifacts lack the exact writer and
publication state, so no particular write is attributed to the hosted failure.

`49a501d` changes the fixture completion check to observe only fresh-database zone, record,
lifecycle and head metadata, then verifies expected DNS and normal ops
readiness before returning the server for login. It waits for current-generation
Ready status, matching lifecycle UID/version receipts and cleared NOTIFY intent
within the original 60-second startup budget. It does not read credentials or
sessions, retry handler refusals, weaken license assertions, or change production
admission. Unique per-instance log files preserve parallel failure evidence.
The helper uses Node 24's read-only SQLite support, matching CI. These observations
settle known fixture startup work; they cannot promise admission after an
independent future change and do not generalize to restored/replaced databases.

All ten controlled starts after the change return settled metadata, then
complete exactly one successful login without subsequent changes to the
observed route/record/lifecycle/head metadata. The post-login diagnostic wait
is observation only, not part of the startup predicate. All 21 SQLite helper
unit cases pass, including stale/missing record status and read-only behavior.
Typecheck and scoped ESLint pass. Two repetitions of the branding and community
license cases pass all six scenarios in 1.7 minutes, preserving real license
withdrawal and exact 402/default-policy assertions. `93e568d` pins the regular
dashboard test job to Node 24 as well, so both test jobs support the helper's
built-in SQLite API explicitly.

After integration, all **149 browser scenarios pass with four workers** in
2.7 minutes, matching hosted concurrency. The product binary is unchanged from
the validated cutover build. Local docs and whitespace checks also pass.

The same hosted run's final Go shard also exposes two separate dry-run fixture
failures: `TestServingZoneCommandFinalExpiryIncludesEmptyAndDryRun` and
`TestServingZoneCommandReauthenticatesCurrentRawProofAndAllScopes` receive 503
before their expected 401/final-expiry and 403/current-scope decisions. Both
create a forward zone and wait only for `ProtectedReady` before the challenged
request. The exact final-clock, raw-proof reauthentication, prepared-callback
and unchanged durable-generation assertions remain required. All other CI jobs
and separate GitHub workflows pass on `2048b1b`; its overall CI result remains
failed until the browser and dry-run fixture follow-ups are validated.

Twenty local dry-run baseline repetitions pass without reproducing the hosted
503. A separate diagnostic run records 20 old readiness observations with a
current writer, matching installed/served generation and active lifecycle, but
without the driver's publication acknowledgment. This demonstrates an
incomplete fixture boundary; it does not identify the unrecorded hosted
transition. These Go fixtures run no controller, so their pending work differs
from the browser's status queues.

`e8685a8` adds a forward-zone wait that requires the same complete durable,
installed and served generation; the exact desired UID/version and active
admitted lifecycle member; matching installed/published driver acknowledgment;
and the current writer term/incarnation through the final lease check.
It runs before the scripted clock or single challenged request, retaining the
existing 15-second fixture budget. No production command, refusal handling,
credential/scope assertion or durable-generation equality check changes.

The final helper passes twenty two-CPU dry-run race repetitions (40 subtest
executions, 64.743 seconds), and ten one-CPU repetitions of all five expiry
and all three raw-proof scope modes (80 subtest executions, 162.706 seconds).
Scoped CLI-package lint reports zero issues. A broader zone-command race
selection was canceled after including unrelated expensive multi-chunk import
tests; it had no completed result and is not counted as passing validation.

The full dashboard unit suite passes **854 tests across 66 files** on Node 24
in 181.30 seconds, including the 21 startup helper checks. Combined with the
149-scenario four-worker browser pass, this validates the local composition.
The next pushed head's hosted results remain separate and are recorded on
PR #54; no feature-package or release-completion claim follows from these fixes.


### Continuing complete-audit work

[completion-plan.md](completion-plan.md) retains every one of the 130 original
subrequirements, all package dependencies and G01–G25 gates, with explicit
active owners and separate external qualification. All GitHub workflows on
`6fe025e` pass, including [CI 41182647688](https://github.com/robertherbaugh/kudurru/actions/runs/41182647688)
and [Security 41182647671](https://github.com/robertherbaugh/kudurru/actions/runs/41182647671).
The known Cloudflare docs check remains excluded by the user.

### F01/F47 integrated identity and credential boundaries

`1bf8586` requires an independently configured kube-system UID for offline
objects/settings and guards initial online requests, including auxiliary
clients. Empty pins retain online-only compatibility; saved state without
identity needs verified refresh. Initial verification is cached until restart;
endpoint retargeting requires restart and continuous replacement detection is
not claimed. D230 and the generated configuration reference record the contract.

`101b614`, `9e7b177` and `41a7694` bound Kubernetes projected credentials to
ten minutes of original lifetime on both JWKS and TokenReview paths, retain
private time evidence through delegated/per-object authorization, and bound the
complete review acquisition including pre-request throttling. A rotating
600-second audience kudurru projection is required; opaque legacy and ordinary
hour-long tokens are deliberately refused. Offline pod/ServiceAccount deletion
is bounded by original expiry, not immediately detected. D231 and the operator
projection example describe this compatibility change.

Combined real-asset package races pass: config 7.932s, settings 4.231s, localdb
2.046s, zonecache 23.589s, Kubernetes authentication 11.264s, core authentication
1.047s, auth stack 10.941s and the actual Kubernetes store 76.313s. These run
without the envtest skip flag. The agent's full TokenReview deadline acceptance
also passes three real-envtest repetitions in 13.225s, including throttling,
shorter caller/client deadlines and observed HTTP cancellation. The existing
legitimate token fixture already requests 600 seconds; refusal cases remain.

### Hosted fuzz regression: escaped SOA export

The `fuzz 2/7` job on `41ffd21` failed `FuzzImportBIND`: an escaped SOA
name exported to different text after re-import. The exact hosted corpus
`f3a91c97a2a41558` reproduces locally and is retained. `fqdn` now folds only
ASCII bytes, preserving arbitrary DNS octets, and SOA/apex NS names use the
existing canonical presentation renderer. Exact mailbox presentation assertions
now expect the canonical spelling of the same escaped label; additional
wire/key comparisons require byte preservation and a stable round trip.

Zone races pass in 3.395 seconds; the unchanged fixed-point fuzz invariant
passes a 60-second run with 109,418 executions. Scoped zone/config/store/auth/API/
CLI lint reports zero issues. This fixes the observed fuzz failure; hosted
checks on the subsequent pushed head remain separate evidence.

Composed startup/identity/health races pass in 38.391 seconds. The new health
fixture first incorrectly expected 200 for stale/unadmitted states; it now
asserts their existing 503 response. It also waits for actual snapshot admission
or explicit refusal within the original 20-second budget, because initial
unsynced and admitted-stale states share SERVFAIL/503. It then requires the
correct age for admitted fresh/stale snapshots, no age for first boot or wrong/
legacy identity, and no leader during outage. Missing host pin is covered by
the separate startup-refusal test; it intentionally cannot start offline.
Final CLI lint reports zero issues and generated configuration/CLI docs pass.

### F72 composed AuditLog diagnosis

`e77d025` implements the bounded metadata probe, existing Diagnosis response
integration and doctor reuse. Limits and exact failure semantics are recorded
in the audit closure and D235. Agent actual Kubernetes/HTTP probe races pass in
12.714s; API and CLI/startup races pass in 3.837s/18.397s. Integrated actual signed
projected-JWT tests exercise both JWKS and TokenReview mounting with original
expiry before and during acquisition, and the production audit callback refuses
wrong-cluster metadata reads through F01's guard. Composed auth/API/CLI races
pass in 6.126s/2.840s/5.719s. No full browser or image qualification is claimed.

### F22b native HTTP/3 discovery

The registry connects only identical bound native DoH/DoH3 endpoints, including
TLS file identity, request path and client-ID policy. Actual HTTP/1.1 and HTTP/2
requests receive the short-lived same-authority Alt-Svc only while HTTP/3 is
ready; a real verifying HTTP/3 client follows it with the same selector.
Plaintext, proxy headers, different public ports and nonmatching/missing/stopped
listeners do not advertise. Focused races pass in 1.299s and the full dnsserver
race suite in 30.461s. Reverse proxies and external UDP reachability remain
explicit deployment responsibilities. Response padding is recorded in the subsequent F22c closure.

### F22c negotiated encrypted response padding

The native write boundary captures the original EDNS padding offer, pads detached
responses to 468-byte blocks within peer/message bounds, and accounts for final
TSIG trailers without counting transport prefixes. Actual wire tests cover every
transport, both supported TSIG algorithms, chained signed DoT transfers, rejected
signatures, inherited padding removal, EDE/AD/DO preservation and maximum-size
answers without space for an option. A transfer test initially parsed the buffer
after the verification library had modified ARCOUNT; it now verifies a copy and
keeps exact original bytes for unchanged record/signature assertions. Final
focused races pass in 1.255s, full transport races in 30.498s, lint zero issues.
No external interoperability or final artifact qualification is inferred.


### F56 self-hosted dashboard fonts

`13c0c9e` pins both OFL font packages, includes their full license texts in the
embedded bundle and records source checksums. Kudurru theme tokens select
Inter/JetBrains Mono; unmodified Latin normal/italic variable faces cover all
four UI weights, with system fallbacks for unavailable fonts and other scripts.
The CSS uses relative hashed assets under the unchanged CSP. The budget now
counts CSS dependencies and raw WOFF2 bytes: **480.5 KiB** entry transfer against
600 KiB, with all four faces counted even if the first page does not use them.

A newly rebuilt CGO-disabled test binary passes **7 browser scenarios in
20.1 seconds**: real embedded root/prefixed assets with external requests
blocked, every normal/italic weight, actual Inter heading painting, shipped
license text, download-failure navigation and light/dark hover contrast. Nine
existing theme tests pass; typecheck, scoped ESLint, theme-generation check and
embedded-handler races pass. The final handler run occurs after the final
bundle build completes. This binary/test subset does not replace the pending
complete final artifact/browser/release gates.

### F06 independent NOTIFY key and source

`c80c6f2` and `46d7641` integrate separate backend and dashboard controls. The
explicit key is independent of transfer authorization; empty retains the old
first-transfer-key/unsigned behavior. The per-zone literal source overrides
host `notify.source`; empty preserves global/system selection. Missing keys,
rotation/revocation, unavailable/wrong-family source and current policy changes
refuse without alternate identity/source or obsolete intent acknowledgment.
Full normalized transfer policy and original elected writer/publication/final
checks remain intact. The primary-zone wizard/settings editor supports creation,
editing, clearing, validation/help and exact If-Match conflict refusal.

Regenerated manifests, Helm CRDs and embedded schemas retain the new public
fields. OpenAPI/Go/TypeScript generation runs without a wire-file change because
object schemas are generic; generated CRD/operator references change. Actual
IPv4/IPv6 UDP, distinct-key signing and current-command races pass. Combined
unoverlaid parent races: zone 1.334s, notifier 2.012s, validator 1.594s,
primaryruntime/network 43.225s, API 3.441s, cmd 37.878s, servingbuild 8.042s. This
includes F29 publication/reload checks and existing mounted NOTIFY regressions.
Integrated dashboard unit tests pass 86/86 in 7.11s with typecheck.

### F29b recursive exceptions and actual installed schema

`ddc5f2d` uses a separate bounded recursive NTA list with absolute expiry and
shared immutable matching. Active exception identity scopes native caches and
raw answers; cache restore/removal cannot renew the exception. Native provenance
marks only excepted RRsets, so expired exceptional signatures remain data while
outside alias proofs retain their own deadline and can refuse the answer.
Agent final seven-package races and ten repeats of real-socket NTA cases pass;
only the pre-existing optional live-Internet test skips. Parent regeneration
removes the temporary agent schema overlay, and the complete-publication/reload
races above pass using ordinary embedded artifacts.

Actual Kubernetes envtest contract races pass in 5.877s: the installed CRDs preserve
NOTIFY choices and the original recursive exception timestamp; direct API
updates reject invalid keys/sources/exception shapes without changing stored
resource versions. The first new fixture marshaled a whole zero-valued upstream
config, correctly rejected by existing enums; it now submits only its intended
partial settings, preserving all admission assertions. D238/D241 record the
policies. External DNS interoperability and complete release gates remain open.


F06 browser composition in `94869e5` passes with the newly rebuilt integrated
binary (11.3s): the real zone editor persists and clears both choices while
preserving transfer policy and metadata, with one PUT per action. The initial
fixture captured an ETag before first controller completion and correctly got
412. The final fixture observes Ready/current generation before opening the
editor. Existing temporary record-read 503s during policy publication are
recognized only on this test's exact record route; eventual 200, visible recovered
page and persisted values remain required. No mutation is replayed. Final
combined Go lint reports zero issues; browser typecheck and ESLint pass.

### F05/F31 composed parsing reuse and authority cookies

`eb20ac7` integrates bounded per-object parsed RR reuse in controller,
zoneview and current primary compilation. It preserves detached records,
complete source/zone identity, TTL/expiry and existing selection, reservation,
ownership, serial and publication rules. Local changed-one-of-1000-records
benchmarks measured 15.67–21.23 ms and approximately 2.99 MB/45,347 allocations,
versus 31.37–36.25 ms and 9.70 MB/249,113 allocations without reuse. These are
shared-host synthetic controller-build measurements, not full request latency
or a fixed-class performance guarantee. Whole-zone hits were measured separately.

`668203c` integrates automatic native recursive authority cookies. Actual
connected local/remote addresses scope random client cookies and bounded
server-cookie state. Malformed/spoofed responses cannot update it; BADCOOKIE,
TCP fallback and case retry share the original work budget. Rotation, original
deadlines, DNSSEC/NTA context and cache/client-answer isolation remain intact.

The integrated nine-package race run passes with actual Kubernetes envtest
assets: zone 3.561s, controller 61.445s, primaryzone 8.890s, zoneview 1.407s,
recursor 4.263s, upstream 4.102s, upstream stage 1.338s, DNSSEC stage 1.126s
and cache stage 1.684s. Only the pre-existing optional live-Internet recursion
test is unconfigured; local UDP/TCP cookie fixtures ran. Scoped combined lint
reports zero issues. The corresponding original audit rows remain historical;
the completion plan and these closures record their current implementation.

### Integration prerequisites and hosted-check follow-up

Separate signed-off commits `39f5e2f`, `aa8041f`, `28d6a5e` and `01579b2`
capture immutable provider inputs, preserve representable UniFi expiry, retain
the original host request lease and bound private MAC grants to their original
inventory source/deadline. Composed races pass for credentials, policy,
identify, integration host/planner, UniFi and Cloudflare. Production mounting,
Kubernetes atomic-source dependencies and Delete semantics remain F09b.

Hosted CI `41203722639` exposed a DoH3 idle test that observed the client's
later detection of a silent server timeout. `09c0eed` installs a test observer
before serving and verifies the actual server context and close reason, with
the existing 200 ms configuration and three-second deadline unchanged. Ten
race-enabled repetitions pass in 14.233s; full dnsserver races pass in 30.164s.
The same hosted run's authenticator status-read failure did not reproduce in
the isolated 25-test file or the complete **880/880** dashboard tests (165.28s,
two workers). That hosted failure remains recorded pending the next run.
Security workflow `41203722606` passes all applicable jobs. The understood
Cloudflare docs check remains excluded by the owner's instruction.

### F09b SQLite production integration mount

`cb3af01`, `f03d968` and `687371f` compose the existing background host with
actual SQLite serving lifetime and complete inventory preparation. Each
instance retains its original credential/root/source inputs and carried elected
writer token. Equivalent background captures update future requests only;
changed authority cancels and joins the old instance. Fenced or unclassified
failures no longer receive automatic provider retries.

Inbound observations stage source-owned desired objects, permanent allocations,
source head/chunks and global inventory in one concrete transaction. Original
UID/RV, ownership, target/group grants and final writer/license checks remain
mandatory. Publication waits for exact primary receipts, including withdrawal
of older source-owned data. Private MAC grants retain source identity and
exclusive expiry. Actual UniFi HTTPS acquisition, omission, ownership rollback,
independent replacement, replica promotion/stale observations and final-license
rollback are covered. Outbound root tests use a bounded provider double through
real ledger/budget/receipt transactions; separate Cloudflare SDK tests use
actual local HTTP servers.

Parent composed races pass: cmd 54.585s, servingbuild 9.262s, host 1.319s, after
the earlier full integration-library races. Agent final mounted races pass
35.274s and host races 7.866s; scoped lint reports zero issues. This implements
the SQLite mount. Kubernetes needs a true atomic source/admission root, and
`Delete` needs a cleanup/finalizer protocol; both remain explicit F09b work.

### Disposable AD protocol validation and Windows prerequisite

`5319c85` supplies a reusable first-party Docker runner, mandatory AD workflow
step, sanitized result artifact and a concrete
[Windows provisioning/acceptance prompt](../../labs/windows-ad-acceptance-prompt.md).
Microsoft's Windows Server container image needs a Windows host, and Microsoft
does not support AD DS inside a container. This Linux VM also has no `/dev/kvm`.
Those constraints do not prevent isolated Samba protocol checks.

On the committed source, `python3 hack/test-ad-samba.py --docker-host
unix:///tmp/kudurru-lab-docker.sock` passes three cases in 7.669s with Samba
4.22.11 and BIND 9.20.30: actual GSS-TSIG authentication plus client-verified
signed REFUSED replies over TCP and UDP, and unsigned UPDATE refusal without
authenticated observation. The result records the exact commit/image/probe and
helper hashes, `lab_or_probe_modified: false`, and successful owned cleanup.
An intentionally incompatible image fails with nonzero status and also cleans
up its container/network. Python compilation and workflow actionlint pass.

Only separate unmodified external executables are used. Realm credentials and
keytabs remain in private container tmpfs; no host ports, host realm, extra
capabilities or Docker runtime logs are used. The probe remains outside the
production binary and refuses every UPDATE. These results do not close Windows
DC/member/DHCP acceptance, production AD writes/ownership/aging/cutover, or the
original two-VM keepalived gate. The container VIP runner identified a real dnsdist cached-health-failure
defect; the completed container acceptance is recorded below.

### F18a global DNS controls and integrated contracts

`50773f9` implements bounded, validated `control.allowedNetworks` and
`control.blockedQTypes` across configuration, schema-driven settings/API/CLI,
initial serving and hot reload. Complete requests retain their original
immutable control component. Alias and DNS64 dependency questions use the same
rules before local/cache/upstream lookup; missing original owners fail closed.
Legacy settings subscribers atomically replace a validated view. Both paths
invalidate the local edge cache; remote edge propagation remains F39b.

Ordinary `GOMAXPROCS=2 make generate docs-gen` updated installed/embedded/chart
CRDs and references. Parent tests use those files directly, without the agent's
temporary generation overlay. Composed races pass for cmd (60.023s),
servingbuild (10.400s), config (1.265s), generated schema (1.072s), control
(1.034s) and integration host (1.328s). The actual `runServe` fixture exercises
one-shot settings mutation, UDP refusal, DNS64, persistence and restart.
Actual Kubernetes envtest admission passes in 17.938s, preserving both lists
and rejecting wrong element/list shapes without changing the saved version.
Combined cmd/serving/config/control/rewrite/kube/host lint reports zero issues.

Agent full config/control/rewrite/serving/servingbuild races also pass, with
475 test/subtest results. Three final mounted/reload repetitions pass in
23.424s. Fixture setup observes initial status/NOTIFY convergence before its
single mutation, and waits for the committed settings publication before DNS
assertions. Distinct DNS64 names per boot preserve exact upstream-call counts
despite the legitimate restored response cache. No refused mutation is replayed;
no production credential/license/writer checks or deadlines were relaxed.

### F68c container lab defect: cached edge health failures

`d1502a6` fixes the lab-reproduced dnsdist health-cache defect (D247): YAML
and Lua now skip packet-cache lookup/insertion only for the exact configured
health QName, after transfer refusal/PROXY authentication and before pool
selection. Six regression cases fail on the previous renderer and pass with
the fix. Edge-renderer and health races pass (27.361s/1.347s); scoped lint
reports zero issues. The complete container run now passes as recorded below. Containers do not
close the original independent-VM gate.


### F62 actual-router CLI and wire contracts

`6938fbb` drives the CLI through the production API router, validator, settings
manager, memory object store and actual query tail. Mixed apply results retain
versions, machine-readable error keys and owner hints; HTTP 200 with a failed
document fails the CLI. Refused writes leave no mutation and are not replayed.
The fixtures also cross the real 500-object pagination boundary, unwrap settings
and consume a filtered named query SSE stream with subscription cleanup. Only
authentication uses the existing apitest identity adapter; this is not a claim
of mounted SQLite/current-credential qualification.

`c439532` reconciles design §10 and operator API prose with ApplyResult, wrapped
settings and QueryRow events, including cluster resume cursors and asynchronous
complete publication. CLI races pass in 2.594s; actual API apply/settings/stream
serialization races pass in 3.182s. Scoped CLI lint reports zero issues.

### F68c completed container alternative

`6eac332` adds the disposable runner and operator instructions. The final run
against application source `b8fd96f14833a6a63f5c34682da87adc480568ac`, dnsdist
2.1.3 and keepalived 2.4.3 passes **12/12 checks in 79.625s**. Real YAML/Lua
preflights also pass. Two auth_hmac instances retain the VIP during lameduck,
remove the backend, move after edge failure (4.2382s), graceful stop (1.2800s),
drain (0.7571s) and whole-node loss (3.5849s), recover MASTER/BACKUP with
nopreempt, and end in the requested split. Four UDP/TCP streams record 2,612
samples, 57 counted transition failures and zero wrong answers; maximum sampled
success gap is 3.2614s. All owned containers/networks/volumes were removed.
Thirteen runner unit checks pass.

The exact sanitized reports are retained in this workspace. This evidence uses
the stated application source, not every later PR commit. It does not certify
independent VMs, Windows, IPv6, HMAC attack handling, stale-snapshot behavior or
replicated SQLite HA. The original VM gate remains explicit.

### F70 central dictionary pruning

`eef61bd`, `b1a29e8` and `a7304d8` implement schema/compatibility 4, reference
constraints and indexes, cached-ID revalidation/locking, and bounded dictionary
sweeps after fact/rollup retention. Every retained fact and all 14 rollup tables
protect references. Each dictionary scans at most 256 candidates per pass with
a durable fixed upper ID and cursor; new ingestion cannot prevent wrap. The
whole scheduler transaction, including COMMIT, has a two-second budget.
Failures roll back rows and progress and cannot report healthy removals.

On disposable PostgreSQL 17.11, the final full central race suites pass in
2.744s/8.671s/25.725s. Three repetitions of concurrent shipping, bounded sweep/
restart and actual delayed-COMMIT cancellation pass in 14.726s. Final explicit
recent-orphan/ancient-reference acceptance passes in 1.288s; scoped lint reports
zero issues. Actual v3 upgrades preserve IDs, invalid references roll back,
stale caches recover, held reader snapshots retain joins, and isolated schemas
remain independent. The owned database resources were removed.

Large upgrades can exceed existing startup migration limits and require a
maintenance window; no deployment-scale migration performance is claimed.
PostgreSQL 16/18 and final integrated-head checks remain separate. D248 records
the design.


### F02a/F02b/F38 audit storage and historical compatibility

`aa3784c` adds SQLite schema 6 attribution columns, strict v1/v2 chain selection
and a durable legacy boundary. The v2 hash includes all new request/client/authz
fields. Migration and verification reject inconsistent formats instead of
trying another hash. Legacy bytes and hashes survive upgrade, restart, mixed
chains and pruning; the reason suffix remains for compatibility.

One leaf redactor prepares new SQLite, spool and central-chain documents before
capping and hashing. It uses the full private-kind manifest (the existing safe
APIToken allowlist is the exception), removes structural credential carriers,
and omits malformed, ambiguous or excessively nested input. Safe public bytes
and exact numbers are preserved. Existing hashed source evidence is not rewritten.
Structural redaction is not free-text DLP, and historical rows/backups retain
their original protection/retention requirements. Unkeyed integrity chains do
not prevent complete rewriting by a database administrator.

Final agent auditdocument/audit/auditlog/spool/auditstore races pass in
1.020s/4.316s/5.234s/22.033s/2.094s. Final targeted SQLite/API audit races pass in
16.856s/4.454s; three actual mounted attribution repetitions pass in 43.704s.
Scoped lint reports zero issues. One existing AuditLog metadata test lacked
envtest in that agent run; it is not claimed as executed.

Parent composition with F70 runs the entire central suite against disposable
PostgreSQL 17.11 with required-backend mode: **67 tests pass**, with package
elapsed times 2.097s/6.863s/22.190s. This includes source-hash preservation and
new central redaction after the final source correction. The fixture was
stopped and removed. Actual mounted F02/F51/F62 CLI/router races pass in 12.502s.

### F51 optional HTTPS-only external downloads

`a15480e` adds host-only, restart-required `blocking.httpsOnly`, false by default.
A private client copy rejects HTTP initial requests and every redirect before
contact, while retaining custom trusted CAs, proxy/local-address guards,
timeouts and redirect callbacks. Permanent policy refusal is credential-free
and does not trigger an immediate list retry. Both catalog formats and replica
external fallback use the policy. Existing cached data and authenticated peer
copies are retained; this is not retrospective transport certification.

Complete list-fetcher/catalog races pass in 3.702s/4.394s. Focused config/API
refusal races pass in 1.182s/2.378s. Actual startup proves both catalog constructors
carry the policy and a BlockList publishes refusal without contacting HTTP or
losing its exact last-good digest (3.058s). The initial fixture omitted that
required digest and correctly could not publish a complete serving view; the
final fixture seeds explicit historical cached data. No production readiness
or publication rule was relaxed. Scoped lint and final mounted-file lint report
zero issues. D251 and the operator download policy document the limits.

### F66 independent storage and composed checks

`b3f6986`/`5048c61` add independent host directory keys and opt-in Helm/Compose
volumes while preserving empty/default paths and the original data claim.
Preflight checks named path collisions, modes, symlinks and selected database
hardlinks before initialization. Split mounts are additive; the operator moves
files only in a stopped backup/copy window, including controlled recreation of
immutable StatefulSet claim templates. Fresh named-volume targets are writable
by UID65532 in the updated image sources.

Agent actual default/split restart races pass in 16.661s, proving primary state,
query rows and cache persistence across four serving lifetimes. Corrected chart/
Compose races pass in 4.204s: 14 positive layouts, six refusals and 12 comparisons
requiring unchanged original data claims. Actual Compose config checks pass
15 combinations. The expanded volume probe fails on the exact old base image
and passes for all new data/querylog/local targets, with owned cleanup. Scoped
lint reports zero issues. No actual CSI provisioning, distinct physical disks,
power-loss, NFS or cross-volume transaction/backup atomicity is claimed.

Ordinary generate/docs-gen/deploy-assets succeed; host-only fields do not enter
the cluster settings CRD. Parent composed config/API/actual-serving/deployment
races pass in **1.472s/1.678s/20.328s/3.684s**, covering F02/F51/F66 together.
The CGO-disabled release build with the built dashboard is **99,557,536 bytes**
against the unchanged **100,000,000-byte** owner budget. Final image/artifact
and hosted-head qualification remain distinct.

### Hosted security recovery

On pushed `7dda727`, [security 41206423248](https://github.com/robertherbaugh/kudurru/actions/runs/41206423248)
passes every applicable job, including SAST, Trivy filesystem/images and
vulnerability checks. Hosted binary build passes after `2d6e781` removes the
Cloudflare SDK root helper imports and their unused service registrations;
provider races pass in 9.101s. `77953ef` documents the necessary root runtime
only for the isolated, unshipped Samba lab. The committed runner passes 3/3
protocol checks in 8.244s with owned cleanup. Local pinned Semgrep reports zero
findings and Trivy configuration scanning is clean.

The hosted authenticator test still fails and is reproduced on Node 24.22.0;
the native request fails before reaching the MSW handler. It remains open
while the agent isolates the cause. Later source commits require their own
hosted results. The understood Cloudflare docs check stays excluded.

### Reviewed zone options and service-account binding contract

`c9f14dc` closes F60d's dedicated CLI implementation. Get emits the complete
reviewed spec and resourceVersion; set accepts one bounded strict YAML/JSON
document, preserves current metadata/status and issues one existing object PUT.
Actual-router and current SQLite tests pass in 6.350s, including final original
credential expiry after the route staged an update or selected a no-op. Stale,
invalid, foreign-managed and unauthorized writes are refused without replay.
CLI reference generation succeeds; scoped lint reports zero issues.

`feefdbc` resolves F50 through D254's shared namespace/name contract, the
existing behavior allowed by the original requirement. Both authenticated
service-account providers match the same binding; distinct namespace/name,
zone scope, narrowing/malformed scopes and an absent account identity are
covered through the actual auth-to-RBAC engine. Package races pass in
1.052s/1.023s and scoped lint is clean. Operator and architecture documentation
explain account creation/recreation, separate credential/disable lifecycles and
independent Kubernetes SAR grants. No provider-isolated subject is claimed.

On pushed `f7ccf4a`, hosted security run
[41207334455](https://github.com/robertherbaugh/kudurru/actions/runs/41207334455)
passes all applicable jobs. Later commits require their own hosted results.

### F42 per-VRID secrets, F52 query explanations and dashboard transport

F42 is implemented by `49a9cf8`, `376d63d`, `e760dd7` (D253). Explicit Secret
references must cover every configured VRID exactly once, with no shared fallback
or Helm content reads. A bounded init stages private root0600 files because
real keepalived 2.4.3 refuses group-readable HMAC files. The normal container
retains SETUID/SETGID for its existing script user; the init drops all capabilities.
Real baseline preflight fails without these script capabilities and passes with
them. Seven disposable cases cover per-VRID/shared/rotated references, missing/
empty/oversized refusal and staging all 255 files (0.531s, retained 30s bound).
Parent composed deployment/CLI/keepalived races pass in 4.774s/1.196s/16.969s.
Ordinary deploy generation and local pinned Trivy misconfiguration scan pass.
Agent lint reports zero issues. Actual kubelet/CSI projection, enforced cgroup
limits and seamless mixed-key rotation are not claimed; coordinated restart
remains required. Owned lab resources were removed.

F52 is implemented in `8879459` (D255). Actual removed rules remain bounded
diagnostic witnesses in the immutable compiled lists. HTTP dig uses the original
question/client/policy selection, requires list-read permission for evidence,
and keeps it outside response caches. The UI uses the same answer without a
second list read or browser regex evaluation; another rule can still block.
Compile/query bounds and partial evidence are explicit. Stale/unselected rules
are omitted; response-address and alias-target exclusions are outside scope.
Full blocking/parser/matcher/stage races pass (1.672s/1.144s/3.280s/1.325s);
API/capture/schema races pass in 4.876s. Actual runServe SQLite/API acceptance
passes in 3.350s, including a disabled rule and a still-blocking second source.
All 28 domain UI tests pass in 39.67s, plus typecheck and scoped frontend/Go
lint. OpenAPI and both clients are regenerated. Fixed-class performance and
remote edge invalidation retain their separate gates.

Dashboard fixture fix `69e23c8` owns a test-only Undici 8.12.3 dispatcher and
closes it before MSW teardown. A controlled cancellation/native-fetch regression
exposes eight unintended real TCP connections with the old fixture and zero
with the fix. The original hosted component failure's native cause was not
captured, so this is a concrete transport defect fix, not a claim that a passing
rerun alone identifies every intermittent failure. The unchanged authenticator
and session tests plus regression pass 1,100 executions; full exact hosted
Node24.22.0 acceptance passes 881/881 (148.55s). Parent `e90dd7a` binds the probe
explicitly to loopback; 26 composed cases pass in 5.78s. No authentication
assertion, timeout, retry, production fetch path or MSW unhandled-request rule
was weakened. Fresh hosted confirmation remains required.

Prior-head f7ccf4a CI exposed TestBINDSecondary/NOTIFY failure in job124555248768.
That actual interoperability regression is assigned for investigation and is
not closed by successful local chart or protocol tests.


### F10a inline declaration reload and F73 independent audit trust

F10a is implemented in `71c0393` (D257). Re-read the bounded original node
file, validate the entire inline section before reconciliation and preserve
last-good ownership on unreadable/invalid/oversized input. Environment-locked
objects remain authoritative. Valid removal intentionally deletes owned
objects. Parent-directory watching and polling handle atomic replacements and
missing directories. Per-object writes retain their original fences; this is
not a cross-object transaction. Full object races pass in 3.408s; composed
objects/configuration/actual runServe reload races pass in 3.432s/1.553s/30.056s.
Eleven AttachDialog cases and scoped frontend/Go lint pass.

F73 is implemented in `d3604c1` (D252). Operator-provisioned Ed25519 trust in
`audit.keySecret` is independent of session keys, retains historical public
keys and supports explicit revocation. Verification captures and rechecks the
Secret revision. Legacy HMAC import requires original retained purpose keys
and a fixed historical creation cutoff. Existing installations must follow the
upgrade runbook: absent trust suspends cluster shipping/verification while DNS
and local audit spooling continue. No replacement trust is silently generated.
Full auditkeys/auditlog races with real Kubernetes envtest pass in 4.919s/15.684s;
composed API/configuration/actual mounted startup and reload races pass in
2.142s/1.199s/53.535s. CRDs and configuration/CLI reference are regenerated.


### F58b bounded query-log download and control capture fixture

F58b (D258) adds an authorized `/queries/export` attachment and dashboard link
using the current scope/filters. The existing history reader preserves retained
privacy, node/source/partial metadata and continuation; no separate data path
or peer fallback is introduced. Default/maximum 1000 rows, 16 MiB output and a
15-second operation budget are explicit. Errors/cancellation yield no attachment.
It is a page export, not a cross-page snapshot or unbounded download.

Composed export and control-capture race checks pass in 3.522s/1.968s. Actual
runServe acceptance passes in 21.137s, including login, native DNS recording and
filtered download. All 20 QueryLogPage cases pass in 29.57s, plus frontend
lint/typecheck and scoped API/command/servingdns Go lint (zero issues). OpenAPI
and both clients are regenerated. These checks do not substitute for hosted CI.

The hosted control-capture failure was reproduced at its original 3s startup
bound: its complete fixture omitted the newly required control component.
Explicitly including the empty control view fixes initialization without changing
production admission, expectations or deadlines.


### F26b cache growth, F25a DoT and F69 retired-node history

F26b `62990dd` (D259) adjusts the cache topology on Resize/Reconfigure while
preserving original scope, generation and absolute entry/security deadlines.
Exact per-shard capacities sum to the configured bound; migration bounds the
new maps during shrink. Agent cache/stage/persistence races and actual mounted
64-to-1,000,000 reload/current-command regressions pass. Local grown/fresh lookup
medians differ by 6.0% at 200k and 0.9% at 1m, all zero allocations; moving
6,000 resident entries takes a median 1.636 ms. Migration is synchronous,
O(resident entries), and temporarily retains old/new indexes. These shared-VM
measurements do not establish a fleet latency bound.

F25a `eeb68c2` enables configured Linux DoT sockets using the existing TLS,
PROXYv2, deadline and shutdown wrappers. Full DNS/config races and scoped lint
pass. Four real sockets pass verified TLS/ALPN queries before/after rotation,
existing-connection survival, per-connection query/idle limits, invalid TLS
refusal and shutdown/rebind checks. Local 1-vs-2 socket median throughput rises
17.6% for persistent connections and 33.2% for connection churn, with no failed
replies in 20 samples. This is local evidence, not a universal speedup or new
aggregate TCP admission limit. F25b QUIC remains distinct and open.

F69 `a4e38ce` (D256) adds immutable exact-node central readers. Every relevant
fact/rollup/count/page/dictionary query is scoped before aggregation or limits;
metadata shares the data transaction. Retired/replaced/renamed nodes do not
require membership. Unknown history is 404 and central failure is redacted 503
without fallback; self stays local and live tails retain their peer transport.
Agent real PostgreSQL races cover all readers and source epochs, raw expiration,
dictionaries, concurrency and actual router/current-command behavior. Parent
retired-node PostgreSQL races pass in 5.596s; 19 composed API/export/schema cases
pass in 4.750s. OpenAPI and clients are regenerated. F71 remains open: legacy
node-only cells can lose source provenance, so automatic attribution or late
replacement would invent history. Independent safe implementation continues.

### Hosted regression fixes and current artifact evidence

Hosted security run 41208468848 passes every applicable job on `7ed9e3b`.
That head's CI run 41208468704 exposed missing control fixture composition and
production template reachability. `72586d7` repairs the fixture; `5594ba8`
removes the UniFi SDK root and its gofakeit initialization while retaining the
bounded official wire reads and original host-loan checks. Full UniFi races
pass in 6.847s, actual HTTPS acquisition in 10.285s, unchanged template guard
in 10.396s, and scoped lint reports zero issues.

BIND NOTIFY baseline `45864f8`, controlled source-gap regression `b0eaf3d` and
readiness fixture `ce033d6` preserve all production refusals and original time
bounds. Two old signed-AXFR diagnostic commands each failed once in 20 runs;
actual status/source traces prove that refusal was correct. Fixed full BIND
acceptance passes all four subcases 20 times, and six controlled status tests
pass ten times. Parent real BIND acceptance passes in 25.650s after integration.

The built-dashboard CGO-free stripped Linux/amd64 artifact at `eeb68c2` is
77,074,592 bytes against the unchanged 100,000,000-byte budget. The dashboard
entry transfer remains 480.5 KiB against 600 KiB; typecheck and generation pass.
Later commits and the final pushed head still require their own hosted checks.


Cache persistence follow-up `3e5eaea` acquires a separate topology lock before
publication exclusion; a stalled dump can delay resizing while ordinary reads,
fills and flush commands continue. The parent regression explicitly observes
the queued resize before challenging those operations. Composed cache/persist/
stage races pass in 3.650s/1.534s/1.549s. Combined DoT/control-capture/signed-AXFR/
actual mounted cache growth races pass in 1.851s/1.525s/2.883s/11.034s. Integrated
API/cache/servingbuild/dnsserver/interop scoped lint reports zero issues.


### F21a upstream routing-file import

D263 implements host-only `upstreams.importFile` through the ordinary loader,
shared bounded parser and canonical groups/suffix routes. Repeated suffix rules
retain all ordered servers; `#` exceptions remain normal-default routes.
Unsupported directives, local answers, collisions and malformed input refuse
the whole source with line-only reports. Input and expanded representations have
explicit byte/count bounds. Conditional scheduling is strict ordered failover,
not an assertion of dnsmasq's adaptive scheduling behavior.

Environment and UI/canonical ownership retain precedence. Host paths cannot
enter durable public sections; both memory/ConfigMap and legacy file backends
now exclude host-only leaves during serialization/export. Imported effective
routing participates in reviewed revisions; adoption survives missing source,
and release refuses absent or mismatched input. No query-path read, implicit
singleton reseed or new watcher is introduced.

Full config/upstream/settings race suites pass in 9.587s/4.610s/4.897s. Actual
mounted UDP routing proves imported suffixes, default exceptions and ordinary
default forwarding; composed mounted/current-command and ownership/restart
races pass in 16.697s/1.769s. Final expanded-input bounds/refusal races pass in
1.364s/1.020s. Scoped config/upstream/settings/audit/command lint, followed by
frozen final parser lint, reports zero issues. Generated reference/API outputs
are refreshed. An initial mounted test used an authoritative-only helper for a
forwarding answer; corrected tests assert the proper non-AA/RA wire behavior.

Hosted security run 41210334267 passes every applicable job on `a45f749`.
CI's only completed failure so far is exported-method documentation; `29dbec0`
adds the missing VerifierFunc.Verify comment and scoped audit lint passes.
The final built-dashboard artifact at `a45f749` is also 77,074,592 bytes.


### F08 doctor and F24b group UDP exemption

F08 `c46f8c9` adds an opt-in authenticated self-NOTIFY diagnostic with one
three-second UDP exchange, selected file-backed TSIG and configured source.
The complete SQLite listener signs its observed address only under original
publication/proof and current key/writer/readiness checks. No writes, callbacks
or zone refreshes occur. Loopback is explicitly local-route evidence; the
Kubernetes serving path is not qualified. D261 and the runbook describe these
limits. Agent doctor races pass twice; parent composed doctor/exemption races
pass in 30.921s. Actual non-loopback Docker qualification is tracked with Knot.

F24b `3234ad4` compiles optional ClientGroup exemptions into original membership.
Only the winning identification tier contributes; ties use any enabled grant.
Eligible UDP resolves membership once before prefix accounting and reuses it
later, with policy/schedule work deferred. Exempt traffic consumes no prefix
budget. Actual mounted UDP/TCP/API/held-publication races pass; parent composed
validator/policy/ingress/identify races pass in 1.357s/1.076s/1.178s/1.073s.
The dashboard explicit-save control preserves membership and ownership; all
13 group-page tests, TypeScript and scoped ESLint pass.

Real Kubernetes admission exposed SSA removing an empty spec when the only
true exemption became omitted false. Typed serialization now explicitly writes
false, retaining the required spec object and revoking the grant. The actual
apiserver persists true and false and rejects nonboolean updates without
changing the object; final Kubernetes/embedded-schema races pass in
14.176s/1.560s. The invalid-value fixture uses a real update to exercise CRD
admission (SSA's earlier type-check reports a distinct non-admission error).
Kustomize, Helm, embedded schema and public references are regenerated.


### F68b actual Knot catalog consumer

`6aa58ed` adds a pinned Knot DNS 3.6.2 container qualification and
`hack/test-knot-catalog.sh`, using the explicitly selected private Docker daemon,
an isolated internal network, no published host ports and a fresh checkout
binary. It qualifies ordinary primary-zone RFC 9432 records, not future native
catalog configuration types. All eight subtests pass: catalog NOTAUTH, signed
AXFR, unsigned REFUSED, wrong-secret BADSIG, unknown-key BADKEY, policy refusal,
exact-source ACL signed refusal and mounted doctor's non-loopback source.
Automatic catalog membership replacement converges before the 3600-second
refresh interval. Agent full acceptance passes in 11.788s and two repeated
races in 24.822s; parent integrated acceptance passes in 38.670s. Scoped lint
and shell syntax checks pass. Raw logs and generated credentials remain private.

### F26c blocking operational hot reload

`38153fa` updates the existing fetcher/resolver loops and readiness consumers
only after settings acceptance. Original source, credentials, deadline,
conditional request, cooldown and cancellation/join remain unchanged. Refresh
shortening/lengthening uses last-success anchors; explicit per-list settings
remain authoritative. Removing the BlockList CRD's injected 24h default enables
new omitted fields to inherit the hot cluster value; historical stored values
are never guessed or rewritten. The default cluster interval remains 24h.
Agent full listfetcher/blocking/config/validator races pass in
4.498s/1.374s/7.551s/8.471s; mounted actual HTTP download, authenticated settings,
complete publication and UDP/TCP DNS races pass in 13.745s. Scoped lint reports
zero issues. Parent regenerates Kustomize/Helm/embedded schemas and references.

Parent composed mounted/published-settings/scheduler/readiness/config/schema
races pass in 8.957s/1.093s/1.093s/1.272s/1.310s.


### F55 verified source-digest page indexes

D267 adds compact bounded parser-result indexes keyed by body digest, format
and subdomain mode. Every complete-serving page still checks the original
object/cursor and opens/verifies the exact body; an index cannot revive missing
or corrupted bytes. Legacy file sources also verify length/digest before reuse;
non-file adapters preserve streaming. Parser line numbers, duplicate rules and
multiple hosts per line survive. Exclusions/filters remain request-local.

The cache accounts at most 128 MiB and 16 indexes, each builder at most 96 MiB,
with the existing four admitted readers bounding concurrently pinned/building
indexes. Oversized normalized expansions continue the same streaming parse;
failed/canceled parsing publishes no index. Unfiltered pages with no exclusions
select only their own rows after verification; filtered totals still require a
normalized-rule scan. Derived indexes are not persisted across process restart.

Final API and real SQLite/publisher/body-reader races pass in 7.224s/8.634s,
including actual-file corruption and loss after populating a warm index,
generation-bound cursors, exclusion changes, parser-buffer ownership, bounded
fallback, eviction and concurrent readers. Scoped API/servingbuild lint reports
zero issues. The two-million-rule local benchmark fits the unchanged 96 MiB
index budget: cold late-page parse 690.5 ms / 48.1 MB / 2,000,118 allocations;
warm page with actual file rehash 29.1 ms / 44.0 KB / 113 allocations; indexed
row selection alone 30.6 microseconds / 10.9 KB / 108 allocations. These are
shared-sandbox measurements, not fixed-class fleet qualification.

Hosted kind run 41210334567 failed on absent cluster audit history after F73.
Its fixture omitted the now-required independent trust Secret. `bededd6`
provisions a fresh cluster/namespace-bound key with the built image before
replicas start. It changes neither audit refusal rules nor the 90-second test
bound. Shell syntax passes; fresh hosted kind qualification remains pending.

### F54a concrete-name regex impact

D268 adds read-only impact routes and the shared Domains/Allowlist/Denylist
**Test a domain** dialog. Original captured entry and compiled lists remain
paired while a later publication changes opposing rules. Production case
semantics, explicit disabled/group scope, 256-list/64-match/128-KiB bounds and
partial reporting avoid asserting exhaustive regex overlap or final client
policy behavior. No DNS request, mutation or browser-side regex evaluator.

API and actual SQLite/publisher/authenticated HTTP races pass in 1.686s/1.998s,
including held-old versus fresh publication, permissions, invalid names,
original-identity failure and bounded output. All 32 domain UI cases pass,
including read-only inspection, disabled/group/partial evidence, nonmatch/error
clearing and late-response isolation across dialog reopen. Typecheck and scoped
UI lint pass; scoped API/servingbuild/blocking lint reports zero issues.
OpenAPI, Go and TypeScript clients include both routes. F54b cache/remote-edge
publication acceptance remains a separate open requirement.

### F07b encrypted DNS TSIG

D265 / `a6bf8e1` verifies original wire on real DoH HTTP/1.1/HTTP/2 GET/POST,
DoQ and DoH3, signs replies with the installed original provider/MAC and
checks the original signature deadline through final response generation.
Invalid/malformed/expired credentials never enter the pipeline. Held expiry
returns signed NOTAUTH/BADTIME, signed HTTP is private/no-store, and encrypted
UPDATE/NOTIFY remains signed REFUSED without native proof or durable mutation.
The agent's full affected-package races pass in 37.923s/49.345s with zero lint
issues. Parent composed actual encrypted/native-proof races pass in
7.016s/1.920s. Tests independently verify response HMACs, key replacement,
framing and original expiry. This qualifies immutable installed keyrings and
replacement listeners; live store-backed key rotation remains F07a work.

### F71 source-owned stopped-source recovery

D271 / `75e3b81`, `123148b`, `f8fbb85` adds schema-5 source ownership across
all 14 central rollup families, source-locked replacement, fact revisions and
bounded stopped-source closed-hour/day work. Late facts and official returns
preserve exact ownership/counts; raw/hourly retention gates prevent incomplete
tails replacing a complete baseline. Scoped evidence/verification routes bind
source/node/hour/head/revision, retained-input digest, expiry and current
permission/license context. No raw DNS values or peer/local fallback.

The agent's final real PostgreSQL regression has 8 central, 109 pgquerylog and
58 SQLite test/subtest passes. Frozen focused races have 66 pgquerylog and 26
mounted API passes; 11 production startup/current-command cases pass; scoped
central/SQLite/API lint reports zero issues. Parent generated API/Go/TypeScript
contracts include recovery status and routes; composed checks follow below.

Schema-4 opaque node-only cells remain unchanged and visibly gated as
`legacy_provenance_required`; independently reconstructable historical ownership
is required for adoption. Expired raw input without a retained hourly baseline
is `source_input_incomplete`. Neither is silently guessed or discarded, and no
universal legacy-repair claim is made. These are documented in the architecture
and PostgreSQL operator runbook.

Parent composed real PostgreSQL source migration/recovery/late-retention/receipt
races pass in 37.141s; mounted API recovery plus regex impact races pass in
2.439s. The disposable fixture and its anonymous volume were removed after
verification. OpenAPI drift/served-schema checks pass in 0.421s. The integrated
dashboard builds at 480.6 KiB transferred entry payload (600 KiB budget).
Four actual browser cases pass in 15.2s: concrete regex evidence/nonmatch and
all zone-page settings/delete/root-protection tests. Typecheck/scoped E2E lint
pass; the captured screenshot is local `.context/regex-impact.png`.

Hosted `bededd6` security 41212199976 and kind/MetalLB 41212199942 pass.
CI 41212199948 exposed one dashboard fixture race: a record POST immediately
after zone creation received `503 current writable serving generation
unavailable`. `4a57e4f` waits for captured Ready status at the desired generation
before the dependent record command, preserving single-attempt mutations and
all command fences. The browser cases above verify this fix; later hosted
results still apply to their own heads. Latest fetched `origin/main` is already
contained in this branch.

### F41b chart/provider annotation parity

D272 reconciles the ServiceProvider address/pool/sharing options with a named
Helm DNS annotation helper. Selected provider annotations override matching
explicit values and unrelated annotations survive. kube-vip no longer receives
an implicit MetalLB sharing key. Historical split-Service sharing under
`provider: none` is preserved for external MetalLB allocation. Service-based
providers now refuse ClusterIP/NodePort DNS topologies that cannot allocate the
requested LoadBalancer address.

Actual Helm render/Go contract parity passes for 16 mixed/split combinations
across direct, sidecar, Deployment and DaemonSet edge modes, with IPv6 family
selection and conflicting explicit address values. Four unsupported topology
cases refuse rendering; existing split/kube-vip/MetalLB cases and the original
provider API pass (deploy 1.377s, provider 0.008s). Scoped lint reports zero issues.
This closes annotation parity only. F41a announcer-owner observation and actual
provider failover qualification remain separate; an annotation is not evidence
of the announcing node.

### F26d captured query-log sinks and disabled restart

`08ee759` and `9a62eb4` implement D266. Complete DNS publications include the
logging owner, retaining enabled/sink/privacy through emission. One bounded
queue/tail and at most one SQLite writer plus one slog sink survive reloads.
Sink preparation precedes publication; failure retains the previous bundle.
Accepted batches drain once to their original destination without replay.
Local history, purge, retention and configured shipping survive disabled/slog
modes, including restart with an existing database. Missing files stay lazy.
Shutdown joins the consumer before closing its writer; a timed-out external
handler cannot cause a replacement writer or close under pending I/O.

Agent mounted races pass in 48.457s, collector/SQLite in 1.154s/67.021s and full
servingbuild in 161.917s. Actual two-start UDP/TCP/history/purge regressions pass
twice in 85.887s. Parent mounted races pass in 38.366s, composed servingbuild/API
in 6.240s/9.873s, complete servingdns in 2.147s and combined CLI/restart races
in 33.815s. Scoped lint reports zero issues. Settings UI passes 22 cases;
generated schemas, help and references are updated. The agent full API run
hit its 180-second limit in source-scan coverage; no full API pass is claimed.

### F37 generated CLI transport

`a106708` plus the parent-generated artifact and Makefile hook implement D270.
The reproducible generator covers all 156 merged OpenAPI operations with direct
calls to their matching generated methods. The CLI retains raw body and
query/header ownership, cookie/CSRF, pagination, SSE, structured errors and
original cancellation. Final method/origin/escaped-segment checks prevent path
rewrites. Unknown routes retain one raw fallback; matched failures never retry
or fall back. Custom HTTP clients and low-level editors remain trusted extensions.

Complete agent generator/client races pass in 2.983s/1.633s and selected actual
router/SQLite CLI races cover 72 tests/subtests in 21.925s; scoped lint reports
zero issues. Parent regenerated for the merged routes; generator/client races
pass in 2.784s/1.580s and combined actual-router CLI/restart races in 33.815s.
These are scoped acceptance results, not a full repository qualification.

Hosted security 41213309941 and kind/MetalLB 41213309889 pass on `8ea7367`.
CI 41213309865 has no failed jobs at this checkpoint, with Go shard 1/2 still
running. GitHub docs 41213309876 exposed stale endpoint references for F54/F71;
this batch regenerates those references. The separate Cloudflare build remains
excluded. Subsequent heads require their own hosted checks.

### F16 exact current-hour domain statistics

`8b98f41` implements D269 with a transaction-maintained retained-fact
hour/node/domain index, plus original source centrally. Whole cells aggregate
directly; ragged interval edges use indexed raw rows. Counts remain exact after
privacy, rollback, purge and retention. SQLite schema 3 pins its WAL snapshot
with pending totals; central schema/compatibility 6 seeds under write locks and
uses an empty FK guard to refuse old connected partition DROP/TRUNCATE paths.
Older readers/janitors must upgrade. Index size scales with retained distinct
hour/node/source/domain entries; pathological boundary intervals still read
matching facts. Source-recovery provenance gates remain unchanged.

Agent final regression passes 61 SQLite, 10 central and 111 pgquerylog cases;
scoped races cover snapshot, upgrade/destructive-operation, privacy, retention
and source recovery. Final scoped lint reports zero issues. Parent composed real
PostgreSQL migrations/pgquerylog, mounted recovery/stats/export API and SQLite
races pass in 1.735s/41.926s/4.217s/173.457s. Its owned fixture and volume were
removed after the command passed.

The exact 1M-row/252k-name SQLite workload passes: raw unique aggregation
1.063s versus index 433.950ms; raw top 1.011s versus index 346.301ms. Full Summary
521.945ms, TopDomains 429.856ms, ingest 63.246s and index 9,809,920 bytes. Central
1M/252k acceptance also passes: Summary 217.787ms, TopDomains 379.051ms, ingest
312.606s and index/indexes 74,096,640 bytes. The central timing predates final
upgrade/snapshot refinements; final regression validates those refinements.
These are shared-sandbox measurements, not fixed-class qualification.

### F43c console capability under YAML

D275 and `hack/labs/dnsdist-console/run.py` qualify the external dnsdist 2.1.3
gate. Ten actual Docker checks pass against the immutable local image
`sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e`:
YAML startup/DNS, failed-command refusal, new UDP/DoT routing, backend add/down/
remove, certificate replacement with active-session continuity, invalid-cert
retention, unchanged PID and persisted-YAML restart convergence. The runner
uses the external client and private configuration, never console keys in argv.
A terminal return acknowledgment is needed; Lua print reaches the daemon log.

An established DoT connection selected to the removed/down backend returned
SERVFAIL, while new connections reached the replacement. This is recorded,
not hidden by reconnect/replay. Certificate-only reload preserved the separately
established connection. Minimal fixture members are forced up; production
health validators, PROXYv2, other encrypted protocols, bounded supervisor
failure convergence and readiness remain F43a/b implementation acceptance.
The capability gate is satisfied without claiming that implementation complete.

Hosted CI 41213309865 on 8ea7367 was superseded/canceled with Go shard 1/2
unfinished; its aggregate ci-ok failed because of that cancellation. The newer
8bdb218 CI remains running. No full old-head CI success is claimed.

### F03 exact Kubernetes watch resume

D277 replaces the previously ignored Kubernetes ResourceVersion with a direct
server watch for explicit nonzero cursors. Actual retained modifications and
deletions arrive in order without replaying current objects as Added. The
original guarded client and namespace are retained; private kinds watch only
labeled backing Secrets and filter decoded namespaces. Initial empty-cursor
informer behavior stays intact. The 256-event channel applies backpressure;
Stop, cancellation and driver Close terminate remote requests and blocked sends.

Compaction or malformed events close the stream with a terminal Err accessor;
opening errors return directly. There is no automatic relist or event/mutation
retry. Cursor 0 is refused for exact resume. SQLite's coalesced feed/compaction
relist remains its separately documented contract, not an exact-history claim.

Final selected real envtest/private-kind/initial-watch and resume races pass in
19.006s. They include replay of an already-deleted object's modification and
deletion, exact resource versions, namespace/private-kind filtering, driver Close,
cancellation under full result-buffer pressure, interrupted watch opening,
malformed events and explicit expired-stream classification. Scoped kube lint
reports zero issues. The earlier compile attempt used the wrong test-only
subject type; it was corrected before the successful final run.

### F40a typed host supervisor settings

`52ec29d` implements D274: 46 edge and 24 VIP keys hydrate the existing explicit
render/run commands. Defaults precede file, environment and explicitly changed
flags. False/zero/empty values, aliases and whole-collection replacement are
preserved; only the exact explicitly overridden key skips lower environment
parsing or file reads. VIP instances and preempt delays have typed shapes and
narrow JSON environment support. Configured zero priority retains provenance
through node discovery. VRIDs remain operator-owned, secrets stay references,
and no supervisor starts automatically from serve.

Every key is node-scoped/host-only/restart-required. Actual authenticated API
coverage refuses value changes and null resets on all 70 leaves through both
settings/node-config routes (280 cases), plus whole-section resets. Existing
source/PROXYv2 credential, TLS, frontend, renderer and legacy/HMAC refusals remain.
Agent full config races pass in 9.586s, selected CLI races in 3.646s (38 cases),
actual chart/Compose checks in 1.294s (32 cases), and scoped lint with zero issues.
Parent selected config/CLI races pass in 1.928s/3.552s. Generated reference drift
passes in 0.028s; the settings UI passes all 22 cases with the new mock schema.
An initial parent combined test invocation incorrectly passed the CLI-only
update flag to other packages; those packages refused the flag before testing.
Correct independent commands above pass; no timeout or assertion was weakened.

Hosted security 41214453550 and GitHub docs 41214453575 pass on 8bdb218.
CI 41214453524 has only Go shard 1/2 still running at this checkpoint, with other
jobs passed. Newer local commits are not yet qualified by that hosted run.

### F53 exact ClientGroup allow/deny scope

`926b2a3` implements D273. The baseline reproduction leaked both allow and deny
rules to adult traffic sharing a Policy with kids, across exact/wildcard/regex
entries. Private copied winning membership now filters list references before
immutable Selection construction, including fallback policies. Optional bounded
BlockList `clientGroupRefs` uses namespace-local groups. Reserved UI group names
retain their original scope before migration; convenience/current approval
writes materialize it. No request diagnostic GroupIDs, lower-tier memberships,
newer publication or query-path store read can widen scope.

Agent repeated full policy/domainlist/unblock/validator races pass in
1.618s/4.182s/26.177s/18.466s; repeated mounted/current-command races pass in
60.127s, API in 5.269s and scoped lint reports zero issues. Parent composed
mounted UDP/TCP, raw-cache, held-publication and current-command races pass in
29.461s. A separate actual Kubernetes API-server test passes in 18.112s,
proving deployed CRD rejection of contradictory reserved scope, duplicates,
invalid names and over-limit sets, and exact accepted readback without pruning.
It bypasses the store validator, so an embedded-only schema cannot mask drift.

Deepcopy and all three deployed/embedded BlockList CRDs are regenerated;
reference/schema drift passes in 0.029s/0.010s. Dashboard types include scope,
and saving list settings preserves it (all 20 list-detail cases pass, plus
typecheck and scoped lint). Original credentials, current-command final fences,
request Received/Deadline and compiled content ownership remain unchanged.
F54b remote-edge invalidation and other topology gates are still separate.

### F21b bounded response-cache inspection

D280 implements `GET /cache/inspect` under cluster cache.read. An exact
name/type/DO/CD/client-derived ECS key reads scalar raw-entry metadata through
Peek. It never enumerates names, serializes stored answers/subnets/upstream
endpoints, resolves misses, rebinds restored entries or affects hit/LRU/prefetch
state. Original HTTP leases select the original resolver and existing cache
incarnation; a newer request cannot adopt retired entries even after A→B→A.
This inspects the raw default route, not the final policy/authority decision.

Selected cache/API/real mounted DNS races pass in 1.037s/1.443s/14.162s.
Additional final API and real SQLite held-HTTP/replacement/cancellation races
pass in 1.716s/3.257s. The actual mounted test warms native DNS, reads metadata,
checks DO/CD/miss isolation and anonymous refusal, and proves no upstream I/O.
An initial test invocation exposed test setup errors (missing captured resolver,
incorrect stale-window assumption and expected validation status); corrected
commands pass without production gate or deadline changes. Generated OpenAPI,
Go/TypeScript clients and references are part of the next integrated batch.

### F41a provider ownership evidence

`1c934e7` and review follow-up `9990ede` implement D278. Six host-only observation
keys mount a bounded read-only provider observer in serve; existing edge.read
protects API evidence. kube-vip per-Service ARP Lease plus vipHost/Ready Node,
or MetalLB L2 status plus UID-matching live speaker/Ready Node and exact
Node-owned Lease, can establish provider-reported ownership. Failed/stale,
ambiguous, replaced and unauthorized inputs clear the owner. MetalLB status
retains its original creation/version evidence; Node renewal is distinct and
cannot turn an old status read into a provider heartbeat or physical ARP proof.

Full provider/config races pass in 1.107s/11.554s. Selected API/cmd races pass
in 1.447s/10.133s, including real envtest both providers, mounted route permissions,
source expiry and read-only Service update refusal; scoped four-package lint
reports zero issues. Review reproduced a replaced-Node/unbound-Lease bug before
the narrow fix. Exact Node owner UID and final Node/Lease head fences fix it;
selected provider/cmd races pass in 1.139s/10.519s, including an actual envtest
Node deletion/recreation while a predecessor-owned Lease is freshly renewed.
No real provider daemon/ARP/BGP or hardware failover qualification is claimed.
F40b keepalived audit/SSE remains separate work.

### F43b and supported F43a runtime paths

`e60001f` implements D276. The external console client has bounded commands,
output, cancellation and an exact random terminal acknowledgment; it never puts
keys in argv or emits raw diagnostics. Supported runtime removals/anonymous
additions preserve private staging, automatic health, accepted restart targets,
original purge delay and fresh status. Uncertain mutation converges through a
validated restart, without assuming rollback or replaying DNS queries. Named
additions retain the original YAML/Lua validator through a staged restart because
pinned dnsdist 2.1.3 rejects runtime validator installation. This fallback does
not close the fully restart-free named-addition requirement.

Certificate rotation uses validated immutable private snapshots, bounded reload
and exact fresh leaf verification for DoT/DoH/DoQ/DoH3. Invalid/half-rotated sources
retain accepted contexts and child-restart material. Actual pinned generated
supervisor acceptance passes in 47.497s, including held DoT, same PID for supported
updates, wrong-node refusal, real lost acknowledgment after rmServer and desired
restart convergence. Scoped supervisor/console/certificate races pass in 6.096s;
scoped lint reports zero issues. First-party targeted-health work for named
runtime additions remains independent and must pass actual same-PID acceptance.

### Hosted CI shutdown fixture correction

Security 41214453550, GitHub docs 41214453575 and kind/MetalLB 41214453416 passed
on 8bdb218. CI 41214453524 finished with one failure:
`TestServingCurrentCommandFinalCredentialClock/expiry/noop`, reporting
`serving_command_test.go:68: context canceled`. Closing the serving root before
the independent elector deterministically reproduces that fixture failure.
`0e8c3be` recognizes the root's original cancellation in the fixture callback;
production authority, time checks and deadlines are unchanged. Twenty repeated
root-shutdown and final-credential-clock race runs pass in 82.997s. Later commits
require fresh hosted checks; the previous run is not credited as fully green.

Integrated API/Go/TypeScript contracts now include cache inspection and VIP
ownership evidence; generated dispatch covers 157 operations. Complete generator
and client races pass in 4.589s/1.833s; reference drift passes in 0.029s, all 22
settings UI cases and dashboard typecheck pass, and the local docs site builds.
The settings mock preserves existing entries and adds only the six observation
keys. Two initial verification commands used a nonexistent Settings test filename
and a non-package docs directory; neither is counted as passing. Correct named
commands above passed. The modified dashboard/dist/index.html remains excluded.

Parent integrated mounted CLI/VIP/cache races pass in 29.632s and cache-stage
races in 1.035s. The combined command initially failed dnsdist tests because its
reexecuted race-instrumented fake waited Go's default one-second exit grace,
exceeding the original 250/500 ms test health/console deadlines. Agent checks had
supplied GORACE externally. `0435528` makes the fake helper set only its child
exit grace to zero while preserving race detection and other options. Parent
scoped dnsdist races then pass in 6.815s with the ordinary invocation; production
deadlines are unchanged. The initial combined invocation is not claimed green.
A fresh origin/main fetch remains fully contained in the branch.

### F18b bounded asynchronous reverse-PTR fallback

`83c1096` implements D281 through the existing identify stage. Eight host-only
restart settings opt into explicit source IP:ports and client networks; default
is disabled. The query path reads bounded memory and queues cold demand without
waiting. Fixed workers, LRU/queue bounds, strict direct PTR response validation,
original acquisition TTL, negative backoff and joined cancellation prevent stale
identity renewal or unbounded lookup work. Inventory/DHCP names and stronger
IP/MAC/CIDR tiers retain precedence; original requests/dependencies retain their
captured identity. PTR is observation, not authentication.

Actual UDP cache/expiry/dedup/ambiguity/bounds/late-response races pass; original
identity/glob/precedence races pass. Typed environment/bounds and both actual API
host-only write/reset routes pass. Actual runServe SQLite DNS proves background
PTR acquisition changes policy selection and source expiry removes it. Final
source PTR/mounted races pass in 2.303s/5.766s. Selected identification/config/API
races pass in 1.069s/1.656s/1.694s; the preceding complete config race passed in
10.465s. Scoped five-package lint reports zero issues. Initial CNAME fixture
encoded its header as PTR and was corrected; initial lint comments/style findings
were corrected. Those failed invocations are not credited green.

### F32 durable node settings and unsupported capability refusal

`ffcf20e` and `bd47907` implement D279. The original SQLite Command transaction
stages a bounded versioned node-bound local log overlay with unchanged final
writer, raw proof, dependency and clock fences. Defaults precede durable API
preferences, explicit host-file leaves and environment. Null deletes the overlay;
log level adopts after commit and format remains restart-required. No canonical
revision/feed or replica payload includes local preferences. Postcommit
cancellation is not described as rollback; a later admitted command can reconcile
a committed overlay awaiting local adoption.

Without the durable command capability, node sets/resets return 501 read_only
before legacy writes/adoption. Reads/schema remain available; node schema rows
advertise readOnly and the dashboard explains host configuration. Cluster settings
remain independently writable. `/settings?scope=node` is a refused mutation alias;
supported node changes use `/node/config` with its original permission/CSRF.

Agent full config/settings races pass in 12.760s/8.277s, actual mounted repeated
same-disk source/reset/logger-format acceptance in 62.368s, and original command
transaction/replication/source tests pass. The separate unsupported-write
regression failed before the fix, then repeated API races pass in 12.001s and
actual legacy/Kubernetes envtest/SQLite receipt checks pass in 10.096s without
skips. All 23 Settings UI cases, dashboard typecheck and scoped Go/UI lint pass.
Parent first integration SQLite/config/API/cmd races pass in
3.330s/2.316s/2.097s/19.039s. New composed checks remain recorded separately.

### F40b private keepalived journal to audit and bounded SSE

`380458f` extends D278 with the existing Consumer's additive random-session,
64-transition journal in its atomic 0600 state file. Read-only serve observation
baselines startup, deduplicates, checks original node/name/VRID/time and reports
restart/regression/gap/legacy/foreign/unavailable sources honestly. Source failure
closes old streams and refuses new ones until valid acquisition recovers; this is
journal availability, not DNS/physical VIP readiness. No second FIFO reader,
supervisor launch, permission broadening or weight-writer capability is added.

`GET /edge/vip/stream` requires edge.read, bounds subscriptions/events/writes and
rechecks original credential/authorizer/time on each emission. Original source
expiry survives audit and queue delay. Audit uses the existing recorder with a
bounded original timeout; refusal does not retry a transition. Real FIFO through
production observer/audit/actual router and real stored serving-token expiry pass,
as do source refusal, recovery, dedup/gap/cancel/queue/deadline races and scoped
lint. Parent composed mounted PTR/VIP races pass in 5.723s and selected API races
in 1.694s. Physical two-VM failover qualification remains external.

### F43a named runtime membership and hosted edge/security corrections

`88ca477` completes the targeted-health extension to `e60001f`/D276. The first-party
v1 name binds an exact captured node ID. Strict original-auth positive and random
wrong-owner probes qualify runtime support; ongoing targeted probes enforce node
identity, drain and readiness. Generic-only backends and structural changes retain
explicit validated restarts. Actual pinned dnsdist 2.1.3 YAML under UID953 and Lua
acceptance pass in 45.28s/44.50s with same-PID named additions/removals, original
PROXY auth, wrong-owner refusal, all TLS leaf reloads, retained invalid-pair
snapshot, child crash and lost-mutation-ack convergence. Default race invocation
passes; parent integrated nodehealth/ingress/log/dnsdist races pass in
1.011s/1.144s/1.066s/10.167s. Established connection migration is not promised.

Hosted security 41217188940 on 5b74077 failed only two Semgrep false positives:
custom exact-leaf TLS verification and an owner-only directory mode. `439ddc3`
adds narrow reasoned annotations without altering trust or permissions. The same
pinned scanner and all 593 rules pass locally with zero findings. The first local
attempt failed registry DNS inside the isolated Docker bridge; host networking
resolved that acquisition failure, with unchanged scan rules.

Hosted edge 41217189046 treated the new console-lab directory as a golden case.
`e581fc8` moves only that fixture to a flat file. Ten actual non-eBPF YAML/Lua
golden checks, Go golden tests (0.090s) and original-contract real supervisor
acceptance (42.119s) pass. Hosted full image/eBPF/volume checks still require a
fresh run; a partial local result is not credited as the entire workflow.

Final composed node/VIP/PTR races pass on the integrated source in
2.106s (config), 4.791s (API) and 15.723s (cmd), including real local envtest.
OpenAPI/Go/TypeScript generation now covers 158 operations. Generator/client/docs
races pass in 3.471s/1.621s/1.235s; all 23 Settings UI tests and dashboard
typecheck pass, and the local docs site builds successfully. The schema mock
updates only new/changed observation keys while preserving existing entries.
The user-owned dashboard/dist/index.html remains excluded from every commit.

A coordinated v5 idle hold confirmed all four agents and absence of Go/test/
compiler/lint/envtest processes before removing only regular Go cache hash files
whose access and modification times were both older than two hours. It reclaimed
5.409 GB; all agents were explicitly released afterward. No source, current build
output, daemon data or lab image was deleted. This is workspace maintenance,
not feature acceptance.


### F59 explicit direct Kubernetes CLI transport

D285 adds explicit `--transport kubernetes` for generic get/apply/delete/describe.
The default HTTP context retains its existing meaning. One selected kubeconfig
provides original namespace, TLS, credentials and impersonation; ambiguous API
overrides and unsupported command families refuse. Direct public CRD access uses
a restricted background-free driver facade with static shipped-CRD paths,
bounded responses/listings and one original command deadline. Apply pins original
absence/version and delete returns replacement conflicts without retrying.
Kubernetes admission/RBAC and manager ownership remain authoritative.

Scoped frozen-source races pass for internal/cli (1.432s), direct store/kube
(18.561s) and actual CLI/envtest (11.790s). Actual tests cover CRUD, namespaces,
impersonation/RBAC, managed ownership, version/UID replacement, source/deadline/
continuation refusal and no background requests; no homelab credentials are used.
Final scoped lint reports zero issues and CLI reference drift passes (0.030s).
No full repository result is claimed.


### F48b captured RP-initiated browser logout

`2947b02` fixes only the real-OP fixture timing: wait past original provider
CreatedAt before requesting numeric iat. `6f278bf` implements D282 with trusted
end-session discovery, exact enrollment-bound encrypted hint, original
credential/writer revocation, bounded continuation and authenticated callback
state. Local revocation and cookie clearing precede navigation; provider errors
or outage cannot undo them. Original deadlines and configured redirect prefixes
remain authoritative. Legacy and CLI logout remain local; large/missing/expired
hints also use local logout. Operator registration and cookie limits are
documented in the access architecture.

Agent frozen-source verification passes five full auth/adapter package races
(382 tests/subtests), selected real-router/CLI races (49), final focused logout
races (29), four API contract tests and scoped lint with zero issues. Actual
local TLS OP tests verify signed original hint, one end-session request, durable
revocation, callback/provider errors, closed OP, original expiry, installed
credential rotation, cross-session hints, CSRF and writer refusal. Dashboard
targeted four files/74 tests pass; final logout two files/20 tests, typecheck and
scoped lint pass. Earlier fixture/compile failures are recorded in the handoff
and are not counted as successful commands. No full repository result is claimed.
Parent merged generation and composed checks are recorded separately.

Parent composition caught an oapi-codegen schema/operation response-name collision;
renaming the source body type to BrowserLogoutBody preserves the wire contract.
Both OpenAPI versions, Go client/dispatch and TypeScript now cover 161 operations.
Final composed browser/provider and actual logout/direct-Kubernetes CLI races pass
(1.016s/1.318s/48.866s); the selected servingauth package had no matching tests and
is not credited as a suite. Complete generator/client races pass (2.998s/1.598s),
API drift/permission checks pass (0.479s), both logout UI files/20 tests and
dashboard typecheck pass. Initial composed attempts failed compilation before
execution due to the naming collision; they are not reproduction evidence for
the separate control-settings CI failure.


### Migration browser publication gate qualification

Hosted CI 41219077122 on 2a2971f passed security, but its dashboard VIP cutover
test treated a documented middleware refusal as an unexpected console exception.
The retained trace shows a successful cutover POST, followed by GET of the exact
report returning 503 with `generation_pending`, then a successful report GET.
This is not a failed or retried mutation.

The browser fixture now accepts that exact refusal only for an explicitly
declared same-origin GET path after verifying the bounded error body and absence
of cookies/redirect. Handler errors, unrelated paths, mutations and malformed
errors still fail. The VIP test deliberately injects one such response, requires
the real completed journal afterward and asserts exactly one cutover POST.
Three real browser repetitions passed before injection; two final injected
recovery repetitions pass in 22.5s. An intermediate injected run correctly failed
because Chromium supplied the HTTP/1 reason phrase; the matcher now recognizes
only the exact empty or Service Unavailable phrase, still requiring the verified
response. The API and production refusal/retry behavior are unchanged.


### F18c independent periodic inventory scheduling fix

`d7e98d8` integrates the narrow D284 scheduling fix: a refused automatic
inventory-only poll cannot permanently disable future observations while its
job/lease remain live. Each future timer event uses its own original frame and
normal admission; outbound Publishers and explicit commands are unchanged.
Controlled fencing, replacement and cancellation coverage and three full agent
host race repetitions pass (20.238s); parent complete host race passes (7.797s).
Full UniFi mounted hostname/native/edge acceptance remains in progress and has
not been marked complete.


### Kubernetes acceptance discovery and logout reference drift

The newer direct-CLI and VIP observer tests required KUBEBUILDER_ASSETS even when
CI had already installed its pinned envtest binaries under bin/k8s. They now
share the existing mounted-test discovery helper. A no-environment-variable race
run executes, without skips, direct CLI CRUD/RBAC (9.28s), VIP kube-vip/MetalLB
observation and mounted API (9.77s) and the new webhook sidecar (5.92s); the whole
selected cmd command passes in 26.175s. An initial helper extraction left one
unused import and failed compilation; the final command above is the pass.
The earlier parent logout-composition command did not set the variable and thus
qualified transport refusals but skipped direct CLI envtest; its 48.866s is not
credited as Kubernetes execution. The original F59 scoped real-envtest evidence
remains independent, and the no-skip rerun supplies current composition evidence.

Hosted GitHub docs 41220103686 caught three missing generated logout endpoint
rows. Both API schemas and clients were current; the endpoint Markdown reference
was not. `make docs-gen` regenerated it and the CLI reference, and the CLI drift
check passes. Local Hugo success alone had not checked reference equality.


### F35b explicit external-dns sidecar

D288 adds `kudurru externaldns-webhook`, a loopback-only transport relay to an
existing Kudurru webhook. The upstream retains licensing, reconciliation,
namespace, record ownership and current mutation admission. Remote connections
require verified HTTPS and a captured startup token; the embedded HTTP webhook
therefore needs an HTTPS frontend for remote use. The relay strips caller
authority, accepts only four protocol operations, bounds I/O/concurrency, joins
cancellation and never follows redirects or replays uncertain writes. This
implements the dedicated sidecar command; F35a direct DNSEndpoint reconciliation
remains open.

Final frozen-source `go test -p 2 -race ./ee/integration/externaldns ./cmd/kudurru
-run 'TestRelay|TestExternalDNSWebhookCLI|TestCLIReferenceDrift' -count=1 -json`
with GOMAXPROCS=2 passes both packages (2.546s/7.105s), eight test results and no
skips. Actual local TLS/router and disposable Kubernetes API tests cover CRUD,
manager ownership, startup credential retention, wrong token/CA, original write
refusal, redirect/lost-ack single attempts, body bounds and joined cancellation.
Scoped final lint passes with zero issues. Initial lint findings were fixed
before these final commands. Generated CLI reference drift passes. No full
repository, deployed external-dns client or direct CR watcher claim is made.

### F45b exact external-account administration

D289 adds bounded `users.read` inventory and current `users.write`
disable/enable/logout routes to the complete SQLite runtime, the
`iam external-users` CLI family and a separate dashboard external-account panel.
Commands require the reviewed independent UID and identity/floor revision.
Disable and logout atomically raise the account floor, revoke exact enrolled
sessions, delete exact-owned personal tokens (including earlier usernames) and
record local audit. Enable preserves revocation history. Same-name foreign
identities and tokens are unaffected. Original raw credential, CSRF, dependency,
writer and final time checks remain in the current command. Stack hosts explicitly
return `external_users_unavailable`; no Kubernetes administration is claimed.
Local administrators may revoke after provider-license loss; enable does not
bypass ordinary licensed provider admission.

Final frozen-source affected library races pass: security 119.177s,
Stack 14.614s, SQLite 291.043s and servingauth 68.170s. Final selected cmd race
`TestServingHTTPExternalAccounts|TestExternalUsersCLI` passes in 30.841s.
Acceptance includes real TLS OP enrollment, production router and generated
CLI dispatch, actual personal-token/session revocation, retained admin access,
stale UID/revision and malformed request refusal, pagination across two
same-name identities and changed-epoch cursor refusal, unsupported runtime,
provider-license loss, current writer refusal, final raw JWT expiry/clock
regression including a no-op, atomic token/audit rollback, SQLite restart and
actual replica installation/promotion. Earlier focused security/cmd races also
passed; these are scoped checks, not the full repository suite.

Dashboard tests pass nine cases, including exact reviewed command/CSRF,
single-send behavior, uncertain outcomes, caller replacement, read/write
permissions, unsupported runtime and recovery after a later-page cursor becomes
invalid. No optimistic mutation replay is introduced. Typechecking and scoped
ESLint pass. Scoped six-package Go lint passes with zero issues. Merged
166-operation OpenAPI/Go dispatch/TypeScript contracts, generated API/CLI
references and their drift/permission checks pass; local Hugo build passes.

Initial failures are not credited as green: a new test used the wrong SQLite
option field; a newly enrolled account's PAT request fell before its original
nanosecond creation boundary at persisted whole-second token precision. The
fixture now waits actual time before requesting a token; production checks were
unchanged. Initial lint found import/style issues, and an immediate React update
assertion required waiting for the caller replacement. Final source passed the
commands above. Physical identity-provider sessions are not ended by these
account operations; RP-initiated logout remains the separate F48b flow.

### F18c mounted UniFi hostname acceptance

Commit 380f43f adds actual TLS controller acquisition through the production
integration host, durable SQLite source and complete runServe DNS assembly.
It qualifies normalized legacy and official-name-only clients, exact hostname
precedence over nameGlob, conflicting IP claims, complete omission, exclusive
source expiry, failed-poll retention bounds, immutable held snapshots, independent
same-name allocations, source UID and mounted endpoint/credential replacement.
CNAME decisions retain the original policy within the unchanged request deadline.
The native path may correctly return SERVFAIL after its original primary image
expires; it never returns retained source answers.

Final source races pass: direct acquisition 40.456s; actual mounted native,
authenticated PROXYv2 and pinned dnsdist 2.1.3 UDP/TCP lifecycle 109.392s.
The opt-in daemon test explicitly ran with `KUDURRU_F18C_DNSDIST=1`; wrong PROXY
credentials are refused. Scoped lint reports zero issues. This is Do53 acceptance
with a TLS provider connection, not DoT/DoH/QUIC routing qualification. The earlier
independent periodic-observer scheduling fix remains separately committed as
d7e98d8/D284 and was not duplicated during integration.

### F61a public CLI loopback login

D286 and 4fddbce implement explicit public CLIClientID with loopback 8000/18000,
state, nonce and S256 PKCE. The original verified OP ID token is stored as bearer;
no Kudurru PAT, session or new server-side CLI grant is issued. Read-only public
source preparation and raw-proof verification retain exact provider/source,
installed credential, audience, nonce/hash and original expiry bounds. The
reusable AEAD receipt is source correlation, never authority. Preparation counts requests atomically per normalized client IP,
independently of authentication-failure buckets. Normal later current commands retain
original raw-proof, writer and final credential checks.

Agent frozen-source qualification passed all eight affected package race suites
(1,367 test/subtest results), complete selected CLI race (97 results, 135.227s),
final scoped lint (zero issues), and post-style CLI race (18 results). Actual
installed Chrome, verified TLS API/OP, signed JWTs and loopback callbacks ran;
no browser skip is credited. Receipt/source/secret rotation, protocol duplicates,
original expiry/clock regression, independent preparation IPs and actual
SQLite restart/follower verification are included. Initial Chrome failure was
an Ed25519 TLS test certificate unsupported by the installed browser; the
fixture now uses ECDSA TLS while retaining EdDSA JWTs and verified trust. Initial
lint findings were fixed before final passes. Merged OpenAPI/client/reference contracts pass; vendor-specific OP registration
beyond the local TLS OP is not claimed. F61b device flow remains a separate commit and acceptance record.


### F61b public CLI device authorization

D287 and b09fc77 add explicit `login --device` over the original public
CLIClientID. Trusted discovery must advertise the device endpoint, grant and
public client authentication. Polling honors pending/slow_down and connection
timeout pacing without replacing the original grant or widening its deadline.
HTTP refusal, redirects, unknown protocol errors and body-read timeout are
terminal. The CLI prints only deliberate verification instructions and stores
the original verified ID token; no RP session/PAT or refresh credential is issued.
Device verification preserves ordinary bearer admission, strict CLI audience,
optional access-token hash, original source/installed credential and time gates.
No standardized device-code-to-ID-token nonce guarantee is claimed.

Agent full affected core-package races passed 339 test/subtest results; the
complete selected CLI race passed 122 results in 230.753s, including installed
Chrome and actual TLS device flows, with no skips. Final targeted in-flight
cancellation, caller deadline, original clock and Kubernetes transport refusal
races passed five results in 8.4s. Final scoped lint reported zero issues.
Concurrent OP grant consumption, ambient bearer refusal, pending/slow_down,
expiry, source rotation, audience/azp/hash refusal, access-token-only response,
real SQLite restart/follower verification and bounded response/HTTP failures
are included. Merged OpenAPI/client/reference contracts pass.

An early pure-core fixture used the web audience and was correctly refused;
that fixture was corrected to the public CLI audience. A later cancellation
fixture waited for disconnect without draining its POST body, hung at teardown
and was stopped; it is not credited green. The fixture now drains the bounded
body and has an explicit teardown release. The final focused command passed.
The original synthetic prepare path did not establish a reproduced cross-IP
lockout: its error was not counted by loginResult. The new independently tested
request limiter counts five accepted preparations then refuses the sixth,
without touching the username/IP authentication-failure buckets.


### Merged CLI contracts and continuing acceptance

Merged OpenAPI 3.1/3.0, generated Go client/dispatch, TypeScript types and API/CLI
references now cover 169 operations. Full apiclient/clientdispatch/docsgen races
pass (1.705s/4.185s/1.395s), selected API OpenAPI/route-permission/generated-client
races pass (6.192s), dashboard typecheck and local Hugo build pass. The full
merged clioidc/oidccore/providerresources/servingauth races pass
(1.028s/3.296s/7.443s/58.161s), as does selected external-admin security race
(9.277s).

The merged selected command suite is NOT green: it passed 51 test/subtest
results, including actual installed Chrome, device flow and external-account
administration, but TestRunServeUniFiHostnamePolicyLifecycle returned SERVFAIL
after the original held CNAME request reached its five-second deadline
(5.165s observed). That regression remains under investigation; no deadline or
original policy guard was relaxed. Earlier isolated F18c acceptance remains
recorded separately and is not substituted for a successful merged command.

Hosted security 41380836776 and GitHub docs 41380836809 passed on 5ca4054.
Main CI 41380836896 failed only Go shard 1 at TestRunServeQueryExport: its initial
single login received 503/unavailable after DNS became ready. Other applicable
main CI jobs passed. These are old-head results; a new pushed head requires new
hosted checks. The separate Cloudflare dashboard docs check remains excluded by
the owner's direction.

### Initial primary status readiness in settings acceptance

85f9243 integrates the separate signed-off settings fixture correction. The
bounded read-only startup observer now requires Ready=True/Serving for the
actual zone generation and an active lifecycle with matching UID and source
resource version. A controlled real controller/HTTP/SQLite interleaving proved
that observedGeneration alone could accept the initial unavailable phase, then
Ready/lifecycle writes legitimately superseded a held one-shot PATCH. The
original publication check refused that request; credentials and writer were
not replaced.

An actual HTTP 100-continue regression retains a PATCH through an independent
CSRF-authorized login publication, proves the original cookie remains valid on
a fresh read, and requires the held PATCH to return 503/unavailable without
changing settings. The corrected isolated settings restart race passed all 15
repetitions (170.464s), original command/credential-clock/shutdown fences passed
(9.199s), and scoped lint reported zero issues. No production code, mutation
retry or request deadline changed. The historical 3/15 failures did not capture
enough diagnostic data to prove their exact past interleaving; the controlled
matching readiness defect is the causal evidence.


### Query-export startup publication qualification

The query-export fixture now observes the actual primary Ready/lifecycle source
and then a complete HTTP `/auth/status` publication before submitting its one
login. Source writes can finish while HTTP still reports `generation_pending`;
that distinct initial-publication phase is read-only and bounded. Authentication,
writer checks and production deadlines are unchanged, and login is never
retried. Ten query-export and ten settings-restart race repetitions passed in
one complete command (399.851s); final eight-package scoped lint reports zero
issues. The original source-only candidate failed with `generation_pending`
and its remaining repetition process was stopped; it is not credited green.
The historical hosted failure was `unavailable` at current command admission,
so no exact retrospective interleaving is claimed without its missing trace.


### F39b/F54b original-publication edge cache safety (D283)

The seven integrated implementation/test commits through `61bb854` add
authenticated, challenge-bound remote publication observations and one bounded
cache controller. Both resolver and authoritative pools start uncached, including
render-only and restart configurations. Positive grants require original served
client-independent input; default per-client limiting, scoped lists/RPZ, zone
ACLs and unproven dynamic input conservatively bypass shared packet caching.
Served ACLs gate both pools. Authoritative permission is separate, false by
default and restricted to proven static primary/forward data.

The agent's final focused default-race acceptance used the actual mounted
SQLite runtime and pinned dnsdist 2.1.3: authoritative cache hits preceded an
ACL change, then allowed client 127.0.0.2 received the answer while denied
client 127.0.0.3 received REFUSED in both query orders. Auth/stalled-pull bypass,
recovery and same-PID ordinary transitions passed (66.338s). Earlier resolver
acceptance covers regex allow/deny changes, late original responses, timed
blocking, client/group/forward ACL isolation and uncertain-child retirement.
Scoped original-owner/controller/protocol races and final lint passed. These
are scoped agent results, not a merged full-suite or final release claim.

Remote invalidation remains asynchronous within original source deadlines,
pulls and acknowledged convergence. It is not atomic cross-node ACL cutover.
Unknown console acknowledgment may restart the matching child and interrupt
connections. No query is replayed, no remote arbitrary pool command is added,
and legacy or incomplete backends stay visibly uncached. Dynamic zone routing,
physical deployments and final composed release qualification remain separate.

### F18c composed held-CNAME deadline qualification

The later combined CLI/F45/F18 race command failed its held-CNAME phase: the
fixture withheld upstream for over seven seconds while the original request
correctly expired after five. The first correction then exposed a missing
original-policy precondition after unrelated expiry work. Neither failed command
is credited as green or treated as evidence of production policy loss.

Integrated `4337c79` schedules a complete authenticated UniFi poll before the
held request, captures the proven Phone policy, and publishes renamed inventory
within the original five-second lifetime. File endpoint/credential replacement
is qualified independently afterward. Three frozen focused races passed
(160.145s total), with held boundaries at 871ms, 1.690s and 1.470s; the full
lifecycle passed 139.618s, and scoped lint passed with zero issues. Native,
authenticated PROXYv2 and pinned dnsdist 2.1.3 UDP/TCP remain exercised. This
proves held policy across inventory publication, not across file reconciliation;
no production timeout, credential gate or refused request was relaxed/replayed.

### Hosted journal restart fixture and merged cache checks

On `c7f13a5`, hosted CI's second Go shard failed only
`TestKeepalivedObservationJournalTransitionsAndRestart`: an intervening valid
`source_ready` notice after `source_restart` reached the test before its next
transition. The original test reproduced locally under the race detector.
`cb838e0` explicitly observes that recovery notice before writing the next
transition. The corrected case passed 100 repetitions (3.510s); the full VIP
package race passed (1.372s). No observer, journal, deadline or API behavior
changed. The failed hosted run is not retroactively green.

Merged focused original-cache/publication, authenticated internal protocol,
dnsdist controller and original-operation-proof races passed respectively
8.117s, 1.305s, 4.196s and 9.403s. Hugo built the updated deployment diagram and
operator contract. The root integration prerequisites preserve pre-queue inputs
and recheck reads as well as writes after budget reservation. Public integration
administration and webhook conformance remain F60c work in progress; helper
qualification is not route/effect qualification.

### F26e complete-view auth control reload (D290)

Integrated `a506100` fixes the controlled real-server mismatch where status
reported installed session locks of 2m idle/7m absolute while issuance used
default/canonical values. Immutable typed controls and exact copied host locks
now participate in the same current View and its aggregate dependency identity.
Five public controls reload on complete SQLite only; original-view cookie
selection/writes/clears share policy, and postcommit policy mismatch refuses
sealing without reissuance. Existing session/token deadlines never extend.
Global/legacy/Kubernetes/host-only restart metadata remains conservative.

The isolated six-library race command failed solely OpenAPI drift (166 generated
versus 169 existing source operations); its five other packages passed. The API
package had 595 passing entries and three existing external PostgreSQL/envtest
skips. Local uncommitted regeneration corrected the contract, whose focused
race passed 5.145s. The final mounted/router/current-command race passed all
97 entries, 56 top-level tests, 347.282s, with no skips; scoped lint passed with
zero issues. Those results do not turn the earlier failed aggregate command
green or claim the skipped external environments. No new operation IDs or
generated files were included in the F26 implementation commit.

On the merged root, the composed operation/auth-controls/route-contract race
passed servingauth (12.040s), API (4.749s) and CLI (41.284s). This verifies
F26 host locks compose with the retained original-operation authority helper;
F60c public integration routes remain under separate qualification.


### F07a encrypted source and current-transaction prerequisites (D292)

Integrated `717d0d7` and `6cb85a8` add a private bounded encrypted TSIG source,
original owner/credential epochs, immutable prepared rings and same-transaction
source checks before current DNS mutation/no-op/replay admission. Actual SQLite
backup/restart/follower promotion and local Kubernetes Secret UID/CAS/restart
fixtures passed. Core source/currentguard races passed 6.688s/18.748s; selected
legacy/native keyring races passed 3.071s/2.287s and scoped lint reported zero
issues. Earlier encrypted-source and actual-envtest evidence is retained in the
agent's exact qualification report.

These are prerequisites, not F07a closure. Production coordinator, serving
transport activation, original wire ownership and background transfer rotation
remain in progress. Kubernetes evidence currently covers encrypted persistence
and explicit setup/cold reads; hot key management is not qualified. No secret
values or encrypted source credentials become public configuration/API fields.

### Hosted supervisor SAST and DoT fixture follow-ups

On pushed `b687223`, hosted SAST reported two retained-zone-history data-flow
findings. Integrated `eaf977f` extracts the existing successful-observation
history and reads captured result fields separately. Pinned Semgrep 1.181.0
reproduced both findings before and neither afterward; no suppression was added.
Focused history/controller races passed 2.601s. This is not a full-security-run
claim.

The same head's second Go shard failed the DoT socket fixture four times with
EOF before the asserted query-count limit. Its sequential client scheduling
could cross the separate 150ms idle deadline. Integrated `f7bd976` queues both
allowed original frames in one bounded TLS write and checks their IDs/questions;
all original idle, maximum-query, TLS, rotation, load and shutdown limits remain.
A controlled actual server-close fixture demonstrates sequential query2 EOF
after idle expiry and both prequeued replies after maximum-query close, followed
by one refused third query. The unmodified random local baseline passed once
(2.039s), while corrected/controlled races passed ten repetitions (11.769s) and
scoped DNS lint passed. The hosted failure remains recorded, and fresh CI is
required for these corrections.


### F60c reviewed integration administration tranche (D291)

Integrated `1975029` adds seven bounded API routes and matching generated-client
CLI list/get/check/preview/sync/actions/act operations on complete SQLite. Every
execution retains explicit UID/revision, original caller and host authority;
Cloudflare sync also requires its exact complete preview digest. Source-only
implementation excludes generated files. Legacy/Kubernetes execution remains
unavailable. Current built-in providers advertise no actions.

The expanded frozen scoped race command passed API1.852s, client1.064s,
host1.084s and CLI87.628s. It runs the real Cloudflare SDK over a verified local
TLS peer with original canonical origin, stale-preview409/no effect, confirmed
write, and applied-but-unreadable503/pending/no replay; real UniFi TLS acquisition
and source publication; actual router/generated CLI; stored cookie CSRF;
scoped-token/stopped-writer/refused transport; and original revocation between
provider requests. This does not claim external provider infrastructure. The
independent original-proof/source/time/replay/disclosure source review found no
blocking issue. Scoped lint initially found four missing DTO comments, then
passed with zero issues. Later source changes only document error response
schemas and exported types; final merged generation remains required.

Earlier commands failed due an unexported embedded path DTO, a viewer fixture
without the declared permission, a new request during publication, and an
optional-digest comparison in the fixture's publication wait. Those failures
remain recorded; they are not relabeled green. The final wait compares actual
term/revision without retrying a refused command or changing production guards.

F60c remains open for webhook conformance, merged contracts and remaining
all-driver/restart release evidence. Exact-RV status is conservative and may be
unknown after its own status publication until a matching observation arrives.

### F44b public SQLite RoleBinding tranche (D294)

Integrated `c44b67d` adds reviewed create/get/replace/delete API and generated-client
CLI commands. Exact UID/resourceVersion and API ownership checks share one
transaction with security floors and audit. Original proof/current permission,
source, writer, clock and cancellation checks apply to changed and no-op effects.
Generic aliases remain refused; legacy/Kubernetes mutations explicitly return503.

The complete focused three-package race command passed API4.139s, client1.089s
and cmd32.999s:34 test/subtest entries, no failures/skips. It covers actual TLS
OP cookie/PAT removal and regrant without revival, genuine fresh enrollment,
actual generated CLI/router, UID/revision/manager/CSRF/scopes, final proof
refusals and durable replica installation/restart/promotion. Replica evidence
covers stored state, not a second restarted HTTP runtime or Kubernetes. Frozen
scoped lint passed with zero issues. The14 source/generated Go fingerprints
remained unchanged during verification; the commit contains12 source/test files.

Earlier commands failed due a schema-name collision, an unused import, conflict
error mapping and fixture status/input assumptions, including bulk apply's
per-item refusal under HTTP200. Those outcomes remain failed commands; the final
command above is independently green. Isolated173-operation generation stayed
uncommitted. Root must regenerate the combined180-operation contract.

F44b remains open: file/inline RoleBinding writes still use ordinary CRUD and
the controlled BEFORE reproduction is prepared but not run. Kubernetes accepted
authority, native desired-write ordering and multi-replica HTTP qualification
remain design/acceptance work. No all-driver completion is claimed.

Hosted `b687223` Go shard1 (job125088928076) subsequently completed successfully
at2026-11-02T04:34:03Z. Shard2 and SAST failures recorded above remain failures
of that head; their integrated corrections still need fresh hosted qualification.

### F07a captured transport tranche (D292)

Integrated `a404a30` retains the exact immutable TSIG provider from the original
publication before verification and through response/transfer writes. Opt-in
captured native mode owns bounded original frames; fixed mode remains available.
Actual same UDP/TCP/DoT/DoQ/HTTP2/HTTP3 connection tests prove held-old/fresh-new
keys, transfer MAC chains, original deadlines, PROXY/SNI identity, native proof
and encrypted no-proof paths, unsigned foreign-signature stripping and cleanup.
Final captured/error race passed10.137s; selected legacy DNS13.067s and
servingbuild5.264s; scoped frozen lint passed with zero issues after three
mechanical corrections. Pending cmd coordinator files were excluded from the
commit and these checks. Production activation, background key use, management
routes and broader Kubernetes rotation remain open; this is transport capability
evidence, not a mounted stored-key completion claim.


### F60c standalone webhook conformance (D295)

`791b49e` adds `integrations webhook conformance` with an explicit target,
trust roots and bearer or mTLS files. It does not resolve ambient API contexts,
Kubernetes credentials or runtime providers. Original captured input bodies and
one whole-run deadline govern version negotiation, advertised read probes and
explicit opt-in mutation probes. Fresh HTTP/1 requests cannot redirect or replay
bodies; uncertainty stops subsequent mutations without cleanup. Fixed reports
separate passed checks from incomplete coverage and uncertain effects. This
implements the standalone command, not the remaining Tier-B runtime lifecycle.

Actual local TLS acceptance exercises the first-party external-dns server's
read/create/identity-only delete path; Tier-B tests verify exact original event
bytes, HMAC and documented delivery header, selected action and credential/file
replacement. Tests cover mTLS, URL prefixes, 307/308/503/disconnect single-attempt
behavior, null/duplicate/unknown/bounded JSON refusal, original cancellation and
caller deadline across requests, and CLI ambient-token nonuse. Capture limits
caller wait and outstanding workers; a blocked OS file call can retain the one
worker slot until it returns, without sending a late request.

Initial focused package/CLI tests pass 29 named/subtest entries with no skips
(0.838s/0.054s). Focused races pass the same 29 entries (2.962s/1.217s).
Initial lint found four local close/style/input-path diagnostics; the corrections
retain all protocol and deadline behavior. Final scoped lint reports zero issues,
and the post-style full new-package race passes in 2.866s (27 entries). These are scoped checks, not a
full cmd or repository run. Merged generation, hosted CI, Tier-B provider runtime
and the remaining all-driver/restart gates remain open.


### Merged administration contracts, November 5

Merged OpenAPI 3.1/3.0, Go client/dispatch, dashboard types and API/CLI references
now contain 180 unique operations: the original 169 plus seven integration and
four RoleBinding operations, with no original operation ID renamed or removed.
Generation, dashboard typecheck and the Hugo documentation build pass. The
user-owned built dashboard artifact is preserved.

The first composed race command remains failed: API (5.661s), generated client
(1.241s) and servingauth (11.366s) pass, while cmd (168.742s) has one failure in
`TestServingHTTPRoleBindingsReviewedCRUD`; 264 other test/subtest entries pass,
with no skips. Its stale-delete assertion received 503 instead of 409. A private
unchanged-assertion diagnostic reproduces two failures in 20 independent race
fixtures: both retain valid writer/base/dependencies but encounter publication
3 changing to 4 at the original-publication check. The production refusal is
preserved. The fixture issued an unchanged PUT, which wakes the asynchronous
publisher, before its later target-conflict commands. The diagnostic does not
uniquely identify the wake caller, since startup signals may coalesce.

The correction gives unchanged replacement its own fixture, with that PUT as
the final HTTP operation. Exact 200/409 expectations, unchanged UID/revision and
image assertions, and the explicit replaced-publication refusal test remain.
No production guard, retry, sleep or broader accepted status was added. Both
fixtures pass 20 independent race repetitions each (40 entries, 128.691s).
The broader selected RoleBinding/router/CLI command separately passes all 18
entries in 41.351s without skips. Generator package checks pass (clientdispatch
0.502s, docsgen 0.042s), and final scoped API/client/servingauth/conformance/cmd
lint reports zero issues. Frozen test and user dashboard artifact hashes match.
The original composed command remains failed; these are corrective scoped
results. Fresh hosted CI and release qualification remain open.


### F44b SQLite file and inline grant history

Integrated `91443d7` mounts the required security capability before first load.
The original accepted source pass and exact target UID/revision/manager stay
pinned through a transaction using the original startup or elector writer.
Both no-op paths cross the guard. Grant floors and object effects commit
together, with revocation time observed after writer-lock acquisition; final
clock/cancellation and source checks remain. Installed inline maps are detached
using the existing decoder, preserving Go integers and load diagnoses. No
ordinary CRUD fallback, successor writer or refused-operation retry is added.

Controlled BEFORE evidence on both actual directory and inline sources showed
the same original cookie/PAT regain operator authority after removal/regrant,
with no operator floor. AFTER uses exact 200->401->401 responses and floor
0->1->1 for both proofs/layouts; genuine later OP enrollment succeeds. Actual
runServe first load and three process starts cover offline removal/regrant.
Replica image installation/restart/promotion and follower refusal also pass.
These prove stored state behavior, not native filesystem atomicity, a second
replica HTTP runtime or Kubernetes accepted authority.

The initial full ordinary command failed two fixture assumptions: a deliberate
memory recorder advances RV on each Apply, and stopping an elector does not
itself replace durable writer incarnation. Corrected fixtures review each
original row and separately test original elector-context cancellation and an
actual same-term incarnation replacement while retaining the old token. The
corrective ordinary selection passes eight entries (objects0.120s/cmd0.928s).
Full objects races pass43 entries in3.974s; selected source/adapter/actual OP/
runServe races pass28 entries in90.345s, with no skips/failures. Frozen scoped
lint reports zero issues; all nine source hashes remain unchanged. Initial
failed commands remain recorded separately. F44b remains open for Kubernetes
and final composed qualification.


### F27 bounded authenticated denial reuse (D296)

Recursive validation now retains complete validated NSEC/NSEC3 proof bundles
from one response and signer, with only the signatures that actually verified.
Covered NXDOMAIN, exact/wildcard NODATA and positive wildcard questions can
avoid another authority exchange. Normal alias handling remains in force;
opt-out, mixed or incomplete proofs and CD queries use ordinary recursion.
No new resolver setting or public API operation is introduced.

A separate cache bounds entries, encoded bytes, records, signer-context buckets
and candidate probes. One cumulative synthesis hash allowance spans the original
client query's aliases and nested lookups, preserving ordinary validation work.
Original TTL/signature/SOA/exception deadlines and trust/flush identity survive
copying, delayed consumers, outer-cache insertion and dump/load. Hits cannot
renew proof validity. RFC 5011 automatic anchors remain a separate open item.

Qualification uses actual signed UDP/TCP authorities, installed BIND signing
and serving private root/child zones, independent synthesized-signature checks,
and the native cache pipeline with delayed insertion and restore. The final
full recursor, upstream-stage and upstream-package race command passed **209**
named/subtest entries; only the existing opt-in live-Internet test was skipped.
BIND was explicitly enabled and ran without skips. After test-only lint style
corrections, the BIND race passed **11** entries (1.405s package time) and scoped
lint reported **0 issues**. All production hashes were unchanged between these
checks. This is scoped local evidence, not a final hosted/release result.

Earlier failures remain part of the record: the first signed-peer fixture failed
before queries because the helper did not support root NSEC3/wildcard signing;
a private signed child fixed that setup. Independent review then found that
per-lookup synthesis allowances could consume the normal validation reserve
across aliases. Two controlled regressions failed before the cumulative fix,
including an actual signed eight-response alias walk returning SERVFAIL. Both
pass in the final full race command. Initial lint findings were corrected;
no validation, expiry or work-bound assertion was relaxed.

Hosted CI on the previously pushed `517ac93` separately failed
`TestCombinedSourceCannotSampleSameGuardSuccessorFrame`; its diagnosis and
correction remain pending. No full hosted-green claim is made here.


### Hosted Integration frame-boundary fixture and composed checks

On `517ac93`, all main CI jobs except Go shard 1 passed; that shard's observed
failure was `TestCombinedSourceCannotSampleSameGuardSuccessorFrame`. The fixture
replaced the instance inside outbound Apply, before receipt persistence, while
asserting an already-confirmed effect. Original-frame admission correctly refused
the receipt write. Independent review confirmed the boundary mismatch; no
production change was warranted.

The corrected fixture covers both timings using the actual fenced ledger writer.
An in-flight effect remains pending with no saved receipt or confirmed baseline.
Replacement after the exact durable confirmation retains Applied=1 and the
original complete receipt. Both cases forbid binder, observer and source-commit
calls and retain Degraded/zero-LastSync status. Replacement occurs only after
the writer releases its lock, without sleeping, replaying or resampling a frame.
Twenty race repetitions passed **60** named/subtest entries (2.496s package);
the full host race suite passed **247** (11.620s), with no failures or skips.

Root composition of the F44 file/inline startup and actual TLS grant-revival
checks with recursive NTA reload and native cache flush/growth passed **9**
entries (113.322s package), with no skips. Final host/cmd/objects scoped lint
reported **0 issues**; all frozen Go fingerprints stayed unchanged. The original
hosted failure remains recorded separately. The installed-BIND CI job now runs
the F27 proof-reuse race explicitly; fresh hosted qualification is pending.


### F07a coordinator and acquisition prerequisites (D292)

Source commits `d5df9df` and `df79fc5` integrate as `28077a8` and `a694d3f`.
Typed TSIG lifecycle plans preserve original UID/resourceVersion/source checks,
validate the complete intended postimage in the same transaction, and reject
later same-kind mutations through the final in-memory journal fence. Optional
coordinator and acquisition interfaces retain original encryption, credential
source and whole-job deadlines. Local cache persistence precedes a source/time
postcheck and memory-only manager publication; the separate databases are not
claimed atomic. File-backed acquisition fingerprints retain their prior format.

The full seven-package ordinary regression passed **1,127** named/subtest entries
(109.904s wall) with eight explicit opt-in skips. The later installed-BIND RPZ
race enabled its opt-in and passed **23** entries (2.517s package), with no skips.
Selected serving lifecycle/current-command races passed **20** entries (36.361s),
with no skips. Initial scoped lint findings were corrected narrowly; affected
kind-fence/mutation races passed **29** entries, then final six-package scoped
lint reported **0 issues**. Earlier compile and fixture-readiness failures remain
recorded independently. The fixture now waits for actual Live publication before
its single reconciliation; no guard, time limit or mutation assertion was relaxed.

These are source/helper/coordinator prerequisites. Production SQLite activation,
management routes, Kubernetes startup capability and final mounted qualification
remain open; no generated artifacts were copied or new routes advertised.

Root merged coordinator/current-command races then passed **13** entries in
13.244s (59.797s including compilation), with no skips and all tracked Go
fingerprints unchanged. This verifies composition of the prerequisites; it
does not qualify the separate uncommitted serve activation.

### Hosted import-report publication refusals

CI run `41958343910` on `fedef02` failed the dashboard background-import test's
console check. The actual browser trace confirms two `generation_pending`
HTTP 503 responses: the exact history GET with `limit=25` and the original
run-detail GET. Functional import assertions passed. The fixture now declares
those exact read targets, verifies the bounded refusal body, injects one refusal
for each, and requires real durable report/history recovery with exactly one
start POST. It retains all partial-success and skipped-object assertions.

Dashboard typechecking passes. The selected background-import test passes
locally (14.2s), followed by all three migration cases (25.5s), using the
pre-existing October 31 embedded-dashboard binary. These are test-fixture
results, not a newly built current-head server claim. The first local command
stopped during setup because the root binary embeds the placeholder; that
failed result remains recorded. No production API/UI or mutation retry changed.
The hosted run's BIND e2e job passed, including the new authenticated-denial
case; one Go shard was still running at this checkpoint. Full hosted success
remains unclaimed.

### F28 pure trust-anchor lifecycle prerequisite (D297)

Source `67abdb0` integrates as `3ed7dee`: five new `internal/trustanchors`
files, with no runtime/configuration/store/resolver mount. Explicit DS bootstrap
and every transition bind the original immutable state and authenticated DNSKEY
observation. Actual signatures establish every supporter and own-key REVOKE
identity. Candidate acceptance requires the original 30-day/first-TTL hold-down,
only checkpointed same-session monotonic progress, and a fresh post-threshold
observation. Missing keys remain trusted; revocations remain bounded permanent
tombstones; all-anchor loss quarantines instead of falling back to insecure
answers or shipped defaults.

Review added original observation TTL/signature/monotonic consumption bounds
and retained bootstrap evidence for the future initial durable CAS. Construction
already refused expired evidence; the bootstrap correction makes that original
proof available at final admission. The strict codec validates complete bounded
history but proves neither a commit nor a writer. Proposed checkpoints become
accepted only after a successful original-admission durable CAS.

Final full pure-package race passes **62** named/subtest entries (1.271s), with
no skips. Initial lint found a missing exported-block comment and deprecated
elliptic point validation; the narrow comment and `crypto/ecdh` validation
corrections preserve key bounds, pass the final race, and report **0 lint issues**.
All committed source fingerprints match the final freeze. Earlier lint failure
and ordinary passes remain separate evidence.

This uses actual signatures with controlled private samples, not a 30-day live
soak. The elected durable owner, feature negotiation/migration, replica install,
scheduler and complete-image resolver mount remain open. Final local response
admission must share retirement exclusion; an asynchronous flush is insufficient.
The existing remote packet-cache lease also cannot prove immediate retirement,
so managed activation requires proven preinstalled bypass or a stronger protocol.
Follower quarantine must be durable and deny-only. None of those mount guarantees
is claimed by this pure package.

### Tier-B runtime transport prerequisite (D298)

`c8553ef` adds a private captured transport in `ee/integration/webhook`; no
production factory registration or provider capability is enabled. Named bearer
or mTLS material and request payloads are detached before the original host
request gate. Every actual attempt is separately charged and retains original
cancellation/deadline through body consumption. Verified TLS, literal-loopback
HTTP restrictions, fresh HTTP/1, no proxy/jar/redirect/GetBody/retry and bounded
methods, paths, headers and bodies apply.

Full ordinary tests pass, followed by **18** race entries with no skips. Actual
TLS/mTLS, original payload retention across a held host gate, refused host before
peer I/O, redirect/4xx/5xx single attempts and in-flight body cancellation/deadline
are covered. Two lint expression-style findings were corrected; the affected
input/TLS race passes **5** entries and final scoped lint reports **0 issues**.
The original failed lint output remains separate evidence.

`685f53f` adds detached protocol negotiation: exact media, matching manifest and
capabilities, filters and advertised action schemas form one retained identity.
Ambiguous JSON, Unicode case aliases, malformed/null declarations, changed
capabilities and credential echoes cannot produce a usable successor silently.
Full ordinary and final package race each pass **35** entries with no skips;
final scoped lint reports **0 issues**. Action schemas have structural bounds;
the existing host still owns semantic parameter validation before invocation.

Provider lifecycle is still being implemented. Record publication requires strong remote ownership and partial receipts; inventory
requires genuine observation time and fixed expiry. Neither a legacy record
list nor HTTP 204 supplies those guarantees. The existing standalone conformance
command remains independent, and no Tier-B production completion is claimed.

### Hosted zone-read fixture correction

CI `41960281277` on `962bd1e` reports one dashboard failure and 153 passes.
The trace shows the exact records GET returning `503/unavailable` with
`committed zone read: serving: generation pending`, then recovering. The reviewed
dry-run and single DELETE each returned 200; the console fixture failed.
`13c914c` allows only that exact declared read refusal and injects it to prove
record-table recovery before one deletion. Other errors remain failures.
Selected local case passes (13.9s), zones plus migration pass all six (39.7s),
and dashboard typecheck passes. These used the existing embedded-dashboard
binary, not a newly built current-head server. Hosted confirmation remains
pending; the full failed run is not relabeled green.

### Tier-B private lifecycle prerequisite

`8195b5a` adds an unregistered action/event lifecycle. Provisioning resolves only
named credentials and performs no network I/O. The first successful Check binds
one declaration; a later observed change permanently fences that incarnation,
including a return to the original declaration. Action schemas are detached and
the host retains semantic validation/RBAC. Each action sends one POST. Events
retain the host's original body/time/delivery ID and separate HMAC key; only the
existing durable host delivery lane may attempt them again.

Full package ordinary and race each pass **46** entries with no failures/skips;
final installed scoped lint reports **0 issues**. An earlier mistyped lint binary
path exited 127 before a scanner ran and remains recorded separately. Tests use
actual TLS for credential retention, declaration replacement, single actions,
signed stable deliveries and cancellation/stop. Record and inventory execution
remain explicitly unsupported in this committed private lifecycle; no production
factory or integration capability is enabled.

### Matcher controlled regression checkpoint

The original single performance process exited successfully but failed relative
qualification. An exact-source overlay of pre-F20 `c6efeb8` and unchanged current
source was then compared in two fresh processes on this VM, using the same 12
original benchmark leaves and 65,536-name inputs. Both processes passed; suffix
controls were stable, while current regex500/1000 misses were about **14.6×**
slower and mixed-policy misses about **2.5–3.2×** slower. This is a material
regression, not a passing performance gate. Unchanged compilation was about
1.09×, so the much larger historical baseline delta cannot be assigned to F20
from these observations. Safe literal filtering before dense dispatch is being
corrected in the isolated worktree; baseline, budgets and gates remain unchanged.

### Tier-B private inventory prerequisite

`29e6aa5` adds complete bounded client/network observations to the private provider.
Original `LastSeen` fixes expiry at three minutes; later polling cannot renew
stale devices. Fixed-IP metadata grants no observed address. Partial/paginated,
null, duplicate, malformed, credential-echoing or cross-network inconsistent
lists refuse. No desired DNS/ClientGroup writes are generated; two list requests
make no remote snapshot atomicity claim. Full ordinary and race each pass
**64** entries, no skips/failures, with **0 scoped lint issues** and matching
frozen hashes. Production registration and capability-aware scheduling remain
unmounted; strong record ownership/partial receipts remain open.

### F28 accepted-owner and transaction prerequisite

Driver source `3cf992e` integrates as `fe0b352`; adapter `a518992` integrates as
`f8b79ed`. The bounded section-prefix postimage fence detects every later actual
same-prefix change, including ABA, while allowing genuine no-ops and unrelated
owners. Its final closure reads only private memory. Accepted-owner framing
bounds complete dormant/managed histories, chunks and envelope/pure-state CAS.
Persistence retains the original writer, context, clock and authenticated
observation; a staged proposal cannot become authority before the outer command
commits. No physical migration, feature registration, runtime or resolver mount
is included.

Frozen driver races pass **46** entries, accepted codecs **21**, persistence
**94**, all without skips. Lint first found three private helper parameter-order
issues. Only those signatures/calls changed; full persistence ordinary then
passed **94** entries and final three-package lint reported **0 issues**. The
prior test-file race freeze is preserved separately from corrected ordinary
qualification. Driver/accepted/pure races were not needlessly repeated.
Retirement through local command/replica admission, durable follower deny-only
quarantine and preactivation managed edge-cache bypass remain open.

### Matcher actual-path diagnostic

The safe-literal correction passed its focused independent oracle but did not
recover the original performance workload: regex500/1000 remain roughly
**15–16×** slower than the exact same-VM preimage. A bounded instrumented overlay
then used the original complete 65,536-name corpora and normal warmup. Both
fixtures actually use fast `Regex`, with no `PatternSet` calls. Regex1000 executes
all 16 mixed batches per name despite independent literal exclusion of 90% of
rule/name pairs; every batch retains eligible members. Policy misses execute ten
individual engines per name, already frozen after warmup. This establishes
execution/count evidence, not timing qualification. Cache/batch redesign,
unchanged performance gates and final qualification remain open.

### External-dns relay shutdown correction

The hosted CLI failure discarded its shutdown error, so its specific cause is
not established. A controlled local BEFORE did reproduce a concrete edge:
an accepted TCP socket awaiting its first HTTP request exhausts the relay's
original two-second Shutdown budget. The relay now closes those sockets when
stopping, including a socket returned by an accept already in progress. Active
handler cancellation/drain, original deadlines and actual errors remain intact.
Seven relay race tests pass, scoped lint reports zero issues, and both original
CLI tests pass under race with actual local Kubernetes envtest (no skips).
The original failed BEFORE and hosted run remain distinct evidence.

### Negotiated inventory scheduling and hosted checkpoint

`3f144ed` adds capability-aware observation scheduling before a Tier-B factory
mount. Actions-only providers disable inventory timers and reject manual sync
without publishing empty observations. Client polling has a one-minute cap;
shorter explicit Resync wins, including when a stored definition has the generic
ten-minute default. Original observation expiry and existing provider scheduling
remain intact. Full host/webhook races pass **258/67** entries without skips,
and final scoped lint reports zero issues.

`4ad694a` retains bounded safe migration-step diagnostics after three ordinary
and five race repetitions passed both original AdGuard/Blocky cases. The race
command passes **15** entries, no skips; original production, assertions and
15-second journal deadline are unchanged. The initial diagnostic wrapper's
postprocessing failure remains recorded separately. No stalled worker was
reproduced and no migration production fix is claimed.

Main CI `41964466507` on `e9f2324` is fully successful, including dashboard-e2e
and both Go shards. That qualifies the pushed head, not later local commits.
The earlier failed run remains failed. Automatic root hook-event production,
strong record ownership, managed-anchor runtime mounts, TSIG activation and
matcher performance remain open.

### F28 physical owner/schema prerequisite (D297)

Source `7b4eacf` integrates as `54216ee`. Schema 7 creates only a non-granting
root-anchor marker; the existing original startup command binds a dormant owner
and validates it on restart. Existing inventory no longer bypasses the third
owner. Complete images require the exact normal owner and mandatory
`managed-root-anchors-v1` feature. Old schema/features refuse without received
image conversion, history reset or same-revision divergence exceptions.
Actual upgraded primary/follower fixtures preserve the old authority metadata
and use the recomputed transitional follower base for the next complete install.
Local.db keeps layout 2 while advancing its shared schema marker. Old quarantine
bytes remain preserved but the current feature-checking reader refuses them.

Initial selected ordinary qualification passes **60** entries. The expanded
full SQLite run preserves **277 passes plus one failure**: the fixture's global
one-entry limit was occupied by the new physical marker. Only its allowance
changed to marker-plus-one-test-row; original ambiguous input, rollback and
second-row refusal assertions remain. That corrected case passes separately.
Full ordinary replication/localdb/runtimeowners pass **31/17/27** entries.
Final focused races pass **25/8/1/27**, no skips, and four-package lint reports
zero issues. The original failed full command is not relabeled or repeated.
No managed updater, resolver, local output retirement, follower deny-only
quarantine, edge-cache bypass or Kubernetes activation is included. Composed
root startup qualification follows integration separately.

The reviewed next prerequisite is an unmounted immutable resolver projection:
Valid/Missing keys supply canonical DS values, quarantine supplies none, and
identity includes both envelope and pure history. It grants no durable or runtime
admission. Planned mode changes refuse ordinary static/default downgrade after
managed history, preserving permanent revocations. This design is not a mounted
updater or recovery path.

Fresh CI `41967856451` on `3622013` exposes one unused test helper left by the
migration diagnostic replacement. `f873643` removes only that dead helper; the
active original-step observer, 15-second journal wait and assertions are
unchanged. The hosted lint failure is preserved; local scoped lint and fresh
hosted qualification for the correction remain pending.

### F28 immutable resolver input prerequisite

Source `3a3718e` integrates as `e6e7a09`, adding exactly four unmounted
`internal/trustanchors/resolverstate` files. Projection validates the complete
structural owner, selects only Valid/Missing SHA-256 DS values, and retains
permanent deny identities. Invalid, dormant and quarantined states provide no
static/default fallback. Detached owner identity binds envelope and pure
versions; identical keys or an expired historical observation cannot substitute
for original durable admission or runtime authority.

Ordinary qualification reports package success. Full package race reports
**22 passing test entries**, zero failures/skips, and scoped installed lint
reports zero issues on the same frozen source. Actual signatures, all four key
states, self-revoke, distinct same-tag keys, bounds, detachment, held original
expiry and identity framing are covered. Root verifies the integrated blobs
and unchanged prerequisites without repeating those checks. Managed updater,
accepted-image resolver publication, durable local denial, command/install and
final-output retirement, edge bypass and recovery remain open.

A later root-only cmd/webhook lint attempt stopped on disk headroom before any
diagnostic; it supplies no lint result. Its raw termination and unchanged source
freeze are preserved separately. Root startup/mount checks remain pending; the
completed resolverstate qualification above is unchanged.

### F28 bounded retirement primitive prerequisite

Source `4842a28` integrates as `49fc20c`: six standard-library-only files add
bounded local exclusion and a monotone owner/floor cursor. One atomic state
joins scope exit, original plan completion and prepared binding swaps. Confirmed
effects can resolve after cancellation while the same scope remains held;
ambiguous or unresolved effects close admission. An escaped scope cannot mutate
a successor. A successful borrowed read check does not extend the callback's
counted hold or authorize detached output.

Final frozen ordinary and race runs each report **80 passing test entries**
(31 top-level cases and subtests), no failures/skips; installed scoped lint
reports zero issues. The original lint failure with nine style findings remains
separate. Equivalent style corrections required no production behavior change.
Root independently verified all six committed blobs and 23 unchanged root
dependencies before integration. The four agent-local generated artifacts were
preserved and excluded. No duplicate tests were run solely for the cherry-pick.

This package is not mounted and supplies no durable, source or response
authority. Actual elected updater, accepted-image resolver, mandatory command,
InstallReplica/ResolveConflict and final-output exclusion, durable local denial,
cache/restore coverage and installed edge bypass remain open.

### F60c SQLite Tier-B clients/actions mount

The registered webhook constructor and SQLite factory now support finite client
inventory and declared actions. Public negotiation refuses hooks/records and
does not acquire an unused HMAC key. The private delivery prerequisite remains
separate. Original `LastSeen` determines expiry; complete polling cannot renew
it or turn `FixedIP` into an observation. Host schema/RBAC and original caller,
credential, writer and final source checks surround one action attempt.

Final mounted TLS/router race reports **7 passing entries**, no failures/skips;
full provider race reports **71 passing entries**, no failures/skips. Installed
lint across cmd, webhook and recursor reports zero issues; this also qualifies
the earlier dead migration-helper removal `f873643`. Root's original schema
directory/inline startup/restart cases passed their earlier ordinary run.

Preserved earlier failures are distinct: two new inventory assertions confused
current serving admission with retained durable timestamps; the narrow diagnostic
observed absent serving publication after credential rotation. Corrected focused
ordinary tests passed. Early action races hit pre-admission publication refusals.
An intermediate repeated provider preflight continuously charged budget writes
and failed setup; separating that preflight restored three action cases. The
viewer case then reported `generation_pending` with zero peer posts. The final
new fixture excludes its inherited unrelated sample primary zone before startup,
as existing current-command fixtures do, and retains the real SQLite writer,
publisher, TLS peer and router. All action/observation assertions and single
mutation attempts remain. No production admission guard or deadline changed.

Exact freezes, failed and successful logs, raw numeric completion and corrected
source backups remain in `.context/tierb-mount-qualification/`. Hooks/event
production, remote record ownership, Delete lifecycle, Kubernetes atomic source
admission and remaining restart/final release qualification are still open.

### Local corrections after hosted CI 41967856451

The query-export fixture now observes protected-serving health within its
original ten-second setup budget, alongside the existing public auth-status
read, before its one login. This supplies an omitted setup precondition; it
does not reserve future writer authority or identify the earlier exact refusal
branch. Its original mounted acceptance passes under race (one test, no skips).

The cookie authority fixture acquires both ephemeral UDP/TCP sockets before
starting servers. Only `EADDRINUSE` permits another setup attempt, at most 16;
other errors and exhaustion fail with the actual bind error. No DNS request is
retried by this setup. Cookie and held-trust-change races pass with **28 entries**,
no failures/skips. The hosted bind failure omitted errno, so its cause remains
unproved. Installed cmd/webhook/recursor lint reports zero issues.

The command package had previously passed in 1,688.604 seconds, then exhausted
its 1,800-second aggregate budget. `make test` now discovers the actual compiled
Test/Example/Fuzz inventory and selects every top-level name in exactly one
batch of at most 100. Each includes all descendants/seeds, race instrumentation,
count 1 and the unchanged 30-minute deadline. A failure is retained and later
batches still run. Four Python coverage/failure-propagation checks pass. Actual
compiled discovery finds **467 names**, partitioned 94/94/93/93/93 with no
duplicates/omissions; this is discovery evidence, not a full suite pass.

Earlier hosted failures and local diagnostic failures remain preserved. Fresh
hosted full CI/security evidence is still required on the pushed correction.

### Matcher grouped dispatch and BeginText diagnostic

The isolated F20 worktree now groups indexed and unindexed rules while retaining
original match/Visit order, charges an optional bounded ASCII-equivalence proof,
and terminates an empty successor only when every member's public `StartCond`
requires BeginText. The latter preserves collected tags and final cancellation.
Corrected focused ordinary qualification passes **19 top-level tests / 118 test
entries**, without failures/skips. Its first failed fixture incorrectly treated
an empty rune language as the public no-match sentinel; the corrected fixture
distinguishes both public compiler shapes. Production proof behavior did not
change in that correction, and the failure remains separately preserved.

Exactly one further original 12-leaf diagnostic completed with child/wrapper
exit 0 and all 58 metrics on frozen source `faf33add`. Sparse regex100 miss is
1,188 ns/op; mixed-policy Evaluate miss is 1,573 ns/op. Dense regex500/1000 miss
is still **14,084 / 27,761 ns/op**, or **2.812× / 2.595×** the retained same-VM
preimage. Seven observations exceed the original relative threshold and floor,
including unchanged-compile allocations of 28 versus 22. The measured suffix
p99 and Evaluate allocation ceilings were not exceeded; unselected gates remain
unqualified. Shared-VM single samples do not establish fixed-class performance.

No source is integrated from this worktree yet. Current dense-path attribution,
the independently discovered regex-plus-CIDR parser gap, comparator/fuzz/races,
full admission/peak memory, five-process performance and final CI gates remain
open. Earlier failures, original workloads/budgets and the StageA wrapper failure
with unknown numeric Go exit are retained unchanged.

### x/net security update and schema-owner fixture corrections

Hosted security run `42313409266` on `cf02b0a` reports CVE-2026-78661 in
`golang.org/x/net` v0.60.0, in both module lockfiles and the four product images.
`c9ab39f` updates the main and isolated AD spike modules to the reported fixed
v0.62.0 plus required x/crypto, x/sys, x/term, x/text and x/sync versions.
Module tidying also removes the unused koanf file-provider dependency and
classifies the already-used pflag dependency as direct.

The first affected-package race command retains its **exit 1**: parser, DHCP,
DNS server, auth users and SQLite querylog packages pass; catalogue reports
87 passing entries and three failed entries in one commit/cancel test. Its
command/error/pin assertions pass, but total runtime-row expectations still
assume no schema-7 sentinel. The run records 1,020 passing entries and five
explicitly skipped opt-in measurements overall, not a green full command.

`f86b161` changes only catalogue, secondary and license test fixtures: capture
the original runtime rows, then require exact preservation plus the intended
test row for a committed mutation, or exact preservation after cancellation or
no-op. The two analogous secondary/license assertions were found by source
review, not reported as reproduced failures. All original pin/error/timing
assertions remain. Corrected focused races pass **7 entries**, no failures/skips;
installed three-package lint reports **0 issues**. Passing unchanged packages
were not repeated.

The AD spike race suite passes **37 entries**, with its explicit MIT
interoperability case skipped because the local KDC tools are unavailable.
That is not Windows or MIT acceptance. The installed Trivy 0.76.0 filesystem
scan exits 0 with **zero vulnerability, secret or misconfiguration findings**
under the existing policy. Only generated/dependency/build output, Git metadata
and private workspace artifacts are excluded; no product source is exempted.
Raw receipts, the original failed fixture and scan output are retained under
`.context/security-xnet-20261106/`. Hosted security run
[`42315068174`](https://github.com/robertherbaugh/kudurru/actions/runs/42315068174)
on `4b962e2` passes SAST, vulncheck, filesystem and all product/license-server
image scans. Full CI remains separate and was still running at this checkpoint.

Hosted CI `42313409142` also exposed two fixtures whose physical setup no
longer represented their intended preimage after schema 7. The historical
audit fixture now removes exactly the fresh anchor sentinel from the closed
database before setting its schema-5 marker. The missing/orphan-owner fixture
removes that exact sentinel from a stopped current-schema database, optionally
inserts its original orphan, and reopens it. Both require exactly one deleted
row; production migration, initialization, redaction and whole-image refusal
assertions are unchanged. The two selected tests pass under race detection
with **4 actual pass entries**, no failures/skips; installed API/persistence
lint reports **0 issues**. Frozen source identities match. Original hosted
failures and pre-edit fixtures remain separate from these corrected results in
`.context/schema7-followon-fixtures-20261106/`.

### F07a SQLite stored-key activation (D292)

`f725fdf` integrates the five-file activation from signed-off `80050f5`.
SQLite startup shares the original encryption/keyring owners between complete
serving and secondary/RPZ acquisition. Listener capture hooks are configured
before binding; an initial file baseline cannot substitute for an unavailable
complete stored source. Kubernetes remains on fixed startup keys, and no
stored-key management route is added by this tranche.

The isolated ordinary lifecycle, native UPDATE/NOTIFY/AXFR, six-transport
restart and TCP/TLS acquisition selections pass on that worktree's frozen
dependencies. Parent qualification on the merged x/net v0.62.0 tree separately
passes all **7 selected top-level tests / 9 actual pass entries**, no failures
or skips, under race detection. Package time is 132.465 seconds; outer time is
201.563 seconds. The TCP test retains its real expiry wait; TLS acquisition
retains its joined same-executable child. All 2,275 frozen source/module/artifact
identities match. The first command-package lint exits 1 with three private
test-helper context-order findings and one G204 warning for executing the
original test binary with fixed arguments. The correction reorders only those
helper parameters/calls and adds that exact subprocess rationale; production,
contexts, deadlines and assertions are unchanged. The failed lint receipt is
preserved. The two affected ordinary tests pass after that correction, with no
failures/skips (package 2.145 seconds); installed command-package lint then
passes with **0 issues** (19.044 seconds). Reversing only the argument reorder
and removing only the subprocess comment reproduces the earlier test bytes.
All other frozen sources are unchanged; the unchanged race selections were
not repeated.

The earlier isolated lifecycle and native failures remain preserved; a later
successful readiness observation does not identify an earlier rejecting fence.
Actual held-current-source behavior, initialized-follower restart/promotion,
management and Kubernetes activation still require their named acceptance.
Private merged receipts are in `.context/tsig-activation-merged-20261107/`;
isolated scope and commit identities are recorded in
`.context/tsig-sqlite-activation-integration-handoff.md`.

### Regex/CIDR modifier delimiter correction

The separately discovered parser gap is corrected at the regex/modifier
boundary. A rule such as
`/^client[.]example$/$client=192.0.2.0/24,dnstype=AAAA` previously selected the
CIDR slash as its delimiter and was discarded as an invalid URL. The parser
now finds the modifier boundary before interpreting its values, respecting
regex escapes, classes and literal quoting in one bounded scan. Existing bare
regex validation and modifier routing remain intact.

The complete parser ordinary suite passes **144 entries**, no failures/skips.
Focused contextual-rule races pass **35 entries**, including actual IPv4/IPv6
and mixed-prefix selection, question type, original rule identity, exceptions,
badfilter cancellation and retained-list independence. Installed two-package
lint reports **0 issues**. All frozen identities match; the original sources
and exact receipts are retained in `.context/parser-cidr-20261107/`.
This is independent of the isolated F20 kernel and its performance failures;
the count probe used its unchanged original parser/workload/module identities.

### November 7 hosted migration-preview fixture

On pushed `cd72760`, security, Compose, edge, AD and Kind/MetalLB workflows
pass. The dashboard suite reports 153 passes and one DNS-name cutover failure;
the Go CI shards are still pending at this checkpoint. The saved browser trace
shows a successful preview followed by a 409: “A zone changed after the
preview.” It establishes a stale reviewed resource version, without identifying
the writer that changed it.

The fixture now waits for the initial primary Ready status before obtaining
its preview, using the same observed-generation predicate as the neighboring
VIP case. Initial controller status publication is a real resource-version
change. The original mutation remains a single submission, with explicit
HTTP 200 and one-request assertions; guards and deadlines are unchanged.
The focused actual browser case passes once (14.6 seconds; outer 15.436),
including stored and served NS/SOA checks. Qualification uses committed
`86d3d57` through an explicit source overlay and the hosted dashboard bundle;
the independent schema8 drafts are excluded. All 2,981 frozen paths match.
The first local invocation printed Bun usage and the second selected no tests;
both receipts are retained and receive no test credit. No mutation was replayed.

Hosted logs/artifacts are in `.context/hosted-cd72760-20261107/`; exact build,
binary and browser receipts are in `.context/migration-browser-cd72760-20261107/`.
The original hosted failure and final-head full CI gate remain open until a
later successful hosted run.


### F28 durable denial and schema8 composition in progress

Exact driver/local-denial source16 from `7058351` integrates as `af6cd63`.
The isolated ordinary and race selections each pass 66 entries; final scoped
lint passes with zero issues after the exact unused-parameter rename. The
first compile failure, first ordinary fixture failures and first lint failure
remain preserved. An early rollback child passed before reaching its SQL target
and receives no injected-rollback credit; the corrected fixture proves actual
SQLite constraints and disposition through the private driver body.

Parent composition registers schema8 and mandatory managed-root-runtime-v1,
initializes Dormant/local state in one original startup Command, and binds only
committed evidence. Genuine schema7 migration, feature/schema and localdb
selections pass 8/16/3 entries; corrected full runtimeowners passes 27. The first
runtimeowners command failed because its corruption fixture used a newly
protected public deletion; stopped-database corruption now reaches the original
missing-history refusal without weakening that guard.

The captured awaiting lane imports the reviewed five source files and grants
no history, gate or serving permit. Combined parent anchor ordinary passes
186 entries without skips, including the original driver cases, local denial,
genuine legacy upgrades, first-install CAS, exact no-ops and actual SQL fault
boundaries. Earlier compile and fixture failures are separate: a test passed a
DB instead of a transaction, a missing-owner case expected the wrong category,
legacy setup lacked authority metadata, and a primary/mirror kind registry
mismatch refused before install. Corrections preserve source guards and original
assertions; no prior failed command is relabeled. Full parent persistence
ordinary now passes 95 entries, including the original positive managed restart,
follower image, canonical tombstone and quarantine acceptance. Follower ordinary
passes 12 entries, retaining original pre-fetch authority through 304 and
interposed installations. An initial follower fixture incorrectly reused an
operation in its independent winner; the corrected winner has its own lifetime.

The first full SQLite ordinary run passes 482 entries and fails two upgrade
children plus their parent: uncaptured installs stop before the intended stale
baseline/divergence checks. Explicit capture before source acquisition restores
all eight upgrade-selection passes without changing their expected errors or
durable divergence assertions. That failed full run remains separate evidence.
Combined default races now pass all six packages: 677 entries with no failures,
skips or race diagnostics (SQLite package 265.201s, outer 277.075s). Downstream
fixture/startup qualification now passes the 13 selected top-level command
tests (17 entries), while runtimekinds/cluster-replica/TSIG source pass 64 entries
and servingbuild passes 202. The first servingbuild command compiled no tests
because an introduced context name collided with a later local declaration;
the fixture-only rename and successful rerun are retained separately. The remaining17 fixture source files also pass default races across11packages:
all80 intended top-level names and144 entries, no missing/unexpected cases,
failures or skips (outer105.387s). This includes hosted inbound DecodeImage
failures, with actual image metadata time and original historical observation/
expiry assertions. The final 14 selected startup/follower/NOTIFY top-level tests
also pass default races: 18 entries, no failures/skips, package 110.850s and
outer 160.485s, with all 2,324 frozen identities unchanged. Installed scoped lint
initially reports two tagged-switch style findings in one awaiting-install test.
Only those branch forms changed; the affected test then passes all five entries
and the same 23-package lint passes with zero issues (outer 6.616s). Production
bytes remain identical to the successful race freeze; earlier failures remain
separate evidence. Private receipts retain exact freezes under
`.context/f28-parent-*-20261107/`.

### F20 indexed-lane final-source qualification in progress

The isolated current owner adds an exact selected-gram absence guard before
safe dense indexed scans. Focused ordinary passes 24 top-level tests/171 entries.
One original 200ms diagnostic reports dense 500/1000 regex misses at 851.6/1818ns,
about 0.17 times the retained preimage; policy hit/p50 still flag relative
arithmetic, and prior dense/compile failures remain retained. This single
shared-VM sample does not close official performance gates.

The unchanged original 10k modifier workload admits all entries, reserves
103,375,626 bytes under the unchanged 128MiB cap, and preserves complete-candidate
refusal/rollback. Eight fresh constructor helpers retain their original expected
acceptance/refusal outcomes. Reservations, isolated constructor peak and full
serving RSS remain distinct metrics.

Current default core races pass 385 entries with two explicit skips: the
standalone memory helper and the then-unconfigured comparator. A separate actual
pinned AdGuardHome v0.107.79 run passes all 46 comparator entries with no skips;
the actual daemon was observed. Captured API default race passes four entries
with no skips. All current23 and original-workload identities match. Independent
fuzz has since passed one bounded 20s run: baseline 97/97, 5,933 coordinator
executions and 30 new interesting inputs. The count reached its final value by
reported second 3, then stayed unchanged through second 21; no sustained
throughput or count of valid-language comparisons is inferred. The unchanged
69-case diagnostic now completes all 291 original metric keys, with zero bytes
and allocations reported in this process. Five latency comparisons exceed the
original 20% plus 25ns floor: unindexed 50 warm, dense 500 early warm, dense 1000
early/late warm and long 256 warm. Unindexed cold and saturated modes remain
slower than the same-process standard-library reference; prior parallel 6B/op
and other failed samples remain evidence. This is no official performance pass.
Five fresh full benchmark/RSS processes, fixed-class performance,
scoped lint/SAST and final merged CI remain open. Original StageA wrapper failure
and unavailable numeric child outcome remain unavailable, not reconstructed.


### Hosted cd72760 final Go shard findings

The final hosted Go shard fails the mounted manual NOTIFY restart/rearm case
with HTTP 503 outside the explicitly retryable prehandler pending response,
and seven inbound integration DecodeImage cases (eight failed entries including
a child) on invalid anchor-owner fixtures. The existing dashboard failure is
independent. The complete hosted run remains failed; root is composing the
inbound fixtures with actual schema8 owner initialization and investigating
NOTIFY readiness without replaying admitted intents or weakening final fences.
The mounted NOTIFY fixture now observes existing read-only Ready/status and
lifecycle source alignment before capturing each original intent head. The
focused ordinary test passes (package 5.095s, outer 11.224s); its exact original
restart, serial/content/sequence and response-category assertions remain. The
hosted log did not identify which final dependency refused, so the source-based
readiness diagnosis is not presented as a reproduced causal trace. The mounted
NOTIFY test also passes in the 14-test default-race selection. Hosted
confirmation remains a separate gate.
Private final shard output is retained under
`.context/hosted-cd72760-20261107/go-shard1-final.log`.

### F28 captured poll and complete-publication prerequisites

`21d0ed9` adds the opaque receipt-bound poll bridge and bounded numeric TCP/UDP
wire helper. Combined ordinary and default-race selections pass 137 entries,
including all 21 poll top-level cases; installed scoped SQLite and wire lint
pass. The production elected updater, managed resolver, final output/cache
retirement and installed edge bypass remain unmounted.

The parent root caller draft separately retains the original complete settings
identity, host, writer, clock and policy before network acquisition. Six actual
caller tests pass ordinarily, including signed TCP bootstrap, normal refresh,
two-phase self-revocation and denial before an expired original host predicate
refuses granting work. That last case injects predicate loss; it does not claim
an actual provider replacement or automatic worker activation. The draft mode
and caller are uncommitted pending the complete serving composition.

The reviewed publication adapter imports onto actual parent schema8. Store-issued
attempts bind the committed owner/local carrier and original image token;
retirement exclusion precedes the writer lock. The complete publisher applies
registration immediately before its whole-pointer swap and retains an actually
swapped bundle across an outer completion error. Preparation can opt into an
original finite lifetime before reading its candidate. Focused ordinary passes
43 entries (22 driver and three publisher top-level cases), with no failures or
skips. The real publisher tests use test owner components and assert the actual
pointer/resource boundary; they do not qualify the production resolver or output
seal. Full SQLite/publisher default races then pass 592 entries with no failures,
skips or race diagnostics (SQLite 289.909s, publisher 5.985s, outer 302.114s).
All 2,301 frozen inputs remain unchanged. Final installed scoped lint passes with zero issues in 29.052s. Its first
invocation found two issues confined to the separate uncommitted caller test
helper; the corrected parameter order and explicit bounded length preserve the
qualified publication sources. Exact evidence is retained under
`.context/f28-parent-publication-20261107/`.

### Hosted schema8 audit fixture correction

On pushed `58aed1f`, Go shard1 passes and shard2 fails the historical audit
credential-redaction fixture: constructing schema5 from schema8 left behind its
private local-anchor carrier. Migration8 correctly refused that preexisting row.
`7649b8d` deletes only the exact fresh migration-proven awaiting carrier while the
fixture Store is closed, asserting one affected row. Historical audit data,
redaction assertions and production migration guards remain unchanged. The exact
fixture passes ordinary/default race; installed scoped lint passes. The first
lint finding on an unrelated draft constant comment and a subsequent launcher
path typo remain separate failed receipts. `7649b8d` is pushed with the qualified
poll prerequisite; hosted CI on that new head remains pending.

### F20 five-process sampling remains incomplete

Four of five original performance processes and three of five original RSS
processes have completed once each, with all original source/workload identities
unchanged. Performance samples report 35, 37, 40 and 20 provisional regressions;
all 35 first-sample failures still trigger in sample3. Sample4's fewer triggers
do not erase any prior observation. Available absolute ceilings
are satisfied in those individual samples, without aggregate qualification.
RSS1/RSS2/RSS3 steady values are 92.73/92.45/92.25MiB under the original
150MiB ceiling; peaks are separately 139.4/141.0/139.5MiB. All three 3M compile
times regress against the retained 2.412s baseline; RSS2 is exactly
4.913/2.412 = 2.036898839138 times, and RSS3 is 4.252/2.412 = 1.762852404643 times.
The earlier handoff arithmetic typo is corrected without changing metric values.
No current five-process median, fixed-class pass, official Go gate or aggregate
serving/held-old-plus-new RSS result is claimed. Prior adverse samples, CPU-class
mismatch, missing zone keys and all earlier limitations remain open.

### F28 source currency and cache identity prerequisite

The Store-issued resolver input now observes exact source/local/cursor state and
one immutable installed gate mark without requiring prior registration. This
allows the first complete publication to check source currency before Apply;
publication work cancellation never expires a successfully registered input.
The fixed cache domain includes full accepted owner, canonical local carrier and
fresh input incarnation. Equal-key checkpoints, local-only denial and reopen
cannot silently reuse a successor's domain. Neither method grants publication,
cache insertion or final output authority.

Exact four-file parent composition passes 85 ordinary and 88 default-race test
entries, no failures/skips/race diagnostics; the latter includes all three real
publisher tests. Installed scoped lint passes with zero issues. All 2,303 frozen
inputs match. The ordinary command's publisher prefix selected zero publisher
tests, recorded separately rather than counted; the race selection includes them.
Evidence is under `.context/f28-parent-current-cache-20261107/`. The original
full SQLite/publisher 592-entry race remains earlier-source evidence.

The separate root caller six cases pass default race. An initial combined
selection compiled config but selected no config tests; a subsequent exact
selection passes the three config tests and all 13 entries under default race.
The root mode/caller draft remains uncommitted pending complete composition.
Common static-to-managed drain, builder/root seal, final transports, cache
insertion exclusion, installed edge bypass and elected updater remain open.

### F28 common mode and first-transition drain prerequisite

The parent Store now composes bounded Static/Managed/Closed mode exclusion with
actual primary commands, follower installs, local denial and complete publication.
First managed acceptance drains counted static callbacks before SQL mutation.
Total active plus waiting admission is bounded to 256 readers and 16 writers;
FIFO anti-barging preserves queued writers. Original lifetimes use private wait
plumbing, and cancellation never releases another active callback's hold.
Unresolved entered callbacks and uncertain completion close admission through
the same CAS used by resolution. Static history creates no managed snapshot.

Final full SQLite default race passes 649 entries (276 top-level), including all
36 new mode cases/51 entries, with zero failures/skips/race diagnostics. Full
persistence95, retirement104 and serving15 passed separately in an earlier
aggregate that itself failed. Installed scoped lint passes with zero issues.
Evidence: `.context/f28-parent-mode-20261107/`.

Earlier failures remain explicit: an undersized three-minute full-package bound,
two original compatibility failures, a zero-test unused-import compile failure,
and an overbroad error-category correction. Final capture retains whether its
original cursor had a retirement gate: stale initialized commands keep
`ErrMismatch`; awaiting first-install races keep accepted-owner `ErrCAS`.
The existing private postcommit-denial fixture now enters mode before R while
preserving its exact actual Commit/cancel/Resolve boundary and every assertion.
No nil-mode production bypass or old assertion relaxation was introduced.

This qualifies counted mode/SQL composition. Actual transport writes, cache
insertion, installed edge bypass, root seal and updater activation remain open.
The previously pushed hosted run on `7649b8d` completed with a migration test
clock race in Go shard1; shard2 passed. Its exact diagnostics and proposed
test-only receiver correction remain separate from this mode evidence.

### Hosted migration clock fixture correction

Hosted run42325871280 on `7649b8d` found a race in the VIP-write migration
rollback fixture: it replaced the running runtime's clock function while an
integration worker read it. The two matching migration expiry/rollback fixtures
now use a private, unmounted command receiver with the same actual Store,
publisher, provider and primary capabilities. Only that receiver gets the
controlled clock; no runtime mutex or worker is copied or replaced.
Original credential, transaction, VIP/no-op and journal assertions are unchanged.
The exact two cases/four entries pass default race, alongside the parent
caller/builder selection; installed command-package lint passes with zero issues.
Evidence: `.context/ci-7649b8d-final-20261107/`. Hosted failure logs remain intact;
this local correction does not itself establish a new hosted green run.

### F20 completed five-process measurements retain failed aggregate

All five original performance processes and five fresh RSS helpers completed
once on unchanged source23 `cd36732f` and original workloads. All 123 five-value
vectors, 615 values and 121 original full-baseline findings are preserved.
The bounded collector exits 1, its control wrapper exits 1, and independent
control integrity is valid. Full arithmetic has 36 measured regressions plus
20 missing zone keys; scoped arithmetic retains all 36 measured failures.
There are 51 unchanged, five improved and nine new modifier metrics, with no
absolute-limit violations. CPU class 2.50GHz/GOMAXPROCS2 versus 2.90GHz/6 and
absence of original Go benchgate qualification remain explicit.

Median steady RSS is 92.45MiB versus the original 150MiB ceiling; separate
peak is 141.0MiB. The 3M suffix/exact fixture does not qualify regex, aggregate
serving or held-old-plus-new RSS. Median compile time is 4.493s versus 2.412s.
Unchanged-list compilation remains 508,879,489ns/25 allocations versus the
committed 62,725,199ns/eight, with policy and regex tail regressions retained.
All earlier individual failures, six-byte parallel observation, StageA missing
numeric outcome, fuzz plateau and report-generation failure remain recorded.
Evidence: `.context/f20d-current-five-process-aggregate-result.md`
(SHA256 `c5cd48a7a5b890f3cf7c4fef5146a895337d7f73ab72eaffb79ae04898f4d22a`).

Source review traces the million-line full parser fingerprint to contextual
modifier support before F20. This explains structural work, not CPU attribution
or permission to waive the gate. The authorized isolated two-file lazy-budget
preparation changes only list.go and program_budget_test.go. Its subsequent parent
qualification is recorded below; full parsing/fingerprint serialization and exact
reservation checks remain. Dedicated fingerprint streaming and sparse batch
dispatch remain separate proposals.

### F28 root output and raw-cache admission composition

The parent binds an actual complete private root component with independent
finite query attempts. Native UDP/TCP/DoT writes, HTTP1/2/3 header/body flush,
and DoQ Write+FIN finish under original mode/retirement admission. Actual
completion remains distinct from later errors; a completed transport is not
reset merely because scope exit observes a later refusal. Full DNS-server
default race passes344 entries with four unchanged opt-in measurement skips;
root integration passes17 with zero failures/skips. Subsequent style-only
corrections pass installed scoped lint with zero issues. Earlier ordinary,
race, lint and preparation failures remain preserved under
`.context/f28-parent-output-20261107/`; race and final lint freezes are distinct.

Detached raw-cache leaders capture a separate insertion attempt from the
original complete lease before acquisition. Final cache mutation holds
mode/retirement exclusion before cache locks, after encoding and before
resource release. Actual insert/delete/failed-refresh paths share admission.
Queued insertion retains the original TTL origin. Every managed raw scope
includes the exact owner/local domain, including forwarded and CD data;
managed dumps are not promoted. Static restored promotion uses admission.

Package-specific race evidence: underlying cache34, corrected full cache
stage45, root/bootstrap plus original transport/restart3, and final
cache/builder selection31 (15 stage/16 builder). Scoped installed lint passes
zero issues on all2,336 frozen inputs. The two earlier commands failed: a new
stale fixture was clamped by default minTTL, and the first restore helper
over-rejected an existing held CNAME read. Their original sources/receipts
remain; the final unchanged CNAME assertion passes after retired semantic
currency skips mutation without refusing the library read. Root admission
still refuses actual retired sources. No single all-green aggregate is claimed.
Evidence: `.context/f28-parent-raw-cache-20261107/`.

Production managed input and updater remain disabled. These cases do not
complete the native/private cache, API dig, installed edge bypass, full
transport retirement/interposition or external interoperability obligations.
The exact original accepted-owner, restart, follower and tombstone requirements
remain; no reset, static fallback or acceptance waiver is introduced.


### F28 API dig final HTTP handoff

The complete SQLite API mount now captures its original private root output
attempt before pipeline work. Typed Huma output stays unchanged; a bounded
4 MiB buffer holds serialization until the actual status/header/body write and
explicit flush enter the original mode/retirement scope. Response and cloned
lease remain alive until that handoff finishes. The earliest request/proof
bound controls the actual write; cancellation interrupts pending output and
joins at callback completion. An actual completed flush survives later
cancellation/scope errors; partial writes abort. Guarded direct handler calls
refuse answer export.

Final API default-race selection passes **36 entries / 14 top-level cases**,
including real TLS HTTP/1 and HTTP/2 peers, postflush cancellation, original
deadlines, refused/oversized/partial output and original query/capture/exclusion
cases. The final production root package passes **4 entries / 2 top-level
cases** using actual authenticated HTTP and precaptured Store bootstrap:
retirement during acquisition refuses the original answer, while retirement
during actual flush waits. Installed API/cmd lint reports **zero issues**.

Earlier failures remain explicit: the initial oversized writer error produced
500 instead of expected 503 (no answer exported); a new root fixture used an
unsupported pipeline stage name; the postflush test extension initially omitted
its cancel-function setup. Each was corrected with original assertions intact.
The combined final-race01 command remains EXIT1 despite its root package PASS;
only the API test fixture changed before the final API PASS. There is no claimed
single all-green aggregate. Original mounted transport/bundle/insertion/output
passes are retained separately. Evidence and exact source comparisons are in
`.context/f28-parent-api-output-20261107/`.

Managed builder input and automatic polling remain disabled. Native/private
cache admission, installed edge bypass, elected updater, full retirement
acceptance and interoperability remain required. F20 and TSIG received no new
execution release; all earlier performance failures and external gates persist.

### F28 native cache mutation admission

The upstream stage retains the original complete root component and finite
query lifetime for native recursion. Independent one-shot attempts precede
priming, delegation/referral DS, address, DNSKEY, DS and private proof acquisition.
Response batches finish validation and nested work before entering common
mode/retirement exclusion, then take native cache locks for actual mutation.
Managed builders require an exact-domain source. The nonwaiting priming flag
avoids lock inversion; absolute TTL origin, flush epochs and original deadline
checks survive queued writes. Legacy standalone static callers remain supported.

Final production default-race evidence passes **228 entries** across recursor,
upstream stage and upstream, with no failures and **two actual skips**:
`TestAggressiveBIND` requires `KUDURRU_RECURSOR_BIND=1`; `TestLiveInternet`
requires `KUDURRU_LIVE_DNS=1`. Neither external acceptance is inferred.
Builder/root composition passes **28 entries / 18 top-level cases**, including
the original mounted stored-TSIG transport/restart case. Root tests use the
actual Store/private source and controlled native exchanger to prove refusal
during acquisition and draining during an admitted write.

An additional actual managed complete publisher, owned resolver and signed UDP
authority test passes **three entries**. Positive validation reuses the DNSKEY
cache; a held second acquisition authenticates and commits local self-revocation
before release, leaves canonical generation unchanged, and cannot enter the
original mutation callback. Its test adapter uses actual captured Store reads;
the private root factory has separate composition coverage. This is not the
complete managed frontend/follower/transport matrix.

Installed scoped lint passes with zero issues. Earlier lint exits remain:
two unchecked test cleanup errors, then a redundant blank channel assignment.
Final cleanup joins a closed completion channel and checks the authority error;
affected tests pass under default race, with production bytes unchanged. A
relative-versus-absolute manifest preparation error and runner refusal before
Go startup are preserved separately. All final 2,348 frozen identities match.
Evidence: `.context/f28-parent-native-cache-20261107/`.

Production managed input and polling remain disabled. Installed edge bypass,
elected updater, full retirement/follower acceptance and interoperability remain
open. Original tombstones/restart requirements, F20 failures, TSIG work and
external release gates are unchanged.

### F28 immutable installed edge-cache bypass prerequisite

The host-only `edge.managedCacheBypass` / `--managed-cache-bypass` option fixes
both pools uncached for every child of a supervisor. YAML and Lua emit a global
continuing skip-cache rule after transfer refusal and PROXY setup, before all
routes. Topology validation rejects attached caches. The sole dynamic worker
cannot attach either pool, regardless of old eligible observations. A bounded
console command checks the running managed marker and synchronously detaches
both pools before acknowledgment; installed readiness binds the exact child
and membership epoch. Unknown completion retires only that child. Backend
managed projections deny both remote eligibility leases, with actual local-only
denial refusing the old publisher projection without canonical progress.

The broad edge/config default-race command passes **423 entries / one opt-in
skip** before the installed-command correction. The skip is the existing
`TestConsoleLiveSupervisor` requiring `KUDURRU_F43_LIVE=1`. Builder composition
passes its selected cases, including actual managed publication/local denial;
the combined command remains EXIT1 for three new CLI layer entries. Correcting
the omitted host-file/environment load assignment makes the CLI's three entries
pass, preserving the original assertions.

Pinned actual dnsdist `sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e`
passes both YAML and Lua under race. Each fixture first warms and hits actual
static resolver and authoritative caches, including a CNAME crossing the suffix
route, then stops and joins that child before starting the bypass supervisor.
Repeated identical DNS requests reach the backend independently; real API pool
observations show both caches absent. Old observations, membership-source epoch
ABA, child crash/restart and actual post-effect lost acknowledgment preserve
bypass. The final live command passes **three entries**, zero failures/skips,
in 16.429s. Final installed scoped lint passes with zero issues. The preceding
focused six-entry managed run and test-only bounded-answer corrections remain
separate receipts.

The initial live command failed both renderer children: dnsdist exposes even
an absent cache as Lua userdata, so comparing `getCache()` to nil was invalid.
The correction uses actual acknowledged detachments; verification reads real
API state and backend wire counts without a corrective console command. Earlier
path diagnostics, the mistyped lint executable's startup failure and
four test-fixture security-lint findings are retained. The integer conversion
now has an explicit bound; private fixture paths, executable wrapper and exact
owned-container cleanup have documented scope. The existing isolated Docker
daemon was restored from retained storage, with the pinned image unchanged and
no pulls, rebuilds or pruning. Evidence:
`.context/f28-parent-edge-bypass-20261107/`.

This qualifies the local edge policy and installed acknowledgment, not the
backend activation protocol. That protocol must retain the original exact
edge/membership authority; `/status` readiness is not a grant. Full membership
replacement, certificate/rollback composition, held managed frontend/follower
retirement and interoperability remain open. Root managed input and automatic
polling remain disabled; no startup-only downgrade or eventual-flush substitute
is adopted. All prior F20, TSIG and external gates remain unchanged.

### F28 bounded updater scheduler prerequisite

The private scheduler now retains one original elected target, borrowed host and
session clock, bounds each acquisition before capture, and rereads the committed
owner after a successful actual receipt. Accepted `NextRefresh` controls polling;
only incomplete transport acquisition gets a new-attempt cooldown, bound to the
exact owner/policy/settings identity. Complete replies are terminal even when
malformed or unauthenticated. Admission, SQL and completion refusals latch a
terminal error without replay; independent denial/update dispositions remain
available. Generation broadcasts avoid stealing the publisher's coalesced local
notice, and bounded cancellation joins the original worker.

Final default-race qualification passes **29 entries / 15 top-level tests**,
zero failures/skips, across the updater, original operation and wire selections.
Tests use real signed socket bootstrap and committed scheduling, incomplete TCP
cooldown, foreign cooldown identity, malformed/authentication/host refusal and
joined cancellation. Installed scoped lint passes with zero issues. The initial
lint failure (unchecked cleanup error and an if-chain style finding) is preserved;
the first race pass preceded the broadcast correction and is recorded separately.
All 3,946 source identities match the final qualification freeze. Evidence:
`.context/f28-parent-updater-20261107/`.

The scheduler is deliberately unmounted. Root still does not enable managed
builder input or automatic polling. Original installed-edge activation admission,
elected lifecycle integration, full managed frontend/follower retirement, recovery
and interoperability remain required. This is no release or full-F28 completion
claim; all F20, TSIG and external gates remain open as previously recorded.


### F20 isolated lazy-budget correctness qualification

Root reviewed and formatted only the two prepared isolated paths, preserving
the exact original source archive, all original workloads and failure receipts.
Reuse retains explicit list-ceiling/nonnegative checks and atomic ancestor
reservation. Badfilter validation allocates its existing proof-disabled budget
only for an actual nonexcluded directive; rebuild rollback remains unchanged.
No parser, module, kernel, proof-work, baseline or capacity changes were made.

Focused ordinary qualification passes **56 entries / 10 top-level tests**,
including the unchanged original 10k workload and aggregate candidate refusal.
Full blocking/listfetcher default race passes **207 entries / 98 top-level
tests** with two actual skips: the BenchmarkMemory helper and external AdGuard
binary test. Scoped installed lint passes with zero issues. All 3,878 frozen
source/evidence identities match. These results cover the isolated tree, not
parent source composition or a new comparator/RSS qualification.

One unchanged original 1M reuse diagnostic reports **514,127,698ns / 66,820B /
29 allocations** with two measured iterations. It does not demonstrate an
allocation reduction against the historical current median 25 allocations.
Current CPU reports 2.90GHz, whereas the earlier aggregate reported 2.50GHz;
GOMAXPROCS remains 2 versus baseline 6. No controlled timing pair, speedup,
fixed-class performance or completed gate is inferred. The committed baseline
62,725,199ns/eight allocations and all 36 measured regressions plus 20 missing
zone keys remain. No rerun, preimage replacement or threshold change occurred.
Evidence: `.context/f20d-parent-small-budget-20261107/result.md`
(SHA256 `21defdc41455d0cf3fb2a0a572c82a5ff58b86c317a2c8b08039b49bd6751ce1`).
The isolated source remains uncommitted and unimported; no further agent command
is released by this checkpoint.


### F07a isolated management A/B/Q qualification

Root executed the exact prepared five-file formatting and one four-name ordinary
selection in the existing isolated activation tree. All **10 entries / four
top-level tests / six children** passed (eight leaves), zero failures/skips or
missing/unexpected names. Package time was 4.592s, process wall 42.004s and
minimum free disk 10,263,027,712B. Raw numeric outcomes preceded enrichment;
all owned children were reaped and the slot yielded before separate final
classification. All formatted45/other40, historical440, tool and path guards
match. No source correction, rerun, extra race/lint, module sync or import occurred.

The held follower request rejects promoted authority without changing durable
credential/image/audit. Create/replace/revoke/reseal retain confirmed encrypted
effects and exact safe audit/receipt bindings without acknowledgment. Original
observer expiry/cancellation/deadline cases pass only their observed private
receiver premises; they do not establish actual-route quiet causality or a
timer census. Evidence: `.context/tsig-management-abq-ordinary-result.md`.

This remains isolated schema6/x-net0.60 evidence. Parent schema8/fixed dependencies
require actual follower capture before the original primary source image read.
Original lint EXIT1/12 findings remains separate; genuine uncertain completion
(M16), whole-route late I/O (M17), remaining C/D/E/F authority axes, parent
generated/CLI composition and Kubernetes remain open. F07a is incomplete.

### F28 original follower socket acquisition prerequisite

The private root follower adapter now captures one actual installed managed
owner, canonical policy and driver denial attempt before opening its own numeric
TCP socket. It retains a separate original follower clock and stamps the first
complete raw arrival through the actual opaque receipt API. It exposes no input
for leader/forwarded bytes, granting update, bootstrap or phase2. Normal responses
retain the existing invalid-denial result and cannot advance canonical state.

Default-race qualification passes **37 entries / 18 top-level cases**, zero
failures/skips, including the unchanged updater/operation/wire selections and
eight new follower entries. The actual schema8 follower is captured before the
original primary image read. Real signed normal/self-revoke socket responses
leave canonical payload, owner/checkpoint and authority unchanged; local denial
invalidates source admission and survives actual Close/Open. No follower writer
is acquired. Primary/unbounded/canceled/promoted/closed originals refuse before
new acquisition. Promotion during a real response first establishes an eligible
successor source; the authenticated old-follower revoke then closes admission
through the driver's original-cut refusal without changing that promoted image.
Each operation refuses a second acquisition. Scoped installed lint passes with
zero issues; all 3,948 frozen identities match. Evidence:
`.context/f28-parent-follower-wire-20261107/`.

The source-currency checks do not claim a complete managed resolver/output
publication. This is an unmounted one-shot adapter, not a follower polling loop;
normal-observation scheduling still needs an explicit authenticated disposition,
not inference from the generic invalid-denial error. Original installed-edge
activation admission, elected/follower mounts, full frontend/cache retirement,
recovery and interoperability remain open. No production activation or full
F28/repository/release qualification is claimed.


### F28 private follower poller and explicit normal disposition

The root follower loop now retains one actual member/term/incarnation and local
clock. Each bounded original acquisition captures before network I/O. Explicit
normal authentication schedules only a local minimum-refresh cooldown, with no
canonical progress. Only incomplete transport uses accepted RetryDue. Exact
owner/policy/settings identity binds both schedules. Complete-input,
authentication, authority/admission and completion errors latch; confirmed local
denial stops the worker. Normal/incomplete acquisition rechecks original
authority, while known denial still reaches the driver without a late granting
check. Caller/runtime cancellation joins original sockets and bounded broadcast
waits; the publisher's single-consumer denial channel remains separate.

Default race passes **128 SQLite entries / 51 top-level** and **50 root entries /
23 top-level**, zero failures/skips. Original tests remain unchanged. New tests
cover explicit normal/revoke classification, zero-TTL known-denial closure,
foreign/substituted stamp refusal and shared legacy consumption, actual socket
cadence/retry/terminal failures, durable denial/reopen, promotion and shutdown.
Focused go vet passes. The recorded installed lint executable disappeared from
the build cache: its existence-check launcher exited 1 before any lint process.
No replacement installed copy was found and no tool was rebuilt; configured lint
remains pending. This is not a lint pass. Evidence:
`.context/f28-parent-follower-poller-20261108/result.md`.

The poller remains unmounted, as does the elected updater. Root does not enable
managed AnchorStore. Backend-to-edge activation authority, joined elected and
follower mounts, complete frontend/cache retirement, recovery/interoperability,
all F20 failures, TSIG gaps and external acceptance remain open.


### F20 parent modifier-aware fingerprint prerequisite

Parent reuse now shares the actual parser's input/sniff/autodetect/CRC path while
classifying only literal-dollar candidates. Both modifier attachment paths
require that byte, and every candidate still reaches the original parser and
compiler callback. Full build parsing, parent regex delimiter fixes, fingerprint
version/options, source opens/closes, exclusions and badfilter keys are unchanged.

Full parent parser/blocking/listfetcher default race passes **339 entries / 117
top-level tests**, zero failures, with the two existing memory-helper/external
AdGuard skips. Focused vet passes; configured lint remains pending because the
installed executable disappeared. One unchanged original 1M diagnostic reports
**67,043,086ns / 66,240B / 17 allocations**, three iterations, 200ms, current
2.90GHz/GOMAXPROCS2. It verifies exact parent owner reuse. This is neither a
controlled before/after pair nor the isolated lazy-DFA implementation, a fresh
five-process result, comparator/RSS evidence or official fixed-class gate.

The isolated F20 tree was not changed or imported. Its budget integration,
all 36 measured regressions plus 20 missing zone keys and every historical
failure remain open. The wrong-doc-path archive preparation failure was retained;
the two edited originals were recovered from immutable parent commit and matched
the preceding source freeze. Source/tests/diagnostic evidence and limits are in
`.context/f20-parent-fingerprint-20261108/result.md`. No benchmark body, baseline,
threshold, module, tool or parser fixture was replaced or rebuilt.


### F20 fingerprint/lazy-DFA overlay compatibility

One read-only six-path Go overlay combined the committed parent fingerprint and
regex-delimiter fixes/tests with the isolated lazy-DFA compiler. Its only list.go
changes select Fingerprint and its CRC result; all existing badfilter budget,
rewind/refusal, list/candidate and ancestor reservation logic stays intact.

Full parser/matcher/blocking/listfetcher default race passes **616 entries / 180
top-level tests**, zero failures, two actual memory-helper/external AdGuard skips.
Wall39.070026479s, minimum free19,414,880,256B; all7,725 frozen identities match.
All3,757 original isolated files remain unchanged, including existing budget,
10k/helper tests and benchmark inputs. Exact overlay, raw completion, inventory
and report: `.context/f20-fingerprint-overlay-20261108/result.md`
(SHA256 `7e6da62ff52fd584936b6a3c1824f0ee68fb1c578cc60ff6bfddd8627a119048`).

This establishes tested overlay compatibility, not source import or new lazy-DFA
performance/RSS/comparator qualification. No benchmark, preimage or baseline was
rerun. Historical36 measured regressions/20 missing zone keys, CPU differences,
configured lint/static gates and final integration remain open.


### F20 root lazy-DFA integration checkpoint

The root now integrates the qualified23 lazy-DFA source paths. Twenty-two are
byte-identical to the isolated handoff; list.go has only the already qualified
parent Fingerprint call/checksum composition. Parent parser delimiter/CIDR
fixes, existing tests and module files remain intact; the isolated tree is
unchanged. This supersedes earlier current-status statements that the engine
was unimported, while retaining their historical evidence.

On this exact source, core blocking/listfetcher/servingbuild default-race checks
pass835 entries (288 top-level), with only the actual memory-helper and unset
AdGuard skips. Mounted API/command checks pass47 entries with no skips. The
pinned v0.107.79 AdGuard comparator then passes all46 entries, zero skips, with
its two documented deviations unchanged. Scoped vet exits0. The configured
lint executable is unavailable; vet is not a lint substitute.

One unchanged original12 200ms diagnostic records all58 metrics at2.90GHz,
GOMAXPROCS2. Unchanged1M is67,115,699ns,66,202B and19 allocations (3 iterations),
so the eight-allocation baseline remains unmet. Policy Evaluate hit/miss is
867.5/1123ns; regex1000p99 is18,276ns and remains adverse. This is not a controlled
before/after pair, five-process median or fixed-class qualification. All36
historical measured regressions,20 missing zone keys, earlier6B allocation,
StageA unknown child status and fuzz throughput limits remain recorded.

Raw outcomes were persisted before enrichment. All3965 frozen root identities
matched, with the pinned binary also verified for comparator execution. Source
archives, original58 comparisons and exact commands are retained under
`.context/f20-root-integration-20261108/`. No benchmark, baseline, threshold,
module or protected dashboard file changed; no push or release occurred.


### F20 sparse unindexed batch dispatch

The integrated sparse route now executes its one existing charged unindexed
batch when every member survives original literal filtering. Results merge at
original IDs, including indexed rules interleaved before a later unindexed hit.
Partial admission and unsafe input retain the prior execution path. Programs,
16-candidate escalation,128 buffer, memory/work limits and Visit are unchanged.

Focused default-race passes88 entries; complete matcher/blocking/listfetcher
races pass452 with only the existing memory-helper and unset comparator skips.
Vet exits0. The new tests require actual batch execution, real tiny-cache
saturation, independent stdlib matches, unchanged reservations and zero query
allocations. One original12 diagnostic yields all58 keys: policy Evaluate
hit/miss824/740.5ns; regex100 miss495.9ns; regex1000p99 remains17,691ns.
Unchanged1M remains68,102,134ns/18allocations. These diagnostic samples do not
clear the historical36 regressions/20 missing keys or establish fixed-class
five-process performance. Configured lint remains pending its unavailable tool.
All3966 frozen identities match. Commands, raw outcomes and every metric with
prior deltas/ratios: `.context/f20-root-sparse-batch-20261108/result.md`.


### F20 unchanged fingerprint allocation correction

The v3 fingerprint now encodes the same field bytes with bounded typed scratch.
Fingerprint-only parsing omits unreturned diagnostic storage; full Parse
continues to expose its original title, maps, counters and samples. Shared
validation state and a lazy exclusion map preserve every real badfilter callback,
temporary resource refusal and cross-source rewind. No pool or additional
retained serving capacity is introduced. Independent compatibility checks use
the original fmt stream and full Parse CRC, including empty/long/NUL/Unicode
names, all formats, cap/byte options, modes and exclusions.

The first allocation guard failed at10 versus8, with100 other passes; source,
output and compiler escape diagnostics are preserved. The corrected focused
selection passes101; complete parser/matcher/blocking/listfetcher default-race
passes641 with2 actual helper/unset comparator skips. Pinned AdGuard then
passes46/zero skips, retaining both documented deviations. Scoped vet exits0.

One original12 diagnostic has all58 keys: unchanged1M79,681,673ns/65,984B/
8allocations (three iterations). This meets the historical allocation count
in one process, not the complete performance gate. Policy Evaluate hit/miss
1095/965.6ns and regex1000p9918,906ns remain recorded, including worsening
relative observations. The original36 regressions/20 missing zone keys and
all prior limits remain open; no workload, baseline or threshold changed.
Configured lint is pending its unavailable executable. All3966 root identities
and the comparator binary match their freezes. Raw outcomes, both source
versions, all58 comparisons and report:
`.context/f20-root-fingerprint-allocations-20261108/result.md`.


### F20 anchored fallback qualification

A read-only overlay on the unchanged actual benchmark fixture counted unindexed
NFA steps on2475 of65536 warmed regex1000 misses; indexed fallback count was0.
The bounded inventory is not CPU attribution, and its truncated per-query log
is retained. The existing all-program start-of-text proof now suppresses
impossible later program starts while advancing every current PC. Mixed and
unanchored programs keep prior behavior; capacity/reservations/freeze/work and
final cancellation checks are unchanged.

Focused default-race98 and full core467 pass (2 existing helper/unset comparator
skips); final pinned comparator46 and vet pass. Oracle fuzz20s completes
127/127 baseline inputs and157288 coordinator executions,128 new interesting,
without a mismatch. Internal skipped invalid/refused inputs are not counted
separately; the old throughput plateau remains recorded.

Original69/all291 diagnostic metrics report0B/0allocs in every current case.
Original20%/noise-floor arithmetic still flags dense500 early warm768.4/421.3ns,
dense1000 early warm482.9/310.7ns and long256 cold66393/54491ns. A private
comparison's reversed MB/s direction was corrected to the retained runner;
superseded output remains. Original12/all58 records unchanged1M70183299ns/
65984B/8allocs, but regex1000p9918550ns remains adverse. No tail-repair or
five-process/fixed-class completion claim. All historical36 measured failures,
20 missing zone keys and older limitations remain. Configured lint is pending.
All3967 frozen root paths match; exact receipts/commands/all comparisons are in
`.context/f20-root-anchored-start-20261108/result.md`.


### F65/F67 original peer transport prerequisite

Peer image acquisition now establishes a finite scope before identity sampling
and resolution, copies one original local member/token vector and retains that
scope through body validation. Expired resolution cannot dial; cancellation at
complete image EOF cannot return a success. Command POST has one credential
attempt and no transport GetBody replay; command.NotAfter can only shorten the
original timeout. Unknown completion stays unknown. Captured GET credential
rotation and higher-term observation remain supported.

Five new declarations include real TLS/original-token behavior and actual SQL
domain non-entry after a refused POST. The first configured-timeout assertion
incorrectly compared deadlines created at different instants; failed source and
receipt remain. Corrected focused race7 and final full replica/follower race59
pass without skips/failures; vet exits0. All3968 frozen root identities match.
Exact commands, limits and outcomes: `.context/ha-original-transport-20261108/`.
Configured lint remains pending the missing executable. Static startup,
individual enrollment proof, CLI/API mounting, promotion and actual partition/
rejoin still require implementation. This does not activate HA or managed roots.


### F65 static SQLite twins and F67 mounted acceptance (D300)

Host-only restart-required `cluster.role: static` and `cluster.peers` now mount
independent SQLite leaders with bounded unique names/endpoints and exact self
membership. Enrolled databases are refused without changing their authority.
Shared TLS/token status and digest reads remain independent of individual member
enrollment. Status must name the configured peer; unreachable peers remain listed
without readiness. The common post-authorization API guard refuses object,
cluster-setting, IAM credential and provider mutations before body decoding or
forwarding, including previously API-owned objects. Static mode also disables
public unblock-request creation and refuses the external-dns mutation webhook.
File loaders, login/logout and local blocking/cache controls remain available.
UI capabilities, schema metadata, Compose examples and architecture are updated.

Mounted acceptance exposed a real postcommit acknowledgment defect: the retained
RuntimeOwner predicate included the old generation cursor, so the command's own
commit invalidated it. A separate retained dependency predicate now preserves
host/provider/catalog/key/managed-anchor checks during exact-receipt acknowledgment.
Original HTTP command admission, writer binding and every other old-image currency
check remain. No mutation replay or deadline extension was introduced.

Final two-runServe race acceptance passes2 top-level tests, including independent
DB identity/local leadership, actual DNS/TLS digest reads, read-only mutations,
single-dispatch node-local blocking, peer loss and restart. Focused blocking43
and builder2 entries pass; the same earlier command's startup failure remains
recorded. Seven unchanged affected packages pass1055 entries with4 actual skips;
config initially failed its generic enum fixture (static without required peers),
then the corrected full config suite passes340 entries. The skipped tests require
PostgreSQL/Kubernetes and receive no qualification credit. Vet exits0; dashboard
capability/account40 and settings23 tests, TypeScript and scoped ESLint pass.
All3 Compose configurations validate. Generated reference drift passes; generation
also corrected existing committed anchor-mode/root-hint reference drift, with the
initial unexpected-output wrapper failure preserved. Earlier mounted/login/body
and UI-fixture failures are retained under `.context/ha-static-20261108/`.

Every qualification used frozen root source and raw numeric persistence before
enrichment. Protected `dashboard/dist/index.html` remains untouched. Configured
Go lint is pending its missing executable and unanswered rebuild approval.
This acceptance runs two actual assemblies within one test process; separate
server-process static acceptance is next. F60b enrollment/promotion, forwarding,
partition/rejoin and final release gates are not closed by static peer routing.


### F65/F67 separate-process static acceptance, November 8

`TestStaticClusterProcessesReadOnlyPeerLossRestart` now starts two actual
server processes using the existing synthetic ten-node license fixture, with
separate SQLite files and original configured peer identities. UDP/TCP DNS,
authenticated status/digest reads, local leaders, distinct database identities,
read-only object/settings refusals and one node-local blocking command pass.
After A exits cleanly, B continues DNS and lists A without claimed readiness;
A restarts from its original database/config with identity and session intact.
Mutations are single attempts without replay or deadline extension.

The frozen-source race-enabled E2E harness passes1 top-level test, zero failures
or skips, child0/wrapper0, wall56.499293604s, minimum17,192,493,056B. Actual server
binaries use the existing CGO-disabled fixture build, without race instrumentation.
E2E package vet exits0. All frozen source identities match; exact receipts are in
`.context/ha-static-process-20261108/`. Existing TestTwoNodes and audit assertions
remain; only their shared licensed fixture name is generalized. Configured lint,
enrolled replication/promotion, partition/rejoin and final release gates remain
open. This evidence does not grant individual member authority from shared
internal credentials.


### F60b cluster status command, November 8

`kudurru cluster status` reads the existing authenticated `/cluster` route once
and renders the server's driver, leader, member count and read-only mode. Wide
output adds cluster ID/epoch; JSON/YAML preserve the original response fields.
Static twins report local leadership; status grants no quorum or promotion
eligibility. Invalid arguments/format/transport and canceled input do not dispatch.

Actual-router race9 entries pass with no skips/failures, child0/wrapper0,
wall23.441864966s, minimum17,223,516,160B. Generated CLI reference and command
package vet pass. The first race run's success fixtures used viewer without
cluster.read and were correctly refused (6 pass/3 failed entries); those results
remain. Success fixtures now use the existing administrator identity; permissions
and refusal assertions are unchanged. Exact frozen-source receipts are under
`.context/cluster-status-20261108/`. Configured lint remains unavailable.
Enrollment, join-token, promotion, forwarding and final HA acceptance remain open.


### F19b shared Kubernetes blocking runtime (D301), November 8

The actual Kubernetes server assembly now mounts the existing runtime annotation
contract on the release-namespace `kudurru-runtime` ConfigMap. Each command keeps
its original five-second-or-earlier context and rechecks authorization before
reading and before one UID/resourceVersion update or absent-state create.
Client retries are disabled. No-op commands still use the captured CAS; conflicts
and lost replies never acquire a new source or repeat the mutation. Confirmed
commits apply locally; unknown completion is explicit and cannot earn local
success credit. Independent later reads may observe the actual durable result.

All replicas use serialized bounded one-second read reconciliation and the same
absolute deadline. Repeated observations, including unrelated resourceVersion
changes, cannot rearm a locally consumed timer. Startup is enabled until first
observation; outages retain confirmed DNS state with its original expiry and
refuse unavailable runtime API reads. Changes retain the existing cache observer.
The API records the action once through the existing audit seam. Existing RBAC
already covers the runtime ConfigMap; no schema or privilege expansion.

Final production has driver6, API4, toggle1 and root failure-path2 actual race
passes. The same combined command retains its startup fixture failure. Final
actual two-runServe acceptance separately passes1 with real API-server state,
DNS on both replicas, one disable, restart without expiry renewal, refused
outage mutation, actual expiry and one enable from the other replica. Final
mounted child0/wrapper0, wall61.709555064s, minimum16,656,818,176B; affected
package vet exits0 in11.622499991s. No skips or race diagnostics in those passes.

Earlier mounted failures remain: implicit policy response, upstream edits absent
from the real settings source, and fast list startup preceding the initial DNS
assertion. Fixtures now select explicit policy/source and blocking list startup;
no mutation replay or original deadline extension was added. One earlier mounted
source also passed before the final unchanged-deadline observation guard.
The harmless controller-runtime missing-logger diagnostic remains in the log.
Exact source freezes, raw outcomes and failed sources are retained under
`.context/kube-blocking-20261108/`. Protected dashboard bytes remain unchanged.

This is eventual shared state, not synchronous acknowledgment by every node,
cluster-wide transactional IAM admission or same-transaction Kubernetes audit.
Configured lint remains unavailable and final driver/release gates remain open.


### F35a direct DNSEndpoint source (D302/D303), November 8

The optional host-only `externalDNS.crd` source now runs inside the original
elected controller term on Kubernetes. It lists/watches source CRs read-only
and derives same-namespace DNSRecordSets with exact source UID ownership,
deterministic names, native validation and each zone's recordSetSelector.
The longest selected primary-zone suffix wins. Invalid current source data
withdraws only its children; foreign owners remain intact and one conflict
does not starve unrelated sources. Original finite term contexts, source/zone
UID and resourceVersion checks, atomic creation, expected-version updates and
original UID/version deletes bound each operation. Lost replies and refusals
are not retried within that operation. Later reconciliation observes anew.

Actual source updates exposed Kubernetes' distinction between POST Update
ownership and SSA Apply ownership for the same manager. The store now permits
that transition only for exclusively self-owned fields under the exact original
resourceVersion. Foreign non-status field owners and intervening edits refuse.
Optional original UID/version deletion preconditions now cover memory, SQLite
and Kubernetes public/private objects; Force cannot bypass them. A stale SQLite
CommandTx delete poisons the entire command. Unguarded semantics remain intact.

Final production has 23 focused race passes with no failures/skips, including
real source permission/watch/update/refusal, cancellation, lost reply, foreign
ownership, replacement and actual runServe DNS/restart/deletion. Store conformance
and same-owner migration previously passed 56 entries on the unchanged final
store implementation. Legacy kube ownership/finalizer/rejection, external-dns
webhook/relay and generated-reference regressions pass 136 race entries with
no failures/skips. Vet passes. Helm renders the source through config.node,
keeps store.driver=kube and grants source get/list/watch only. No RBAC expansion.

Exact receipts and failed source archives are under
`.context/externaldns-crd-20261108/`. Final focused race child0/wrapper0,
wall71.913893334s, minimum15,052,296,192B; legacy regression child0/wrapper0,
wall27.051235979s, minimum15,104,389,120B; vet child0 in30.250234413s.
Earlier import collision, CRD fixture annotation failures (including one
accidental unchanged repeat), and actual same-manager SSA conflicts remain
preserved. Failed envtest children were identified and reaped. Helm succeeded;
the first optional Python YAML verification lacked its module, then exact
rendered blocks were verified without installing dependencies.

Source, zone and child remain separate Kubernetes API objects: this is an
eventual controller, not an atomic cross-object transaction or proof of
partition-safe distributed fencing. The minimal specification-only test CRD is
not an upstream CRD replacement. Disabling the adapter leaves existing data;
normal Kubernetes owner-reference garbage collection follows source deletion.
One mounted server restart is not multi-node lease-loss/partition acceptance.
Those cases, configured lint (unavailable executable), and final G09/G21/G22
and release gates remain open. Protected dashboard bytes are unchanged.


### F46 shared Kubernetes password admission (D304), November 8

The actual Kubernetes server now mounts one shared lockout owner for local
password login. One private release-namespace Secret atomically reserves both
normalized IP-prefix and username capacity before hashing, retaining the driver's
cluster-identity transport, original finite context and UID/resourceVersion CAS.
Five IP failures within five minutes lock for five minutes; ten username failures
within fifteen minutes lock for fifteen minutes. IPv6 uses the existing /64 key.
A successful password removes its own IP reservation and resets only older
completed username failures; other pending work and newer failures survive.
Durable sequence numbers distinguish even identical wall-time samples.

Unfinished reservations become failures at their original expiry. The document
has a 512KiB/4096-key maximum and only reclaims expired entries. HMAC keys under
its persisted random salt omit raw usernames/addresses; Secret access remains
sensitive. Clock rollback refuses rather than resetting counters. Every operation
retains an original bound of at most30s; unknown writes, conflicts, malformed state
and outages refuse without retries or local fallback. Committed-but-canceled
operations retain their effect and cannot return admission. Privileged Secret
removal resets counters. Existing Secret RBAC suffices; no new config or CRD.

Actual two-runServe acceptance proves a successful password, five failed logins
alternating replicas, both replicas refusing a correct password while locked,
restart persistence, unavailable shared reads returning503/no write or cookie,
and recovery preserving the lock. Real kube tests additionally verify original
CAS interposition, exactly one committed lost-reply reservation, no replay,
canceled input and a preexisting foreign Secret left unchanged. Pure tests cover
simultaneous reservations, later-failure preservation, successful username reset,
idle expiry, malformed/capacity refusal and original context/ticket consumption.

Final production has7 targeted race entries with zero failures/skips,
child0/wrapper0, wall35.457727048s, minimum14,743,908,352B. Earlier expanded pure9
and actual kube1 passes remain; full unchanged users/stack regression passes75
entries, zero failures/skips, wall14.226176477s. Final affected-package vet exits0
in4.811018429s, minimum15,063,453,696B. Exact freezes, logs and receipts are in
`.context/login-lockouts-20261108/`.

Earlier failures remain: the first mounted correct-password login returned503
because JSON omitted monotonic metadata carried by the original context deadline;
reservation expiry now normalizes to UTC before durable identity comparison,
without extending the deadline. That run also had a test variable-name compile
error. A later negative fixture tried to change an immutable Kubernetes Secret
type and was rejected; it now creates a separate preexisting foreign Secret.
No failed run is relabeled green. Protected dashboard bytes remain untouched.

This is the Kubernetes password mount only. Complete SQLite login/factor counters
still need same-command persistence and replication composition; OIDC initiation
limits are distinct. Shared counters do not grant credentials, TOTP replay
protection or cross-object IAM transactions. Configured lint remains unavailable;
F46's remaining driver/route work and final G12/G22/release gates stay open.


### F46 SQLite original-command password admission (D305), November 8

The mounted login now stages bounded shared IP-prefix/username reservations,
one password/TOTP verification, result and successful enrollment inside its
original currentAuthCommand. Only an exact package-issued, command-bound
pre-mutation credential refusal can commit failed counters. Final errors, clock
rollback, expiration and poisoned mutations roll back all effects. The earliest
original deadline/30s bound is retained; no second verification or new lease.

An optional versioned runtime row carries the complete 512KiB/4096-key document.
Prefix bounds precede allocation; unknown versions/malformed state refuse without
repair. Replica validation uses stored observation time. Legacy absence initializes
on first admission; privileged removal resets the operational limit. Safe object
exports omit it. Original policy/HMAC keys/concurrency semantics are unchanged.

Focused race-02 passes11 entries with no failures/skips (30.250527820s). Full
security/servingauth/runtimeowners/loginlimit default-race passes324 entries with
no failures/skips (77.315912087s, minimum14,762,717,184B). Affected vet exits0
(6.013013003s), and exact11-file gofmt check has empty output. Configured lint
is unavailable and vet is not its substitute.

Actual HTTP adapters alternate failures against one canonical Store, reject
locked correct passwords after adapter recreation, and carry exact counter bytes
through a separate physical follower's pre-captured install, Close/Open and
read-only authority refusal. Success, final guard failure, clock rollback and
ignored mutation poison check exact effects. This is not production forwarding.

The full55-top-level HTTP selection completed with58 pass entries and32 failed
entries, zero skips, numeric1 (268.668862522s, minimum14,658,461,696B). Failures
are browser-start503/expected302, including fixtures used by external-account
and RoleBinding tests. Source review identifies original-source currency after
an own pending/session commit as a candidate cause; investigation is ongoing.
No full HTTP/release green claim. All receipts and exact freezes are retained
under `.context/sqlite-login-lockouts-20261108/`.

Earlier race-01 failed because a success fixture used the password-change floor
itself; the corrected test uses a later time and asserts actual target entry.
Earlier final-error/clock apparent passes did not reach their intended target
and remain invalid-target evidence. No production acceptance check was weakened.
Self-service password/factor routes, OIDC admission, enrolled-replica forwarding,
configured lint and final F46/G12/G22/release gates remain open.


### Browser response after its own commit (D306), November 8

Full HTTP regression for D305 exposed 32 failed entries at browser-start 503,
with 58 passing entries. A new diagnostic proved that BeginBrowser had actually
committed its pending row, retiring the original source revision while all
captured dependencies remained current. Its postcommit Browser.Current check
therefore rejected its own successful command. The diagnostic's numeric1 and
all earlier failures remain preserved, rather than being treated as unrelated.

The private response helper now retains the exact original RuntimeOwner,
request lifetime and writer episode. It invokes unchanged original-source,
provider, HTTP and transaction gates. Only actual successful command completion
and final original-writer checks return a response-current predicate. That
predicate excludes the retired source revision but still rejects cancellation,
writer loss, term/incarnation changes and captured dependency replacement.
It grants no authentication, provider exchange or mutation. Start/callback still
observe their exact pending row/enrollment in a complete publication and check
original cookie policy and expiry before sealing. Final emission also checks the exact acknowledgment publication, so later revocation refuses without resealing. Local logout must commit
before the optional provider continuation; no provider is recaptured or retried.

Source-03 complete default-race HTTP selection passes 108 entries across 58 top-level tests,
zero failures/skips, child 0/wrapper 0, wall 371.588192269s,
minimum 14,489,436,160B. Includes actual TLS login/logout, frozen grants,
external administration, provider replacement during exchange, cookie-policy
replacement, invalid proofs, concurrency, ordinary password/factor/session
routes and the new exact completion targets. New negative targets stage a real
enrollment before callback/final/poison refusal and verify complete rollback;
original cancellation, writer stop, equal-key replacement and request release
invalidate a successful completion. Focused prior source-02 race 10 also passes.
Final source-04 adds a memory-only check of the exact publication used to seal
the response. A later canonical revocation invalidates that acknowledgment
even while the original dependencies remain installed; it does not reseal or
replay. All affected browser/external-account/frozen-grant/completion races
pass 52 entries across 23 top-level tests, zero failures/skips,
wall 146.833130272s, minimum 14,440,554,496B.
Final root vet exits 0, wall 2.005811144s. Receipts/freezes/original sources
are retained under `.context/oidc-own-commit-20261108/`. Configured lint remains
unavailable; full release and remaining F46 routes/forwarding gates stay open.


### F46 shared SQLite factor proof failures (D307), November 8

Authenticated TOTP setup, confirmation and disable now use the same bounded
shared IP-prefix/username counters as local login. Cookie, exact current account,
CSRF and existing local factor pacing remain admission prerequisites. Counters,
proof verification and factor effects share the original finite command; the
private security callback never receives a caller-claimed verification result.

Only command-bound password refusals or challenge/candidate-code refusals issued
before any authentication mutation can commit failures. Later conflicts,
enrollment/factor errors, original context expiry, clock rollback and poisoned
commands roll back counters and authentication state together. Valid proofs are
not verified a second time at final commit. The original five-IP/ten-username
policy and 512KiB/4096-key bounds remain unchanged. The one-second factor throttle
remains an additional local control, not a newly claimed distributed pace.

Real HTTP tests alternate failed setup/disable requests across separate adapters,
recreate one adapter and verify that setup, confirmation and login all refuse the
shared lock. Every failure changes only the versioned counter row; every other
payload field and object revision remain exact. Real challenge/code tests combine
setup, malformed challenge, disable and incorrect numeric code failures, preserve
factor refusal ownership across restart, and confirm exactly three password checks
across five attempts; challenge and candidate-code refusals occur before hashing.
Locked confirmation does no hash.
Valid activation and final-error/clock/poison cases assert actual target entry,
exact rollback or one committed factor/counter effect.

Focused default-race passes 24 entries, no failures/skips, wall
57.896847543s, minimum 14,382,419,968B. Full security/loginlimit
regression passes 110 entries, no test failures/skips, wall
81.331390614s, minimum 14,547,116,032B. The sqliteadapter package
has no direct test files; its actual transaction behavior is exercised by the
security and mounted HTTP tests. Affected vet exits 0 in
2.807594720s. Exact source freezes, original five-file archive and
receipts are in `.context/sqlite-factor-lockouts-20261108/`.

Three existing HTTP assertions now distinguish the intended operational counter
commit from unchanged authentication state, comparing the entire remaining
image and object revision; original declarations/messages and negative bodies
remain. Independent failure requests wait for publication of the actual previous
source before capturing their next request; no refused mutation is replayed.
Pure factor APIs retain their original no-op behavior and regression tests.
Self-service password changes, OIDC budgets, enrolled-node forwarding, configured
lint and final F46/G12/G22/release gates remain open. Kubernetes factor authority
is not implied by this complete-SQLite mount. No push or release.


### F46 SQLite self-service password admission (D308), November 8

Self-service password changes now share login and TOTP-management failure
counters. The original cookie/API-token and exact current account are verified
first; the current password and factor are admitted and verified once before any
password mutation. Invalid proofs commit only counters. All later failures abort
the entire command, including its counter result, password, factor consumption,
revocation floors, token deletions and replacement enrollment.

A private same-command enrollment helper uses the actual password decision just
verified before this operation's own password change. No caller can construct a
claimed decision to issue a session; token callers receive no cookie enrollment.
Exact local ownership, accepted token-provider aliases, must-change behavior and
original revocation-floor time remain. New-password hashing happens outside locks
before shared admission; only current-password/factor verification is admitted.
The opaque preparation is one-use, redacted and bound to the original request's
30-second-or-earlier deadline, retained through publication and response sealing.

Real HTTP adapters alternate invalid current passwords and invalid numeric TOTP
proofs. Each failure changes only the intended counter row, leaving the complete
remaining image exact. Recreating the adapter cannot bypass the lock. Actual TOTP
password change consumes its proof once. Private command tests reach staged
password/enrollment before callback/final/clock/poison/cancellation refusals and
assert exact rollback. Success revokes the old cookie and seals the replacement;
consumed preparation cannot be reused.

Final full security race: 105 test passes, no failures/skips, wall
103.992015973s, minimum 14,422,274,048B. Final focused HTTP race:
18 passes, no failures/skips, wall 107.030013562s, minimum
14,223,118,336B. Affected vet exits 0 in 4.011298767s;
formatting is clean. Earlier source01 focused race passed 19 entries. An overly
broad combined test selector was interrupted (child -15/wrapper 241); that raw
attempt is preserved and not credited as qualification. Source02 was then tested
with separate full-security and focused-HTTP commands.

Evidence, source freezes, archives and raw receipts are retained in
`.context/sqlite-password-change-lockouts-20261108/`. The issue.go preimage was
read from its exact committed blob immediately after that helper edit, as recorded
in the archive note; it is not claimed as a prior working-byte archive.
Configured lint remains unavailable. OIDC budgets, enrolled-replica forwarding,
Kubernetes factor semantics and final F46/G12/G22/release gates remain open.
No push or release.


### F46 shared SQLite browser request budgets (D309), November 8

Browser starts, callbacks and CLI preparations now charge one bounded replicated
request-budget document before the first complete HTTP lease capture and before
credential interpretation. This non-granting command modifies only operational
counters. It does not authorize a provider, interpret a cookie, acquire an
identity, or turn the D306 response predicate into a continuation. Only confirmed
charge completion reaches the existing adapter's initial authentication capture.

The original 30-second-or-earlier request context and exact elected writer remain
through dispatch, provider I/O and response. Nested writer binding cannot adopt a
successor, even with the same term/incarnation; provider handles narrow their
original source currency with the retained writer check. Final command/context/
clock/poison failures roll back charges. A charge that committed before later
cancellation remains charged and cannot dispatch or be refunded/retried.

Policies retain thresholds of five starts, ten callbacks and five CLI preparations
per normalized IP prefix. Each lane resets after a one-minute idle gap; reaching
its threshold starts a separate one-minute lock. One optional
`runtime/browser-admission/v1/state` document retains salted hashes, fixed-version
policy and source time, bounded by 512KiB/4096 live keys across lanes. Unknown,
empty, malformed or oversized state refuses; live history is never evicted.
Legacy absence initializes, and privileged removal explicitly resets this
operational history. Complete follower install/restart retains exact bytes;
followers cannot charge locally. Safe object exports omit this runtime state.

The existing local per-attempt latch and concurrency caps remain additional
controls; no distributed exchange-uniqueness claim is made. A second admitted
callback's counter commit can retire the first callback's original source during
provider I/O. That first request returns 503 without a second exchange or session.
The concurrent-callback test now proves the intervening commit changed only the
budget, retains the one-exchange assertion and requires zero sessions. Its original
302 expectation and source01 failure (27 pass/two failed entries) are archived.
Independent actual TLS single-callback success remains positive.

Full requestlimit/runtime-owner default-race: 45 passes, no failures/skips,
wall 6.613112600s, minimum 14,272,958,464B. The broad source02
browser/CLI/logout/writer run retained 98 passes, one failure and one skip, wall
319.369973289s. The failing CLI final-clock fixture lacked the original
primary runtime in its private clock-only clone; the fixture now carries that
same actual primary. Its original final-clock refusal assertion is unchanged.
The skipped installed-Chrome case was subsequently executed explicitly.

Final source03 focused default-race passes 32 entries, no failures/skips,
wall 71.328128825s, minimum 14,047,240,192B. It includes the actual
installed Chrome/Playwright TLS browser, corrected final-clock refusal, real
browser success/concurrent source refusal, shared adapters/follower restart,
missing-writer refusal, original cancellation/equal-fence replacement and
transaction rollback/capacity/privacy targets. Affected vet exits 0 in
2.206111614s. Formatting is clean; configured golangci-lint remains
unavailable. All raw outcomes, frozen sources, original fixtures and browser
input hashes are retained under `.context/sqlite-browser-admission-20261108/`.

This is the complete SQLite HTTP mount. Kubernetes browser admission, enrolled-
replica forwarding and final F46/G12/G22/release gates remain open. Earlier F20
performance failures and unrelated runtime/external acceptance gates are unchanged.
No push or release.


### F46 Kubernetes browser request admission (D310), November 8

The actual Kubernetes server charges starts and callbacks before selecting an
OIDC provider, including unknown-provider requests. It uses the same D309
five-start/ten-callback prefix policies, one-minute lock, salted address keys and
512KiB/4096-key bounds. The separate release-namespace Secret
`kudurru-browser-admission` must have the exact private type, UID, resource version,
shape and no owner references or deletion timestamp. Confirmed absence may create;
privileged deletion resets this operational history.

One original request context, bounded by thirty seconds or an earlier deadline,
spans the direct read, exactly one versioned create/update and provider dispatch.
There is no write retry, cache or local-counter fallback. Conflicts, malformed
state, API outages and uncertain write replies refuse dispatch. A committed charge
survives later cancellation, clock failure or a lost response without refund.
This adds request admission, not a cross-object authentication transaction or a
shared TOTP replay authority. Enrolled SQLite forwarding remains separate.

Default-race evidence: backend/document and actual API-server tests pass 22
entries; two actual runServe replica/restart/outage tests pass; the full Stack
suite passes 46 entries. All have zero failures/skips and exact frozen input
matches. Wall times are 18.480313138s, 49.101403396s and 21.055286751s;
minimum free bytes are 14,020,022,272, 13,847,126,016 and 14,116,102,144.
Actual Kubernetes tests exercise alternating clients, restart, original-version
conflicts, retained lost-reply writes, canceled requests and foreign Secrets.
The mounted route checks both lanes, unknown providers, no cookies, Retry-After
and admission-store outages. Affected vet exits 0 in 4.010559710s. Existing
formatting receipt is clean; configured golangci-lint remains unavailable.
Receipts and input identities: `.context/kube-browser-admission-20261108/`.

F46 enrolled-node forwarding and final driver/release qualification remain open.
The historical F20 performance failures and other external acceptance gates are
unchanged. No push, deployment or release.


### F25b CID-routed QUIC socket groups (D311), November 8

Linux DoQ and HTTP3 now honor `listeners.sockets` with immutable 1–256-socket
reuse-port groups and a destination-CID BPF selector. Each server CID has one
shard byte and 120 random bits. Shared token protection supports Retry across
sockets; application connection/worker caps remain shared. Handshake limits are
per library transport. An exclusive first bind is retained through reuse-port
setup; bind/filter failure refuses startup and any accept failure closes the
whole group. Other platforms retain one socket.

Actual IPv4/IPv6 kernel routing, every shard's TLS/QUIC v1/v2 with and without
Retry, and real DoQ/HTTP3 exchanges across three successive migrations are
covered, with single-socket controls. Partial binds, simultaneous startups,
limits and cleanup are covered. Upstream quic-go v0.63.0 failed the third
migration even on one socket. Its path manager discarded the active probing
CID's ownership while clearing candidate paths. The pinned MIT module retains
all 500 original files and hashes; only `path_manager.go` changes, retaining the
active probing ID and retiring it on the next switch. A new twelve-switch test
uses two probing CID slots; original upstream assertions remain unchanged.
Docker dependency resolution includes the local replacement. No module-cache
edit or upstream acceptance is claimed.

Final frozen-source default-race results: upstream path/CID tests 39 passes,
zero skips (11.421829848s); complete dnsserver/config 706 passes, four existing
spike/measurement skips (56.692364702s); actual runServe signed-transport and
SQLite encryption/restart test one pass (50.283144008s). No failures or race
diagnostics. Minimum free bytes respectively 13,898,244,096, 13,652,758,528 and
13,433,724,928. Affected vet exits 0 (19.833713570s). Configuration reference
generation exits 0; docs drift tests pass four entries. Mock help matches the
schema exactly. Configured golangci-lint remains unavailable.

One fresh local 1s/case benchmark on two 2.90GHz cores records DoQ one/four
sockets at 55,708/48,841ns (12.3268% lower) and HTTP3 at 70,311/79,129ns
(12.5414% higher). Allocations are 138 and 262 respectively for both counts.
The adverse HTTP3 result is retained. This is not a fixed-class throughput or
release-performance qualification. Earlier migration failures, the refused
module-cache overlay, and the diagnostic candidate that retired all probing
IDs immediately remain preserved separately; that candidate is not final code.

Evidence and exact inputs: `.context/quic-multisocket-20261108/`. Frozen final
Go source matches all race/vet/benchmark receipts; later changes are documentation
and mock help only. User dashboard/dist bytes remain untouched. F25b behavior is
implemented; configured lint, fixed-class and final integrated release gates
remain open. No push, deployment or release.


### Integrated configured lint restored, November 8

Built golangci-lint v2.15.0 (the AD workflow's pinned version) with Go 1.27.3
into a private persistent tool directory. The complete repository scan uses the
unchanged `.golangci.yml`, concurrency 2 and all packages, with issue truncation
disabled. Final exit 0, zero issues, 54.091007994s, minimum free 11,519,586,304B.
Original complete inventory had 39 findings; its numeric failure and the earlier
36-entry truncated report remain preserved. A subsequent two-finding failure
also remains preserved.

Corrections cover test error handling, identifiers, public documentation and the
current Kubernetes FieldsV1 accessor. The QUIC whole-group test now explicitly
requires a socket failure error. Actual compiled-program instruction/state limits,
range-index invariants, deterministic test randomness and disposable credentials
justify narrow line-specific analyzer annotations. The private uncancelable
matcher path intentionally passes nil to its own helper; context-aware diagnostics
retain their original context. No analyzer is disabled in repository configuration.
A Go scanner comparison proves identical noncomment production tokens in all
five touched production files. No matcher kernel, capacity, proof, default,
benchmark, baseline or algorithm changed.

Default-race verification passes the three QUIC socket-failure entries and 13
changed follower-cancellation, fingerprint-oracle and actual API-server ownership
entries, with zero failures/skips. Wall times 6.213863577s and 41.267271066s;
minimum free 11,901,157,376B and 11,388,620,800B. Final production-token check and
whitespace check pass. All commands verify frozen source identities; user
`dashboard/dist/index.html` bytes remain unchanged. The first archive parser
attempt failed before source edits, and a first Go token-check invocation rejected
its file arguments before running; both diagnostic failures are retained.

Evidence: `.context/lint-restoration-20261108/`. This supersedes unavailable-lint
status for the current root source. Historical F20 relative/missing-scope/CPU
failures, incomplete F28 activation, HA/AD implementation and external/final
release gates remain open. No push, deployment or release.


### Composed storage, serving and central qualification, November 8

On integrated commit `19d6c7297e2d85a9c17c5229fa1a43af81a98127`, full
SQLite/trust-anchor default-race suites pass 964 entries across six packages;
serving/build/primary-runtime/runtime-owner suites pass 477 across eight packages.
Both have zero failures or skips. Wall times are 290.880184649s and
266.968804250s, with minimum free bytes 11,312,537,600 and 10,999,070,720.

Full central/audit races use the actual pinned PostgreSQL 17 image and pass
244 entries with zero failures. Their one measurement-test skip is subsequently
run explicitly: `TestDomainHourMillionCentral` ingests the original one million
rows with 252,000 distinct names and passes under default race detection, with
zero failures/skips. The ordinary and scale commands take 110.196661470s and
420.164267912s, with minimum free bytes 10,956,136,448 and 9,743,323,136.
Two package-level no-test-files markers are separate from the measurement skip.
Only the owned PostgreSQL container and its volumes are removed afterward;
the existing isolated Docker daemon and other retained fixtures are preserved.

`make size` builds an actual stripped, static CGO-disabled linux/amd64 ELF of
80,384,160 bytes, below the unchanged 100,000,000-byte ceiling. Version smoke
reports 0.2.0, commit 19d6c729, Go 1.27.3. ELF metadata confirms no dynamic
interpreter. This private artifact includes the unchanged user dashboard bundle;
it is not a clean published release artifact.

All commands match the same 4,530 tracked-file source freeze, including the
protected user dashboard bytes. Numeric outcomes are persisted before enrichment.
Evidence: `.context/integrated-qualification-20261108/`. These composed passes do
not complete F20 performance, F28 activation, enrolled HA, AD implementation,
Windows/fixed-class acceptance, hosted CI or the remaining release gates.
No push, deployment, release or waived gate.


### Complete local benchmark scope and indexed-literal refinement, November 8

On 859bacf6, all ten original fresh native processes (five blocking, five zone)
exit 0 with unchanged workloads, baseline, limits and 20%/noise-floor rules.
GOMAXPROCS=6 and the reported 2.90GHz CPU model match the committed baseline;
this sandbox is still not a named fixed-class runner. The original Go benchgate
actually runs and exits 1: 96 ok, 12 improved, four regressions, 300 new metrics,
zero missing metrics and zero absolute-limit violations. There are 99 case names,
439 metric vectors and 2,195 values, exactly five per vector. The prior missing
zone scope is now measured, without rewriting any historical failure.

The four failed medians are exact-list miss p50 163ns versus 109ns; regex-1000
miss p99 18,434ns versus 12,661ns; suffix-list miss average 175.9ns versus
140.8ns; and zone CNAME p99 1,545ns versus 1,128ns. Unchanged-list compilation
is 72,818,058ns with eight allocations; steady 3M RSS is 92.58MiB, below the
original 150MiB ceiling. This suffix/exact RSS fixture does not qualify regex,
complete serving or simultaneously held old/new snapshots. All native outcomes,
full vectors and the numeric gate failure are retained in
`.context/current-benchmark-gate-20261108/`.

An actual CPU profile of the unchanged original regex-1000 miss workload attributes
34.58% cumulative sampled CPU to indexed batch exclusion. A coincidental selected
gram sent a name through all indexed literals. The new refinement uses that same
index to require at least one complete original literal before scanning its lane.
It keeps existing sparse dispatch, rule ordering/tags, Visit callbacks, Unicode
bypass, programs, cache limits, proof budgets and retained accounting unchanged.
The existing gram tests and benchmark sources remain byte-identical.

Default-race checks pass 93 focused entries with no skips and 656 full
blocking/parser/matcher/list-fetcher entries with two actual skips (memory helper
and unconfigured external comparator). A separate actual pinned AdGuard v0.107.79
race passes all 46 entries without skips; documented deviations are unchanged.
Independent automaton-oracle fuzzing passes 255 baseline seeds and 197,295
coordinator executions in 20s requested time; internal input skips are not
reported as valid-language comparisons. Vet and configured scoped lint exit 0.
All commands match frozen source. Receipts: `.context/indexed-literal-20261108/`.

The same original 3s CPU-profile workload before/after records 898.7/600.9ns
average and 18,226/1,455ns p99, both zero B/op and allocations. This diagnostic
supports the refinement but is not a replacement five-process gate. All four
pre-fix failures and earlier F20 failures remain evidence. A prepared CRC
optimization remains unmerged because unchanged compilation passed the actual
gate. Final-source five-process and fixed-class qualification remain open.
No push, deployment, release or baseline change.


### Benchmark evidence validation, November 8

`make bench` and `bench-baseline` now require exactly BENCH_COUNT measurements
for every current metric. The gate also offers explicit processor-suffix checks;
strict CPU mode requires actual CPU headers. Parsing refuses non-finite/negative
values, malformed result rows, duplicate units, invalid iterations and mixed CPU
headers. Thresholds and absolute maxima cannot be NaN, infinity or negative.
No baseline, corpus, limit, noise floor, ignore rule or measurement command changes.

Default-race tests pass 25 entries without failures/skips; vet and configured
scoped lint pass. Tests cover partial/extra samples, missing individual metrics,
mixed processors, missing/mismatched CPUs, native failure and actual regression.
The final tool checks all retained original five-process data with samples=5,
procs=6 and strict CPU enabled: exit 1 and byte-identical full findings to the
original gate, preserving all four regressions. The Makefile dry run retains all
ten original native commands. Evidence: `.context/benchgate-integrity-20261108/`.

These integrity checks do not certify fresh-process provenance or a named runner
class. F21c/F63 calibration, remaining measured regressions and release gates
remain open. No baseline refresh, push, deployment or release.


### CNAME answer allocation, November 8

The original zone CNAME profile shows answer assembly and slice growth in the
lookup path. A first alias now reserves two RR slots, so the ordinary alias plus
one address answer allocates once instead of twice. Additional hops and larger
RRsets retain normal bounded growth. Query-type behavior, ownership callbacks,
hop budgets, wildcard copies, zone cuts and answer ordering are unchanged.
A new allocation assertion also exercises an actual ownership refusal before
following the target; all prior lookup and benchmark fixtures remain unchanged.

Full zone/zone-cache/primary-zone/authoritative-stage default races pass 659
entries, zero failures/skips, 30.851530642s and minimum free 10,667,352,064B.
Actual Kubernetes assets are enabled for the cache suite. Affected vet and
configured scoped lint exit 0. Evidence: `.context/query-tail-20261108/`.
The original 3s profiled CNAME sample records two allocations before and one
after; its after sample is 885.2ns average and 1,774ns p99. Those diagnostic
samples do not clear the original five-process 1,545ns/1,128ns median regression.
The independent pure-list miss profile points mainly to filter memory access;
no speculative Bloom-filter or nil-receiver rewrite is applied. Final-source
five-process qualification remains required. No baseline, limit or corpus edits.

### Integrated five-process benchmark after literal and CNAME fixes

All ten original native commands completed with exit 0, all groups reaped,
no source mismatches or resource stops. Five fresh blocking processes and five
fresh zone processes supply 99 benchmark cases, 439 metrics and 2,195 values.
The actual Go benchgate exits **1** with unchanged baseline, limits, 20% threshold,
five-sample count, six-processor suffix and strict CPU checks. Both CPU headers
report Intel Xeon at 2.90 GHz; this is not a named fixed-runner qualification.

Findings: 97 ok, 1 regression, 14 improved, 300 new; no missing metrics or
absolute-limit violations. The remaining relative failure is exact-list miss
p50: 158 ns versus 109 ns (+44.9541%). Its five values are 151, 158, 166, 165,
and 95 ns; the low fifth sample does not replace any prior sample.

Regex-1000 miss p99 is 1,593 ns, down from the earlier 18,434 ns median.
CNAME lookup p99 is 1,291 ns, with exactly one allocation in all five processes;
the earlier CNAME failure remains retained. Suffix miss Evaluate is 167.4 ns,
just within the original relative threshold. Unchanged 1M compilation is
72,382,832 ns and eight allocations. Steady 3M suffix/exact RSS is 92.54 MiB;
this does not measure whole-serving or retained old+new regex RSS.

Native wall time totals 668.472929887 seconds; sampled minimum free space is
10,866,843,648 bytes. Every raw per-process receipt preceded enrichment. Full
vectors and all gate findings remain beside this report. Prior full gate FAIL
on 859bacf6, isolated F20 failures, calibration gaps and external acceptance
remain unchanged. No benchmark, corpus, threshold, baseline or limit was edited.

Evidence: `.context/final-benchmark-gate-20261108/`.

### Release benchmark collection and baseline integrity

A manual workflow now accepts the owner's explicit runner label and collects
five fresh processes each for original blocking, zone, cache, upstream-strategy,
UDP/DoT/QUIC and Persist200k suites. Separate reports keep blocking helper ignore
rules from hiding the added suites. The collector preserves raw numeric failures,
resource stops and source/fixture identities, refuses output replacement, and
requires matching class/toolchain/commands/fixtures plus intact report hashes
for comparison. It never rewrites the committed baseline or declares release
qualification. Named-class execution, calibration of new absolute limits and
final hosted evidence remain open; the 40-command collector has not been run.

The actual Go gate now rejects failed/empty baselines and applies explicit sample
and processor controls to both inputs. Socket q/s is higher-is-better; packet
loss retains its original lower-is-better direction. Four collector control tests
pass, including an actual nonzero Python child; gate default races pass 37 entries
with zero skips, plus vet and installed scoped lint. Both workflow files parse
with the existing YAML v3 dependency. A missing Python YAML module and the first
helper's YAML 1.1 interpretation of `on` are retained as validation-tool failures;
no workflow input was changed to conceal them. Rechecking the retained full
benchmark output produces byte-identical findings and exit 1. No threshold,
noise floor, ignore rule, absolute limit, benchmark or corpus changed.

Evidence: `.context/release-benchmark-tools-20261108/`; commands and limitations
are documented in `bench/README.md`. F21c/F63 and final performance gates remain open.


### Replication admission and final command expiry, November 8

Inbound peer authorization now retains the original finite request context and
concurrency slot; canceled or expired successful authorization cannot dispatch.
Coordinator pulls, status and promotion include queueing in that original bound;
expired waiters invoke no dependency callbacks and grant no eligibility.
Forwarded command envelopes now register expiry after domain final checks at the
actual SQLite commit/no-op boundary, including retained dedup outcomes. Failed
check registration refuses and rolls back the whole command.

The archived original command/SQLite adapter reproduces expired status-200
mutation and dedup outcomes plus missing final-check capacity refusal (numeric 1,
four failed entries). Final default races pass 160 entries / 83 top-level tests
across cluster, internalapi, replica and follower, with no failures/skips; the
actual Kubernetes Lease handoff uses installed 1.37.0 API-server/etcd binaries.
Vet and configured scoped lint pass. Earlier 159-pass/one-envtest-skip and a
lint error in new test cleanup remain retained; the final run includes that test
correction. This closes these admission defects, not enrollment, operator
commit authorization, forwarding mounts or HA partition/rejoin acceptance.

Evidence: `.context/replica-admission-20261108/` and
`.context/replica-command-expiry-20261108/`. F60b/G11/G22 remain open.


### Expanded release workload measurements, November 8

The new collector ran once on committed 63a52742 in the isolated clean checkout:
all 40 original native commands and seven original Go shape/absolute checks exit
0, with five fresh processes per suite, no retries/replacements, resource stops
or remaining process groups. All 4,537 source identities match. The eight suites
produce 136 case names, 592 metric keys and 2,960 values; every vector has five
measurements. Native wall times total 1,112.418948899 seconds and sampled minimum
free space is 10,477,436,928 bytes. Raw numeric outcomes precede enrichment.

A separate actual Go comparison against unchanged baseline/limits exits **1**:
95 ok, 14 improved, three regressions, 300 new, no missing original metrics or
absolute-limit violations. Suffix miss latency 307.4 vs 255.1 ns/op, p50 221 vs
170 ns, and Evaluate 172.5 vs 140.8 ns/op fail the original relative arithmetic.
Earlier exact-miss and all older failures remain recorded; no sample was replaced.
Unchanged-1M median is 71,770,967 ns/8 allocations, regex-1000 p99 1,578 ns,
steady 3M suffix/exact RSS 92.97 MiB and separate peak 140.8 MiB.

UDP one/six-socket medians are 199,535/270,035 queries/s; persistent DoT
236,547/211,708, with zero loss/failure values in all reported samples. DoQ
one/four sockets are 20,487/22,092 ns/op and HTTP3 31,360/29,628; additional
sockets are not uniformly faster. Original Persist200k save/restore and all
cache/upstream vectors and allocations are retained without inventing new limits.

The explicit class is local-sandbox-diagnostic (reported 2.90 GHz, GOMAXPROCS 6).
The recording result does not override the failed original relative gate or
establish a named fixed class. F21c/F63 class selection, actual calibration,
new absolute limits, hosted and final release qualification remain open.
Evidence: `.context/release-expanded-measurements-20261108/result.md`, every
native receipt/log, all-vectors.csv and original-baseline-gate.json/log.


### November 8 — SQLite TSIG management composition

Permission-checked metadata/create/replace/overlap-removal/revoke routes now mount
through the actual SQLite command facade and complete serving owner. Generated
OpenAPI, Go and TypeScript clients include these routes. Provider lifetime,
original writer, same-transaction source/credential checks, redacted audit and
exact committed-publication observation remain mandatory. Architecture and
operator documentation distinguish durable receipt from serving acknowledgment.

Ordinary composition passed; new final-clock and physical deferred-FK commit
error tests passed (18 entries). The broad core race exposed two contract
updates (resource count and virtual TSIG SAR mapping), now corrected. The
subsequent mounted selection passed 64 entries but FAILED four entries: two
pending-startup subcases plus their parent, and native UPDATE. Startup capture
failure currently drops the expected empty unsigned SERVFAIL. The native UPDATE
advanced the durable head while its peer received EOF: the original data
generation output guard rejects the command's own commit. No wire acknowledgment
is credited and the mutation was not replayed. These response-path corrections
are the next production task; F07a remains incomplete.

All three earlier external API skips were executed against real PostgreSQL and
Kubernetes envtest: default race passed 17 entries with no skips. Full configured
lint passes after two test-only switch cleanups; affected-package vet and
dashboard typecheck pass. Original failures and every source freeze remain in
`.context/tsig-management-integration-20261108/`. Production Kubernetes authority,
remaining original final-authority cases and release gates remain open.


### November 8 — native UPDATE acknowledgment and startup refusal

D313 separates the exact empty response returned by the actual root UPDATE
adapter from ordinary DNS data. The private completion retains original writer,
verified immutable keys/dependency installations, request/signature deadlines and
pre-command mode/retirement read attempt. Binding after the pipeline rejects
changed response codes, replaced messages, answer data and transfers. Ordinary
queries retain their complete generation check; a separate UPDATE invalidates a
held query. No command retry, replacement capture or new Store lookup is added.
Unavailable startup captures emit only empty unsigned SERVFAIL through the
original transport deadline, with partial lease cleanup and no baseline signer.

The unchanged original startup/native UPDATE-NOTIFY-transfer tests now pass.
Full DNS-server and primaryruntime race qualification passed 581 entries with
four existing measurement/helper skips, no failures (133.607475977 seconds).
The broad mounted race passed 36 entries, including actual background TCP/TLS,
management and encrypted restart, native transfer and anchor barriers. It failed
the deliberate writer-fencing test and its parent because the optional fixture
classified the intended driver exit as unexpected. That outcome is preserved.
The fixture now accepts only the explicitly expected loss of its original term
with the actual unavailable error. The corrected output/primary selection passes
14 entries, no failures/skips (55.705224212 seconds), including signed TCP/UDP
acknowledgments and final data/key/writer/cleanup/deadline refusals. Production
bytes are unchanged between these race selections. Full configured root lint
passes (83.564644963 seconds, zero issues); affected-package vet passes.

Earlier new-fixture missing-preparation-bound failures and the stopped original
run remain in `.context/dns-response-admission-20261108/`; no original deadlines
or production guards were relaxed. These scoped checks do not complete the
remaining F07a/Kubernetes, managed activation, HA, performance or release gates.

### November 8 — observed Kubernetes startup identity prerequisite

`StartupClusterID` now shares actual unpinned namespace discovery or the existing
authenticated pinned guard, bounded by the original caller and startup timeout.
Empty UIDs never signal readiness, configured expectations are not observations,
and Store closure cancels held work. No global startup/health or TSIG mount call
site is added; unsigned saved-state availability remains independent.

Focused default race passed 28 entries; an additional held pinned-Close test
passed. Full Kubernetes driver default race passed 202 entries with no failures
in 78.927497716 seconds against actual envtest. Exactly three optional spike
measurements skipped because `KUDURRU_MEASURE=1` was absent: TestMeasureStore,
TestMeasureFirstRequests and TestMeasureElectorProductionTimings. Configured
scoped lint passed with zero issues (6.614283894 seconds); vet passed
(1.009347458 seconds). All frozen sources matched. Evidence is retained in
`.context/kube-tsig-identity-20261108/`. Kubernetes complete TSIG preparation,
transport/background mounts and accepted management authority remain open;
these checks supply no performance, hot-rotation or release qualification.


### November 8 — Kubernetes immutable stored TSIG startup mount

D314 adds a single asynchronous preparation under the original startup deadline:
actual authenticated cluster identity, complete bounded private source, then full
decrypt/merge with the captured encryption and file inputs. Unknown and failed
sources cannot authorize file-only signing. Successful keys are immutable until
restart; no query-time Store lookup, late credential adoption, hot administration
or cross-resource transaction authority is introduced. Unrelated unsigned
saved-state DNS and unsigned secondary acquisition remain independent.

All six native/encrypted transports select the actual StaticKeyring before
verification without claiming a complete serving lease. Secondary jobs preserve
stored credential epochs and original acquired expiry. NOTIFY selects immutable
keys before network work; only its initial round waits for preparation. An actual
envtest/runServe test verifies online/restart signatures, signed SOA/transfer and
NOTIFY, API-outage and wrong-owner refusal, and unchanged persisted acquisition
metadata when no complete source is available. It passed default race with no
skip (38.275064642 seconds). Focused default race passed 18 entries with no skips;
full DNS-server/secondary/notifier/tsigkeys races passed 650 entries with exactly
four existing measurement/helper skips, no failures (55.162494342 seconds).

Original Kubernetes offline and SQLite TSIG compatibility races passed 31 entries,
no failures/skips (203.367148342 seconds), including native UPDATE acknowledgments,
background TCP/TLS, encryption restart, pending-source refusal and original expiry.
Full configured root lint passed with zero issues (87.161939136 seconds); affected
vet passed (3.611785218 seconds). Production bytes stayed unchanged across these
checks. Source01's optional-fixture failures (signed-only HTTP helper applied to
unsigned queries; mixed-case key assertion and missing secondary primaries) remain
preserved, with source02 corrections limited to the new tests. The mounted test
alone expanded in source03. Receipts/freezes live in
`.context/kube-tsig-startup-20261108/`.

This is startup-only source activation. Kubernetes management/current mutation
authority, remaining F07 acceptance, managed-root activation, HA, AD, all retained
performance failures and external/final release gates remain open. No push or
release publication follows from these checks.


## November 8: retained managed edge cache policy (D315)

The supervisor now exclusively owns its configuration-directory inode from
before preparation through worker and child shutdown. First managed startup
persists one private versioned bypass identity, syncing the file and directory
ancestry before child startup. Restart retains that identity; disabling bypass,
malformed or indirect storage refuses without repair. Failed preparation does
not erase the policy. Actual installed evidence still requires the loaded child,
both console detachments and the original source/child epoch.

Evidence: `.context/edge-managed-durability-20261108/`. The first focused command
failed compilation on a new integer-type mismatch, before any tests executed;
that receipt is retained. After the correction, focused default race passes 21
entries with the opt-in live case skipped. Actual pinned dnsdist YAML and Lua
acceptance then passes all three entries without skips, including full supervisor
restart on the same directory, disabled-bypass refusal and uncached postrestart
queries. Full edge default race passes 189 entries with two explicitly opt-in
live skips; the managed-cache live case was exercised separately.

This closes a restart prerequisite, not F28 activation. The authenticated
backend-to-edge exchange, original activation admission, elected/follower mounts,
complete retirement/interoperability and final release gates remain open. Storage
must persist for the deployment; filesystem replacement/rollback, independent
processes and orphan daemons outside the supported PID-1 container lifecycle
are not fenced by an advisory directory lock.

Configured scoped lint passes with zero issues after six specific gosec findings
were annotated: one public file-format tag and five operator/test-owned paths.
No behavior changed after race/live qualification; the first lint failure is
retained. Source hashes and raw outcomes are in the evidence directory.


## November 8: authenticated managed edge observations (D316)

The optional host-only `edge.managedObservationAddress` listener exposes the
actual retained deployment, fresh supervisor incarnation, acknowledged child/
source epochs and detached backend membership through the existing authenticated
internal TLS guard. One fresh challenge, expected node/deployment, original
2-second total deadline, 128-member and 128 KiB limits bound acquisition.
Substituted/malformed/redirected/expired observations refuse without replacement.
The listener and zone polling share one captured credential source. Listener
failure cancels and joins the supervisor and other listener.

Evidence: `.context/edge-managed-observation-20261108/`. Focused default race
passes 26 entries without skips. Actual pinned dnsdist YAML/Lua acceptance passes
three entries without skips, exercising the authenticated exchange after loaded
policy/source/child acknowledgment and full supervisor restart. Full internal
API, edge and configuration default race passes 596 entries, with only the two
opt-in console/managed-live cases skipped; managed-live ran separately. Root
bind-failure coverage is lifecycle evidence with a script child, not dnsdist
cache qualification. The CLI and configuration references are regenerated,
including the prior D315 help text omitted from its commit.

These observations grant no backend activation or mutation authority. Explicit
exhaustive topology, original local activation admission, updater/follower
mounts and the complete F28 retirement/interoperability requirements remain open.
All previous release, performance and external acceptance failures remain open.

Root edge/supervisor configuration and generated-reference default race passes
46 entries without failures/skips. The first full configured lint command fails
one missing exported-error-group comment; its receipt is retained. Only the
missing comment and explanatory node-name documentation changed afterward.

The final full configured repository lint passes with zero issues in 58.4938 s
on source04. All code behavior and tests match the qualified source; the sole
post-race Go edit is the exported-error-group documentation comment.


### Original complete edge fleet collector

The private root collector now validates the entire explicit nonempty fleet before
network acquisition, detaches requirements, verifies exact local backend tuples
on every authenticated response and issues only one complete result. Partial
failure cannot retry earlier peers or proceed to later peers. Original caller
and runtime cancellation, issued pointer identity and finite lifetime remain
mandatory. Focused default race passes 17 entries without failures/skips; evidence
is `.context/anchor-edge-fleet-20261108/`. The collector is unmounted and supplies
no activation grant. The explicit topology/lifecycle contract, input adoption,
terminal writer/follower behavior and full F28 acceptance remain open.

Scoped configured lint passes after renaming one test variable that shadowed a
builtin; the first lint failure is retained, and production bytes did not change.
Source review also confirms two independent automatic retry layers, RunWriter
and cluster.Runner.superviseLeader. A future terminal anchor outcome must survive
both without stopping unrelated writer work. This remains a mounting constraint,
not a claim that the current unmounted worker is active.


### F28 terminal writer state across both retry layers

A runtime-owned terminal record now survives host replacement and cluster
Runner restart within the original elected term/incarnation. The failed anchor
child stays joined until cancellation, leaving unrelated writer services alive.
Completed-receipt cancellation remains terminal and preserves original error
identity. A canceled incomplete socket joins without inventing a receipt; only
a genuinely new term/incarnation clears the terminal record. This adapter is
still unmounted; managed activation and full F28 acceptance remain open.

Evidence: `.context/anchor-writer-terminal-20261108/`. Initial focused race
failed one new test because its server accepted only one exchange. That fixture
now accepts and holds both original canceled callers. The correction also adds
a real signed receipt with final-pin cancellation test. Focused race passes 10
entries; composed Anchor/RootAnchor/ServingRuntime race passes 92, no skips or
failures. Vet exits 0. First scoped lint failed one unused test parameter;
renaming only that parameter to underscore yields configured lint exit 0 with
zero issues (23.6383 s). Production source matches the successful race/vet cut.
Prior failures and all performance/external gates remain retained.


### F28 direct standalone runtime activation (D317)

Explicit host-only `upstreams.recursion.managedTopology: direct` now mounts the
actual elected updater and managed complete builder on standalone SQLite. It
requires no PROXYv2 ingress and an exhaustive operator declaration that clients
reach the node without a caching proxy. The private admission belongs to one
root owner/runtime, survives successful preparation cancellation and is retained
by managed publication. Disabled topology, unsupported stores and enrolled roles
cannot activate; remote edge and enrolled HA acceptance remain open.

The first bounded anchor step completes before unrelated elected startup writers
start. All outcomes release those services; terminal anchor work remains parked
with its term/incarnation record. No original source/capture/receipt checks were
relaxed. UI-owned upstream replacement now preserves the original host topology
alongside the import path, with original environment precedence.

Evidence `.context/anchor-direct-activation-20261108/` preserves all commands and
source01..09. Focused ordinary9 passes. First mounted ordinary fails a new DoQ
fixture nonzero ID; exact ID-zero correction passes actual three-phase runServe:
signed-wire bootstrap, all six transports, retained restart without rebootstrap,
and disabled admission returning empty unsigned SERVFAIL/auth503. First composed
race (96 pass/1 fail) exposes genuine primary-startup generation interposition;
startup sequencing fixes it. A focused race then passes8. The next composed race
passes96 but fails one new shutdown expectation: primary's existing writer check
may return ErrUnavailable when cancellation races a generation wake. Only that
new fixture is narrowed to accept the exact joined cancellation outcome with
same original live token/term/incarnation; early worker exit still fails. Final
direct race passes5/no skips, including deterministic held-reply startup ordering,
terminal caller replacement, explicit follower refusal and all6 runServe paths.
The preceding composed commands remain failures, not relabeled full-suite green.

Full servingbuild race passes218/no skips. The same command's config suite
fails2 (lost host topology under UI section ownership, missing enum help); fixes
and new source-precedence cases yield full config race350/no skips. Affected vet
exits0/22.2384s; reference generation and all4 drift tests pass. Full configured
lint initially fails one context-parameter order in a new helper; signature/call
reordering only yields lint0/24.2393s/zero issues. Production runtime bytes match
the qualified race cut; final config loader behavior is source08, final test-only
signature source09. Complete signed native-recursive/root-retirement integration,
edge activation, enrolled follower lifecycle and RFC interoperability are still
required. All HA/AD, fixed-class/performance and external/final gates stay open.


### F28 native recursive DNS through the complete runtime

The direct standalone runtime now has actual native-recursion acceptance over
UDP, TCP, DoT, DoH, DoQ and DoH3. A real non-loopback private root serves signed
ED25519 DNSKEY records and priming NS/glue. Every response must retain AD, the
original DS identity and a verifiable signature. Both actual runServe lifetimes
pass: restart reacquires native data over UDP, performs no second bootstrap TCP
fetch and leaves the accepted owner version unchanged. The outer raw cache is
disabled so it cannot substitute for the native resolver path.

Evidence: `.context/anchor-native-mount-20261108/`. The first ordinary command
fails readiness; a new fixture startup-error report identifies its omitted
required upstream strategy in the second failed command. Supplying that fixture
field yields ordinary exit0/one pass (20.8447 s); combined direct authoritative
and native-recursive default-race exits0/two passes/no skips (64.9035 s). No
production behavior changed in this tranche. Earlier failures remain preserved.
Configured command-package lint exits0/zero issues (15.0280 s). All-anchor-loss
retirement, follower supervision/activation, edge topology and external RFC
interoperability remain open; this is not full F28 or release qualification.


### F28 direct follower supervisor and actual revocation output (D318)

Direct SQLite activation now admits enrolled primary/follower roles. The complete
runtime mounts a deny-only follower supervisor; initial absent/dormant data waits
without acquiring trust or initializing canonical state. Every captured child
retains its exact installed incarnation. Terminal results survive later installs
within the same member/authority term, including their local incarnation changes.
Role/term changes cancel and join old acquisition before successor work; failures
park without ending unrelated runtime services. Edge topology and full HA
enrollment/forwarding/promotion orchestration remain separately incomplete.

Evidence `.context/anchor-follower-supervisor-20261108/` retains source01..05
and every numeric outcome. The first focused ordinary command passes3/fails2:
one failure exposes supervisor rearm on InstallReplica's incarnation increment;
the other compares raw producer/receiver documents across canonicalization.
The supervisor now retains terminal results by member/term while preserving the
individual poller's stricter checks. The fixture verifies installed generation
and exact receiver bytes before/after. Corrected ordinary passes5/no skips; the
final composed anchor/root/runtime default-race passes106/no skips (128.5986 s).
Promotion now requires actual peer EOF before runtime shutdown. Original failed
commands remain failures.

A separate actual runServe test bootstraps a primary from a real signed response,
then installs its complete image on an enrolled follower with target capture
before source read. The mounted follower serves all six transports while its
independent acquisition is held; authenticated REVOKE then retires output to
empty unsigned SERVFAIL and readiness/auth503. Actual close/open/runServe
restart preserves denial, exact canonical image and the two total root fetches.
Ordinary1 passes (29.8483 s); default-race1 passes/no skips (32.2512 s). This
proves local follower denial, not canonical follower progress or accelerated
RFC hold-down. The outer raw cache is disabled in this fixture; previous cache
retirement evidence remains separately recorded. All-anchor-loss canonical
writer/restart composition, edge activation, RFC interoperability and remaining
HA/AD/performance/external release requirements remain open.

Final static follow-up: docs generator exits0 and full config/reference ordinary
checks pass354/no skips. The first full lint exits1: unchecked joined-child result
and two lostcancel diagnostics. An explicit child object now owns cancellation
and completion; canceled old-epoch results are consumed without permitting retry.
Final affected default-race on source06 passes6/no skips (43.2701 s), including
all mounted follower transport/restart and peer-EOF cases. The broader106 result
remains tied to its prior source03; it is not silently relabeled as source06.

Second full lint fails one S1005 discarded-value diagnostic. Source07 records
non-cancellation errors from joined children instead; final full configured
lint exits0/zero issues (29.2496 s). This last change adds completion logging
only; source06's six race results retain their exact source fingerprint. Same-term
install after a successful normal observation still needs a bounded fresh-poll
continuation; terminal old operations must never be rearmed. That follow-up
remains explicit rather than claiming complete automatic follower liveness.


### F28 fresh follower acquisition after completed normal input

A successful authenticated normal observation now permits a later fresh capture
of a different installed incarnation within the same member/authority term. Its
original operation has already completed with the exact final authority check.
The successor operation captures its own source/local carrier/deadline before
wire acquisition; unchanged owner/settings retain the existing cooldown.
Terminal errors still set `stopped` before any future capture, and the supervisor
retains terminal outcomes across same-term installs. A held receipt cannot adopt
a successor, even after an earlier normal observation succeeded. Denial and
incomplete transport do not create this normal-completion permission.

Evidence `.context/anchor-follower-continuation-20261108/`: production source01
passes all27 follower default-race entries/no skips (255.8060 s including cold
compilation after the explicit standard Go build-cache reclamation). The actual
SQLite tests retain canonical follower bytes through signed normal input and
new primary refresh/install. The held-response test is strengthened separately
to exercise successful normal input, one admitted new source, then another
install while that next receipt is held. Final results are recorded below.
The prior stricter per-operation epoch test and malformed-terminal-install
regression remain unchanged. This closes the D318 successful-normal continuation
gap; canonical writer all-loss/restart, edge activation, recovery, enrolled HA
orchestration and external/final release requirements remain open.

The final strengthened pair passes2/no skips under default race (24.8420 s),
including a previously successful normal observation before the held successor
receipt is invalidated by a second actual install. Production code is identical
to the 27-pass follower suite; only that new test was strengthened.

Final scoped configured command-package lint exits0/zero issues (182.5157 s,
including cold normal-mode dependencies). No earlier failures are erased.


### F28 canonical all-anchor loss and physical restart

The actual elector-carried root caller now has a complete-runtime regression
for signed bootstrap followed by signed self-revocation of the sole anchor.
A real net.Pipe write inside the original Output.Emit scope holds retirement;
after the authority observes the second acquisition, denial cannot complete
until that write drains. Both local denial and paired canonical update commit,
the old transfer continuation refuses, and accepted history retains quarantine
and the permanent revocation tombstone.

The same SQLite database is physically closed and reopened under managed,
static/default and disabled-topology configuration. Each complete runtime
refuses protected publication with prepare_failed, preserving the exact accepted
version and encoded pure state without another root acquisition. The two root
operations are explicitly invoked under the actual elector; this does not
accelerate or qualify the one-hour scheduler minimum. Restart runs the complete
read runtime, not a newly elected writer. The held output is one scoped pipe
write; prior mounted six-transport evidence remains separate.

Evidence: `.context/anchor-canonical-loss-20261108/`. Initial ordinary passes1
with no skips (39.6666 s). The strengthened source02 synchronizes on the actual
second authority request and passes1 under default race with no skips (28.4512 s).
Initial scoped lint fails QF1003; source03 changes only the restart branch to a
tagged switch. Edge activation, explicit recovery, enrolled HA orchestration,
RFC interoperability and final release requirements remain open.

Final scoped configured command-package lint exits0/zero issues (27.6478 s)
on source03. The prior race remains tied to source02; only the tagged-switch
style correction separates them.


### F28 original installed edge fleet activation (D319)

SQLite host topology now supports `edges` with 1–32 explicit unique node,
observation address, retained deployment identity and exact backend DNS/internal
membership requirements. Internal credentials must exist. The complete root
constructor captures one local image/token before contacting any peer, validates
the whole fleet, then acquires one authenticated installed observation from each.
The original Store publication barrier adopts only the exact issued observation
once into runtime life; any deadline, peer/membership or local authority/image
refusal fails startup without recapture. All original canonical/local-denial,
writer, complete publication, native/raw cache and final-output checks remain.

Runtime lifetime adoption relies on the previously qualified D315 persistent
immutable bypass across edge children; diagnostic readiness never grants it.
Host declarations cannot discover undeclared proxies. Replacing/rolling back edge
deployment storage is excluded as before. An initial empty follower requires its
first complete image before edge startup; direct followers can still wait.
Full enrolled HA bootstrap/orchestration remains open.

Evidence `.context/anchor-edge-activation-20261108/`: focused ordinary44 passes,
no skips (36.6714 s). Actual pinned dnsdist2.1.3 image89c7f196 plus a real runServe
backend initially fails only the final query before post-restart health recovery;
that original failure is retained. The corrected bounded readiness wait passes1,
no skips (23.8418 s). This actual YAML daemon test starts installed bypass before
the backend is ready, bootstraps once, queries both authoritative/resolver pools
over UDP/TCP, restarts the edge with retained deployment/new run identity, then
joins the backend and observes no cached data. Backend restart authenticates the
fleet again without another root bootstrap or accepted-version progress; both
actual pools have zero cache capacity/entries. Previous Lua installed-cache
qualification remains separate; this is not all external edge transport coverage.

Composed source03 default race passes133 entries/no skips (167.4716 s), including
the enabled pinned daemon, all managed-root/runtime cases and selected config
cases. Source review then found the host fleet needed the same explicit retention
as topology across UI-owned upstream section loading. Source04 preserves that
host list and tests actual file/UI/environment precedence. Final config/static
outcomes are recorded separately below; source03's race evidence is not relabeled.

Safe operator recovery, initial edge follower installation/orchestration, complete
RFC interoperability/hold-down and remaining release gates are still open.

Final configuration source05 passes363 default-race entries/no skips
(31.6501 s). The first full config race retained361 passes/two failures: the new
fixture incorrectly expected environment-list support, which existing policy
intentionally refuses, and the generic enum fixture lacked the required fleet.
Corrected fixtures retain that refusal, test scalar environment topology
precedence, and supply a complete fleet for the positive enum case. The actual
file/UI list preservation now passes without weakening collection validation.
The reference generator also exits0 (1.4053 s).

Final generated-reference tests pass4/no skips (0.8041 s), and full repository
configured golangci-lint exits0/zero issues on source06 (115.1813 s). No rules
or thresholds changed. The final runtime/activation production files are identical
to source03; source04/05 changes are the separately qualified host loader and
config fixtures. Minimum free space during lint was32,947,277,824 bytes.


### F28 recovery provenance and persistence primitives (D320)

Explicit quarantined-owner recovery authenticates new DS-selected DNSKEY trust,
retains initial policy/seed identity and exact permanent tombstones, and records
at most 32 immutable version-2 recovery events. Ordinary refresh preserves the
ledger. A separate opaque proof binds both complete state digests and the original
wall/monotonic lifetime through captured SQLite commit. No operator command,
authorization/audit mount, migration or automatic reseed is exposed here.

Pure race73 and pure/accepted/persistence race190 passed without skips. The wider
trustanchors/SQLite default-race run reached 975 passing test entries but FAILED
at its five-minute package deadline; it is not full SQLite qualification. New
recovery tests prove actual original-refusal rollback, committed recovery, physical
writer/follower restart, ordinary continuation and mounted rejection of an
independently well-formed replica rewriting prior recovery provenance. Their first
run failed because a writer-only fixture read was used on a follower; the corrected
read-only installed-image fixture passes both tests under race. Evidence remains
in .context/anchor-recovery-20261108; all earlier failures are retained.

A separate qualification compiled the exact pre-change pure codec from be4446fc
and fed it actual generated format-1 and recovered format-2 documents: race1
passed, preserving format-1 bytes and rejecting format 2. This checks the codec,
not an entire old daemon or mixed-version fleet. The first lint invocation could
not start a missing executable; the installed configured linter then reported
one test variable shadowing builtin copy, corrected with a local rename.
Offline operator admission/audit, strict existing-schema opening, operational
mixed-version acceptance and complete F28/release gates remain open.

Final configured trustanchors/SQLite scoped lint passes (zero issues, 2.006s).


### F28 offline original-command recovery (D321)

`anchors status|recover` now uses a strict existing current-schema SQLite open:
no database/directory creation, schema migration, metadata repair or authority
initialization. Original finite context, existing WAL mode, complete metadata and
exclusive process ownership are required. Real SQLite tests cover missing/empty/
indirect/old/new-schema files, missing identity/audit metadata, non-WAL state and
actual process-lock exclusion; 17 race entries pass without skips.

The OS-authorized offline command requires explicit stopped-process acknowledgment,
exact original accepted-envelope digest, reason, bounded replacement DS records
and numeric root endpoints. It captures original owner/writer/source before TCP
acquisition, uses actual preparse receipt sampling and returns the independent SQL
disposition. Trust and the OS UID/reason/version audit commit in one transaction.
An actual SQLite trigger rejecting the audit proves rollback of both; stale input,
unauthenticated input and cancellation preserve the original quarantine. The first
command build failed on a duplicate fixture variable declaration and is retained;
corrected ordinary7 and final default-race8 pass without skips.

The final race includes actual `runServe` after offline recovery, complete native
recursive DNSKEY authentication and AD output over UDP/TCP/DoT/DoH/DoQ/DoH3, then
a second physical restart of the same database. Original configured seed/policy
identity stays fixed; the explicit replacement key verifies the actual wire. The
original recovery version and tombstones remain, with one recovery TCP acquisition
and no restart rebootstrap/progress. Wall65.7071s, package25.987s, minimum32,864,342,016B;
evidence .context/anchor-recovery-command-20261108. This is not full enrolled HA,
external RFC interoperability, fleet mixed-version or final release acceptance.

Final configured command/SQLite/trustanchors lint passes (zero issues, 29.051s).
The first two lint runs retained test-only G115/G304 diagnostics; explicit clock
range termination and test-path annotations fixed them. One broad race invocation
was accidentally started before lint had exited; root terminated only that newly
owned group (child -15, wrapper241), retained its137 passes as unqualified and
verified no remaining Go/compiler/link/test processes before the sequential run.
The separate earlier five-minute package timeout remains failed evidence.

The final sequential default-race suite on source06 passes all 1,026 test entries
across trustanchors, accepted, persistence, resolverstate, retirement, SQLite and
runtimeowners, with zero failures/skips. Go0, wall280.2494s, SQLite268.006s, minimum
32,446,283,776B, log1,071,360B, no source mismatches or resource stop. This supersedes
neither the retained five-minute timeout nor the interrupted overlap attempt;
those outcomes remain explicit.

Final CLI/default-race30 passes without failures/skips (65.9093s), including
all offline recovery cases, regenerated CLI-reference drift, shell completion
and existing convenience-command regressions. Source06 remains unchanged.


### F60b/G11 promotion authorization prerequisite, November 8

D322 changes coordinator operator authorization from an early verdict to a
retained original proof check plus dependency release. Missing callbacks refuse.
The coordinator keeps that original authorization and the complete serving pin
through SQLite `PromoteChecked`, whose final memory/clock check runs after all
staged authority/feed writes and before commit or idempotent success. A refusal
rolls back, preserves memory authority and generation notifications, and survives
physical reopen. Cancellation cannot be hidden by a callback returning nil.
The low-level store CAS remains available to already-authorized internal callers;
this change does not mount enrollment, forwarding or operator promotion routes.

Focused default-race qualification passes 21 entries without failures or skips,
including actual SQLite rollback/reopen, exact retry, terminal SQL1811 failure,
original-context/dependency lifetime and queued admission. The full SQLite, follower, replica, serving, servingauth and servingbuild
default-race run passes 1,192 entries without failures or skips (296.055 seconds).
All frozen source identities match; configured scoped lint passes with zero
issues (8.415 seconds). Evidence:
`.context/promotion-authorization-20261108/`.

### F20 retained exact-miss performance diagnosis, November 8

No matcher, parser, workload or gate changed. The original five-process gate on
6cace80f still fails exact-1M miss p50 (158 ns median versus 109 ns). One isolated
original Evaluate CPU profile reports 88.96 ns/op, zero allocations, and attributes
most samples to normalization/filter memory loads; nil regex calls are a minor
cost. One full original latency-sibling CPU profile reproduces 169 ns exact-miss
p50 with a 35 ns timer p50. Profiling is diagnostic and cannot replace unprofiled
samples or named-runner evidence. The profiles do not justify a matcher semantic
or filter-capacity change. Original failures and fixed-class/final gates remain
open. Evidence: `.context/exact-miss-review-20261108/`.


### F60b/G11 independent member identity prerequisite, November 8

D323 adds `internal/cluster/memberidentity`. Exclusive durable 0600 creation and
strict loading retain a private node-local Ed25519 CA bound to the original
member UUID. Public enrollment data is detached; no private key enters images.
Standard TLS 1.3 mutual verification uses exact independent member CAs before
application bytes. Short-lived leaf renewal/restart keeps the enrolled CA;
a different CA cannot impersonate it by claiming the same UUID. Route checks
match the actual verified connection to the exact member and recheck original
certificate expiry. No insecure certificate-verification fallback is installed.

Initial default-race17 passes without failures/skips, including actual two-peer
TLS, wrong-client/wrong-server refusal before HTTP, same-UUID replacement,
physical key reopen, concurrent create, bad-file refusal, and held original leaf
expiry. Expanded final default-race21 passes without failures/skips (4.612 s),
adding another enrolled member claiming the wrong UUID, anonymous-client and
TLS1.2 refusals. Configured scoped lint passes with zero issues (2.007 s). Its
first run flagged intentional private-key serialization; narrowly justified
annotations identify the protected file and in-memory comparison. That failure
is retained; final source differs from race02 only in those comments. This is an identity
prerequisite only: durable membership, join tokens, root wiring, current
revocation/output checks, forwarding and complete HA remain open. Evidence:
`.context/member-identity-20261108/`.


### F60b/G11 captured member replication client, November 8

D324 adds explicit `NewMemberClient` with the node-local D323 identity and each
original resolved peer's exact CA. Local UUID/key mismatch, missing target trust
and custom transports refuse. One captured exchange owns a TLS transport with
connection reuse disabled, so a changed enrollment at the same address cannot
send through an old connection. Certificate/request lifetime is checked before
authority observation and final image/command-result admission. A TLS rejection
cannot create network-loss promotion eligibility; a POST alert or original
cancellation after complete body read preserves unknown outcome without replay.

Focused default-race4 passes against actual SQLite/TLS image, 304 and command
paths, same-address changed trust, same-UUID impostor refusing before HTTP,
explicit dedup retry, credential refusal, and actual committed command with
cancellation at completed body EOF. Final composed replica/memberidentity/
follower default-race112 passes without failures/skips (28.271 s), including
actual remote-TLS refusal classification. Configured scoped lint passes with
zero issues (6.220 s); source03 differs from qualified source02 only in the
constructor documentation. This client does not
supply durable enrollment, current membership/output exclusion or root startup;
those remain open. Legacy shared-credential transport tests are retained.
Evidence: `.context/replica-member-client-20261108/`.

### F60b/G11 bounded enrollment owner, November 8

D325 adds immutable complete member/invitation state and bounded canonical wire
formats. Owner limits are 64 active members, 256 historical identities, 64
invitations and 512 KiB. Hash-only stored invitations bind original cluster,
issuer term, exact target and an absolute lifetime of at most ten minutes.
Purpose-separated possession signatures also bind original timestamps. Accept
consumes the invitation and adds the member together in one successor; revoked
UUIDs and signing keys cannot return under replacement certificates. Revocation
cancels older target invitations; explicit expiry pruning never removes members.
Opaque admission retains original context, actual time, issuer and before/after
digests for a terminal final check. It is not a durable commit receipt.

Default-race54 passes without failures/skips (59.722 s), including actual 256-member
history exhaustion, new-certificate/same-key revocation, valid-but-rewritten
history refusal, one-time consumption, strict wire decoding, real expiry and
zero-value admission refusal. Final configured scoped lint passes with zero
issues (3.649 s); earlier missing-comment and formatting failures are retained.
Final source04 differs from race-qualified source02 only in comments/whitespace. These pure
primitives do not supply durable owner migration, join-token routes, operator
policy, follower install exclusion or root HA mounting. All earlier performance
failures and external acceptance requirements remain open.
Evidence: `.context/enrollment-owner-20261108/`.

### F60b/G11 complete enrollment storage, November 8

D326 introduces supported schema9 and required member-enrollment-v1 image
negotiation. Migration inserts only an exact non-granting marker; original startup
converts that marker to Dormant. Missing, malformed, orphaned or oversized
current-schema membership never repairs. Schema8 anchor retirement provenance
and bytes remain unchanged, with the loader explicitly supporting schema9.
The original schema8-only migration regression still targets actual schema8;
current-store fixtures assert the actual supported marker without emulation.

One-use original writer/operator captures retain exact owner CAS and finite
lifetime. The SQLite adapter atomically stores invitation consumption and member
addition, registers the postimage fence, and retains original possession/expiry
through commit. Generic command owner mutations and actual install/conflict
replacement preserve history; legacy bulk writes refuse the enrollment prefix.
Secret invitation responses remain staged until the outer command commits.

Expanded focused default-race26 passes without failures/skips (15.626 s). Cases
include actual SQL1811 rollback, original cancellation/authorization, postimage
ABA, real expiry after staging, raw erasure refusal, physical primary/follower
restart, higher-image resurrection and explicit conflict-resolution refusal,
quarantine rollback, complete current-schema corruption refusal, schema8-to9
row preservation and actual downgrade refusal. Earlier unsupported-schema
loader and missing-token fixture failures remain preserved. The first broad run
had 1003 passes and 16 failed entries: random UUID ordering in a new history fixture,
new mandatory-row accounting, future-schema and old-schema fixture framing.
Those are corrected while preserving original negative targets and validation
error priority; 34 regression race entries pass. Final composed source05 default-race suite
passes 1464 entries without failures/test skips (357.534 s), including serving,
authentication publication and local storage. The accepted framing package has
no standalone tests and reports a package-only skip; actual adapter/image cases
exercise it. Full configured root lint passes with zero issues (130.222 s).
A later admission-only correction puts opaque handle terminal/consumption state
in private shared cells, so copied values cannot reopen attempts; final source06
affected default-race passes 69 entries without failures/skips (42.676 s).
Final source06 mounted startup/recovery/cluster default-race passes 26 entries
without failures/skips (120.999 s); final full configured lint exits 0 with zero
issues (99.199 s). Root enrollment routes, operator/output
barriers, current peer exclusion and HA orchestration remain open; this is not
production enrollment completion. Evidence: `.context/enrollment-storage-20261108/`.

### F60b/G11 atomic initial Primary enrollment, November 8

D327 stages first membership and Standalone-to-Primary authority in one actual
SQLite Command from an original Store-issued complete cut, an existing Dormant
owner and the local private identity. The caller retains original operator
authorization and dependency pins; final admission checks the original lifetime
and exact planned authority while old memory authority remains installed.
Audit can be appended in the same transaction. A successful commit advances one
generation and fences the old writer; rollback changes none of these effects.
Uncertain completion blocks the old writer and wakes dependents without claiming
a rollback or a committed candidate. The actual restart reads durable authority.

Focused source01 default-race passes 21 entries without failures/skips (13.626 s).
Composed enrollment, SQLite, replica and serving/auth/build race passes 1259
entries without failures or test skips (374.423 s). The accepted framing package
has no standalone tests; actual adapter/image cases exercise it. Final source02 explicitly distinguishes unknown completion from confirmed
rollback; all 21 focused race entries pass again (11.219 s), and configured
SQLite/enrollment lint passes with zero issues (8.018 s).
Root operator routes, publication/output acknowledgment, peer exclusion, join
transport and full HA assembly remain open.
Evidence: `.context/enrollment-primary-20261108/`.

### F63/G23 fixed benchmark runner decision, November 8

The owner explicitly selected Ubicloud 8-core Ubuntu 24.04. The measurement
workflow now pins `ubicloud-standard-8-ubuntu-2404`, with the original
`GOMAXPROCS=6`, five fresh samples per suite, original corpora and durations.
The runner-choice decision is resolved. Eight host cores do not silently change
the original six-processor workload or authorize a baseline/threshold reset.

Actual hosted baseline calibration, additional absolute limits and candidate
comparison remain open. The workflow currently records evidence; a successful
recording alone does not qualify the release. The original local gate failure
(exact1M miss p50 median158ns versus109ns) and every earlier failed result remain
retained. Local sandbox results cannot close this named-runner gate. No workflow
execution, push, baseline change or performance completion is claimed here.

### F60b/G11 original peer request and output boundary, November 8

D328 adds actual TLS certificate admission and Store-issued original peer
requests. One bounded read cut requires active local/remote members and exact
Primary authority before body I/O. A request permits one image or one actual
command; copied handles share consumption. Original owner/writer checks, the
membership postimage fence, original cancellation and the final certificate check
protect actual command completion. The last peer check follows domain checks.

Only a successful actual completion permits one output scope. That scope compares
the exact resulting authority/generation and original owner and holds wmu through
bounded emission and explicit flush. Original request/peer-certificate/local-CA
expiration supplies the earlier transport deadline. Canceled lock waiters return
without acquiring replacement authority; an active emitter retains exclusion
until it actually returns. Unknown commit has a distinct error and no output.

Initial source01 focused race 36 passed. Source02 expanded actual HTTP coverage
reported 44 passes/4 failed entries: the HTTP2 fixture waited for END_STREAM while
deliberately preventing handler return. Source03 checks protected body bytes or
304 headers before releasing the handler, then separately checks clean stream
completion. The original failed receipt remains. Source03 focused race 52 passes
without failures/skips (22.442 s). Configured lint initially found two test-only
style/check issues; both are corrected in source04 and lint passes with zero
issues (2.808 s). Composed member/enrollment/replica default-race passes
221 entries (55.893 s); command/authentication regressions pass 209 (31.252 s),
without failures or skips. Final original-writer review also rejects a foreign or
explicitly zero carried WriterToken at capture and command entry, while retaining
the exact matching token. Source05 focused race passes 55 entries (22.842 s);
source06 final carried-token regressions pass 29 entries (18.243 s), without
failures/skips. Final source06 configured scoped lint passes with zero issues (10.020 s).
All 2,960 source06 identities match final bookkeeping.

The existing replica server/root routes, original domain output permission pins,
join transport, operator routes and complete HA assembly remain open. This is
not production HA completion. Evidence: `.context/member-request-20261108/`.

### F60b/G11 enrolled replication HTTP mount, November 8

D329 adds NewMemberServer with original Store-issued peer admission before body
acquisition. Mandatory actual read/write deadlines, the earlier request/peer
lifetime and one concurrency slot cover acquisition through explicit response
flush. Exact image/304 responses use the original complete cut and authority.
SQLiteMemberAtomic retains original peer/writer checks around domain authorization,
mutation and durable dedup. The synchronous command callback must keep domain
and dependency pins through commit and emit. WithCommandOutput requires the
actual command completion kind; image receipts cannot acknowledge commands.

Late admission, command-expiry, write or flush failure aborts the actual HTTP
stream. There is no delayed buffered success after the original output scope.
Actual member clients retain unknown completion after an acknowledgment is lost
and preserve one domain effect across an explicit same-command dedup retry.
Early generic refusals expose no authority metadata. The original bound also
covers queueing/body acquisition, and an active flush retains its concurrency slot.

Source01 focused race retained 17 passes/3 failed entries: two image comparisons
included independently sampled image creation timestamps. Source02 compares exact
generation and complete payload while preserving the actual live response;
focused race passes 36 entries without failures/skips (23.041 s). Source03 full
member/enrollment/replica default-race passes 266 entries (68.118 s), no failed or
skipped tests. Accepted framing has a package-only no-test skip. Final command-kind
review adds explicit image-receipt refusal; source04 affected member/command race
passes 146 entries (46.481 s), no failures/skips. Initial lint found context argument
ordering and a test switch style issue; source05 corrects those without changing
admission semantics. Final configured scoped lint exits0 with zero issues (4.612 s).
All 2,962 source05 identities match. Evidence: `.context/member-server-20261108/`.

These are actual HTTP/1 and HTTP/2 library mounts and actual SQLite transactions.
Production listener/domain assembly, operator activation/join-token/promotion,
join transport, forwarding and complete HA/restart/rejoin acceptance remain open.
The test domain is deliberately a transaction fixture, not production RBAC credit.
The legacy error-only-authorizer server is preserved without enrolled-authority
credit. No hosted benchmark, push, deployment or release occurred.

### F60b/G11 authenticated refusal and promotion eligibility, November 8

D330 fixes the member client's classification of an actual authenticated primary
that aborts HTTP before response headers. TLSHandshakeDone records contact only
after the original independently enrolled peer CA verifies the actual connection.
That contact persists within the original exchange; it neither refreshes trust
nor supplies a term, accepted image or command success. ExchangeError preserves
Reached=true on later transport failure, clearing the coordinator's missed-pull
window. POST still returns an unknown outcome and is never automatically replayed.
Failed connections without verified contact remain network-loss evidence.

The new tests first reproduced both missing-contact failures on unchanged client
source (numeric1, two failed tests, 4.209 s). Fixed full replica default-race passes
82 entries without failures/skips (22.242 s), including three repeated actual
TLS/HTTP aborts with no promotion window, one-dispatch unknown writes and a real
failed TCP connection. The actual enrolled server/client image/dedup/committed
revocation integration passes three entries (6.214 s). Final configured scoped
lint exits0 with zero issues (4.611 s). All 2,962 source03 identities match;
original failing receipts remain under `.context/member-contact-20261108/`.
Root operator/domain/listener/join assembly and full HA acceptance remain open.

### G04/G24 hosted security findings, November 8

Hosted security run 42514564273 on 9cc2ae04 failed its source scans. Image,
license-server image and reachable Go vulnerability scans passed. The retained
standalone QUIC module still selected vulnerable upstream dependency versions;
its module pins now match the patched application crypto/net/text selections
and use x/mod 0.41.0. The root go.mod/go.sum and original UPSTREAM.json/license
are unchanged. Upstream development-only workflows, examples and simulator
fixtures follow the existing scan fixture policy. Production QUIC remains
scanned, with narrow annotations for verified parse-on-error, mandatory QUIC
TLS 1.3 and fatal internal panic contracts. Trivy accepts only the explicitly
named, unused upstream fuzz/simulator Dockerfile findings, with reasons.

The exact tracked-file snapshot passes the configured Semgrep scan (exit 0,
57.447 seconds) and Trivy vulnerability/secret/misconfiguration scan (exit 0,
7.452 seconds). The first local Trivy scan's three upstream-fixture findings
remain recorded. Standalone QUIC/HTTP3/handshake races pass; the combined run
still exits 1 because the wire STREAM zero-allocation assertion observes four
allocations under the race detector (1,678 pass entries, two failed entries and
two socket-buffer privilege skips). No upstream assertion was changed or
omitted. The complete ordinary wire suite passes 472 entries, including that
allocation test. Actual application DNS/upstream races pass 455 entries with
four existing opt-in/privilege skips. Evidence, original failed logs and exact
frozen identities are in `.context/security-quic-20261108/`.

Fresh hosted CI 42514564260 also found a BIND ZoneTransferDeny readiness failure;
its actual local reproduction remains failure. The earlier operational-reload
failure did not reproduce in 20 focused race iterations; diagnostics now retain
the underlying error, with no production fix or flake waiver inferred. These
failures and all final implementation/performance/external gates remain open.

### G23 BIND per-zone readiness, November 8

The hosted TestBINDSecondary/ZoneTransferDeny failure reproduces on the actual
local BIND process: the fixture had settled only interop.test, then queried
independently initialized denied.test once. Primary status publication can
retire a zone's earlier source RV until its committed lifecycle aligns. The
existing authenticated status probe now accepts the exact zone name; the deny
case waits for its own current Ready projection, nonzero serving serial and
matching SOA before sending its one transfer. The NOTIFY setup also waits for
the changed zone's matching status/serial within its original 20-second bound.
No transfer, mutation, or refused operation is replayed. The denial assertion
now requires REFUSED and zero records, so SERVFAIL cannot satisfy it.

The unchanged initial reproduction exits 1 (three passes/two failed entries).
The corrected actual BIND run exits 0 with all five entries, zero failures or
skips, in 18.637 seconds. Both receipts/logs and frozen sources are retained in
`.context/security-quic-20261108/{bind-reproduction,bind-ready}.json`. This is
fixture readiness and stronger negative coverage, not a production relaxation;
full hosted CI and final release qualification remain open.

The first broader interoperability race run also exposed initial setup overlap:
BIND's first signed SOA arrived while the other primary's status was still
publishing, received an unsigned refusal and waited its own refresh interval.
That run remains exit 1 (27 passes/two failures/one explicit Knot skip). Initial
fixture setup now settles both independently owned zones inside the original
20-second bound before starting named. The corrected full interoperability race
suite passes 29 entries in 47.298 seconds, with the sole separately enabled Knot
catalog skip; scoped configured lint passes in 6.822 seconds with zero issues.
No production source, transfer retry or comparator assertion was weakened.

### G04/G21 shared-settings ownership and API fixtures, November 8

Hosted shard 2 on 9cc2ae04 exposed two independent regressions. Upstream
ownership serialized node-only managed topology/edge identities into the shared
section; the backend correctly refused them. Shared persistence/export now
filters every non-public upstream schema key while preserving effective public
values. Original node declarations and schema defaults survive replacement even
when stale shared sections are malformed or carry foreign host fields. Invalid
original host declarations still refuse. Actual file-backed release/adopt and
two-node restart retain independent direct/edge topology and deployment identity.

The audit schema-5 fixture now removes only the exact fresh schema-9 enrollment
sentinel while the database is closed, just as it already removed the later
anchor sentinels. Current production migration admission is unchanged.

The first composed race run remains failed/incomplete: 818 test passes, one
settings failure, one PostgreSQL skip, then a diagnostic SIGQUIT of the owned API
test after its flush-cancellation fixture blocked on an unbounded channel receive.
The captured stack confirms query_output_test.go:376. The mocked flush previously
returned on context cancellation before its cancellation callback necessarily
ran; stopping that callback after I/O returned is legal. The fixture now keeps
the simulated transport pending until the actual deadline interrupt, with bounded
observation waits. Production output behavior is unchanged.

Final source03 focused race passes 16 entries without failures/skips. Full
settings/config/API race passes 1,037 entries, no failures, in 194.904 seconds,
with PostgreSQL enabled (exact local image retained in the receipt). Its sole
Kubernetes asset skip is subsequently executed with actual 1.37.0 control-plane
assets: one race pass, no skips, 10.822 seconds. Every frozen identity matches.
An initial lint invocation could not find the executable on PATH; the installed
lint then found one test-owned file-read annotation. Those failed receipts remain.
Evidence: `.context/settings-scope-20261108/`. Hosted final-head qualification
and all unrelated feature/release gates remain open.

Final source04 configured scoped lint passes with zero issues in 3.208 seconds.
Source04 changes from the race-qualified source03 are documentation and the
exact temporary-file annotation only; no behavioral change followed the races.

### F63/G23 actual selected-runner recording, November 8

Hosted run 42514599389 measured exact candidate 9cc2ae0423070fc514c94a25ab5b92e3816889e7.
The job API confirms ubicloud-standard-8-ubuntu-2404 and actual runner
`gr4410z6aj5j3qnxa4v3979221`; native headers identify AMD EPYC 9454P.
Go 1.27.3 and original GOMAXPROCS=6 were retained. All 40 native measurements
and seven shape/absolute checks exit zero, with every process group reaped and
no resource stop. Seven combined hashes match result.json. Independent bounded
review finds 136 cases, 592 metric keys and 2,960 values, exactly five per key.

Median unchanged-1M compilation is 53,321,245 ns/op with eight allocations;
suffix-1M p99 hit/miss is 961/600 ns. Exact-1M miss p50 is 170 ns. CNAME p99 is
1,722 ns with one allocation. Steady 3M RSS is 94.24 MiB, separately 143.7 MiB
peak, with 1.963 s compile. The original absolute limits pass. Recording reports
measurement_complete=true and release_qualified=false. It does not erase the
prior 158 ns versus 109 ns exact-miss failure or any earlier failed sample.

The retained Intel Xeon baseline is a different class. To obtain an actual
reference on Ubicloud without making the candidate its own baseline, the new
explicit reference workflow pins 0cc9565e9a878f8223fc1cee5201eb7eca8aacd2, the
last original baseline source commit. Blocking/zone benchmark executable bodies
match except comments; original cache/strategy cases are retained. Separate
clean source/control checkouts and all identities surround twenty fresh native
commands and three original shape/absolute checks. Failures remain numeric and
no sample is replaced. Eight combined Python control tests pass, including
native/gate failure retention, changed-source/resource stops and wrong-reference
refusal. Both changed workflow YAML files parse with the existing YAML dependency.
This reference workflow has not yet run; no baseline or limit is changed.

Same-class reference comparison, new cache/strategy absolute calibration and
final-current-source qualification remain open. Complete downloaded artifacts,
independent vectors/review, original inputs and control-test evidence are under
`.context/hosted-release-20261108/`. Original failures and external release gates
remain unchanged.

### G23 current-source actual Knot, November 8

The pinned Knot DNS 3.6.2 executable (image digest 174b0fcb24c6c3bc5065c9d1ce46b588eec607a5f32bc40ff0cda60150f00bd9)
passes the actual current-checkout catalog/NOTIFY/TSIG acceptance under race:
nine test pass entries, no failures/skips, 31.855 seconds. Signed transfer and
query/unsigned/wrong-key/unknown-key/policy/source refusals plus mounted doctor
checks execute. All 4,638 frozen identities match; the test removes only its own
private-daemon containers/network. No residual fixture container remains. Earlier
Knot skips and private-daemon setup/pull failures remain recorded. Evidence:
`.context/knot-current-20261108/`. No production release or deployment occurred.

### G04 integration promotion fixture cleanup and publication, November 8

The preserved old hosted shard 1 log reveals an actual 30-minute timeout in
TestIntegrationRuntimeActualReplicaPromotionFencesHeldObservation. Its observation
assertion failed, then LIFO cleanup joined the intentionally blocked provider
before releasing its barrier. Fixture barrier releases now run as defers before
runtime cleanups, including the independent replacement case. Failure diagnostics
report primary/follower presence and replica poll count without private data.
The original assertion did not reproduce in ten cleanup-corrected local race
iterations; this does not establish its cause or waive the hosted failure.

The fixture also waited for provider Ready without first requiring its complete
inventory publication. It now retains the original published observation before
the single held Sync and compares the follower's timestamps to that same value.
No refused operation is retried, no source proof is replaced, and no production
behavior or original assertion is relaxed. All 42 integration race entries pass
without failures/skips in 221.764 seconds; configured command-package lint passes
with zero issues in 28.047 seconds. Frozen identities match. Evidence:
`.context/integration-failover-20261108/`. Hosted run 42514564260 was canceled by
normal PR concurrency after pushing 86a3ff91; its prior e2e/shard2 failures and
shard1 timeout/log are retained. The fresh run remains independently required.

### F63/G23 immutable reference result and heterogeneous CPU pool, November 8

Actual reference run 42517712186 measured exact original 0cc9565e under controls
86a3ff91. All twenty native commands exit zero; all groups reap, no resource stop,
366.648 seconds total native wall, minimum free 138,759,421,952 bytes. All three
report hashes verify. Five samples supply 45 cases and 219 metric keys. The
reference's original blocking/zone gate exits 1: old CNAME p99 median 2,090 ns
exceeds the unchanged 2,000 ns ceiling. Its five values are 2,090, 2,280, 2,230,
1,950 and 1,730 ns. Cache and strategy shape checks exit zero. The aggregate
correctly retains exit1, measurement_complete=true and release_qualified=false.

Native headers identify AMD EPYC 9224, whereas candidate run 42514599389 used
AMD EPYC 9454P. Both requested the exact owner-selected Ubicloud eight-core
Ubuntu 24.04 label and retained GOMAXPROCS6. That pool label therefore does not
pin a CPU model across jobs. These two runs cannot satisfy the strict CPU-match
comparison. Reference exact-miss p50 median190ns is recorded but cannot clear
the prior candidate/local regression. No sample or baseline is replaced.

Next measurement design must place original reference and candidate in one VM,
retaining both source manifests, every failed reference limit and the original
strict comparison. It must not select replacement samples until a desired CPU
or result appears. New absolute calibration and final candidate qualification
remain open. Complete reference artifacts and failure evidence are retained
under `.context/hosted-release-20261108/reference-artifact/`.


### F63/G23 paired runner control, November 8

The selected Ubicloud pool supplied different CPUs to the candidate and original
reference, and the reference independently failed its CNAME ceiling. The new
paired workflow places both existing collectors sequentially in one VM: original
0cc9565e remains immutable, candidate source is exact, all sixty native process
receipts and original gates remain mandatory. A failed reference is ineligible
for comparison and keeps the overall result failed; the independent candidate
recording does not clear it. Eligible original scopes use the unchanged strict
Go comparison. No baseline, threshold, corpus or sample replacement is introduced.
Twelve control tests and workflow YAML validation pass. Hosted paired execution
and new absolute calibration remain pending; this is not release qualification.


### F08 Kubernetes doctor egress diagnostic, November 8

The missing Kubernetes self-NOTIFY mount first fails in the actual API-server
fixture (native exit1, 26.444s); that original result is retained. D331 now mounts
the diagnostic with the same complete immutable TSIG startup inventory used by
all six listeners. It checks the original verified packet/key identity and
finite deadline, reports only the observed UDP source, and grants no writer,
leader or zone-readiness authority. Unknown/wrong-owner sources cannot fall back
to file keys. Other NOTIFY traffic retains its original receiver.

Actual Kubernetes stored/file key round trips and restart, offline/wrong-owner
startup, unsigned/wrong-key/TCP/malformed refusals, plus all original SQLite
Doctor tests pass under default race: 35 entries, zero failures/skips, 39.065s.
All frozen source identities match. Scoped command lint exits0 with zero issues
in 27.649s. Evidence is `.context/doctor-kube-20261108/`. This closes the named
Kubernetes diagnostic gap; TSIG hot management, HA assembly and remaining final
release gates remain open.


### F21c original cache/strategy calibration, November 8

The selected-runner historical reference supplies five fresh values for all20
cache and60 strategy metrics. Separate committed ceilings use each original
median plus max(20%, original noise floor); reports, process receipts, hashes and
all vectors are retained in bench/calibration/ubicloud8-20261108. All thirteen
control tests pass. Collectors now pin and enforce those files alongside unchanged
blocking/zone limits. No baseline, old threshold, ignore or workload is changed.

Actual Go absolute checks of the retained reference cache and strategy reports
exit0. The full reference's old CNAME failure remains1 and cannot establish a
paired baseline. Retained candidate42514599389 fails both new scopes: five cache
limits (parallel hit168.6ns, Set288B/op, memory551258352B/op,468.7bytes/entry,
349.8overhead-bytes/entry) and three strategy limits (fast/strict126398ns,
slow-first/rtt_weighted259747ns and177us p50). These are honest absolute failures,
not a CPU-matched relative comparison; current-source optimization/qualification
remains open. No benchmark was rerun to select or replace samples. Evidence:
.context/cache-strategy-calibration-20261108/.


### F21c cache provenance allocation, November 8

Original cache calibration exposed160 bytes of empty inline provenance per
entry. Cache entries now omit only exactly-zero metadata; complete nonzero
scope/security/upstream metadata shares one allocation with its entry. Restored
scope changes replace immutable metadata so retained dump snapshots cannot mix
old keys with a new scope. Original lifetimes, original fields, version2 wire
format, admission and workloads are unchanged. Both insertion forms have an
actual one-allocation guard; returned values do not alias owners. A deterministic
blocked actual dump plus concurrent restore rebind preserves all32 original rows.

Full cache/persistence/cache-stage/upstream-stage default races pass102 entries
without failures/skips (7.413s), followed by configured scoped lint exit0/zero
issues (2.812s). All source01 identities match. One unchanged original-cache
1s diagnostic on this sandbox exits0 (26.842s): Set128B/1alloc, memory308.7bytes
and189.8overhead-bytes per entry; all Get/Growth lookup cases0alloc. Parallel
hit163.6ns remains above the new156.48ns ceiling; no local timing or one-process
result is fixed-runner or five-process qualification. Original hosted cache and
strategy failures remain open pending actual new-source measurement. Evidence:
.context/cache-metadata-20261108/.


### F18c/F26d/F28 held DNS across canonical commits, November 8

D332 distinguishes retained recursive/policy responses from data-generation
currency. Actual query-log reload and UniFi held queries originally failed in
all three local repetitions (six failures, native exit1, 123.201s). The root
component now retains the typed original runtime dependencies and actual anchor
input for ordinary forwarded/policy responses. Authoritative/local/mixed output,
API dig and native resolver mutations retain full generation checks; the original
ordinary-query-after-UPDATE refusal test is unchanged and passes. Empty unsynced
SERVFAIL can report unavailable local data without answer/delegation/proof rows.
Raw-cache work consumes but skips an old-generation mutation, never an anchor
retirement refusal. Original query lifetimes, mode/retirement holds and all
expected policy/address values remain unchanged.

Earlier drafts and failures remain: the first fixed run still failed UniFi;
source04 broader races had87 passes, one expired-record output failure and one
explicit live-edge skip. Source05 had12 passes and a restoration observation
failure. A diagnostic-only richer assertion run passed and did not establish
that failure's cause. The final fixture checks the exact successful TCP
allocation/restoration observation rather than discarding a converged UDP result
and reacquiring another cut during continuous polling; it still asserts every
original exact address and bound. No held request is retried or renewed.

Final source06 mounted default races pass44 entries without failures/skips
(267.998s), including both original held cases, full UniFi lifecycle, native
six-transport bootstrap/revocation/restart, actual pinned dnsdist edge restart,
root cache/output retirement and original UPDATE refusal. Configured command
and SQLite lint exits0 with zero issues (32.255s); frozen source identities match.
Earlier root39 and broader87 passing entries remain scoped evidence, not a full
repository green claim. Evidence: .context/held-fixtures-20261108/.
