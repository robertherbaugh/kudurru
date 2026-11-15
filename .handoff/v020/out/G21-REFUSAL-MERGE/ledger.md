### G21-11 Refusal inventory on the pull request head — November 13, 2026 (no decision row)

The refusal inventory of G21-REFUSAL-INVENTORY and G21-REFUSAL-OPEN now
runs on the integrated head 606a53a8. The fourteen commits were
cherry-picked one at a time, with no conflict except in
`docs/deploy/capabilities.md` (from ee5415ca). The seven commits of the
parent and child zone fix on the same branch were not brought.

ee5415ca was narrowed. The command path's 503 unavailable "current writable
serving generation unavailable" (`currentCommandAPIError` in
`cmd/kudurru/serving_settings_api.go`) keeps its body without a hint. The
cmd test that asserted a hint and the `capabilities.md` row change were
dropped. `TestServingBlockingCurrentRawScopeAndCanceledWriter`,
`TestServingBlockingMissingCanonicalSingletonIsUnavailable`,
`docs/deploy/sqlite-cluster.md` and `docs/deploy/api-contracts.md` are
unchanged, so D399-REDO keeps the meaning of that answer. The walk part of
ee5415ca was kept:

- The walk treats that answer as transient, as it treats
  `generation_pending`: the probe is sent again for at most a minute.
- The message is a constant of the walk, not of `internal/api`.
- The DHCP setup probe is back in path order.

The walks then asked for the operations that other lanes had added:

- `updateLocalUser` and `deleteLocalUser`: `read_only` on static and
  `user_management_unavailable` on Kubernetes.
- The ServiceAccount and IdentityProvider commands and
  `createServiceAccountToken` (D396): `read_only` on static and
  `iam_management_unavailable` on Kubernetes. The new allowlist row points
  to the new glossary anchor `docs/deploy/capabilities.md#iam_management_unavailable`.
  That page also gained a capability row, and its local-user row now covers
  changes and deletion.
- `getServiceAccount`, `getIdentityProvider` and `listInventory` (D397):
  no refusal on any shape.

The emitter check found four new hintless 503s in `internal/api`:

- Two authorizer errors (`authroutes.go me`, `authz.go listAllowed`) and a
  serving inventory that could not be read are internal or transient
  faults. They joined `hintlessRefusals`.
- `listInventory` on a server without the client inventory now has a hint.

The integration refusal's hint said "integrations run only on the SQLite
store". That is no longer true: Kubernetes mounts the integration runtime.
The hint now says that the server was started without the runtime.

The enrolled process walks found a follower race. A forwarded probe
(`putClientgroups`) answered 503 unavailable "the enrolled primary is
unavailable; the write was not applied" while the primary was up. The
follower had installed newer data, which the walk's own forwarded probes
had caused, before it sent the command. The walk now sends a probe again,
for at most a minute, after two follower answers that say nothing was
sent: that one, and the documented "the follower's source changed". An
answer that lasts the minute still fails the walk.

The fenced walk and the final primary and follower walks run only in the
plain variants of `testEnrolledProcessesWith`. The managed anchor variants
(`enrolled_anchors_test.go`) count root acquisitions across that flow, so
they do not walk.

Evidence under `.context/v020-final-20261110/evidence/G21-REFUSAL-MERGE/`:

- `api01-before.log`: a retained failure. Right after the cherry-picks,
  `TestRefusalEmittersExplainThemselves` reported 4 hintless sites, and
  `TestRefusalInventoryNamesEveryOperation` reported 12 operations without a
  line.
- `root01-dump.log`: a retained failure (no race, with dump; 75.7 s).
  Standalone and the closed members pass. Static and Kubernetes fail for
  the IAM writers above; Kubernetes also fails for the missing
  `iam_management_unavailable` allowlist row.
- `race02-root.log`: `-race` run of the standalone, static, Kubernetes and
  closed-member walks, `TestLegacyAuthHandlerFallbackAndRefusals`,
  `TestRunServeKubeForwardedAuthRoutesAnswerEnvelope` and the two blocking
  tests that ee5415ca broke. All pass (170.0 s). Per walk:
  - standalone: 212 operations, 18 refused, 28 stopped at validation, 42
    answered 404;
  - static: 102 refused, 7 at validation, 25 answered 404;
  - Kubernetes: 51, 28, 48;
  - fenced and conflicted: 212 of 212.
- `race03-api.log`: `-race ./internal/api/...` passes: `internal/api`
  334.9 s, `internal/api/refusalinventory` 1.8 s. This includes
  `TestOpenAPIDrift` and `TestRoutePermissionLint`.
- `e2e01.log`: a retained failure, not about the code. The linker was
  killed (out of memory) while three heavy runs were building.
- `e2e02.log`, `e2e03.log`: retained failures (`KUDURRU_E2E=1`).
  - The conflict test passes (212 of 212 on each conflicted process).
  - The promote test fails in its follower apply cut, before any walk; see
    the base defect below.
  - In e2e03, the partition test's follower walk failed for `putClientgroups`
    with the follower race above. That led to the transient-answer table.
