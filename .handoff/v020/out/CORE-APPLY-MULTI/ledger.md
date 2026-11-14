### CORE-APPLY-MULTI one request applies every document on SQLite — November 11, 2026 (D378)

On the complete SQLite runtime a request that carries several items applied
only its first new or changed item. `POST /apply` and `POST
/{kind}:batchDelete` run one current command for each item, a current command
is admitted only while the request's original lease is the current
publication, and the first commit retires that lease. Every later item
answered `unavailable` ("current writable serving generation unavailable"): a
three-document `kudurru apply`, `kudurru apply -f a -f b -f c`, a restore into
a new node and the dashboard's multi-delete each needed one run for each
object. A standalone node and the primary of a cluster were affected alike.

One request now applies every item. Both routes declare an item sequence
(`api.Options.ObjectItems`). Root gives the request one command chain of the
provider coordinator and binds it to the elected writer episode of its first
item. Until one of the request's commands has completed, each item is admitted
on the original lease exactly as a single write is. The first completed
command anchors the chain: on its own commit receipt, which the Store now
issues in the command's finalizer under the writer lock
(`sqlite.CommandReceipt`), or on the coordinator's current mark when the
command changed nothing. Each later item is admitted under the publication
gate only while the coordinator's mark of the last command that changed
canonical state is still the chain's anchor, compared by identity, and while
the Store's authority is the lease's own authority advanced only in its
installed revision (`GenerationToken.AdvancedTo`,
`Publisher.CheckWriterAdvanced`). Every item still reads the raw credential,
grants, ownership, preconditions and limits again in its own transaction.

