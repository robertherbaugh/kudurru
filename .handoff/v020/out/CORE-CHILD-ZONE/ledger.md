### CORE-CHILD-ZONE primary parent and child zones on one node — November 13, 2026 (D404)

**What was wrong.** On a SQLite node a primary zone `example.` and a primary
zone `child.example.` could leave the child, the parent or both answering
SERVFAIL until the process restarted (readiness: `zone awaiting committed
publication`). The cause is a cycle of two exact checks, not a timing window:

1. Every committed-status write advances a `DNSZone`'s resourceVersion, so a
   zone's `ZoneLifecycle` receipt is often behind its `DNSZone` until the
   driver realigns it.
2. Fencing a child in its parent (`parent-fence`, `late-parent-fence`,
   `parent-release`) and the child's lifecycle transition that records the
   obligation required the *exact* receipt of both zones, in the SQLite
   adapter (`sqliteadapter.source`) and again in
   `currentguard.checkBackground`.
3. Realigning either receipt is itself a lifecycle CAS, and its membership
   check requires exactly that fence.

So a parent whose receipt was behind could never fence a new child, and a
child whose receipt was behind (a status write before its fence, a late
parent, or a child deleted and created again while the parent's receipt was
behind) could never be fenced. The driver retried with backoff for ever. This
is why the commit-gap package saw "answers within about 0.9 s or never": it
depended on whether a status write landed between the child's creation and its
fence. A restart re-read the same state and usually won the race before the
next status write. It predates D395.

**What now happens.** The commands that change only a parent/child boundary
need the same zone (object name, UID, apex, type, a source that is not
removed), not the exact receipt: the three boundary admissions with a parent,
and a lifecycle transition that changes nothing but `Sequence` and `Parents`
(`boundarySource`, `boundaryTransition`, `boundaryCommand`; the adapter marks
the guard's check with `BackgroundCheck.Boundary`, and the guard accepts that
flag only for those reasons and refuses it for any other). They never advance
the receipt; every other command, and the realignment itself, keep the exact
check. DNS also keeps answering across the pair's own transitions: a parent
with a desired child that is not yet fenced keeps serving its committed head
(the child's lifecycle entry or desired-only coverage is the longer routing
match at and below its apex) but is not admitted, so UPDATE, NOTIFY, status
and publication acknowledgment wait, as D395 does for a zone's own write; and
a child whose obligation is witnessed by a parent that is still Fencing (a late
parent) keeps serving and stays admitted, while that parent serves only once
Active. `docs/architecture/zones.md` describes the rule with a sequence
diagram. The mounted commit-gap test carries its child zone again.

Evidence under `.context/v020-final-20261110/evidence/CORE-CHILD-ZONE/`:

- Reproduction at the lowest layer, `internal/primaryruntime/driver`
  (`ancestry_receipt_test.go`, real SQLite adapter, deterministic):
  `TestLateChildConvergesWhileParentReceiptIsBehind`,
  `TestPreparingChildConvergesWhileItsReceiptIsBehind`,
  `TestLateParentConvergesWhileChildReceiptIsBehind` and
  `TestParentAndChildConvergeUnderStatusWrites` (both orders, child deleted
  and recreated three times, a status write after each lifecycle change).
  Before the fix `test01`/`test05`: every one fails (8 s bound, never
  converges; 3 of 3 for the last). After: `test03`/`test04` pass in 0.3 to
  4.4 s. (`test01`'s last test was a first version whose writer wrote every
  5 ms; that writer itself never settles, since each realignment triggered a
  new write, and it was replaced by the per-change writer; `test03`'s failure
  of it is that version.)
