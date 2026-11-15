### CORE-CHILD-RECONCILE one parent/child boundary rule, and a new child answers sooner — November 13, 2026 (D404)

Two packages fixed the same defect independently: a primary zone and a
primary child zone on one node could leave one or both answering SERVFAIL
until a restart. CORE-OPEN4 (in the pull request, D385 item 4) and
CORE-CHILD-ZONE (branch `wip/v020/flake-childzone`, not in the pull request)
found the same cycle: fencing a child in its parent and the child's lifecycle
transition required the exact lifecycle receipt of both zones, in
`sqliteadapter.source` and again in `currentguard.checkBackground`, while
realigning either receipt is a lifecycle CAS whose membership check needs that
fence. Every committed-status write puts a receipt behind its `DNSZone`.

**Where the two fixes differed, and what was kept.** Both relaxed the exact
receipt to "the same zone" for boundary-only commands. They differed in three
places:

1. *How the guard knows a check is boundary-only.* CORE-OPEN4 relaxed the
   guard by reason (`parent-fence`, `late-parent-fence`, `parent-release`)
   alone. CORE-CHILD-ZONE had the adapter, which knows the command, set
   `BackgroundCheck.Boundary`; the guard honors it only with those reasons or
   `lifecycle` and answers unavailable for the flag with any other reason.
   Kept: the flag. A reason alone also relaxed any future unmarked check with
   that reason.
2. *Whether a boundary command advances a receipt.* CORE-OPEN4's fence
   transition recorded the child's current source (`Runtime.currentSource`,
   read outside the transaction), so the child's CAS could stay exact.
   CORE-CHILD-ZONE never advances a receipt in a boundary command and reads the
   child's obligation-only transition (`boundaryTransition`: nothing but
   `Sequence` and `Parents` changes) with `boundarySource`. Kept: never
   advance. Recording a source, including a new intent digest, belongs to the
   exact realignment paths, and the outside read raced the next status write.
   `currentSource` is removed. One gap that this exposed is closed:
   `Runtime.FenceParents`' own Preparing-to-Fencing CAS kept the old receipt,
   so a Preparing child whose receipt was behind could not progress through
   the runtime path (the driver's own phase CAS already carried the observed
   source). It now carries the source observed at that moment, still checked
   exactly in the transaction (`Runtime.observedSource`).
3. *Deletion.* CORE-OPEN4 refused a `DNSZone` with a deletion timestamp on the
   relaxed path; CORE-CHILD-ZONE did not check it. Kept: refuse (the stricter
   rule). Both the adapter and the guard now call one predicate,
   `sqliteadapter.SameBoundaryZone` (object name, UID of the lifecycle and of
   its source, type, apex, not deleting, source not removed); the exact receipt
   still qualifies on its own.

The flake lane's serving changes (`image.go`, `membership.go`: a parent with an
unfenced desired child keeps serving and is not admitted; a child under a
Fencing parent keeps serving) had no counterpart in CORE-OPEN4 and are kept as
they were. `docs/architecture/zones.md` states the rule once (the CORE-OPEN4
paragraph is replaced by a pointer to the D404 paragraph and its sequence
diagram). The flake lane's commits were cherry-picked: `6ee32906` (resolved
against the head as above, commit `50534605`), `edcc5f5c`, `282fbc36`,
`d7671c31`, `34ca179e`; `880e4e2e` and its revert `8ffd0610` were not brought
except the test bound of `880e4e2e` that the revert kept. The five
refusal-inventory commits of that branch's base were not brought.

**Contradictory tests.** CORE-OPEN4's `TestLateChildFencesWhenBothSourcesAdvanced`
(`sqliteadapter`) and `TestActualLateChildFencesWhenBothSourcesAdvanced`
(`currentguard`) asserted that the fence records the child's current source.
That contradicts the kept rule, so they now assert the opposite (the fence
changes only the obligation; the source equals the old receipt) and,
additionally, that the child's own realignment records the current source once
it activates. `TestActualBackgroundGuardRelaxesOnlyBoundaryRevisions` now passes
the flag and additionally requires that an unmarked check with a boundary
reason is refused, that the flag is refused for `source-reconcile`, and that an
obligation-only `lifecycle` check refuses a deleting source. No bound was
loosened.

**A new child answers sooner (the open item of CORE-CHILD-ZONE).** Names under
a new child apex answer SERVFAIL until the child's first publication. That
answer is kept, deliberately: the parent's answer there would be a negative
answer that resolvers cache for the parent's negative TTL (minutes), hiding the
child after it activates, and a referral needs delegation data the parent does
not have and leads back to the same unavailable zone on this node. The
publication before the one that carries the new `DNSZone` still answers the
parent's data. What the driver waited for between zones: each of the child's
steps (creation, parent fence, Preparing-to-Fencing, activation; for a
re-created child also the release or retirement of the old incarnation) waited
behind one step of every other zone, and each such step reconciles that zone
and waits for its publication. The driver now steps a zone being created ahead
of the rotation (without moving its cursor) and keeps a zone whose step
committed a change needing another step in focus for up to eight passes. The
source refresh of an active nonprimary zone does not take the focus (review
fix): it repeats whenever that zone's `DNSZone` changes and would otherwise
give it up to nine steps for each step of the rest of the rotation. Steps
keep their own admission and publication waits; only the order changes. The
D395 bound (about 0.3 s) is not reached: the child's own steps (about 0.2 to
1 s each on this runner) and the rest of the pass already running remain.

