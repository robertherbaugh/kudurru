### G21-01 Kubernetes forwarded auth and IAM routes answer the envelope — November 10, 2026 (decision row pending a number)

On the Kubernetes driver the API forwards its `/auth` and `/iam` routes to the
legacy authentication stack after its own admission. Six registered routes had
no pattern on that mux and answered Go's plain-text `404 page not found`:
`GET /iam/rolebindings/{namespace}/{name}`, `GET /auth/cli/providers`,
`POST /auth/cli/{idp}/prepare`, `POST /auth/cli/{idp}/verify`,
`GET /auth/oidc/{idp}/logout` and `GET /auth/oidc/{idp}/logout/callback`. The
map found this by reading; a mounted envtest run confirmed all six and no
seventh.

The stack now serves the RoleBinding item read from the cache that backs the
list: the stored object with `apiVersion`, `kind`, UID and resourceVersion,
`404 not_found` for an absent object and `400 invalid` for a target that is not
a namespace label and object name (a config-file binding is not an object and
stays list-only). Public CLI login answers `503 cli_login_unavailable` and
provider logout continuation `503 provider_logout_unavailable`, each with a
hint, because both need the captured provider resources of the complete SQLite
runtime. A last fallback on the legacy mux answers `503 unavailable` with the
hint "requires the complete SQLite runtime" for any forwarded route without a
handler. `runServe` and the contract harness build the mux through one
function, `legacyAuthHandler`. `kudurru login` in its browser and device modes
keeps the `cli_login_unavailable` key through the safe-error filter and prints
a fixed client message that names `--token` and `--username`; no server text is
retained and no context is saved.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `race01-g2101-prefix`, `race02-g2101-prefix-inventory` (retained failures
  before the fix): envtest `TestRunServeKubeForwardedAuthRoutesAnswerEnvelope`
  fails; the inventory lists exactly the six routes with
  `status=404 content-type="text/plain; charset=utf-8"`.
- `race03-g2101-fixed`: the same test passes in 8.97 s on a mounted Kubernetes
  `runServe`. It reads the RoleBinding item (UID equal to the object created
  through the API server), checks six refused or absent targets, checks the five
  keyed refusals with hints, runs `kudurru login` and `kudurru login --device`
  through a verified-TLS front and matches the printed refusal, and requests all
  39 `auth` and `iam` operations of the served OpenAPI document with an
  administrator session, asserting that every answer of 400 or above is the JSON
  envelope with a key. `TestLegacyAuthHandlerFallbackAndRefusals` covers the
  fallback and its precedence.
- `race04-g2101-stack-apiclient`: default race of `internal/auth/stack`
  (10.4 s, includes the new `TestIAMRoleBindingItemRead`) and
  `internal/apiclient` (1.7 s, includes
  `TestCLILoginProvidersKeepsUnavailableKeyWithoutServerText`).
- `lint01-g2101`: scoped golangci-lint, 0 issues.
- `race11-root-regression` (final tree with regenerated artifacts): default race
  selection of 129 root tests around the three changes (Kubernetes mounts, the
  contract harness, CLI login, settings, source-entry, domain, migration,
  adoption, cutover, offline import, TSIG, unblock, zone and notify commands,
  member commands and forwards), 604.6 s: 127 passes, one skip
  (`TestCLIOIDCBrowserInstalledChromeActualTLS`, needs an installed browser) and
  one failure. `TestServingAdoptionActualHTTPFinalExpiryRollsBackAllZones`
  failed with a race-detector report between the test's own replacement of
  `opts.Now` (`serving_migration_adopt_test.go:375`) and the integration host's
  background refresh reading it (`integration_runtime.go:146`). Neither access
  is touched by this package, whose only edit in that file removes the gate
  wrapper from the test server. `race12-adoption-clock-race-head-x20` repeats
  the test 20 times at the same tree: 20 passes, 58.8 s. The race is a rare
  defect of that test and stays open. `TestForwardedShapes` now builds its mux
  through `legacyAuthHandler` and decodes the item read strictly into
  `api.RoleBindingDocument`.

