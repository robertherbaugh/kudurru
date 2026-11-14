### F28-HA-1/F28-HA-5 Managed root anchors on fenced and conflicted members, and member clock skew — November 10, 2026 (D369)

The map predicted from code reading that a node with `anchorMode: managed` stops
answering all DNS, and cannot restart, once it is fenced or conflicted. The
prediction reproduced on unmodified `33ee04bc`. `race01` and `race02` run the new
managed-mode copy of `TestRootAnchorPreparationPublishesFencedDNSOnly` beside the
static original: the static test passes, and the managed test fails after 16.5 s
with the publisher never reaching `dns_only` and both UDP and TCP answering
`SERVFAIL` for the node's own SOA. Root admission accepted only the writable and
follower roles; managed preparation, every output capture and startup admission all
used that one rule. The exposure is wider than an old primary: a follower observes
a successor's term (and becomes fenced) before it installs that term's image, so
every managed follower went through the same outage at each promotion.

Root admission for managed anchors is now split (D369). Serving admission accepts
every role that can hold this member's accepted owner: writable, follower, fenced
and conflict. A fenced or conflicted cut is prepared, checked and published only
with its token's own DNS-only disposition (D360), through the unchanged Store
registration path; `internal/store/sqlite/anchor_publication.go` already accepted
`PublicationOptions{DNSOnly: true}` with an anchor registration and needed no
change. Startup admission accepts the same roles with `managedTopology: direct`,
and with `edges` after the same complete fleet observation under the original
token. Updater admission is unchanged: only the elected writer of the original
term polls as a writer, and an unelected role never receives a writer token.

The decision the map left open is recorded as "yes": every role without an elected
updater keeps the deny-only supervisor (D318). A fenced or conflicted member can
stay in that role for days, and it is still answering with DNSSEC validation, so a
revoked root key must not stay trusted there. The supervisor runs one child per
member, term and role. A role change inside one term (fenced to follower by
install, or to conflict or divergence) joins the old child and admits one new
child; ordinary installs still cannot rearm a terminal result. A signed REVOKE
observed in the fenced or conflicted role commits the same durable local denial as
on a follower and retires the DNS-only output; no observation in these roles
changes accepted history, the canonical image or the authority.

F28-HA-5: the follower poller and the elected updater returned a terminal
`ErrClock` whenever the local wall clock was before the committed checkpoint's
wall time, which the supervisor and the writer adapter then kept for the whole
authority epoch or writer term. Both now use one rule, `anchorCheckpointWait`: a
clock that trails the checkpoint by at most `trustanchors.ClockTolerance` (five
minutes) waits until it reaches the checkpoint, acquires nothing meanwhile and logs
a time-synchronization warning; a larger gap stays a terminal `ErrClock`. The
poller then acquires once and resumes its cooldown; the updater returns to the
accepted `NextRefresh`. The wait creates no sample, receipt, checkpoint or state,
so the pure package's credit rules are untouched: a new writer session still gets
no credit for its first observation, and credit inside one session is still
checkpointed monotonic progress, withheld when wall and monotonic progress differ
by more than the tolerance. The comparison reads the wall clock through a private
field on the poller and updater that only tests set (the mounted poller takes it
from the test-only `servingRuntimeOptions.AnchorWall`); samples, receipts and
stored state always use the real session clock, and no protocol constant changed.

The wait must not create a terminal outcome of its own. An independent review of
the first form (lane head `9204e534`) found one. The poller pins every captured
operation to its installed incarnation, and only a completed normal observation
let a later capture move to a newer one. A child that had only waited so far had
completed nothing, so any ordinary replicated install during the wait, which can
last five minutes, ended it with `ErrFenced`, and the supervisor kept that result
for the whole member, term and role. That hit every freshly started child: a
process start, a term change, or the new fenced-to-follower role change. A poller
that has started no acquisition and whose last operation only deferred for its
clock was neither held nor refused (D318), so its next fresh capture may now adopt
a newer installed incarnation in the same member, term and role, as the
supervisor's readiness wait does. The other rules are unchanged and each has a
test: a refused or failed acquisition cannot move, a clock wait after a failed
acquisition changes nothing, no operation crosses a role change, and a first
capture that never waited must still match the cut its supervisor classified. The
elected updater has no installed incarnation to keep (every step captures a fresh
operation under its original writer term) and needed no change.