Evidence under `.context/v020-final-20261110/evidence/CORE-CHILD-RECONCILE/`:

- Merge (`50534605`): `unit01` (the merged code with the CORE-OPEN4 tests
  unchanged: the three contradictory assertions fail, as expected), `unit02`
  (after updating them, the child's Reconcile from Preparing failed with
  `admission conflict`, which found the `FenceParents` gap), `unit03`
  (`currentguard`, `sqliteadapter`, `primaryruntime`, `driver` pass), `unit04`
  (`internal/primaryruntime/...` and `internal/servingzones` pass), `unit05`
  (`internal/servingbuild` passes, 101.5 s, including
  `TestPrimaryParentAndChildServeAcrossEachOther`), `race01` (`driver`,
  `currentguard`, `sqliteadapter` under race: pass).
- Scheduling, `internal/primaryruntime/driver`
  `TestNewChildStepsAreNotQueuedBehindEveryZone` (eight unrelated active
  zones; counts their publication waits while a child is created): before
  (`focus02`, `focus04`, `focus06`, `focus08`, overlay
  `overlay-before-focus.json`) 72 waits after the child's lifecycle existed and
  73 after its `DNSZone` was created, every run fails; after (`focus03`,
  `focus05`, `focus07`) 0 after the lifecycle existed, at most one in-flight
  pass from the creation, 5 of 5 pass. Under race (`race03`, retained) it
  failed twice: the fixture's 1 s operation timeout cut the child's steps
  short and their backoff let the rotation run; the test now uses the
  production 10 s timeout and passes 4 of 4 under race (`race04`).
  `TestLeadingZonesKeepTheRotationFair` (regression guard, not fail-first):
  passes (`race04`). `race05`, `race07`: the driver and `primaryruntime`
  packages pass under race. `race06`: 16 alternating race runs of
  `TestActualConcurrentCreationTypeReplacementAndMovedApex` (it failed once in
  `race03` at its 8 s `await`, as it did on the base in CORE-CHILD-ZONE
  `review-06`): 8 of 8 pass before (14.1 to 23.4 s), 8 of 8 after (12.2 to
  17.9 s).
- Tried and reverted: ending a running pass on any store change after the poll
  interval, so a zone created mid-pass leads sooner. Under race the driver
  package went from 142 s to 527 s with three failures (`race02`, retained),
  because with the tests' 10 ms poll interval every step forced a full read.
  `focus09` and `mount04` (first of its two runs) are from that version.
- Mounted, `TestRunServeSQLitePrimaryParentAndChildZones` now creates the child
  under its serving parent with `awaitNewChild`, which samples every 10 ms from
  the POST and requires the parent's negative answer, then SERVFAIL, then the
  child's answer (its own negative answer allowed), never the parent's
  negative answer once the child was seen, no silence without race, and a first
  answer within 15 s. Three zones, without race: before the order change
  (`mount01`, overlay) 3 of 3 pass, first answer 0.87 to 2.47 s; after
  (`mount02`) 3 of 3 pass, 0.49 to 4.12 s (the review's `review-mount02`,
  load about 20, measured up to 5.21 s, so the three-zone range is 0.5 to
  5.2 s; under race, `review-mount03`, 13.8 to 27.2 s), and
  `TestRunServeKubePrimaryParentAndChildZones` 3 of 3 pass (0.18 to 0.36 s);
  0 failed probes in every run. With three zones the order hardly matters.
  With 20 more zones on the node (overlay `overlay-manyzones-*.json`, evidence
  only): `mount06`, alternating, final code: before 3.43 to 14.61 s (median
  4.54 s), after 2.47 to 4.87 s (median 3.91 s), 2 of 2 runs pass each.
  `mount03` and `mount05` are earlier versions (without the re-creation fix in
  `223e2647`); `dbg04` (instrumented, retained failure: a re-created child took
  more than 15 s at load about 20) showed the old incarnation's release step
  did not keep focus, which `223e2647` fixed; `dbg05` after it: 1.86 to 5.27 s.
  `dbg01` to `dbg03`: instrumented baselines (step and read timings).
- Final code, the CORE-OPEN4 mounted tests and neighbors without race
  (`mount07`): `TestRunServeSQLiteParentEditedAsChildIsCreated` (31.3 s),
  `TestRunServeSQLiteZoneChangesApplyFromTheirPublication` (14.0 s),
  `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites` (31.2 s),
  `TestRunServeSQLiteCompletePrimaryUpdateRestart` (6.8 s),
  `TestRunServeKubePrimaryParentAndChildZones` (52.5 s): all pass. Under race
  they and the commit-gap test (child zone included) pass in the full run.
  `race08`: `internal/servingbuild` (424 s) and `internal/servingzones` pass
  under race.
