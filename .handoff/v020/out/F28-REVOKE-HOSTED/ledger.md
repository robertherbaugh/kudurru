### F28-REVOKE-HOSTED Why the BIND revocation interop test failed on hosted CI — November 13, 2026 (D398)

Lane `wip/v020/f28`, base `87551fe9` (the end of F28-OPEN7). Source commits
`0e7b4879` (test(interop): fail the revocation test fast on a binary without
its seam) and `c7159c4b` (fix(anchors): count an acquisition its writer term
ended as canceled), then the review fixes `45ccd2ad` (fix(anchors):
describe and test the canceled acquisition outcome) and `10baf336`
(test(interop): compare the next refresh with the time the node stopped).
No generated file changed, so there is no `chore(gen)`
commit. No new dependency.

What now happens:

- **Cause of the hosted failures.** `TestBINDManagedRootRevocation`
  (F28-RFC) restarts the node with `KUDURRU_TEST_ANCHOR_UPDATER_AHEAD`, which
  moves the writer updater's wall clock just past the committed refresh. That
  variable is read only by `cmd/kudurru/anchor_updater_wall_licensetest.go`,
  which arrived in commit `43d42037` of F28-OPEN7. The pull request branch
  (`606a53a8`, and the hosted heads `a3b5a1d0`, `c5b0637c`, `95c57113`) had
  the test but not that file. On hosted the variable was ignored, so the
  restarted node's refresh was correctly an hour away and it made no
  acquisition. Both subtests then failed at `managed_root_revoke_test.go:145`
  with `1 managed acquisitions, want 3` and `want 2`. The hosted logs show
  this: after the seam restart the node sends no query at all to named, and
  logs no updater line. BIND made no difference. Hosted runs 9.18.39 and this
  machine 9.18.50, and both publish DNSKEY TTL 3600 with 30-day signatures,
  so the RFC 5011 interval is 1 h on either. Timing made no difference
  either, and neither did the product: the updater's due rule is right.
  Merging F28-OPEN7 restores the seam. The window widening in `ea9386c6`
  (`43bbfad4` on the PR) was not the fix. It stays, because it is harmless
  and keeps the "exactly two" margin.
- **A missing seam now fails as a missing seam.** A writer updater that
  starts on a moved wall clock logs `root anchor updater runs its due checks
  on a moved test clock` with the offset. A release binary never does, since
  its wall is nil. The test requires that line within 20 s of the seam
  restart, and otherwise fails with "the binary lacks the
  KUDURRU_TEST_ANCHOR_UPDATER_AHEAD seam". This also covers a stale prebuilt
  `KUDURRU_E2E_LICENSED_BINARY`.
- **Expectations come from the published timers.** The test reads the root
  DNSKEY set over UDP, which the acquisition count never matches. It takes
  the original TTL and the signature expiration and computes the RFC 5011
  section 2.3 interval itself: max(1 h, min(15 d, TTL/2, remaining
  validity/2)). It requires:
  - the bootstrap's committed refresh to lie at that interval after the
    acquisition;
  - in the continuing case, the rescheduled refresh to lie at the re-signed
    zone's interval after the second acquisition;
  - the node to have stopped (timed after `node.Stop` returns) before its
    moved clock reaches that rescheduled refresh. This is what makes "exactly 2" a consequence of the timers rather than an
    assumption;
  - in the quarantine case, the bootstrap's refresh to be kept exactly. Its
    third acquisition follows from that.
- **F28-OPEN7 review follow-ups.**
  - `kudurru_root_anchor_updater_acquisitions_total` has a `canceled` outcome
    for an acquisition whose writer context had ended. `refused` now means
    only that the updater stopped for its term on the reply or on a store
    error. The deploy and architecture docs and the metric's HELP text list
    all five outcomes.
  - `settleBootstrap` in `test/e2e` now requires no canceled acquisition and
    at most 2 raced ones (`enrolledBootstrapRaces`), so an acquisition storm
    no longer passes because the metric agrees.

Evidence under `.context/v020-final-20261110/evidence/F28-REVOKE-HOSTED/`:

- `repro01.log`: the old test at `87551fe9` with the seam, hosted shape
  (`taskset -c 0-3`, `-race`). `TestBINDManagedRootRevocation` PASS (2
  subtests, 282.8 s).
- `repro02-noseam.log` (retained failure, the reproduction): the old test
  with a `-tags licensetest` binary whose
  `anchor_updater_wall_licensetest.go` was overlaid empty (`go build
  -overlay`, passed as `KUDURRU_E2E_LICENSED_BINARY`). Both subtests FAIL at
  line 145 with `committed refresh: 1 managed acquisitions, want 3` and
  `want 2`, the hosted failure exactly. named logged 2 TCP DNSKEY
  acquisitions in all, one per bootstrap.
