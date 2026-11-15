### CI-RED-2 The five hosted failures of 7648c479 — November 12, 2026 (no decision number assigned)

Base-versus-head, in one line each (base control 33ee04bc, head = the lane at
923d7720, which has the same failing tests and code as 7648c479; identical
command: race binary, `taskset -c 0-3`, `GOMAXPROCS=4`, one process per run).
None of the rates below discriminates: every one is 0 failures at both
commits, so each verdict rests on a deterministic reproduction and a code
comparison, not on a measured rate.

1. `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled` (login 503):
   head 0/40, base 0/40. The mechanism predates the pass: the held-login
   reproduction is refused 10 of 10 at both commits. The test and its settle
   helper are byte-identical at base; the login's publication check is
   semantically unchanged (for a single-command login `currentAuthCommandThen`
   still reaches `servinghttp.CheckWriter`; this pass added the D378 item
   chain and `primaryRequired` to that function and changed
   `internal/servinghttp/http.go` and `internal/serving/status.go`, so the
   code is not byte-identical). Whether this pass widened the window was not
   measured. Test ordering, fixed in the test.
2. `TestRunServeDirectAnchorsFencedAndConflictRestart` (login 503): head 0/40,
   base not applicable (the test was added in this pass by ed1c0499, D369, and
   is not in 33ee04bc). Same mechanism as 1, through 1's helper. Fixed in the
   test.
