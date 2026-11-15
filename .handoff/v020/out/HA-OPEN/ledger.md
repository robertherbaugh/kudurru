### HA-OPEN Open items of the merged follower packages — November 11, 2026 (D368)

This package closes, tests or rebuts the open items that the HA-REFUSAL and
HA-AUTH-SETTINGS reviews left on the merged follower contract (D368). The
HA-REFUSAL fix had never been verified; it was re-read against the merged
tree and its open points were rechecked through the real seams.

What now happens:

- A member fenced by data that diverges from its primary's at the same
  revision still answers `503 cluster_conflict` with the offline hint, but the
  message and the CLI hint now name the divergence instead of "conflicting
  promotions" (`TestCaptureRefusalNamesFencedAndConflictedAuthority/divergence`).
- The block page on a follower, fenced or conflicted node keeps a pending or
  declined request's status sentence, without the form and without the
  invitation to add details or ask again, and adds why the form is missing
  (`TestRequestFormOnANodeThatCannotStoreRequests`, which failed without the
  change, `light04`).
- A blocking switch through a follower whose view of `Kudurru/default` is older
  than the primary's is now a retained `409 conflict` on the primary that the
  follower relays (after the same current check as an object write), instead of
  an aborted exchange answered as `503 unavailable` "authentication operation
  unavailable". A switch that was not sent, or whose exchange failed, answers
  with the object write's `unavailable` and `completion_unknown` messages.
- A forwarded member command whose dial to the primary failed (the connection
  was refused or the host was reported unreachable) is counted as not sent:
  `replica.Client.Write` returns unavailable without `ErrOutcomeUnknown`, and
  the follower answers that the change was not applied. A dial that is still
  pending when the exchange's deadline ends (a host that silently drops
  packets) surfaces as the context's deadline, not as a dial error, and stays
  `completion_unknown`, as does every failure after a connection exists (TLS,
  dropped connection, lost response). This applies to every forwarded domain
  (object writes, zone commands, login, sign-out, settings, blocking).
- The promote-and-rejoin process scenario now restarts the former primary
  while it cannot reach the new primary's internal listener (a second relay in
  front of the follower), and asserts `503 cluster_fenced` naming the newer
  primary, the hint and no `Retry-After` for a read and a write, that it keeps
  answering DNS from its last valid data, and, after it rejoins, that a
  settings patch sent through it is forwarded and committed and that the write
  it refused while fenced exists on neither node.

Evidence under `.context/v020-final-20261110/evidence/HA-OPEN/`:

- `light05`: `TestWriteToAnUndialablePeerIsNotSent` and the replica command
  tests pass; `light06` (retained, expected failure) shows the same test failing
  without the change ("write outcome unknown ... connection refused").
- `fix-01`: `TestWriteWhoseDialRunsIntoTheDeadlineStaysUnknown` (new, Linux;
  a listener with backlog 0 and a full accept queue, client timeout 2 s) and
  `TestWriteToAnUndialablePeerIsNotSent` pass (3.08 s, 0.25 s). The new test
  pins the documented limit: a dial that runs into the deadline answers
  `ErrOutcomeUnknown`. The reviewer's `review-01` showed this case; the docs
  had promised `unavailable` for it and were narrowed (dfd6bf48).
- `fix-02` (`-race -count=1 -v -run TestRootMemberForward ./cmd/kudurru`):
  13 of 13 passed, 86.5 s, including the new
  `TestRootMemberForwardUndialablePrimaryAnswersNotApplied` (2.27 s): with the
  primary's member listener stopped, the blocking switch, `POST /clientgroups`,
  `PATCH /settings` and `POST /auth/logout` each answer `503 unavailable` with
  "was not applied" ("was not revoked" for the sign-out) and nothing reaches
  the primary. `fix-04` (retained, expected failure): the same test with
  `internal/cluster/replica/client.go` overlaid by its 68a709e2 version fails;
  all four answered `completion_unknown`.
- `race02` (retained, expected failure): without the primary-side change the
  stale-version command aborted the exchange (`EOF` on HTTP/1, `INTERNAL_ERROR`
  on HTTP/2) and the follower answered `503 unavailable` "authentication
  operation unavailable".
- `light07`, `light08`: `go test -race -count=2 ./internal/cluster/replica
  ./internal/servinghttp ./internal/blockpage`: ok (50.4 s, 30.7 s, 5.3 s).
