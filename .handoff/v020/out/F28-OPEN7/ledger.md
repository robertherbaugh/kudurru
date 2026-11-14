### F28-OPEN7 The held managed-anchor package, exact root-check counts and DNS across a persisted fence — November 13, 2026 (D369)

Lane `wip/v020/f28`, base `7b11a139`. The eleven held F28-OPEN6 commits were
cherry-picked one at a time (`533e7104`, `153320fd`, `6e63eb03`, `18913b79`,
`1e692b23`, `e29b7c26`, `43d42037`, `e1fda92a`, `90cdcb98`, `04aff32f`,
`faae0487`), followed by this package's source commits `131a236a`,
`2c636e66`, `75f8f07f`, `a09860c6`, `74e41d81`, `2fd09d9c`, `09a9d6ec` and
`b22b24c1`, and the review fixes `f43b2f77`, `3afb1485`, `9bf55b66`,
`6d3aa571` and `87551fe9`. No generated file changed (no `chore(gen)`
commit; the new metric has no generated reference), no new dependency.

What now happens:

- **The held package is back without its count regression.** Two picks
  conflicted. `serving_runtime.go` kept both sets of test-only fields. In the
  promote flow, the HA lane's partitioned restart of the old primary is kept:
  the restarted old primary is now sampled from the start of its process
  (before it is fenced) through the fenced restart checks, the heal, the
  install of the successor and its follower role, and the exact
  F28-REJOIN-COUNT checks (one fenced check before the partition heals, one
  follower check after the install) are kept as they were. 8ff9aa69 had
  changed `absorbSettings` to take the follower; the rejoin call is updated
  (`131a236a`). The integrator's
  earlier merge (`c7432461`) had dropped the "forwarded blocking switch" wait
  of F28-REJOIN-COUNT; that is what failed all three tests with "partition: 4
  root acquisitions, want 3" in `INTEGRATE-26/e2e.log`: the TCP log there
  shows the bootstrap, the follower's start check, the blocking switch's check
  about two seconds before the settings change, and the settings check. On
  this tree the wait is present and that check is counted exactly.
- **Every root-check count is exact, the bootstrap included.** The
  standalone bootstrap was accepted as 1 to 4 acquisitions (and with no
  bound in the conflict flow), because an ordinary startup commit that lands
  while the reply is in flight makes the updater discard it and acquire
  again. The elected updater now counts its acquisitions by outcome in
  `kudurru_root_anchor_updater_acquisitions_total{outcome}` (`applied`,
  `raced`, `failed`, `refused`; `3afb1485`), and both flows require exactly
  one applied acquisition plus the raced ones that counter reports, none
  failed or refused, every one in normal mode (`settleBootstrap`,
  `9bf55b66`). In every final run the bootstrap was exactly 1
  acquisition, 0 raced, in every test. A forwarded settings change was
  absorbed as one or two checks, because the two patches of
  `enrolledForwardedSettings` either shared the follower's one-second wake
  coalescing or did not. The helper now hands each patch to the managed
  variant, which reads the count before the patch, sends it (the follower
  answers only once it serves the change) and requires exactly one check,
  settled for five seconds, before the next patch. Both forwarded changes and
  the change through the rejoined member are exactly two checks in every run;
  the partition, follower restart, promotion, fenced, rejoin and final
  `rejoin acquisitions` (exactly 4) checks are unchanged and passed whenever
  reached.
