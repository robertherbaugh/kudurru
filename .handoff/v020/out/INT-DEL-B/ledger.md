### F09b/F60c Integration `Delete` lifecycle, part 2 of 3: elected terminating job and SQLite cleanup — November 10, 2026 (D367)

This package lands DEL-3 (the host's terminating job) and DEL-4 (the SQLite
root: mode-aware guards, atomic local purge, finalization, release, and removal
of the refusal). An Integration with `deletionPolicy: Delete` now runs on
SQLite, and a delete request is followed by a cleanup instead of leaving the
object terminating forever. The refusal in `integrationRuntime.current` is gone
in the same commit that mounts the cleanup, so `Delete` never behaves as a
silent `Retain` (D239). The D367 row is restated in full in
`out/INT-DEL-B/decision.md`: the protocol is the one INT-DEL-A recorded, with
the points this package had to decide spelled out (see "Decided here" below).

Host (DEL-3, `ee/integration/host`). Every job holds one mode,
`ExecutionGuard.Mode`: normal, `cleanup` or `release`. `Host.disposition`
derives it from the captured object (`InstanceMode`): a live object runs the
ordinary lease; an object that is being deleted and carries the reserved
finalizer runs the cleanup job when its policy is `Delete` and the release job
under any other policy; one that is being deleted without the reserved
finalizer stays `Paused/Deleting` as before. A change of mode replaces the job
like any other change of authority, so the ordinary job is canceled and joined
before the terminating job is elected on the same per-instance key, and a
recreated object of the same name cannot start until the terminating job has
returned from its last root callback. The terminating lease
(`runTerminating`) repeats bounded runs, each under one operation timeout, on a
freshly captured frame, and takes a concurrency slot only while a run executes.
One cleanup run of a publisher provisions and checks the provider with the
captured named credentials (never `Start`), rereads every configured
destination completely, resolves pending intents by the existing recovery
rules, builds cleanup plans (`plan.Input.Cleanup`) and executes their deletes
through the same `applyPlan` an ordinary sync uses (durable intent, fence,
budgeted request, receipt, confirm), rereads, and forgets each ledger row whose
record is absent (`ledger.Repository.Forget`): a confirmed delete, or a fresh
complete snapshot that holds none of the baseline's provider IDs and no marked
row at the key. When no record, ledger row, operation or uncertain intent of
the incarnation is left it calls the root's `Purge` and `Finalize`. A provider
that does not publish (UniFi, webhook) owns nothing remotely: its cleanup
builds no provider, needs no credential and makes no provider call. Recovery
has one relaxation, in the cleanup mode only: a partially applied delete is
resumed although the credential or configuration digest changed since it was
recorded. Operator commands (`CaptureCommand`, check, preview, sync, invoke)
are refused for a terminating job with `ErrFenced`. The local status has phase
`Terminating`, a reason and counts (`Result.Remaining`, `Conflicts`,
`Pending`), follows the object's resource version across the job's own status
writes, and is published through the existing `Status` callback.

