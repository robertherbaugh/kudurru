### F09b/F60c Integration `Delete` lifecycle, part 3 of 3: API/CLI, release command, operator docs and mounted acceptance — November 10, 2026 (D367)

This package lands DEL-5 (API and CLI terminating state and the reviewed
release command), the code-side half of DEL-6 (operator guide, architecture
page and diagram, CRD field description), and the rest of DEL-7 (mounted
restart, failover and license-expiry acceptance). The final D367 row is in
`out/INT-DEL-C/decision.md` and supersedes the drafts of INT-DEL-A and
INT-DEL-B: it adds the operator surface and the release command and changes
nothing that those packages decided.

API and CLI (DEL-5). `IntegrationSummary` (`GET /integrations`,
`GET /integrations/{namespace}/{name}`, `kudurru integrations get|list`)
carries `deletionPolicy`, `terminating` and `deletionRequestedAt`, read from
the stored object, and, while the host's status for that exact revision is
`Terminating`, its `remaining`, `blocked` and `uncertain` counts.
`kudurru integrations list` adds the columns POLICY, REASON and DELETION
REQUESTED. Check, preview, sync and act on a terminating instance were already
refused by the host (`ErrFenced`); they now have a mounted test and answer
`409` with no provider request. The new route
`POST /integrations/{namespace}/{name}/release` and
`kudurru integrations release NAME --uid --revision` are root's
(`integrationRuntime.IntegrationRelease`, `cmd/kudurru/integration_release.go`):
the object in the request's published snapshot and the one the command reads
must both be the reviewed UID and resource version, be deleting and carry the
reserved cleanup finalizer (under any policy). One current command
re-authorizes the original credential for the new permission
`integrations.release` (admin only), counts what stays, removes the finalizer
with `CommandTx.UpdateFinalizer` by that exact identity and writes an
`integration.release` audit row (`released without cleanup by the reviewed
release command: deletionPolicy is ...; removed kudurru.io/integration-cleanup.
Retained for uid ...: N ledger rows, M operations, ...`). The answer is
`{released, ledgerRows, operations}`. It needs no license, credential or
provider and does not take the per-instance lease: a running cleanup is
fenced at its next effect, and a provider request already in flight leaves
its ledger row pending. A live object, a stale UID or revision and a follower
(not the elected writer) are `409`; mode, digest or params in the body are
`400`; a viewer is `403`. The route is refused in static mode like sync and
act. Decided here (in D367): the release removes the finalizer instead of
rewriting `spec.deletionPolicy` to `Retain` as the map suggested, because an
API spec write would have to impersonate the file reconciler's manager or
take the object over with `Force` and would rewrite what the operator
declared, while the exact-identity metadata write is the one the cleanup
itself uses; and it has its own permission rather than reusing
`integrations.sync`, because abandoning the cleanup promise is a different
authority than publishing.

Fix found by the acceptance. A pending intent that a complete reread showed
as completed (no receipt recorded) was settled by `RecordReceipt` with
`ObservedAt` straight from `time.Now`; the SQLite command returns the written
object with the monotonic reading, the store reads it back without it, and
`Confirm` compared the two with `reflect.DeepEqual` and refused with
`ErrInvalid`. The intent then stayed pending forever (`CleanupFailed` in a
cleanup, `Degraded` in an ordinary sync). `fix(integration): record recovery
receipts in UTC` records it in UTC without a monotonic reading, as the
adapters already do (`ee/integration/host/engine.go`).

Five problems the verifier of INT-DEL-B reported (`followups/INT-DEL-B-verify.json`,
`newProblems`), each resolved here:

1. The documented history ceiling. `docs/deploy/integrations.md` and
   `docs/architecture/integrations.md` said an instance's history has no
   limit and that the open limit is the 200,000-row scan of the purge and
   the finalization. Root's capture starts with `ReadGeneration`, which
   refuses a store of more than 100,000 objects or 256 MiB of documents of
   all kinds (`replication.Limits` defaults; `ReplicationLimits` is never
   set outside the store package), so no Integration job gets a frame past
   it and the 200,000-row bound is never reached first. Both pages now name
   the store limit as what bounds a history (and the comment at
   `integrationScanLimit` says the same). This is by code reading, like the
   verifier's finding: no test fills a store to 100,000 objects (the
   verifier's probe ran out of memory while seeding).
