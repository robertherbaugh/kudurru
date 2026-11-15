### FWD-SELF Forwarded own password change and personal tokens, with the FWD-E2E part for them — November 10, 2026 (D368)

An enrolled SQLite follower now forwards the caller's own password change
(`POST /auth/password`) and the creation and revocation of personal API tokens
(`POST /auth/tokens`, `DELETE /auth/tokens/{id}`) to its primary as two typed
version-2 member commands, `auth.password` and `auth.tokens`. They follow the
D362/D363 pattern used by `auth.logout` and `settings.patch`: the route is
bound to 30 seconds before the body is read, the follower checks the request
against its leased view, captures its source once and sends one command with
the raw credential proof; the primary verifies that credential again in the
member transaction, where effect and dedup commit together. Clean refusals are
retained results, an unsent command answers `503 unavailable`, a failed
exchange `503 completion_unknown`, and nothing is retried.

For `auth.password` the follower checks the new password's length rules and
hashes it under the route's bound, so only the hash travels. The payload also
carries the current password, the factor code, `revokeTokens`, the client key,
an unstored 32-byte nonce (the dedup fingerprint is no password oracle) and,
for a cookie caller, a session ID and CSRF hash that the follower prepared. The
primary runs the new `security.ChangeForwardedLocalPasswordWithAdmission`, the
same admission as its own route (shared lockout counters, current password,
factor, account manager), which enrolls exactly the prepared session. A wrong
password or factor, a lockout (`429` with its Retry-After) and `managed_by` are
retained results whose counters commit on the primary. The result is emitted
under the command scope only, because the change ends a cookie caller's
credential. After the commit the follower pulls once, for a cookie caller and a
token caller alike, and then seals the new cookie or, for a token caller,
answers from the first view that carries the new hash (at most five seconds).
A committed change is never answered `503 unavailable`, which means that
nothing was sent: a token caller whose view still lacks the hash at that bound
gets 200 from the latest view for the original account with
`mustChangePassword` false, as the primary's own route answers from the
postcommit account, and a cookie caller whose session cannot be sealed within
the route's bound gets `503 committed_output_unavailable` (the password
changed; sign in with the new one; do not replay). Each committed change
leaves the two object audit rows of the shared password body (the LocalUser
update and its `passwordChangedAt` status) in the member commit, attributed to
the original caller (`User:<name>`, the command ID as request ID, method
`POST`, path `clusterreplica.PathWrite`, a non-empty authz), the same
attribution as every other forwarded domain. This closes the dead end
of a must-change user who signed in on a follower (D363) and then got `403
password_change_required` for every route and `primary_required` for the
change itself.

For `auth.tokens` the routes' bodies moved into `stageToken` and
`deleteTokenCurrent` and run unchanged on the primary. For a creation the
follower generates the token and sends only its ID and the secret's hash,
because a retained outcome may not be sensitive; the primary checks the staged
row against that hash with the new `servingauth.AuthenticateIssuedTokenCurrent`
instead of the secret. Refusals before the write (scope wider than the
caller's, invalid lifetime or scope, a lifetime beyond
`auth.tokens.maxLifetime`) are retained. A revocation keeps the
owner-or-`tokens.delete` rule and its retained `404` and is emitted under the
command scope only, because it may revoke the token that sent it. For both
commands a credential that the primary no longer accepts or authorizes is a
retained `401` or `403`, except that a command whose dedup record exists is
aborted, as for `auth.logout`. A creation and a revocation each leave one
APIToken object audit row in their commit with the same attribution. The follower answers once its own view
authenticates the new token, or no longer lists the revoked one (at most five
seconds), so the token works on that follower at once. `kudurru login
--username` therefore works against a follower: the earlier refusal before the
password was sent (and its `GET /auth/status` preflight) is removed.

The three operations moved to the `forwarded` class: the follower table counts
are now 33 forwarded, 14 local and 47 primary (`TestFollowerOperationsClassifyEveryMutation`),
the mounted follower API has 47 refused and 47 admitted mutations, and the
OpenAPI document and endpoint index mark them `forwarded`. The allow/deny
domain commands and unblock decisions stay `primary`: the D362 pattern does not
apply cleanly to them. The eleven domain routes build closure plans inside the
transaction rather than a re-entrant command body over decoded input, `POST
/queries/{id}/actions` resolves its domain from the answering node's own query
log, and the source-entry patch edits list bodies; unblock decisions write
their decision audit row in the API layer outside the command's transaction
and flush the answering node's response cache, so a forward would split the
effect from its audit row. The same list edit is forwarded today as a BlockList
object write, and docs/deploy/sqlite-join.md says so.

Evidence under `.context/v020-final-20261110/evidence/HA-SELF/`:

- `race02.log`: `go test -race -count=1 -run '^(TestRootMemberPasswordCommandActualHTTP|TestRootMemberTokenCommandActualHTTP|TestRootMemberForwardPasswordAndTokensActualHTTP)$' ./cmd/kudurru`:
  ok, 14.7 s.
- `race03.log`: `go test -race -count=1 -v -run '^(TestRootMember|TestMemberForward|TestMemberLogout|TestServingFollower|TestFollower|TestCLILogin|TestServingHTTP|TestLogin|TestServingCanonicalSettings)' ./cmd/kudurru`:
  123 passed, 0 skipped, 0 failed, 418.1 s. New:
  `TestRootMemberPasswordCommandActualHTTP` (5.30 s: a wrong current password
  is the retained 401 with Credentials; forged subject, invalid hash, a token
  caller with a session, a cookie caller without one and an unknown field
  abort; a cookie caller's change commits the forwarded hash and enrolls the
  prepared session, the old cookie stops verifying, cannot recover its
  retained outcome and is a retained 401 for a new command; a token caller's
  change commits and its duplicate replays without a second write; repeated
  wrong passwords reach the shared lockout's 429 with Retry-After; no
  plaintext password in the durable image),
  `TestRootMemberTokenCommandActualHTTP` (2.55 s: creation with the
  follower's ID and hash, inherited scopes, exact dedup replay, the secret
  authenticating on the primary's published view; retained 403 for a wider
  scope and 400 for an invalid lifetime and an unknown role, none stored;
  forged subject, invalid hash, invalid ID, unknown action and a delete with a
  body abort; retained 404 for an unknown token; a token revokes itself, cannot
  recover that outcome and is a retained 401 afterward; the secret is not in
  the durable image) and `TestRootMemberForwardPasswordAndTokensActualHTTP`
  (5.74 s, through the follower's mounted API and real lease: a must-change
  session signed in on the follower is refused elsewhere, a wrong current
  password is the relayed 401, the change answers 200 with a new cookie that
  works on the follower at once while the old cookie is refused; a token
  created through the follower works there at once and is stored with its hash
  on the primary, an unknown role is the follower's own 400 (`tokenRequestIntent`
  runs before the forward, so it never reaches the primary), an unknown token
  the relayed 404,
  and the token's own revocation through the follower is 204 and refused there
  afterward). The updated `TestRootMemberFollowerRefusesUnforwardedMutations`,
  `TestMemberForwardRouteCoversEveryForwardedRoute`,
  `TestRootMemberFollowerRequestLifetimeBound` and
  `TestCLILoginTOTPActualSQLiteLostResponseNeverRetries` (now: no request
  other than the login precedes the proof) pass in this run.
- `light01.log`: `go test -race ./internal/auth/security ./internal/auth/users ./internal/servingauth`: ok (80.6 s, 7.5 s, 57.7 s).
  `light02.log`: the internal/api follower and cluster-info tests: ok.
- `race04.log`: `go test -race -count=1 ./internal/api/... ./internal/auth/... ./internal/servingauth/... ./internal/apiclient/...` 25 packages ok, 0 failed (internal/api 212.5 s).
- `process02.log`: `KUDURRU_E2E=1 go test -count=1 -v -run '^(TestEnrolledClusterProcessesPartitionCatchupAndRestart|TestEnrolledClusterProcessesPromoteAndRejoin|TestEnrolledProcessConflictResolution)$' ./test/e2e`:
  3 of 3 passed (129.4 s, 170.4 s, 64.9 s; 371.2 s for the package). Between
  two real processes a new must-change user signs in on the follower, a wrong
  current password is relayed as 401, the change through the follower gives a
  cookie that works on both nodes while the old one stops working on both,
  the new password works on the primary, and `kudurru login --username admin`
  against the follower (file credential store) stores a token that
  `kudurru api /auth/me` uses on the follower and that the primary accepts.
  The refusal scenario now expects `changePassword`, `createToken` and
  `deleteToken` among the forwarded operations.
- `lint02.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./internal/servingauth/...`, `./internal/auth/security/...`,
  `./internal/auth/users/...`, `./internal/api/...`, `./internal/apiclient/...`
  and `./test/e2e/...`: 0 issues; `gofmt -l` empty.
- `gen01.log` and `drift01.log`: `make openapi`, `make client-go`, `bun run
  gen:api`, `make docs-gen` changed only the two OpenAPI documents and the
  endpoint index (three rows `refused` to `forwarded`); `TestOpenAPIDrift`,
  `TestReferenceDrift`, `TestAggregatedRolesDrift` and `TestCLIReferenceDrift`
  pass after the generated commit.
