### CI-RED Hosted CI red on the merged head — November 11, 2026 (no decision)

Both hosted failures of the merged head 68a709e2 are fixed in test code and CI
configuration; no product code, deadline or assertion changed and no existing
test is skipped. After review, the fixture-to-tag comparison that the first fix
removed is restored as its own test, and the serving fixtures' clock race found
in the batch-1 replay is fixed.

The helm job failed in its "Generated deployment assets are current" step:
`make deploy-assets` is `go test ./deploy -update`, and under `-update`
`TestChartUpgradeFromV01` regenerated `deploy/testdata/v0.1.0/chart` from the
`v0.1.0` tag with `git archive`. The hosted checkout has no tags, so the step
failed with `git archive v0.1.0 (fetch the tag): exit status 128`. The committed
0.1.0 fixtures (five small YAML files of immutable fields) describe a release that
never changes, so they are not deployment assets: they are now regenerated only
with their own flag, `go test ./deploy -run TestChartUpgradeFromV01 -update-v01-chart`,
whose failure message names `git fetch origin tag v0.1.0`. `-update` and the plain
run only compare. Every other user of the tag was checked: only
`kudurrutest.Previous` builds the old binary, it is used only by `test/e2e`, and
the e2e job already fetches the tag and sets `KUDURRU_REQUIRE_PREVIOUS_RELEASE`.

Moving regeneration off `-update` removed an incidental guard: wherever the tag
existed, `make deploy-assets` plus `git diff --exit-code -- deploy` caught a
fixture that no longer matched the tag. `TestChartUpgradeV01FixtureMatchesTag`
restores it explicitly: it renders the 0.1.0 chart from the tag and requires each
of the five fixtures to match byte for byte (the writer and the check share
`v01Fixture`). Without the tag it skips, or fails when
`KUDURRU_REQUIRE_PREVIOUS_RELEASE` is set. The helm job now fetches the `v0.1.0`
tag (the same `git fetch` line as the e2e job) and sets that variable on its
"Generated deployment assets are current" step, so hosted CI runs the comparison
and a lost tag fails instead of skipping.

The serving command, TSIG and branding fixtures now set their runtime's
`opts.Now` once, before the runtime starts, to a `fixtureClock` (an atomic
pointer to the clock function). Tests swap and restore the function with `Set`
instead of assigning `f.s.opts.Now` while the runtime's goroutines read it.
Components a test builds with a construction-time clock (HTTP adapters,
`api.Server`) take `currentClock()`, the function in place at that moment, so they
still do not follow a later swap. All 21 swap sites in 12 test files and the
branding fixture's unblock and licensed-adoption sites were converted; the zone
notify test's counter became atomic.