- `race06` (`-race -count=2`, the root follower, member command, sign-out,
  settings, closed-member, blocking, CLI login and cluster-status selection):
  every test passed except `TestServingBlockingWriterCanceledAfterCommitBeforeObservation`
  (teardown error `serving_runtime_test.go:140: providerresources: owner
  closed`). The same test failed the same way in the earlier `race04` run. The
  reviewer's runs show that it fails in most runs, in both subtests, and on the
  base commit as well: `review-02` (this selection without `-skip`, `-race
  -count=2`) failed it in both iterations (`helper` twice, `blocking` once)
  with nothing else failing, and `review-06` (the package's sources replaced by
  their 68a709e2 versions through `-overlay`, `-count=4`) failed it in 3 of 4
  iterations with the same error. It is a fixture
  teardown order defect unrelated to this package: the test's extra
  `providercoordinator` shares the fixture's provider resources and its
  cleanup, which runs first, closes them before `runServingFixture` stops the
  always-on runtime, whose `Run` then returns `ErrClosed`. This became visible
  after 7f351615 kept the always-on runtime alive past `t.Context()`. Another
  lane (CI-RED) is running that test in a loop; it is not changed here.
  `race04` also retained an earlier failure of `TestRootMemberBlockingCommandActualHTTP`
  ("replay or refusals changed state"): the retained 409 adds a dedup record,
  so the test now compares the generation after the exact replay and again
  after the stale refusal, and the remaining refusals against the latter.
- `race07` (final tree, `-v -race -count=2`, the same selection with
  `-skip '^TestServingBlockingWriterCanceledAfterCommitBeforeObservation$'`
  on the command line): exit 0, 46 tests x 2 = 92 top-level passes, no
  `t.Skip`, 281.8 s. The 92-pass figure exists only because `-skip` excluded
  that test; without it the selection is not green (`review-02`). It includes
  `TestRootMemberBlockingCommandActualHTTP`,
  `TestRootMemberForwardBlockingStaleViewAndFailedExchange`,
  `TestRootMemberForwardSettingsBackToBackActualHTTP`,
  `TestRootMemberForwardSignOutFailuresActualHTTP`,
  `TestRootMemberClosedMemberProviderLoginNamesDurableState`,
  `TestRootMemberFollowerRefusesUnforwardedMutations` and the other
  `TestServingBlocking*` tests.
- `e2e01` (retained failure) and `e2e02`: the immediate refusal of a
  conflicted member (no capture wait) was built once more. With it,
  `TestEnrolledProcessConflictResolution` failed: both conflicted members
  answered `SERVFAIL` for their own authoritative data while readiness said
  "complete serving generation unavailable: preparing". A 200 ms probe loop
  (`e2e02`, source in `e2e02-probe-source.txt`, not committed) showed the window
  closing within one probe interval. The shortcut was reverted again; the
  capture wait stays, as D368 records.
- `e2e03` (retained failure): the first version of the fenced assertion checked
  the refused write's DNS answer on the new primary once, right after the
  forwarded settings patch, and the check failed once; `e2e05` passed all three
  scenarios with a waited check that logged one differing first answer
  (logged answers afterwards were authoritative NXDOMAIN). The check now logs
  every differing answer as received and fails if it never converges.
- `e2e06` (final tree, `KUDURRU_E2E=1 -race`): 3 of 3 passed in 311.3 s:
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` 108.1 s,
  `TestEnrolledClusterProcessesPromoteAndRejoin` 129.0 s (with the new fenced,
  rejoined-settings and refused-write checks),
  `TestEnrolledProcessConflictResolution` 66.1 s. The one differing DNS answer
  logged this time was `read udp ...: i/o timeout` on the new primary right
  after the forwarded settings patch, so the earlier one-shot failures were a
  query that got no answer within 3 seconds, not a wrong answer. This ran while
  the machine carried several other race runs; whether a settings commit can
  stall a primary's UDP answers is not established and is recorded below.
- `fix-03` (`KUDURRU_E2E=1 -race -v`, promote-and-rejoin only): passed,
  136.1 s, with one more unanswered UDP query on `enrolled-b` (the new
  primary). `fix-05` (the same scenario with the nodes' log level temporarily
  set to debug, not committed): passed, 134.7 s, and established the cause:
  `enrolled-b` logged `settings applied` at 21:33:20.454 and a coalesced
  republication right after, then `write failed err="serving: dependency
  changed" proto=udp` at 21:33:20.570, exactly 3 s before the test's client
  logged its timeout at 21:33:23.570. The query's answer was resolved from the
  generation that the republication replaced, the guarded emission refused to
  write it, and UDP sends nothing in that case, so the client got no answer
  until it asked again. The check's comment now names this (002ac941).
