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