Blocking and pausing. Nothing is finalized while any of these holds, and each
has its own reason: `RecordsConflict` (a conflict at the name of an owned
record set, or a record set shared with another owner's rows),
`OwnershipUnproven` (a marker without ledger evidence, or ledger provider IDs
that still exist without the marker), `PendingRecovery`, `DestinationRemoved`
(ledger rows of a destination the spec no longer names, or of an instance whose
provider does not publish), `SnapshotIncomplete`, `LocalStateRemains`. Proven
record sets are still deleted and forgotten while others block.
`LicenseUnavailable` and `Suspended` start no job; `CredentialsUnavailable`
ends a publisher's lease before any provider call. `BudgetExhausted`,
`RateLimited`, `ProviderFailed` and `ProvisionFailed` retry with the existing
classified backoff; a blocked cleanup is re-evaluated at the resync interval,
at most every ten minutes, and at once when the object changes.

SQLite root (DEL-4, `cmd/kudurru`). `integrationGuardAdmits` is the one rule
for the captured cut (`current`), the pre-effect fence (`fence`) and every
transaction (`integrationInputsCurrent`): the Integration must be what the
guard's mode claims. Normal mode needs a live, unsuspended object, and one
whose policy is `Delete` must carry the finalizer, so an unarmed instance gets
no intent, budget charge or source commit (the host reports it as
`Paused/CleanupFinalizerMissing`); cleanup needs a deletion timestamp, the
finalizer, policy `Delete` and no suspension; release needs a deletion
timestamp and the finalizer under another policy and is the only mode that
runs without a license that allows integrations. License, key, writer-token
and publication pins are unchanged. A terminating instance selects no sources:
it does not make the capture prepare the primary image, and source, target and
group changes neither wake nor fence it. `integrationRuntime.purge` removes in
ONE command the record sets and zones the incarnation's source owns (each must
still be the UID and resource version of the captured cut), its source head and
chunks, its name allocations and its deliveries, withdraws its clients from the
global inventory, writes an `integration.purge` audit row, and then waits until
the published bundle shows none of it. Every delete names the exact UID and
resource version read in that transaction. A source object that carries
someone else's finalizer is only marked for deletion and reported
(`ErrRemains`). `integrationRuntime.finalize` verifies in ONE command that no
ledger row, operation, source head or chunk, name allocation, delivery or
source-owned object of the incarnation is left, removes the finalizer with
`CommandTx.UpdateFinalizer` by the guard's exact UID and resource version, and
writes an `integration.cleanup` audit row with what the finalizing lease
deleted, forgot and purged. Under the release mode it removes the finalizer
without any check or cleanup and records `integration.release` with what stays
behind. `stageIntegrationStatus` writes `Ready=False` with the terminating
reason and, in `status.records`, the remaining, blocked and uncertain counts.
The shared request budget row is charged like any other request and never
removed.

Decided here (all within D367; the row now says so):

- A missing credential pauses only a publisher's cleanup. Requiring a
  credential and a reachable controller to purge the local state of an inbound
  instance would trap exactly the operator who removed the controller.
- Only a partially applied delete is resumed across changed authority. Any
  other partially applied intent stays pending in cleanup (nothing is desired
  any more, so finishing a create or update is not authorized) and blocks with
  `PendingRecovery` until the operator releases the object. The Cloudflare
  adapter sends a record set's effects in one batch, so this needs a record
  set of more than 200 rows or a provider without atomic batches.
- Ledger evidence without the marker blocks, like the marker without ledger
  evidence: a record whose provider IDs still exist but no longer carry the
  marker was taken over by someone, and its ledger row is not forgotten.
- Proven record sets are deleted while others block. "Never finish partially"
  means never finalize; waiting for every record set to be provable would not
  make a release all-or-nothing either, because a conflict can appear between
  two runs.
- The terminating job holds a concurrency slot per run, not per lease, so a
  blocked cleanup cannot starve the four default slots.

Evidence under `.context/v020-final-20261110/evidence/INT-DEL-B/`:

- Host library under race, committed tree (`host07`, whole package: exit 0,
  84 passes, no skips, 12.5 s; the new tests five and ten more times in
  `host05` and `host06`: exit 0, 21.9 s and 43.3 s).
  New, over the real `Host.Run` loop, the real ledger repository on the memory
  store, and a provider whose every remote call goes through the host's
  request adapter (budget, fence, lease):
  `TestTerminatingDeleteElectsDeletesForgetsAndFinalizesOnce` (the ordinary
  provider is stopped before the cleanup provider is provisioned; one delete
  of the proven record set; a previous incarnation's row, a hand-made row and
  an external-dns row unchanged; no ledger or operation row left; budget row
  kept; one purge, one finalization under the cleanup mode),
  `TestTerminatingRetainIsReleasedWithoutProviderOrLicense` (unlicensed,
  suspended, credential unresolvable: one finalization under the release
  mode, no factory call, no request, no purge, no budget row),
  `TestTerminatingPausedReasonsMakeNoProviderCall` (six subtests: `license`,
  `suspended`, `publisher credential` with the durable status, `observer
  needs no credential`, `live object without the finalizer`, `another
  finalizer only`),
  `TestTerminatingLeaseLossBetweenApplyAndReceiptResolvesByReread` (the lease
  ends inside the provider's delete; the fenced lease records no receipt and
  leaves the ledger pending; the next lease confirms the absence with zero
  further provider mutations and zero adapter recoveries),
  `TestTerminatingBlockingConditionsNeverFinalize` (seven subtests: conflict
  at a name with owned rows, incomplete snapshot, marker without ledger
  evidence, ledger evidence without marker, record removed at the provider by
  someone else, destination removed from the spec, provider answer lost
  without a lease change; each checks the local and the published reason and
  counts, that the report follows the object's resource version, and that
  cleanup finishes once the cause is gone),
  `TestTerminatingBudgetExhaustionMakesNoRequestAndResumes` (the request
  counter does not move while the window is full; the next window finishes
  the cleanup; the budget row keeps its UID),
  `TestTerminatingReplacementWaitsForCleanupJoin` (a recreated UID of the
  same name builds no provider while the old job is inside a root callback),
  `TestTerminatingCleanupContinuesAcrossBoundedRuns` (four record sets under
  a 750 ms run bound with every second delete slower than the rest of its
  run: one lease, four runs, each record set deleted exactly once),
  `TestTerminatingBlockedJobHoldsNoConcurrencySlot`,
  `TestTerminatingJobRefusesCommands`, `TestInstanceModeFollowsTheStoredObject`
  and `TestCleanupRecoveryResumesOnlyDeletesAcrossChangedAuthority` (a
  partially applied delete is resumed in the cleanup mode after the config
  and credential digests changed, is left pending by an ordinary sync and by
  a cleanup input under an ordinary guard; a partially applied create stays
  pending in cleanup).
- Negative controls of the host tests, by build overlay with the worktree
  unchanged (`negative01` to `negative05`, each fails exactly its target):
  no relaxation in recovery, and the relaxation not bound to the cleanup
  guard (`TestCleanupRecoveryResumesOnlyDeletesAcrossChangedAuthority/partial_delete`);
  a ledger row forgotten although its provider IDs still exist
  (`.../ledger_evidence_without_marker`); a license-gated release
  (`TestTerminatingRetainIsReleasedWithoutProviderOrLicense`); a terminating
  job that accepts a command capture (`TestTerminatingJobRefusesCommands`).
