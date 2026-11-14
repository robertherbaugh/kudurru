### FLAKE-OIDC-WRITER fixture teardown order — November 11, 2026 (no decision number)

**Answer to the integrator's question:** the failure predates the first
completion batch. No commit between `33ee04bc` and `de0d249a` introduced it, and
there is no measurable worsening. It is a defect of the test fixtures, not of the
product.

What now happens: the mounted serving fixtures (`runServingFixture`, the branding
fixture, the TSIG command fixture workers and the held TSIG follower) run the
always-on runtime under `servingFixtureAlwaysOn(t)`
(`cmd/kudurru/serving_runtime_test.go`), a context that does not end with
`t.Context()` and is canceled only in the fixture's own cleanup, after the
elected writer has been canceled and joined. The held TSIG follower
(`tsigABQNewFollower`, `cmd/kudurru/serving_tsig_authority_test.go`) used to
cancel its writer and its always-on runtime back to back and join both
afterwards, with the writer's election context derived from the always-on
context; since the review follow-up it cancels and joins the writer first,
reports an error if the runtime had already stopped by then, and only then
cancels the always-on runtime, and the writer's election context is no longer a
child of the always-on context. That is the order the server uses at
shutdown: `cluster.Runner.Run` cancels the election and waits for the leader
group before it cancels the always-on group, each under its own
`context.WithoutCancel` child (`internal/cluster/runner.go`). The fix is commit
`7f351615 test(serving): stop fixture runtimes after their writers` (merged at
68a709e2 with its errcheck follow-up), test files only. No writer-liveness
assertion, deadline, `transientServing` or product code changed.

Mechanism (four instrumented failures in `diag02`, timing probes `probe03`-`06`
on both trees): the testing package cancels `t.Context()` before it calls the
first cleanup, and Go delivers the cancellation to the children of a context one
at a time in map order. The fixture's always-on context and its elector context
were sibling children of `t.Context()`. When the always-on context was reached
first, `servingRuntime.Run` returned, its `defer s.cancel()` ended the runtime
lifetime that `begin` ties every `RunWriter` child to (`context.AfterFunc(s.life,
cancel)`), `RunWriter` returned `context.Canceled`, and the fixture evaluated
`ctx.Err() == nil` on the term context 60 to 350 microseconds before the same
cancellation reached the elector context. The probes measured the same walk on
the base and on the head. A production writer cannot see this: the always-on
group is canceled only after the leader group has joined, and an always-on
failure is fatal to the whole runner, which then cancels the election first as
well.

Why the batch is not the cause (valid controls, same command per tree, scripted
in `rate.sh`/`rate2.sh`, one race binary per tree built from a clean checkout;
the base binary rebuilt in r2 has the same SHA-256 as the one the first controls
used, `41994de6...`):

| Measurement | base `33ee04bc` | head `de0d249a` | fixed |
|---|---|---|---|
| contention: `GOMAXPROCS=4 taskset -c 0`, `-test.count=10`, three interleaved rounds under other lanes' load (load 4-7; `contention03`-`09`) | 3 failing runs of 30 (1, 1, 1) | 3 of 30 (3, 0, 0) | 0 of 10 (`contention05`) |
| `taskset -c 0-3`, `-test.count=30`, two rounds under load 3-7 (`rate-*-round1/2`) | 0 of 60 | 3 of 60 | — |
| r2, quieter machine (load 2-5): `taskset -c 0-3`, `-test.count=30` x2 (`r2-06`, `r2-07`) | 0 of 60 | prefix overlay at d65d0446: 0 of 60 | — |
| r2 contention, two rounds (`r2-13`, `r2-14`, `r2-15`) | 0 of 20 | prefix overlay: 0 of 20 | head d65d0446: 0 of 20 |

