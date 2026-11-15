### FLAKE-CLOCK-AND-READINESS Fixture clock race, static twin readiness and import step cut — November 12, 2026 (no decision)

The fixture clock data race is fixed, and has been since CI-RED. The serving
command, TSIG and branding fixtures set their runtime's `opts.Now` once, before
the runtime starts, to `fixtureClock.Now` (an atomic pointer to the clock
function). Tests swap that function with `Set`, and components built with a
construction-time clock take `currentClock()`. That change (`d65d0446` on this
lane, `ebe67588` on the PR branch) was already on the lane head. The race
report in `batch1-rerun02` came from the head before it. This package checked
that no test still assigns `opts.Now` on a running runtime. The five tests that
still assign `opts.Now` do it on an unmounted receiver's copy of `opts`, which
no running goroutine reads. The package then ran every test that swaps the
clock under the race detector. No product clock code and no assertion changed.

`TestRunServeStaticTwinsReadOnlyPeerLossRestart` had a readiness wait that
looked like a stall. It is not one. It is CPU starvation of a race-instrumented
startup. Tracing (diagnostic code, not committed) logged every publisher attempt
with its outcome and duration, every Wake caller, every store generation commit
with its caller, every raw import step refusal, and the per-thread scheduler
statistics. The debug server logs were kept. A static node becomes ready after
a fixed sequence of about 15 preparation attempts and 10 commits. The commits
are: bootstrap and object load, controller and list fetcher status, the zone
lifecycle CAS steps, Admission and PrimaryStatus. The last four of them each
wait for the previous publication. About two thirds of the early attempts are
superseded by those commits or by Wakes, and nothing livelocks. Once the commits
stop, each preparation publishes. The work is CPU bound. On one dedicated CPU
under `-race`, node-a needs about 9.9 s of CPU and is ready at 9.9 s wall with
0.2 s of run-queue wait. Without `-race` it needs 1.0-2.0 s. One equal-priority
busy loop on the same CPU makes the same sequence run at half speed: node-a has
had 9.9 s of CPU and 10.4 s of run-queue wait at the 20 s mark, and the old test
fails with exactly the hosted message `complete serving generation unavailable:
preparing`. With two busy loops it has had 6.7 s of CPU and 14.8 s of wait. In
the starved runs the publisher reaches the same revisions in the same order,
only later.

A real node does not stall here. Preparation always makes progress, and a
non-race node on one core is ready in 1-2 s. A real node would need a 10-20x CPU
shortfall to take 20 s, and no product deadline depends on that time. The fix is
in the harness. `startStaticMount` keeps its 20 s readiness budget but measures
it with `readinessBudget`: wall time minus the starved part of it, which is the
run-queue wait of the process's threads (`/proc/self/task/*/schedstat`) divided
by GOMAXPROCS. A 2 minute wall ceiling still applies. On a machine with idle
CPUs, a stall builds up no run-queue wait and still fails after 20 s. On a
platform without scheduler statistics the budget is plain wall time. Each
readiness logs, and each failure reports, the wall time, process CPU time and
run-queue wait. A later failure therefore shows whether it was starvation. The
budget is used by all 15 tests that call `startStaticMount`.

`TestServingImportSettingsSynchronousAndBackground` had a separate cause: two
defects in the product's import step command (`MigrationCommand`), now fixed.
Under starvation it failed 3 of 8 times before the fix (`diag09`). A later
import step waits up to 5 s for a current cut and then runs its command once.

