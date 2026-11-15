### HA-OPEN6 The follower lane's held fixes on the pull request head, and what their verification left open — November 13, 2026 (D381)

**Held commits.** The lane was reset to the integrated head `223269c8`. Of the
commits held on `wip/v020/ha-held`:

- CI-RED-2 `4b78eec5` (SERVFAIL instead of REFUSED when an UPDATE's keyring
  stops being current): kept, cherry-picked cleanly (`43dcd029`). Its new test
  is renamed and rewritten by HA-OPEN5's `ad85c999`.
- CI-RED-2 `1621b669` (anchor updater captures again after a lost capture):
  changed. The F28 lane landed the same rule at head (`anchorUpdater.step`
  marks a capture refused with `replication.ErrCAS` as raced and schedules the
  next step a second later), so the product hunk is dropped and one mechanism
  remains. Head had no test reaching that capture branch; its test and the
  test-only hook `servingRuntime.anchorCaptureRead` are kept, adapted to
  head's `raced` field (`5d28289e`).
- CI-RED-2 `5edb45b2` (log in before the zone exists): dropped. It worked
  around the sign-in 503 across a status write, which D399 removes, and it
  conflicts with the current `serve_anchor_activation_test.go`.
- CI-RED-2 `93a9a09b` (pin the login refusal on a seeded store): dropped. It
  pins the 503 that D399 removes; the inverted assertion is already covered by
  `TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates` (`c19f1610`),
  and its seeded-start part flaked in verification on `generation_pending`
  inside the startup readiness gap, which is PUBLISH-STARVATION's (D405).
- HA-OPEN5, all ten: kept (lane SHAs `c0e00366`, `978c38f1`, `359e4f26`,
  `f6b471b6`, `fbee6fc6`, `caf2dba7`, `649d6f25`, `1724fa5c`, `e3f5b683`,
  `cb9ae60f`, in the held order). `ad85c999`, `98439589`, `9d2c42f4`, `472a9f79`,
  `56f06b45`, `84f54bf9`, `c8c9e3b1`, `7ac4f5a2` applied cleanly; `29c32ca3`
  conflicted in `docs/deploy/sqlite-cluster.md` (head had added the
  `primary_required` row; kept head's three rows with HA-OPEN5's wording of the
  `completion_unknown` row); `4a98737f` conflicted in
  `cmd/kudurru/primary_runtime_test.go` (head had removed
  `expectWriterLoss`; kept head's fields plus HA-OPEN5's `timeout`).

**A signed UPDATE through a follower (D381).** A burst through a follower
could still get an immediate SERVFAIL that a second identical send would not
get. The new mounted test `TestRootMemberFollowerUpdateBurstWithinTheProductionBound`
(six UPDATEs through the follower's real DNS listener under the production
four-second `memberForwardUpdateTimeout`, while the follower pulls) failed 7
of 7 race runs (`diag02`, `diag03`) with SERVFAIL after 90 to 290 ms on the
third or fourth update. Temporary diagnostics showed the cause: the forward
capture only retains the follower's token, `WithMemberForward` checks it again
when it acquires the store for the exchange, and a pull that installed the
primary's next commit between the two made `Forward` fail with "replication:
authority changed" before anything was sent; `forwardUpdate` answered that at
once. `EnrolledForwarder.Forward` now joins `clusterreplica.ErrNotSent` to any
error returned before the member client's exchange begins, and
`forwardUpdate` treats such a source change like one found at capture: it
waits for its current publication, selects the zone and admits the key again
there, and sends the same command ID once. A refusal after the exchange may
have begun is never followed by a second send. The CMD-RECAPTURE decline in
`docs/architecture/access.md` is kept for a native UPDATE (a second capture
would admit the same wire message on a second keyring) and the page now says
why a forwarded one differs: the follower re-admits only the same key
identity, and the primary decides everything again. The process tests' follower
UPDATEs (inserts over UDP and TCP, the failed prerequisite, the ungranted type,
the marker) are now sent once through the first follower and must get their
answer at once; through the rejoined member a SERVFAIL at the four-second
bound is accepted and the update sent again (review fixes below); the native
UPDATE to the newly promoted primary keeps its retry, because a native UPDATE
still meets SERVFAIL right after a commit (every run logs it for
`promoted.dyn.test.`). What is left through a follower is the documented
SERVFAIL at the four-second bound itself, outcome unknown: pulling every 20 ms
in the fixture (`diag04`) kept the follower's publisher from publishing for
the whole bound and kept a committed update on the primary waiting for its
publication, the publication-starvation limit that PUBLISH-STARVATION (D405)
works on; the test now pulls once a second and accepts at most one SERVFAIL,
only at the bound (review fixes below).

