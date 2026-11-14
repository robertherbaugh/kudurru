### F28-OPEN6 What verification left open in the managed-anchor packages — November 13, 2026 (D369)

Lane `wip/v020/f28`, base `97c9117b`. Source commits `1ef0b201`, `4827ec14`,
`1658a4c7`, `5757051e`, `2649a86f`, `83e671b2`, `e79c29fc`, `d485cce0`, and
after the review `a23db4d3`, `8ff9aa69` and `b400469f`. No
generated file changed (no `chore(gen)` commit), no new dependency.

What now happens:

- **A managed primary whose refresh is not due accepts writer commands without
  waiting for the anchor step.** A restarted, promoted or resolved managed
  primary already holds accepted managed trust, so it publishes and reports
  ready and mode primary. Its writer episode held the elected services that
  admit writer commands (primary driver, providers) behind the anchor
  updater's first step, which can be a 30-second root round trip, so every
  writer command in that window was refused 503 "current writable serving
  generation unavailable". The anchor child now releases that startup gate at
  once when the image it starts on carries an accepted managed owner that is
  not due for a refresh within a minute by the updater's own wall clock (the
  step's due and checkpoint rule). That first step then only captures and
  schedules, and a startup write that races the capture refuses only the
  capture. Before the bootstrap, and whenever the refresh is due, the first
  step still precedes the writer services, as it did before this package.
  The review showed why (`a23db4d3`): a due reply may be a REVOKE, and a
  revocation denial that a startup write rolls back is terminal for the
  writer term (`anchorUpdateRaced`). With the gate released on every managed
  writer, a restarted writer's first-step REVOKE ended 3 of 3 with a terminal
  updater and the revocation uncommitted (`review-probe-head.log`). This
  removes the anchor-step wait for the usual restart, promotion and
  resolution. It does not make readiness imply writer admission. Readiness
  depends on publication only (`serving_mount.go` `ready()`), so a due
  writer, for up to its 30-second first step, and any writer, during the
  short start of its writer services after `resetWriter`, can still answer
  503 while it reports ready. That remainder belongs to CI-RED-2 and CI-RED-3.
  No test asserts writer admission at the first ready report. The three "wait
  for the updater's first step" blocks are gone from the managed conflict
  e2e test, and its writes after the restart, both promotions and the
  resolution restart all ran in `fix-e2e01`.
- **Capture-phase races.** A capture refused by ErrCAS keeps its own
  consecutive count and logs "root anchor capture raced an ordinary commit;
  capturing again" (it used to log the update-phase count, often 0). Its
  one-second pace is deliberately not backed off: a backed-off capture kept a
  fresh node unready for many seconds under race (retained runs `race05`,
  `race08`), because the bootstrap it delays is what lets the node publish.
  Instead its warning is logged only at the first, second, fourth, eighth and
  so on consecutive race (`anchorCaptureRaceLogged`, at most 12 lines in an
  hour of races). The status board still shows every race as `raced`.
