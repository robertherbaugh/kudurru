### INT-OPEN Open items of the merged Delete lifecycle packages — November 11, 2026 (D367)

This package closed what the INT-DEL-C verifier and implementer left open on
the merged tree (68a709e2), and checked that the five problems the INT-DEL-B
verifier raised are resolved there.

What now happens:

1. **A provider request that hangs until the run's bound is `ProviderFailed`
   again** (verify2 new problem 1). The INT-DEL-C fix noted every local commit
   that the run's bound cut, including the `RecordReceipt` that follows a
   provider request on the already expired context. A Cloudflare batch whose
   reread fails returns a partial receipt with the context's error, so a hung
   provider showed `Terminating/CleanupFailed`. The terminating lease now also
   notes when `Publisher.Apply` or `RecoveryPublisher.Recover` returned only
   after the bound (`terminatingLease.providerCut`, set by `provided(ctx)` in
   `applyPlan` and `recover`), and that wins over the local cut. The backoff is
   the same either way; sync passes a nil lease and is unchanged.
   D367's row does not describe reason classification, so it does not change.
2. **Cosmetic and wording** (verify2 new problem 2, review2 nit). The
   architecture page's local-failures paragraph and the `localCut` comment are
   rewrapped; the page and the operator guide's reason table now say that a
   provider request answering after the bound is `ProviderFailed`. The guide's
   license bullet no longer says "expires or is suspended": license expiry
   pauses with `LicenseUnavailable`, and `spec.suspend` with `Suspended`.
3. **Two of the implementer's untested cases now have mounted tests** on real
   SQLite, the real host, ledger and budget, and the Cloudflare SDK against the
   TLS peer: an ambiguous batch settled by the same running writer from a
   reread without a restart (one batch, then finalized), and the reviewed
   release called while the cleanup's delete batch is held at the provider
   (the release answers at once; when the batch completes, the fenced run
   records no receipt, sends no second batch and writes no
   `integration.cleanup` audit; the ledger row keeps its pending intent and
   the delete operation stays without a receipt). Both pass on the merged code:
   they pin documented behavior, they do not fix anything.
4. **INT-DEL-B verifier problems, confirmed resolved in 68a709e2 by reading
   the merged code and by the mounted run below:** the docs name the store
   ceiling of 100,000 objects or 256 MiB (docs/deploy/integrations.md:119-134,
   docs/architecture/integrations.md:386-391); a Forget cut without progress
   halves the lease's commit size and a run with progress yields
   (`forgetLimit`, `yields`, cleanup.go); the mounted multi-page test
   `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` exists and
   passed; a failed first ledger count is `uncounted` and keeps the last
   counts; and the commit pairing concern is moot after the merge, because
   both e6ab72b4 and 3e71816d are in the tree (the root's `Alive` wiring is at
   cmd/kudurru/integration_runtime.go:79).

Evidence under `.context/v020-final-20261110/evidence/INT-OPEN/`:

- `host01-provider-cut.log` (retained failure): the new host test's first
  draft expected `RecordSetsDeleted: 1`; a delete settled by a reread counts
  as recovered, not as deleted by the lease. The reason assertions passed;
  the expectation was corrected to `RecordsForgotten: 1` and exactly one
  provider delete.
- `host02-provider-cut-x3.log`: `go test -race -count=3 -run
  TestTerminatingRunCutIn ./ee/integration/host`: 9 of 9 passed
  (`TestTerminatingRunCutInALocalReadIsNoProviderFailure`,
  `TestTerminatingRunCutInALocalCommitIsNoProviderFailure`,
  `TestTerminatingRunCutInAProviderRequestIsAProviderFailure`), 16.3 s.
