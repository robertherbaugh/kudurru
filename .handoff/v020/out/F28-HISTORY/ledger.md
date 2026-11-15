### F28-HA-2 Managed anchor history across promotion, rejoin and conflict resolution — November 10, 2026 (D369)

The map predicted from code reading that a member cannot install the
authoritative branch when its own managed owner generation is equal or higher.
The prediction reproduced. Every authenticated root observation by an elected
writer adds one generation to the accepted owner, and `InstallReplica` and
`ResolveConflict` accepted a different managed owner only with a strictly greater
envelope and pure generation and an update time that was not earlier. Two writers
of one history count separately. `race01` runs the new store tests on unmodified
lane head `f9818a59`: an old primary at owner generation 3 cannot install its
promoted follower's branch at generation 2 or at a different generation 3, a
losing conflict branch at generation 4 cannot adopt the audited resolution's
branch at generation 3, and every install returns
`trustanchors/accepted: original owner changed`. `final-neg08` shows the same
through two mounted runtimes and the production puller: the old primary stays
fenced with `reason="dns_only"`. It would have stayed there until the promoted
writer's generation passed its own, about a day at the real root and never if
that updater had stopped.

A different managed owner is now ordered by authority term first (D369).
`InstallReplica` adopts it when the image's term has a strictly higher epoch than
the installed image's term. `ResolveConflict` and `ResolveAudited` adopt the
branch they keep in the transaction that allocates the new term. Neither compares
generations or update times across branches. Adoption still requires the same
cluster, history, initialization time, policy, seed digest and resolver domain,
and the incoming state must retain every recovery record the member accepted
(`RetainsRecoveryHistory`, unchanged). An unchanged owner, the first install and
the first managed owner over a dormant one are not adoptions and keep their
rules; a managed owner is never replaced by a dormant one; every install inside
one term keeps the strict successor rule. `cluster resolve --offline` keeps the
resolving member's own image (D361), so the adoption there happens on the other
members, through their scheduled pulls under the new term.

No revocation is lost. In the same transaction as the owner rows, the image and
the quarantine of the abandoned branch, the node-local carrier is rebased onto
the adopted owner. It keeps every key the abandoned owner revoked and every key
the member's own supervisor denied, and its floor becomes the adopted generation
minus one. Adoption requires the adopted owner to revoke every one of those keys,
the rule an install inside one term already applies to a pending denial
(`TestAnchorLocalInstallCannotOmitPendingDeny`). Otherwise the install, or a
resolution that would keep such a branch, is refused with `accepted.ErrCAS`
before any write: the member stays fenced, keeps answering DNS-only from its own
branch, which already revoked the key (a follower whose own supervisor denied it
stays retired, as before), and takes no later image of the new term. The first
scheduled pull after the canonical writer has recorded the same revocation
adopts the branch, and the member publishes at once. The first form of this
package installed such a branch and kept its generation retired instead; the
review showed that turned a member answering DNS-only into one answering
`SERVFAIL` to every query, with equal trust safety and the same wait, and the
fix commits replaced it (see "Review fixes" below). A denial receipt that named a generation above the adopted owner
is restated at the adopted owner, because the carrier format (unchanged) cannot
name one; a terminal carrier and a denial that no longer fits its bounded list
stay terminal.

The abandoned branch's floor can lie at or above the adopted generation, so the
retirement gate needed one transition that is not monotone. `Change.StageRebase`
takes the exact floor committed with the adopted owner. It requires an installed
mark of the same domain and a floor that retires every generation below the
target, and a confirmed installation clears the registration. A restart restores
the same gate from the committed carrier. The adopted owner can be one the member
held before (the old primary returns to the owner its promoted follower never
left), and a write attempt captured for that owner before the split would find
its expected mark installed again. The gate therefore counts confirmed rebases
and refuses every write attempt captured before one. The source's exact cursor
identity refuses such an attempt independently; `final-neg07` removes the gate's
count and shows the attempt then stopped by the source with
`original owner changed`. `Stage` keeps its monotone rule.

Lane commits on `wip/v020/f28`: `90fe321b` (gate), `bf3addf9` (store rule, store
tests, architecture and operator docs), `3d374c08` (mounted tests) and `febbcd46`
(six lines in `docs/deploy/sqlite-join.md` only). The package changes no API
route, DTO, setting, CRD, permission or CLI text, so there is no generated
commit.