- Lint `lint01`, `lint02` (`./internal/primaryruntime/... ./internal/servingbuild/... ./cmd/kudurru/...`):
  0 issues. Bench guards `bench01`: 14 tests OK. No API, DTO or generated file
  changed, so there is no generated commit.
- Review fix (focus fairness, `internal/primaryruntime/driver`
  `TestRefreshedActiveZoneDoesNotTakeTheFocus`: eight active primaries and an
  active forward zone whose `DNSZone` gets a status write after every
  committed change of its lifecycle, so its source is ahead at every read;
  the test reads the forward zone's lifecycle sequence at each publication
  wait of the other zones). Before the fix (`fix-focus02-before`,
  `fix-focus05-before`): 6 of 6 fail, 27 refreshes in 80 waits and 9
  refreshes between two steps of the other zones. After (`fix-focus04-after`):
  5 of 5 pass, at most 1 between two of them. `fix-focus01-before` (retained)
  is a first version with a free-running status writer that never let a
  refresh commit (every one conflicted); `fix-focus03-after` (retained) is the
  fixed code against that test's too-strict liveness check (at least eight
  refreshes; a fair rotation gives about three here), relaxed to two.
  `fix-race01-driver` (retained) is the driver package under race against
  that too-strict check (only the new test failed); `fix-race02-driver`: the
  driver package under race, `-count=2`, final code. `fix-lint01`: 0 issues.
  `TestImageLateDesiredChildBeforeLifecycleBlocksParent` is renamed
  `...HoldsParentAdmission` (the parent keeps serving; only its admission
  waits), assertions unchanged (`fix-unit01-image`: pass).
- Full command package: `full01` to `full04` (below).

**Full command package.** `python3 hack/go-test.py --timeout=30m github.com/robertherbaugh/kudurru/cmd/kudurru`
through the heavy wrapper at `3e822b60` (864 tests in 9 race batches, then the
no-race timing pass). `full01` ran batches 1 to 7 and was cut off by the 2 h
background-command limit during batch 8 (load about 20, three other lanes'
suites running); `full02` ran batches 8 and 9 and the no-race pass with the
same batching code (`hack/go-test.py`'s own `inventory` and `batches`). Batches
2, 3, 5, 6, 7 and 9 pass; the no-race pass
(`TestRunServeSQLiteLongApplyKeepsServingDNSAndReads`,
`TestRunServeSQLiteZonesAnswerAcrossCommits`,
`TestRunServeSQLitePrimaryParentAndChildZones`) passes. Six tests failed:
`TestRunServeKubeUniFiSourcesTwoReplicasAndPartition` ("the non-holder's
grant lapsed between holder commits"), `TestRunServeUniFiHostnameHeldPublicationDeadline`
("original hostname policy unavailable at held request capture"),
`TestAnchorEdgeActivationJoinedFollower` (a data race in the test fixture:
`joinPrimaryOn` in `cluster_join_test.go:51` writes the config while the
publisher's `config.Flatten` reads it), `TestRunServeKubeRPZFeeds` ("restart
with the primary unavailable: 40 answers for the listed name, none NOERROR"),
`TestRunServeSQLiteTOTPEnrollmentRestartRecovery` (`POST /auth/totp/confirm`
503) and `TestRunServeSQLiteCompoundZoneCommands` (`POST /zones/compound/notify`
503 `generation_pending`). Controls, three race runs of the six each:
`full03` on this head: UniFi hostname 2 of 3, Kubernetes UniFi 1 of 3, TOTP
1 of 3 fail, the other three pass 3 of 3; `full04` with the nine production
files of this package overlaid back to `95c57113` (`overlay-base-source.json`;
tests unchanged): RPZ 1 of 3, compound zone commands 2 of 3, Kubernetes UniFi
1 of 3, UniFi hostname 2 of 3, TOTP 1 of 3 fail, the anchor test passes 3 of
3. So five of the six fail without this package's change too; the sixth is a
fixture data race in code this package does not touch (`cluster_join_test.go`,
`internal/config`) that did not recur in six runs.

**Still open.** A new child still answers SERVFAIL for a few seconds on a
loaded node (2.5 to 4.9 s measured with 23 zones, up to 5.2 s with three at
load about 20), not the D395 0.3 s, so direction item (4) is only partly met
and needs a follow-up: its own
four or five steps, each a transaction, a publication wait and a fresh read,
and the rest of the pass already running when it was created. Shortening that
further means merging lifecycle steps or yielding a running pass cheaply, both
changes to the driver's protocol that were not made. Under race a new child
can still take tens of seconds (the mounted test allows three minutes). The
mounted no-race bound stays 15 s, about three times the slowest sample; it
can be tightened toward 10 s once more samples on loaded runners exist. Not
re-measured here: deleting a parent while its child survives (existing
re-incarnation, CORE-CHILD-ZONE's open item). D385 item 4 (CORE-OPEN4) is
superseded by D404 and should be marked so in the decision log. The setup
retries after 503 `current writable serving generation unavailable` in the
mounted tests remain (CMD-RECAPTURE, D399).