A foreign commit is a state-changing command of anyone else through the
coordinator: a public API mutation under any credential, including a sign-in,
a write forwarded from a follower, an operator-started integration operation, a
migration step or the unblock-request sweep. A command whose SQL outcome is
unknown counts as one. After a foreign commit every later item of the request
answers `unavailable` ("another change was committed on this node while the
request ran; this item was not applied; review current state before a new
decision"), is not applied and is not tried again; earlier items stay
committed. Writers outside the coordinator, which take only the Store lock or
the primary pin, are not detected and do not stop a request: the node's own
follow-up work (status reconciliation, the zone lifecycle and its heads, list
fetch status, integration reconciliation, the RoleBinding objects source, the
login-lane request charge and provider authority admission) and two writers
that act for another party, RFC 2136 updates under a TSIG key and commands of
the local admin socket. D378 accepts this: an item reads everything it is
admitted on again in its own transaction. Replacements of node-local dependency
installations between two items do not stop a request either. The request's
one lifetime bounds all items, and an item reached after it ended answers
`unavailable` with that cause instead of "current credential is invalid or
expired". The single-command check (`CheckWriter`, `CheckWriterLease`) is
unchanged for every other route and for the first item.

Every commit retires the publication that local DNS answers and every other
API request are bound to, until the node has published again, and the items
of one request commit back to back. Unpaced, one request kept the node without
a current publication for its whole duration (review probe: 27.5 s without a
correct local answer during one 300-document apply on a loaded machine, and
`503 generation_pending` for a second CLI). Once a request's items have run
for two seconds since the node last had a current publication for it, the next
item now first waits, inside the request's lifetime and writer episode and for
at most two seconds, until the publisher is current again
(`Publisher.WaitTerm`), and then commits nothing for 200 ms. The wait captures
no lease; the chain's compare still decides, so a command of another caller
committed in the pause stops every later item. A wait that reaches its bound
goes on without the publication and without the rest, so the pace makes a
request at most about twice as slow and cannot by itself run it out of its
lifetime. `kudurru apply` and `kudurru restore` now default
`--request-timeout` to the server's five minutes. The dashboard's multi-select
delete sends one batch delete per namespace one after another instead of all
at once, because of two concurrent multi-item requests one is refused as a
whole.

A follower is unchanged: it forwards each item as its own member command,
never uses the chain and stops when its installed source changes (the
forwarding lane's decision, D375, with two statements of its reasoning
corrected in `out/CORE-APPLY-MULTI/decision-D375.md`). The Kubernetes driver
has no such gate and was not affected; it now has the same test.
`docs/deploy/sqlite-cluster.md` section 4 and `docs/deploy/sqlite-join.md` no
longer contradict each other about a file that is cut through a follower:
repeating it there does not finish it, because the primary commits a
deduplication record for every forwarded command, also for an unchanged
document. `docs/deploy/api-contracts.md` and
`docs/architecture/configuration-and-store.md` ("Requests with several items",
with a diagram) describe the rule.

Commits on `wip/v020/core`: `71c3249c` (Store receipt), `3111477d` (advanced
authority compare), `2b351dea` (coordinator chain), `14608688` (the fix and
its root and mounted tests), `94ab1e7f` (Kubernetes and two-process tests),
`4ef8f537` and `da930f26` (documentation), `adb5df10` (forwarded-write tests
and the quiet wait of the mounted tests), and the generated commit `a3af56bf`. Review fixes: `9c38431c` (dashboard
batch deletes one after another), `663e6150` (the pace and its root and
mounted tests), `4e40af6e` (five-minute CLI default for apply and restore),
`af6347b3` (documentation), the generated commit `4e7e7b97` (CLI
reference) and `02089768` (a lint fix in a test, source only, committed after
the generated commit).

Evidence under `.context/v020-final-20261110/evidence/CORE-APPLY-MULTI/`:

- `race01-repro-base` (retained failure, the defect on the unmodified base):
  `repro-base.sh` returns every changed Go file to commit `b4987f32` with a
  build overlay and adds only the reproduction test.
  `TestRunServeSQLiteMultiDocumentApplyCLI` on a mounted `runServe` with the
  real CLI fails in 13.16 s: `dnsrecordset/default/x0 created`, then `x1` and
  `x2` refused with "current writable serving generation unavailable".
- `race02-first-fix` (retained failure of the first design): the mounted CLI
  apply passes, and `TestRunServeSQLiteMultiFileApplyAndRestoreIntoNewNode`
  fails: the restore into a new node creates `dnszone/multi` and refuses the
  other seven documents. That version still required the original lease's
  dependency installations for later items. `race04-diag-zone-first` (an
  overlay that prints the refusing check, not committed) shows the cause: the
  provider resource set of the retired lease is replaced by the node while
  the request runs ("provider not current"), which cut the restore at its
  fifth of eight documents in that run. Later items no longer compare those
  installations; each command pins the current ones.
- `race03-libraries`: default race of `internal/store/sqlite` (422.9 s),
  `internal/auth/providercoordinator` (70.8 s), `internal/serving` (6.3 s) and
  `internal/servinghttp` (13.4 s): all pass. They include
  `TestCommandReceiptStatesExactDisposition`,
  `TestCommandReceiptServesOneCommand`,
  `TestServingTokenAdvancedToComparesEveryOtherFieldExactly`,
  `TestHTTPWriterAdvancedKeepsOriginalAuthorityAcrossCommits`,
  `TestChainFollowsOwnReceiptsAndRefusesAForeignCommand`,
  `TestChainAnchorsOnItsFirstCompletedCommand`,
  `TestChainRefusesAnotherCoordinatorAndReentry` and
  `TestChainTreatsOutputAndMemberCommandsAsForeign`. In the same log
  `internal/api` fails only `TestOpenAPIDrift`; `drift01-base` shows that this
  drift and the `TestReferenceDrift` failure exist on the unmodified base.
  `race08-api`: default race of `internal/api` after regeneration, 267.0 s,
  pass.
- `race05-root` (125.4 s) and `race05-e2e` (275.2 s), first complete pass:
  the mounted CLI apply (39.0 s), the mounted multi-file apply and restore
  (56.9 s), the Kubernetes test (7.9 s), eight root item tests, and the
  two-process tests `TestEnrolledClusterProcessesPartitionCatchupAndRestart`
  (121.6 s) and `TestEnrolledClusterProcessesPromoteAndRejoin` (125.5 s).
- `race06-gen`, `race06-regression`, `race06-drift`, `race06-lint`:
  regeneration of the artifacts that were stale at the base (`make openapi`,
  `make client-go`, `bun run gen:api`, `make docs-gen`: five files), then the
  root regression selection in default race: 174 tests pass, none fails, none
  skips, 1230.2 s. It covers the current-command, object, zone, domain,
  source-entry, unblock, blocking, migration, adoption, cutover, member-command
  and member-forward tests, `TestServingHTTP*`, five existing mounted SQLite
  command tests, the real-router and generated-transport CLI tests,
  `TestCLIReferenceDrift`, `TestCompleteCommandRouteFamiliesAreReviewed` and
  the new tests. `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass after regeneration. The lint run reported two
  gosec findings in the new tests (a test-owned file read and an integer
  conversion), which are fixed.
- `race07-root` (268.5 s), `race07-e2e` (260.9 s) and `race07-lint`, the
  final verification of the content that is committed (`da930f26` plus the
  test changes of `adb5df10` and the regenerated files, all committed
  unchanged afterwards): the 13 new tests of the root package and
  `TestCLIReferenceDrift`, each twice, 28 passes and no skip: nine
  `TestServingItems*` tests, `TestRootMemberUnchangedForwardedWriteStillCommits`,
  the mounted CLI apply (35.0 s and 38.3 s), the mounted multi-file apply and
  restore (58.5 s and 67.0 s) and the Kubernetes test (7.3 s and 7.6 s). No
  run needed the repeat of a request that applied nothing. The two-process
  tests pass in 117.5 s and 123.9 s. Scoped golangci-lint v2.15.0 over the
  seven touched package trees: 0 issues.
- `final01-clean-tree`: on the clean committed tree `a3af56bf`: `gofmt -l`
  empty for every changed Go file, `go build ./...`, `CGO_ENABLED=0 go build
  ./cmd/kudurru`, `go vet` of the six library and test packages, and
  `TestOpenAPIDrift`, `TestReferenceDrift` and `TestAggregatedRolesDrift` pass.
  `go.mod` and `go.sum` are unchanged; there is no new dependency.
  `race09-duration-compare` also runs `TestCLIReferenceDrift` on that tree.
- `race09-duration-compare` and `race10-duration-compare`:
  `TestServingZoneCommandActualHTTPCompleteSQLite`, an existing test that does
  not use the new path, took 183.9 s in the regression run. The two logs run
  it from a binary of the unmodified base (build overlay) and a binary of the
  committed tree, alternating, on the loaded machine (load average 5 to 11 on
  8 cores). `race09` (retained failure): base 167 s and 178 s, tree 112 s, then
  the tree's second run fails after its last request waited the test's whole
  two-minute budget for publication; that log kept only the last lines.
  `race10`, three more rounds with full output: tree 176.6 s, 218.8 s and
  67.1 s, base 199.8 s, 218.5 s and 232.1 s, all six pass, and the longest
  wait at a single request is 80 s for the tree and 80 s for the base. The
  test's duration follows the machine's load on both trees and its budget is
  within reach on this machine without this change.
- Review fixes (`fix-*`). `fix-01-before` (retained failure): with the pace
  call disabled, `TestServingItemsLetTheNodePublishBetweenItems` fails (item
  1 began without a current publication) and
  `TestServingItemsPaceEndsWithTheRequestLifetime` fails (items applied after
  the lifetime). `fix-03-mounted-long` and `fix-08-race-items-mounted`
  (retained failures of an unbounded wait): under the race detector one
  apply of 300 and then of 120 documents did not answer within five minutes;
  `fix-04-diag-pace` (diagnostic overlay) shows single waits of 1.9 s to
  9.5 s for a publication under that load. `fix-06-norace-paced` and
  `fix-07-norace-unpaced` (no race detector, 150 documents): with a
  half-second budget 26.3 s and the longest time without a local answer
  3.3 s; unpaced 3.9 s, no correct local answer and no read during the whole
  request (the new mounted test fails). `fix-15-bound-before` (retained
  failure): with the wait's bound disabled `TestServingItemsPaceWaitIsBounded`
  fails after 120 s. `fix-09-race-bounded-wait`: with the bound, race, the
  mounted 120-document apply passes in 92.8 s (10 local answers, longest gap
  84.0 s, 3 reads ok and 43 refused) and the three pace tests pass.
  `fix-10-norace-bounded` (no race detector, twice): 120 documents in 4.0 s
  and 4.9 s, longest gap 4.0 s and 2.4 s. `fix-05b-dashboard`: dashboard
  typecheck and lint pass for the sequential batch deletes. `fix-11-docs-gen`:
  `make docs-gen` changes only `docs-site/content/reference/cli.md`.
  `fix-12-race-final` (race, on `4e7e7b97`): 19 tests pass, none fails or
  skips, 241.6 s: twelve `TestServingItems*` tests,
  `TestRootMemberUnchangedForwardedWriteStillCommits`, the three mounted
  SQLite tests (the long apply 115.9 s: 120 documents in 82.5 s, 14 local
  answers, longest gap 73.6 s), the Kubernetes test,
  `TestApplyAndRestoreDefaultToTheRequestLifetime` and
  `TestCLIReferenceDrift`. `fix-13-lint` found one revive issue in a new test,
  fixed in `02089768`; `fix-16-lint`, the rerun on `02089768`: 0 issues. `fix-14-drift` (on
  `02089768`): `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass, `gofmt -l` is empty for every changed Go
  file, `CGO_ENABLED=0 go build ./cmd/kudurru` passes, `go.mod` and `go.sum`
  are unchanged.
- `mermaid01`: the 29 Mermaid blocks of
  `docs/architecture/configuration-and-store.md`, including the new one, parse
  with the vendored bundle (the architecture lane's checker).

The acceptance tests, by criterion: (a) `TestRunServeSQLiteMultiDocumentApplyCLI`
(mounted `runServe`, real router, real CLI: three created in one request,
served over DNS, three `unchanged` on the second run); (b)
`TestRunServeSQLiteMultiFileApplyAndRestoreIntoNewNode` (`apply -f a -f b -f
c`, then `backup` and a `restore` of eight documents into a new node in one
run, served over DNS, eight `unchanged` on the second run); (c) the batch
delete in (a), `TestServingItemsBatchDeleteFollowsOwnCommits` and the
two-process test; (d) `TestServingItemsForeignCommandRefusesLaterItems`
(another administrator's write through the real router),
`TestServingItemsForwardedFollowerWriteRefusesLaterItems` (a member command
over the production member listener) and
`TestServingItemsFirstItemKeepsTheOriginalSource`, with
`TestServingItemsApplyFollowsOwnCommitsAcrossFollowUpWrites` and
`TestServingItemsReplacedDependencyKeepsLaterItems` for the writes that do not
count; each asserts that a refused item is not stored and that every item ran
its command once; (e) `TestServingItemsOriginalLifetimeBoundsEveryItem`; (f)
`TestServingItemsRevokedOrDowngradedCredentialStopsLaterItems`; (g) the
two-process test applies five new record sets in one request on the primary
while a follower pulls, repeats them for five `unchanged` results and deletes
them in one batch delete, next to the forwarding lane's existing two-document
applies and batch delete through the follower; (h)
`TestRunServeKubeMultiDocumentApplyAndBatchDelete` on a mounted Kubernetes
`runServe` with envtest.

What remains open. The root cause of the pause is outside this package: every
commit withholds local answers and refuses other requests until the next
publication (`cmd/kudurru/serving_update_output.go`), also for a run of single
writes on the unmodified base (review probe: 60 single applies left 62 of 75
queries unanswered); it belongs to the lane that owns the retained-response
guard. The pace only bounds it: on a node that needs more than two seconds to
publish (a large data set or a loaded machine) the waits end at their bound
and local answers stay withheld for most of the request (84 s of a 93 s
request under the race detector), and other callers on the node are answered
`generation_pending` while the items commit. A request is still held to the
publication it began on until its first item, as every single write is: when
the node's own work lands between the request's capture and that item, the
whole request is refused with "current writable serving generation
unavailable" before anything is applied, and it has to be sent again; of two
multi-item requests sent at once one usually ends this way (the dashboard now
sends its per-namespace requests one after another). Writers outside the
coordinator are not detected, including RFC 2136 updates and the admin socket,
which act for someone else (D378 accepts this), and no test puts one of them
between two items. The mounted CLI tests wait for a quiet node and repeat only
a request that applied nothing, up to five times, and now log each repeat as
`repeated=<n>` (0 in every fix run). Through a follower a file that does not
fit between two pulls is still cut, and repeating it there does not finish
it; chaining a follower on the primary's receipts needs receipts and
preconditions in the member protocol and was not attempted. Criterion (g) is
met by the forwarding lane's two-document applies and batch delete through
the follower in the two-process test plus this package's five-document
requests on the primary; no process-level test sends a file longer than one
pull interval through a follower and asserts the cut. The foreign-commit
refusal is proven in the root fixture only. The mark for a command whose SQL
outcome is unknown has no test, because no seam produces a failed commit.
`TestServingZoneCommandActualHTTPCompleteSQLite` failed once in six runs of
this lane's tree on the loaded machine (`race09`) by running out of its own
two-minute wait for publication; the base binary waits up to 80 s at a single
request under the same load, so the test is sensitive to load, and it is not
changed here. Watch it in the integrator's full run; if it fails there and not
on the base, bisect across `2b351dea`.
