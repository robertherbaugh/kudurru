### FLAKE-PRIMARY-UNAVAILABLE writer terms, fake console health window, TOTP step edge — November 12, 2026 (no decision number)

This package covers two map entries, FLAKE-PRIMARY-UNAVAILABLE and
FLAKE-AUTH-COOKIE-AND-SUPERVISOR. There are three separate causes, with one
commit each (plus lint follow-ups). Only the first one changes product code.

**1. `primaryruntime: required adapter/publication unavailable` and `replication: writer fenced` with a live term (product, `internal/store/sqlite`).**
What now happens: the SQLite elector samples authority and registers the term
it starts in the same `wmu` section (`Elector.nextTerm`). The store keeps the
cancel function of each live term. Every path that retires writer tokens
cancels those terms *before* the change becomes visible: `publishAuthorityLocked`
when the authority changes, the new `blockWritesLocked` (the fail-closed write
block in `observeAuthorityLocked`, the divergent replica install and the
uncertain enrollment completion), and `Close`. The elector still waits on
`authorityChanged` and joins the term as before. Before the change, the store
published the new authority (or set `writeBlocked`) and left the cancellation
to the elector goroutine. In that gap, a writer that checks `ServingAuthority`
(the primary driver's `current`, the serving writer's `writerCurrent`) found its
token retired while its term context was still alive. It then returned
`ErrUnavailable` or `ErrFenced`, which the mounted fixtures report as a writer
failure: `TestServingHTTPClusterActivationIdentityRefusalAndRollback/invalid-member`
(hosted de0d249a attempt 2, `serving_command_test.go:73`) and
`TestServingHTTPClusterActivationCommitted` (`writer fenced`, FLAKE-OIDC-WRITER
r2-09 run 5 and confirm04), both after a committed cluster activation, and
`TestDoctorNotifyCannotAdoptNewPublicationKeysOrWriter/writer` (hosted
ffd18d41, `primary driver ended early`) after a fence. The doctor case had
been papered over on November 9 by a fixture tolerance (`expectWriterLoss`,
5d25cda4). That tolerance is now removed (06041187): the primary network
fixture again reports every driver error under a live term, fenced cases
included.

**Can a real node hit it? Yes.** The window opens on every authority change
the node makes or observes: cluster activation, a higher-term fence, a
promotion conflict, an uncertain enrollment completion. Nothing can be written
in it, because every mutation rechecks the writer token inside its
transaction. But `cluster.Runner.superviseLeader` saw the `serving-writer`
runnable fail while its term was alive, so it logged `leader-only runnable
failed; restarting` at ERROR, counted a restart and set `lastErr` in the runner
status, and the term ended a moment later. The defect was a false error report
and a status blemish, not a wrong write and not a lost primary.
A second, smaller part of the status blemish did not depend on the order: the
primary driver answers `ErrUnavailable` once its term is canceled, and
`Runner.invoke` recorded every error other than `context.Canceled` as
`RunnableStatus.LastError`, so even a clean end of term left
`primaryruntime: required adapter/publication unavailable` there (no log, no
restart). `invoke` now ignores an error returned after its context ended
(6ae73784); a panic is still recorded. The same rule covers an always-on
runnable that returns an error after the process context ended, which
`runAlways` already did not treat as fatal. `TestLeaderOnlyWriterEndOfTermIsNotAFailure`
(`internal/cluster/runner_term_end_test.go`) runs `cluster.Runner` on a real
SQLite elector with a writer that checks `ServingAuthority` and answers an
error on every stop, fences the store with a higher term, and requires the
writer to find its term over, `Restarts == 0` and an empty `LastError`.

**2. `TestSupervisorLifecycle`: `4 starts, want 2` (test harness, `ee/edge/dnsdist`).**
Each health probe of a staged dnsdist member is a console round trip. The fake
console re-executes the race-instrumented test binary. The harness bounded
health qualification with 250 ms. When one round trip took longer, the first
probe timed out, the supervisor restored the accepted configuration and
restarted dnsdist (start 3), and the retry five seconds later staged again
(start 4). This is the same sequence as the hosted 19bfecc4 failure. Under
contention on one CPU (`taskset -c 0` with three `yes` loops on that CPU), the
fake log shows exactly that: one `console health`, then a stop and a start with
the old backend set (`sup02`, 5 of 5 failing). The supervisor behaves as
designed for an unqualified member.
**Real node: no.** It uses the 15 s default health window and the real dnsdist
console client, which answers in milliseconds, so it restarts this way only
when a new member really stays down. The harness now uses a 5 s window, like
the live console test. `TestNamedStagingHealthFailureRetainsAcceptedFiles`,
whose subject is the rejection, sets its own 250 ms window. The fake gains a
`delay-<verb>` fault file. No product code changes.

