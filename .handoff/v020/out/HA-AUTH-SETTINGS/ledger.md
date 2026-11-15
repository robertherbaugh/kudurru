### FWD-LOGOUT and FWD-SETTINGS Forwarded sign-out, own-session revocation and cluster settings patch — November 10, 2026 (D368)

An enrolled SQLite follower now forwards sign-out (`POST /auth/logout`,
`POST /auth/logout/browser`), the revocation of one of the caller's own
sessions (`DELETE /auth/sessions/{id}`) and the cluster settings patch
(`PATCH /settings`) to its primary as two typed version-2 member commands,
`auth.logout` and `settings.patch`. Both follow the D362/D363 pattern. The
route is bound to 30 seconds before the body is read, the credential is
admitted against the follower's leased view, the source is captured once and
one command carrying the raw credential proof is sent. The primary verifies
that credential again inside the member transaction, where the effect, its
audit and the dedup record commit together. Clean refusals are retained
results, an unsent command answers `503 unavailable`, a failed exchange
`503 completion_unknown`, and nothing is retried.

For `auth.logout` the primary derives the session to end from the verified
cookie (new `servingauth.AuthenticateCurrentSessionWithClock`), never from an
ID that the follower names. `DELETE /auth/sessions/{id}` keeps the route's
owner check and retains its `404`. A token or JWT caller is a committed no-op.
A credential that the primary no longer accepts is a retained `401`, but a
command whose dedup record already exists is aborted instead, so a revoked
credential never recovers its own retained outcome. Because the command may
revoke its own credential, the result is emitted under the command scope only.
On the follower, a sign-out always clears the browser's cookies, also when the
command was not sent or its outcome is unknown, and the answer then says that
the session may stay valid until it expires. A successful sign-out pulls once,
so the follower refuses the session at once. A follower offers no provider
logout continuation.

For `settings.patch` the follower sends the merge patch, the If-Match and an
unstored 32-byte random nonce, with the `Kudurru/default` resourceVersion of its
own view as precondition. A stale view is a retained `409 conflict`. The
primary runs `api.Server.ForwardedSettingsCommand`, which repeats the route's
key, scope, lock and value checks, merges into the transaction's singleton,
checks branding against the transaction's pinned license (now and again at
commit) and stages one write whose object audit row names the original
caller. Refusals before the write (`400 invalid`, `400 host_only`,
`402 license`, `409`, `412`) are retained. A failed write rolls back. After a
committed patch the follower pulls once and answers when its published view no
longer carries the version that the patch was made against (bounded to five
seconds within the route's deadline; a failed pull or a late publication still
answers the committed result). Without that wait, found in review, the
caller's next patch through the same follower was a `409` and a forwarded
blocking switch a generic `503` for about five seconds, and the dashboard's
Settings page saves one key per request. The follower answers with the
primary's body under its own node name. The four
operations moved to the `forwarded` class, so `GET /cluster` lists them, the
OpenAPI document marks them `x-kudurru-follower: forwarded`, and a follower's
settings schema no longer marks cluster keys read-only. `PATCH /node/config`
stays `primary`.

Evidence under `.context/v020-final-20261110/evidence/HA-AUTH-SETTINGS/`:

- `race06.log`: `go test -race -count=1 -run '^(TestRootMember|TestMemberForward|TestServingHTTP.*Logout|TestServingHTTPDurableLoginTokensSessionsLogout|TestServingCanonicalSettings|TestServingBrandingHeld|TestServeLegacyNodeSettingsReadOnly|TestServingFollower|TestFollower)' ./cmd/kudurru`:
  37 passed, 0 skipped, 0 failed, 226.0 s. This includes the new
  `TestRootMemberLogoutCommandActualHTTP` (6.28 s: token no-op, retained
  404 for an unknown session ID, forged subject, unknown field and precondition aborted,
  revocation of a second own session with an exact dedup replay, sign-out of
  the cookie's own session, a revoked credential cannot recover its retained
  outcome, a new command with that credential is a retained 401, no raw cookie
  in the durable image), `TestRootMemberSettingsCommandActualHTTP` (3.97 s:
  commit with exactly one attributed audit row, dedup replay with no second
  write, stale precondition 409, retained 400 for unknown key, invalid value
  and empty patch, 400 host_only for a node key, 412 for an upstreams If-Match,
  402 for branding without entitlement, forged subject, missing precondition
  and short nonce aborted, the refused DSN and the raw token absent from the
  durable image, a revoked credential cannot recover the retained result) and
  `TestRootMemberForwardSettingsAndSignOutActualHTTP` (14.65 s: through the
  follower's mounted API and real lease: PATCH /settings, the relayed 400, the
  settings schema, a session revocation, the 404, sign-out without CSRF
  refused, browser sign-out clearing cookies and the follower refusing the
  session after its pull, and an API-token sign-out). It also includes the
  primary's own logout and settings tests, the follower refusal inventory
  (`TestRootMemberFollowerRefusesUnforwardedMutations`) and the route bound
  (`TestMemberForwardRouteCoversEveryForwardedRoute`,
  `TestRootMemberFollowerRequestLifetimeBound`).
- `light02.log`: `go test -race -count=1 ./internal/api ./internal/servingauth`
  passed (285.3 s and 98.3 s), including `TestSettingsForwardSeam` and
  `TestForwardedSettingsRefusalClassification`.