What remains open: public CLI login, provider logout continuation, session
inventory, TOTP, external-account administration, API user commands and
RoleBinding writes stay SQLite-only on Kubernetes; each has a keyed refusal, and
the capability matrix and UI gating are G21-02/G21-13 work. The CLI recognizes
the refusal at the provider list, which both login modes read first; it does not
special-case the prepare and verify routes.

### G21-05 contract cleanups: vestigial 503 gate, declared 501, explained refusals — November 10, 2026

The complete SQLite runtime no longer wraps the API in `servingMutationRoutes`.
That gate answered `503 current_command_unavailable` for every unsafe request
under `/zones`, `/unblockrequests` and BlockList source entries that was not on
its list of converted commands. All nine registered unsafe routes of those
families are complete current commands, so only paths that are not routes still
hit it. Such a path now gets the router's own `404 not_found`
("no route METHOD path"). `TestCompleteCommandRouteFamiliesAreReviewed` keeps
the review at test time: it derives the unsafe routes of the three families from
the real API server and fails when one is added or removed without updating the
reviewed table. The map's acceptance text expected `405`; the router has always
answered `404 not_found` for any unmatched method and path
(`TestUnknownAPIPathIsEnvelope`), and this package did not change that contract.

`PATCH /settings` and `PATCH /node/config` declare the `501 read_only` answer
that a server without a node configuration writer (Kubernetes) already gave for
node-scope changes. `POST /settings/sections/{section}/ownership/{action}` and
`.../yaml` on canonical SQLite settings keep `503 unavailable` and now carry a
hint that names `kudurru settings set` and `kudurru settings get`. The
defensive stored-TSIG refusal says "not available on this node" instead of
blaming the storage driver, since both drivers mount management. The
dashboard's Authenticator panel shows "Authenticator management is unavailable
on this server" for `factor_management_unavailable`.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `race05-g2105-prefix` (retained failure before the fix): mounted SQLite
  `TestRunServeSQLiteUnregisteredMutationsAndOwnershipRefusals` shows ten
  unregistered unsafe paths answering `503 current_command_unavailable`, three
  ownership refusals without a hint and the three CLI texts without a hint.
- `race06-g2105-fixed`: 28 passes in 92.0 s: the mounted test (router `404` for
  the ten paths, a registered zone deletion still reaching its command, exact
  hints, and the exact CLI text of `kudurru settings adopt|release|yaml`),
  the route-family review test, and the source-entry, domain, migration and
  stored-TSIG command tests whose harnesses used the gate.
- `dashboard01-authenticators`: vitest for `Authenticators.test.tsx` and
  `Sessions.test.tsx`, 55 passes, with `tsc --noEmit` and scoped eslint clean.
- `gen01`, `drift01`: `make client-go` (with `make openapi`), `bun run gen:api`
  and `make docs-gen` change `api/openapi.json`, `api/openapi-3.0.json`,
  `internal/apiclient/openapi/zz_generated.client.go` and
  `dashboard/src/api/generated.ts` (the `501` response of the two PATCH
  operations) and nothing under `docs-site/content/reference`. Afterwards
  `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestLintRoutesCatchesMissingDeclaration`, `TestOpenAPIServedAndDocs`,
  `TestUnknownAPIPathIsEnvelope`, `TestGeneratedClient`, `TestCLIReferenceDrift`,
  `TestReferenceDrift` and `TestAggregatedRolesDrift` pass, and the dashboard
  typecheck passes with the regenerated types.
- `race10-api-stack-apiclient`: default race of `internal/api/...` (187.8 s),
  `internal/apiclient/...` and `internal/auth/stack` on the regenerated tree,
  exit 0.

`TestServingMutationRoutesDomainAliasesAndSourceCommand`, the unit test of the
removed gate, is deleted with it; the route-family test replaces its purpose.
Nothing else is open in this entry. The refusal inventory across every runtime
shape (G21-11) and role-aware refusals (G21-02) are separate packages.

### FWD-BOUND follower forwarding for apply, kind routes and batch deletion — November 10, 2026 (decision row pending a number)