**3. `TestServingHTTPAuthCookieFactorAndPasswordClears/confirm`: `got 401, want 204` (test, `cmd/kudurru`).**
The confirm code was computed from the wall clock 30 s back. That is the oldest
step the one-step window (`totp.Window = 1`) accepts. When a step boundary fell
between computing the code and the server checking it, the code was two steps
old and the server refused it, correctly. `TestServingHTTPFactorsSetupConfirmLoginDisable`,
`TestServingHTTPFactorsRotationAndConcurrentConfirm` (old-factor proof for
setup) and `TestServingHTTPLocalUsersResetFactorTokensAndRollback` (login,
with a publication wait between computing the code and the request) had the
same construction; the review found the last two.
**Real user: no.** An authenticator shows the current step's code, and a code
typed just before a boundary is still accepted one step later. Request budget,
replay floors and publication were ruled out: this confirm is the first TOTP
use of the fixture, and a 401 rather than a 429 means the budget was not
exhausted. All four tests now use `factorPreviousStepCall` (generic over the
call's result; the local-user login goes through `localUsersLoginWith`), which
holds the fixture clock (`fixtureClock.Set`) at one instant for the request and
sends the code of the step before it. `grep -rn "Add(-30\*time.Second)" cmd`
now finds nothing.

Evidence under `.context/v020-final-20261110/evidence/FLAKE-PRIMARY-UNAVAILABLE/`:

- Deterministic reproduction of cause 1: `TestElectorTermEndsBeforeAuthorityChangeIsVisible`
  (`internal/store/sqlite/elector_term_order_test.go`; subtests higher-term-fence,
  promotion-conflict, enable-primary, close). It requires the term to be
  canceled when the change returns, with `GOMAXPROCS(1)` so the elector
  goroutine cannot run first, and requires a writer that sees its token retired
  to find its term over. Before the fix, `-race -count=5` failed 19 of 20
  subtest runs (`det01-store-before-fix.log`). The only pass was one `close`
  run, because `Close` does more work after publishing. After the fix:
  `-count=20` passed (`det02`, 10 s), and after the lint follow-up `-count=10`
  passed (`det03`).
- Package runs after the fix under race: `./internal/store/sqlite/...`,
  `./internal/cluster/...` and `./internal/primaryruntime/...` passed (`unit03`,
  sqlite 290 s). The first `internal/primaryruntime` run failed only because
  `KUBEBUILDER_ASSETS` was not exported (three `/kube` subtests: `native envtest
  assets required`). It was rerun with the assets and passed (`unit04`, 50 s).
- Deterministic reproduction of cause 2: `TestSupervisorNamedStagingSlowConsoleHealthProbe`
  makes each health probe take 400 ms. With the old 250 ms harness window
  (temporary edit, restored) it failed 3 of 3 (`sup03`, `timed out waiting for
  promoted backend`). With the fix it passed 3 of 3, together with
  `TestNamedStagingHealthFailureRetainsAcceptedFiles` (`sup04`). The original
  test under the same one-CPU contention: 5 of 5 failed before (`sup02`) and 5
  of 5 passed after (`sup06`, 3.5 to 5.5 s each). Uncontended on one CPU before
  the fix it passed 10 of 10 (`sup01`), which is why the failure only shows up
  under load. 200 consecutive runs under race at 38d6d1c6
  (`-test.count=200`, one process each): `taskset -c 0-1` 200 of 200 passed
  (`sup07`, 272 s), `taskset -c 0-3` 200 of 200 passed (`sup08`, 278 s). All of
  `./ee/edge/...` passed under race (`sup05`).
- Reproduction of cause 3: `TestServingHTTPFactorPreviousStepAcrossBoundary`
  holds the server clock at the real current instant, sends the code the old
  construction yields when it is computed one step before the server check
  (two steps old), requires 401, and then confirms with the helper (204). It
  no longer sleeps. The version in a9fcbd05 waited for a real boundary (up to
  30 s, 28 s in `cmd01`) and needed its goroutine to wake within 100 ms of it,
  which the review flagged as a timing dependency of its own. An earlier draft
  moved the server clock 30 to 60 s ahead instead. With the assertion inverted
  to 204, that draft failed 3 of 3, but with **503**, not 401
  (`auth01-overlay-old-construction-at-boundary-x3.log`, retained). A server
  clock ahead of the publication is refused before the code check, which is
  why the clock is held at the current instant, never moved ahead.
- Mounted runs from one race binary built at the lane head
  (`cmd00-build-head.txt`; tree equal to a9fcbd05; the later closure-only lint
  change 4cacd855 and the store test lint change 47d8fff5 do not change behavior). First, the changed tests and the
  named tests: all passed (`cmd01`). Second, the selection `TestPrimary.*`,
  `TestDoctor.*`, `TestServingHTTPCluster.*`, `TestServingCommand.*`,
  `TestServingHTTPAuth.*`, `TestServingHTTPFactor.*`, `TestServingUpdateOutput.*`,
  `TestAnchorDNSOnly.*`, `TestServingAnchorDNSOnly.*`, `TestServingFixture.*`,
  `TestServingWriter.*` and `TestRootMember.*`: 70 top-level passes, one
  environment skip (`TestDoctorDatabase`, no PostgreSQL DSN), no failure
  (`cmd02`, 247 s).
- 200 consecutive runs of the three named tests
  (`TestServingHTTPClusterActivationIdentityRefusalAndRollback`,
  `TestDoctorNotifyCannotAdoptNewPublicationKeysOrWriter`,
  `TestServingHTTPAuthCookieFactorAndPasswordClears`), race, one process each:
  `taskset -c 0-1` (`cmd03-named-cpu2-x200.log`) and `taskset -c 0-3`
  (`cmd04-named-cpu4-x200.log`). With 2 CPUs, each test passed 200 of 200,
  no failure line (48 min 47 s including the lock wait). With 4 CPUs, each test
  passed 200 of 200, no failure line (44 min 29 s). The doctor test ran with
  the strict fixture, without the `expectWriterLoss` tolerance.
- Review fixes (6ae73784, 2e05ffe5, 2b073574). The runner test
  `TestLeaderOnlyWriterEndOfTermIsNotAFailure` failed 5 of 5 on `LastError`
  before the `invoke` change (`fix-02-runner-before-lasterr-fix-x5.log`;
  `fix-01` was a first draft whose writer returned nil on a canceled term, so
  it passed and showed nothing) and passed `-race -count=20` after it
  (`fix-03`, 20 s). All of `./internal/cluster/...` passed under race
  (`fix-04`). The four previous-step TOTP tests, the boundary test and every
  `TestServingHTTPLocalUsers.*` test, race, `taskset -c 0-1`, `-count=10`:
  13 tests, 10 of 10 passes each, no failure, the boundary test 2.6 to 3.7 s
  instead of up to 30 s (`fix-05-totp-sites-cpu2-x10.log`, 559 s). Scoped
  lint over `./internal/cluster/... ./cmd/kudurru/...`: 0 issues (`fix-06`).
- Mounted control for cause 1 (run by the reviewer): with the old store code
  overlaid and the strict fixture, race, `taskset -c 0-1`, about 9 of 25
  iterations finished before the background time limit
  (`review-06-mounted-oldstore-strict-cpu2-x25.log`, incomplete). It had two
  failures with the exact hosted signature, `primary_runtime_test.go:163:
  primary driver ended early primaryruntime: required adapter/publication
  unavailable`: one in `TestDoctorNotifyCannotAdoptNewPublicationKeysOrWriter`
  and one in `TestRootUpdateOutputActualCompletionAndFinalRefusals`. At the
  lane head the same selection passed 25 of 25 (`review-07-mounted-head-cpu2-x25.log`).
  The two activation tests (`TestServingHTTPClusterActivationIdentityRefusalAndRollback`,
  `TestServingHTTPClusterActivationCommitted`) passed all 9 old-store
  iterations. Their hosted failures (`serving_command_test.go:73`, `writer
  fenced`) are attributed to cause 1 by mechanism (`writerCurrent` finding
  its token retired under a live term, which the store test reproduces) and
  were not reproduced mounted.
- Scoped lint: `lint02` found SA9010 in the new helper (fixed in a follow-up
  commit) and `lint03` found errcheck/revive in the new store test (fixed in
  47d8fff5). The rerun over `./cmd/kudurru/... ./ee/edge/dnsdist/... ./internal/store/sqlite/...`
  is `lint04`: 0 issues. `go vet` and `gofmt -l` are clean for the touched packages (`vet01`).

Open: the hosted `go test 1/2` shard and the hosted batch compositions were
not replayed for this package; the claim that these three classes are gone
from CI rests on the deterministic tests and the repetition runs, and for the
two activation failures on the mechanism alone (see the mounted control). Cause 1 was
fixed in the store rather than in the fixtures: any other code that changes
authority must go through `publishAuthorityLocked` or `blockWritesLocked` to
keep the ordering. A future path that sets `writeBlocked` or swaps
`s.authority` directly would reopen the window, and only the four transitions
in the store test would catch it. The `providerresources: owner closed`
publication retries noted by FLAKE-OIDC-WRITER were not examined here.