- `negative01-provider-cut-unnoted.log`: the same new test with an overlay
  that restores the old `case st.localCut:` fails ("timed out waiting for
  Terminating/ProviderFailed", 10.2 s).
- `host03-package.log`: `go test -race -count=1 ./ee/integration/host
  ./ee/integration/ledger`: ok 43.4 s and 5.8 s.
- `mounted01-inflight.log` (retained failure): the ambiguous-without-restart
  test's first draft cleared only the peer's `ambiguous` flag; the peer's
  ambiguous mode also sets `unreadable`, so reads kept failing and the test
  timed out in `ProviderFailed` with no second batch sent. The release test
  passed in that run.
- `mounted02-inflight-x2.log`: both new mounted tests, race, count=2: 4 of 4
  passed, 24.9 s.
- `mounted03-delete-selection.log`: race, `-run
  '^(TestIntegrationDelete|TestIntegrationAdminTerminatingStateAndReviewedRelease|TestIntegrationRuntimeDelete|TestIntegrationRoot)'`
  on bb144c16: 17 of 17 top-level tests passed, 0 skipped, 170.3 s
  (the multi-page history test 66.7 s).
- Review fix, recover path (79ad173c). The Apply-path test never reaches
  `RecoveryPublisher.Recover` (its delete settles from a reread), so the
  `provided(ctx)` note after `Recover` had no test that failed without it.
  `TestTerminatingRecoveryCutInAProviderRequestIsAProviderFailure` leaves a
  pending delete intent with a partial receipt, resumes it through `Recover`,
  which hangs until the run's bound and answers with a partial receipt and
  the context's error, and expects `ProviderFailed` and no `CleanupFailed`
  before the next run settles the delete from a reread (two deletes, one
  recovery). The fixture now also records the terminating reasons from the
  host's log, because a wake can fence the status write of the run it
  follows; the Apply-path test checks those reasons too.
  - `fix-host01-recover-cut.log`: the new test alone, race: passed, 1.14 s.
  - `fix-negative01-recover-unnoted.log` (retained, not a valid control): an
    overlay deleting `st.provided(ctx)` after `Recover`, run before the
    fixture recorded reasons from the log. The test passed although the log
    shows `CleanupFailed`: the published statuses had missed it. This is why
    the fixture now reads the log.
  - `fix-negative02-recover-unnoted.log`: the same overlay with the final
    test: 3 of 3 failed ("a run cut in a recovery request was reported as
    CleanupFailed: [ProviderFailed CleanupFailed]"); the other three
    `TestTerminatingRunCutIn*` tests passed 3 of 3.
  - `fix-host01-cut-x5.log`: the four `TestTerminating*Cut*` tests, race,
    count=5: 20 of 20 passed, 28.3 s.
  - `fix-host02-package-x2.log`: `go test -race -count=2 -p 2
    ./ee/integration/host ./ee/integration/ledger`: ok, 85.7 s and 6.2 s.
    `fix-host09-package-head.log`: the same at 79ad173c, count=1: ok, 54.0 s
    and 3.8 s. `fix-vet01.log` (go vet, clean) and `fix-lint01.log`
    (golangci-lint v2.15.0, 0 issues).
- Flaky existing test, not changed (review finding 2):
  `TestActualSQLiteSourceWaitsWithOriginalCancellationAndWriterToken/lease_cancel`
  (source_sqlite_test.go:303, last changed by 0a0e5a5e, outside this
  package) fails under load with "canceled original writer entered command
  ... 1". `fix-host03-sqlite-source-x30.log` and
  `fix-host04-sqlite-source-before-x30.log`: 30 of 30 passed each;
  `fix-host05-sqlite-source-before-cpu12-x40.log` (`-cpu 12 -count=40`): 3 of
  40 failed on unmodified HEAD. By code reading the outcome is safe: the
  lease reaches the source scope's context through `context.AfterFunc`
  (source_scope.go:74), which cancels on another goroutine, but the final
  commit check (`sourceLifetime`) reads `s.lease.Err()` directly, so a
  command that enters before the cancel lands is refused by
  `RequireCommitCheck` and commits nothing (the answer carries an error in
  every failure). The test's `commands == 0` assertion assumes the cancel
  has propagated before the writer is released. An experiment that waited
  for the source scope's context to be done before releasing the writer
  (`fix-host06-sqlite-source-after-cpu12-x40.log`: 1 of 40 still failed;
  `fix-host07-*-x40-{1,2,3}.log`: 120 of 120 passed;
  `fix-host08-sqlite-source-instrumented-x300.log`: 300 of 300 passed with
  logging) narrowed the race but did not remove it, and the remaining path
  is not explained, so the experiment was reverted and nothing was
  committed. For dev-docs/followups.md: find the remaining path, then either
  make the cancel deterministic before the command can enter or assert only
  that the command was refused and nothing was committed (the test's
  revision and dedup checks), and prove separately that a lease canceled at
  that instant cannot commit.
- `lint01.log`: golangci-lint v2.15.0 on `./ee/integration/host/...
  ./cmd/kudurru/...`: 0 issues. `vet01.log`: `go vet ./ee/integration/host`
  clean. gofmt clean for every touched file.

What remains open: DEL-8 (Kubernetes) is not done; no Integration runs there.
Still without a mounted test: replica promotion during a UniFi or webhook
cleanup, the SDK bound of 200 effects per batch with one budget charge per
HTTP attempt (adapter tests only), and a credential rotated between a partial
delete and its recovery (host level only). The new release-race test has no
negative control: it pins behavior that already held, and the fences it
relies on are covered by the failover test's negative overlay from INT-DEL-C.
The store ceiling is still known by code reading only, and the
26,000-operation probe was not rerun. The flaky source test above is unresolved.
A run cut inside a provider request by
a slow local fence or budget charge that the request itself makes still
shows `ProviderFailed`, as before. The tracker rows, the decision log, the
changelog and the stale sample at dev-docs/design-record.md:1062 remain the
integrator's (text in out/INT-DEL-C/). The full `./cmd/kudurru` package was
not run (only through hack/go-test.py).