Evidence under `.context/v020-final-20261110/evidence/F28-HISTORY/` (default race
detector; real SQLite member databases; every owner produced by the pure state
machine from signed DNSKEY responses; no forged anchor state or sample). The
`final-*` logs ran in one batch on lane head `3d374c08` with a clean worktree
(`final-batch.sh`; each log records the head and the count of uncommitted paths,
`final-tree.log` the state after it). `febbcd46` was committed after the batch
and touches documentation only.

- `final-race01`, storage composition (`./internal/trustanchors/...`,
  `./internal/store/sqlite`, `./internal/runtimeowners`,
  `./internal/store/follower`): exit 0, 1,151 pass entries (433 top-level tests),
  zero failures, zero skips; the SQLite package took 273.2 s. It contains:
  - `TestAnchorHistoryRejoiningPrimaryAdoptsPromotedBranch/lower-generation`
    (0.93 s) and `/equal-generation` (1.01 s): an old primary commits one
    observation its follower never pulls; the follower is promoted and, in the
    second case, polls as the new writer. The old primary is fenced by the
    promoted term and installs that branch. It becomes a follower on the adopted
    owner, the carrier advances exactly once with the floor at the adopted
    generation minus one, the registration of the abandoned branch and an earlier
    registration of the shared owner are no longer current, a publication attempt
    captured before the split is refused by the gate, the adopted owner registers
    and reads, and the abandoned image with its owner is in quarantine. A
    physical close and reopen restores the same carrier and publishes again, and
    the next canonical observation installs under the strict rule.
  - `TestAnchorHistoryLosingBranchAdoptsAuditedResolution` (1.13 s): two members
    promoted from one image into the same epoch both poll; the loser polls twice.
    A conflicted member installs nothing. `ResolveAudited` keeps the first
    branch without changing its anchor cut; the loser observes the new term and
    adopts generation 3 over its own generation 4, with its branch quarantined.
  - `TestAnchorHistoryResolutionKeepingRemoteBranchAdopts` (0.91 s): the Store
    API keeps the other member's branch; the resolving member adopts it in the
    resolution transaction and continues that history as the new writer.
  - `TestAnchorHistoryAdoptionKeepsLocalRevocationDenied`:
    (first form, replaced by the review fixes; the final form is described
    under "Review fixes") `/revoked-by-old-primary` (0.94 s) adopted a branch
    that still trusts a key the old primary revoked and kept the adopted
    generation retired until the canonical tombstone. `/denied-by-follower`
    (0.94 s) was the same for a follower's own deny-only observation. `/revoked-by-canonical-branch`
    (0.68 s) adopts a branch that revoked a key the old primary still trusted.
  - `TestAnchorHistorySameTermInstallStaysStrict` (four subtests): a different
    owner at a lower or an equal generation inside one term is still
    `accepted.ErrCAS` with nothing changed, on an ordinary follower and on a
    member that adopted a branch earlier. These four re-seal an image no honest
    writer can produce, and say so.
  - `TestAnchorHistoryAdoptionKeepsIdentityAndRecoveryRefusals` (four subtests):
    under a higher term, another history, other seeds and a dormant owner are
    refused, and so is the promoted branch for an old primary that holds a real
    `Recover` record the branch lacks. Nothing changes and nothing is
    quarantined.
  - `TestAnchorHistoryAdoptionCarrierRules` (four subtests, first form): a
    receipt above the adopted generation is restated, a terminal carrier stays
    terminal, a revocation that does not fit closes the carrier, and a carrier
    bound to another member or owner is refused.
  - `TestCursorRebase*` (five tests, eight subtests) in the gate package: lower,
    equal and higher targets on the rebased floor with the old registration and
    write attempt retired; a rebased floor that keeps the target retired, and a
    terminal one; rollback and unknown completion; refusal of a foreign domain,
    a malformed mark, a shallow floor, the same mark and an unbound gate; and a
    return to an earlier owner whose old write attempt must not run.
  - Every existing anchor test of those packages, including
    `TestAnchorLocalInstallCannotOmitPendingDeny`,
    `TestAnchorLocalInstallRetainsDenyAndRequiresTombstoneCoverage`,
    `TestAnchorInstallCannotAdoptSuccessorBaseline`,
    `TestCursorFullExpectedIdentityAndRegressionRefusal` and the recovery tests.