- **Managed members keep validated DNS while a fence is persisted (product
  fix).** Diagnostic runs (`diag05`, temporary instrumentation in the binary,
  not committed) showed the SERVFAIL at conflict detection: writes blocked,
  mode still primary, the managed input refused with "anchor lifecycle
  unavailable". `ObserveAuthority` blocks writers and retires every elected
  term, then persists the new authority (an fsync), then publishes it, and
  the anchor source read the write block itself as unavailable. The store now
  marks that transition of an unblocked store as in flight (`fenceInFlight`)
  and DNS output reads the block in its DNS form (`Store.DNSServingAuthority`):
  the managed and static anchor inputs and root admission stay current, so
  recursion and zone answers continue from the served publication until the
  DNS-only cut takes over. Writers, protected HTTP, publication and the updater
  still see the store blocked. A fence whose write fails keeps DNS blocked, and
  a retry of it does not reopen DNS. The same applies to the divergence
  install. D395's served-publication grace was not used for this: it would also
  admit an input retired by a revocation or owner change.
- **The samplers no longer tolerate SERVFAIL.** Once the node reports a
  publication (ready or DNS-only), every sample must be validated, and no
  answer at all fails too. The only window allowed is the start of a process,
  before its first publication (its listeners answer SERVFAIL until then,
  0.8 to 1.2 s in these runs, logged). A SERVFAIL whose extended error carries
  the resolver's "no name server of the zone answered" (the stand-in root, in
  the loaded test process, timed out; `e2e-final-1`) is counted and logged as
  an upstream failure, not failed; a refused anchor input answers SERVFAIL
  without that text. The promote variant now also samples the follower across
  its own promotion. Both exemptions are now bounded (`9bf55b66`): the
  startup gap may last 5 s in all per sampler (0.65 to 2.0 s observed), and a
  root timeout counts only when the resolver's wrapped cause is "context
  deadline exceeded", at most two per sampler and never two in a row; any
  other non-validated sample fails. No final run used the root-timeout
  exemption.
- **Readiness of a managed recursive node: cause measured, directive not
  met.** Direction item 2 asked to find and fix why a fresh managed recursive
  node becomes ready slowly. The cause named before ("every startup commit
  cancels and restarts the preparation") is not what costs the time. Two
  instrumented runs (`fix-diag01-startup-commits.log`,
  `fix-diag02-startup-joins.log`: every generation commit with its caller,
  every preparation with its start, cancel reason, join and result; an
  overlay, nothing committed) show the phase-0 startup as a serial chain:
  the startup writes (seed, admin user, controller and list statuses, about
  2.5 s), the writer start and the anchor bootstrap commit (the gate holds
  the other writer services until the first step, so that the bootstrap
  is one acquisition), then the primary driver's three lifecycle transitions
  of the new zone (one commit each, about 1.0 to 1.6 s apart, each after a
  full image re-read), then one full preparation (1.1 to 1.8 s under race),
  then the zone's admission. A preparation cannot be published across a
  later commit (the token CAS is the publication invariant), so the first
  publishable preparation starts after the last lifecycle commit whether or
  not earlier ones were canceled: canceling them costs no time to ready.
  The cancellations by `Wake` and provider-dependency changes seen in the
  same runs (up to 1 s of work each) end before the bootstrap or before a
  lifecycle commit that would supersede them anyway. On this machine (load
  15 to 28 on 8 cores, `-race`) the chain takes 8.5 to 12 s, and the
  10-second bound fails: both tests failed in both instrumented runs and in
  every review run (12 of 12), at the base as well (`diag03`, `diag07`).
  Shortening the chain needs either the bootstrap and the lifecycle work to
  overlap (the bootstrap would then race them and the root count would no
  longer be one) or the primary driver to make several lifecycle
  transitions per pass (a primaryruntime change outside this package). The
  readiness bounds were not changed. This needs an integrator decision:
  accept the two tests as load-bound (and run them at lower parallelism),
  raise their readiness bound with a recorded reason, or schedule the driver
  change.
- **Smaller items.** The divergence install's half of the fence fix now has
  store tests (`f43b2f77`): a same-revision image with a different digest is
  installed on a static follower while its authority write is held by an exec
  hook on the writer connection; the anchor input stays current and DNS
  unblocked throughout, and a failed authority write leaves writes and DNS
  blocked. The live edge primary now settles its startup cascade read-only
  before it signs in (`87551fe9`). Setup writes after a new zone wait until
  the source has been quiet for two seconds (`9bf55b66`). An early return of
  the direct writer in `TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture`
  now names the term context's error and cause and the authority mode
  (`6d3aa571`). The retired follower's not-ready check now requires an
  explicit 503 from `/healthz/ready` (a silent listener returned 0 and passed).
  The hosted-CI data race in `TestAnchorEdgeActivationJoinedFollower` is gone:
  every runtime fixture gets a private data directory before the runtime is
  built, and `joinPrimaryOn` no longer writes the configuration of a running
  runtime. The first sign-in on the recovered primary uses the 35-second
  request timeout of the shared flow's follower login (it exceeded five
  seconds in `e2e01`).