- `interop04-noseam-guard.log` (retained, expected failure): the new test
  with the same seamless binary. Both subtests fail at the seam check with
  the message that names the missing file.
- `interop03-package.log`: the whole `./test/interop` package, `taskset -c
  0-3 -race -count=1 -v`. ok in 418.5 s, every test PASS, and the three Knot
  tests SKIP (no `KUDURRU_KNOT`). The revocation test PASS (284.1 s); the
  published timers were TTL 3600 s and a refresh of 1 h; the counts were 3
  (quarantined) and 2 (continuing).
- `interop11-final.log`: the revocation test at the final head `c7159c4b`,
  hosted shape. PASS (279.6 s).
- `e2e06-settle.log`: `TestEnrolledManagedAnchorsPartitionRestartAndRevoke`
  PASS (222.7 s) and `TestEnrolledManagedAnchorsConflictResolution` PASS
  (109.7 s) under `-race`. Each bootstrap was 1 acquisition, 0 raced.
- `unit01.log` and `race10-anchors.log`: the cmd/kudurru anchor updater,
  status, writer and direct-anchor selections under `-race`. 29 PASS,
  including the new `TestAnchorAcquisitionOutcomeSeparatesCancellation`. One
  FAIL, `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` ("did not
  become ready", 10.13 s, load average about 9). This is the known F28-OPEN7
  finding 1, which also fails at the base. No data race.
- `fix01-mutant.log` (retained, expected failure): the new
  `TestAnchorUpdaterCountsWriterEndDuringQueryAsCanceled`, which ends the
  writer context from inside the root query, run against a mutant whose
  `step` classifies with `context.Background()`. FAIL with `failed:1
  canceled:0`, so the test pins the call site, not only the classifier.
- `fix02-unit.log`: `TestAnchorUpdater*`, `TestAnchorAcquisitionOutcome*`,
  `TestAnchorStatus*` and `TestDefaultBuildHasNoAnchorUpdaterWall` at
  `45ccd2ad`, `-race -count=3`: 57 PASS, ok in 101.8 s, no data race.
- `fix05-interop.log`: the revocation test at `10baf336`, hosted shape.
  PASS (282.3 s; OnlyTrustedKey 152.2 s with 3 acquisitions,
  SecondSeedContinues 129.9 s with 2), TTL 3600 s, refresh 1 h.
  `fix03-interop.log` is the same command without `KUDURRU_E2E=1` and only
  shows a SKIP; it is not evidence.
- `fix04-lint.log`: golangci-lint v2.15.0 on `./cmd/kudurru/...` and
  `./test/interop/...` at the review fixes, 0 issues.
- `race05-restarted-writer.log`:
  `TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture`
  `-race -count=8`, 8 PASS. F28-OPEN7 finding 8 did not reproduce, and its
  cause is still unknown.
- `lint09.log`: golangci-lint v2.15.0 on `./cmd/kudurru/...`,
  `./test/interop/...` and `./test/e2e/...`, 0 issues. `lint07.log` is the
  earlier run, whose one S1024 issue was fixed before the commit was final.
  `pyguards08.log`: bench guards OK (14 tests).

Still open:

- The hosted e2e job stays red on this test until F28-OPEN7 (`43d42037`) is
  on the pull request branch. The test needs that seam, and it is not on the
  branch yet. No hosted run has been made at a head that has both.
- The cap of 2 raced bootstrap acquisitions in `test/e2e` (`enrolledBootstrapRaces`)
  has only local evidence (0 raced in every run here). Watch the first
  hosted run: its failure message prints the counts, so 3 or more raced can
  be judged as a storm or as a bound that is too tight.
- F28-RFC-1 (Knot via docker) and F28-RFC-2 (rollover from real authority
  wires) are not part of this package. The Knot tests SKIP here and must not
  be credited from this evidence.
- F28-OPEN7 finding 1 is unchanged: two managed-recursive readiness tests
  miss their 10 s bound under load. Raising the bound, or changing the
  startup chain, is the integrator's call.
- F28-OPEN7 finding 8 is unchanged: 8 of 8 passed here and the cause is
  unknown.
- F28-OPEN7 finding 9 is unchanged: the full `./cmd/kudurru` run is reserved
  for the integrator.
- F28-OPEN7 new item 2 is unchanged: the `enrolledQuietReview` quiet-period
  heuristic is still how the e2e tests avoid the 503-after-ready window. It
  belongs to the CI-RED-2/3 product item: ready does not yet mean writes are
  admitted.