- `internal/servingbuild` `TestPrimaryParentAndChildServeAcrossEachOther`
  (real driver at production intervals, real committed-status controller,
  real complete publisher; both orders; name-server writes of each; three
  delete/recreate rounds; samples the DNS publication every 1 ms). Before
  (`test07`, all five source files reverted): 4 of 4 subtests fail, 3 stuck
  for 60 s, 1 with the parent unserved after a child's creation and the child
  unserved under a fencing parent. After: `test06` 6 of 6 subtests pass, no
  unserved sample. `repro01` (stuck in round 4 and 3) and `measure05`/`06`
  (gaps up to 0.81 s for the parent and 0.49 s for the child with only the
  adapter fix or none) are the earlier versions of this test; `measure01` to
  `03` measured `CaptureHTTP` instead of the DNS capture and are not DNS
  evidence.
- `internal/primaryruntime` `TestImageLateParentFencingKeepsChildServing`
  (new) and `TestImageLateDesiredChildBeforeLifecycleBlocksParent` (updated to
  the new rule: the parent is not admitted but keeps serving outside the
  child): both fail with the image change reverted (`unit03`).
  `internal/primaryruntime/currentguard`
  `TestBackgroundBoundaryAcceptsSameZoneBehindItsReceipt` and
  `internal/primaryruntime/sqliteadapter`
  `TestBoundaryTransitionAndCommandAreNarrow` pin the narrowness of the rule.
- Mounted `runServe`: new `TestRunServeSQLitePrimaryParentAndChildZones`
  (`cmd/kudurru/serve_child_zone_test.go`, both orders, name-server writes of
  each, two delete/recreate rounds of the child, 5 ms UDP probes of a name of
  each zone from its first answer until deletion, no SERVFAIL, no wrong
  answer, no silence without the race detector, every first answer within
  the 60 s mounted bound, three minutes under race) and `TestRunServeSQLiteZonesAnswerAcrossCommits` with its
  child zone back. Before (`mount03`, overlay `overlay-before.json` of the
  five source files at `01cf27b1`): 4 of 4 subtests never reach the first
  answer of the child or the parent (`zone awaiting committed publication:
  child.family.example.`). After, without the race detector: `mount01` 3 of 3
  commit-gap runs pass (0 of about 9,800 to 11,500 probes per name without a
  correct answer, child included), new test 5 of 6 subtests pass with 0
  failed probes and 1 failed on a setup POST answered 503 `current writable
  serving generation unavailable` (not DNS; the test now asks such a POST
  again and accepts 409 on the repeat); `mount02` 6 of 6 subtests pass, 0 of
  1,591 to 6,236 probes per name without a correct answer. Under the race
  detector (`race03`, 2 runs each): the new test 4 of 4 subtests,
  the commit-gap test 2 of 2 (0 of about 23,000 to 31,000 probes per name),
  `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites` 2 of 2,
  `TestRunServeSQLiteCompletePrimaryUpdateRestart` 2 of 2 pass.
  `hack/go-test.py` runs the new SQLite test a second time without the race
  detector (`NORACE_TESTS`).
- Kubernetes driver (envtest): new `TestRunServeKubePrimaryParentAndChildZones`
  runs the same sequence. The Kubernetes driver has no lifecycle driver and
  never had the cycle; `mount02` 6 of 6 and `race03` 4 of 4 subtests pass,
  0 failed probes. The `kube` variants of the `internal/primaryruntime`
  harness tests pass with envtest (`unit02`, `race02`).
- Package runs: `unit04` (no race) and `race02` (race; `race01` is a retained
  failure where the shared build-cache trim removed files mid-build, not a
  test failure): `internal/primaryruntime/...`, `internal/servingzones`,
  `internal/servingbuild` all pass. Lint `lint01`
  (`./internal/primaryruntime/... ./internal/servingbuild/... ./cmd/kudurru/...`):
  0 issues. Bench guards: 14 tests OK.
- Instrumentation used to find the cause (never committed): `dbg/work.go`
  (per-step driver log) with `dbg/overlay-driver.json`; `dbg03` shows the
  stuck state (child `preparing`, obligation commit 0, child and parent both
  `admission conflict` for more than 35 s, parent receipt not matching), `dbg06` the
  second cycle in the guard after the adapter-only fix.