- `e2e04-base.log`: the base 606a53a8 with `-count=3`. The partition test
  passes 3 of 3. `TestEnrolledClusterProcessesPromoteAndRejoin` fails 3 of
  3 with the same cut failure: "document 6 of 40 after the cut at 5 was not
  refused as documented ... the enrolled primary is unavailable; the write
  was not applied".
- `e2e05.log`: the head with the transient-answer table, `-count=3` of the
  three tests.
  - `TestEnrolledProcessConflictResolution` passes 3 of 3.
  - `TestEnrolledClusterProcessesPartitionCatchupAndRestart` passes 1 of 3.
    The pass has both walks: primary 25 refused, 28 at validation, 42
    answered 404; follower 82, 13, 33.
  - `TestEnrolledClusterProcessesPromoteAndRejoin` passes 1 of 3. The pass
    has the fenced walk with 212 of 212 answering `cluster_fenced`.
  - Each of the four failures is the base cut defect, before any walk.
- `lint01.log`: golangci-lint v2.15.0 for `internal/api/...`,
  `test/e2e/...`, `cmd/kudurru/...`, `internal/auth/stack/...` and
  `internal/servinghttp/...`. One issue, gosec G117 in
  `cmd/kudurru/serve_inventory_mount_test.go:159`. That file is unchanged
  since da0beb37, before this package. `gofmt -l` is empty for the touched
  files, and the bench guards pass (14 tests).
- `full01-cmd.log`, `full02-cmd-batches7-9.log`: the full
  `python3 hack/go-test.py --timeout=30m .../cmd/kudurru` with `taskset -c
  0-3` and `GOMAXPROCS=4` (870 tests in 9 batches). The first invocation
  ran batches 1 to 6 and was stopped by the agent's two-hour limit in batch
  7. Batches 7 to 9 and the norace timing tests then ran through the same
  batching code (`/tmp/g21merge/rest.py`, which imports `hack/go-test.py`).
  - Batches 5 and 9 and the norace tests pass.
  - 22 tests failed once, on a machine that the other lanes also loaded
    (load average 8 to 14). Each was then run three times at the head and
    three times at the base; the numbers follow (`rerun01-head.log`,
    `rerun02-head.log`, `rerun03-base.log`).

| Test | Head, 3 reruns | Base, 3 reruns |
| --- | --- | --- |
| `TestIntegrationUniFiPushMirrorRecordsReviewed` | 3 (0 failed) | 2 (1 failed) |
| `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` | 2 (1 failed) | 0 (3 failed) |
| `TestRunServeSQLiteTOTPEnrollmentRestartRecovery` | 3 (0 failed) | 2 (1 failed) |
| `TestRunServeWebhookRecordsFileIntegrationAndCLI` | 2 (1 failed) | 2 (1 failed) |
| `TestServingNodeCommandActualRefusesStaleAuthorityAndFinalExpiry` | 3 (0 failed) | 3 (0 failed) |
| `TestServingTSIGObserverQuietOriginalExpiry` | 3 (0 failed) | 1 (2 failed) |
| `TestRunServeSQLiteCapturedBrowserBothOriginsAndRestart` | 2 (1 failed) | 1 (2 failed) |
| `TestRunServeQuerylogClickHouseAndLokiSinks` | 2 (1 failed) | 3 (0 failed) |
| `TestRunServeRefusalInventoryStandalone` | 3 (0 failed) | new test |
| `TestRunServeSQLiteSourceEntriesRestart` | 3 (0 failed) | 3 (0 failed) |
| `TestServingBlockingActualHTTPStatusNoopAndExpiry` | 3 (0 failed) | 3 (0 failed) |
| `TestServingNodeCommandActualSQLFailureAndCommittedReadback` | 1 (2 failed) | 2 (1 failed) |
| `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` | 2 (1 failed) | 1 (2 failed) |
| `TestRunServeKubeUniFiSourcesTwoReplicasAndPartition` | 3 (0 failed) | 3 (0 failed) |
| `TestServingHTTPFactorsRotationAndConcurrentConfirm` | 3 (0 failed) | 3 (0 failed) |
| `TestServingHTTPRequestBudgetsAcrossAdaptersAndReplicaRestart` | 3 (0 failed) | 3 (0 failed) |
| `TestServingMigrationCutWaitDoesNotBoundTheStepCommand` | 1 (2 failed) | 2 (1 failed) |
| `TestOfflineCLIActualOwnershipPartialReport` | 3 (0 failed) | 3 (0 failed) |
| `TestIntegrationWebhookHooksMountedZoneChangedOneSource` | 3 (0 failed) | 3 (0 failed) |
| `TestRunServeUniFiHostnameHeldPublicationDeadline` | 2 (1 failed) | 2 (1 failed) |
| `TestServingHTTPBrowserLogoutExpiredOrMismatchedHintStaysLocal` | 1 (2 failed) | 3 (0 failed) |
| `TestRunServeUniFiHostnamePolicyLifecycle` | 3 (0 failed) | 3 (0 failed) |