3. `TestRunServeSQLiteReversePTRPolicy` ("expired identity did not
   reacquire"): head 0/40, base 0/40. Predates the pass: the delayed-PTR
   fixture with the old check fails 10 of 10 at both commits. Test ordering,
   fixed in the test.
4. `TestSupervisorLifecycle` (ee/edge/dnsdist): recorded only (owned by
   FLAKE-PRIMARY-UNAVAILABLE).
5. `TestEnrolledClusterProcessesPromoteAndRejoin` (activation 409): head 0/15,
   base 0/5 (40 each were asked for). Introduced by this pass in the test:
   dd2e3b53, which is not in the base, added the second zone before the
   enrollment review without waiting for its status; the reproduction answers
   409 5 of 5. Fixed in the test.

The review of the first fix found three product defects behind or next to
these failures. Two are fixed here, one is measured and left for a decision:

- A signed UPDATE forwarded by an enrolled follower could be answered
  REFUSED during a burst (the 1-of-24 failure of the fixed e2e test). Fixed.
- A fresh node with managed direct anchors parked its anchor updater for the
  whole writer term (on a single node, until restart). Fixed.
- A sign-in is refused 503 whenever another commit publishes while it runs;
  on a node with steady UPDATE commits about one sign-in in eight is refused.
  Measured; not changed (needs a decision).

What happens now in each case:

Login 503 (1 and 2). Both tests wrote their zone into SQLite before the first
`runServe`, so the zone had no status. The node first answers the zone about
2.0 s after start and then publishes the zone's first Ready status write
about 0.4-0.6 s later (readiness drops, in-flight UDP answers are dropped,
then it realigns); a restart of the same store publishes nothing after
readiness. The settle helper logged in right after the first DNS answer, and
a login under race takes about 0.5 s. A login is a command held to the
publication its request began on: D199 keeps in-flight work on its captured
publication and D208 rechecks a mutation (session enrollment is one) in the
current command. `currentAuthCommandThen` -> `servinghttp.CheckWriter` ->
`Publisher.CheckWriterLease` returns `ErrPending` when the current bundle is
not the request's, which the session route answers 503 `unavailable`
"current writable serving generation unavailable" with Retry-After and no
effect. No decision row states this consequence for sign-in (the earlier
ledger and the 5edb45b2 comment cited D313, which covers UPDATE
acknowledgments; both now cite D199 and D208). The fix orders the tests: the
first start of both begins on an empty node, waits for readiness, logs in,
creates the zone and record in one `POST /apply` (D378) and waits for the
zone's status; zone status reads in that wait are observations and are read
again after a 503 (as the e2e `enrolledProcessRead` does). Restarts keep the
login after the first answer. A `controlMountedRequest` failure names the
error key and message of an error body. Because the empty-node change left no
committed test starting a managed anchor node on a seeded store,
`TestRunServeDirectAnchorsSeededFirstStartRefusesStraddlingLogin` now does:
it seeds the zone, starts, settles through DNS only, and requires the anchors
to bootstrap exactly once and managed history to be committed. It also pins
the refusal: the login's headers arrive, a signed UPDATE commits and is
served, then the body arrives, and the login answers 503 `unavailable` with
Retry-After and sets no cookie.

Sign-in under steady commits (measured, unchanged). On one mounted node
(non-race binary, 4 CPUs, a loaded shared machine), 100 single-shot logins
per UPDATE rate, never retried:

| Signed UPDATEs | Logins 200 / 503 | Login p50 / p95 | UPDATE NOERROR / SERVFAIL |
|---|---|---|---|
| none | 100 / 0 | 149 ms / 331 ms | - |
| 1 per second | 89 / 11 | 701 ms / 2.28 s | 51 / 27 |
| 10 per second | 87 / 13 | 2.42 s / 4.71 s | 89 / 92 (6 timeouts) |
| back to back (one sender) | 73 / 27 | 3.72 s / 6.84 s | 64 / 172,229 (26 timeouts) |

So sign-in is not starved (at least 73 of 100 succeed even against a
back-to-back sender), but on a node that takes DHCP updates an operator
meets an explicit, effect-free, retryable 503 on about one sign-in in eight,
and sign-in slows down several-fold. The machine's load average was 9-13
during the run, so the absolute numbers are pessimistic; the UPDATE SERVFAIL
counts of the back-to-back row are mostly fast refusals in the sender's own
loop while logins committed, and are not a rate per second. The refusal is honest (nothing is
committed, Retry-After is set) and the dashboard can retry it, but no row
decides it. Proposed followup for the integrator (no decision number was
assigned to this package): "Sign-in and other single session commands are
refused 503 when any other commit publishes while they run (about 1 in 8 at
1-10 signed UPDATEs per second). Decide whether a command that has not
entered its transaction may capture again before its first effect, or
whether the UI retries on 503 `unavailable` with Retry-After; record the
decision." The same refusal appears for other commands: the aborted e2e
chunk below had one `POST /objects/dnsrecordsets: status 503 ... unavailable`
on the static process.

PTR reacquisition (3). An expired PTR name is never served stale: the first
query after expiry is answered unblocked at once and only queues the
background lookup. The test read its PTR counter at that first unblocked
answer, racing the resolver worker. The fake PTR server answers the negative
reacquisition 250 ms late (inside the 1 s resolver timeout) and now counts a
lookup after it has written its answer; the test waits, bounded, for the
second answered lookup and then requires every query for one second after it
to be unblocked. With the fake server answering the PTR name again on
reacquisition, the new check fails 3 of 3, while the previous check
(faad35d4) passed it.

Activation 409 (5). The enrollment review returns the exact served source
digest, and `POST /cluster/activate` refuses a review that a later
publication made stale (409 `conflict`). Since dd2e3b53 the test also creates
the dyn-test UPDATE zone before the review, but waited only for the e2e-test
zone's startup status write; dyn-test's first Ready status write could land
between review and activation. `enrolledZoneObjectSettled` waits for any zone
object, and the test waits for both zones before the review.

Forwarded UPDATE answered REFUSED (the fixed e2e run 4 failure). Diagnosed:
a signed UPDATE on an enrolled follower captured the follower's publication,
and the follower then installed the primary's next commit (in a burst, often
the previous update's own) before `forwardUpdate` checked its keyring. The
keyring predicate (`RuntimeOwner.NotifyKeys` current, which compares the
captured token's authority, including the installed revision, with the
store's) then reported "not current", and the follower answered REFUSED,
which tells an RFC 2136 client its key is refused. It is a product defect,
not test ordering. A keyring that stopped being current is now SERVFAIL, sent
nothing, never retried (D381), on the follower and in both branches of the
primary's `captureUpdate`; a missing proof or an unknown key still answers
REFUSED. `TestRootMemberFollowerUpdateAcrossInstallAnswersServfail` holds an
UPDATE on a mounted follower right after it captured its publication,
installs the primary's next commit and requires SERVFAIL, no effect, and a
later forwarded UPDATE that commits; before the fix it answered REFUSED. The
burst's accepted rcodes are unchanged. `docs/deploy/dynamic-updates.md` and
`docs/architecture/zones.md` say so. The primary-side change has no test of
its own: on the primary the earlier `CheckWriter` catches almost all of this
window.

Anchor updater parked on a fresh node. Diagnosed with temporary markers: the
updater's bootstrap step commits, and its next step's
`captureAnchorOperation` read its image while first-boot status writes were
committing; `PublishIfCurrent` refused the stale token with `replication:
authority changed` before any acquisition, and the writer recorded that as
terminal for the term ("root anchor updater stopped for this writer term"),
2 of 3 empty-node starts. On a single node that is the process lifetime: the
node would not refresh its anchors until a restart. The capture acquired,
authenticated and staged nothing, so there is nothing to replay; the step now
reports `captureLost` and captures again a second later. Failures after the
capture keep their D297 meaning (terminal). With the fix the same diagnostic
shows the capture lost once and captured again on the next step, 3 of 3, and
no stop. `TestAnchorUpdaterCaptureLostToConcurrentCommitCapturesAgain`
commits an ordinary write inside the capture (a test-only hook,
`servingRuntime.anchorCaptureRead`, after the image read) and requires no
stop, no acquisition and a normal next step; it failed 3 of 3 before.
`docs/architecture/upstreams-and-cache.md` says so. The empty node still
becomes ready only at about 5.7-10 s under race on this machine; that delay
is not caused by the updater stop.

Evidence under `.context/v020-final-20261110/evidence/CI-RED-2/` (scripts,
including the temporary test files and overlays, in
`.context/v020-final-20261110/scripts-CI-RED-2/`; saved output of every
failed rate run under `failures/`). Every rate run is `rate.sh`: one race test
binary, one process per run, `taskset` to 4 CPUs, `GOMAXPROCS=4`, through the
heavy wrapper.

First pass (5edb45b2 and earlier):

- `rate01-head-*.log`, `rate02-base-*.log`: test 1 head 40/40 pass (median
  10.6 s), base 40/40 (12.7 s); test 2 head 40/40 (18.5 s), base: the test does
  not exist at 33ee04bc, so its 40 "passes" of 1.4 s are "no tests to run";
  test 3 head 40/40 (5.5 s), base 40/40 (4.0 s).
- `rate03-head-e2e.log`: test 5 head 15/15 (181 s each), stopped at 15.
  `rate04-base-e2e.log`: base 5/5 (85 s; the base test has no UPDATE zone),
  cut off by the 2-hour background limit.
- `diag01`..`diag05`: the publication timeline of the managed anchor fixture
  (seeded zone: first answer about 2.0 s, status write and readiness gap about
  2.5 s, realigned about 3.0 s; restart: nothing after readiness; empty first
  boot: defaults and their status writes at 1.2-1.6 s, ready at 6-7 s, the
  updater ERROR at about 4 s).
- `repro03-login-*.log`, `repro04-base-*.log`: held login across the seeded
  node's readiness gap refused 10/10 at head and at base; on the empty node
  held 1 s, passes 10/10 at both. `repro04-base` also has the delayed-PTR
  fixture with the old check: 0/10 at base. `repro01-ptr-delay-old-check.log`:
  0/10 at head. `repro02-e2e-review-then-publication.log`: 409 5/5.
- `fixed02-100-*.log`, `fixed04-test1-extra10.log` (5edb45b2): test 2 100/100
  (median 30.0 s), test 3 100/100 (4.2 s), test 1 99/100 then 10/10 (26.4 s);
  its run 1 failed on an ops-port bind collision with another process
  (`offlineFreeAddress`, FLAKE-PORT-RACE).
- `fixed03-e2e-100.log` (5edb45b2): test 5 23 of 24 pass (191 s); activation
  succeeded in all 24. Run 4 failed on the REFUSED burst UPDATE diagnosed
  above.
- `review-01-cmd.log`, `review-02-e2e.log`: the reviewer's runs (3/3 each of
  tests 1-3, 2/2 of test 5).

This pass (4b78eec5 .. 93a9a09b):

- `fix-01`..`fix-03-follower-refused-before.log`: the new follower test
  before the product fix: REFUSED on every run that got past the pull
  (1 of 3, 0 of 5 with a too-strict primary wait, 1 of 5); the other runs
  failed on a transient "unauthorized peer" refusal of the fixture's pull,
  which `fix-04-diag-pull.log` shows clears on the next pull (once per run).
  The fixture now pulls again on failure (a read, bounded at 50 attempts).
  `fix-04` also shows REFUSED 3 of 3 once the pull is retried.
- `fix-05-follower-refused-after.log`: after the fix, the new test 5/5 plus
  `TestPrimaryRuntime*`, `TestRootMemberUpdate*` and
  `TestServingRuntimeActualElectorNativeUpdate*` 5/5 (race).
- `fix-07-ptr-negative-assertion.log`: the strengthened PTR test 5/5; with
  the fake server answering the PTR name again, 3/3 fail at the new check;
  the previous check with the same mutant passes 3/3.
- `fix-08-anchor-updater-diag.log`, `fix-10-anchor-updater-diag-after.log`:
  the empty-node updater markers before (capture lost, stopped, 2/3) and
  after (capture lost once, captured again, 3/3, no stop).
- `fix-09-anchor-capture-race.log`: the new updater test fails 3/3 before
  the fix (overlay of the old updater with only the new field); after it,
  all `TestAnchorUpdater*` 5/5 (race).
- `fix-11-seeded-login.log`: the new seeded first-start test 5/5 (race,
  about 5 s each).
- `fix-12-login-under-updates.log` (temporary
  `zz_cired2_loginload_test.go`, non-race): the sign-in table above.
- `fix-e2e-100-chunk1.log`: the fixed e2e test, 8 runs, 4 FAIL, stopped by
  hand: the machine's load average was 12-14 on 8 CPUs (other lanes, plus
  this lane's own race runs outside the e2e CPU set) and the worktree source
  changed during run 9. The failures (`failures/fix-e2e-chunk1/`): a 503
  `unavailable` on `POST /objects/dnsrecordsets` at 12 s (the command
  refusal above); "fenced former primary never named its state: status=0";
  "a refused forwarded UPDATE was applied" (a single, non-retrying
  observation that fails on any transient answer); and a first
  via-rejoined UPDATE answered SERVFAIL where the test requires NOERROR at
  once. None is a REFUSED. They are not a measurement of the fixed test on a
  quiet slot.
- `fix-e2e-100-chunk2.log` (93a9a09b, quiet slot): 23 of 24 pass (median
  about 190 s), stopped at 24 by a machine restart. Run 17
  (`failures/fix-e2e-chunk2/run-17.txt`) failed on a forwarded UPDATE through
  the rejoined former primary (via-rejoined-after) answered SERVFAIL where
  the test required NOERROR at once, right after the new primary answered two
  SERVFAILs to the native UPDATE before it. No REFUSED in any run.
- Fourth fix (5318385e, test only). That SERVFAIL, and chunk 1's run 8 on
  via-rejoined-udp, is the documented D381 answer: an enrolled follower
  answers SERVFAIL, sending nothing, when its source changes under the request
  (it installed the primary's next commit, often an asynchronous status write;
  this is also what the REFUSED of the first fix became), and relays the
  primary's SERVFAIL when a republication replaced the lease the UPDATE
  captured. RFC 2136 clients send it again; the test already did so for the
  native UPDATE just before. `enrolledFollowerUpdates` now sends each
  idempotent update again after a SERVFAIL (`enrolledEventuallyRcode`,
  bounded at 20 s, every SERVFAIL logged); any other unexpected rcode,
  REFUSED included, still fails at once, and the burst's accepted rcodes are
  unchanged. The check that the prerequisite and ungranted updates were not
  applied was one observation and failed on any transient answer (chunk 1
  run 7, "a refused forwarded UPDATE was applied" at 33 s, without node logs);
  it now waits for the authoritative NXDOMAIN over UDP and TCP and logs every
  differing answer, as the refused-fenced check does. This is a guard change
  the integrator should know about: a single forwarded UPDATE is no longer
  required to answer NOERROR on its first attempt. The node logs at the
  fixture's info level were empty in every saved failure, so the attribution
  of the two SERVFAILs to a concurrent install or republication rests on the
  code (`forwardUpdate`'s SERVFAIL paths: keyring not current, member capture
  refused, forward refused or lost, or the primary's own SERVFAIL), not on a
  logged reason.
- `fix-14-build-lint-e2e.log` (5318385e): race e2e binary built, golangci-lint
  v2.15.0 over `./test/e2e/...` 0 issues, `gofmt -l` empty, `go vet` clean.
- `fix-15-e2e-100.log` (5318385e, race, `taskset -c 0-3`, `GOMAXPROCS=4`,
  one heavy slot held for the whole run): 34 of 37 pass (median about 230 s;
  run 1 took 734 s at a load average of 25-28), stopped by hand during run
  38 to change the test. No REFUSED and no forwarded-UPDATE failure in any
  run. Failures (`failures/fix-e2e-100/`): run 29, an API port bind
  collision of enrolled-b before readiness (`bind: address already in use`,
  FLAKE-PORT-RACE, not this package); runs 30 and 37, a single command on
  the primary refused 503 `unavailable` "current writable serving generation
  unavailable": the batch deletion of the five direct records (every item,
  the first included) right after `enrolledZoneSettled`, and
  `DELETE /objects/dnsrecordsets/default/hosts` right after creating the
  successor record. Same refusal as the sign-in 503 above, met by a
  command of the test itself.
- Fifth fix (271e77bd, test only). A record change is followed by a status
  write of its zone (serial, content hash, record count), and a command
  whose request began before that write publishes is refused, as the test's
  own comment before the direct apply says. `enrolledZoneObjectSettled`
  required only Ready for the current generation, already true from the
  zone's first status write, and an unchanged resourceVersion across one
  DNS answer, so it could return before the latest record change's status
  write. It now also requires the zone's status serial to equal the SOA
  serial the primary serves; the deletion of hosts now waits for that settle
  after the successor record is created. This is a stronger wait, not a
  weaker assertion; the refusal itself is unchanged.
- `fix-16-zone-settle-smoke.log` (271e77bd): both enrolled process tests
  pass (`TestEnrolledClusterProcessesPartitionCatchupAndRestart` 163 s,
  `TestEnrolledClusterProcessesPromoteAndRejoin` 210 s; the latter logged
  two SERVFAILs on the native UPDATE after promotion and one burst SERVFAIL,
  all within the accepted answers). `fix-17-lint-e2e.log`: golangci-lint
  0 issues, `gofmt -l` empty.
- `fix-18-e2e-100.log` (271e77bd, same command as fix-15): 39 of 40 pass
  (median 224 s), the last 34 consecutive; stopped by hand during run 41 to
  free the shared slot. Run 6 (`failures/fix-e2e-100b/run-6.txt`) failed at
  27 s: `POST /objects/dnszones` on the freshly started primary, right after
  its first login, refused 503 `unavailable`; the empty node publishes
  again after it first reports ready (the diagnostic timelines show
  readiness 200 -> 503 -> 200 about 0.3-1 s after the first ready), and the
  zone creation's request began on the publication that replaced. Same
  refusal again; nothing in the test can wait for a publication that the
  node does not expose.
- `fix-13-lint.log` (93a9a09b): `gofmt -l` empty, `go vet ./cmd/kudurru ./test/e2e`
  clean, golangci-lint v2.15.0 over `./cmd/kudurru/...` and `./test/e2e/...`: 0
  issues. `python3 -B -m unittest discover -s hack -p 'test_bench_*.py'`: 14 OK.

Open. The acceptance of 100 consecutive passes of the fixed
`TestEnrolledClusterProcessesPromoteAndRejoin` is not met. Across the fixed
runs on a quiet slot (fix-e2e-100-chunk2, fix-15, fix-18: 101 runs, 96
pass) the remaining failures are: one API port bind collision
(FLAKE-PORT-RACE), one forwarded-UPDATE SERVFAIL (now retried as an RFC 2136
client does, 5318385e), and three single commands on the primary refused
503 `unavailable` because another publication landed while they ran (two
fixed in the test by the stronger zone settle, 271e77bd; one, the first
zone creation on a freshly started node, has no observable state to wait
for). After 271e77bd: 39 of 40, the longest streak 34. The last class is
the product behavior measured in the sign-in table: any single command
whose request began on a publication that another commit then replaced is
refused 503 with Retry-After and no effect. It is honest and effect-free,
but it is reached by an operator (about one sign-in in eight under 1-10
signed UPDATEs per second, and here by ordinary object writes right after a
start or another write), and no decision row covers it. D199 and D208 are
the closest rows, not a decision on this consequence. Proposed followup and
decision for the integrator: "A single session or object command is
refused 503 `unavailable` when another commit publishes while it runs.
Decide whether a command that has not entered its transaction may capture
again before its first effect (once, bounded), or whether clients and the
dashboard retry 503 `unavailable` with Retry-After; then remove the
test-side waits that exist only for this." Until that decision the e2e job
can still go red on this test at roughly 1-3 runs in 100. Also open: the
anchor updater's terminal state has no operator-visible signal (no status
field or metric), so no mounted test can assert that the updater is still
running after an empty first boot; the fix is proven through the real
updater with a test-only hook
(`TestAnchorUpdaterCaptureLostToConcurrentCommitCapturesAgain`) and the
diagnostic markers of fix-10. Proposed followup: expose the root anchor
updater's state (running, or stopped with its reason) in the anchor status
the API already serves. Base-versus-head rates remain non-discriminating
(0 failures at both commits for tests 1 and 3; test 5 15 and 5 runs), and
whether this pass widened the sign-in window was not measured.
`TestSupervisorLifecycle` stays with FLAKE-PRIMARY-UNAVAILABLE. The
temporary diagnostic test files remain only under `scripts-CI-RED-2/`.