- In two of the failures the settings step's raw error was `serving:
  generation pending` (reported as `current writable serving generation
  unavailable`). The import's zone commit (rev 5) is followed by the zone
  lifecycle driver's own commits (6, 7 and 8). After rev 8 was published the
  settings step captured that cut, the driver committed rev 9, and the
  step's command was refused by the memory-only lease check before anything
  was authenticated, prepared or staged. The step then ended the import
  partial. A real node can hit this: a publication wakes both the import's
  next step and the lifecycle driver. Now a step whose command is refused
  with `serving.ErrPending` before its transaction body began is treated
  like a cut that is not yet published: it waits for the next publication
  and captures again, within the same 5 s wait. Once the body begins (the
  plan is prepared, the raw proof reauthorized and the effect staged there),
  any refusal is final, so a refused or uncertain mutation is still never
  replayed. The commit check and every refusal from inside the transaction
  stay terminal.
- The third failure, `servingauth: owner bounds exceeded`, was the same
  wait: the 5 s wait context also bounded the step's command, because the
  command ran on the context that carried the captured lease. In the trace
  the step began at 10:08:42.672 and was refused at 10:08:47.737, 5.07 s
  later, mid-transaction (`transaction rollback unconfirmed`). The
  cancellation surfaced as `ErrBounds` because `captureCurrent` maps every
  `CheckSectionBounds` error, including a canceled context, to that
  refusal. `servinghttp.WithCommandStep` now takes the capture context
  separately, and the command runs on the step's own context (still bound by
  the request or job context and the program's 10 minute deadline). The
  comment there already said the wait was for the cut only.
- The fourth symptom, the background job missing the test's 10 s
  `importSettingsJournal` wait, was not traced separately. It did not recur
  in the starved replay after the fix (`fix-13`), which is consistent with
  it being the same 5 s wait plus the refused step, but that is not proven.

Evidence under `.context/v020-final-20261110/evidence/FLAKE-CLOCK-AND-READINESS/`:

- `race01-clock-swap-tests-count2.log`: all 85 tests that swap a fixture clock
  (found by following calls from every `clock.Set` and `runtimeClock.Set`
  caller), plus the 5 tests that assign `opts.Now` on a receiver copy. They ran
  `-race -count=2`, `GOMAXPROCS=4`, pinned to 4 CPUs, at `2b073574`: 170 of 170
  passed in 898 s, with no `DATA RACE`.
- `diag01`-`diag03`: the static test under `-race` on 2 CPUs (6 of 6 passed)
  and on 1 CPU (4 of 4 and 2 of 2 passed), with debug server logs and the
  publisher, Wake and generation-commit traces (`diag0N-trace.txt`).
  `diag04-cpu.prof`: a CPU profile on 1 CPU, 37.97 s of samples in 38.97 s of
  wall time, about half of it in the race runtime.
- `diag05`/`diag06`/`diag07` (`diag06` and `diag07` are retained failures): the
  static test on one CPU before the fix with 0, 1 and 2 busy loops. 0: passed,
  node-a ready at 9.9 s wall with 9.8 s CPU. 1: failed `static server never
  ready node-a ... preparing` at 20.0 s with 9.9 s CPU and 10.4 s run-queue
  wait. 2: failed the same way with 6.7 s CPU and 14.8 s wait.
- `diag08-batch1-replay.log`: batch 1 (91 tests, as `hack/go-test.py` splits
  it) before the fix, `-race`, 4 CPUs, load 6-16: passed. node-a was ready at
  7.1 s wall with 17.0 s CPU. 131 goroutines at its start, so earlier tests
  had leaked no work.
- `diag09-import-settings-1cpu-1competitor.log` (retained failure): the import
  settings test, `-count=8`, on one CPU with one busy loop, before the fix:
  3 failures as described above, with the raw step errors in `diag09-trace.txt`.
- `diag10-static-norace-1cpu.log`: the static test without `-race` on one CPU:
  3 of 3 passed. node-a was ready in 1.0-2.0 s.
- `fix01`/`fix06-budget-unit-race.log`: `TestReadinessBudgetChargesOnlyRunnableTime`
  and `TestProcessSchedStatReadsThisProcess` passed 3 of 3 under `-race`.
- `fix08-static-4cpu-unloaded.log`: the fixed static test, 4 CPUs, no busy
  loop: 3 of 3 passed in 96 s. node-a was ready at 7.4-8.9 s wall.
- `fix09-static-mount-callers.log`: all 15 tests that use `startStaticMount`,
  `-race`, 4 CPUs: 15 of 15 passed in 214 s.
- `fix07-static-1cpu-1competitor.log` (retained failure): the fixed test with
  the starvation of `diag06`. node-a's readiness now passes, at 21.9 s and
  21.3 s wall, where the old test failed at 20 s. Both runs then fail later at
  the test's 5 s HTTP client timeout on `POST /auth/login`. `fix02`, `fix03`
  and `fix04` are retained runs of an earlier version of the budget that did
  not divide the run-queue wait by GOMAXPROCS. Readiness passed in every one,
  including at 24-33 s wall on 2 CPUs with 2 busy loops (`fix04`), and the
  heavier-starved runs failed at the same login timeout.
- `lint01.log`, `lint02.log`: golangci-lint v2.15.0 over `./cmd/kudurru/...`
  found 0 issues. `gofmt -l` is empty.
- `bench-guards01.log` (retained failure, not caused by this package):
  `test_bench_socket_calibration` fails on this lane branch because
  `internal/dnsserver/quic_socket_test.go` differs from its pinned digest. The
  file was changed by the earlier lane commit `c510158d` (testport). The PR
  branch restored it in `2133f47e`, and this package did not touch it.

- `fix-10-migration-cut-after.log`: the two new tests,
  `TestServingMigrationStepRecapturesCutSupersededBeforeDispatch` (a store
  commit lands after the settings step captured its cut, and the hook waits
  until `CheckWriter` reports the cut superseded) and
  `TestServingMigrationCutWaitDoesNotBoundTheStepCommand` (5.5 s between the
  capture and the command), `-race -count=2`: 4 of 4 passed in 17.8 s.
- `fix-11-migration-cut-old-logic.log` (retained failure, expected): the same
  tests with the previous step logic restored in the working tree (command on
  the wait context, a refusal after capture final). Both failed with `Import
  stopped before further writes: unavailable: current writable serving
  generation unavailable`. The working tree was restored before committing.
- `fix-12-migration-regression.log`: every test matching `TestServingMigration`,
  `TestServingImport`, `Migration`, `TestReadinessBudget` and
  `TestProcessSchedStat` in `./cmd/kudurru` plus `./internal/api` and
  `./internal/servinghttp`, `-race`: 66 top-level PASS, 0 FAIL, no `DATA RACE`.
- `fix-13-import-settings-1cpu-1competitor.log`: the `diag09` condition after
  the fix (one CPU, `GOMAXPROCS=1`, one equal-priority busy loop on the same
  CPU, `-count=8`): 8 of 8 passed (17.6-19.7 s each), where `diag09` failed 3
  of 8. The log header names `b419e103` because the binary was built from the
  uncommitted working tree that became `28468b1e`/`fb3edc1a`.
- `fix-14-batch1-replay-4cpu-run{1,2,3}.log`: batch 1 (the 90 tests of
  `diag08`, as `hack/go-test.py` split it when `CI-RED/batch1.log` failed),
  `-race`, pinned to 4 CPUs, `GOMAXPROCS=4`, under the other lanes' load (load
  average 4.6 to 26.0): 3 of 3 runs passed, 90 of 90 tests each, no `DATA
  RACE`, about 6-7 minutes each. node-a was ready at 6.2-6.6 s wall with
  0.3-2.1 s of run-queue wait, node-b at 7.4-8.1 s with 0.5-5.4 s. These runs
  were not starved enough to need the budget, so they show the change is
  harmless under batch conditions, not that it turns a batch failure into a
  pass. Every readiness line now records the scheduler figures, so a
  recurrence will show whether it was starvation.
- `fix-15-static-2cpu-2competitors.log` and `fix-16-static-4cpu-4competitors.log`:
  the committed budget (with the GOMAXPROCS division) under multi-P
  starvation, `-count=2` each, 2 of 2 and 2 of 2 passed. 2 CPUs, 2 busy loops:
  node-b ready at 17.0-17.7 s wall with 16.6-21.3 s of summed wait, charged
  8.7-7.1 s. 4 CPUs, 4 busy loops: node-b ready at 16.4-19.6 s wall with
  39.2-54.9 s of summed wait, charged 6.6-5.9 s. In the 19.6 s run the old
  fixed 20 s deadline had 0.4 s left. The credit is the summed wait divided
  by GOMAXPROCS, so it equals the real delay only when every P is runnable.
  The effective wall limit for a 20 s budget is 20 s / (1 - wait rate /
  GOMAXPROCS): about 50 s in `fix-15` (1.2 of 2 threads waiting on average),
  about 67 s in `fix-16` (2.8 of 4), and about 28 s for the unloaded `fix08`
  figures (1.0 of 4). Starvation is therefore only partly discounted when
  few threads are runnable, which is the conservative direction. The 2
  minute ceiling bounds it.
- `TestProcessSchedStatReadsThisProcess` no longer fails where per-thread
  schedstat is missing. It locks its goroutine to one OS thread, gates on that
  thread's `/proc/thread-self/schedstat` (absence is the supported wall-time
  fallback, covered by the fake-stat case of
  `TestReadinessBudgetChargesOnlyRunnableTime`), and checks monotonic progress
  on that thread's own counters instead of process-wide sums that shrink when
  a thread exits. Passed in `fix-12` and all `fix-14` runs.
- `fix-17-lint.log` (retained failure: two `revive` unused-parameter issues in
  the new test, fixed in `5d01c0cb`) and `fix-19-lint.log`: golangci-lint
  v2.15.0 over `./cmd/kudurru/...` and `./internal/servinghttp/...`: 0 issues.
  `gofmt -l` is empty. `fix-18-bench-guards.log`: the same single pre-existing
  `test_bench_socket_calibration` failure as `bench-guards01.log`.

Open: the static test's 5 s HTTP client timeouts (`POST /auth/login` and the
other requests around `serve_static_cluster_test.go:164-177`) still fail under
starvation heavier than any batch replay (1 CPU shared with a busy loop,
`fix07`). They were left unchanged on purpose: they bound single requests to
a ready server, no replay hit them, and raising them would only hide a slow
request. The budget was not applied to the 15 s join wait in `stop()` or to
the other mounted tests with their own fixed 20 s readiness loops (63
occurrences of `20 * time.Second` in `cmd/kudurru/*_test.go`). If a replay
shows one of them failing with the same signature, reuse `readinessBudget`
there. `captureCurrent` still maps a canceled context to `servingauth: owner
bounds exceeded`, which made this failure look like a bounds problem; a
follow-up should return the context error instead (not changed here, to keep
this package to the reproduced defects). The import step re-capture refines
the dispatch-once rule for migration steps. It may deserve a decision-log row
recording that a refusal before the step's transaction body began is not a
dispatch (this package was assigned no decision number). The full `cmd/kudurru`
suite through `hack/go-test.py` was not run here; only batch 1 (three times)
and the targeted sets above.