The same review found two proof gaps with correct production behavior behind
them, and both now have lane tests: the edges test ran with static anchors, and
no test drove a fenced member back to follower by install under the mounted
supervisor. The first form is commits `c662dfc0` and `9204e534`; the review fixes
are the two commits after them, ending at lane head `f9818a59`.

Evidence under `.context/v020-final-20261110/evidence/F28-FENCED/` (default race
detector, real SQLite, real TCP/UDP sockets, signed in-process root authority; no
forged anchor state or sample). The first block ran on the first form (lane head
`9204e534`); the review-fix block after it ran on the final tree:

- `race03`: `TestRootAnchorPreparationPublishesFencedDNSOnly` (static, 1.05 s) and
  `TestRootAnchorManagedPreparationPublishesFencedDNSOnly` (3.01 s) pass with the
  fix.
- `race08`, every first-round test in its final form plus the affected root selection
  (`Anchor|Doctor|Primary|Cluster|Join|Promot|Member|Enroll|Fenc|Replica|Follower|
  CLIReference|ManagedTopology|ManagedEdges|ServingRuntime` over `./cmd/kudurru`
  and `./internal/config`): exit 0, 360 pass entries (171 top-level tests), zero
  failures, one actual skip (`TestDoctorDatabase`, no PostgreSQL DSN), 405.9 s plus
  2.4 s. It contains:
  - `TestRootAnchorManagedPreparationPublishesFencedDNSOnly` (3.04 s) and
    `TestRootAnchorManagedPreparationPublishesConflictDNSOnly` (2.97 s): an actual
    managed/direct runtime bootstraps through the elected `RunWriter` mount, then
    loses its term. Status `dns_only`, authoritative SOA over UDP and TCP through
    the production output guard, `CaptureHTTP` refused, the published bundle keeps
    the managed root seal, exactly one supervision acquisition (two root reads in
    total, none more inside the cooldown), and the image, accepted owner version
    and authority are unchanged.
  - `TestAnchorDNSOnlySupervisorRevocationRetiresOutput/fenced` (1.80 s) and
    `/conflict` (1.83 s): the supervisor's own held signed REVOKE is released; a
    held output attempt refuses to emit, UDP and TCP answer an empty `SERVFAIL`,
    two root reads, canonical image and authority unchanged.
  - `TestAnchorFollowerSupervisorFencedRoleKeepsDenyOnlyPollingAcrossReopen`
    (1.56 s): a follower fenced by a higher term publishes DNS-only, a new child
    for the fenced role acquires independently, the REVOKE retires output, and a
    physical reopen of the fenced database passes startup admission with the
    denial retained and no acquisition.
  - `TestAnchorEdgeActivationAdmitsDNSOnlyCutOnlyWithCompleteFleet` (4.39 s, eight
    subtests): for a fenced and for a conflicted backend, a complete two-edge
    observation admits the DNS-only cut (protected HTTP stays closed); an
    unavailable edge, a missing backend membership and an authority change during
    the observation each refuse after contacting the whole fleet exactly once.
    This fixture leaves `anchorMode` at its static default with a dormant owner,
    so it proves the edges admission rule only and its `dns_only` publication is
    the static path from D360. The managed owner behind edges is proven by
    `TestAnchorEdgeActivationManagedDNSOnlyRestartNeedsCompleteFleet` below.
  - `TestRunServeDirectAnchorsFencedAndConflictRestart` (17.62 s): one actual
    `runServe` database is bootstrapped as writer, then fenced, then conflicted,
    with a physical close, reopen and restart for each role. Each start reaches
    operational listeners with serving not ready and auth status 503, answers the
    last-good record on UDP, TCP, DoT, DoH, DoQ and DoH3, makes exactly one root
    acquisition (three in total with the bootstrap) and leaves the authority, the
    image and the accepted owner version unchanged.
  - `TestAnchorCheckpointWaitBounds` (seven cases, including exactly five minutes
    and one nanosecond more), `TestAnchorFollowerPollerWaitsForCheckpointWithin
    ClockTolerance` (3.03 s: a checkpoint two seconds ahead gives two non-attempts
    with one root read, then exactly one poll, then the cooldown),
    `TestAnchorFollowerPollerCheckpointBeyondClockToleranceIsTerminal` (0.96 s) and
    `TestAnchorUpdaterCheckpointSkewWaitsWithinToleranceAndRefusesBeyond` (2.82 s:
    a successor writer session neither stops nor acquires, keeps the accepted
    owner version, and returns to the exact `NextRefresh`; five minutes plus one
    second stays terminal and is not recaptured).
  - Every pre-existing anchor, follower, supervisor, edge, cluster, join and
    promotion test in that selection, including `TestAnchorFollowerPollerRetains
    EpochDuringCooldown`, `TestAnchorFollowerSupervisorTerminalSurvivesGeneration
    Install`, `TestAnchorEdgeActivationRefusesChangedOriginal` and
    `TestCLIReferenceDrift`.
