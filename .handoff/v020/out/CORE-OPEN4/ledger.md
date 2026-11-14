### CORE-OPEN4 zone-group, impersonation and parent/child gaps closed — November 13, 2026 (D385)

This package closes what the verifications of IAM-ZONEGROUP, IAM-IMPERSONATION
and CORE-COMMIT-GAP left open. Item 3 of the integrator's list (the 503
`current writable serving generation unavailable` answers after another commit)
belongs to CMD-RECAPTURE (D399) and was not touched: the shared command path
(`servinghttp.CheckWriter`, `Publisher.CheckWriterLease`,
`currentAuthCommandThen`) and the 503 retries of the mounted zone-group tests
are unchanged.

**A zone deleted during a PUT is not created again.** On the store
(Kubernetes) path `authorizeWrite` reads the DNSZone and admits a PUT at zone
scope when the zone exists and its label is unchanged. `writeObject` then reads
it again for the compare-and-swap. If an administrator deleted the zone between
the two reads, `writeObject` found nothing and applied without an expected
resourceVersion, so a zone-group member's or zone admin's PUT created the zone
again, label included. `writeObject` now requires `zones.write` at cluster
scope whenever a DNSZone write finds no zone on its own read. A delete after
that read fails the compare-and-swap with 409. The SQLite command path already
read and authorized in one transaction and treated a missing zone as a create.

**Lists answer 503 instead of silently leaving items out.** `allowed()` turned
an authorizer or zone-group read error into "not allowed". List filters
(`GET /zones`, `GET /objects/{kind}` on the store and the captured paths) now
use `listAllowed`, which reports the error. The whole list answers 503, and a
credential that ended meanwhile answers 401. `GET /auth/me` answers 503 when a
verb of its matrix cannot be decided. `allowed()` keeps its old meaning only
for the cluster-scope capability checks, which never read a zone. The code and
docs/architecture/access.md now say the same thing.

**Impersonation refusal match anchored.** `impersonateVerbDenied` now
recognizes only the apiserver's own form
(`<resource> ["<name>"] is forbidden: User "<requester>" cannot impersonate...`,
which covers legacy `impersonate`, `impersonate:<mode>` and
`impersonate-on:<mode>:<verb>`). The quoted requester is parsed as a Go string
and must differ from the impersonated user. A principal whose name contains the
phrase, or an admission webhook message that uses it, is the principal's 403
again, not a 503 deployment fault.

**Docs.** The admin guide (`docs/guide/admin/09-people-and-tokens.md`) and
access.md now say that on Kubernetes the forced field-ownership takeover happens
on every label change through kudurru: adding, changing or removing the label,
always by a cluster-scope writer. It removes every field that kubectl or a
GitOps tool set and the document omits, when managed-by is empty, `api` or
`bootstrap`. access.md also describes the create rule, the list 503 and the
anchored match. The broken code fence in `docs/deploy/helm.md` (a verifier
finding of IAM-IMPERSONATION) is fixed. `docs/architecture/zones.md` describes
the boundary-command rule below.

**Parent/child SERVFAIL for good: cause found and fixed (in two steps).**
This was the new problem raised by the CORE-COMMIT-GAP verifier. On a mounted
SQLite node, a primary child created under a primary parent could leave the
pair SERVFAIL ("zone awaiting committed publication"). A driver trace showed a
deadlock (temporary overlays `overlay-childdbg` and `overlay-conflictdbg*`,
which log every step error and every `ErrConflict` site; never committed). The
child's fence (`AdmitAncestor` or `FenceParents`, an admission with a `Parent`)
required both zones' recorded lifecycle sources to equal their DNSZones' latest
revisions. The parent's DNSZone had moved on (an edit, its own status write).
An Active parent records a new source only through a transition whose
`verifyDescendants` and adapter `membership` need every desired child already
fenced. The child's DNSZone could also move on, and the driver fences a child
before it refreshes the child's source. A boundary command changes nothing in
the parent head but the child's system reservation (`candidate` enforces this),
so the revisions add no safety there.