**Unauthorized pull after a commit (verify note).** The member pull refused as
"replica: unauthorized peer" right after an ordinary commit was investigated
with temporary logging (`diag01`, `diag02`): `MemberServer.image` read the
image under one authority, the zone status write that follows a create
replaced it before the emission, and `MemberRequest.WithOutput` refused with
"replication: authority changed". The refusal is written without
authenticated metadata, so the client reported an unauthenticated response as
"unauthorized peer" and the coordinator counted a rejected pull until the next
interval. An image is a read and nothing is written at that point, so the
member server now captures the member request again and reads the current
image, at most three times within the request's deadline, each attempt
admitted and checked in full. `memberUpdatePull` no longer retries that
refusal, and `TestRootMemberPullRightAfterACommitIsServed` makes five
single-attempt pulls right after commits (it hit the refusal on 3 of 4 runs of
the old fixture). Diagnostic logging was reverted before every commit.

**The other verify notes.**

- `TestWriteWhoseDialRunsIntoTheDeadlineStaysUnknown` (hosted CI: "dial tcp
  ...: i/o timeout" reported as not sent): the member client's dialer timeout
  and HTTP client timeout are the same exchange timeout, and whichever fired
  first decided the answer. `notDispatched` now excludes a dial whose error is
  a timeout, so both give the documented unknown outcome;
  `TestDialTimeoutIsNotANotSentDial` pins the classification without
  depending on timer order, and the mounted test also ends the dial through a
  shorter caller deadline.
- "root anchor updater stopped for this writer term: sql: transaction has
  already been committed or rolled back": both occurrences were at the
  teardown of a failed test. `database/sql` reports `sql.ErrTxDone` for a
  transaction its context rollback ended; `anchorWriterCanceled` treats that
  as the cancellation once the writer context has ended, and
  `TestAnchorWriterCanceledOnlyAfterTheContextEnded` pins every case.
- The retiring branch of `captureUpdate`: `TestRootMemberUpdateForRetiringZoneIsAnsweredAtOnce`
  deletes the zone on the mounted primary, waits for its publication to show
  the zone retiring or retired, and requires a forwarded UPDATE with a
  20-second bound to answer SERVFAIL or NOTAUTH well inside it (52 to 137 ms).
- `completion_unknown` after the primary answered:
  `TestRootMemberForwardAnswerNotRelayedIsUnknown` holds a forwarded client
  group write on the real follower, after the primary committed and answered,
  through the test-only hook `servingRuntime.memberRelay`, until the
  request's 30-second lifetime ends; the answer is 503 `completion_unknown`,
  relayed once, and the object is on the primary. This reaches
  `objectForwardError`'s default (deadline) branch only; the new "source
  changed after the primary answered" branch is pinned by the unit test
  `TestObjectForwardErrorAfterTheAnswerIsUnknown` alone (review fixes below). A version that installed a
  newer commit inside the hook deadlocked (`race02`, retained): the follower
  holds its Store lock from the forward's acquisition through the relay, so
  an install cannot land there.
- The production four-second bound under race: the burst test above runs
  under it.
- `TestRootMemberFollowerUpdateAcrossInstallAnswersServfail` (now
  `...WaitsForItsPublication`) and `TestRootMemberUpdateKeyIdentityAndRetryActualHTTP`:
  HA-OPEN5's `4a98737f` and `472a9f79`, carried here, removed their timing and
  `CommitSequence` dependence; the stale-test fixture's last retry is gone.

**Strict refused-write check.** HA-OPEN5's strict `enrolledAbsent` (every
query answered over UDP and TCP) now runs on a head that contains D378/D395
(`10d3ab5b` and its follow-ups): it passed in every process run below.

**Large apply through a follower (D375).** Measured on this head over the
six process runs (18 measurements): a file of 40 documents through a follower
was applied completely 5 times and was cut after 23 to 37 documents
otherwise; each of the 5 files of 160 documents (sent only after a complete
40) was cut after 9 to 33. So not every document can be applied unless a
foreign commit intervenes: the cut is the follower's own pull. D378 chains a request's items on the
node that owns the commits and D399 recaptures only before a request's first
dispatch; neither prevents the follower's own pull of the request's earlier
items from changing its source. D375 stands; `docs/deploy/sqlite-join.md`
says so with the numbers, and the cut item's refusal already names the primary
and says to send the rest there (`29c32ca3`).