- Review fixes (after the "accept-with-fixes" review, `review-*.log`):
  - `TestParentAndChildConvergeUnderStatusWrites` was flaky under the race
    detector at the shared 8 s `await` (`review-06`, `review-08`: 2 of 4
    race runs failed at the pair-admitted wait after a child's creation). It
    is slow, not stuck. A per-step driver log (`fix-01`, `fix-06`, overlay
    `dbg2`/`dbg3`, never committed) attributes the time: each driver step is
    several SQLite transactions and took 0.3 to 1 s under race; the child's
    Fencing step (Prepare, fence, Activate, publish) exceeded the fixture's
    1 s operation timeout and was retried after the 1 s backoff; and the
    parent's source reconcile was refused (`admission conflict`) up to three
    times in a row while the child's boundary moved, each refusal adding a
    1 or 2 s backoff for the parent. The test now has its own one-minute
    bound (as the `servingbuild` pair test) and logs every wait over 3 s.
    `fix-16`: 10 of 10 consecutive race runs pass (17.5 to 29.8 s each,
    longest single wait 11.6 s; load 11 to 15). The other three tests of
    `ancestry_receipt_test.go` then failed the same way in a parallel race
    run of the touched packages at load about 28 (`fix-18`, retained), so
    all four use that bound (`awaitConverged`). With the base source
    overlaid all four still fail, at the one-minute bound (`fix-22`); the
    driver package passes 3 of 3 race runs (`fix-23`).
  - `servingbuild` `TestPrimaryParentAndChildServeAcrossEachOther` failed
    once in `fix-18` with `child.example. unserved during "child
    ns1.child.example."`. The log shows the cause is the test: it recreated
    the child 300 ms after deleting it, the deletion had not finished under
    race, so the next round's wait passed at once on the deleted
    incarnation ("pair 1 served after 0s", never seen in earlier runs),
    watched it, and recorded its retirement as a gap of the new child. The
    test now waits until the deleted child has stopped serving before
    recreating it; the gap check is unchanged. `fix-24`: 3 of 3 race runs
    pass (6 of 6 subtests, no gap).
  - Tried and reverted: folding a digest of every zone below a primary
    parent into the parent's retry cursor, so a parent's backoff resets
    when its child commits (`880e4e2e`, reverted by `8ffd0610`). It did not
    help: mounted race first answers of the recreated child were 13 to 37 s
    with it (`fix-07`) and 7 to 35 s without it (`fix-08`); in the driver
    test the race waits were the same (`fix-04` without, `fix-05` with); and
    in 20 alternating race runs of
    `TestActualConcurrentCreationTypeReplacementAndMovedApex` it was slower
    (median 16.8 s against 15.0 s on the base source) and failed once at the
    8 s `await` (`fix-15`), because retrying the parent on every child commit
    puts more refused parent steps in front of the child in the one serial
    loop.
  - Mounted latency under race (review finding: a recreated child took up
    to 50 s against a 60 s bound). The instrumented mounted run `fix-14`
    (per-step log, load up to 34) attributes it: no step waited out a retry
    backoff (0 skipped steps in 127); the serial driver steps every zone in
    turn, and each step took 1 to 10 s under race (median 1.6 s), including
    the republish and publication wait of the unchanged `root` zone, so a
    child's four lifecycle steps sit behind several steps of every other
    zone, and a step can hit the production 10 s operation timeout. That run
    is a retained failure: the 60 s bound expired while the child was still
    progressing (in Fencing). The mounted parent/child test now allows three
    minutes for a first answer under race (`awaitMountedDNSResultWithin`;
    every other caller keeps one minute) and still logs first answers over
    5 s. `fix-17`: see below.
  - `TestActualConcurrentCreationTypeReplacementAndMovedApex` (existing,
    failed once in `review-06`): 20 alternating race runs of the base source
    and of the D404 source at equal load (`fix-21`, two prebuilt binaries
    run in turn): base 20 of 20 pass, median 10.1 s (6.4 to 21.7 s); D404 20
    of 20 pass, median 10.2 s (7.8 to 20.8 s). D404 does not lengthen the
    type-replacement path; the review failure is load against the shared 8 s
    `await` that this existing test still uses (not changed here).
  - Boundary refusals through the real SQLite transaction: new
    `sqliteadapter` `TestBoundarySourceRefusesAnotherZoneThroughTheTransaction`
    puts both receipts behind with a status write, captures the runtime's
    real `parent-fence` command and drives it and the child's
    obligation-only `LifecycleCAS` through the adapter. The admission is
    refused for a replaced UID, a retyped zone or a deleted DNSZone of the
    parent or the child; the CAS is refused for the same changes of the
    child; the same zones are admitted (`fix-11`, 12 subtests pass). An apex
    cannot change in place (the store refuses a `zoneName` edit), which a
    subtest pins, so a moved apex is a replaced UID. On the base adapter
    the "same zone" case and every CAS case fail with `admission conflict`
    (`fix-12`). The child's CAS reads only the child's own source, as before
    D404: on the base adapter with exact receipts, a parent retyped or
    deleted after its fence also does not refuse the child's CAS (`fix-10`);
    the parent's retirement then makes the obligation obsolete
    (`obsoleteParent`).
  - Mounted under race on the final source (`fix-17`, load up to 28):
    `TestRunServeSQLitePrimaryParentAndChildZones` passes (first answers
    11 to 34 s, 0 failed probes). `TestRunServeSQLiteZonesAnswerAcrossCommits`
    is a retained failure: a setup `POST /objects/blocklists` was answered
    503 `current writable serving generation unavailable` (the command-path
    refusal CMD-RECAPTURE, D399 owns; not DNS), and its first answer took
    57.7 s against its own one-minute bound, which this package did not
    change.
  - Package and lint runs on the final source: `fix-25` (race, parallel,
    `./internal/primaryruntime/... ./internal/servingzones
    ./internal/servingbuild`): every touched package passes
    (`primaryruntime`, `currentguard`, `driver`, `sqliteadapter`,
    `servingzones`, `servingbuild`); the untouched
    `internal/primaryruntime/network` fails
    `TestActualNotifyAncestorExpiryInvalidatesBoundChildWithoutNewGeneration`
    (`context deadline exceeded`), which also fails 1 of 3 on the base
    source (`review-09`). `fix-26` (lint of
    `./internal/primaryruntime/... ./internal/servingbuild/...
    ./cmd/kudurru/...`: 0 issues), `fix-19` (lint before the last test commit: 0
    issues), `fix-13` (driver and adapter without race: pass), bench guards
    `fix-20` (14 OK).

**Still open.** Convergence is bounded but not fast on this shared machine.
Without the race detector a new child took up to 12 s to answer in the
mounted test (a single new primary 5 to 8 s in the same runs, load 15 to 18);
under the race detector 7 to 50 s, against 0.4 to 1.9 s at the `servingbuild`
layer. The cause is the serial driver: it steps every zone in turn and every
step republishes and waits for publication, even for an unchanged active
zone, so a node with more zones makes a new child wait longer. Shortening
that (for example skipping the publication wait of a zone whose installed
member is already acknowledged) changes the driver's acknowledgment rule and
was not attempted here. Retry backoff is not the cause (`fix-14`), and
resetting it from children was measured and reverted. The existing driver
tests still use the shared 8 s `await` and can fail under race on a loaded
runner (`TestActualConcurrentCreationTypeReplacementAndMovedApex` once in
`review-06`, equally likely on the base source per `fix-21`).
`internal/primaryruntime/network`
`TestActualNotifyAncestorExpiryInvalidatesBoundChildWithoutNewGeneration`
also fails 1 of 3 under race on the base source (`review-09`); it predates
this package. Deleting a *parent* still retires and re-incarnates its
surviving child (existing behavior,
`TestActualNewLateParentAndParentRemovalSurvivingChild`), so the child is
likely unavailable during that replacement; that was not measured, and the
mounted test does not delete the parent. The new mounted test asks a setup
POST again after 503 `current writable serving generation unavailable`, a
command-path refusal another lane (CMD-RECAPTURE, D399) owns.