- `final-race02`, the affected command selection (`Anchor|Doctor|Primary|Cluster|
  Join|Promot|Member|Enroll|Fenc|Replica|Follower|CLIReference|ManagedTopology|
  ManagedEdges|ServingRuntime` over `./cmd/kudurru`): exit 0, 331 pass entries
  (171 top-level tests), zero failures, one skip (`TestDoctorDatabase`, no
  PostgreSQL DSN), 468.8 s. Against F28-FENCED's `fix-race07` it adds exactly:
  - `TestAnchorHistoryRejoiningPrimaryAdoptsPromotedBranchAndPublishes/
    lower-generation` (3.51 s) and `/equal-generation` (3.87 s): two actual
    managed/direct runtimes on one signed in-process root with two seed keys.
    The old primary runs the bootstrap and one more production root operation.
    The follower's database is promoted for real and, in the second case,
    physically reopened under an actual elector whose writer runs its own
    operation. The old primary observes the promoted term, publishes `dns_only`
    with the managed seal of its own image and emits through the production
    output guard, then `follower.Puller.Pull` installs the promoted branch. It is
    a follower on the promoted image, both members hold the same owner version,
    complete publication resumes with the managed seal of the adopted image, the
    abandoned owner is in quarantine, a second pull changes nothing, and root
    reads are exact: one supervision poll in the fenced role and one in the
    follower role, none more inside the cooldown.
  - `TestAnchorHistoryRejoiningPrimaryKeepsItsRevocationUntilCanonical`
    (4.10 s, first form, replaced by the review fixes): the root starts serving the first key with its self-signed REVOKE;
    the old primary's operation takes the two-phase lane and still has one
    trusted key. The successor is promoted from the image before that. The old
    primary serves its own branch as DNS-only data, pulls and installs the
    promoted branch, which trusts both keys. From then on the Store refuses
    resolver input, no publication of the adopted generation exists, the output
    guard refuses to emit and protected HTTP is closed, across the new role's
    supervision poll and an unchanged pull. After the promoted writer's own
    operation records the REVOKE and the next pull installs it, the member
    publishes completely with one trusted key.
  - Every earlier anchor, follower, supervisor, edge, cluster, join and
    promotion test in that selection, and `TestCLIReferenceDrift`.
- `final-race03`: `./internal/cluster/...` (six packages with tests, including
  `replica` and `enrollment/persistence`, which install and resolve through the
  changed seam): all `ok`.
- `final-race04`: the separate-process enrolled scenarios, which use static
  anchors: `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (67.5 s),
  `TestEnrolledClusterProcessesPromoteAndRejoin` (79.3 s) and
  `TestEnrolledProcessConflictResolution` (55.8 s) pass, zero skips, 217.5 s.
  They show that the changed install and resolution calls behave as before for
  owners that are not managed.
- `final-lint01`: scoped golangci-lint v2.15.0 over `./internal/store/sqlite/...`,
  `./internal/trustanchors/...` and `./cmd/kudurru/...`: zero issues.
  `final-vet01`: `gofmt -l` prints nothing for the touched packages and `go vet`
  is clean. `final-drift01`: `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass.
- `drift02` (light run on the final lane head `febbcd46`, clean worktree): the
  same three drift tests and the `./deploy` package pass after the docs-only
  commit.

Retained failed runs and what they showed. The negative runs replace production
files through `go test -overlay` and never touch the worktree (`mutations.py`;
each log starts with the replaced files as a diff):

- `race01` (unmodified production code, first form of the store tests; 10
  failing entries in 5 tests): the predicted defect, as above. The identity and
  recovery refusals pass there, as they should.
- `final-neg08` (the committed tests against the six SQLite store files of
  `f9818a59`): both mounted tests fail at the pull with
  `original owner changed`, the old primary still `dns_only`. `race05` is the
  same on the uncommitted tree.
- `final-neg01` is not a negative run. Its overlay was built from `HEAD` after
  the commits, so it replaced nothing, and the mounted tests passed a second
  time (4 pass entries, 13.7 s). `mutations.py` now pins the base commit and
  `final-neg08` is the rerun.
- `final-neg02` (adoption forgets what the member knew to be revoked): the two
  denial flows end with the floor one too low, a receipt is not restated and a
  revocation that does not fit is dropped.