The revision was checked in two places, and the first fix (`c3c4be32`) relaxed
only one. The SQLite adapter checks both zones of a boundary command with
`boundarySource`: the exact lifecycle as before, and the same source zone (UID,
type, apex, not deleting), not its latest revision; a removed source keeps the
exact check. The fence's child transition records the child's current source
(`Runtime.currentSource`). But the production background guard,
`currentguard.checkBackground` (mounted at `cmd/kudurru/primary_runtime.go`),
still compared the parent's current DNSZone revision and intent digest with the
parent lifecycle's recorded source, so a parent whose DNSZone moved on still
could not have a child fenced; the adapter tests had passed only with the
permissive test guard. The reviewer reproduced this with the real guard
(`review-01`, `review-04`) and on a mounted node (`review-07`: pair 1 of 2
SERVFAIL for 90 s, readiness 503). `87618b4a` makes the guard agree: for the
parent-fence, late-parent-fence and parent-release reasons it needs the same
object name, UID, type and apex and an undeleted, unremoved source, not the
latest revision. The guard reads no source policy for those reasons, and every
other reason keeps the exact revision check. The parent then records its own
source on its next reconcile. The child zone is back in
`TestRunServeSQLiteZonesAnswerAcrossCommits` (reverting the test half of
`b1ae724a`).

Evidence under `.context/v020-final-20261110/evidence/CORE-OPEN4/`:

- `TestZoneDeletedDuringPutIsNotRecreated` (`internal/api`) deletes the zone
  before each DNSZone read of the PUT in turn, for a group member and a zone
  admin. Before the change (`unit02-before.log`) it fails at read 3 with HTTP 201
  and the zone recreated with its label. After (`unit01-after.log`) it passes.
  `TestServingObjectCommandResolvesZoneGroupInTransaction` now also deletes a
  member zone and checks that the group token's PUT of it gets 403 and commits
  nothing. It passed under race in `race02` and `race08`. It pins the SQLite
  behavior, which was already correct, so it does not fail before the change.
- `TestZoneGroupUnreadableZoneFailsLists` (`internal/api`): before
  (`unit03-lists-before.log`, overlay `overlay-lists.json`) it fails with "status
  200, want 503: {"items":[]}". After it passes (`unit04`). The whole
  `internal/api` package passes (`unit05-api-all.log`, 65.7 s), and the
  zone-group tests pass under race (`race03`).
- `TestImpersonateVerbDeniedMatchesOnlyTheAPIServerForm` (`internal/store/kube`):
  before (`unit07`, overlay `overlay-impersonate.json`) it fails for the odd
  principal name (twice), the self-named requester and the webhook message.
  After it passes. `race01-kube-impersonation.log`: every `Impersonat*` test
  passes under race on envtest 1.37, including
  `TestImpersonationWithoutImpersonateVerb`, which sees the real apiserver
  refusal (20.5 s).