2. Cleanup of a long history progressing only at the run bound, and
   stalling. Two causes. (a) The `invalid input` after a provider delete cut
   before its receipt is the monotonic-clock receipt that
   `fix(integration): record recovery receipts in UTC` (cce5d712) fixes: the
   next run confirms the delete by a reread and records the receipt, instead
   of `CleanupFailed` until the resync interval. (b) A `Forget` commit
   carried up to 1,024 operations whatever the store's speed, so a commit
   that cannot finish in one run's bound was cut at the same place every run
   and reported as `ProviderFailed` (the verifier's 5 s host probe never
   progressed). Now a run that its bound cuts inside a `Forget` without any
   progress sets the lease's commit size to half of what that `Forget` tried
   and runs again at once (`CleanupInProgress`), and a run that made
   progress yields before its next commit once less of its bound is left
   than one and a half times the last commit took, or an eighth of the
   bound. Provider deletes come before any history work in a run, so the
   history no longer uses up the bound before them.
   (c) Found by the review of this package: a run that its bound cut
   without progress in any other local step (a ledger count, a ledger or
   history read, a fence, an intent, receipt, confirmation or abandonment
   commit of the shared apply and recovery path, the purge or the
   finalization) was still reported as `ProviderFailed`, as the reviewer's
   second race run of the multi-page test showed
   (`Terminating/ProviderFailed` with `pending=1`, a cut intent or receipt
   commit). The lease now notes a cut local step
   (`terminatingLease.local`; `applyPlan` and `recover` take the lease, nil
   in a sync, whose behavior is unchanged), and such a run reports
   `CleanupFailed` with the ordinary temporary-failure backoff.
   `ProviderFailed` stays for a run cut inside a provider request,
   including the fence and budget charge the request itself makes.
3. No mounted test of a history longer than one stored page:
   `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` (real SQLite,
   keyset pages, 1,001 operation rows over four pages).
4. The failed count overwriting the kept counts: a run whose first ledger
   count fails is now uncounted, like a fenced run, and keeps what the last
   run reported.
5. Commit hygiene: see the note to the integrator (e6ab72b4 and 3e71816d
   must be picked together).

Docs (DEL-6, code side). `docs/deploy/integrations.md`: the summary fields,
the `409` refusals, resumption after restart, failover, license expiry and a
missing credential, and the release command with its permission and audit
row. `docs/architecture/integrations.md`: the release command in the
lifecycle state diagram and the terminating-job section; the paragraph that
said no mounted restart or promotion test existed now names them. The Go
comment of `spec.deletionPolicy` (and so the three CRD copies and the CRD
reference) states what `Retain` and `Delete` do and that the policy has no
effect on Kubernetes yet. `grep -r "refused before provider work" docs
docs-site/content` returns nothing.

Evidence under `.context/v020-final-20261110/evidence/INT-DEL-C/`:

- Review fixes (bd7a5658, 99862457, docs 643c7350 and 0fac45e0), all
  under race. New host tests `TestTerminatingRunCutInALocalReadIsNoProviderFailure`
  (stalls the operation reads after the provider delete; 2.1 s) and
  `TestTerminatingRunCutInALocalCommitIsNoProviderFailure` (stalls the
  intent write before any provider delete; 1.1 s), `fix-host04-local-commit`.
  Each fails without its change: `fix-negative02-local-cut-before`
  (before bd7a5658: times out in `Terminating/ProviderFailed`, 10.1 s) and
  `fix-negative03-local-commit-unnoted` (overlay with the noting in
  `applyPlan` and `recover` disabled, worktree unchanged: times out in
  `Terminating/ProviderFailed`, 10.1 s). `fix-negative01-local-cut-before`
  is a retained first draft of the read test that passed before the fix:
  it stalled the first ledger count, which a lease reaches before it has
  inspected its provider, so it never took the publisher branch; the test
  was moved to the operation read. `fix-host05-package`: `ee/integration/host`
  54.8 s and `ee/integration/ledger` 6.5 s, ok. Scoped lint `fix-lint03`:
  0 issues over `./ee/integration/host/...` (`fix-lint02` only met another
  lane's parallel golangci-lint and did not run). Mounted:
  `fix-mounted01-selection` (after bd7a5658 only; 13 of 13 passed, 207.7 s,
  but the multi-page test still showed `Terminating/ProviderFailed` with
  `pending=1`, which led to 99862457) and `fix-mounted02-selection-x2` on
  the final tree (`-race -count=2 -run 'TestIntegrationDelete|TestIntegrationAdminTerminatingStateAndReviewedRelease'`):
  26 of 26 passed, 0 skipped, 387.1 s;
  `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` 94.0 s and
  87.3 s, with the status sequences Election, CleanupInProgress,
  AuthorityChanged and Election, CleanupInProgress, and no
  `ProviderFailed`. The only `ProviderFailed` lines in that log are the
  `unreadable` and `ambiguous` subtests of
  `TestIntegrationDeleteResumesAfterRestart`, whose peer answers with
  errors on purpose. No generated artifact changed.

- Final mounted selection under race through the lock on the committed
  tree (`mounted13-selection-committed`, same `-run` as below plus the
  drift tests): 34 top-level tests, 34 passed, 0 skipped, 326.2 s, including
  `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` (104.3 s),
  `TestIntegrationAdminTerminatingStateAndReviewedRelease` (5.8 s),
  `TestIntegrationDeleteResumesAfterRestart` (16.7 s),
  `TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes` (7.5 s),
  `TestIntegrationDeleteLicenseExpiryPausesCleanup` (18.3 s),
  `TestIntegrationDeleteRetainLeavesProviderRecordsAndLedger` (3.9 s),
  `TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes`
  (5.4 s), `TestIntegrationDeleteCloudflareCleanupRemovesOnlyOwnedRecordsAndFinalizes`
  (4.9 s, foreign and unowned rows byte-identical) and
  `TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes` (10.2 s,
  release by declaring `Retain`). (The log's last line says `exit=0` from
  the grep of the pipeline; the `ok ... 326.162s` line is the test result.)
- Long-history mounted test: `mounted10-history-pages` (retained, failed
  on the test's own page count, off by one, before any cleanup),
  `mounted11-history-pages` (retained, failed at the fixture's 15 s await
  before any provider delete: under race each root capture of a store this
  size is slow), `mounted12-history-pages` (passed, 88.1 s, three runs of
  one lease, finalized 33 s after the delete request), then `mounted13`.
- Host (`host01-new-tests`, `host02-package`): `ee/integration/host` under
  race 52.7 s and `ee/integration/ledger` 5.7 s, exit 0;
  `TestTerminatingForgetCommitsShrinkToTheRunBound` 17.7 s.
  Negative controls by build overlay, worktree unchanged:
  `negative02-forget-no-shrink` (no halving: times out after 91.7 s in
  `Terminating/ProviderFailed` with `Remaining:1`, the verifier's symptom)
  and `negative03-uncounted-ledger-failure` (a failed count reports
  `Remaining:0, Conflicts:0` after `Remaining:1, Conflicts:1`).
- Scoped lint through the lock (`lint02`): 0 issues over `./cmd/kudurru/...`
  and `./ee/integration/host/...`. Drift on the final tree: `drift02-committed`.
- Earlier mounted selection under race through the lock
  (`mounted07-selection`, `-run '^(TestIntegrationDelete|TestIntegrationAdmin|TestIntegrationCLI|TestIntegrationRuntimeDelete|TestIntegrationGuard|TestIntegrationRoot|TestIntegrationForget|TestIntegrationRuntimeActual|TestSQLiteObjectSourceIntegrationCleanupFinalizer|TestCLIReferenceDrift)'`):
  34 top-level tests, 33 passed and one failed, 225.2 s. The failure was
  the first version of `TestIntegrationAdminTerminatingStateAndReviewedRelease`,
  which read the summary before the job's own status write had been
  published (its revision lagged the stored object); the test now waits until
  the summary names the stored revision, and `mounted08-admin-x3` passed it
  3 of 3 (exit 0, 21.1 s). Every other test of the selection passed in
  `mounted07`, including all INT-DEL-B mounted tests unchanged and:
  `TestIntegrationDeleteRetainLeavesProviderRecordsAndLedger` (4.5 s),
  `TestIntegrationDeleteResumesAfterRestart` with subtests `unreadable` and
  `ambiguous` (19.7 s), `TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes`
  (9.2 s), `TestIntegrationDeleteLicenseExpiryPausesCleanup` with subtests
  `renewed` and `released` (18.9 s), and `TestIntegrationAdminCLIActualRouter`.
- What the new tests prove. Restart: the whole serving runtime is closed,
  the SQLite file closed and reopened, and a new runtime started; while the
  record list was unreadable nothing was deleted, and after a batch applied
  but answered 503 the intent stayed pending and was not resent; after the
  restart exactly one delete batch in total, each published provider ID
  deleted once, no ledger or operation row, budget row kept. Failover: the
  replica is installed from the image taken while the delete batch is held
  at the peer and then promoted; the held batch completes at the peer, the
  old store still has the pending intent, the finalizer and no
  `integration.cleanup`; the promoted writer finishes with no second batch
  and writes its `integration.cleanup` summary. License expiry: the license
  clock passes expiry plus grace while the batch is held; the cleanup is
  `Terminating/LicenseUnavailable`, the peer's request counter does not move
  across a wake and 1.3 s, the object keeps the finalizer; a renewed license
  finishes with exactly one batch, and the reviewed release without a license
  removes the object with no request and keeps the ledger row. Retain
  control: the object is gone in the delete request, no request reaches the
  peer, both published rows and the ledger row stay, and no cleanup or
  release audit row is written. API/CLI: see above; the release keeps both
  published rows at the peer and the ledger row, and the audit trail has
  `integration.release` and no `integration.cleanup`.
- Retained failed runs: `mounted01-first-run` (exit 1, 83.7 s: the failover
  test failed with `CleanupFailed`, and the admin test failed on a live-object
  `statusKnown` assertion that was wrong, see "Known gaps"; the restart,
  license and Retain tests passed). `mounted02-diagnostic-overlay` to
  `mounted06-diagnostic-overlay` are diagnostic runs by build overlay
  (worktree unchanged) that added the error to the host's log line and then
  located it: `integration ledger: invalid input at repository.go:162`, the
  `reflect.DeepEqual` of `currentOperation` inside `Confirm`, called from
  `recover` at `engine.go:687`; the two receipts differed only in the internal
  representation of `ObservedAt` (equal by `Time.Equal`, both UTC). That led
  to the fix commit.
- Negative controls by build overlay with the worktree unchanged:
  `negative04-release-admits-live-object` (the release check without the
  deletion timestamp and finalizer clauses) and
  `negative05-recovery-receipt-monotonic` (cce5d712 reverted) each fail
  their target test: `TestIntegrationAdminTerminatingStateAndReviewedRelease`
  fails with "a live object was released 200" (4.0 s), and
  `TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes` stays
  `Terminating/CleanupFailed` with `Pending:1` on the promoted writer
  (22.0 s).
  (`negative01-release-admits-live-object` and `mounted09-selection-committed`
  are empty: the earlier run of this package was cut off by the
  machine restart before they wrote anything.)
- Libraries under race (`race01-libraries`): `ee/integration/host` 40.8 s,
  `internal/api` 264.6 s, `internal/apiclient` 1.8 s, `internal/rbac` 1.0 s,
  exit 0. `go vet ./cmd/kudurru ./internal/api` clean (`vet01`). Scoped lint
  through the lock (`lint01`): 0 issues over `./cmd/kudurru/...`,
  `./internal/api/...` and `./ee/integration/host/...`. `gofmt -l` clean.
- Generated (`gen01`, `gen02-docs`): `make generate`, `make client-go`,
  `bun run gen:api`, `make docs-gen`; `make rbac-manifests` and
  `make deploy-assets` changed nothing. Drift on the committed tree
  (`drift01-committed`): `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestReferenceDrift`, `TestAggregatedRolesDrift`, `TestCLIReferenceDrift`.

What remains open. The Kubernetes half (DEL-8): no Integration executes on
Kubernetes, so `Delete` has no effect there, nothing arms the finalizer, and
the release route is not mounted (the integration administration exists only
on SQLite). The tracker rows (completion-plan.md:94 and :185, gaps.md,
followup-audit.md:82/587), the decision log, the changelog and the stale
sample at dev-docs/design-record.md:1062 (it nests `deletionPolicy` under
`spec.cloudflare`; the type has it at `spec.deletionPolicy`) are the
integrator's to edit. Not covered by a mounted test: an ambiguous batch
resolved by the same writer without a restart (host-level only), replica
promotion of a UniFi or webhook instance's cleanup, the SDK-level bound of 200
effects per batch and one budget charge per HTTP attempt (adapter tests
only), a credential rotated between a partial delete and its recovery
(host-level only), and the release racing an in-flight batch (the outcome is
documented, not tested). Known gap found and not changed: the summary of a
live instance shows `statusKnown: false` after the instance's own status
write, because the host's status keeps the revision it captured; only a
terminating instance's report follows the object's revision (INT-DEL-B), so
the dashboard lane should not treat `statusKnown: false` on a live object as
an error. The known limits INT-DEL-B listed (the empty inventory source entry, the
rows `Retain` leaves, the per-lease concurrency slot) are unchanged; the
200,000-row scan bound it listed is replaced by the store limit above (100,000
objects or 256 MiB of all kinds), which is not raised here and stops every
Integration job, not only the cleanup. A long history still costs one run
bound per few commits on a slow store (the 26,000-operation SQLite probe of
the verifier took 6 min 51 s); this package makes such a cleanup converge,
not faster, and the 26,000-operation case was not rerun after the change.
The halved commit size never grows again within the lease (a new lease
starts at 1,024). A slow store can still show a transient `CleanupFailed`
with backoff before such a cleanup converges, and a run cut inside a provider
request by a slow local fence or budget charge that the request makes still
shows `ProviderFailed`.
