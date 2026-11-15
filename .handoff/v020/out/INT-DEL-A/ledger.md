### F09b/F60c Integration `Delete` lifecycle, part 1 of 3: reserved finalizer, cleanup plan and ledger forget — November 10, 2026 (D367)

D367 records the whole `deletionPolicy: Delete` protocol (map items DEL-1 to
DEL-7, with the Kubernetes half DEL-8 as a later lane). This package lands DEL-1
and DEL-2, the parts the terminating job is built on. Running a `Delete` instance
is still refused by the SQLite root adapter (`integrationRuntime.current`), on
purpose: the refusal is removed only together with the cleanup job, so `Delete`
never behaves as a silent `Retain` (D239).

Store (DEL-1). The reserved finalizer `kudurru.io/integration-cleanup` and its
pure rule live in `internal/store/store.go`. SQLite admission applies the rule
inside `planApply`, after validation and defaulting, so `Store.Apply`,
`CommandTx.Apply` and restore arm an Integration whose policy is `Delete` in the
same write as its spec. No caller's document can set or strip the finalizer: a
same-manager apply without finalizers writes nothing, an edited document and a
`Force` takeover keep it, and an apply during termination neither finalizes nor
revives the object. Changing the policy to `Retain` disarms a live object and
releases a terminating one. `Retain` is the default, so a document that omits
`deletionPolicy` does the same; the audit row of a write that drops the
finalizer now says why (`disarmed: deletionPolicy is Retain; dropped
kudurru.io/integration-cleanup` on a live object, `released without cleanup:
...` on a terminating one, appended to a `Force` takeover's reason), so a
release is never mistaken for a finished cleanup. The Kubernetes driver has no
admission and never arms the finalizer itself (nothing would clear it while no
host runs there); it carries a finalizer the host armed through server-side
apply and through the `Force` takeover, which otherwise removes every field the
new manager omits. The new optional driver interface `store.FinalizerWriter`
changes one finalizer on the exact UID and resourceVersion the caller observed,
keeps manager, spec, status and generation, and removes a terminating object
when its last finalizer goes: `Store.UpdateFinalizer` and the transaction-bound
`CommandTx.UpdateFinalizer` on SQLite, a merge patch under the field manager
`kudurru-finalizer` on Kubernetes. That patch replaces the whole finalizer list,
so it carries the observed resourceVersion as the apiserver's optimistic lock: a
change between the driver's read and its write is a conflict that is not
retried, and a finalizer another actor added in between survives.

File reconciler. On SQLite an Integration is file-declared, so removing its
document is the delete request: an armed object now stays visible with a
deletion timestamp, re-adding the document with `Delete` does not finalize it,
and declaring `Retain` (or omitting the line) releases it, which the reconciler
logs as `objects: finalized` instead of an ordinary update. The delete is
requested and counted once: later passes leave an object that already has a
deletion timestamp alone, where they used to report, count and log a deletion on
every poll and file event. Admission acts on writes only, so a live `Delete`
Integration stored without the finalizer (a database written before admission
armed it, or a live `UpdateFinalizer` removal) used to stay unarmed behind its
unchanged document; the reconciler now applies that document
(`store.ReservedFinalizerUnarmed`), which arms the object on SQLite and changes
nothing else. On Kubernetes the same apply is a no-op at the apiserver, is
reported as unchanged and is made once per agreement, not on every pass.

Planner and ledger (DEL-2). `plan.Input.Cleanup` builds a deletes-only plan. It
refuses desired records and per-record policies, forces `sync` over the
destination's and the ledger's retained `upsert-only`/`create-only` policy, and
deletes a record set only when every row carries this cluster and UID marker (or
the ledger-proven legacy marker), every provider ID is in the confirmed ledger
baseline of that record set, and its name has no conflict. Every record set of
the owner that it leaves in place is listed in `Plan.Retained` with a reason
(conflict at the name, record set shared with rows of another owner, marker
without ledger evidence). The flag is part of the plan digest, and ordinary plans
keep the digests and operation IDs they had (pinned against values recorded
before the change). `ledger.Repository.Forget` removes one record's ledger row
and its operations by exact UID and resourceVersion, row first, through a new
guarded `Delete` callback. It refuses a pending row, a partially applied
operation, a stale identity, and a baseline that still lists provider content
unless the host supplies a fresh complete observation of absence. That last
case goes beyond the map, which allowed only a confirmed-absent baseline: a
record that someone removed at the provider is never confirmed by a delete
receipt, and its row would otherwise block cleanup forever. The SQLite root
adapter wires the callback (`integrationRuntime.forget`) as one fenced command
under the same lease, configuration and writer checks as every ledger write; it
can name only ledger and operation rows, so the shared request budget is out of
reach. The host gains `ownerLedgers`, `ownerOperations` and `strandedLedgers`,
which read one incarnation's rows across all destinations and name the rows of a
destination the spec no longer reaches.

Evidence under `.context/v020-final-20261110/evidence/INT-DEL-A/`:

- Store rule, unit (`race13`): `TestIntegrationCleanupArmedRule` (10 cases, each
  for both drivers and four caller documents) and
  `TestPlanFinalizerExactIdentityAndTermination` pass.
- SQLite, real `state.db` (`race02`, 5.8 s): `TestFinalizers` (unchanged),
  `TestFinalizersReservedIntegrationCleanup` with its six subtests
  (`ArmedInTheSameWrite`, `NoApplyStripsIt`, `RetainDisarmsAndReleases`,
  `MetadataOnlyRemoval`, `CommandTransaction`, `SurvivesReopenAndRestore`) and
  the shared `TestReservedFinalizerConformance` pass. Whole package under race
  (`race06`): exit 0 in 320.9 s.
- Kubernetes, envtest 1.37 (`race03`, repeated on the committed tree in `race15`):
  `TestReservedFinalizerConformance`, `TestReservedIntegrationFinalizerSurvivesApply`
  (non-Force server-side apply, a foreign `kubectl` field manager, Force takeover,
  applies during deletion, release by `Retain`, host disarm) and
  `TestUpdateFinalizerIsMetadataOnlyAndExact` pass, with the existing
  `TestDeleteHonoursFinalizers` and `TestForceIsWholeObjectTakeover`. Whole
  package under race (`race11`): exit 0, 85 passes, the three opt-in measurement
  skips, 73.7 s.
- Negative controls with the rule removed from each driver (`negative01`,
  `negative02`): the SQLite cases fail in all six subtests and the conformance
  case; on Kubernetes the conformance case fails at the Force takeover
  (`finalizers=[]`) and a document that lists the reserved name arms it.
- Mounted SQLite runtime (`race09`, `race10`):
  `TestSQLiteObjectSourceIntegrationCleanupFinalizer` (file loader: create armed,
  unchanged and edited documents, document removal leaves a terminating object,
  repeated pass, re-declared unchanged and edited, release by declared `Retain`,
  new incarnation) and `TestIntegrationRuntimeActualLedgerForgetIsFencedAndAtomic`
  (a real elected lease; the root deleter refuses a budget row, a desired object,
  a request without UID and `Force`; a stale or missing later request rolls back
  the ledger delete; a guard of another lease and a guard fenced by an
  Integration change forget nothing; the exact forget removes one ledger row and
  two operations with three audit rows; the budget row remains). The selection
  `^(TestIntegration|TestSQLiteObjectSource|TestCLIReferenceDrift)` under race:
  exit 0, 37 passes, no skips, 206.9 s, including the unchanged
  `TestIntegrationRuntimeRefusesDeleteAndUnlicensedExecution`.
- Planner, ledger and host under race (`race12`): exit 0, 109 passes, no skips;
  ledger, planner and the Cloudflare adapter again on the committed tree
  (`race16`): exit 0, 62 passes, no skips. New: `TestOrdinaryPlanDigestUnchangedByCleanupMode`,
  `TestCleanupDeletesOnlyMarkerAndLedgerProvenRows`,
  `TestCleanupOverridesRetainedPolicyOnlyInCleanupMode`,
  `TestCleanupConflictsMixedSetsAndAdoption`,
  `TestCleanupRefusesDesiredStatePendingRowsAndCapacity`,
  `TestForgetRemovesConfirmedAbsentRecordByExactIdentity`,
  `TestForgetRefusesPendingBaselineStaleAndForeignState` (four subtests),
  `TestForgetOrderSurvivesACarrierWithoutTransactions`,
  `TestSQLiteForgetIsFencedExactAndDurable` (real SQLite: stale term refused,
  reopen, budget row intact) and
  `TestOwnerLedgersAndOperationsSpanDestinationsRemovedFromSpec`.
- Real Cloudflare SDK against the local HTTP double (`race08`, `race16`):
  `TestSDKCleanupPlanDeletesOnlyLedgerProvenRowsOnUpsertOnlyZone` sends exactly
  one batch with the one proven delete on an `upsert-only` zone; a copied marker,
  a previous incarnation's row and a hand-made row stay byte-identical, and the
  adapter still refuses the same delete under `upsert-only`.
- Other touched or dependent packages under race: `internal/store/memory`,
  `internal/objects`, `internal/store/validator` (`race07`), `ee/integration/unifi`,
  `ee/integration/webhook` (`race08`), `api/...` (`race14`): exit 0.
- Drift (`drift01`, `drift02`): `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in `race10`. No
  generated artifact changed, so there is no generated commit.
- Scoped lint on the committed tree (`lint02`, HEAD `057eed2d`): zero issues over `api/v1alpha1`, `internal/store`,
  `internal/store/{conformance,sqlite,kube}`, `ee/integration/{plan,ledger,host,cloudflare}`
  and `cmd/kudurru`.

Review fixes (commits `a67e7346`, `20d51820`, `e449084f`, `1466f53b`, `88f45f86`
and `91e900da` on `wip/v020/int`; the last one changes one sentence of the
operator guide and was made after the runs below, which name `88f45f86`; logs
`fix-01` to `fix-17`):

- Kubernetes patch lock (`fix-01`, 17.5 s): the new envtest case
  `TestUpdateFinalizerActualConflictIsNotRetried` wraps the transport and changes
  the object between the driver's read and its first `kudurru-finalizer` patch,
  for the arming write and for the release. Both subtests get `ErrConflict` after
  exactly one patch, and the concurrently written finalizers survive.
- Plan digest (`fix-03`): `TestCleanupFlagAloneChangesDigestAndOperationIDs`
  builds both modes over observations that plan the same single delete, checks
  that the two plans are equal apart from the flag, the digest and the operation
  ID, and that the digests, operation IDs and durable intent names differ.
- Audit reasons (`fix-16`, on the committed tree; `fix-06` is the same selection
  with the first wording of the reason, see `fix-08a` below):
  `TestReservedFinalizerDropReason` (8 cases), `TestReservedFinalizerUnarmed`, and in
  `TestFinalizersReservedIntegrationCleanup` the extended
  `RetainDisarmsAndReleases` and the new subtest `OmittedPolicyDropIsAudited`
  (disarm, release with another finalizer left, release by a `Force` takeover
  with the policy omitted, and no reason on writes that keep the obligation).
- File reconciler over real SQLite (`fix-08`):
  `TestReconcileRequestsDeletionOfATerminatingObjectOnce` (three later passes and
  a restarted loader report nothing, the deleted metric stays 1, one `objects:
  deleted` line, no further audit row, and the object is gone after the cleanup
  owner's release), `TestReconcileArmsAnUnarmedDeleteIntegrationFromItsUnchangedDocument`
  (same process and after a restart: one update that changes only
  `metadata.finalizers`, then unchanged), `TestReconcileReleaseByDeclarationIsVisible`
  (policy omitted and explicit: `objects: finalized`, the audit reason, a new
  incarnation on the next pass) and
  `TestReconcileUnarmedDeleteIntegrationWithoutAdmissionIsAppliedOnce` (memory
  driver: one apply over three passes).
- File reconciler over the Kubernetes driver (envtest, in `fix-12` and `fix-16`):
  `TestFileDeclaredDeleteIntegrationIsNotRewrittenWithoutAdmission` reports the
  unarmed `Delete` Integration as unchanged on three passes of a restarted
  loader, with an unchanged resourceVersion and exactly one apply, and makes no
  apply once a host has armed the object.
- Mounted SQLite object sources (`fix-11`, through the lock wrapper, HEAD
  `88f45f86`): the selection
  `^(TestSQLiteObjectSource|TestIntegration|TestRunServeSQLiteSourceBinding|TestRunServeUniFiHostname|TestApply|TestDelete$|TestCLIReferenceDrift)`
  under race: exit 0, 43 passes, no skips, 346.0 s.
  `TestSQLiteObjectSourceIntegrationCleanupFinalizer` now also asserts that a
  live object whose finalizer was removed is armed by the next pass over its
  unchanged document (same loader and a restarted one, generation and UID
  kept), that two further passes over the terminating object report no
  deletion, and that the release audit row is `integration.finalize` by the
  file manager with the `released without cleanup` reason.
- Whole packages under race: `internal/store`, `internal/objects`,
  `ee/integration/plan` (`fix-10`): exit 0. `internal/store/kube` (`fix-12`): exit
  0, 87 passes, the three opt-in measurement skips, 74.9 s.
  `internal/store/sqlite` and `internal/api` (`fix-13`): exit 0 in 268.8 s and
  190.5 s. Every new or extended library and driver test again, twice, on the
  committed tree (`fix-16`): exit 0, 50 passes.
- Hygiene: `gofmt`, `go vet` and `CGO_ENABLED=0 go build ./...` clean (`fix-15`).
  Scoped lint through the lock wrapper on HEAD `88f45f86` (`fix-14`): zero issues
  over `internal/store`, `internal/store/{sqlite,kube,conformance}`,
  `internal/objects`, `ee/integration/plan` and `cmd/kudurru`. No generated
  artifact changed, so there is still no generated commit;
  `TestCLIReferenceDrift` passes in `fix-11`.

Retained failed runs of the review fixes (the negative controls and before-fix
runs were expected to fail; `fix-08a` was not): `fix-02` (build overlay, the patch without `metadata.resourceVersion`: both subtests fail with
`err=<nil> patches=1`, the stale write lands). `fix-04` (build overlay, the flag
set only after the digest: only the new planner test fails, with both digests
`abe407b9...`, the value the reviewer recorded). `fix-05`, `fix-07` and `fix-09`
(the new tests against the store and reconciler code as committed at `057eed2d`:
empty audit reasons, `deleted=1` on a pass over the terminating object,
`updated=0 unchanged=1 finalizers=[]` over an unarmed object, `objects: updated`
for a release). `fix-08a` is a transcript of the first run after the reconciler
fix, whose log file the passing rerun overwrote: it showed that the memory
reference driver rewrites the object on every `Apply`, and that the reconciler
defaults a document before it applies it, so the store cannot tell an omitted
`deletionPolicy` from an explicit `Retain` on the mounted path. The audit reason
therefore names the effective policy only, and the "unchanged without admission"
claim is proven on the real Kubernetes driver instead of the memory driver.
`fix-17` was not run: a negative control of the mounted test against the
`057eed2d` store and reconciler code waited 39 minutes for the machine-wide lock
behind other lanes and was canceled. The mounted test is therefore shown to pass
on the fixed tree (`fix-11`) but not shown to fail before the fix; the same
assertions fail before the fix at the library seam over real SQLite (`fix-07`,
`fix-09`).

Retained failed runs of the implementation: `race01` (two new SQLite subtests
read the bookmark of a resumed watch as an object event; the tests now skip bookmarks). `race04` (the
new mounted forget test built a receipt with a local-zone time; the repository's
exact comparison of the stored operation refused it, and the test now uses the
canonical UTC form the providers use; the other four tests of that run passed).
`lint01` (two findings in new tests: a `gosec` index warning and a deprecated
`FieldsV1.Raw` access; both rewritten).

Open after this package: the elected terminating job (DEL-3), the SQLite root
cleanup guards, local purge, finalization and removal of the refusal (DEL-4), the
API/CLI surface and release command (DEL-5), the operator guide and CRD field
description (DEL-6), the mounted lifecycle acceptance (DEL-7) and the Kubernetes
half (DEL-8). Until DEL-3 and DEL-4 land, nothing clears the finalizer: removing
the document of a SQLite Integration declared with `Delete` leaves it terminating
until it is declared with `Retain` or without a `deletionPolicy` line. The
cleanup plan and `Forget` have no production caller yet, and the root deleter is reachable only under an ordinary
(live object) guard. The memory reference driver has no deletion timestamp and
does not run the reserved-finalizer conformance case. On Kubernetes
`UpdateFinalizer` is on the driver only: the audit and identity-gate store
wrappers do not forward it, and no host arms the finalizer there yet.

Handed to the next packages by the review: (1) DEL-4 must check
`HasFinalizer(IntegrationCleanupFinalizer)` inside every intent and source
commit, as the map requires. The reconciler arms a declared object that was
stored unarmed on its next pass, but until that pass the object is unarmed, and
only the per-transaction check keeps a `Delete` instance from owning external
state in that window; DEL-4 should add a mounted test with a pre-existing
unarmed object when it removes the refusal. (2) A terminating Integration is
still released by any apply whose defaulted policy is `Retain`, including a
re-added document without the line or a restored older copy. This is now
documented, audited and logged, but it is not prevented; DEL-5 should consider
making release an explicit command only, with a reconciler diagnosis when a
document targets a terminating Integration (a new diagnosis reason is part of
the API contract, so it was not added here). (3) A Kubernetes `Force` takeover
whose body says `Retain` strips the finalizer without a specific reason: on
Kubernetes audit events come from the store wrapper (D156), not from the driver,
and the wrapper was not changed. (4) The memory reference
driver rewrites on every `Apply`, so there the reconciler's extra apply of an
unarmed `Delete` document counts as one update per loader start.