Evidence under `.context/v020-final-20261110/evidence/F28-OPEN7/`:

Review fixes (on `87551fe9`; the review's own runs are `review-*.log`):

- `fix-store01-divergence.log` (retained): the first form of the divergence
  tests held the SQLite lock from a second connection, which blocks the
  install's immediate transaction before it reaches the divergence; both
  failed as fixtures. `fix-store02-divergence.log`: the four
  `TestAnchorInputStays*` store tests, `-race -count=3`: 12 PASS.
  `fix-mut01-divergence.log`: with the in-flight mark removed from the
  divergence install (overlay), `TestAnchorInputStaysCurrentWhileADivergenceIsPersisted`
  FAILS ("input current false, DNS blocked true").
- `fix-race01-acquisition-counter.log`: `^(TestAnchorUpdater|TestAnchorStatus|TestAnchorCaptureRace)`,
  `-race`: 17 PASS (the raced-bootstrap test requires 1 raced and 1 applied,
  the terminal-updater test 1 refused).
- `fix-diag01-startup-commits.log`, `fix-diag02-startup-joins.log`: the
  readiness timelines above (`TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart`
  with an instrumentation overlay, `-count=2` each: 4 FAIL on the 10-second
  bound).
- `fix-edge-live01.log` (retained): run without `KUDURRU_ANCHOR_EDGE_WORKDIR`,
  6 setup FAIL, nothing started. `fix-edge-live02.log`:
  `TestRunServeManagedEdgePinnedDaemonFollower` and
  `TestRunServeManagedEdgePinnedDaemonRestart`, live pinned dnsdist,
  `-race -count=3`: ok in 418 s (6 PASS, not `-v`), no container left.
- `fix-race02-cmd-anchors.log`: the anchors selection below, `-race`, once:
  140 PASS, 0 SKIP, no data race, 2 FAIL:
  `TestOfflineAnchorRecoveredTrustServesAfterRestart` and
  `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` (the 10-second
  readiness bound, above); `TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture`
  passed there and 4 of 4 in `fix-race03-restarted-writer.log`.
- `fix-e2e-1.log`, `fix-e2e-2.log`, `fix-e2e-3.log`: all
  `TestEnrolledManagedAnchors*`, `-race`, `-v`: all four PASS in all three
  runs (Partition 265.0, 208.4, 242.0 s; PromoteAndRejoin 269.2, 242.4,
  268.6 s; OfflineRecovery 247.0, 238.4, 248.9 s; Conflict 106.5, 107.0,
  106.1 s), no skip. Every bootstrap was 1 acquisition, 0 raced; every
  settings change exactly one check; no failing sample after a first
  publication and no root-timeout exemption used (follower across its
  promotion 595 to 840 samples; restarted old primary 1505 to 1935, 181 to
  230 DNS-only, startup gap 0.65 to 1.14 s; conflict members 490 to 988, 432
  to 650 DNS-only).
- `fix-lint01.log`: golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./test/e2e/...`, `./internal/store/sqlite/...`: 0 issues.

Earlier runs of this package:

- `store01.log`: `TestAnchorInputStaysCurrentWhileAFenceIsPersisted`,
  `TestAnchorInputStaysRetiredAfterAFailedFence` and the six
  `TestAnchorPublicationCurrent*` tests, `-race`: 8 PASS. `mut01-store.log`:
  with the old block check (overlay), the first FAILS ("input current false,
  DNS blocked true").
- `race06-fence-window.log`: `TestRootAnchorManagedRecursionAcrossAPersistedFence`
  and `TestRootAnchorManagedRecursionStopsAfterAFailedFence`, `-race
  -count=3`: 6 PASS (198 to 220 validated UDP and TCP samples per run, 57 to 60
  of them while the fence write was held for 1.5 s). `mut03-fence-window.log`:
  the first FAILS at sample 0 with the old block check. `race02`-`race05`
  (retained) are fixture fixes: the fixture's canonical settings overrode the
  recursive group (answers came from the default forwarders) and the lock
  holder needed a busy timeout.
- `race08-sqlite-serving.log`: `go test -race ./internal/store/sqlite
  ./internal/serving`: ok (739.8 s, 40.6 s).
- `race07-cmd-anchors.log` (retained): the anchors selection failed four join
  tests ("invalid or untrusted member identity") after the first data-race fix;
  fixed in `2fd09d9c`. `race09-cmd-anchors.log`: `-race` selection
  `^(Test.*Anchor|TestManagedTopology|TestManagedEdges|TestServingRuntime|TestRootAnchor|TestClusterResolve|TestClusterJoin|TestDefaultBuildHasNoAnchorUpdaterWall)`
  over `./cmd/kudurru ./internal/config`: 140 PASS, 0 SKIP, no data race, 2
  FAIL: `TestOfflineAnchorRecoveredTrustServesAfterRestart` (10-second
  readiness, see above) and `TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture`
  ("direct writer stopped before caller cancellation"); `diag07` reran the
  latter 5 of 6 PASS, the sixth failing its 3-second "restarted writer did not
  capture" wait. `TestAnchorEdgeActivationJoinedFollower`,
  `TestRunServeDirectAnchorsConflictServesAcrossUnrealignedStatusWrite`
  (now on a tree with `6780f27d`), the three `TestClusterResolve*` and
  `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` passed there. The
  pinned-dnsdist edge tests were not in this selection.
- `race01-recursive.log`, `diag02`-`diag04`, `diag07`: the readiness
  measurements above.
- `e2e01-picked.log` (retained): the picked tree before this package's test
  changes: Partition PASS, PromoteAndRejoin FAIL (forwarded UPDATE through the
  rejoined member answered REFUSED), OfflineRecovery FAIL (sign-in timeout),
  ConflictResolution PASS with one 57 ms SERVFAIL on C.
  `diag05-e2e-servfail.log` (instrumented, retained): diagnosis of that gap;
  PromoteAndRejoin failed in the first form of the per-patch count (count read
  after the patch). `diag06-e2e-after-fix.log` (instrumented): both tests
  PASS, no gap after publication, one 891 ms startup gap.
- Final runs, `KUDURRU_E2E=1 go test -race -count=1 -run '^TestEnrolledManagedAnchors' ./test/e2e`:
  - `e2e-final-1.log` (before `09a9d6ec`): Partition PASS 303.9 s,
    PromoteAndRejoin FAIL 263.4 s (one stand-in root timeout, EDE 9 "no name
    server of the zone answered", long after every transition; this is what
    `09a9d6ec` classifies), OfflineRecovery PASS 243.6 s, Conflict PASS 102.5 s.
    `e2e-final-2-stopped.log`: stopped by hand to rerun on the final commit.
  - `e2e-final-3.log`: Partition PASS 264.2 s, PromoteAndRejoin PASS 256.1 s,
    OfflineRecovery FAIL 230.5 s, Conflict PASS 96.2 s.
  - `e2e-final-4.log`: Partition PASS 223.3 s, PromoteAndRejoin PASS 255.3 s,
    OfflineRecovery FAIL 237.0 s, Conflict PASS 105.6 s.
  - `e2e-final-5.log`: Partition PASS 217.8 s, PromoteAndRejoin PASS
    256.6 s, OfflineRecovery FAIL 228.2 s, Conflict PASS 110.5 s.
  The three OfflineRecovery failures were "enrolled read refused
  /cluster/anchors 401". `diag08-recovery-401.log` (a temporary log of each
  status read) placed it on the follower, 2.1 s after the new sign-in on the
  recovered primary: the session was committed after the follower had
  installed the recovered image, so the follower knew it only after its next
  five-second pull. The test now waits (at most four pulls, retrying only 401
  and 503) until the follower accepts the session before it reads the
  follower's status (`b22b24c1`). Every anchor check before that read passed
  in all three runs, including the partition count.
  - On the final commit: `e2e-final-6.log`: all four PASS (221.0 s, 283.5 s,
    236.3 s, 107.4 s). `e2e-final-7.log`: all four PASS (235.5 s, 282.4 s,
    260.6 s, 104.1 s). `e2e-final-8.log`: Partition
    PASS 213.6 s, PromoteAndRejoin FAIL 4.8 s (setup: the fresh standalone
    primary answered `POST /objects/dnsrecordsets` 503 "unavailable" right
    after it reported ready, before any anchor step of the test; this is the
    open "readiness does not imply writer admission" window below, for a
    writer before its bootstrap), OfflineRecovery PASS 270.7 s, Conflict PASS
    110.4 s.
  In every final run each settings change was exactly one check, and no
  sampled member had a failing sample after its first publication except the
  one stand-in root timeout of `e2e-final-1` (follower across its own
  promotion 546 to 807 samples; restarted old primary 1245 to 2049 samples,
  187 to 238 DNS-only, with a startup gap of 0.8 to 2.0 s before its first
  publication; conflict members 547 to 1028 samples, 439 to 681 DNS-only). No
  test was skipped.
- `lint01.log`, `lint03.log`, `lint04-e2e.log`: golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./test/e2e/...` and `./internal/store/sqlite/...`: 0 issues;
  `lint02-licensetest.log` (`--build-tags licensetest`, `./cmd/kudurru/...`):
  0 issues. `bench01.log`: 14 bench guard tests OK.

What remains open:

- Direction item 2 is not met: a fresh managed recursive node's readiness is
  bound by the serial startup chain described above, and
  `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` and
  `TestOfflineAnchorRecoveredTrustServesAfterRestart` keep failing their
  10-second bound on this loaded machine, at the base as well. The bounds
  were not changed; the integrator decides (see above).
- The full `./cmd/kudurru` package has not been run with `2fd09d9c` (every
  runtime fixture gets a private data directory); the integrator runs it
  through `hack/go-test.py` after the pick, and a new fixture failure there
  belongs to that commit.
- `TestDirectAnchorRestartedWriterStartsServicesBeforeFirstCapture` failed 2
  of 7 earlier runs; it passed in every run since (5 of 5 in review,
  5 of 5 here) and now reports what ended the writer if it fails
  again. The cause is not known.
- The startup gap (SERVFAIL before a node's first publication) remains a
  product item; the samplers now bound it at 5 s.

- A forwarded UPDATE through the just-rejoined member can still answer
  REFUSED or SERVFAIL (`e2e01`; HA lane item, unchanged).
- The items F28-OPEN6 listed as open and not touched here stay open:
  readiness does not imply writer admission for a writer before its
  bootstrap or with a due refresh (CI-RED-2 and CI-RED-3; `e2e-final-8`
  failed on a write between a new zone's commits and their publication,
  which the fixture now waits out), the retire-on-REVOKE-then-promote sequence is not run, an
  abandoned-term member is not brought back through the production puller,
  and the status route's projection of a real puller refusal is proven only
  in-process.