- Mounted SQLite runtime under race, through the lock wrapper (`mounted02`,
  selection
  `^(TestIntegration|TestSQLiteObjectSource|TestRunServeSQLiteSourceBinding|TestRunServeUniFiHostname|TestApply|TestDelete$|TestCLIReferenceDrift)`:
  exit 0, 50 passes, no skips, 372.0 s; no Go file changed between that run
  and the commits). New, on the real SQLite writer, publisher, host, ledger
  and budget:
  `TestIntegrationDeleteCloudflareCleanupRemovesOnlyOwnedRecordsAndFinalizes`
  (the registered Cloudflare adapter and SDK against the verified-TLS peer of
  its canonical origin, on an `upsert-only` zone: the object is stored armed;
  with the peer held, the delete request leaves a terminating object with its
  spec and credential reference, the host reports `Terminating` and refuses a
  sync; then exactly one batch whose delete effects name exactly the two
  published provider IDs; a foreign-marker row, a hand-made row and an
  external-dns record with its heritage TXT are byte-identical; no ledger or
  operation row; budget row kept; the operator's own record set untouched;
  the audit trail ends with `integration.finalize` and `integration.cleanup`
  ("deleted 1 provider record sets", "forgot 1 ledger rows and 2 operations");
  no credential bytes in the image; the name is free for a new armed
  incarnation), `TestIntegrationDeleteUniFiPurgesLocalStateAndFinalizes` (the
  UniFi adapter against the HTTPS controller double: no controller request
  during cleanup; the source-owned record set, source head, chunks and name
  allocation are gone, the inventory has no client of the source, the zone
  stops answering the generated name; the operator's record, its answer and
  the group's hand-added member stay; the purge's deletes and its
  `integration.purge` row are one unbroken run of the audit log),
  `TestIntegrationDeleteHeldSourceObjectKeepsTheInstanceTerminating`
  (`LocalStateRemains` while a generated record set carries another
  finalizer; finishes after that finalizer is gone and the object is declared
  again), `TestIntegrationDeleteReleaseNeedsNoLicenseCredentialOrProvider`
  (unlicensed runtime, unresolvable credential, terminating `Retain` object
  with the finalizer: gone with no provider built, the ledger row kept, no
  budget row, an `integration.release` audit row and no `integration.cleanup`),
  `TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes` (two subtests:
  `resolved` and `released`; `RecordsConflict` in the host status at the
  object's current revision and in the `Ready` condition, `status.records`
  1 remaining and 1 blocked, no delete effect; then either the cleanup
  finishes after the conflicting row is gone, or declaring `Retain` removes
  the object with no provider write and the ledger kept),
  `TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes` (the
  peer's request counter does not move while the credential file is missing;
  restoring it finishes the cleanup),
  `TestIntegrationGuardAdmitsOnlyTheObjectItsModeClaims` (nine object states
  under each of the three modes) and the rewritten
  `TestIntegrationRuntimeDeleteRunsArmedAndRefusesUnarmedOrUnlicensed` (three
  subtests: an armed `Delete` instance commits its source; one whose finalizer
  was removed is `Paused/CleanupFinalizerMissing` with zero polls, no source
  and no budget row until an apply arms it; unlicensed is unchanged). It
  replaces `TestIntegrationRuntimeRefusesDeleteAndUnlicensedExecution`, which
  asserted the refusal. The unchanged
  `TestIntegrationRuntimeActualLedgerForgetIsFencedAndAtomic` and
  `TestSQLiteObjectSourceIntegrationCleanupFinalizer` pass in the same run.
- Negative control of the mounted tests (`negative06`, build overlay that puts
  the old refusal back into `integrationRuntime.current`): the Cloudflare and
  UniFi lifecycle tests and the `delete` and `unarmed` subtests fail; the
  release test and the `unlicensed` subtest pass, as they do not depend on a
  `Delete` instance running.
- Other integration libraries under race, committed tree (`race06`):
  `ee/integration/cloudflare` 9.8 s, `unifi` 6.9 s, `webhook` 2.9 s, `plan`
  1.2 s, `ledger` 3.6 s, `inbound` 14.2 s, all exit 0.
- Drift, committed tree (`drift02`): `TestOpenAPIDrift`, `TestReferenceDrift`
  and `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in
  `mounted02`. No generated artifact changed, so there is no generated commit.
- Hygiene: `gofmt`, `go vet ./ee/integration/...` and `CGO_ENABLED=0 go build
  ./...` clean (`fmt-vet-build01`). Scoped lint through the lock wrapper
  (`lint01`, the same Go sources as the commits): zero issues over
  `./cmd/kudurru/...` and `./ee/integration/host/...`. No dependency was
  added.

Retained failed runs: `host01` (the first run of the new host tests; only
`TestTerminatingDeleteElectsDeletesForgetsAndFinalizesOnce` failed, in its
setup: the ordinary sync reports the three seeded rows of other owners as
conflicts, so the instance is `Degraded/ConflictsOrPending`, not `Ready`, and
the helper now waits for the reported publication instead). `mounted01` (the
first mounted run, seven of the eight selected tests passed;
`TestIntegrationDeleteUniFiPurgesLocalStateAndFinalizes` failed at its
inventory assertion, because the test read the published bundle in the instant
after finalization, when `Publisher.CaptureHTTP` refuses a capture until the
publisher has caught up with that last commit; the assertion now waits for the
capture). Neither changed product code. `negative01` to `negative06` are the
negative controls above and were expected to fail.

Review fixes (accept-with-fixes review; four source commits on top of the
implementation: `fix(integration): bound cleanup history reads and report
counts`, `test(integration): prove root's mode rule and purge refusals`,
`docs(integrations): document cleanup limits and every reason`, `test(integration):
wait for the recorded finalization`):

