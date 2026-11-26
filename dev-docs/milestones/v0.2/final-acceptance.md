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
| Performance | **Final-source qualification failed** on `a22255a2`: 60 native processes succeeded, but all ten absolute report gates failed. Original-reference / candidate CNAME lookup p99 medians are 2,187 / 2,363 ns against the unchanged 2,000 ns product maximum. The reference is ineligible, so relative comparisons were refused. The owner-approved restarted-host amendment, calibration and limits were honored. Complete evidence below; release-blocking [#143](https://github.com/robertherbaugh/kudurru/issues/143). |
| Hosted checks | PR #54's last reported source run `43770178234` failed the two observer tests and `ci-ok`; its other 23 jobs passed. Follow-up run `43775954528` passes both observer shards but exposed the schema-contract, cleanup-setup, UPDATE EOF and rollback test failures recorded below. **All 26 jobs passed** in final-source run `43781940600` on `a22255a2`, including both observer shards, the corrected ledger shard, e2e and `ci-ok`. `security-ok` also passed. Required review still governs merge. |
| Manual UI and production license | The normal candidate archive executable accepted the owner-supplied issued Enterprise key through its production trust root. All eight entitlement, branding, restart, tampered-signature, minute-reload and no-key fallback checks passed on that candidate. Human visual approval on the exact final artifacts, evidence of authorized production issuance, and an expiry/grace acceptance record remain in [#133](https://github.com/robertherbaugh/kudurru/issues/133). |
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

The non-required hosted pinned interop workflow passed in [run 44803939665](https://github.com/robertherbaugh/kudurru/actions/runs/44803939665)
at checked-out commit `ee08383e139ddd7c0be4ef848321066607fb53bd`. Its two
artifacts retain the exact image identities, versions, and unskipped results
for Knot DNS 3.6.2 (catalog, inbound secondary, managed root and RFC 5011)
and dnsdist 2.1.3 (all live suites, including client IDs, cache isolation and
managed anchors). The pass does not remove the dnsdist empty-FIN limitation
or qualify a release.

D417 ([#158](https://github.com/robertherbaugh/kudurru/issues/158)) keeps strict
IXFR validation: a signed, same-serial, single-SOA reply without AA is refused
without changing the stored copy or renewing its confirmation.

| Package / remaining scope | Follow-up |
|---|---|
| ZONE: Kubernetes workflow and runtime qualification | [#126](https://github.com/robertherbaugh/kudurru/issues/126) |
| ZONE: view ownership parity, shared caps, transfer/signing limitations | [#136](https://github.com/robertherbaugh/kudurru/issues/136) |
| DNS / EDGE: dnsdist 2.1.3 empty-FIN limitation | [#127](https://github.com/robertherbaugh/kudurru/issues/127) |
| UPDATE / HA: owner-accepted publication grace, bounded follower apply, and member replacement | [#129 disposition and evidence](#owner-disposition-for-129); [#159](https://github.com/robertherbaugh/kudurru/issues/159) tracks the lifetime-refusal primary name. D409 remains implemented; loaded runtime and UPDATE findings remain in [#138](https://github.com/robertherbaugh/kudurru/issues/138). |
| DNS: failed final performance qualification; managed-anchor residual metric/refusal/abandoned-term coverage | [#143](https://github.com/robertherbaugh/kudurru/issues/143); [#135](https://github.com/robertherbaugh/kudurru/issues/135) tracks residual F28 coverage |
| INTEGRATIONS: whole-image capture/cleanup scale and original time bounds | [#124](https://github.com/robertherbaugh/kudurru/issues/124) |
| INTEGRATIONS / IDENTITY: partial UniFi update/delete recovery, actual live providers and unexplained partial-create stall | [#125](https://github.com/robertherbaugh/kudurru/issues/125) |
| INTEGRATIONS: Kubernetes K3/K7, limits/refusals, credential and lease-loss qualification | [#126](https://github.com/robertherbaugh/kudurru/issues/126) |
| INTEGRATIONS: webhook retention, mixed-version event cursor, budget/audit coverage and deferred event topics | [#132](https://github.com/robertherbaugh/kudurru/issues/132) |
| IAM: documented Kubernetes workflow refusals, restart-bound settings; remaining human/production-license approval | [#126](https://github.com/robertherbaugh/kudurru/issues/126), [#133](https://github.com/robertherbaugh/kudurru/issues/133) |
| IAM / HA: residual self-command error classification and mounted follower audit | [#137](https://github.com/robertherbaugh/kudurru/issues/137) |
| Runtime: loaded readiness/long-apply/refusal attribution and term-end diagnostics | [#138](https://github.com/robertherbaugh/kudurru/issues/138) |
| RBAC: precise permission-scanner helper allowlist and negative controls | [#139](https://github.com/robertherbaugh/kudurru/issues/139) |
| OBS: Events, tracing and live sink final-source qualification | [#131](https://github.com/robertherbaugh/kudurru/issues/131) |
| EDGE: restore-hold continuity and temporarily unreachable backend behavior | [#128](https://github.com/robertherbaugh/kudurru/issues/128) |
| OPS: manual UI, production entitlement/branding, package/deployment and release protection checks | [#133](https://github.com/robertherbaugh/kudurru/issues/133), [#140](https://github.com/robertherbaugh/kudurru/issues/140) |

The current report closes the specific final-source ClickHouse/Loki and
issued-production-signature gaps within #131/#133. GitHub denied attempts to
update the existing issue bodies with HTTP 403 (`Resource not accessible by
integration`); those issue texts may still name the original uncompleted scope.
The final-source report above changed no issue body or issue state. Its
committed evidence records the specific completed checks; remaining requirements
in those issues stay open.

### Verified issue closeout, November 21, 2026

The following scopes are complete on `main`. This evidence does not qualify
the published release artifacts or waive any other acceptance gate.

| Issue | Merged changes and evidence |
|---|---|
| [#122: query-log hour boundaries](https://github.com/robertherbaugh/kudurru/issues/122) | [#186](https://github.com/robertherbaugh/kudurru/pull/186) (`bec88078`) persists dirty-hour markers across restart on top of the live-read fix `63004b9c`. Fixed-clock regressions cover the hour boundary, a restarted and stopped source, and local and cluster agreement between raw, summary and client counts in partial-hour windows. The restart and stopped-source regressions failed on the old base; [hosted CI](https://github.com/robertherbaugh/kudurru/actions/runs/44809011740) passed with PostgreSQL 16 and 18. |
| [#123: Kubernetes external-source health](https://github.com/robertherbaugh/kudurru/issues/123) | [#196](https://github.com/robertherbaugh/kudurru/pull/196) (`6757c340`) adds per-kind source conditions and metrics without replacing last-good answers. Mounted envtest exercises denied Ingress, Gateway API and Service reads, permission restoration and watch recovery. Focused tests pin cap accounting and watch-only recovery; [#213](https://github.com/robertherbaugh/kudurru/pull/213) (`115ba9bd`) verifies bounded retries and selector compatibility without changing the [documented caps](../../../docs/architecture/zones.md). |
| [#130: RPZ diagnostics and CoreDNS import](https://github.com/robertherbaugh/kudurru/issues/130) | [#195](https://github.com/robertherbaugh/kudurru/pull/195) (`0d01b9df`) verifies both drivers' RPZ explanation and corrects Kubernetes policy state; [#188](https://github.com/robertherbaugh/kudurru/pull/188) (`48a80232`) pins retained answers and distinct omitted-name counts for multi-port imports. [#208](https://github.com/robertherbaugh/kudurru/pull/208) (`e12f07e1`) verifies pending filters, CLI messages and the signed Kubernetes fail-closed behavior. The [committed audit](followup-audit.md#sqlite-restart-and-pending-filters-163) records SQLite restart windows of 152 ms and 303 ms; the #208 description records 891 ms under the race detector. These runs had no NOERROR samples. The timings are samples, not release thresholds. |
| [#155: definite Kubernetes head-write refusals](https://github.com/robertherbaugh/kudurru/issues/155) | [#193](https://github.com/robertherbaugh/kudurru/pull/193) (`adafd18d`) implements D384 option A for definite 400/403/413/422 rejections and preserves a single read-back and no replay for ambiguous outcomes. Unit tests and a mounted API-proxy 403 regression verify the classification and unchanged state; this was not a live RBAC change. [Hosted CI](https://github.com/robertherbaugh/kudurru/actions/runs/44811605270) passed. |
| [#167: hosted DEB/RPM lifecycle](https://github.com/robertherbaugh/kudurru/issues/167) | [#180](https://github.com/robertherbaugh/kudurru/pull/180) (`07881d30`) adds the optional workflow. The [hosted job](https://github.com/robertherbaugh/kudurru/actions/runs/44684111762/job/136114422668) ran the real maintainer scripts and systemd lifecycle, including checksum-verified upgrade with edited config retained, restart, removal, DEB purge and RPM `.rpmsave` retention. This is snapshot-package evidence only. |

The #167 [result artifact](https://github.com/robertherbaugh/kudurru/actions/runs/44684111762/artifacts/14606526721)
reported `maintainerScriptsRun: true` and `systemdLifecycleRun: true` for both
formats. The SHA-256 values from that run are recorded here before the artifact expires:

| Package | SHA-256 |
|---|---|
| `kudurru_0.1.0_amd64.deb` | `8e331f16743b4ce7f5cfe3d71a6d6b4f9d52a0dab60f4d633abc29efbd1c36ec` |
| `kudurru_0.2.0~SNAPSHOT-07881d30_amd64.deb` | `17fe476af85c5fe66e8e6897cb626cc2ccebf2c728c68cf6e4961f222820d022` |
| `kudurru-0.1.0-1.x86_64.rpm` | `ddd5aec37db4a17050d1c0f5629a6566012e2eabe6a88b0ba0229addfba06437` |
| `kudurru-0.2.0~SNAPSHOT_07881d30-1.x86_64.rpm` | `d81414c4868b7141d5b5a4980c1879328d07d9434654528f78988d87420b77e5` |

Related issues remain open where the merged work covers only part of their scope:

| Issue | Merged progress | Still open |
|---|---|---|
| #125 | #216 verifies recovery through a mounted SQLite and UniFi double, including restart, lease loss and foreign-record protection; #211 documents live prerequisites. | Authorized live acceptance and receipts in #153. |
| #126 | #192 verifies the CLI and refusal contracts; #193 resolves definite head-write classification. | Mounted K3/K7 qualification in #156. |
| #127 | #197 retains strict authority validation for same-serial IXFR without AA. | Hosted pinned private-daemon suites in #157, plus the documented dnsdist limitation. |
| #129 | #190 documents the owner's grace, deadline and replacement decisions; #187 verifies how cut follower apply results name the original primary. | Deferred child #160. D358 remains separate. |
| #131 | #209 adds bounded, counted Kubernetes Events delivery. | Tracing and hosted live-sink follow-ups #165 and #164. |
| #133 | #183 documents the manual acceptance runbooks and #180 provides snapshot-package lifecycle evidence. | Final-artifact visual approval, authorized production issuance and expiry/grace evidence. |
| #135 | #215 verifies abandoned-term rejoin, and #217 records hosted managed-anchor bootstrap and readiness evidence. | Production puller refusal and fencing coverage in #169. |
| #136 | #206 and #202 cover Kubernetes ownership and SQLite partial-refusal visibility, respectively. | Pinned live edge TCP qualification in #127 and the remaining driver limits. |
| #138 | #198 pins retiring-zone refusal and `LastError` lifetime; #210 covers budget classification and rotation cancellation. | Loaded runtime attribution in #172. |
| #140 | #182 provides a merge-only proposal and a read-only verifier. | A November 21 read of the live rules still found rebase allowed and no required `promotion` check on either release branch. Apply and verify the live policy separately. |
| #143 | #181 diagnoses the ineligible CNAME result; #179 proposes a host-equivalence preflight. | A passing final-source run on qualified hardware under the unchanged gates. |

### Owner disposition for #129

On November 20, 2026, the owner accepted D395 as implemented (up to one second
of grace for ordinary answers; transfers still require the current publication,
and UPDATE acknowledgments, API dig, and mutations remain strict) and D375 as
implemented (one 30-second follower apply lifetime, honest per-document
partial results and no replay; send large workloads to the primary). The
[documented member replacement procedure](../../../docs/deploy/sqlite-join.md#replace-a-member-that-cannot-follow)
is sufficient for v0.2; the #160 retirement command stays deferred.

The owner decision is separate from the verification:

- **Local grace and replacement regressions:** focused `-race` runs on
  `ce3ab6e2` passed `TestPublisherServedWithinKeepsRecentlyReplacedPublications`,
  `TestPublisherServedWithinHasNoGapAtTheSwap`,
  `TestRootAnchorServedDNSFollowsThePublication`,
  `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation`,
  `TestReplacementReceiverNeedsItsOwnNameAndAddress`,
  `TestInvitationAndActiveMemberCapacityDoNotPrune`,
  `TestRevokedKeyCannotReturnUnderNewCertificateAndUUID`, and
  `TestRootMemberListenerTLSSeparationAndLiveRevocation`. Those tests check
  grace expiry, strict AXFR and mutation behavior, and the member identity and
  roster rules; they are not an operator replacement drill.
- **Local cutoff regression:** [#159 evidence](../../followups.md#local-follower-apply-evidence-159)
  covers both forced cutoffs, the durable per-document results, and no replay.
- **Hosted single-runner process tests:** [CI run 44667589755](https://github.com/robertherbaugh/kudurru/actions/runs/44667589755/job/136061099846)
  on `91b4f2c5` (merged in #184) passed `TestEnrolledClusterProcessesPartitionCatchupAndRestart`
  and `TestEnrolledClusterProcessesPromoteAndRejoin` under `-race`. The
  original 200-document follower applies each returned all 200 as created,
  in 8.49 seconds and 8.37 seconds, respectively. These timings are one
  runner's result, not a guarantee under load.

These results do not qualify a final release artifact or independent two-VM VIP
failover. The D358 deferral and other release gates are unchanged. Keep #129
open while its deferred #160 sub-issue remains open.

## Explicit future acceptance

AD/Windows production acceptance remains future work under **D353**. The
isolated GSS-TSIG probe is not production AD DNS replacement. Independent
**two-VM VIP/failover** acceptance remains future work under **D358**. Local
containers, a single VM, envtest and KIND/MetalLB cannot close that requirement.

## Final execution record

The [portable evidence bundle](evidence/2026-11-16-final-source/README.md) retains
raw qualification archives, input/receipt hashes, all metric vectors and selected
artifact/test records. Local supplementary logs remain under `.context/`. The source follow-up is
[PR #134](https://github.com/robertherbaugh/kudurru/pull/134). A fresh checkout
can review the committed bundle in [PR #144](https://github.com/robertherbaugh/kudurru/pull/144),
stacked on #134, without local logs. No GA tag or release was published.

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
  errors and terminal DNS errors still fail except the narrowly classified
  read timeout described next. The final hosted shard passed on `a22255a2`.
- The EOF-only local ledger rerun failed in 1,182.577s: 1,097/1,100 real
  NOERROR, ten SERVFAIL, 16 EOF retries and three TCP read timeouts. That failed
  log is retained. The client and server pipeline both have five-second
  deadlines (`internal/dnsserver/handler.go`), so a withheld original response
  may expire at the reader before TCP closes. The bulk test now separately
  counts/retries only a `net.OpError` whose operation is `read` and whose
  `Timeout()` is true. It keeps four workers, the five-second client timeout,
  the original three-minute logical-change deadline, all 1,100 real NOERROR and
  every replay/signature-expiry/restart assertion. Other transport/DNS errors
  still fail. This test qualifies ledger capacity/correctness, not success of
  every first attempt. The corrected test compiles with race instrumentation;
  hosted command shard 4/4 passes the complete test. No production response
  authorization or request deadline changed.
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
Human visual approval stays open in #133. The later issued production-key
check below supersedes only the missing real-key proof.

The owner then supplied an issued key from the Mac for the disposable normal
candidate. The encrypted transfer and private test copies were removed afterward;
the original Downloads file was retained. The archive-identical binary accepted
its embedded production trust root and reported **active Enterprise, three nodes
and all eight features**, matching the signed claims. All **eight checks passed**:

- License UI feature states, additional Policy creation, audit API and unlocked
  long-term history.
- Enterprise branding save/render and restart persistence of entitlement,
  authenticated session and DNS.
- The normal minute watcher rejects a tampered production signature, refuses
  new policies and keeps DNS answering; restoring the original key reactivates
  the entitlement.
- Removing the disposable copy returns to Community/default branding and locks
  licensed history, while preserving the existing Policy, session and DNS.

The [issued-key result](evidence/2026-11-16-final-source/production-license-result.json),
[cleanup record](evidence/2026-11-16-final-source/production-license-cleanup.json)
and agent-inspected screenshot are committed without the key, licensee or
license ID. This accepts an already-issued signature on the normal candidate
archive; it does not test license-server issuance or a production-key expiry/
grace transition. On November 20, 2026, the owner confirmed that an issued-key
run in a disposable VM remains preferred. The runbook checks the final archive
after a restart at each advanced guest time, not live wall-clock jumps. A
[clock-controlled synthetic fallback](manual-acceptance.md#synthetic-fallback-for-expiry-and-grace-only)
is allowed for expiry/grace behavior only. Injected-clock tests on a source build
do not add production trust, signing, or issuance evidence and are not a run
of the final archive. The [manual runbook](manual-acceptance.md) also defines
explicit human UI approval on the exact final artifact SHA256 values; merging
code or the runbook does not supply approval. No new gate result is recorded.

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
calibration, host and approval are unchanged. The complete corrected-source
result below is retained in documentation PR #144, based on #134; this keeps
source CI tied to its actual code head.

### Complete final-source performance result

The one fresh corrected sequence ran on `a22255a2bc981a8fa12afcd2778267d7b681de1b`
from **00:43:41 to 01:10:32 UTC on November 16, 2026**, using the newly guarded
approved host and unchanged historical calibration/limits. All **60 native
processes completed with exit zero**, with no resource stop or source/host/input
guard failure. Independent verification checks 159 raw files, exact process
order, original/candidate source maps (406/5,352 paths), report concatenation,
receipt hashes and the immutable approved inputs.

Qualification nevertheless **failed**: all **ten absolute report gates failed**.
CNAME lookup p99 medians are **2,187 ns reference / 2,363 ns candidate** against
the unchanged **2,000 ns** fixed maximum. The other 40 matched fixed-product
checks pass, but calibrated cache/upstream and candidate transport/persistence
limits also fail. Because the original reference is ineligible, the collector
correctly refuses all three relative comparisons. There is no valid relative
regression verdict and no performance pass to transfer to this source.

The [complete evidence](evidence/2026-11-16-final-source/README.md) contains every
five-value vector, raw process/gate output, input/host/approval hashes and the
earlier source-correction hold. Raw-artifact manifest SHA256:
`e61801285bef4b5477b6094ea6290c7b9f35e5e2f9ffe10984cd2aa100b3fb9c`.
Collector exit 1 and `performance_pass: false` remain explicit. The independent
verifier exits 0 because it confirms this failed outcome. The original failed
homelab/calibration history is unchanged. No qualification timing was repeated
after this failure, no sample replaced and no limit relaxed.

[#143](https://github.com/robertherbaugh/kudurru/issues/143) tracks the blocking
result. A further attempt requires a diagnosed fix or separately reviewed
method change under the existing protocol. The host amendment itself supplies
no such approval after a failure. D383/D412/G23 remain open.

#### Retained-vector diagnosis (#175)

**`release_qualified: false` — descriptive comparisons only.** The
[offline comparison](../../../bench/README.md#final-source-cname-diagnosis-175)
joins five-process medians by report, benchmark and unit. Historical calibration
`cloud-shared-20261115` and the final reference measured the same source,
`0cc9565e`, before and after the sandbox restart. The final candidate was
`a22255a2`. Both comparisons match 219 metrics; the 373 candidate metrics
without a reference are excluded. The aggregate below uses only the 79
matched `ns/op`, `p50-ns` and `p99-ns` metrics, not all units.

| Metric | Calibration reference | Final reference | Final candidate | Final reference / calibration | Candidate / final reference |
|---|---:|---:|---:|---:|---:|
| CNAME p99 (ns) | 1,890 | 2,187 | 2,363 | 1.157 | 1.080 |
| CNAME p50 (ns) | 825 | 1,087 | 1,102 | 1.318 | 1.014 |
| CNAME ns/op | 974.2 | 1,289 | 1,402 | 1.323 | 1.088 |
| Hit p99 (ns) | 901 | 1,078 | 1,295 | 1.196 | 1.201 |
| Hit ns/op | 563.3 | 652.4 | 695.1 | 1.158 | 1.065 |
| Median ratio, 79 metrics | — | — | — | 1.111 | 1.005 |

The same-source ratios range from 0.930 to 1.881, with fast upstream `ns/op`
up 46–53%. Within the final run, all three nanosecond ratios for nodata and
nxdomain are at or below 1.00. The CNAME calibration median left just 110 ns
(5.5% of the unchanged 2,000 ns maximum); the five processes ranged from
1,706 to 2,602 ns. The final reference's median alone exceeded the limit by
187 ns. Unchanged source drifted enough to fail the gate without the candidate.
This does not identify the reason for the drift or establish physical-host
contention. The final reference remains ineligible for a regression verdict.

#### Local zone hot-path diagnosis (#175)

**`release_qualified: false` — instrumented local microbenchmark only.** On
November 20, 2026, detached `0cc9565e` and workspace head `61916ce5` were built
with Go 1.27.4, `linux/amd64`, `CGO_ENABLED=0`. The benchmark body is identical
and `internal/zone` is unchanged between `a22255a2` and `61916ce5`; the local
binaries still represent different full checkouts and dependency sets.

- `go tool nm -size`: `resolve` grew from 780 to 1,256 bytes and `answer` from
  816 to 934 bytes. `go build -gcflags=-m`: neither became inlineable, the
  outer `Lookup` remains inlineable, and `LookupFrom` now inlines into the call
  to non-inlined `LookupWithin`. The callback does not escape; the two-slot
  CNAME answer slice does.
- `go tool objdump`: `resolve` reserves 368 → 392 stack bytes in its prologue
  and adds the fallback check before `step`, the NXDOMAIN fallback check and
  the post-alias `owns` nil check. The standalone benchmark passes nil for
  `owns` and does not enable fallback, but still executes their guards on the
  relevant paths. `answer` adds `runtime.makeslice` on the CNAME branch, not on
  the direct hit path.

Six interleaved 500 ms `BenchmarkLookup100k` processes at six Go processors
finished with exit zero, each with a CPU profile and `GODEBUG=gctrace=1`.
The table is in execution order. Each subbenchmark had one forced GC at
startup; additional GC cycles below are those logged between the verbose
start marker and the result line, not cycles attributed to a particular query.
The VM exposed eight CPUs throughout. Full `/proc/loadavg` and
`/proc/pressure/cpu` snapshots were taken before and after each process;
`perf stat` was unavailable.

| Process | CNAME p99 (ns) | Hit p99 (ns) | Extra GC, CNAME / hit | 1-minute load, before → after | CPU PSI `some avg10` %, before → after |
|---|---:|---:|---:|---:|---:|
| Reference 1 | 2,172 | 1,084 | 1 / 0 | 8.20 → 7.63 | 38.34 → 26.43 |
| Head 1 | 1,585 | 924 | 0 / 0 | 7.63 → 7.10 | 26.43 → 14.87 |
| Head 2 | 1,452 | 1,094 | 0 / 0 | 7.10 → 6.61 | 14.87 → 10.33 |
| Reference 2 | 2,072 | 796 | 1 / 1 | 6.61 → 6.61 | 10.33 → 6.93 |
| Reference 3 | 1,832 | 871 | 1 / 1 | 6.61 → 6.16 | 6.93 → 3.80 |
| Head 3 | 2,153 | 1,028 | 0 / 0 | 6.16 → 5.75 | 3.80 → 2.55 |

Median CNAME p99 went from 2,072 to 1,585 ns, in the opposite direction to
the retained candidate comparison, and CNAME allocations fell from 2 to 1/op
(48 to 32 B/op in each process). Median hit p99 rose from 871 to 1,028 ns,
but the direction flipped across adjacent pairs, and hit allocations stayed
at 1/op. The other median p99s were wildcard 1,637 → 1,477 ns, nodata
690 → 808 ns, and nxdomain 552 → 596 ns. The pooled CPU profiles include zone
construction, all lookup cases, timing, sorting and GC. `appendKey` accounted
for 16.3% → 16.8% of flat samples; `resolve` 2.55% → 2.19% and `answer`
0.90% → 0.58%. The profiles do not attribute the hit tail to an added guard.

**No concrete source latency cost was isolated.** The static comparison
confirms extra instructions on the lookup path, but neither it nor the
profiled samples under changing CPU pressure explains the failed CNAME gate.
The hit p99 signal in both datasets warrants a separate, source-isolating
diagnostic if pursued; do not infer a fix from the broad source comparison.
No source fix, qualification or method change is part of this diagnosis.

### Final hosted CI and release disposition

Run [43781940600](https://github.com/robertherbaugh/kudurru/actions/runs/43781940600)
**passed all 26 jobs** on final source `a22255a2`, including `ci-ok`, both
observer shards, the corrected complete ledger test and e2e/BIND interop. Security
run [43781940738](https://github.com/robertherbaugh/kudurru/actions/runs/43781940738)
has passed `security-ok`. The earlier `50006070` run was superseded by the
read-timeout test correction and is not claimed as final green CI. Final source
check/job snapshots are committed in the evidence bundle. Hosted Compose,
KIND/MetalLB and development-image checks also passed on `a22255a2`; their
exact URLs are in the check snapshot. These retain their single-environment
scope and do not close D358.

The issued-key checks establish production-signature acceptance on the tested
candidate only. Final-artifact human UI approval, authorized production
issuance evidence, and expiry/grace evidence (preferred disposable VM or the
labeled synthetic fallback) remain open. So do package maintainer scripts/
actual systemd lifecycle, issue dispositions, performance, and required PR
review. D353 and D358 retain the future scope above. No release or production
deployment occurred.