- Late child: `TestLateChildFencesAParentWhoseSourceAdvanced` and
  `TestLateChildFencesWhenBothSourcesAdvanced` (`internal/primaryruntime/sqliteadapter`)
  fail before (`unit08`, `unit11`, overlay `overlay-latechild-before.json`) with
  "admission conflict" and pass after (`unit09`, `unit10`). The second test also
  checks that a replaced child zone is still refused. These use the permissive
  adapter test guard, so on their own they did not prove the production path.
  - With the production guard (`internal/primaryruntime/currentguard`,
    `latechild_test.go`, real guard on real SQLite):
    `TestActualLateChildFencesAParentWhoseSourceAdvanced` (subtests
    `AdmitAncestor` and `FenceParents`),
    `TestActualLateChildFencesWhenBothSourcesAdvanced` and
    `TestActualBackgroundGuardRelaxesOnlyBoundaryRevisions` fail before
    `87618b4a` (`fix-01-guard-before.log`: "admission conflict" for each step
    and each of the three boundary reasons) and pass after.
    `TestActualLateChildIsNotFencedIntoAReplacedParent` passes before and after
    (it pins that a replaced parent is still refused). The relaxation test also
    checks that a replaced, retyped, moved or deleting source is refused for
    every boundary reason, and that `source-reconcile` and `lifecycle` still
    refuse a source past the recorded revision. `fix-02-guard-after-race.log`:
    `currentguard` (90 s), `sqliteadapter` (130 s) and `primaryruntime` (240 s)
    pass under race, count=2.
  - Mounted: `TestRunServeSQLiteParentEditedAsChildIsCreated` (new,
    `cmd/kudurru/serve_zone_change_mount_test.go`, adapted from the reviewer's
    probe) creates four parent/child pairs and edits each answering parent's
    name servers at the same moment as its child is created (child object name
    sorting first). With the pre-fix guard through an overlay
    (`fix-overlay-guard-before.json`) it fails at pair 0
    (`fix-03-mount-parent-edit-before-norace.log`: "30s after the child's
    records: parent=SERVFAIL child=SERVFAIL readiness=503"). After the fix,
    `fix-05-mount-after-norace.log`: 3 of 3 pass without race, every pair
    answering 1.3 to 5.3 s after the child's records. A retained failed run,
    `fix-04-mount-after-norace.log`, passed pair 0 and then failed on the
    concurrent child `POST` answered 503 "current writable serving generation
    unavailable" (the D399 family); the test helper now asks such a `POST`
    again and counts a later 409 "already exists" as created.
    `fix-06-mount-after-race.log` (race, count=2, about 25 minutes, with other
    lanes' heavy runs sharing the machine): the new test, the zone-change test
    and `TestRunServeSQLiteZonesAnswerAcrossCommits` pass 2 of 2 each. Pairs
    answered 19 to 61 s after the child's records under race (bound 2 min),
    and the commit-gap test had 0 probes without a correct answer across about
    42,000 probes per name per run.
  - Mounted reproduction before the fix: `child02` (child created 2 s after the
    parent answered) has 2 of 6 runs stuck for 60 s, and `child03` has 1 of 6
    (with the step and conflict trace). `child01` (no forward zone, simpler
    setup) passes 6 of 6, so the deadlock depends on timing.
  - `TestRunServeSQLiteZonesAnswerAcrossCommits` with the child created together
    with its parent, under race: with the old adapter (`gap03`, overlay
    `overlay-adapter-before.json`) 3 of 3 fail in setup ("mounted DNS never
    reached probe.private.example."). With only the parent half of the fix
    (`gap02`, `gap04` traced) 3 of 6 runs still deadlock, on the child's stale
    source. That trace is what led to the second half. With the final code:
    `gap06` 3 of 3 pass without race (about 11,100 probes per name, 0 without a
    correct answer). `gap05` 3 of 4 pass under race (22,000 to 41,500 probes per
    name, 0 without a correct answer). The fourth `gap05` run failed on a setup
    `POST /objects/blocklists` answered 503 "current writable serving generation
    unavailable" (the CMD-RECAPTURE family, D399), not on DNS. `gap01` (parent
    half only, without race) passed 5 of 5.
  - `race04`/`race05`/`race06` (`internal/primaryruntime`, `.../sqliteadapter`,
    `.../driver`, with envtest) and `race07` (`internal/servingbuild` 587 s,
    `internal/servingzones`, `internal/primaryruntime/network`) pass under race.
    `race04`'s kube subtests failed only because `KUBEBUILDER_ASSETS` was unset;
    `race05` reruns them with it set and they pass.
- `TestRunServeSQLiteZoneChangesApplyFromTheirPublication` (new,
  `cmd/kudurru/serve_zone_change_mount_test.go`; the forward-group and
  tightened-ACL coverage the CORE-COMMIT-GAP verifier found missing). A forward
  zone moves to another upstream group, and a primary zone's query access is
  tightened so the prober is refused, then loosened. After review it fails on
  any unanswered probe without the race detector (at most one per write under
  it), on any SERVFAIL or other answer, on an old answer later than
  `servedSuccessorGrace` after the first new one, and when the first new answer
  comes later than 5 s (30 s under race) after the write's 200.
  `fix-05-mount-after-norace.log`: 3 of 3 pass; the first new answer came 131
  to 383 ms after the 200, with 0 old answers inside the grace and 0 probes
  unanswered (`change01`/`change02` from before the review: 3 of 3 each).
  `fix-06` under race: 2 of 2 pass, first new answer 2.3 to 4.6 s after the
  200, 0 old answers inside the grace, 0 unanswered. What
  it proves is "the change takes effect within a bound of its commit, and no old
  answer after the first new one plus the grace". It does not prove refusal from
  the very first publication that carries the tightened ACL: the write returns
  at commit, before its publication, so 13 to 30 old answers between the 200 and
  the first new one are allowed, and a mounted probe cannot see which
  publication answered it. That rule is proven only in `internal/servingbuild`
  and `internal/servingzones`. It covers existing D395 behavior, so it is not a
  fail-first test.
- `race02-cmd-selection.log` (32 tests from the auth, IAM, zone-group and
  impersonation mount files, under race): 30 pass. The two failures,
  `TestServingHTTPAuthCookieCommittedEnrollmentMismatch` and
  `TestServingHTTPLocalUsersResetImageRestartPromotion`, are a data race in the
  test fixture, not in this package. `newServingHTTPFixture`
  (`serving_http_test.go:34`) writes `f.s.opts.Now` after the serving runtime
  started, while the integration host reads it (`integration_runtime.go:157`).
  `race08-cmd-final-selection.log` (20 tests after the zones fix:
  zone-group mounts, `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites`,
  `TestRunServeSQLiteCompletePrimaryUpdateRestart`, `TestRootAnchor*`,
  `TestServingAnchor*`, the new zone-change test) all pass.
- Review fixes: `fix-07-lint.log` (`./internal/primaryruntime/currentguard/...`,
  `./cmd/kudurru/...`) reports 0 issues; `fix-08-bench-guards.log` 14 tests OK.
  No API, DTO or generated file changed, so there is still no generated commit.
- Lint: `lint01` (`./internal/api/...`, `./internal/store/kube/...`) and `lint02`
  (also `./internal/primaryruntime/...`, `./cmd/kudurru/...`) report 0 issues.
  Bench guards: 14 tests OK. `TestOpenAPIDrift` and `TestReferenceDrift` pass
  (`drift01`). No API or DTO changed, so there is no generated commit.

**Still open.** The 503 "current writable serving generation unavailable"
retries in `cmd/kudurru/serve_zone_change_mount_test.go` (`zoneChangeNode.request`,
which also asks a POST again and counts a later 409 "already exists" as created)
and in `cmd/kudurru/serve_commit_gap_test.go` mask the D399 family like the
zone-group mount tests' retries do; CMD-RECAPTURE should remove them together
with those (the integrator has to hand these two files over). The Kubernetes
half of the delete-during-PUT rule (a delete between `writeObject`'s read and
its `Apply` fails the compare-and-swap with 409) is argued from
`internal/store/kube/crud.go` `Apply` (an `ExpectedResourceVersion` with no
current object returns `ErrConflict`, and a NotFound under a precondition is
treated as raced) and is not exercised on envtest or the mounted Kubernetes
test; the captured SQLite list paths that now call `listAllowed` have no 503
test. A mounted test cannot show that the first publication carrying a
tightened ACL refuses (see above); that stays proven only below `cmd/kudurru`.
Under race a parent/child pair took up to 61 s to answer on a loaded machine,
so the driver's retry backoff deserves a measurement. The fixture data race above (`f.s.opts.Now` written after
start) is a test defect in the integration and serving-HTTP fixtures. It is not
fixed here and should go to the lane that owns them. Under race the
created-together parent took 21 to 54 s to answer for the first time (`gap05`),
close to the 60 s bound of `awaitMountedDNS`. The driver's per-zone retry
backoff (1 s doubling to 32 s after transient conflicts) is the likely reason;
I did not measure it. The seven assertions that CORE-COMMIT-GAP inverted or
loosened still need the integrator's explicit acceptance; I did not re-audit
them. The trailer remark of the CORE-COMMIT-GAP verifier needs no change: the
brief asks for the model that actually ran. The 503 `generation unavailable`
setup refusals stay with CMD-RECAPTURE. The 1.39 s CORE-OPEN3 `run05` gap and
`TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` were not looked at again.