D362 forwards canonical object writes from an enrolled SQLite follower, and
`POST /apply`, the `POST`/`PUT`/`DELETE` routes of `/blocklists`,
`/clientgroups` and `/policies`, and `POST /{kind}:batchDelete` reach the same
forward. Capture refuses a request without a lifetime of at most 30 seconds,
and that lifetime was installed only for paths under `/objects/` and `/zones/`.
The other routes were therefore refused, with the misleading message "the
follower's source changed": kind routes with `503`, apply and batch deletion
with `200` and that error for every item. The tracker and CHANGELOG already
described `kudurru apply` as forwarded.

One predicate, `memberForwardRoute`, now names the 25 registered routes that a
follower forwards, compared segment by segment as the router does. A capture
refusal says whether the follower's source changed, the lifetime ended, the
request was canceled, or the route is not forwarded, and that the write was not
applied. Apply and batch deletion send one member command for each document or
name inside the single request lifetime and keep their per-item answers. An
item is reported with `unavailable`, not applied and never replayed when the
lifetime has ended, when the request was canceled, or when the follower
installed a newer image after the request began (its next pull, usually
carrying the request's earlier items). An item whose exchange with the primary
fails, including an unreachable primary, reports `completion_unknown`, as a
single forwarded write does. There is no document cap.

Review fixes (same day, commits `83b49fdf`, `6ddd7cef`, `8e3eeb87`, `d620b1ad`
and `8c7e4862`; the acknowledgment fix `29cdf81a` has its own entry below):

- Lifetime ended and canceled. The review showed that the documented
  `unavailable` refusal was unreachable through the router: `ObjectForward` and
  `ZoneForward` took a failed `servinghttp.SourceToken` (which refuses every
  ended context) to mean "not a follower" and fell through to the follower's
  local command, whose credential check answered `401 unauthenticated`,
  "current credential is invalid or expired". The forwards now read the mode of
  the request's original lease through the new `servinghttp.SourceMode`, which
  answers for an ended context while the handler holds its lease and returns no
  token. A request that began on a follower stays a forward and is answered with
  the capture refusal; nothing is sent and nothing runs locally.
- Route coverage test. `TestMemberForwardRouteCoversEveryForwardedRoute`
  compared the predicate with a list derived from the same predicate. It now
  requests every registered unsafe route on an API server whose
  `ObjectForward` and `ZoneForward` hooks record that they were reached, with
  the documents, name lists and zone inputs that forwarded routes take, and
  requires that the reached routes, the bounded routes and the reviewed table
  are the same 25.
- Operator page. `docs/deploy/sqlite-join.md` said both that an unreachable
  primary refuses a write with `503` and that it reports `completion_unknown`.
  The member client joins an unknown outcome to every failed write exchange, so
  the page now says `503 completion_unknown`, and `503 unavailable` for a
  primary that answers but cannot run the command (`8c7e4862` removes an
  ambiguous word from that sentence).
- Earlier bound kept (`d620b1ad`). The review listed one narrowed deadline:
  before this package a follower gave every `POST`, `PUT` and `DELETE` under
  `/objects/` and `/zones/` the 30-second lifetime, and the predicate then gave
  it only to the 25 forwarded routes. The other requests under the two
  prefixes ran with the ordinary five-minute route lifetime instead.
  `POST /zones/{zone}/notify` is the only registered one. No answer changed
  (see the probe below), but nothing justified taking the deadline away.
  `boundMemberObjectWrites` now bounds the forwarded routes and, as before,
  every other unsafe request under the two prefixes. `memberForwardRoute` is
  unchanged and still names exactly the forwarded routes, so the route test and
  the "this route is not forwarded from a follower" refusal keep their meaning.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `race07-fwdbound-prefix` (retained failure before the fix): on the follower
  fixture over actual member mTLS, `POST /apply` answers `200` with
  `errorKey: unavailable`, "the follower's source changed", for its first
  document.
- `race08-fwdbound-fixed-x5` (retained failure of a new test, not of the fix):
  `TestRootMemberForwardApplyKindRoutesAndBatchDelete` passes five of five and
  `TestMemberCaptureErrorNamesItsCause` five of five;
  `TestMemberForwardRouteCoversEveryForwardedRoute` fails five of five because
  the test compared a sorted list with an unsorted one. The two sets were equal;
  the test now sorts both.
- `race09-fwdbound-fixed-x3`: 15 passes in 23.4 s, three runs each of the route
  table test, the capture test, the route-family test and two follower tests
  (the two follower tests pass again in `race11-root-regression`).
  `TestRootMemberForwardApplyKindRoutesAndBatchDelete` applies three documents
  (zone, record set, block list) in one request, applies a refused document
  beside a committed one, creates, conflicts on and replaces a client group
  through the kind routes, creates and deletes a block list, batch-deletes a
  record set and a client group, and checks the primary's store, exactly one
  attributed audit row, no local install before the pull, and convergence of the
  follower after its pull. `TestRootMemberForwardApplyOriginalSourceCutRefusesItems`
  holds the body, lets the follower install a successor, and requires `200` with
  "the follower's source changed" for both documents and nothing on the primary.
- Real two-process runs with a relay-controlled partition. `process01-enrolled`
  and `process02-enrolled` are retained failures of the first test versions, and
  both taught something. In `process01` every new step before the partition
  passed in both scenarios (apply of two documents, `PUT` and `DELETE` of a block
  list, batch deletion, each converging in DNS or the list route on both nodes);
  the partitioned apply then reported its document with `completion_unknown`,
  not `unavailable`, because the member client classifies a failed POST exchange
  as an unknown outcome. The test now accepts either explicit key and proves by
  later DNS observation that nothing was applied or replayed. In `process02` the
  promote scenario passed (98.3 s) and the other failed before the partition:
  the second document of a two-document apply answered `completion_unknown`.
  The member server withholds its acknowledgment when the primary's authority
  advances again between a command's commit and its answer
  (`MemberRequest.CheckCommandOutput`), and the status controller's write for
  the block list created by the first document did that. It was the behavior
  of every forwarded write; a multi-item request provoked it against itself
  (fixed in the next entry). The test of that commit then applied one new
  document, waited for both nodes, and applied two documents of which the first
  was already stored (`unchanged`, `created`); the review-fix pass restored two
  new documents. `process03-enrolled-x2` (that earlier test shape): both
  scenarios pass twice (87.0, 103.5, 84.6 and 102.8 s; package 384.8 s). Each
  run covers through the follower:
  apply of one and of two documents, `PUT` (201) and `DELETE` (204) on
  `/blocklists/default/via-editor`, `POST /dnsrecordsets:batchDelete`, each
  observed on both nodes; and during the partition a kind-route `503`, an apply
  whose document is an explicit error and a batch deletion whose item is an
  explicit error, none applied or replayed after reconnection.
- `lint02-all`: `go vet` and scoped golangci-lint over `cmd/kudurru`,
  `internal/api`, `internal/auth/stack`, `internal/apiclient` and `test/e2e`,
  0 issues. `lint03-final` repeats both on the final tree: 0 issues.
- `final01-head`: at lane commit `55753c42` the tree is clean, `gofmt -l` is
  empty, `CGO_ENABLED=0 go build ./...` succeeds, `test/e2e` lints with 0
  issues after its last edit, and the drift tests pass again. The delivered
  head `0f580333` differs from it only by one corrected paragraph in
  `docs/deploy/sqlite-join.md`.
- Review-fix evidence (`fix-*` logs). `fix-race02-root-prefix-mutation`
  (retained failures before the fixes; `go test -overlay` restores the six
  changed sources to `0f580333` and removes `policies` from the predicate): on
  the follower fixture, after the body was held 31 s, `POST /apply` answers
  `200` with `errorKey: unauthenticated` for both documents, `PUT
  /clientgroups/default/late-c` and `POST /zones/late-zone/records` answer
  `401`, and `POST /clientgroups:batchDelete` reports `unauthenticated` for its
  name; a request canceled while its body is held answers `401` on a recorder;
  the route test reports "`POST /policies` reaches the follower forward but
  memberForwardRoute does not bound it" for the three policy routes.
  `fix-race05-root-fixed`: 28 root tests pass with `-race` in 124.6 s on the
  fixed tree, among them `TestRootMemberForwardAfterLifetimeEndedIsRefusedAsAForward`
  (33.3 s: `unavailable`, "30-second forwarding lifetime ended", "not applied"
  for the two apply items, the kind route `503`, the zone record command `503`
  and the batch item; the three new groups absent and the batch's group still
  present on the primary), `TestRootMemberForwardCanceledRequestIsRefusedAsAForward`
  (`503 unavailable`, "the request was canceled before this write was sent"),
  the rewritten route test, the two earlier FWD-BOUND follower tests, every
  `TestRootMember*` test, and the nine tests the reviewer selected.
  `fix-race06-root-fixed-x5`: the canceled, route and acknowledgment tests pass
  five times each (15 passes, 67.8 s). `fix-race03-light-packages`: default
  race of `internal/servinghttp` (19.5 s, includes
  `TestSourceModeAnswersForAnEndedRequestUntilRelease`).
- Earlier bound. `TestRootMemberFollowerRequestLifetimeBound` observes the
  remaining lifetime of a request where the API handler receives it, behind
  the production middleware chain of the follower and of its primary.
  `fix-race08-bound-prefix` (retained failure before the fix;
  `serving_member_objects.go` restored to `29cdf81a` through
  `go test -overlay`): on the follower, `POST /zones/example.test/notify` and
  four unregistered unsafe paths under `/zones/` and `/objects/` have
  4m59.99s instead of at most 30 s. `fix-race09-root-final` (fixed tree): 37
  root tests pass with `-race` in 179.1 s: the 28 of `fix-race05-root-fixed`,
  the new test (12 forwarded routes and 5 earlier-bounded paths at most 30 s
  on the follower; 9 other requests there, and all 26 on the primary, the
  ordinary lifetime), `TestCLIReferenceDrift`, and seven manual NOTIFY tests
  (five `TestServingZoneNotify*`, `TestRunServeSQLiteManualNotifyRestartAndRearm`
  and `TestZonesImportExportNotify`). `fix-race10-bound-fixed-x5`: the new test
  passes five times (13.1 s). `fix-race12-probe-unforwarded` (evidence only;
  `fix-probe-unforwarded_test.go.txt` supplied through `go test -overlay`): on
  the follower fixture `POST /zones/example.test/notify` answers
  `503 unavailable`, "current NOTIFY command unavailable", in 7 ms, and the
  four unregistered paths answer `404 not_found` in 1 ms. The bound therefore
  never decided an answer, with or without the fix.

What remains open for this entry: the lifetime-ended refusal is proven on the
in-process follower fixture over actual member mTLS, not in two real processes.
An apply through a follower is still cut by the follower's next pull (five
seconds by default): every later document then reports `unavailable`, "the
follower's source changed", and is not applied. That is the original-source
rule, the operator page says so, and the process test starts each apply right
after the follower converged so that it does not meet a pull. A request whose
context has ended on a primary or standalone node still gets the local
command's `401`, "current credential is invalid or expired"; that path was not
part of this package. The route test's probes cover the request shapes of
today's forwarded routes; a new route that needs a body of another shape before
it writes a canonical object reaches no hook there and still needs a review.
The earlier bound that `d620b1ad` keeps is the two-prefix rule as it was: a
future node-local route under `/objects/` or `/zones/` gets the 30-second
lifetime on a follower, as it would have before this package.
DHCPScope objects still answer "this object kind cannot be written through a
follower". Typed `primary_required` refusals, role reporting and the remaining
forwarded domains are FWD-REFUSAL, FWD-LOGOUT, FWD-SETTINGS, FWD-SELF,
FWD-NODELOCAL, FWD-UI and FWD-E2E.