- `process01.log` (before the final lint rename) and `process02.log` (final
  tree): `KUDURRU_E2E=1 go test -run '^TestEnrolledClusterProcesses(PartitionCatchupAndRestart|PromoteAndRejoin)$' ./test/e2e`.
  Between two real processes, a settings change through the follower converges
  on both nodes, an invalid value is the primary's relayed 400, the session
  that the follower created is signed out through the follower (cookies
  cleared, a saved copy of the cookie refused by both nodes), and a settings
  patch while the follower is partitioned answers an explicit 503 and is never
  applied or replayed after reconnection. process01: both passed (116.4 s and
  122.7 s). process02: both passed again (110.9 s and 123.9 s; 245.8 s for the package).
- `lint03.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./internal/api/...`, `./internal/servingauth/...` and `./test/e2e/...`:
  0 issues. `drift01.log`/`drift02.log`: the drift tests pass after the
  generated commit.
- Review fixes (after an independent review of a7c85692):
  - `fix-race01-prefix.log`: the new
    `TestRootMemberForwardSettingsBackToBackActualHTTP` with the post-commit
    wait disabled fails as the reviewer measured: the second patch right
    after the first is `409 conflict` (3.49 s). This is the retained failed run
    that shows the test fails without the fix.
  - `fix-race02.log`: `go test -race -count=2 -run '^(TestRootMemberForwardSettingsBackToBackActualHTTP|TestRootMemberForwardSignOutFailuresActualHTTP|TestMemberLogoutFailureAnswers|TestRootMemberLogoutCommandActualHTTP|TestRootMemberSettingsCommandActualHTTP|TestRootMemberForwardSettingsAndSignOutActualHTTP|TestMemberForwardRouteCoversEveryForwardedRoute)$' ./cmd/kudurru`:
    7 tests passed twice each, 0 skipped, 70.2 s. The back-to-back test (6.0 s
    and 6.6 s) sends two patches and then a blocking switch through the
    follower with no retry: 200, 200, 202, and the follower serves each
    patch at once. `TestRootMemberLogoutCommandActualHTTP` now also revokes a
    live session of a second local account with the first account's cookie:
    a retained 404, and neither session ends.
    `TestRootMemberForwardSignOutFailuresActualHTTP` (6.7 s and 7.4 s), through
    the follower's mounted API: a sign-out of a session that the primary
    already revoked is the relayed 401 and clears the cookies; with the
    primary's writer stopped, `DELETE /auth/sessions/{id}` answers
    `503 completion_unknown` and keeps the caller's cookies, and a sign-out
    answers `503 completion_unknown`, clears the cookies and says that the
    session may stay valid; both saved cookies stay accepted by the follower
    and neither session is revoked on the primary.
    `TestMemberLogoutFailureAnswers` pins the unsent (`unavailable`),
    unknown-outcome and refusal answers, including the unsent branch, which
    no mounted fixture reaches.
  - `fix-race03-owner-mutation.log`: with the owner check removed from
    `rootMemberLogoutDomain.Execute` (existence check only), the
    foreign-account case fails (`200 {"revoked":true}`). The source was
    restored before any commit.
  - `fix-light01.log`: `go test -race ./internal/servingauth` passed
    (91.4 s) after `AuthenticateCurrentWithClock` stopped verifying each
    cookie twice.
  - `fix-process01.log`: `KUDURRU_E2E=1 go test -run '^TestEnrolledClusterProcesses(PartitionCatchupAndRestart|PromoteAndRejoin)$' ./test/e2e`:
    both passed (105.5 s and 120.8 s; 240.4 s for the package). The process
    test now sends two settings patches through the follower back to back,
    both `200`, and signs a second follower session out during the
    partition: `503` with key `unavailable` or `completion_unknown` and a
    message that the cookie was cleared, an empty cookie jar, and the saved
    cookie still accepted; after reconnection both nodes still accept it, so
    the sign-out was neither applied nor replayed.
  - `fix-lint01.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
    `./internal/servingauth/...` and `./test/e2e/...` at 17246f3a: 0 issues;
    `gofmt -l` empty. No route, DTO or schema changed, so nothing was
    regenerated.
- Retained failed runs: `race01.log` (an early harness that read the
  generation before publication: `serving: generation pending`, fixed in the
  test), `race03.log` (the mounted follower test answered
  `503 primary_required` for PATCH /settings before the classification moved
  to `forwarded`, which shows that the test fails without the change),
  `light01.log` (the follower schema test and the OpenAPI drift before the
  classification and the regeneration were finished) and `lint01.log` (a
  helper named `clear` shadowed the builtin; renamed in its own commit).

Open: self-service password, API tokens and TOTP, the allow/deny domain
commands and unblock decisions still answer `503 primary_required` on a
follower. The process test covers settings, sign-out and a partitioned
sign-out, not `DELETE /auth/sessions/{id}` (that route is covered through the follower's
mounted API in process). An unreachable primary makes a sign-out answer
`completion_unknown`, not `unavailable`: the member client treats every failed
POST as an unknown outcome, so `unavailable` appears only when the follower
never attempts the exchange. A forwarded blocking switch whose settings
precondition is stale still answers a generic `503` instead of the primary's
`409`; that is the existing FWD-BLOCKING relay, not changed here, and the
settings wait above removes the case that this package caused. Forwarded
settings on the rejoined old primary after a promotion is not asserted in the
process test. The dashboard does not yet consume the
new forwarded operation IDs. The full `./cmd/kudurru` package was not run
here; the integrator runs it through `hack/go-test.py`.
