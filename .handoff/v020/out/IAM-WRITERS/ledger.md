### IAM-WRITERS IAM writers for local accounts, ServiceAccounts and IdentityProviders — November 10, 2026 (D396)

The complete SQLite runtime now has reviewed writers for every remaining IAM
object (IAM-3, G21-08 API and CLI part). `PATCH /iam/users/{name}` changes the
display name, the local groups or the disabled state of an API-managed account
and `DELETE /iam/users/{name}` deletes it. `POST /iam/serviceaccounts`,
`PUT`/`DELETE /iam/serviceaccounts/{namespace}/{name}` and
`POST /iam/serviceaccounts/{namespace}/{name}/tokens` manage kudurru service
accounts and issue their tokens. `POST /iam/identityproviders` and
`PUT`/`DELETE /iam/identityproviders/{name}` manage provider objects. Every
command works like the RoleBinding commands (D294): the permission and cookie
CSRF are checked on the request's original view before the body is read, the
body is decoded strictly, and one current command reverifies the original raw
credential for each attribute, compares the exact reviewed UID and
resourceVersion and the API manager, stages the object through the security
owners (account or provider revocation floor) and deletes the tokens the change
ends. The object audit row commits with them. A group change, disable or delete
ends the account's sessions and deletes its tokens; enabling restores none. A
service account token is issued only for the exact UID of an enabled account,
needs `serviceaccounts.write` and `tokens.write`, must authenticate from its
stored form inside the transaction, and its secret is only in the 201 answer.
Provider objects carry a credential reference, never a value. The built-in
admin, other managers and a service account with bootstrap authority are
refused by replace, delete and token issuance alike, and an issued token
expires no later than the calling API token (as `POST /auth/tokens`) and within
`auth.tokens.maxLifetime`. `GET /auth/status` reports `capabilities.iamObjectCommands`. On
Kubernetes the commands answer `503 iam_management_unavailable` or
`503 user_management_unavailable`, with a hint and no `Retry-After`, and the new
item reads (`GET /iam/serviceaccounts/{namespace}/{name}`,
`GET /iam/identityproviders/{name}`) answer from the stack's caches. All nine new
mutations are `primary` in the follower table. The CLI gains
`kudurru iam users create|reset-password|update|delete|list`,
`kudurru iam serviceaccounts` (`sa`) and `kudurru iam identityproviders` (`idp`),
each a single attempt without redirect or replay.

Evidence under `.context/v020-final-20261110/evidence/IAM-WRITERS/`:

- `race01.log`: `go test -race -count=1 -v ./cmd/kudurru` selection, 25 tests
  passed in 177.9 s: `TestServingHTTPIAMServiceAccountsReviewedLifecycle`
  (create-only, exact review, token issuance with a stale UID refused, token
  secret absent from the stored image, audit and token inventory, disable
  floor and token deletion, refused issuance while disabled, enable restores
  nothing, delete and recreate with a new UID), `...StrictInputOwnershipAndPermission`
  (duplicate and unknown keys, `spec.token`, server metadata, wrong kind,
  query, CSRF, viewer token 403, other manager 409, bootstrap authority 403,
  stopped writer 503, image unchanged on every refusal),
  `TestServingHTTPIAMLocalUserUpdateAndDelete` (display name keeps the session,
  group change ends cookie and token, disable refuses login, built-in admin
  403, stale delete 409, delete floor, recreated name gets a new UID and no
  token), `TestServingHTTPIAMIdentityProvidersReviewedLifecycle` (no secret
  value accepted or echoed, provider floor on change and delete, other
  commands still commit after the provider set changed),
  `TestIAMWritersCLIReviewedCommands` (full CLI over the real router),
  `TestRunServeSQLiteIAMWritersRevokeAcrossRestart` (mounted `runServe`, 22.6 s:
  a deleted account's token stays refused after a real restart, another
  account's token keeps working, the provider object survives and is deleted),
  `TestForwardedShapes` (Kubernetes stack: every new command answers its keyed
  503 with a hint and no `Retry-After`, capability false, item reads), and the
  unchanged RoleBinding, local-user, external-user and local-user mount tests.
- `race02.log`: the two service account tests again after a lint fix, passed.
- `race03.log`: `internal/apiclient`, `internal/auth/stack`, and the
  `internal/api` follower, route-lint, envelope and RoleBinding tests under
  race, passed.
- `run04.log`: the four fixture tests and `TestForwardedShapes` without race,
  passed. Retained failures: `run01.log`/`run02.log` (first drafts: the
  fixture has no management entitlement, so bindings grant nothing there; a
  PrincipalState name rule; a local user without a role cannot create a
  token; the tests were corrected, not the code) and `run03.log` (build
  failure from an edit during compilation).
- `api01.log`: `TestFollowerOperationsClassifyEveryMutation` failed on the
  reviewed contract size (primary 50 → 59) until it and
  `docs/deploy/sqlite-join.md` were updated.
- `lint01.log` (one ineffassign in a test, fixed), `lint02.log` (0 issues),
  `gen01.log` (openapi, client-go, docs-gen, dashboard gen:api) and
  `drift01.log` (`TestOpenAPIDrift`, `TestReferenceDrift`,
  `TestCLIReferenceDrift`, `TestAggregatedRolesDrift`, route lint, passed).

- Review fixes (commits 6c79f799, 12e0fe15, d6fc4bb6 and 0d969081 on top of
  8bee347e; no generated artifact changed, so there is no new chore(gen)).
  The reviewer's `review-race01.log` (2 of 2 failures of the mounted test,
  `503 generation_pending` on the token POST) and `review-probe01.log` (a
  token issued for a bootstrap-authority and a system-owned account) are the
  retained failures they fix.
  `fix-before01.log`: the extended `TestServingHTTPIAMServiceAccountsReviewedLifecycle`
  (expiry bound: "outlives its issuer: <nil>") and
  `...StrictInputOwnershipAndPermission` (system-owned issuance "got 201,
  want 409") fail against the previous handler (`go test -overlay`).
  `fix-race01.log`: first race run of the IAM selection; all passed except
  `TestServingHTTPIAMWritersDeleteFloorsSurviveReplicaPromotion`, failed by a
  data race on `f.s.opts.Now` written by `newServingHTTPFixture` while the
  runtime runs (`serving_http_test.go:34`). `fix-race02.log`: the same test
  alone, passed. `fix-race03.log`: the race again (in
  `...ConcurrentCreateAndReplace`) and a losing concurrent DELETE answering
  404, which the test now accepts for deletes only. The fixture race is
  pre-existing and is fixed on the PR branch by ebe67588 ("swap fixture clocks
  without racing the runtime"); the lane does not carry that fix.
  `fix-race04.log`: `-race -count=3` over every IAM writer test,
  `TestIAMWritersCLIReviewedCommands` and `TestForwardedShapes`: all 3 runs
  passed for every test (mounted test 27.5 to 36.7 s) except one of three
  `TestServingHTTPIAMWritersDeleteAtSecurityCapacity` runs (503 on a read
  while the 4096-row section was still publishing). `fix-race05.log`: 3 of 4
  passed; the failure is the staging wait (15 s `localUsersFresh`) expiring
  while the full section published. The test now waits up to 90 s for the
  same condition and checks that the delete's 503 is the capacity refusal
  (`"key":"unavailable"`, not `generation_pending`); `fix-race06.log`: 4 of 4
  passed (16.6 to 19.7 s). `fix-race07.log`:
  `TestRunServeSQLiteIAMWritersRevokeAcrossRestart` under `-race -count=5`
  at load average 10 to 13: 5 of 5 passed (26.6 to 31.9 s), so 8 of 8
  across `fix-race04.log` and `fix-race07.log`. `fix-lint01.log`: golangci-lint on
  `./cmd/kudurru/...`, 0 issues. `fix-bench01.log`: bench guards OK. `fix-race08.log`: after the
  0d969081 refactor, every IAM writer test, the CLI test, the mounted test and
  the seven RoleBinding tests under race, all passed, no data race;
  `fix-lint03.log`: 0 issues. `fix-lint02.log`: an attempt to run the PR
  branch's `TestPermissionVerbsAreUsedOrReserved` through `-overlay` did not
  build (it needs the PR branch's `rbac.LintVerbUse`), so the lint itself was
  not run here.
  New tests: `TestServingHTTPIAMWritersConcurrentCreateAndReplace` (same create,
  reviewed replace and IdentityProvider delete twice at once; each commits
  exactly once), `TestServingHTTPIAMWritersOriginalFinalClockAndCancellation`
  (expiry, rollback and cancel, noop and changed: the floor is the original
  verification time and the image is unchanged; a released lease is not
  repaired), `TestServingHTTPIAMWritersDeleteFloorsSurviveReplicaPromotion`
  (service account and provider delete floors and the token deletion survive
  replica install, restart and promotion; the promoted owner refuses the old
  review), `TestServingHTTPIAMWritersDeleteAtSecurityCapacity`. The mounted
  test now waits on an independent GET between commands and never retries a
  mutation.

What remains open: the dashboard forms (create, edit, delete and token display
for users, service accounts, role bindings and providers on the Access page,
with vitest and Playwright specs) belong to the dashboard lane. The merge must
remove `users.delete`, `serviceaccounts.write`, `serviceaccounts.delete`,
`tokens.write`, `identityproviders.write` and `identityproviders.delete` from
`reserved:` in `permissions.yaml` and add the nine operations and two item
reads to the refusal inventory (`operationRefusals`, capabilities page). There
are no writers on Kubernetes (kubectl stays the writer there, as for
RoleBindings), no `/iam/groups` CRUD (`groups.*` stay reserved), and no OIDC
login against a TLS double through an API-created provider was run: the
provider tests prove the object, floor and publication path, not discovery and
sign-in through the new object. Not proven per writer as in D294: no
actual-TLS router test and no live follower that refuses the revoked
credential (the promotion test proves the image, not a serving follower);
no session or token of an API-created provider shown refused after its
replace or delete (only the provider floor row is asserted); no mounted test
of `PATCH`/`DELETE /iam/users`; the self-change cookie clearing
(`clearOwnCookie`) and a caller holding `serviceaccounts.write` without
`tokens.write` are untested. Account and provider floors are permanent and
share the 4096-row security cap with sessions; nothing prunes them, and a
deleted local user's TOTP record stays too. At the cap, deletes and new
sign-ins answer `503 unavailable` and commit nothing, with no API recovery.
Pruning floors of UIDs that can no longer authenticate needs its own design
(it relaxes the monotonic-floor rule of the security owner). Publishing a
section near the cap takes 15 s or more under the race detector. Integration:
for the PR branch's reverse permission lint (D393,
`TestPermissionVerbsAreUsedOrReserved`), 0d969081 replaced `iamAttrs` with
`iamTarget(servingHTTPCluster("<resource>", "<verb>"), namespace, name)` and
made every verb a literal, so only known callees carry the pairs (not run
against the PR lint, see `fix-lint02.log`). The merge must still remove the six
reservations listed above, and the owner must confirm that
`tokens.write`, reserved there for v1.0 ("issuing API tokens for another
principal"), is used in v0.2.
