# v0.2.0 follow-up acceptance

PR [#54](https://github.com/robertherbaugh/kudurru/pull/54) merged as
`04ff78aa` on November 15, 2026. That integration milestone does not publish
v0.2.0 or accept every feature package. The release remains **on hold** until
the open gates below have evidence and the required review decision. No release
tag, production deployment or publication is authorized by this report.

This report reconciles the package snapshot at `7663c957` with the later
[acceptance audit](followup-audit.md#final-integration-resumed-november-15-2026-d397-d399-d405d409),
[local release report](local-release-acceptance.md) and current implementation.
Historical failed runs remain failed. A later source pass supersedes only the
specific finding it exercises, not a package's complete acceptance criteria.

## Current work and gates

| Gate | Current evidence and remaining requirement |
|---|---|
| TSIG observers | Thirty race repetitions reproduced both historical failures: the expected credential/deadline errors and committed image were correct, while the publisher notification channel closed. The fixture issued a redundant Wake and included an unrelated retrying provider. The follow-up removes those notification sources only; expiry, original cancellation/deadline, quiet channels, publication, writer eligibility, audit and durable-image assertions remain. Final repeated results and hosted CI are recorded below. |
| #94 | Implemented per-group `cache: true`, default off for non-default groups. Default group caching and the global cache switch retain their existing precedence. The route plan is bound before lookup and partitions by group, routing and trust configuration. Covers conditional suffixes, forward-zone/Policy hints, TTL bounds/countdown, stale/prefetch, reload flushes and cache inspection. The dashboard saves the toggle immediately. Focused Go race suites, dashboard type checking, 40 upstream tests, lint and bundle build pass. |
| Packaged upgrade | Released v0.1.0 Linux archive and RPM downloaded and checked against published SHA256 checksums. The existing upgrade test now takes a stopped-volume backup and restores it under the released old binary, checking DNS, credentials, schemas and history. Candidate package execution and exact hashes/results are recorded below. |
| Performance | Earlier approval and pass bind runtime `5e19a32b`, not this source. Calibration manifest `3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266` remains immutable. This sandbox has a new VM/boot/CPU identity. On November 16 the owner approved the disclosed host-continuity amendment, retaining all limits and historical calibration. Qualification uses a new guarded host record and fresh same-host original-reference/candidate interleaving; its exact outcome is recorded below. |
| Hosted checks | PR #54's last reported source run `43770178234` failed the two observer tests and `ci-ok`; its other 23 jobs passed. Follow-up run `43775954528` passes both observer shards but exposed the schema-contract, cleanup-setup, UPDATE EOF and rollback test failures recorded below. Required checks must pass on the corrected source head. |
| Manual UI and production license | Earlier HTTP/browser and synthetic-license proofs retain their exact scope. They do not establish human visual approval or production entitlement issuance/signing. Final-artifact browser/deployment checks below have their own scope. Remaining owner checks are tracked in [#133](https://github.com/robertherbaugh/kudurru/issues/133). |
| Review and publication | Required CI/security/review checks govern merge. Publishing v0.2.0 is a separate release-branch milestone. |

## Historical findings reconciled

These entries are closed as allegations of missing implementation or missing
specific proof; residual acceptance work stays in the next table.

| Package | Superseded historical statement | Current evidence |
|---|---|---|
| ZONE / HA / UPDATE | Command recapture and starvation fixes were reverted; follower applies are always cut by their own pull; UPDATE receipts impose a permanent 256-commit limit | D399, D405–D407, D409 and the final-integration audit identify the integrated fixes and mounted/process proofs. Receipts expire under the original signature deadline and bound simultaneously live receipts. Original lifetime and post-admission failure rules remain. |
| DNS | Managed-root writer ordering remains unexplained | D411 records the elected startup ordering correction and preserved acquisition/readiness assertions. Its runtime and final performance gates remain distinct. |
| RPZ | Kubernetes startup can capture empty policy and forward a listed name during restart | D410 captures policy/configuration coherently and rejects an unsynced/missing index; deterministic negative controls and mounted restart race checks are recorded. |
| IDENTITY / INTEGRATIONS | The inventory regression and observation scheduling have no integrated fix | D397/D408 and final integration record the lease MAC gate, start-paced observations, status refresh and post-publication integration startup, with 20 mounted race runs. Sustained CPU starvation remains a finite-budget limitation. |
| IAM | Roughly 4,000 revocations exhaust permanent history; TLS and serving-follower revocation proofs are absent | D413 replaces permanent-history exhaustion with checkpoint compaction. D414 and [IAM live acceptance](iam-live-acceptance.md) supply mounted TLS, original credential revocation, follower and live Authentik proofs; the later local report adds actual member-mTLS process replication. Do not reopen the obsolete 4,000-cycle limit. |
| Dashboard / OPS | Views, inventory, IAM administration, RPZ and integration pages are missing; Lists polls every pending feed at 3 seconds | DASH-OPEN2-FINISH is integrated; RecordEditor retains served-view namespaces and RPZ polling uses `rpzPollInterval`. Later audit evidence records full dashboard tests. Human visual acceptance stays separate. |
| Build / OPS | No full release snapshot or green source CI has ever completed | The local report records all 20 release steps, four platforms, five archives, six packages and both local images on `5e19a32b`; CI `43745021235` passed on that exact runtime. Neither result transfers automatically to this follow-up. |

The primary compiler checks dynamic ownership before adding endpoints
(`internal/primaryzone/compiler.go`), including view endpoints; the editor keeps
`initial.namespace` for served view rows (`RecordEditor.tsx`). Those historical
claims are no longer treated as confirmed current defects. Namespace, ownership,
transfer privacy and driver differences must still retain their existing tests.

## Remaining package findings

Every link below is a concrete follow-up with acceptance criteria. Filing an
issue does not defer a v0.2 requirement or accept a limitation. The owner must
resolve or explicitly disposition the applicable gates before publication.
Historical low-severity notes not independently reproduced retain their original
status in the audit, rather than becoming invented current failures.

| Package / remaining scope | Follow-up |
|---|---|
| ZONE: enabled external-source RBAC failures and caps; TSIG CLI on Kubernetes | [#123](https://github.com/robertherbaugh/kudurru/issues/123), [#126](https://github.com/robertherbaugh/kudurru/issues/126) |
| ZONE: view ownership parity, shared caps, transfer/signing limitations | [#136](https://github.com/robertherbaugh/kudurru/issues/136) |
| ZONE / DNS / EDGE: hosted pinned interop, signed SOA-only IXFR without AA, dnsdist 2.1.3 empty-FIN limitation | [#127](https://github.com/robertherbaugh/kudurru/issues/127) |
| UPDATE / HA: bounded failures after admission, long apply partial outcomes, member retirement and D395 ordinary-answer grace decision | [#129](https://github.com/robertherbaugh/kudurru/issues/129); D409 remains implemented |
| DNS: final performance qualification; managed-anchor residual metric/refusal/abandoned-term coverage | Performance gate above; [#135](https://github.com/robertherbaugh/kudurru/issues/135) tracks residual F28 coverage |
| RPZ / OPS: explanation projection, offline signed-feed restart, compile window and multi-port CoreDNS loss reporting | [#130](https://github.com/robertherbaugh/kudurru/issues/130) |
| INTEGRATIONS: whole-image capture/cleanup scale and original time bounds | [#124](https://github.com/robertherbaugh/kudurru/issues/124) |
| INTEGRATIONS / IDENTITY: partial UniFi update/delete recovery, actual live providers and unexplained partial-create stall | [#125](https://github.com/robertherbaugh/kudurru/issues/125) |
| INTEGRATIONS: Kubernetes K3/K7, limits/refusals, credential and lease-loss qualification | [#126](https://github.com/robertherbaugh/kudurru/issues/126) |
| INTEGRATIONS: webhook retention, mixed-version event cursor, budget/audit coverage and deferred event topics | [#132](https://github.com/robertherbaugh/kudurru/issues/132) |
| IAM: documented Kubernetes workflow refusals, restart-bound settings; remaining human/production-license approval | [#126](https://github.com/robertherbaugh/kudurru/issues/126), [#133](https://github.com/robertherbaugh/kudurru/issues/133) |
| IAM / HA: residual self-command error classification and mounted follower audit | [#137](https://github.com/robertherbaugh/kudurru/issues/137) |
| Runtime: loaded readiness/long-apply/refusal attribution and term-end diagnostics | [#138](https://github.com/robertherbaugh/kudurru/issues/138) |
| RBAC: precise permission-scanner helper allowlist and negative controls | [#139](https://github.com/robertherbaugh/kudurru/issues/139) |
| OBS: hour-boundary summaries; Events, tracing and live sink final-source qualification | [#122](https://github.com/robertherbaugh/kudurru/issues/122), [#131](https://github.com/robertherbaugh/kudurru/issues/131) |
| EDGE: restore-hold continuity and temporarily unreachable backend behavior | [#128](https://github.com/robertherbaugh/kudurru/issues/128) |
| OPS: manual UI, production entitlement/branding, package/deployment and release protection checks | [#133](https://github.com/robertherbaugh/kudurru/issues/133), [#140](https://github.com/robertherbaugh/kudurru/issues/140) |

## Explicit future acceptance

AD/Windows production acceptance remains future work under **D353**. The
isolated GSS-TSIG probe is not production AD DNS replacement. Independent
**two-VM VIP/failover** acceptance remains future work under **D358**. Local
containers, a single VM, envtest and KIND/MetalLB cannot close that requirement.

## Final execution record

The local evidence directory is `.context/`; the identities, outcomes and
limitations below are the portable record. The follow-up is
[PR #134](https://github.com/robertherbaugh/kudurru/pull/134). A fresh checkout
cannot rely on local logs. No GA tag or release was published.

### TSIG observer verification

Thirty race repetitions of the original source reproduced the quiet-window
failures (`tsig-before.log`, 155.366s). Removing the unrelated provider retries
and redundant publisher wake eliminated those particular failures in the first
post-fix repetition, but that run still **failed four deadline setup cases**
while cold cross-platform builds and lint competed for the sandbox
(`tsig-after.log`, 465.992s). Those results remain failed and retained.

After every unrelated local build/test finished, the final repetition **passed
30 repetitions of each expiry, cancellation and original-deadline case** in 185.696 seconds:

```sh
CGO_ENABLED=1 GOTOOLCHAIN=go1.27.4 go test -race -v ./cmd/kudurru \
  -run '^TestServingTSIGObserver' -count=30 -timeout=10m
```

The four-second parent bound, one-second earlier deadline established before
authentication/commit, observation budget, quiet publication/authority channels,
original lifetime, committed-image/audit checks and writer-eligibility assertions
are unchanged. No production observer code changed. Evidence:
`tsig-final-quiet.log`. Hosted checks still govern merge on the final PR head.

### Additional hosted findings and source corrections

Run [43775954528](https://github.com/robertherbaugh/kudurru/actions/runs/43775954528)
on `ed293dca` completed with **22 source jobs passed** and four failed jobs:
packages, command shard 4/4, e2e and `ci-ok`. Both original observer shards
(1/4 and 2/4) passed. The failed run is retained; it is not green CI.

- `TestSettingsSchemaMetadata` still expected five upstream-group fields.
  It now checks all six, including the new boolean cache field's false default
  and help, while retaining the existing field/option assertions. Its focused
  race check passes.
- The cleanup fixture's one-second resync and repeated budgeted provider Check
  preflight prevented setup convergence before its direct-store delete
  challenge. This reproduced three times locally. Setup now observes the
  integration's Ready phase and protected serving publication without
  dispatching another command. All original cleanup bounds, purge fencing,
  late-delivery/finalizer refusal and no-provider-request assertions remain;
  **20 race repetitions pass in 179.898s**. This does not accept command
  starvation under sustained writes; #124 retains that separate scope.
- The 1,100-update receipt-ledger test received 1,099 NOERROR replies and one
  EOF. Native mutation output deliberately retains full generation currency
  and closes TCP when the original response cannot be emitted
  (`internal/dnsserver/native_capture.go`). Its idempotent bulk replacements
  already retry ambiguous SERVFAIL; they now count and retry specifically EOF
  under the same original three-minute change deadline. EOF never counts as
  success; all 1,100 actual NOERROR replies and the original exact replay,
  signature expiry and restart assertions remain required. Other transport
  errors and terminal DNS errors still fail. The full local/hosted outcome
  remains to be recorded.
- Hosted e2e failed only the new rollback's immediate v0.1.0 DNS probe, already
  corrected by the convergence check and successful packaged run above.

Evidence: `ci-ed293dca-packages.log`, `ci-ed293dca-commands-4.log`,
`ci-ed293dca-e2e.log`, `settings-schema-cache.log`, `cleanup-before.log`,
`cleanup-after.log` and `update-ledger-after.log`. The cleanup reproduction
was stopped after three failures; it was not a completed 20-repeat run.

### Final runtime candidate

GoReleaser 2.18.3 completed the full snapshot in 13m43s with Go 1.27.4,
`CGO_ENABLED=0`, Syft 1.54.1 and Helm 4.3.0. The command was:

```sh
GORELEASER_CURRENT_TAG=v0.2.0-rc.1 GOTOOLCHAIN=go1.27.4 GOMAXPROCS=4 \
  goreleaser release --snapshot --clean --skip=publish,sign --parallelism 2
```

The local candidate is `0.2.0-rc.1-SNAPSHOT-d1725c59`, declared runtime source
`d1725c592be0ecf5857f3d8013b5287a318f61a9`. Later changes in this PR are tests
and documentation; `cmd/` production code, `internal/`, `ee/`, `api/`, module
files and `dashboard/src/` retain that runtime. The build ran with documentation
edits and the dashboard build hook's generated bundle in the working tree;
this is an unsigned local snapshot, not a clean signed release artifact.

All four Linux/macOS amd64/arm64 binaries have CGO disabled. Five archives,
six packages and their SBOMs passed all **22 artifact checksum checks**. Both
local ko images reported the candidate version and were removed after the
snapshot check. The Linux amd64 executable is 85,688,480 bytes.

| Artifact | SHA256 |
|---|---|
| Published v0.1.0 Linux amd64 archive | `6628e2bbec9b64c06ae13f5e19ec0585f5475904bdef7d8222591b10a50ecdb1` |
| Published v0.1.0 Linux amd64 executable | `e4e73e53cf7bd965ad1e5eb3f66b9bf450ab9e4e81f25efb9aa93db8b510512a` |
| Published v0.1.0 x86_64 RPM | `ddd5aec37db4a17050d1c0f5629a6566012e2eabe6a88b0ba0229addfba06437` |
| Candidate Linux amd64 archive | `5a869018fa270e701ec353454d63104fbecfd8ba78b282cd8768865cc0a6db36` |
| Candidate Linux amd64 executable | `b2207adfdb849dab8f26ec2c0544d1a10a9b9fbb274b62c1e22ac9cf4ecb2cfe` |
| Candidate x86_64 RPM | `cbbb65123344813651c7bfe61735862e6e540c1c054d98a96eb238ea4ededc9f` |

The published v0.1.0 archive/RPM were checked against that release's published
checksums before extraction. Local logs `final-snapshot-2.log`,
`final-snapshot-images.log` and `final-artifacts-inspection.json` retain the
complete command output and every artifact hash. The first snapshot stopped
because Helm was absent; its failed log is retained separately.

### Packaged upgrade and rollback

The packaged suite uses the checksum-verified **extracted candidate archive**
and the **published v0.1.0 executable**, with previous-release, PostgreSQL and
envtest requirements mandatory. PostgreSQL 16 is pinned to
`sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea`;
envtest uses Kubernetes 1.37.0. All state is disposable.

The first run passed the existing upgrade, restart, Kubernetes IAM migration,
mixed-version, managed-anchor and newer-schema refusal assertions. Its new
rollback assertion probed DNS immediately after v0.1.0 readiness, before the
restored policy compilation completed. That run remains failed. The test now
uses the same bounded 30-second convergence check as its existing upgrade and
restart paths, comparing every original DNS answer, response code and AA bit
over UDP and TCP. It keeps the original schemas, three credential identities,
query-history prefix and audit-history assertions.

The corrected packaged run **passed all nine tests in 185.446 seconds**:

- `TestAnchorUpgradeFromReleasedV010` and `TestAnchorUpgradeStaticTwinV010`.
- `TestMixedVersionStaticPeerV01` and `TestMixedVersionCentralDatabaseV01`.
- `TestBuiltBinaryRefusesNewerAuditSpool` and `TestBuiltBinaryRefusesNewerDatabases`.
- `TestKubeUpgradeFromV01`, including offline IAM migration, 12 unchanged
  answers and verification of the original AuditLog batch.
- `TestUpgradeFromV01DataDirectory`, including its two candidate restarts,
  refused downgrade and `BackupRestoresReleasedBinaryForRollback`.
- `TestUpgradeFromV01RefusesLegacyObjects`.

Evidence is `packaged-upgrade.log`, `packaged-upgrade-plan.json` and
`packaged-upgrade-result.json`; the first failed run is retained as
`packaged-upgrade-first.log`. The disposable PostgreSQL container/volume and
its credential files were removed after completion.

An isolated RPM root/database installation of the published v0.1.0 RPM followed
by the candidate RPM **passed**: edited configuration and environment files
were preserved, the installed executable matched the archive, the systemd unit
was present and the reported version was correct. Maintainer scripts/triggers
were disabled, and no systemd lifecycle was run. Process restarts are exercised
separately; this RPM check does not accept maintainer scripts or host service
management. Evidence: `rpm-rehearsal-result.json` and `rpm-rehearsal.log`.

The audit API upgrade subtests use a separately built `licensetest` executable
with a synthetic trusted key. The normal candidate proves data migration and
persistence; the synthetic fixture additionally verifies the chained audit API.
Neither is production entitlement issuance/signing acceptance.

### Browser, deployment and live services

Real Chrome exercised the dashboard embedded in the normal candidate binary:
non-default cache opt-in saves immediately, the default-group checkbox is
checked/disabled, conditional DNS answers remain correct, and a process restart
retains the saved setting, original session and DNS. The no-key node reports
Community, locks the licensed history page and refuses a second Policy with
HTTP 402. All checks passed with no uncaught browser exceptions. The binary was
then confirmed byte-identical to the extracted archive. Screenshots
`upstream-cache-artifact.png` and `community-license-artifact.png`, plus
`artifact-browser-result.json`, retain the agent's browser/visual evidence.
Human visual approval and an actual production key remain open in #133.

Both mandatory live sink tests passed on final source:
`TestLiveClickHouseReadsTheDocumentedTable` and
`TestLiveLokiServesTheDocumentedMapping`. Servers were ClickHouse 25.8
(`sha256:0152dd511befe6a2c2ef53e930726179669b08116da78500b37c51c96ff5ee77`)
and Loki 3.7.9
(`sha256:0db216b630ae1f348a1584c67f3cead8b4c1fc839169a39501db664744158e28`),
with `KUDURRU_REQUIRE_SINK_SERVERS=1`; both disposable containers were removed.
This closes those two final-source live-server checks within #131, not all
Events/tracing acceptance.

Live final-source recursion and IANA anchor checks also passed, including
managed bootstrap against all 13 reachable IPv4 root endpoints (IPv6 was
unreachable). Seed 38696 authenticated all 13; seed 20326 authenticated none.
The **normal packaged candidate** passed the live recursive-node script:
validated `example.com` with AD set, DNSSEC-bogus SERVFAIL with EDE 6, and
checking-disabled resolution, with 18 outgoing queries and zero timeouts.
Evidence: `final-live-recursion.log`, `final-live-anchor-bootstrap.log` and
`final-live-recursive-artifact.log`. The initial script preflight stopped
because `dig` was missing; its log remains separate.

### Release protection audit

Read-only GitHub ruleset and effective branch-rule API checks found active
rulesets `25103326` (default branch) and `24898698` (`release*`). Both require
`ci-ok`, `security-ok` and one approving code-owner review. Release branches
also require last-push approval, stale-review dismissal and resolved threads.
PR #134 still requires review.

The release ruleset allows `merge` **and `rebase`** and omits the `promotion`
check, contrary to the release checklist and promotion workflow. This current
settings gap is tracked specifically in
[#140](https://github.com/robertherbaugh/kudurru/issues/140). The owner-role
bypass is exempt on the default branch and restricted to PRs on release
branches. Classic branch-protection endpoints returned HTTP 403; the audit
cannot exclude additional classic constraints. No repository settings were
changed and no promotion was attempted. Local evidence:
`release-protection-review.json`, both `*-ruleset.json` files and the three
`effective-*-rules.json` files.

### Source checks and pending decisions

Focused upstream/cache/configuration race suites, cache inspection, dashboard
type checking, 40 upstream dashboard tests, lint, bundle build, Go vet,
generated-file/validator drift, version synchronization and all 168
documentation link checks passed. The benchmark protocol's 32 controls passed;
they do not measure or approve candidate performance.

An actual attempted preflight with the historical host correctly refused
`dedicated host identity changed` **before any timing or output directory**.
The immutable calibration and original approval are retained locally, alongside
the original pending proposal. On November 16, 2026, Robert Herbaugh replied
"Approve this host-continuity amendment" to that exact proposal. The replacement
host and transition records bind this decision and both host identities before
the fresh qualification. Its results must pass before D383/D412 can close.
The first guarded sequence on `7a642715` completed 20 of 60 native processes,
all with exit zero, before the hosted failures required final-source test
corrections. Applying the diagnosed API schema fix caused the source guard to
hold the next measurement: `reference and control checkouts must be clean`.
The incomplete record and explicit source-correction hold are retained under
`.context/performance-final-20261116/`. It supplies no qualification pass and
no samples for the fresh complete sequence on the corrected source. Limits,
calibration, host and approval are unchanged. Full results belong in a small
follow-up documentation PR based on #134; this keeps source CI tied to its
actual code head while preserving independently reviewable evidence.

Production-license/human checks, issue dispositions and required PR review
remain separate decisions. D353 and D358 retain exactly the future scope above.