- Review fixes (commits `fix(cluster): never answer a committed forwarded
  password as unavailable` and `docs(cluster): describe the committed answer
  of a forwarded password`). The reviewer's probe (`review-probe01.log`)
  showed a token caller's committed change answered `503 unavailable` after
  5.48 s in 1 of 6 attempts. Two tests-only runtime seams
  (`memberPullInterval`, `memberSelfPull`) and the fixture option
  `memberForwardRarePull` (scheduled pull once an hour) make that race
  deterministic. `fix-prefix01.log`: with `serving_member_self.go` as it was
  at `afcf2ffc`, `TestRootMemberForwardTokenCallerPasswordActualHTTP` fails
  with `503 {"key":"unavailable","message":"authentication operation
  unavailable"}` after 5.3 s and `TestRootMemberForwardCookiePasswordUnservedActualHTTP`
  fails (the old code ignores the skipped-pull seam and answers 200).
  `fix-race01.log`: the five self-service tests with the fix; the two new ones
  passed, `TestRootMemberPasswordCommandActualHTTP` failed on the new audit
  assertion, which expected one LocalUser row per change where the shared
  password body writes two (the update and the `passwordChangedAt` status);
  the assertion now expects the two attributed rows. `fix-race02.log`: that
  test passes (6.0 s). `fix-race03.log`: `go test -race -count=2 -v` over the
  five self-service tests, every `TestRootMemberForward*`,
  `TestRootMemberSettings*`, `TestRootMemberLogout*`,
  `TestRootMemberFollowerRefusesUnforwardedMutations`,
  `TestMemberForwardRouteCoversEveryForwardedRoute`,
  `TestRootMemberFollowerRequestLifetimeBound` and `TestCLILogin*`: 32 tests,
  64 passed, 0 skipped, 0 failed, no race report, 338.9 s. New:
  `TestRootMemberForwardTokenCallerPasswordActualHTTP` (9.4 to 9.5 s: a token
  caller's change through the follower answers 200 with
  `mustChangePassword` false after the follower installed the new hash; with
  the explicit pull skipped, a `revokeTokens` change still answers 200 for the
  account, commits on the primary, removes the token there, and the follower
  refuses the token once it pulls) and
  `TestRootMemberForwardCookiePasswordUnservedActualHTTP` (33.5 to 33.7 s,
  most of it the route's 30-second bound: a cookie change that the follower
  cannot seal answers `503 committed_output_unavailable` and is committed on
  the primary). `TestRootMemberPasswordCommandActualHTTP` and
  `TestRootMemberTokenCommandActualHTTP` now assert the object audit rows on
  the primary (two LocalUser rows per committed password change, one APIToken
  row for a creation and one for a revocation, each `User:root-command` with
  the command ID, method, `PathWrite` and authz; none for a refused change or a
  dedup replay). `TestRootMemberForwardPasswordAndTokensActualHTTP` adds a
  scope wider than the caller's token through the follower: the primary's
  relayed `403`, and no token stored. `fix-lint01.log`: scoped golangci-lint
  v2.15.0 over `./cmd/kudurru/...`: 0 issues; `gofmt -l` empty. No route, DTO
  or permission changed, so no artifact was regenerated.
- Retained failed runs: `fix-race01.log` and `fix-prefix01.log` (above), `race01.log` (the cookie-caller password change
  aborted, reported as `completion_unknown`, because the fixtures' fixed clock
  never advanced past the account's new floor, so the new session could not be
  issued strictly after it; the tests now let the fixture clock follow real
  time, as on a real node; the token-caller and token tests passed in that
  run), `process01.log` (both enrolled scenarios failed at `GET /cluster`
  with 403 after the new helper logged the test user in on the primary with
  the primary client's cookie jar, replacing the administrator's session; the
  helper now uses a client of its own; the conflict scenario passed) and
  `lint01.log` (two gosec G101 false positives and one unused field, fixed in
  `fix(cluster): satisfy lint in the self-service forward` and the e2e
  commit).

Open: FWD-E2E stays open. Done here for it: the password, token and CLI-login
process assertions in the enrolled scenarios and the refusal list edits. Not
done: one representative per primary-only group on the real follower process,
node-local process assertions, the partition answer for the new routes, the
same routes running locally after promotion, a re-run of the static twin
scenario, the pinned Semgrep scan, and frozen-source hashes under a
`.context/follower-routes-<date>/` directory. The process e2e scenarios, the
drift tests and the generators were not re-run after the review fixes (no
route, DTO or permission changed). Also open: TOTP setup, confirmation and removal, the allow/deny domain commands
and unblock decisions remain `primary` on a follower, for the reasons above;
they need typed re-entries (and, for unblock, the decision audit inside the
command's transaction) before they can be forwarded. The audit assertions compare the forwarded rows with the attribution of the
other forwarded domains (`PathWrite`, the command ID), not with a mounted
primary route's own request path. The process test exercises the success and
relayed-refusal paths, not an unreachable primary for these routes (that
answer is the shared forwarder's, covered for sign-out and settings). The
dashboard does not yet use the new forwarded operation IDs. The full
`./cmd/kudurru` package was not run here; the integrator runs it through
`hack/go-test.py`.