- `race09`: `TestRunServeDirectAnchorsFencedAndConflictRestart` (17.45 s) again and
  `TestRunServeDirectAnchorsFencedRecursiveValidation` (8.16 s): a fenced managed
  node restarted against a private signed root on a non-loopback interface returns
  AD-validated native recursion on all six transports from its last accepted
  owner, with exactly one supervision TCP acquisition and unchanged history.
- `pure01` (light run): `TestClockJumpCannotCreditCandidateAndBackwardTimeRefuses`,
  `TestRestartPreservesOnlyDurablyCheckpointedSessionProgress` and
  `TestFiveSeedAnchorsAndIndependentClockSessions` pass; the pure package is
  untouched.
- `lint01`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`: zero issues.
  `gofmt -l` is empty for the touched files and `go vet ./cmd/kudurru` is clean.
- `drift01` (light run on the lane head): `TestOpenAPIDrift`, `TestReferenceDrift`
  and `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in `race08`.
  The package changes no API route, DTO, setting, CRD, permission or CLI text, so
  there is no generated commit.

Review fixes, on lane head `f9818a59` with a clean worktree (`fix-lint02`,
`fix-vet03`, `fix-race07`; every `fix-*` log records the head, the count of
uncommitted paths and a digest of the `cmd/` diff against `9204e534`, which is
`b01da997d44800ee` for the final tree):

- `fix-race07`, the same affected root selection over `./cmd/kudurru`
  (`./internal/config` is untouched by the review fixes and was not rerun): exit 0,
  327 pass entries (169 top-level tests), zero failures, the same one skip
  (`TestDoctorDatabase`), 427.6 s. Against `race08` its test set drops only
  the nine `./internal/config` tests and adds the six tests below and
  `TestRunServeDirectAnchorsFencedRecursiveValidation` (`race09`). It contains:
  - `TestAnchorFollowerPollerClockWaitAdoptsOrdinaryInstall` (1.46 s): a poller
    built as `runAnchorFollowerEpoch` builds it (supervisor epoch preset) defers
    with its wall clock held two seconds before the checkpoint. Two ordinary
    primary writes are installed during the wait (same member, term and role,
    newer incarnation, unchanged checkpoint). Each following step still waits,
    acquires nothing and holds the newly installed cut; once the clock reaches the
    checkpoint there is exactly one poll (two root reads in total), then the
    cooldown, with image and authority unchanged.
  - `TestAnchorFollowerSupervisorClockWaitSurvivesOrdinaryInstall` (6.61 s): the
    same through the mounted supervisor, its child and the child's wake loop. The
    follower keeps complete publication while its child waits, an ordinary install
    lands, two further clock checks prove a capture after the install, and after
    the clock is released there is exactly one supervision poll and none more
    inside the cooldown.
  - `TestAnchorFollowerPollerClockWaitKeepsRefusalRules` (three subtests): after a
    wait, a role change is still a terminal `ErrFenced` and is not recaptured; a
    poller whose acquisition failed, then waited for a clock set back, still cannot
    move to a newer install; and a first capture that never waited still refuses a
    cut newer than the one its supervisor classified.
  - `TestAnchorUpdaterCheckpointWaitSurvivesOrdinaryWrites` (0.93 s): two ordinary
    writes during a newly elected writer's wait neither stop it nor make it due;
    it returns to the exact accepted `NextRefresh` with the owner version unchanged.
  - `TestAnchorEdgeActivationManagedDNSOnlyRestartNeedsCompleteFleet` (13.29 s,
    eight subtests): for a fenced and for a conflicted backend, an actual managed
    backend bootstraps through its elected writer behind a complete two-edge fleet
    (one root read, two fleet calls), loses its term durably, and its database is
    physically closed and reopened. With a complete observation the restart is
    admitted (two more fleet calls), publishes `dns_only` with the managed root
    seal and no static seal, emits through the production output guard, refuses
    `CaptureHTTP`, makes exactly one supervision acquisition (two root reads, none
    more inside the cooldown) and leaves the accepted owner version and the
    authority unchanged. An unavailable edge, a missing backend membership and an
    authority change during the observation each refuse startup after contacting
    the whole fleet exactly once, with no root acquisition.
  - `TestAnchorFollowerSupervisorPromotionFencedThenInstallResumes` (3.45 s): a
    second member database is promoted for real. The supervised follower observes
    that term (fenced under it: `dns_only`, protected HTTP closed, one new
    supervision poll, managed seal emitted), then installs the successor's image,
    which is a role change inside that one term: complete publication resumes
    with one new supervision poll (four root reads in total, none more inside the
    cooldown), and the image, accepted owner version and authority are unchanged.
    The accepted owner is the same on both sides of this promotion; a divergent
    owner is F28-HA-2.