go test 1/2 failed in `TestServingBlockingWriterCanceledAfterCommitBeforeObservation`
(`helper` and `blocking`) with `providerresources: owner closed` at
serving_runtime_test.go:140. Mechanism: the test builds a second provider
coordinator over the fixture runtime's provider resource owner, and closing that
coordinator closes the shared owner. Its cleanup was registered after the
fixture's cleanups, so it ran first. Since 7f351615 (`test(serving): stop fixture
runtimes after their writers`) the always-on runtime no longer ends with
`t.Context()`, so it was still running when the owner closed; its provider worker
returned `providerresources.ErrClosed` and the fixture reported it. Before
7f351615 the same cleanup only raced an already canceled Run, so 7f351615 exposed
the test's ordering bug rather than introducing a product fault. The cleanup is now
registered through the fixture's `beforeStart` hook, so it runs after the writer and
the runtime have joined, with its own bounded context.

Evidence under `.context/v020-final-20261110/evidence/CI-RED/`:

- `helm00-tagless-base-repro.log`: the hosted failure reproduced at the base
  (`TestChartUpgradeFromV01`, `upgrade_test.go:94: git archive v0.1.0 (fetch the
  tag): exit status 128`).
- `helm05-shim-tagless.log` and `helm07-shim-deploy-and-assets.log`: with a `git`
  shim on `PATH` that refuses every `v0.1.0` reference inside the worktree (a
  tagless checkout), `go test -count=1 ./deploy/...` passes (13.7 s) and
  `make deploy-assets` passes (12.7 s) with a clean tree afterwards;
  `-update-v01-chart` under the shim fails with the fetch hint; with the real tag
  it reproduces the committed fixtures byte for byte. `helm06` is a retained run
  where the first shim also refused `v0.1.0-rc.*` in `TestReleaseTagScript`'s
  scratch repository (a shim scoping error, not a product failure).
- `helm01`-`helm04`: the rest of the helm job's steps (`make helm-check`,
  version-sync unit tests, `make version-check`, deploy-assets diff, goreleaser
  check) pass at e84395b9.
- `serving01-base-repro.log`: the hosted failure reproduced at the base, three
  failures (`helper` and `blocking`) in a repeated run.
- `serving02-fixed-race.log` and `serving04-fixed-race-count40.log`: with the fix,
  the test passes repeatedly under `-race` (serving04: `-count=40`, 4 CPUs, ok in 76 s, no `owner closed`).
  `serving03` was stopped by the agent tool's time limit before a result.
- `batch1.log`..`batch8.log`, `batch1-rerun02.log`, `batch1-rerun03.log`: the
  complete `cmd/kudurru` inventory as `hack/go-test.py` splits it at this head
  (724 tests, 8 batches of 90-91; the hosted log of the older head showed 7), each
  run with `go test -race -count=1 -timeout=30m`, `GOMAXPROCS=4`, pinned to 4
  CPUs. Batches 2-8 pass (220-508 s each). Batch 1 passed on its third run
  (`batch1-rerun03`, 501 s). Its two retained failures are intermittent and
  unrelated to this package, see below.
- `fix01-clock-probe-before-and-after.log`: the new
  `TestServingCommandFixtureClockSwapWhileRunning` passes under `-race` (0.84 s);
  a temporary probe in the same run, identical except that it assigns
  `f.s.opts.Now` as the tests did before, fails with `WARNING: DATA RACE` /
  `race detected during execution of test` (the probe was not committed).
- `fix02-clock-touched-tests-race-count2.log` (retained failure): the first run of
  the 102 tests of the touched files panicked in
  `TestServingTSIGInitializedFollowerRestartAndOriginalPromotion`, a nil
  `fixtureClock` in a fixture literal without one (`tsigCommandHTTP`). Fixed with
  `currentClock()`, which falls back to `opts.Now` for such literals.
- `fix05-clock-fixture-users-race.log` (retained failure): the 229 tests of every
  file that uses these fixtures, `-race -count=1`, 4 CPUs, 838 s: no data race;
  one failure, `TestServingImportSettingsSynchronousAndBackground/sync/object-and-settings`
  (`serving_import_settings_test.go:57: ... unavailable: current writable serving
  generation unavailable`), a readiness stall under load in a test that never
  swaps a clock (its branding runtime clock only delegates). It passed
  `-count=8` under `-race` in isolation (`fix06-import-settings-count8.log`, 62 s).
- `fix07-clock-touched-tests-race-count2.log` (retained, no result): the same run
  as fix09, cut off by the machine restart before it finished.
- `fix09-clock-touched-tests-race-count2.log`: the 102 tests of the 20 touched
  files, `-race -count=2`, 4 CPUs, at d65d0446: ok in 944 s, no data race.
- `fix10-batch1-head.log`: `cmd/kudurru` batch 1 (91 tests, the batch that failed
  twice before) replayed at d65d0446 as `hack/go-test.py` splits it,
  `-race -count=1`, `GOMAXPROCS=4`, pinned to 4 CPUs: ok in 350 s.
- `fix03-helm-fixture-guard.log`: with the tag, a fixture with one object deleted
  passes `TestChartUpgradeFromV01` but fails `TestChartUpgradeV01FixtureMatchesTag`
  (`default.yaml is not what the chart of the v0.1.0 tag renders`); the clean tree
  passes both. With the tag hidden by a `git` shim, the new test skips, and fails
  with `KUDURRU_REQUIRE_PREVIOUS_RELEASE=1`; `make deploy-assets` passes (11.1 s)
  with no diff under `deploy/testdata`, `deploy/helm` or `deploy/kustomize`;
  `-update-v01-chart` fails before writing with the fetch hint. With the tag,
  `-update-v01-chart` rewrites the fixtures byte for byte and `./deploy/...` passes
  (11.7 s). Retained in the same log: a first `make deploy-assets` whose shim also
  hid `v0.1.0` from `TestReleaseTagScript`'s scratch repository (shim scope), and
  two runs that passed the package after the test flag and did not build.
- `fix04-lint-deploy.log`, `fix08-lint.log`: golangci-lint v2.15.0 over
  `./deploy/...` and `./cmd/kudurru/...` after the review fixes: 0 issues;
  `gofmt -l` empty.
- `lint01.log`: golangci-lint v2.15.0 over `./deploy/...` and `./cmd/kudurru/...`: 0 issues;
  `gofmt -l` empty and `go vet ./deploy/` clean for the touched files.

Intermittent failures recorded for later packages (not fixed here):

1. `TestRunServeStaticTwinsReadOnlyPeerLossRestart` (`batch1.log`):
   `serve_static_cluster_test.go:157: static server never ready node-a not ready:
   serving(complete serving generation unavailable: preparing)`, the 20 s readiness
   budget of `startStaticMount` expiring under load. It passed in isolation
   `-count=5` (`static01`) and in both batch-1 reruns.
2. `TestServingBlockingCurrentProofFinalClockAndRollback/changed-backwards`
   (`batch1-rerun02.log`): a race-detector report. The test swaps and restores
   `f.s.opts.Now` while the fixture's always-on runtime runs; the integration host's
   refresh (`integrationRuntime.capture`, integration_runtime.go:157) reads
   `r.s.opts.Now` from its own goroutine. 0 of 20 in isolation (`race01`). Fixed
   after review by `fixtureClock` (above).
3. `TestServingImportSettingsSynchronousAndBackground/sync/object-and-settings`
   (`fix05`): a writable serving generation not ready in time under load, the same
   class as 1.

Open: the readiness stalls (1 and 3) are not fixed; `startStaticMount` does not
capture server logs, so the cause of the static twins stall is not diagnosed.
Capturing per-node server logs there would need a change to how `runServe`
routes its process-wide logger, which is out of scope for this package.
`hack/go-test.py` has no retry, so one such stall still fails a shard. After the
clock fix only batch 1 (`fix10`) and the tests of the files that use the changed
fixtures (`fix05`, `fix09`) were replayed; batches 2-8 were not rerun (they
passed once each before the fix, and the fix only touches test fixtures). The other packages
of go test shard 1 (everything except `cmd/kudurru`) were not replayed, and the
helm job's new tag fetch has not run on hosted CI yet. Hosted CI being green is
therefore likely but not proven by this package.