### FWD-BOUND review fix: a forwarded command is acknowledged under its own commit — November 10, 2026 (decision row pending a number)

The review confirmed that a multi-document apply through a follower was not
reliable: a later document could answer `completion_unknown` although the
primary had committed it, and the committed process test avoided the case. The
cause is on the primary and concerns every forwarded command, not only apply. A
member command committed, released the Store's writer lock, and then took the
lock again to admit its acknowledgment, which requires the exact authority and
generation the command produced (`MemberRequest.WithCommandOutput`). The
publication gate keeps other current commands out of that gap, but not writers
that take only the Store lock, such as status reconciliation. A commit in the
gap made the comparison fail (`ErrCAS`), the member server aborted the
response, and the follower mapped the lost exchange to `completion_unknown`.

`Store.MemberCommandWithOutput` now runs the same output admission in the
command's finalizer, after confirmed SQL completion and before the writer lock
is released, as a local command's output already does (D347).
`providercoordinator.WithMemberCommandOutput`, the only production caller (the
root's scoped member executor for `blocking.toggle`, `objects.write`,
`objects.delete`, `zones.command` and `auth.login`), uses it. No comparison is
relaxed: the exact authority and generation, the original owner, the peer
certificate, the deadline and the command receipt are all checked as before,
and completion is reported apart from the output's result. `MemberCommand`
followed by `WithCommandOutput`, and image reads, keep the two-step form and
keep refusing after any other commit. The process test applies two new
documents again.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `fix-race01-ack-coordinator-prefix` (retained failure before the fix, three
  of three): `TestMemberCommandOutputAcknowledgesBeforeAnUngatedWriter` with the
  three sources restored to `0f580333` through `go test -overlay`. A writer
  queued during the command commits wherever the Store consults the original
  request before the output; the coordinator returns `committed=true` with
  "replication: authority changed". With the fix the test passes: the queued
  writer commits only after the acknowledgment returned.