Every failure on either tree is the same line,
`serving_http_oidc_test.go:113: browser writer ended context canceled`. The base
fails with it under the identical command, so the batch did not introduce it.
The 0-of-60 against 3-of-60 difference is not significant (one-sided Fisher exact
p = 0.12) and does not appear in the contention rows, where the base and the head
fail at the same rate. The unamplified rate depends on machine load (the r2 rows,
on a quieter machine, saw no failure on any tree), which is why the deterministic
test below is the proof, not the rates. The fixture and lifecycle files that
produce the failure are byte-identical on both commits
(`identical01-fixture-and-lifecycle-files.log`). The same class is in hosted CI
eleven days before the batch (run 41214453524 of October 31,
`TestServingCurrentCommandFinalCredentialClock/expiry/noop:
serving_command_test.go:68: context canceled`).

Evidence under `.context/v020-final-20261110/evidence/FLAKE-OIDC-WRITER/`:

- Deterministic reproduction, `TestServingFixtureAlwaysOnRuntimeOutlivesTestContext`
  (`cmd/kudurru/serving_fixture_teardown_test.go`): it holds an elector context
  open across the end of `t.Context()` and requires two more publications and a
  live writer in the first cleanup. At d65d0446 with `servingFixtureAlwaysOn`
  reverted to a child of `t.Context()` through a build overlay
  (`diag/r2-prefix-serving_runtime_test.go.txt`): fails 20 of 20
  (`r2-05-det-prefix-x20.log`, `the always-on runtime stopped with the test
  context and ended the writer while its own context was alive: context
  canceled`). At d65d0446 as committed: passes 20 of 20 together with
  `TestServingWriterEndsWhenAlwaysOnRuntimeStopsFirst` (`r2-04-det-head-x20.log`,
  52 s). Earlier, at de0d249a with the old fixtures: 20 of 20 failing
  (`det03`), 20 of 20 passing with the fix (`det04`).
- Deterministic reproduction on the base control: the same test file overlaid
  onto `33ee04bc` (its fixtures are the pre-fix ones; the checkout on disk stays
  pristine): `TestServingFixtureAlwaysOnRuntimeOutlivesTestContext` fails 20 of
  20 with the same message, `TestServingWriterEndsWhenAlwaysOnRuntimeStopsFirst`
  passes 20 of 20 (`r2-16-build-base-33ee04bc-det-overlay.log`,
  `r2-17-det-base-33ee04bc-x20.log`, 37 s).
- The original test 200 consecutive runs at d65d0446 under race, `taskset -c
  0-3`, four processes of 50 (`r2-08-original-head-x50-1`..`4`, 1,029 to 1,038 s
  each, load 2.6-3.9): 200 of 200 passed. Earlier, at 796c04e3 (the fix alone):
  200 of 200 passed (`confirm05-original-fixed-x50-1`..`4`).
- The hosted batch (`INTEGRATE-1/batch-oidc.txt`, 93 names) at d65d0446:
  `r2-09-batch-oidc-head-1`..`4` passed (325 to 346 s each); `r2-09-...-5`
  FAILED on two classes owned by other packages, none of them this one:
  `TestRunServeSQLiteStoredTSIGPendingStartupAndCleanup/conflicting_first_source`
  (`ops: listen tcp 127.0.0.1:33091: bind: address already in use`,
  FLAKE-PORT-RACE) and `TestServingHTTPClusterActivationCommitted`
  (`serving_command_test.go:76: replication: writer fenced`; the activation
  commits a new incarnation, the store fences the writer and `RunWriter` returns
  the fence before the elector cancels the term context; the same class as the
  `invalid-member` failure in `confirm04`, see the open paragraph). A sixth
  replay passed (`r2-19-batch-oidc-head-6.log`, 325 s): five of the six replays
  at d65d0446 passed (by count, not five in a row), and the failing one failed
  only on other packages' classes. Earlier, at 796c04e3: 5 of 5 passed (`confirm02-batch-oidc-fixed-1`..`5`).
  The base control replayed the batch twice and passed both
  (`r2-10-batch-oidc-base-33ee04bc-1`, `-2`; with `confirm06` three of three).
  The head de0d249a replay `confirm07` was cut off by the machine restart and has
  no result.
