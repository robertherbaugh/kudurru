### G21-11 Refusal inventory test across every runtime shape — November 13, 2026 (no decision row)

The claim "no unexplained 501/503 remains" is now checked by tests, in two
halves: the mounted walks for every refusal a probe reaches, and a static check
of this package's emitters for the rest (both described below). It builds
on the follower table of D368 and D381 and does not add a second follower
classification. `internal/api/refusals.go` names every API operation in
`operationRefusals`, with the 501/503 key that a probe answers on each runtime
shape: standalone, enrolled primary, static, follower and Kubernetes. The
primary, the static twin and the follower inherit the standalone key. The
follower's `primary_required` for every `FollowerPrimary` mutation comes from
`followerOperations`. Fenced and conflicted members answer `cluster_fenced` or
`cluster_conflict` for every operation. Refusals that exist only because the
query log, DHCP or a VRRP address is turned off are listed apart, in
`featureRefusals`. `refusalKeys` is the allowlist of 501/503 keys. Each row
names its anchor on the operator page `docs/deploy/capabilities.md` (G21-13).

`TestRefusalInventoryNamesEveryOperation` fails for a route that has no line,
and its message says what to add and how to see what each shape answers. It
also fails for a stale line, an undeclared key, and an anchor that the page
does not have. The new package `internal/api/refusalinventory` walks the
served OpenAPI document with an admin session and runs the session-ending
operations last. It fails for:

- an error without the JSON envelope;
- a 501/503 that the table does not declare for that shape;
- a declared refusal that the shape does not answer;
- a key without an allowlist row;
- a refusal without a hint;
- a durable refusal that carries `Retry-After`, or a transient one that does
  not.

The walk runs through the real seams:

- mounted `runServe` for a licensed SQLite standalone node with the query log
  on;
- a static twin;
- a Kubernetes node against envtest;
- the fenced and conflicted states of the in-process member fixture, through
  the production serving gate;
