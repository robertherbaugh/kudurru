### F28-REJOIN-COUNT Rejoin root acquisitions in the managed-anchors process test — November 13, 2026 (no decision)

`TestEnrolledManagedAnchorsPromoteAndRejoin` failed on the last three hosted
runs (INTEGRATE-18, -20, -21) at `rejoin acquisitions 5` against a bound of 3
to 4. Running the product with debug logs, a temporary log line on each
deny-only acquisition of the follower poller (role, incarnation, settings
resourceVersion, previous identity, offset) and a per-request log of the
stand-in root showed that the old primary makes three root acquisitions after
its partitioned restart. The first is the fenced child's first check (offset
0, mode fenced) while the old primary cannot reach the new primary. The second
is the follower child's first check (offset 0, mode follower) after it
installs the successor. The third is a second check by that follower child
after the settings patch forwarded through the rejoined member (owner
unchanged, settings resourceVersion 377 to 379). Each one follows a rule the
design states (D369; docs/architecture/upstreams-and-cache.md): one child
per member, term and role, each checking once at start, and a changed
settings identity ends the one-hour cooldown of an unchanged owner. The
product acquires no more often than the design allows. The old bound was
written for the F28 lane's flow, where the settings patch through the
rejoined member came later, and the hand merge kept the HA lane's partitioned
restart and the settings patch ahead of the count. The test bound was wrong,
so no product code changed.

The test now synchronizes on each rule and asserts exact counts. It waits for
exactly one check while the old primary is fenced, before the partition
heals. It waits for exactly one more once the old primary follows, before the
settings patch. It absorbs the settings patch with the existing
`absorbSettings` helper (one check per coalesced settings change; the two
patches coalesce or not depending on whether the second lands inside the
follower's one-second wake coalescing, so 1 or 2). At the end it requires
exactly four acquisitions since the absorbed baseline.

The same repeated runs showed a second instance of the same rule earlier in
the flow, shared by both `TestEnrolledManagedAnchors*` process tests. The
forwarded blocking switch (`POST /dns/blocking` through the follower) writes
`Kudurru/default` status, so the follower checks the root again for the
changed settings identity. Depending on when its five-second pull installs
the switch, that check either coalesced into the first settings patch's
absorption window or landed just before the window's baseline was taken. In
the second case it was never absorbed, and the later `partition` check failed
with one acquisition too many (run02, and diag09/diag10, where the root log
puts the check 0.7 s and 0.13 s before the settings patch started, with
settings resourceVersion 1 to 83). The test now waits for exactly that one
check after both nodes serve the switch and adds it to the baseline.

Evidence under `.context/v020-final-20261110/evidence/F28-REJOIN-COUNT/`:

- `diag01.log` and `diag01/` (temporary instrumentation, not committed):
  attribution of the three post-restart acquisitions described above. The run
  failed at the old bound, as on hosted CI (`rejoin acquisitions 5`).
- `run02.log` to `run08.log` (first commit, `-race`; 05 to 08 under
  `taskset -c 0-3`): the new exact fenced and follower checks passed in every
  run that reached them (run03, 04, 05, 08). The full test passed in run05
  (233.8 s) and run08 (243.7 s). Failed runs that were kept: run02
  (`partition: 5 root acquisitions, want 4`, the blocking-switch race above);
  run03 and run04 (a forwarded UPDATE through the rejoined member answered
  REFUSED, the HA lane's held-across-install item, also seen in CI-RED-2);
  run06 (`UPDATE on enrolled-b answered SERVFAIL`, the HA lane's
  forwarded-UPDATE SERVFAIL under load); run07 (`refused partitioned write was
  applied or replayed`, a one-shot DNS check in the HA partition step).
- `diag09.log`, `diag10.log` and their directories (instrumented): both
  failed at `partition: 4 root acquisitions, want 3`. The root and poller logs
  attribute the extra check to the blocking switch's settings write.
- `run13.log` to `run16.log` (second commit, both
  `TestEnrolledManagedAnchors(PromoteAndRejoin|PartitionRestartAndRevoke)`,
  `-race`, `taskset -c 0-3`): `PartitionRestartAndRevoke` passed in run13
  (210.2 s), run14 (179.2 s) and run16 (214.7 s). `PromoteAndRejoin` passed in
  run13 (268.5 s) and run14 (253.5 s). Failed runs that were kept: run15 (both
  tests failed in the forwarded UPDATE steps before any anchor count: a burst
  UPDATE answered REFUSED, and `UPDATE on enrolled-b answered SERVFAIL`);
  run16 `PromoteAndRejoin` (`UPDATE on enrolled-a answered SERVFAIL, want
  NXRRSET`, after the new fenced, rejoin and settings checks had passed). All
  are HA lane forwarded-UPDATE items. Every run that reached a new check
  passed it: the blocking-switch check 6 of 6, the fenced and rejoin checks 7
  of 7 (runs 03, 04, 05, 08, 13, 14, 16), and the final `rejoin acquisitions`
  check (now exactly 4) 4 of 4 (runs 05, 08, 13, 14). No test was skipped.
- `lint01.log`: golangci-lint v2.15.0 on `./test/e2e/...`, 0 issues.

Review follow-up: an independent review (`review-run01.log` to
`review-run06.log`, `-race`, `taskset -c 0-3`) passed
`PartitionRestartAndRevoke` 3 of 3 (197.8 s, 199.3 s, 196.0 s) and
`PromoteAndRejoin` 1 of 6 (run06, 238.5 s, final `n != 4` check passed).
Every new anchor check passed whenever reached (blocking switch 7 of 7,
fenced and rejoin 4 of 4). The five failures were HA lane steps: a burst
UPDATE through the rejoined member answered REFUSED (run01, 03, 04),
`UPDATE on enrolled-b answered SERVFAIL` (run05), and a new one,
`POST /objects/dnszones: status 503` at 3.6 s during setup (run02). The
architecture page (docs/architecture/upstreams-and-cache.md) now states that
the settings identity is the `Kudurru/default` UID and resourceVersion, so a
status-only write such as the blocking switch makes each running deny-only
member check the root once, paced only by the one-second wake coalescing.
Whether the cooldown should bind the settings spec only (root-server
politeness) is a follow-up for the integrator.

Open: hosted CI has not yet run with these commits; the first hosted e2e job
should confirm that `rejoin acquisitions` no longer fires. The settings-patch
absorption is still a 1-or-2 range by design (coalescing of the two patches
`enrolledForwardedSettings` sends), as is the earlier forwarded settings
change, so the final `== 4` is exact relative to that absorbed baseline. The
shared helper was left unchanged because F28-OPEN6 edits the same files. Making it exact would need a single patch or a status field
naming the last checked settings identity. The forwarded UPDATE that answers
REFUSED or SERVFAIL through a just-installed follower, and the one-shot
partitioned DNS check, are HA lane items that can still fail this test
before it reaches the anchor counts.