Evidence under `.context/v020-final-20261110/evidence/HA-OPEN6/`:

- `e2e01.log` to `e2e06.log` (`.commit` files beside them): `KUDURRU_E2E=1 go
  test -v -count=1 -run 'TestEnrolledClusterProcesses|TestEnrolledManagedAnchorsPromoteAndRejoin'
  ./test/e2e`, through the heavy wrapper. Every run passed all three tests, 0
  skipped, 18 of 18 in total: `e2e01` at `cb9ae60f` (held commits only, the
  follower UPDATEs still retried; none retried), 717 s; `e2e02` to `e2e04` at
  `fb4138e4` (all source fixes, follower UPDATEs sent once), 634 s, 609 s,
  646 s; `e2e05`, `e2e06` at `3e2974b0` (differs from the final head only in a
  cmd/kudurru test file, a gofmt-only change and docs), 666 s, 631 s.
  `TestEnrolledManagedAnchorsPromoteAndRejoin` 238 to 288 s,
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` 145 to 168 s,
  `TestEnrolledClusterProcessesPromoteAndRejoin` 198 to 223 s. Every burst
  through a follower was 8 of 8 NOERROR (1.1 s to 15.6 s); no forwarded
  UPDATE answered SERVFAIL or REFUSED in any run. The native UPDATE to the
  newly promoted primary was retried after 2 to 7 SERVFAILs in every run
  (unchanged, see below). The strict `enrolledAbsent` checks passed in all
  six PromoteAndRejoin runs.
- `diag01`, `diag02` (unauthorized pull; temporary logging), `diag03`,
  `diag04` (burst SERVFAIL; temporary logging), with `diag-patch.diff`:
  retained diagnostic runs. `diag02` and `diag03` hold the 7 of 7 failing
  burst runs before the fix; `diag04` the bound expiries with 20 ms pulls.
- `race01-anchor.log`: `TestAnchorUpdater` under `-race -count=3`: passed
  (100.9 s).
- `race02.log`: stopped by hand, the deadlocking first relay test (retained).
- `race03.log`: the new and affected cmd/kudurru tests under `-race
  -count=3` after the fix: the burst test failed 3 of 3 at the four-second
  bound (20 ms pulls, load average 14 to 21), all others passed.
- `race04-follower-x3.log`, `race05-root-selection.log`: the follower tests
  `-count=3` (8 passed, 1 failed: the anchor-unavailable pull after a
  canceled pull) and the root selection `TestRootMember|TestMemberForward|
  TestMemberLogout|TestAnchorUpdater|TestAnchorWriter|TestPrimaryRuntime|
  TestPrimaryUpdateIngress|TestRootUpdateOutput|TestServingItems|
  TestObjectForwardError|TestMemberItemRefusal|
  TestRunServeSQLiteStoredTSIGNativeUpdateTransferNotify` (79 passed, 1
  failed for the same cause, 488 s).
- `race06-follower-x5.log`: after `3e2974b0`, `TestRootMemberFollowerUpdate|
  TestRootMemberPullRightAfterACommitIsServed` under `-race -count=5`: 15 of
  15 passed, 0 skipped, 230 s; every burst 6 of 6 NOERROR; no pull read
  again.
- `light01`, `light02`, `light04`: `internal/cluster/replica` under `-race`
  (dial tests `-count=3`, full package twice): passed. `light03` also ran
  `internal/store/sqlite`, which hit the default 10-minute test timeout at
  `-p 2` on the loaded machine (untouched package; not rerun).
- `lint02.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./internal/cluster/replica/...`, `./test/e2e/...`: 0 issues (`lint01`
  found the gofmt alignment fixed in `372409b9`). `go vet` clean,
  `pyguards.log` 14 of 14.

Open. The native signed UPDATE to a primary still answers SERVFAIL right
after a commit (every process run logs it on the new primary); it is not
recaptured, by the decision CMD-RECAPTURE recorded in access.md, which this
package keeps. A forwarded UPDATE still answers the documented SERVFAIL at
the four-second bound when a publication takes longer than that, which the
fixture produced by pulling every 20 ms; that is the publication-starvation
limit PUBLISH-STARVATION (D405) is changing, and Publisher.Run was not
touched here. The follower pull refused with "store/sqlite: anchor
lifecycle unavailable" after a canceled pull is found and fixed (review
fixes below). The large
apply cut through a follower (D375) is measured and documented, not removed.
FWD-E2E's wider acceptance (one `primary_required` representative per
primary-only group, node-local routes, `/cluster` role on the follower in the
process test, the static twin and conflict re-runs, Semgrep, the full
`internal/api` and `internal/auth` default-race runs, frozen-source hashes)
was not done in this package. The full `./cmd/kudurru` package was not run.

### HA-OPEN6 review fixes — November 13, 2026 (D381)

The review (verdict accept-with-fixes, lane head `fefcf07e`) found a single
signed UPDATE through the rejoined member answered SERVFAIL in one of three
runs of `TestEnrolledManagedAnchorsPromoteAndRejoin`, with no elapsed time
recorded. Seven commits on top (`fb9037ac`..`4a77f00a`) address it and the
other findings.

**Through the rejoined member.** A run with temporary logging in a
licensed binary built outside the worktree through `go test -overlay`
(`fix-diag01`, 6 runs, worktree untouched) reproduced it in the rejoined
member's burst: the newly promoted primary took 1.1 to 1.4 s to answer each
forwarded commit, against 0.3 to 0.7 s on the first primary; the first
UPDATE after the forwarded settings patch waited 3.1 s on the follower for a
current keyring; and one update lost a compare-and-swap round against the
primary's own status write, committed on the second round after 3.47 s,
and its publication wait ran into the command's deadline. The primary
answered SERVFAIL, the follower's exchange ended "write outcome unknown" at
4.34 s. That is the documented SERVFAIL at the four-second bound for a
commit whose publication did not complete, the publication latency
PUBLISH-STARVATION (D405) works on. The rejoined member's updates now accept
a SERVFAIL only at the bound (3.9 s or later, logged) and send the
idempotent update again, at most four times; a SERVFAIL before the bound
still fails at once. The first follower's updates stay single-send, and
`enrolledExpectUpdate` reports when an UPDATE was sent and how long its
answer took. The reviewer's own failure has no elapsed time, so whether it
was this bound case cannot be shown.

**A repeated command ID (new defect).** `TestRootMemberUpdateKeyIdentityAndRetryActualHTTP`
answered SERVFAIL instead of the retained NOERROR for the same command ID
sent again in 1 of 3 race runs (`fix-race02`) and 2 of 15 diagnostic runs
(`fix-diag04`): Apply's receipt replay committed nothing again, but the
publication of the retained outcome met "serving: requested term
superseded", and Apply reports a replay as committed, so it was final.
`runMemberUpdate` now waits for the next publication and runs the command
again when Apply deduplicated it and only that publication failed (`9397a519`);
15 of 15 race runs passed (`fix-race05`). `docs/architecture/zones.md` says so.

**A pull after a canceled pull (finding 2, new defect).**
`TestRootMemberPullAfterACanceledPullIsServed` commits on the primary,
cancels a follower pull after 0 to 1.8 s and requires the next single pull
to succeed, in 16 rounds. It failed 5 of 5 race runs (`fix-race02`,
`fix-diag02`) with "store/sqlite: anchor lifecycle unavailable", and twenty
more pulls 100 ms apart were refused too: not transient. The refusal came
from `BindAnchorLifecycle`'s `anchorBlocked` check. database/sql rolls a
transaction back when its context ends, so the install's final Rollback
reported `sql.ErrTxDone`, and `anchorSQL.finish` classified every rollback
error as an unknown completion, which closes anchor admission for the Store
until the process restarts. A production pull timeout during an install
therefore stopped a follower's replication. `anchorSQL` now carries the
transaction's context; `ErrTxDone` from the final Rollback is a confirmed
rollback only when that context has ended and no commit was attempted
(`88a7bc05`). A rollback error under a live context, a commit error and any
commit attempt stay unknown, as the existing `TestAnchorSQLActualTermination`
cases require; its new `context-ended` case fails without the change
(`fix-light02`). After it the mounted test passed 3 of 3 (`fix-race03`), and
`memberUpdatePull` is one attempt again. This changes the anchor lane's
stated rule ("Neither an error nor ErrTxDone is evidence of rollback") in
one case; the integrator should have the anchor lane confirm it.
`docs/architecture/upstreams-and-cache.md` says it.

**The other findings.**

- Finding 3: `TestRootMemberForwardAnswerNotRelayedIsUnknown` reaches the
  default (deadline) `completion_unknown` branch only. With the Store lock
  held through the relay, no cause for the new "source changed after the
  primary answered" branch exists in that fixture; the test now says so and
  asserts the default message's own "outcome is unknown" text (`8dc8503c`).
  The new branch is unit-tested only.
- Finding 4 (a forwarded UPDATE may hold the follower's Store lock for up to
  the four-second bound): not changed. The follower already holds that lock
  for up to the same bound for any forward to a slow or unreachable primary;
  only a valid TSIG key holder can cause it; a cap on the number of
  publications would not bound the time; and answering zone-state causes at
  once brings back the burst SERVFAILs HA-OPEN5 removed. No mounted test of a
  zone that never becomes active was added.
- Finding 5: `docs/deploy/dynamic-updates.md` and the changelog list the
  remaining SERVFAIL cases and say a client's retry of an idempotent update is
  safe (`d5872a2c`).
- Finding 6: the burst test accepts at most one SERVFAIL, only at the bound,
  and requires at least five of six commits (`3c268d8f`); `fix-race02` had
  two runs with exactly one.
- Finding 7: `TestHTTPForwardingAmbiguousTimeoutAndExplicitSameIDRetry`'s
  retry now uses its own 10 s client. That exposed a second load race (1 of
  5, "lost-response retry executed twice"): the client's 100 ms timeout could
  cancel the dropped exchange before it committed. The dropped exchange now
  runs to its end and the test waits for it (`fb9037ac`); 20 of 20 race runs
  passed (`fix-replica03`), the unchanged test 10 of 10 (`fix-replica02-orig`).

Weakened guards the review listed, kept: the `CommitSequence` comparisons
replaced by zone data and serial (a NOTIFY rearm also advances the head's
`CommitSequence`, so `before+1` was not an exactly-once proof), and
`ErrUnavailable` not required with a timed-out dial (which timer fires first
decides the error; `ErrOutcomeUnknown` is still required).

Evidence under `.context/v020-final-20261110/evidence/HA-OPEN6/` (all `fix-*`):

- `fix-e2e01.log` (at `bde0791c`, with the cmd/kudurru test and sqlite
  edits uncommitted; the node binary was built from committed product code):
  the three enrolled process tests `-count=2`: 6 of 6 passed, 0 skipped,
  1352 s. Through the rejoined member one burst met the bound five times and
  completed after resends (36.2 s for 8).
- `fix-e2e02.log` (at the final head `4a77f00a`): `-count=2`: 5 of 6 passed.
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` failed once (29 s):
  burst UPDATE 2 of 8 through the first follower answered SERVFAIL after
  330 ms, before the bound. The node logs are at level error and hold
  nothing; the cause is not identified.