- **The conflicted member's SERVFAIL (fix-e2e02-conflict.log) is diagnosed.** A
  primary that becomes Conflict or Fenced after the committed-status writer wrote
  its zone's status, and before the driver realigned the zone lifecycle's source
  receipt to the new DNSZone resourceVersion, can never realign: a DNS-only node
  commits nothing. Under the exact-resourceVersion serving rule the zone is
  served as committed-unavailable coverage (SERVFAIL, AA clear) for the whole
  conflict. In that run the heal came inside the half-second window after B's
  write. The product fix is `6780f27d` ("keep serving a zone across its own
  status write", CORE-STATUS-WRITE-SERVFAIL), which is on the PR branch but not
  on this lane. `TestRunServeDirectAnchorsConflictServesAcrossUnrealignedStatusWrite`
  builds that durable state on one runServe database with managed anchors and
  requires authoritative answers on all six transports.
- **F28-HA-4 runs by default.** A `-tags licensetest`-only seam,
  `KUDURRU_TEST_ANCHOR_UPDATER_AHEAD`, moves one process's writer-updater wall
  clock ahead for its due and checkpoint comparisons only (samples, receipts and
  stored state keep the real clock; a default build has none,
  `TestDefaultBuildHasNoAnchorUpdaterWall`). `TestEnrolledManagedAnchorsOfflineRecovery`
  (was the skipping `TestSoakEnrolledAnchorsOfflineRecovery`) restarts the
  primary with it, so its updater observes the REVOKE; both nodes quarantine;
  recover on the follower database is refused with no change and no
  acquisition; the stopped primary's offline recover commits with exactly one
  recovery acquisition; the follower adopts the recovered image through its
  scheduled pulls without a restart; keys, tombstones and recovery record are
  identical on both. About 170 s, no skip. `KUDURRU_ANCHOR_SOAK=1` still runs it
  with production timers.
- **A real store refusal reaches the gauge.** In the partition variant the
  follower has denied the sole key. The root answers normally again and the
  primary, restarted with its refresh due, commits a new owner that still
  trusts the key. The follower's scheduled pull through `follower.Puller`,
  `Coordinator.Pull` and `ObservePull` is refused (`carried_revocation`) and
  installs nothing. The follower's running metrics listener exports
  `kudurru_root_anchor_adoption_refused{reason="carried_revocation"}` as 0
  before the owner moves (now required, so a silent listener cannot pass) and
  as 1 after. `GET /cluster/anchors` on that retired follower answers 503: the
  route serves only under a lease of a published complete generation
  (`getClusterAnchors`, `CheckReadLease`), and a retired follower publishes
  none. The test now requires that the node also reports not ready in that
  case. The route assertion runs only if the route is open, which a retired
  follower's never is. The route's projection of a refusal shares
  `anchorCurrentRefusal` with the gauge and is covered in-process
  (`TestAnchorStatusBoardTracksAdoptionRefusal`,
  `TestAnchorStatusRefusalGaugeFollowsRole`), not by a real puller refusal.
- **Edge test gaps (F28-EF1-LIVE).** The observation listener stays up across
  the second edge restart of `TestRunServeManagedEdgePinnedDaemonFollower` and
  serves whichever supervisor is current (an observation while the child is
  down is counted and refused); the stale D370 comment is gone.
- **History recheck.** `TestClusterResolveAbandonedMemberRunsAsTheNewPrimary`
  abandons a lost term through the actual CLI, starts an actual serving runtime
  on the member with the store's elector carrying the writer token, and requires
  readiness, complete publication for the new term, a running primary driver, a
  writer token for exactly that term and an ordinary write under it.
- **Sampling.** The validated-recursion sampler starts with the process and
  judges every sample from the node's first DNS answer of any kind. A wrong
  answer (an rcode other than SERVFAIL, or NOERROR that is not the validated
  key set) fails. So does a node not validated when sampling stops, a SERVFAIL
  or no-answer gap longer than ten seconds, or, where the role must be
  sampled while fenced or conflicted, no validated DNS-only sample. Shorter
  gaps are logged as `OPEN F28-HA-3` with time, length, startup or not,
  DNS-only or not, and the first answer. d485cce0 failed on the first
  SERVFAIL, which turned `TestEnrolledManagedAnchorsPromoteAndRejoin` and
  `TestEnrolledManagedAnchorsConflictResolution` red by design and stopped the
  conflict test before its resolution step. The known transition defect
  (below) is now recorded on every run without failing the suite, and a
  persistent SERVFAIL (a lost zone or lost trust) still fails. This is looser
  than d485cce0's zero-SERVFAIL rule and than the pre-package promote
  sampler, which failed on any non-validated answer from readiness on. The
  F28-HA-3 criterion "never SERVFAIL" is therefore not met; it is recorded,
  bounded and open.
- **Deterministic settings absorption.** `absorbSettings` slept a fixed three
  seconds after the first deny-only acquisition of the two-patch settings
  change. When the second came later it was counted at the partition check
  ("partition: 4 root acquisitions, want 3"; 3 of 4 review runs on the lane
  binary, and earlier in the F28-PROCESS logs). The ledger's earlier
  attribution of the extra acquisition to the PR overlay and D377 was wrong.
  It now waits until the follower's supervisor is running with its next
  check at the minimum refresh and the count has been unchanged for five
  seconds, and logs each supervisor change. That fixed the settings case
  (`fix-e2e01`: the second acquisition came 0.6 s after the first and was
  absorbed). It did not remove every extra acquisition: in `fix-e2e02` one
  partition run absorbed 2 and still counted one more at the partition check
  ("5 root acquisitions, want 4"). The source of that query is not
  identified. Since `b400469f` the stand-in root lists each TCP DNSKEY query
  (time, mode, source, flags) in a failing count, and the test logs the
  primary updater's and the follower supervisor's last state before the
  check, so the next occurrence names it.

Evidence under `.context/v020-final-20261110/evidence/F28-OPEN6/`:

- `race01`-`race10` (earlier implementer, retained): `race01` and `race03`/`race10`
  show the new gate tests failing (the second also with the early release
  disabled by mutation: "writer services waited for an in-flight poll of already
  accepted managed trust"); `race05`/`race08` show the backed-off capture pace
  breaking `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` 4 of 4
  ("complete serving generation unavailable: preparing"), `race06` the same test
  passing at the base; `race07` is a build-cache trim; `race09` the selection
  passing after the pace was restored (ok, 121.0 s).
- `race11` (retained), `race13`: the conflict status-write test on this lane:
  `race11` failed in the shared settle helper (503 on a zone read under load),
  replaced by an observation-tolerant helper; `race13` fails as expected,
  "did not answer its zone across an unrealigned status write; last rcode
  SERVFAIL" after 30 s. `race12` was stopped by hand (source edited while it
  compiled). `race14`: the same test, `TestRunServeDirectAnchorsFencedAndConflictRestart`,
  `TestDirectAnchorRestartedWriter*` and `TestAnchorUpdater*` with
  `internal/primaryruntime/image.go` of the PR branch (6780f27d) as an overlay:
  ok, 80.3 s.
- `race15` (retained): the first form of the abandoned-member runtime test did
  not run the elector (driver never started). `race16`: `^TestClusterResolve`
  3 of 3 PASS (abandoned member runtime 16.6 s).
- `race17`: `-race`, `KUDURRU_ANCHOR_EDGE_LIVE=1` against the pinned dnsdist image
  on the lab daemon: `TestRunServeManagedEdgePinnedDaemonRestart` PASS 28.4 s,
  `TestRunServeManagedEdgePinnedDaemonFollower` PASS 49.8 s,
  `TestAnchorEdgeActivationJoinedFollower`, all twelve `TestAnchorUpdater*`
  (including `TestAnchorUpdaterCaptureRaceRetriesPacedWithoutStopping`), all
  `TestDirectAnchor*` (including `TestDirectAnchorRestartedWriterDuePollDoesNotHoldWriterServices`
  and `TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture`), every
  `TestRunServeDirectAnchors*` and `TestDefaultBuildHasNoAnchorUpdaterWall`
  PASS; the one FAIL is the conflict status-write test (no 6780f27d on the
  lane, as in `race13`). No data race, no container left.
- `e2e01`-`e2e06` (`KUDURRU_E2E=1`, `-race`): `TestEnrolledManagedAnchorsOfflineRecovery`
  PASS in every run (168.1 s, 173.7 s, 165.8 s); the partition variant with the
  refusal step PASS in `e2e05` and `e2e06` (142.2 s, 154.2 s; `e2e01`/`e2e02`
  failed on the route read of the retired follower, which answers 503);
  `TestEnrolledClusterProcesses*` and `TestEnrolledProcessConflictResolution`
  PASS. Every managed conflict run passed the one-shot writes right after both
  promotions and after the primary's restart with the waits removed.
  `e2e03`-`e2e05` use a licensetest binary built from this lane with the PR
  branch's 6780f27d, a8be6d71 and 89bda06a production files as an overlay
  (`/tmp/f28-open6/overlay-pr.json`), the closest approximation of the
  integrated branch available here. `e2e07` (diagnostic test overlay that only
  turns the heal-to-conflict sampler failure into a log line) runs the rest of
  the conflict flow, including the write right after the resolution restart:
  PASS 98.7 s on the lane binary (the sampler line is logged, B's first failure
  33 ms after its write, EDE 23, C's 1 s later without EDE).
- `lint01` (retained): three issues in the new tests, fixed in `83e671b2` and
  `d485cce0`. `lint02`: golangci-lint v2.15.0 over `./cmd/kudurru/...` and
  `./test/e2e/...` at `d485cce0`: 0 issues. `bench01`: 14 bench guard tests OK.

Review fixes (after `d485cce0`), same directory, `fix-*` logs:

- `fix-race01-gate`: `-race -count=2` of `TestDirectAnchorRestartedWriter*`,
  `TestAnchorUpdater*`, `TestDirectAnchorActualWriterBootstrap*`: ok, 123.2 s
  (not verbose).
- `fix-race02-gate-mutant` (retained, stopped by hand): the first form of the
  new tests hung in cleanup with the early release forced on. The held root
  reply was released after the root fixture joined its handler, so the
  cleanup order was fixed.
- `fix-race03-gate-mutant`: early release forced on for every managed writer
  (`return true || ...`): `TestDirectAnchorRestartedWriterDuePollPrecedesWriterServices`
  and `TestDirectAnchorRestartedWriterDueRevokeCommitsBeforeWriterServices`
  FAIL 3 of 3 each ("writer services started while a due root poll was in
  flight"). This is the reviewer's case. Their probe (`review-probe-head.log`)
  showed the same release ending in a terminal updater with the REVOKE
  uncommitted 3 of 3.
- `fix-race04-selection`: `-race -count=2 -v` of `TestDirectAnchor*`,
  `TestAnchorUpdater*`, `TestAnchorCaptureRace*`, `TestRunServeDirectAnchors*`,
  `TestDefaultBuildHasNoAnchorUpdaterWall` and `TestAnchorStatus*`: 58 PASS
  lines, no data race. The two new due-poll tests PASS twice each (4.85 s and
  4.86 s; 3.84 s and 4.01 s), as do
  `TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture` and
  `TestAnchorCaptureRaceLogIsBounded`. The one FAIL, twice, is
  `TestRunServeDirectAnchorsConflictServesAcrossUnrealignedStatusWrite`
  (40.3 s, 41.1 s), expected on this lane without 6780f27d.
- `fix-e2e01` (`KUDURRU_E2E=1`, `-race`, lane binary at `a23db4d3` plus the
  e2e changes): `TestEnrolledManagedAnchorsPartitionRestartAndRevoke` PASS
  154.0 s (refusal step reached; route 503 with the node not ready),
  `...PromoteAndRejoin` PASS 152.5 s (two gaps logged: a 1.07 s startup gap,
  and a 0.94 s gap while fenced with EDE 23), `...OfflineRecovery` PASS
  173.5 s, `...ConflictResolution` PASS 101.4 s. The conflict run includes
  the writes after both promotions and after the resolution restart with no
  updater wait. B had 4 gaps, longest 353 ms; C had 1 gap of 310 ms; all
  were taken while not DNS-only.
- `fix-e2e02` (`-count=2`, plus `TestEnrolledProcessConflictResolution`): 9
  PASS, 1 FAIL. The Partition run FAILED once (147.0 s, "partition: 5 root
  acquisitions, want 4" after absorbing 2, see above) and PASSED once
  (140.7 s). PromoteAndRejoin passed 2 of 2, OfflineRecovery 2 of 2, managed
  ConflictResolution 2 of 2 (longest gap 201 ms) and the static
  TestEnrolledProcessConflictResolution 2 of 2.
- `fix-e2e03-partition-diag` (`-count=3`, `b400469f` instrumentation):
  Partition PASS 3 of 3 (155.2 s, 149.3 s, 144.5 s). Totals since the
  absorbSettings change: Partition 5 of 6, OfflineRecovery 3 of 3,
  PromoteAndRejoin 3 of 3, managed ConflictResolution 3 of 3.
- `fix-lint01` (retained): one revive issue (context-as-argument) in the new
  test helper, fixed in `b400469f`. `fix-lint02`: golangci-lint v2.15.0 over
  `./cmd/kudurru/...` and `./test/e2e/...`: 0 issues. `fix-lint03-licensetest`:
  the same with `--build-tags licensetest`: 0 issues. `fix-bench01`: 14
  bench guard tests OK.

What remains open:

- **SERVFAIL on members holding valid trust at authority transitions (product
  defect, not fixed).** Every run records it. The restarted old primary
  answers SERVFAIL for up to about 1 s during startup and around its
  fenced-to-follower install (EDE 23 on the lane, the D377 commit-window class
  that a8be6d71 fixes). In the conflict variant B and C answer SERVFAIL for
  0.1 to 0.4 s around B's write and the conflict detection, while not
  DNS-only. The likely mechanism is the managed resolver input's currency
  check (`AnchorResolverInput.Current`: `gate.MatchesInstalled(mark)`)
  refusing a lookup whose lease predates an authority change. The samplers
  now record these gaps (`OPEN F28-HA-3`) and fail only on a wrong answer, a
  gap over ten seconds or a node not validated at the end, so the F28-HA-3
  criterion "never SERVFAIL on a Follower, Fenced or Conflict node holding
  valid trust" is **not met**. It needs a product fix or an owner decision
  against the anchor output guard's design. A startup gap (before the first
  validated answer) is now judged, and bounded the same way, rather than
  ignored.
- **Readiness does not imply writer admission.** A writer whose committed
  refresh is due keeps its first anchor step (up to 30 s) ahead of the writer
  services, so it can answer writer commands 503 while it reports ready. Any
  writer can do the same during the short start of its writer services.
  Readiness depends on publication only. No test issues a writer command at
  the first ready report. Defer to CI-RED-2 and CI-RED-3.
- **The occasional extra root acquisition before the partition check** (1 of
  6 partition runs after the settings fix) is not explained. The
  instrumentation in `b400469f` names it the next time it happens.
- The retire-on-REVOKE-then-promote sequence is not run. Fetch counts stay
  bounded rather than exact (`enrolled_cluster_test.go` still accepts 3 or 4
  rejoin acquisitions). A member of the abandoned term is not brought back
  through the production puller (only the abandoned member's own runtime is
  started).
- A retired follower's `GET /cluster/anchors` is closed by design (it
  publishes no generation, so no read lease). The refusal's online surface on
  it is the gauge, and the route's projection of a real puller refusal is
  not proven end to end.
- The conflict status-write test fails on this lane by design until 6780f27d is
  integrated. `5757051e` and `83e671b2` should be picked only onto a branch
  that contains 6780f27d, and that test run there twice under `-race` before
  item 2 is recorded as closed (proven so far only by the single overlay run
  `race14`). Its fixture builds the durable state by hand (status write plus
  ObserveAuthority on a closed database) rather than through the heal race.
- The full `./cmd/kudurru` package and the live edge tests were not rerun after
  the review fixes.