- `fix-race02-root-prefix-mutation` (retained failure before the fix):
  `TestRootMemberForwardIsAcknowledgedBesideOtherPrimaryCommits` forwards 40
  client group writes through the follower fixture over actual member mTLS
  while one primary goroutine commits a section row every 300 microseconds.
  15 of 40 answered `503 completion_unknown`; all 40 were stored on the primary.
- `fix-race05-root-fixed`: the same test passes (40 of 40 answered `201`) in the
  28-test root selection, with every `TestRootMember*` test (blocking, object,
  zone and login commands, listener and revocation). `fix-race06-root-fixed-x5`:
  five more passes (200 forwards, none unacknowledged).
- `fix-race03-light-packages`: default race of
  `internal/cluster/enrollment/persistence` (140.9 s) and
  `internal/auth/providercoordinator` (92.6 s). New store tests:
  `TestMemberCommandWithOutputExcludesWritersThroughAcknowledgment` (a queued
  writer cannot complete during a 100 ms acknowledgment and commits right
  after it), `TestMemberCommandThenOutputStillRefusesAfterAnotherCommit` (the
  two-step form still answers `ErrCAS`) and
  `TestMemberCommandWithOutputSeparatesCompletionFromOutput` (no-op, output
  error, output cancellation, refused command, canceled command, image request,
  revoked peer, missing output). `fix-race04-replica`: default race of
  `internal/cluster/replica` (40.6 s). `fix-race07-sqlite`: default race of
  `internal/store/sqlite` (261.8 s).