- in test/e2e, an enrolled primary and its follower (real processes; the
  follower's sign-in is forwarded) and both conflicted processes of the
  conflict scenario.

Walking the shapes found real defects, and they are fixed:

- On Kubernetes, `sessions_unavailable`, `factor_management_unavailable`,
  `external_users_unavailable` and `user_management_unavailable` had no hint.
- Those keys, `cli_login_unavailable`, `provider_logout_unavailable` and the
  legacy mux fallback carried `Retry-After: 1`, because the auth error writer
  adds it by default.
- Cluster enrollment, invitations and promotion on a static or standalone
  node had no hint and carried `Retry-After: 1`.
- An unknown or unready OIDC provider (browser and CLI), DHCP reads without
  DHCP, the VIP event stream without a VRRP address, integration
  administration on Kubernetes and `generation_pending` had no hint.

Each durable refusal now sets `NoRetryAfter`, as D368 does for the fenced and
conflicted answers. Statuses and keys did not change.

Review fixes (November 13, 2026). An independent review found that the walks
stopped at request validation for more than half of the operations, and that
the table therefore missed the Kubernetes `unavailable` of seven integration
operations. Now:

- Probes send a query, a body and an `If-Match` where the handler decides
  (`probeRequests` in the walk): the integration reads and commands (namespace,
  uid and revision of a review that names nothing) and the DHCP mutations reach
  their handlers. Every integration operation declares `unavailable` on
  Kubernetes, as `docs/deploy/capabilities.md` says, and the DHCP mutations
  joined `featureRefusals`. Probes that stop at validation fell from 72 to 30 of
  191 on the standalone node and from 58 to 30 on Kubernetes; each walk logs how
  many stopped at validation and how many answered 404.
- `TestRefusalEmittersExplainThemselves` parses `internal/api` and fails for
  any `errUnavailable` call, or `newError` with 501 or 503 and an empty hint,
  that `hintlessRefusals` does not name. The list holds 144 internal-fault and
  per-request answers (a command result that went missing, a broken plan
  invariant, an exceeded bound) whose message is the whole explanation.
  `errNotImplemented` (eight call sites) now always has a hint, and the
  deployment limits that had none gained one: no block page renderer, no DHCP
  setup manager, no query history for suggestions, status and actions, and
  cluster-wide query actions without a central database.
- A declared `unavailable` may not carry `Retry-After`, except for browser and
  CLI provider sign-in (`transientUnavailable`), so the `NoRetryAfter` of
  enrollment, invitations and promotion is now enforced by every walk.
  `TestLegacyAuthHandlerFallbackAndRefusals` asserts no `Retry-After` on each
  legacy refusal, including the last fallback.
- A probe of a mutation that succeeds outside the reviewed `probeMutations`
  list fails the walk. The enrolled process test walks the primary and the
  follower as its last step, after the restart checks.
- A `generation_pending` answer is waited out through `GET /auth/me` before
  the operation is sent again. Provider sign-in admission charges its
  five-request lane before the serving generation is captured, so quick
  resends of `oidcStart` ended in 429 (fix05, below).

Evidence under `.context/v020-final-20261110/evidence/G21-REFUSAL-INVENTORY/`:

- `explore01`-`explore04.log`: development walks with
  `KUDURRU_REFUSAL_INVENTORY_DUMP=1`. These are retained failures that show
  the defects above. explore01 ran without a license, so most mutations
  answered 402. explore02 found the missing hints. explore03 found the
  `Retry-After: 1` on durable refusals and the undeclared Kubernetes `501
  read_only` of `PATCH /node/config`. In explore04 only the standalone
  settings-ownership line was still missing.
- `explore05.log`: a retained failure. Each request to a fenced or conflicted
  member waits for the serving gate's 2-second capture wait, so a sequential
  walk of 191 operations ran past its 5-minute budget. The walk now probes
  closed members 16 at a time. `explore06.log`: fenced and conflicted pass,
  191 of 191 operations refused (54.9 s).
- `e2e01.log`: empty. The run was stopped at its background limit while it
  waited for the heavy lock. `e2e02.log`: a retained failure. The enrolled
  fixtures run without the query log (which led to `featureRefusals`).
  Promotion review and commands are not refused on enrolled members, and OIDC
  start and callback answer `primary_required` on a follower. `e2e03.log`:
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` passed (184.6 s;
  primary: 191 operations, 17 refused; follower: 191, 67) and
  `TestEnrolledProcessConflictResolution` passed (120.9 s; 191 of 191 refused
  on each conflicted process). `e2e04-final.log`: the same two tests pass on the
  committed tree (134.9 s and 133.9 s; the same counts).
- `race01-light.log`: `-race` for `internal/auth/stack`, `internal/apiclient`
  and `internal/api/refusalinventory`; all pass.
- `race02-api.json`: `-race` for all of `internal/api`: 668 pass, 3
  environmental skips, 1 fail (267.6 s). The one failure is
  `TestRefusalInventoryNamesEveryOperation`, and only because this lane does
  not have `docs/deploy/capabilities.md`; the integration branch does.
  `api03-anchors-with-pr-capabilities.log`: the same test passes with that
  page copied from `robertherbaugh/v020-features-review`. The page was removed
  again afterward and is not committed.
- `race03-root.log`: `-race` selection of 62 tests in `cmd/kudurru` (652.6
  s). 59 passed. These are retained failures: the standalone and static walks
  saw `generation_pending` for more than the walk's 10-second window on the
  provider routes, and that answer had no hint. The walk now waits up to a
  minute for that transient answer only, and the answer has a hint.
  `TestServingHTTPTOTPLoginPasswordConsumeExactlyOnce` failed once with
  401 instead of 429 (its per-second limiter expired under the batch's load).
  This lane did not change that path.
- `race04-root.log`: `-race`. All four inventory tests, the legacy fallback
  test and the TOTP test pass (129.3 s): standalone 25.3 s, static 17.5 s,
  Kubernetes 17.1 s, fenced 30.2 s, conflicted 30.4 s.
- `drift01.log`: `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestOpenAPIServedAndDocs`, `TestUnknownAPIPathIsEnvelope` and both follower
  table tests pass. No generated artifact changed.
- `lint01.log`: a retained failure (one gosec G115 finding in the new walk
  test). `lint02.log`: 0 issues after the fix.

Review-fix evidence (`fix01`...`fix09`):

- `fix01-emitters-before.log`: `TestRefusalEmittersExplainThemselves` with an
  empty `hintlessRefusals` and the hint changes stashed: 151 hintless sites
  reported, among them the five configuration answers above and
  `errNotImplemented answers 501 with hint <nil>`.
- `fix02-root.log`: a retained failure. The ownership probe body with
  `action` was rejected (`action` is a path parameter); the body went back to
  the revision only.
- `fix03-light.log`: `-race` for `internal/api/refusalinventory`,
  `internal/auth/stack`, `internal/servinghttp`: all pass.
- `fix04-api.json`: `-race` for all of `internal/api`, with
  `docs/deploy/capabilities.md` copied from the integration branch for the
  run and removed afterward: 671 pass, 3 environmental skips, 0 fail (417.5 s).
- `fix05-root.log`: a retained failure. The static walk's `oidcStart` answered
  `429 rate_limited` after quick resends of a pending answer; the other walks
  passed (standalone 15 refused, 30 stopped at validation, 39 not found;
  Kubernetes 45, 30, 39).
- `fix06-root.log`: `-race -count=2` of the standalone, static, Kubernetes and
  closed-member walks, `TestLegacyAuthHandlerFallbackAndRefusals` and
  `TestRunServeKubeForwardedAuthRoutesAnswerEnvelope`: every test passes in
  both iterations (302.2 s). Per iteration: standalone 191 walked, 15 refused,
  30 validation, 39 not found; static 87, 10, 22; Kubernetes 45, 30, 39; fenced
  and conflicted 191 of 191.
- `fix07-e2e.log`: `KUDURRU_E2E=1`.
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` passes (186.9 s;
  primary 21 refused, 30 validation, 39 not found; follower 71, 16, 29) and
  `TestEnrolledProcessConflictResolution` passes (137.1 s; 191 of 191 on each
  conflicted process).
- `fix08-lint.log`: a retained failure, golangci-lint for `internal/api/...`,
  `test/e2e/...` and `cmd/kudurru/...`: one gosec G304 in the emitter check
  (`os.ReadFile` of a listed name). `fix09-lint.log`: 0 issues after the
  parser reads the files itself.
- The bench guards (`hack/test_bench_*.py`) show only the pre-existing
  socket-calibration failure described below; no bench file was touched.

What remains open:

- On the integration branch, `TestRefusalInventoryNamesEveryOperation` and
  every walk will fail for the seven operations that other lanes added:
  `createEdgeBlock`, `listEdgeBlocks`, `revokeEdgeBlock`, `getClusterAnchors`,
  `getBlockListRPZFeeds`, `listRPZFeeds` and `querylogSinks`. Each needs a
  line in `operationRefusals`; the failure messages say so. This is by design.
- The fenced shape is walked only in the in-process member fixture, not
  between processes. This lane's e2e has no fenced scenario; the integration
  branch's `enrolledFencedClosed` (enrolled_cluster_test.go) is where an
  `enrolledRefusalInventory(t, apiA, api.ShapeFenced, document)` call belongs,
  with the document read before the promotion. The integrator must add it and
  run the e2e once more; until then the fenced shape between processes is
  unmet.
- The walks still stop at request validation for 30 of 191 operations on the
  standalone node, the primary and Kubernetes (16 on the follower, 10 on the
  static node), and 39 answer 404 for an object that does not exist. A refusal
  that a handler decides after validation or after the lookup is not observed
  there. In `internal/api` the static check covers it: every hintless 501/503
  is named and classified. Handlers outside `internal/api` (`cmd/kudurru`,
  `internal/auth/stack`, `internal/servinghttp`) have no static check; their
  refusals are checked where a probe reaches them.
- `browserLogout` is probed after `logout` ends the session, so it answers 401
  on every shape and is never observed with a live session.
- The provider sign-in hint ("GET /api/v1/auth/status lists the identity
  providers ...") is also answered for transient failures of that path (a
  writer bind or a command error), where "try again" is the useful part.
- `docs/deploy/capabilities.md` (on the integration branch) explains
  `not_implemented` only as an integration provider's missing command. It now
  also means a server started without that part of the runtime; the glossary
  row should say so when the page is next edited.
- In this lane, `CONTRIBUTING.md` links `docs/deploy/capabilities.md`, which
  is on the integration branch.
- An unrelated pre-existing failure: `hack/test_bench_socket_calibration.py`
  fails in this lane. `internal/dnsserver/quic_socket_test.go` no longer
  matches its pinned SHA-256, because of the earlier lane commit c510158d.