- `final-neg03` (every install may adopt): a lower and an equal generation are
  accepted inside one term, and a successor without the tombstone erases a
  carried denial.
- `final-neg04` (adoption ignores recovery history): the old primary that holds
  a recovery record installs the branch that lacks it.
- `final-neg05` (the abandoned branch's floor is kept): the install succeeds and
  the adopted owner stays retired in the rejoin, audited resolution and
  remote-branch tests.
- `final-neg06` (a carried denial does not retire the adopted generation): both
  denial flows leave the floor open under an owner that trusts the key.
  The rule it mutated was replaced by the review fixes (a pending carried key
  now refuses the adoption); `fix-neg01` is the negative for the final rule.
- `final-neg07` (the gate ignores its rebase count): the gate test's old write
  attempt runs, and in the store test that attempt is refused only by the
  source's cursor identity.
- `race04`: the first mounted run. The rejoin test passed; the revocation test
  failed on its own helper, which passed a 90-second context to a publication
  capture that needs at most 30 seconds (`retirement: invalid input`). `race06`
  is the rerun after that test fix.
- `mut01`: a mutation that did not compile. `mut02` to `mut06` are the first
  runs of the five store mutations, before the gate counted rebases.
- `lint01` (before the commits): two findings in the new mounted test file only,
  a gosec G115 conversion and revive's context-as-argument. Both were fixed
  before the commits.

`race02` is the first passing run of the store tests (19 pass entries) and
`race03` the first storage composition run, both before the gate counted rebases
and before the stale-attempt assertions existed.

Review fixes (verdict "accept-with-fixes", two minor findings). Fix commits on
`wip/v020/f28`: `8a047222` (store rule, store and mounted tests, architecture
page) and `06afcb73` (operator page only). No earlier fixer commits or
uncommitted work existed when these began.

- A carried revocation the adopted owner lacks no longer turns a DNS-only member
  into a `SERVFAIL` one. `anchorAdoption` and `anchorLocalForAdoption` refuse the
  adoption with `accepted.ErrCAS` unless the adopted owner revokes every key the
  abandoned owner revoked and every key in the node-local denial, and the
  carrier floor on adoption is always the adopted generation minus one. This
  restores on the adoption lane the same-term guard the first form bypassed. The
  D369 row records the rejected alternative and its availability cost.
- `docs/deploy/sqlite-join.md` now says that such a member stays fenced and
  answers DNS from its own data, takes no later data from the primary, how to
  recognize the state until F28-GATE-4, that no command clears a carried denial,
  and the options: replace it with a newly joined receiver or keep it out of
  service.

Evidence (`fix-*` logs, default race detector):

- `fix-store03` (lane head `06afcb73`, clean tree): `./internal/store/sqlite`,
  `./internal/trustanchors/retirement`, `./internal/store/follower`, `-v`: all
  `ok` (417.7 s, 1.2 s, 6.6 s), 906 pass entries (365 top-level), zero failures,
  zero skips. It contains `TestAnchorHistoryAdoptionKeepsLocalRevocationDenied`
  in its final form: `/revoked-by-old-primary` (1.33 s) refuses the promoted
  branch with nothing changed and nothing quarantined, across a physical
  restart and a canonical successor without the tombstone, then adopts the
  canonical tombstone with the floor at the adopted generation minus one, one
  trusted key, the follower role in the promoted term and the abandoned owner in
  quarantine; `/denied-by-follower` (1.22 s) is the same for a follower's own
  denial and keeps its first receipt; `/resolution-keeping-a-branch-that-trusts-it`
  (1.08 s, new) shows `ResolveConflict` refusing to keep a remote branch that
  still trusts a key the resolving member revoked, with the conflict unchanged;
  `/revoked-by-canonical-branch` (1.01 s) is unchanged. And
  `TestAnchorHistoryAdoptionCarrierRules` (2.81 s, five subtests):
  `/pending-key-refused` (new), `/restated-receipt` (now with an adopted owner
  that carries the tombstone), `/terminal-stays-terminal`, `/full-denial-refused`
  (a full carrier with denials the adopted owner does not revoke is refused; it
  replaces the saturation subtest, whose fixture is now refused before
  saturation, and the saturation branch stays the same as in
  `anchorLocalForOwner`) and `/bound-to-something-else`. Every other test of
  the three packages, including `TestAnchorLocalInstallCannotOmitPendingDeny`,
  passes unchanged.
- `fix-mounted01` (tests uncommitted on `febbcd46`, the same content as
  `8a047222`): `^TestAnchorHistory` over `./cmd/kudurru`, `-race -count=2`:
  exit 0, 8 pass entries, 33.4 s. In its final form
  `TestAnchorHistoryRejoiningPrimaryKeepsItsRevocationUntilCanonical` (5.87 s,
  5.42 s) shows two production pulls refused with `accepted.ErrCAS` while the
  old primary stays fenced on its own image and owner, nothing quarantined,
  `dns_only` with the managed seal of its own image, emitting through the
  production output guard with one trusted key, protected HTTP closed and no
  second supervision poll; after the promoted writer records the REVOKE the
  next pull adopts it, the member follows the promoted term, publishes
  completely with one trusted key, and the abandoned owner is in quarantine.
- `fix-neg01` (store tests against `anchor_local.go` and `anchor_command.go` of
  `febbcd46` through `-overlay`): fails as it should, in the two denial flows,
  the new resolution refusal, `/pending-key-refused` and `/full-denial-refused`.
  `fix-neg02` (the mounted test against the same two files): fails at the
  first pull, which adopts the branch that trusts the revoked key.
- `fix-store01` (retained failure): the first form of the rewritten carrier
  fixture failed its own precondition, because the promoted writer's tombstone
  reached the old primary's receipt generation; the fixture now lets the old
  primary run one more generation ahead. `fix-store02` is the passing rerun of
  the anchor selection (non-verbose).
- `fix-lint01`: scoped golangci-lint v2.15.0 over `./internal/store/sqlite/...`
  and `./cmd/kudurru/...` on `06afcb73`: 0 issues. `fix-vet01`: `gofmt -l`
  empty and `go vet` clean for the store and retirement packages.

Not rerun after the fixes: `./internal/cluster/...`, `test/e2e`, the drift tests
(the fixes change no generated input) and the wider `./cmd/kudurru` selection of
`final-race02`.

What remains open. Everything here is one process: the mounted tests run two
runtimes with real member databases, an actual elector, production root
operations and the production puller, but the promoted term reaches the old
primary through `Store.ObserveAuthority` and the image through an in-process
source, not through member TLS between separate processes. That acceptance,
including the three-node conflict with `cluster resolve --offline` in managed
mode, is F28-HA-3; the store test covers the audited resolution with real
member databases. The mounted tests assert publication and the production output
guard, not UDP or TCP exchanges. Three refusals are deliberate and leave a member
fenced on its own last-good DNS until an operator replaces it with a newly
joined receiver: the returning member holds an `anchors recover` record the
current primary lacks; the current primary recovered after the split at a pure
generation the returning member had already reached (`RetainsRecoveryHistory`
compares generations of two branches there, and this package did not relax it);
and the returning member holds managed history while the current primary's
owner is still dormant. The first and the third have tests (the third with a
re-sealed image); the second is read from the code. A member that carries a
revocation refuses the canonical branch until its writer records the same one,
which follows that writer's refresh schedule, and serves its own branch
DNS-only meanwhile. If the canonical writer can no longer observe it (the root
stopped publishing the key, or the canonical branch never knew the key), the
member stays fenced and never follows that writer again; no command clears a
carried denial, and `docs/deploy/sqlite-join.md` tells the operator to replace
the member with a newly joined receiver or keep it out of service. Neither that state nor a refused adoption is
visible online: `anchors status --offline` does not print the node-local
carrier, the puller's error is not logged, and the coordinator status of a
fenced member reports only `not_follower`. Operator-visible status is
F28-GATE-4. Two further points are read from the code and have no test here. A
signed REVOKE that a fenced member verifies while the adoption commits still
closes anchor admission for that process, as it already did for any install
(D297); after a restart the supervisor polls again. A follower with a pending local denial that is
promoted keeps its own owner, which still trusts the key; its writer cannot
commit an observation that omits the REVOKE (`original owner changed`, the
reviewer's `TestReviewProbePendingDenialMemberPromoted`). That is promotion,
not adoption, and unchanged by this package. The opt-in
pinned dnsdist edge test was not rerun, and the full `./cmd/kudurru` package was
not run (the integrator runs `hack/go-test.py`).