- Two real processes. `fix-process01-enrolled-prefix-x2` (node binary built
  from the three sources at `0f580333`; `fix-overlay-binary-check` shows that
  the overlay reaches the fixture's nested build): the restored process test
  passes four of four scenario runs (429.5 s). The window is narrow in real
  processes, so the restored assertion alone does not fail before the fix; the
  earlier retained `process02-enrolled` did (one of four scenario runs).
- Two real processes under load (evidence only, not a committed test).
  `fix-process-stress-variant_test.go.txt` is the process test with one added
  block, supplied through `go test -overlay`: after the committed apply rounds
  it sends 150 applies of two new documents through the follower back to back,
  without waiting for convergence, and classifies the answers.
  `fix-process03-stress-prefix` (node binary built from the three sources at
  `0f580333`), two runs: 144 of 150 applies answered `created` for both
  documents and 6 answered `completion_unknown` for one of them, in each run
  (12 of 300). The unanswered document was the first of its request in 8 of
  the 12 and the second in 4, so the primary's follow-up writes for the
  previous request interpose as well as those for the same request. The
  variant checks the record sets (the second documents): all 4 that answered
  `completion_unknown` were served by the primary afterwards, so they had
  committed.
  `fix-process05-stress-fixed` (node binary of `29cdf81a`), two runs: 150 of
  150 applies answered `created` for both documents in each run (300 of 300),
  no `completion_unknown`, no other answer. `fix-process04-stress-fixed` is the
  same run cut off after its first line when the first fixer's session was
  interrupted; it holds no result.
  `fix-process02-enrolled-fixed-x3`: both scenarios pass three times on the
  fixed tree (102.2, 113.1, 94.8, 114.3, 94.6 and 112.9 s; package 643.7 s).
  Each run applies two new documents (a block list and a record set) through
  the follower four times, requires `created` for both every time, observes
  each on both nodes, and deletes the four record sets in one
  `POST /dnsrecordsets:batchDelete` that must report four successes.
- `fix-lint01`: `gofmt -l` empty, `go vet` clean and scoped golangci-lint
  v2.15.0 with 0 issues over `cmd/kudurru`, `internal/store/sqlite`,
  `internal/auth/providercoordinator`, `internal/servinghttp`,
  `internal/cluster/enrollment/persistence` and `test/e2e`.
  `fix-final01-head`: at `29cdf81a` the tree is clean, `gofmt -l` is empty,
  `CGO_ENABLED=0 go build ./...` succeeds and `TestOpenAPIDrift`,
  `TestRoutePermissionLint`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass.
- Final head. The second fixer (the first was interrupted before it could
  report) repeated the checks at `ba1351ac` with a clean tree, after its own
  three commits: `d620b1ad` and `8c7e4862` (see the entry above) and
  `ba1351ac`, which names `MemberCommandWithOutput` in two places of the
  architecture page and in one code comment that still described the two-step
  form, and separates a paragraph that `29cdf81a` had run into the next one.
  `fix-lint02-final`: `gofmt -l` empty for every Go file changed since
  `0f580333`, `go vet` clean and scoped golangci-lint v2.15.0 with 0 issues
  over the same six packages; `fix-lint03-replica` adds
  `internal/cluster/replica`, where `ba1351ac` changed the comment.
  `fix-race11-packages-final`: default race of `internal/servinghttp` (7.9 s),
  `internal/auth/providercoordinator` (37.9 s),
  `internal/cluster/enrollment/persistence` (60.2 s),
  `internal/cluster/replica` (19.8 s) and `internal/store/sqlite` (271.7 s).
  `fix-process06-enrolled-final-x2`: both two-process scenarios pass twice
  (98.4, 113.0, 95.3 and 114.7 s; package 428.1 s).
  `fix-final02-head`: `CGO_ENABLED=0 go build ./...` succeeds;
  `TestOpenAPIDrift`, `TestRoutePermissionLint`, `TestReferenceDrift`,
  `TestAggregatedRolesDrift` and `TestCLIReferenceDrift` pass; no generated
  file differs from `0f580333`. There is therefore no new generated commit: the
  seven fix commits (`83b49fdf`, `6ddd7cef`, `8e3eeb87`, `29cdf81a`,
  `d620b1ad`, `8c7e4862`, `ba1351ac`) sit on top of `0f580333` (`chore(gen)`).

What remains open: the acknowledgment still travels over the network, so a
forwarded command whose response is lost, cut by the 30-second bound or
refused by the remaining output checks (a fenced writer, a revoked peer, an
expired credential or certificate) answers `completion_unknown`, as designed.
The acknowledgment is written to the peer while the writer lock is held, as it
already was; it now also holds the lock from the commit on, so a slow peer
delays other primary writers by up to the request's remaining lifetime. Nothing
measures that delay. The process-level evidence of the window is statistical
and comes from an evidence-only variant (12 of 300 applies unanswered before
the fix, 0 of 300 after); the committed process test did not fail before the
fix in four scenario runs. The committed proofs are the coordinator test,
which is deterministic, and the mounted test of 40 forwards beside a committing
writer (15 of 40 unanswered before the fix). The review left the choice
between closing the window and documenting it to the integrator: this entry
closes it, and its decision row is drafted without a number.