- Root's mode rule now has tests that fail without it.
  `TestIntegrationRootRefusesAModeTheStoredObjectDoesNotAllow` calls
  `current`, the stored-object fence (`storedCurrent`, split out of `fence`),
  `integrationInputsCurrent` inside a real SQLite command, and the full
  `integrationRuntime.command`, for eight stored states (retained, armed,
  unarmed, suspended, cleanup, frozen, release, held by another finalizer)
  under each of the three modes. Each guard names the object's exact UID and
  resource version, so only the mode can refuse it; exactly four guards are
  admitted, and a refused one leaves no ledger, operation, budget or source
  head row. `TestIntegrationRuntimeRootRefusesAJobTheStoredObjectDoesNotAllow`
  (two subtests) mounts a host whose capture misreports the object (an
  unarmed `Delete` object shown armed; a live object shown deleting): root
  alone gives the job no provision, poll, source, budget charge, purge or
  finalization.
- Cleanup no longer loads the whole operation history. Ledger rows are read
  per configured destination, as a sync reads them, and only counted
  owner-wide (`ledgerCounts`, 100,000-row bound); a ledger row is forgotten
  with one bounded batch of its operations (`operationBatch`: 4,096
  operations, 8 MiB); the rest of the history is forgotten a stored page at a
  time (`operationPages`, no row bound, ends with the run's context) once no
  ledger row is left, repeated until a complete read finds none. A bounded
  read that meets more than it admits reports the new reason
  `CapacityExceeded` instead of `CleanupFailed`.
- `Result.Remaining` is counted when a run starts and carried on every early
  return (budget, incomplete snapshot, provider failure, missing credential,
  provider unavailable); a run fenced before it could count keeps the last
  run's counts.
- A release job still runs without a license, but again only while root's
  serving lifetime lasts (new `Options.Alive`): the earlier license bypass had
  also dropped that part of `Allowed`.
- New mounted `TestIntegrationDeletePurgeFencesAChangedObjectAndFinalizeRefusesLeftovers`:
  a generated record set changed after the cut fences the purge as one
  command (nothing removed); the next purge removes it with a queued
  `IntegrationDelivery` (Deliveries 1); a delivery stored between purge and
  finalization makes `finalize` refuse with `ErrRemains` while the finalizer
  stays; a later purge removes it and finalization succeeds; two
  `integration.purge` audit rows. `TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes`
  now asserts that root's capture selects the publisher's record set while it
  lives and nothing once it terminates (`needsSources`).
  `TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes` now
  asserts Remaining 1 and `status.records.managed` 1 on the credential stop.
- The UniFi purge test reads both names from one successfully captured
  bundle in one wait, so a refused capture can neither satisfy the negative
  wait nor fail the positive check.
- Docs: the operator guide's reason table adds `CapacityExceeded`,
  `CleanupFailed`, `ProviderUnavailable` and `ReleaseFailed`, explains the
  counts, says `LicenseUnavailable` and `Suspended` are host-status only, and
  lists the bounds; restart and failover resumption is worded as the design.

Evidence of the fixes (same directory, `fix-*`):

- Host package under race, whole package (`fix-host04-package-v`): exit 0,
  87 passes, no skips, 53.0 s. New: `TestTerminatingLongOperationHistoryIsForgottenInBoundedSteps`,
  `TestOperationReadsAreBoundedWhateverTheHistory` (a history of 100,179
  rows and 33.7 MB is read and batched), `TestTerminatingCapacityIsReportedAndRetried`;
  remaining-count assertions added to the blocked, budget and slot tests.
- Host negative controls by build overlay (`fix-negative-h1` to `h7`, each
  exit 1 on its target): remaining not counted at run start; a fenced run
  reporting zero; the operation read keeping the 100,000-row bound; an
  unbounded batch; capacity not named; leftovers in one pass; a release that
  ignores the serving lifetime.
- Mounted selection under race through the lock
  (`fix-mounted02-selection`, `^(TestIntegrationDelete|TestIntegrationRuntime|TestIntegrationForget|TestIntegrationAdmin|TestIntegrationOperation|TestIntegrationRoot|TestIntegrationGuard)`):
  exit 0, 31 top-level passes, no skips, 219.4 s.
- Mounted negative controls by build overlay, worktree unchanged, each exit 1:
  `fix-negative-m1-root-rule-always-admits` (`integrationGuardAdmits`
  returns true: both new root tests fail, every site and both subtests);
  `fix-negative-m2-purge-no-cut-check` and
  `fix-negative-m3-finalize-no-remains-check` (the purge/finalize test
  fails); `fix-negative-m4-terminating-publisher-needs-sources` (the blocked
  test fails at the "selects nothing once terminating" wait).
- Repetition, `-count=20` under race of the UniFi purge test and the new
  purge/finalize test: `fix-mounted03-unifi-and-purge-x20` failed once (exit
  1, 369.7 s): the purge/finalize test read its record of `Finalize` calls
  after the object was gone, which is before the wrapper records the
  successful call, so it saw only the `ErrRemains` refusal. A fourth commit,
  `test(integration): wait for the recorded finalization`, waits for that
  record; `fix-mounted04-unifi-and-purge-x20` then passed 20 of 20 for each
  test (exit 0, 365.1 s). The UniFi test did not fail in 40 runs after its
  fix.
- Scoped lint through the lock (`fix-lint01`): zero issues over
  `./cmd/kudurru/...` and `./ee/integration/host/...`. `gofmt -l` clean,
  `go build ./...` and `go vet ./ee/integration/host/` clean
  (`fix-fmt-vet-build01`). No generated artifact changed; no dependency
  added.
- Retained failed runs: `fix-host01-first-run` (written by an earlier fixer
  run that was cut off; the first version of the long-history test timed out
  at `ProviderFailed` with two rows left, after 67.5 s; the uncommitted code
  was changed before `fix-host02`, where the whole new set passed, and the
  exact change cannot be reconstructed from what was retained).
  `fix-mounted01-first-run` passed (exit 0).

Open after this package: the API and CLI surface (DEL-5: `deletionPolicy`,
`terminating` and `deletionRequestedAt` in the summary, the reviewed `release`
command and its permission, CLI columns), the CRD field description, tracker
edits and the remaining operator-guide detail (DEL-6), the rest of the mounted
acceptance list (DEL-7), and the Kubernetes half (DEL-8). Not shown by a mounted
test in this package, only by the host-level tests over the memory ledger:
lease loss and takeover in the middle of a cleanup (DEL-7 cases 4 to 6: restart
with an unreadable list, an ambiguous batch, replica promotion while a batch is
held), license expiry after the delete request (case 7; the host tests cover
a cleanup that starts unlicensed and resumes, not an expiry in the middle of a
run), and the SDK-level bounds (at most 200 effects per batch, one budget
charge per HTTP attempt).
A run that cannot settle a pending intent stops
there, before it deletes anything else of that instance.
The summary in the `integration.cleanup` audit row counts only what the
finalizing lease did itself; earlier leases and runs are in their own audit
rows. Known limit left open by the fixes: the root's purge and finalization scan
every row of the kinds they check in one transaction, bounded at 200,000 rows
of a kind store-wide (`integrationScanLimit`). The private rows carry no owner
label or prefix to select by, so the operations of every other Integration
count; a store holding more than 200,000 operations cannot finalize a
`Delete` cleanup (`CapacityExceeded` after the provider records are gone; the
operator must release). Lifting it needs an owner-indexed read of the private
kinds. Known limits that this package did not change: a purged source keeps an
empty entry in the inventory state (the inventory has no source removal, and
its removal watermarks reference the source), which counts toward the 128
inventory sources; `Retain` still leaves ledger rows, operations, allocations
and source rows behind; an ordinary lease still holds one of the four default
concurrency slots for its whole lifetime, so a fifth instance, terminating or
not, waits for a slot. On Kubernetes no Integration executes, so `Delete` has
no effect there and the host never arms the finalizer.