Of the 22 tests, 21 have a base number; the inventory walk is new. Of
those 21:

- 9 passed every rerun in both trees.
- 7 failed at least once in both trees.
- 3 failed only at the base.
- 2 failed only at the head, and both were run five more times on each
  tree (`rerun04-head.log`, `rerun05-base.log`):
  - `TestServingHTTPBrowserLogoutExpiredOrMismatchedHintStaysLocal`: its
    `expired` subtest waits 2.1 s for a short logout continuation. Head 6
    of 8 in all (5 of 5 in the second set), base 8 of 8.
  - `TestRunServeQuerylogClickHouseAndLokiSinks`: each failure was
    `PATCH /settings: HTTP 503`. Head 6 of 8 in all (4 of 5 in the second
    set), base 8 of 8.
  - This package does not change either path. Its `cmd/kudurru` changes
    add a hint or `NoRetryAfter` to refusal bodies and nothing else. The
    integrator should still watch these two on the merged tree.

`TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` failed 3 of 3
at the base and 1 of 3 at the head ("managed recursive runtime did not
become ready"). The other repeated failures are load or timing failures in
both trees (`SQLITE_BUSY`, "original integration operation unavailable",
a hostname policy deadline). Among them are the known base flakes
`TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` and
`TestRunServeSQLiteCapturedBrowserBothOriginsAndRestart`.
`TestCLIOIDCBrowserActualTLSProofRefusals` did not fail.

`TestRunServeRefusalInventoryStandalone` failed once in `full01-cmd.log`
with `oidcStart` answering `429 rate_limited`. Every send of provider
sign-in is charged against its admission lane (five per minute for the
start and CLI lanes, ten for the callback) and the charge is a commit. On
a loaded node that takes longer than the 2 s capture wait to prepare the
next generation, the operation's own charge leaves it pending again, so
the walk's resends after `generation_pending` used up the lane. The walk
now counts the charged sends of `oidcStart`, `oidcCallback` and
`prepareCLILogin` (`admissionBudget` in
`internal/api/refusalinventory/walk.go`). When the lane is full it waits
one window before it sends again, at most twice, instead of sending a
request the lane would refuse. A 429 is still judged by the inventory and
fails the walk; it is not accepted as an answer.

- `fix01-before.log`: a retained failure. The new
  `TestProbeStaysWithinTheSignInAdmissionLimit` (a double that follows the
  lane's rule and answers `generation_pending` to the first seven admitted
  sends) fails before the change: "answer 429 rate_limited ... after 5
  admitted sends".
- `fix02-inventory.log`: `-race ./internal/api/refusalinventory` passes
  with the change (3.0 s), the new test included.
- `fix03-mounted.log`: `-race -count=3 -run TestRunServeRefusalInventory
  ./cmd/kudurru` passes (147.8 s).
- `fix05-mounted-2cpu.log`: the same, verbose, with `GOMAXPROCS=2` and
  `taskset -c 0-1`: standalone, static and Kubernetes pass 3 of 3 each
  (standalone 20.6 to 25.9 s; 212 operations, 18, 102 and 51 refused).
  Two cores did not reproduce the starvation of the full run, so the
  mounted runs show that the change breaks nothing; the unit test is what
  shows the 429 is gone.
- `fix04-lint.log`: golangci-lint for `internal/api/refusalinventory/...`
  reports 0 issues. The bench guards pass (14 tests).

`lint02.log`: golangci-lint for `internal/api/...` and `test/e2e/...` at
the final head reports 0 issues. The drift and refusal tests of
`internal/api` and the walk unit tests pass there too.

What remains open:

- The base defect that the promote test shows is not fixed here. A
  forwarded write cut by the follower's own pull answers "the enrolled
  primary is unavailable; the write was not applied" instead of the
  documented "the follower's source changed". It fails
  `TestEnrolledClusterProcessesPromoteAndRejoin` 3 of 3 at 606a53a8, and
  intermittently the partition test too. The likely place is
  `objectForwardError` in `cmd/kudurru/serving_member_objects.go`: an
  unsent forward whose error is the local `serving.ErrPending` of a
  superseded read lease (`withMemberForward`, `CheckReadLease`) falls into
  the "primary unavailable" case. This belongs with the capture work
  (D399-REDO) or the HA lane. The walk's resend of that message should be
  removed once the follower answers correctly.
- `currentCommandAPIError` still has no hint. Whether it gets one is left
  to D399-REDO.
- The G21-REFUSAL-OPEN changelog draft has a bullet that gives that answer
  a hint. That bullet no longer applies.
- The `cmd/kudurru` refusals still have no static emitter check; only the
  walks check them.