- `fix-lint02`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`: zero issues.
  `fix-vet03`: `go vet ./cmd/kudurru` is clean; `gofmt -l` is empty for the touched
  files. `fix-vet02` vets the first review-fix commit alone through an overlay.
- `fix-drift01` (light run): `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in `fix-race07`.
  The review fixes change no API route, DTO, setting, CRD, permission or CLI text,
  so there is still no generated commit.

Retained failed runs and what they showed:

- `race01`, `race02` (unmodified production code): the predicted defect, as above.
- `race05` (mutation A: serving admission put back to writable-or-follower; 14
  failing entries): all six HA-1 tests fail. `runServe` never brings listeners up
  on the fenced database, edges topology refuses with "root anchor policy
  unavailable or changed" before contacting any edge (0 calls), fenced and
  conflicted nodes answer `SERVFAIL` on UDP and TCP with `prepare_failed`, and no
  supervision acquisition happens.
- `race06` (mutation B: D369 serving admission kept, supervision put back to the
  follower role only; 5 failing tests): DNS-only serving and the edges admission
  test pass, and every supervision assertion fails (root reads stay at one, the
  fenced follower never acquires, the revocation is never seen).
- `race07` (mutation C: any checkpoint ahead of the local clock is terminal again):
  the two within-tolerance tests and three bound cases fail with `ErrClock`; the
  beyond-tolerance test passes either way, as it should.
- `race10` (mutation A again, on the two tests added or extended after `race05`):
  `TestAnchorDNSOnlySupervisorRevocationRetiresOutput/fenced` and `/conflict` never
  acquire, and `TestRunServeDirectAnchorsFencedRecursiveValidation` never brings
  listeners up.
- `review-probe01`, `review-probe02` (the reviewer's probes on `9204e534`, source
  in `review-probe-source.log`): `TestReviewProbeClockWaitThenOrdinaryInstall`
  fails with a terminal `replication: writer fenced` after one ordinary install
  during the wait (the hole above), and
  `TestReviewProbeLaneEdgesDNSOnlyFixtureAnchorMode` shows the edges fixture with
  `anchorMode="static"`, a dormant owner and a static seal (the proof gap). The
  managed edges restart and promotion probes pass. `review-probe01` is the earlier
  run in which one probe failed on the reviewer's own helper.
- `fix-race01` (the new tests before the fix: the poller file overlaid with the
  `9204e534` rule, test seam kept; 24 pass entries, 2 failing tests): the step
  test fails with `an ordinary install during the checkpoint wait ended this poller
  for its epoch: err=replication: writer fenced reads=1`. The mounted test logs
  `root anchor follower stopped for this authority epoch err="replication: writer
  fenced"` and fails with no clock check after the install and one root read. The
  refusal-rule, updater, managed edges and promotion tests pass, as they should.
- `fix-race03` (mutation A again, on the edges and promotion tests; 19 failing
  entries, none passing): the managed and the static edges tests refuse with "root
  anchor policy unavailable or changed" before contacting any edge, and the
  promotion test never reaches `dns_only` in the fenced role.