- The root, writer, anchor, member and query-log selection (133 names) at
  d65d0446: passed, 296 pass lines, no failure (`r2-11-select-root-writer-anchor-member-querylog.log`,
  427 s). The rest of the fixture users (238 names): passed, 523 pass lines, one
  skip (`TestCLIOIDCBrowserInstalledChromeActualTLS`, which needs an explicit
  browser executable), no failure (`r2-12-select-fixture-users-rest.log`, 1,005 s).
- Scoped lint of `./cmd/kudurru/...` at d65d0446: 0 issues
  (`r2-20-lint-cmd-kudurru-head.log`; the two errcheck findings of `confirm08`
  at 796c04e3 were fixed by the follow-up merged at 68a709e2). Earlier runs at 796c04e3 had one failure
  each, both retained: `confirm03`
  (`TestServingBlockingWriterCanceledAfterCommitBeforeObservation/blocking`,
  `providerresources: owner closed`, the ordering bug in that test which the fix
  exposed and CI-RED fixed in e84395b9) and `confirm04`
  (`TestServingHTTPClusterActivationIdentityRefusalAndRollback/invalid-member`,
  `replication: writer fenced`).
- Review follow-up `259540a5 test(tsig): join the held follower writer before
  its runtime`: `TestServingTSIGHeldFollowerHTTPAcrossPromotion`,
  `TestServingFixtureAlwaysOnRuntimeOutlivesTestContext` and
  `TestServingWriterEndsWhenAlwaysOnRuntimeStopsFirst` with `-count=5` under
  race passed (`fix-01-head-tsig-held-and-det-x5.log`, 23 s test time). With a
  build overlay that restores the old order (always-on runtime canceled before
  the writer joined, `diag/fix-old-order-serving_tsig_authority_test.go.txt`,
  with a 50 ms pause to make it deterministic), the new cleanup check fails 3 of
  3 with `held follower always-on runtime stopped before its writer joined`
  (`fix-02-old-order-overlay-tsig-held-x3.log`). Scoped lint of
  `./cmd/kudurru/...`: 0 issues (`fix-03-lint-cmd-kudurru.log`).
- `r2-18-owner-closed-attribution-head-x8.log` is empty: that run was cut off by
  the interruption before it started and was not repeated.

Open: the lines `complete serving generation not published ... error="providerresources: owner closed"`
followed within a second by `published again` appear in passing runs of the
original test at d65d0446 (with and without the fix: `r2-07`, `r2-08`, `r2-14`,
`r2-15`) and never on the base. That is not a new event: the log line itself is
new (`1b665699 feat(serving): log why a serving generation is not published`,
merged into the lane after the base and after 796c04e3). Which pinned resource
reports `ErrClosed` (`providerresources.Owner.WithCurrent` also answers
`ErrClosed` when it has no current snapshot, and `Pinned` answers it after its
callback ended) was not attributed; the generation recovers on the next try and
no test fails from it. The `replication: writer fenced` writer failure after a
committed cluster activation (`r2-09` run 5, `confirm04`) is a different writer
exit while the term context is alive: the fence is the expected effect of the
activation, so either the command fixture's assertion must accept a fence that
the test itself caused or the writer must observe the new authority before it
reports; this package did not change it (FLAKE-PRIMARY-UNAVAILABLE scope, or a
follow-up). `cmd/kudurru/primary_runtime_test.go:157` runs the publisher and the
elector under one context, so at the end of a test the publisher can see the
cancellation before the elected term does; by code reading that is the same
teardown order hazard as the fixtures fixed here and a candidate cause of
`primary driver ended early`. It was not tested here and is left to
FLAKE-PRIMARY-UNAVAILABLE, which should pick it up. The deterministic
regression test covers the shared `runServingFixture`/`servingFixtureAlwaysOn`
path only: a new fixture that runs the always-on runtime under a direct child
of `t.Context()` again would not be caught by any test.