- `fix-diag05.log` (final head, diagnostic binary): `-count=3`: 9 of 9
  passed, so that SERVFAIL did not recur with logging.
- `fix-diag01` to `fix-diag04`: the diagnostic runs above (failed runs
  retained).
- `fix-race02.log`: the affected cmd/kudurru tests `-race -count=3` before
  the two fixes (the canceled-pull and dedup failures above);
  `fix-race03-canceled-pull.log` 3 of 3; `fix-race05-dedup.log` 15 of 15;
  `fix-race06-root-selection.log`: the reviewer's root selection
  (`TestRootMember|TestMemberForward|TestMemberLogout|TestAnchorUpdater|TestAnchorWriter|TestPrimaryRuntime|TestPrimaryUpdateIngress|TestRootUpdateOutput|TestServingItems|TestObjectForwardError|TestMemberItemRefusal|TestRunServeSQLiteStoredTSIGNativeUpdateTransferNotify`)
  under `-race` at the final head: ok, 500 s.
- `fix-race04-sqlite.log`: `internal/store/sqlite` under `-race`: ok, 900 s.
  `fix-race07-replica.log`: `internal/cluster/replica` under `-race`: ok.
  `fix-light01-anchorsql.log`: `TestAnchorSQLActualTermination -count=5`: ok.
- `fix-lint02.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./internal/cluster/replica/...`, `./internal/store/sqlite/...`,
  `./test/e2e/...`: 0 issues (`fix-lint01-parallel.log` refused to start
  beside another lint). `fix-pyguards.log` 14 of 14.

Open. A signed UPDATE through the first follower was answered SERVFAIL
330 ms after it was sent, before the bound, in 1 of 15 test runs at the
final head (`fix-e2e02`, `fix-diag05`); the cause is not identified, so "never a
transient SERVFAIL" is not proven. Through the rejoined member the
four-second bound is met often on this loaded machine, because the promoted
primary publishes each forwarded commit three to four times slower than the
first primary; the reason was not found and belongs with PUBLISH-STARVATION
(D405). A native UPDATE to a primary still answers SERVFAIL right after a
commit (CMD-RECAPTURE, unchanged). The `sqlite` classification change needs
the anchor lane's confirmation. FWD-E2E's wider acceptance remains as stated
above.