- `fix-race04` (mutation B again; 15 pass entries, 4 failing): the static edges test
  passes in full, which is exactly why it could not stand for managed anchors. The
  two managed `complete` cases publish and emit, then fail on the supervision read
  that never comes; the promotion test fails in the fenced role.
- `fix-race05` (mutation R: the supervisor keys its child by member and term only,
  as before D369): only the new promotion test fails. The member returns to
  complete publication, its fenced-role child stops with `replication: writer
  fenced` at the role change, no new child is admitted and root reads stay at
  three, so it would have no revocation polling for the rest of the term.
  `TestAnchorFollowerSupervisorFencedRoleKeepsDenyOnlyPollingAcrossReopen` and
  `TestAnchorFollowerSupervisorTerminalSurvivesGenerationInstall` pass under it.
- `fix-race08` (mutation R over the whole anchor selection,
  `Anchor|Fenc|Follower|ManagedTopology|ManagedEdges|ServingRuntime|Promot|Replica`,
  on lane head `f9818a59`; 194 pass entries, 100 passing top-level tests,
  332.0 s): exactly one test fails, the new promotion test. No other test in that
  selection notices a supervisor that ignores the role, which is the gap the
  review named.
- `fix-lint01` (first lint of the fix): one staticcheck QF1001 finding (De Morgan)
  on the new condition. It was rewritten to the equivalent form with no other
  change. `fix-race02` (the new tests: 26 pass entries, 34.2 s) and `fix-race06`
  (the affected selection: 327 pass entries, 169 top-level tests, one skip,
  430.6 s) passed before that rewrite, on digest `38537d583d13d8ee`; `fix-race07`
  is the rerun after it. `fix-race01` and `fix-race03` to `fix-race05` also ran
  on that earlier digest.

`race04` is the first passing run of the new tests (26 pass entries, 39.6 s),
before the revocation test was extended to the conflict role and renamed and before
the recursion test existed. The first-round mutations were temporary edits of the
working tree, restored byte-for-byte before the commits. The review-round variants
(`fix-race01`, `fix-race03` to `fix-race05`, `fix-race08`) were compiled through
`go test -overlay` and never touched the worktree (`fix-batch01.sh`,
`fix-batch02.sh`; `fix-overlay-source.log` shows each replaced file as a diff);
lane head `f9818a59` contains no mutation.

What remains open. This package does not change how a fenced or conflicted member
gets back to follower: F28-HA-2 (package F28-HISTORY) still owns the strict
`anchorSuccessor` install rule, which refuses an authoritative branch whose owner
generation is not greater than the local one and also refuses an owner whose
`UpdatedAt` is earlier than the local one (a second place where member clock skew
matters). Until then such a member now serves DNS-only and supervises instead of
answering `SERVFAIL`, but it can stay fenced until the primary's generation passes
its own. Fencing and conflict in these tests come from `Store.ObserveAuthority` on
real databases, not from scheduled member-TLS exchanges between separate processes
(the promotion test promotes a second real member database, but still delivers its
term and its image by direct Store calls); the separate-process managed acceptance
is F28-HA-3/F28-HA-4. The edges tests use in-process authenticated edge peers; the
opt-in pinned dnsdist test was not rerun.
The clock wait is proven at the poller and updater step, at the pure rule's bounds
and, for the poller, under the mounted supervisor, always with a test wall-clock
reading (trailing or held), not with a skewed operating-system clock and not
between separate processes; the updater's wait has no mounted test. A gap beyond
five minutes is still visible only as a log line (`invalid clock continuity`); an
operator-visible status is F28-GATE-4. Two refusals that predate this package are
unchanged and can still end supervision for a member, term and role without any
clock skew: an install that lands inside a capture, between its image read and the
Store barrier check, refuses that capture (D318), and a child whose very first
capture already sees a newer cut than its supervisor classified is refused. Each
window lasts only as long as one capture. The first is read from the code and has
no dedicated test; the second is pinned by
`TestAnchorFollowerPollerClockWaitKeepsRefusalRules/unwaited-first-capture`. D369's
history clause (F28-HA-2) is decided in the row but not implemented by these
commits.
Each promotion now costs a follower up to two root DNSKEY TCP queries (one in the
fenced role, one after install) instead of one. The enrolled process scenarios in
`test/e2e` were not rerun: they use static anchors with topology disabled, where no
changed function is mounted.