- `light09`: `TestOpenAPIDrift` and the `internal/api` follower tests pass (no
  route, DTO or schema changed, so there is no `chore(gen)` commit).
- `lint01`: one staticcheck finding (SA4006, an unused generation read in the
  member command test), fixed. `lint02` on the final tree (`./cmd/kudurru/... ./internal/cluster/replica/... ./internal/servinghttp/... ./internal/blockpage/... ./test/e2e/...`): 0 issues. `fix-06` after the review fixes (`./cmd/kudurru/... ./internal/cluster/replica/... ./test/e2e/...`): 0 issues.

HA-OPEN review (accept-with-fixes) resolved: the timed-out-dial overclaim
(docs, changelog and D368 row narrowed; pinned by a test, dfd6bf48), the
mounted "not applied" proof for four domains (11dbaf9c), the unanswered UDP
query (cause established and recorded, 002ac941; the DNS-path change is the
follow-up below) and the flaky test's wording (above).

Review items resolved: HA-REFUSAL finding 7 (divergence message, a8545edc),
finding 8 (block page status, b82ca593), finding 3 (process-level
`cluster_fenced`, now in the promote-and-rejoin scenario). Findings 1, 4 and 5
were fixed by the HA-REFUSAL fix and are covered by tests that passed again in
`race06` (`TestRootMemberClosedMemberProviderLoginNamesDurableState`,
`TestCLILogin*`, `TestRootMemberFollower*`). Finding 2 (capture wait) stays
rebutted with the evidence above. Finding 6 (a commit message) is for the
integrator. HA-AUTH-SETTINGS fixer notes resolved: the unreachable-primary
sign-out answer (`unavailable` when nothing could be dialed), the generic `503`
for a stale blocking switch, and forwarded settings on the rejoined old
primary, now asserted.

The guards that the reviewers listed as changed were each rechecked and are
changes by design, not weakenings: the follower cache flush test now expects a
local flush (FWD-NODELOCAL) and asserts the image unchanged and a credential-less
request refused; the cluster-status row gained the ROLE column; a closed member
answers without `Retry-After` (G21-02, opt-in `NoRetryAfter`); the follower
schema test checks node keys read-only and cluster keys as on a non-follower
because `PATCH /settings` is forwarded; the classification counts moved with
the four newly forwarded operations; and the `primary_required` assertions for
`PATCH /settings` were replaced by `POST /allowlist` (still primary-only), with
the forwarded settings patch now asserted positively on the partitioned
follower's rejoin and on the rejoined former primary.

Open: a conflicted member keeps the two-second capture wait because the DNS
publication window it hides must be closed in the publication path first; the
window was not investigated further here. The process tests' sign-out failure
checks and the last assertion of `TestRootMemberForwardBlockingStaleViewAndFailedExchange` still accept either `unavailable` or `completion_unknown`, because the
relay's partition accepts connections and drops them (a connection exists) and a stopped writer behind a live listener may fail after dispatch; the unavailable answer itself is now asserted exactly by `TestRootMemberForwardUndialablePrimaryAnswersNotApplied`. The blocking switch answers with the object write's `unavailable` and `completion_unknown` texts, which name a write rather than the switch; that is intended (D368: "as for an object write").
A UDP query to the new primary right after a forwarded settings patch went unanswered for 3 seconds in four scenario runs (`e2e03`, `e2e06`, `review-03`, `fix-03`). The cause is established (`fix-05`): a UDP query whose answer was resolved from a serving generation that a republication replaces before the write is dropped silently by the guarded emission (`internal/dnsserver/native_capture.go`, `serving.ErrDependency`), so the client sees a timeout instead of an answer. It does not read the store, but every settings or data commit can drop the UDP answers in flight. It is not caused by this package and is not changed here. Proposed follow-up for `dev-docs/followups.md`: on `ErrDependency` at write time, resolve the query once more against the current generation (bounded by the query deadline) or answer `SERVFAIL`, instead of sending nothing over UDP; then tighten the refused-fenced check in `test/e2e/enrolled_cluster_test.go` back to a short bounded assertion. `TestServingBlockingWriterCanceledAfterCommitBeforeObservation` fails in most runs, in both subtests and on the base commit too, on teardown for the fixture reason above; the lane that owns the fixture teardown order (CI-RED) must land its fix before the integrator's full `./cmd/kudurru` run. The full `./cmd/kudurru` package was not run (only selections). The G21-02
dashboard banner remains for the dashboard lane.
