### FWD-REFUSAL, FWD-NODELOCAL and G21-02 Typed follower refusal, role reporting and node-local cache flush — November 10, 2026 (D368)

An enrolled SQLite follower now treats every API operation with an unsafe
method in one of three ways, kept by operation ID in one reviewed table
(`internal/api/follower.go`) and published in the OpenAPI document as
`x-kudurru-follower`. `forwarded` operations (blocking, the canonical object
routes including `POST /apply`, the kind routes and the five forwarded
`:batchDelete` routes, the four zone commands and local login: 26 operations)
go to the primary as typed member commands, as before. `local` operations (14:
cache flush, query-log purge and rollup verification, `POST /dns/query`, the
node DHCP routes, promotion, activation and join-token, and the two legacy
settings-ownership routes) run on the node. Everything else (53 operations,
and any operation that the table does not name) is refused by a route
middleware that `Server.declare` attaches where every route is registered:
after authentication and the permission check, before the body is read, it
answers `503 primary_required`, naming the primary from the authority that
the request's lease captured, with a hint and no `Retry-After`. A route added
later is therefore refused on a follower until someone classifies it, and the
inventory test fails for any registered mutation without a line. The writer
seams (`currentAuthCommandThen`, `currentAuthCommandOutput`,
`MigrationCommand`), the TSIG and integration mappers and the OIDC/CLI login
admission answer the same key when a follower's request reaches them, and a
DHCPScope object written through a follower is refused the same way.

`GET /cluster` and `kudurru cluster status` (new `ROLE` column) report `role`
(`standalone`, `primary`, `follower`), `primary`, `primaryRequired` and, on a
follower, the forwarded and local operation IDs. The API membership follows
the store's elector, so a follower names its primary as leader (and the list
refresher behind the router would answer its 409 naming the primary), and
`POST /blocklists/{ns}/{name}/refresh` on a follower answers
`503 primary_required` instead of a false `202`. `GET /auth/status` on a
follower carries `role` and `primary` and lists no OIDC provider. The settings
schema marks keys read-only exactly when their PATCH would be refused
(`PATCH /node/config` gets a hint to set log settings in the node's file or
environment). The block page replaces the unblock form with a message on a
follower, fenced or conflicted member, and a posted form answers `503` without
a store attempt. `POST /cache/flush` runs on a follower without a transaction:
it verifies the raw credential against the request's leased authentication
view (`AuthorizeOriginalTransition`), checks that the lease is still current,
and flushes only the node's own cache. A fenced or conflicted member now
answers `503 cluster_fenced` or `503 cluster_conflict` with a hint (the
conflict hint names `kudurru cluster resolution --offline` and
`kudurru cluster resolve --offline`) and no `Retry-After`;
`generation_pending` with `Retry-After: 1` remains for a publication being
prepared. The CLI adds hints for `primary_required`, `completion_unknown`,
`cluster_fenced` and `cluster_conflict`, and `kudurru login` reports a
follower's refusal as a known outcome.

Evidence under `.context/v020-final-20261110/evidence/HA-REFUSAL/`:

- `race01`-`race03-api-follower.log`: development runs of the new
  `internal/api` tests. race01 and race02 are retained failures (the test used
  a wrong schema path, then asserted on a fixture with no writable cluster
  key); race03 passed.
- `race04-api-mutation-expected-fail.log`: mutation check with the router
  middleware removed and `refreshBlockList` dropped from the table:
  `TestFollowerOperationsClassifyEveryMutation` and
  `TestFollowerRefusesEveryUnforwardedMutation` fail as intended (every
  primary operation reached its handler and answered 400/501).
- `race05-root-follower.log`, `race05b-light-packages.log`: first green runs.
- `gen01.log`: `make client-go` (190 operations), `bun run gen:api`, dashboard
  `typecheck` and `make docs-gen`, all exit 0.
- `race06-packages.log`: `go test -race -count=1` over `./internal/api/...`
  (206 s), `internal/servinghttp`, `internal/auth`, `internal/auth/stack`,
  `internal/blockpage`, `internal/cluster`, `internal/apiclient`,
  `hack/docsgen`: all ok.
- `race07-root-regression.log`: root regression selection of 45 test-name
  prefixes in `./cmd/kudurru` under `-race` (member forwarding, serving
  cache/node/settings/domain/unblock/zone/TSIG/blocking, CLI login, real
  router, runServe SQLite follower, TSIG follower, cache flush, local users,
  unblock requests, static twins, unregistered mutations, auth reload):
  ok in 890.7 s.
- `race08-root-new-x3.log`: the new and changed root tests
  `TestRootMemberFollowerRefusesUnforwardedMutations`,
  `TestRootMemberFollowerCommandSeamsNameThePrimary`,
  `TestRootMemberFollowerCacheFlushIsNodeLocal`,
  `TestServingCacheCommandFollowerAndMissingCapture`,
  `TestClusterStatusAndRefusalOnAFollower`, `TestClusterStatusRealRouter`,
  `TestMemberForwardRouteCoversEveryForwardedRoute`,
  `TestCLILoginTOTPSafeStageErrors`, `TestErrorHint`, `-race -count=3`: 27 of
  27 passed, 0 skipped, 39.0 s package time.
- `race09-root-mutation-expected-fail.log`: build overlay removing the router
  middleware, the command-seam refusal and the follower-local cache branch;
  all five follower root tests fail (58 route-level `want 503
  primary_required` failures, cache flush answering `unavailable`, list
  refresh losing the typed refusal), so the tests depend on the change.
- `lint01.log`: retained failure, one gosec G101 on a password literal in the
  new e2e helper; fixed. `lint02-final.log`: `gofmt -l` empty, `go vet` and
  scoped `golangci-lint` v2.15.0 over every touched package: 0 issues.
- `process01-enrolled.log` (before the lint fix) and
  `process02-enrolled-final.log` (final tree): `KUDURRU_E2E=1 go test -race`
  of `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (107.2 s),
  `TestEnrolledClusterProcessesPromoteAndRejoin` (125.0 s),
  `TestEnrolledProcessConflictResolution` (85.7 s) and
  `TestStaticClusterProcessesReadOnlyPeerLossRestart` (16.7 s): 4 of 4 passed
  both times, 344 s package time for the final run. The partition scenario
  now asserts the follower contract between real processes (role and primary
  on `/cluster` and `/auth/status`, one refused mutation per domain with key,
  primary name, hint and no `Retry-After`, no durable effect on the primary,
  list refresh not `202`, local cache flush); the conflict scenario asserts
  `cluster_conflict` with the offline commands and no `Retry-After`.
- `drift01.log`: `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestReferenceDrift`, `TestCLIReferenceDrift`, `TestAggregatedRolesDrift`
  after the `chore(gen)` commit.

Review fixes (accept-with-fixes, five minor findings). Provider login (OIDC
start and callback, CLI prepare) on a fenced or conflicted member now answers
`cluster_fenced` or `cluster_conflict` with a hint and no `Retry-After`, from
the same `servinghttp.AuthorityRefusal` that the lease adapter uses, instead
of `503 unavailable` with `Retry-After: 1`. Refusing a conflicted member
without the capture wait was implemented and then reverted (see `fix-09` to
`fix-11` below); both closed states keep the bounded two-second wait.
`kudurru login --username` reads
`GET /auth/status` and refuses on a follower before it sends the password, so
no orphan session is created on the primary, and the refusal text of a later
login stage no longer claims that nothing was changed. The two import previews
stay `primary` (decided, not changed): each plans an apply that only the
primary runs, and the decommission preview's revision is the primary's
resourceVersion that the decommission checks; the changelog now says that they
used to run on a follower. `TestCLILoginTOTPActualSQLiteLostResponseNeverRetries`
counted every request other than the login as a retry; it now requires exactly
one proof-free `GET /auth/status` before the login and still no request after
it.

- `fix-01-capture-wait-expected-fail.log`: with the conflict shortcut removed,
  its unit test
  `TestCaptureRefusalNamesFencedAndConflictedAuthority/conflict_refuses_without_the_capture_wait`
  failed after 30.0 s, as intended. The shortcut and that subtest were later
  reverted (below).
- `fix-02-expected-fail.log`: build overlay restoring the earlier admission
  and login code: `TestRootMemberClosedMemberProviderLoginNamesDurableState`
  (fenced and conflict: the three login routes answer `unavailable` with
  `Retry-After: 1`) and `TestCLILoginRefusesFollowerBeforeSendingProof`
  (the password was posted) fail, as intended.
- `fix-03-root-race.log`: retained failure. The new tests passed, but
  `TestCLILoginTOTPActualSQLiteLostResponseNeverRetries` failed twice because
  it counted the new status preflight as a retry; the test was made precise
  as described above.
- `fix-04-servinghttp-apiclient-race.log`: `internal/servinghttp` and
  `internal/apiclient`, `-race -count=2`: ok.
- `fix-05-root-race.log`: `./cmd/kudurru` `-race -count=2` over the CLI
  login, OIDC, device, follower, cache, request-budget and cluster-status
  selections plus the new tests: 102 top-level passes, 2 skips (the
  installed-Chrome qualification, which needs an explicit browser), 0
  failures, 402.6 s.
- `fix-06-lint.log`: `gofmt -l` empty; scoped `golangci-lint` v2.15.0 over
  `cmd/kudurru`, `internal/servinghttp`, `internal/apiclient`, `internal/api`:
  0 issues.
- `fix-07-api-follower.log`: `internal/api` follower tests,
  `TestRoutePermissionLint`, `TestOpenAPIDrift`, `-race`: ok.
- `fix-09-e2e-enrolled.log`: retained failure with the conflict shortcut.
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` passed (111.3 s);
  `TestEnrolledProcessConflictResolution` failed at its check that valid
  local DNS keeps answering: right after the API closed, both conflicted
  members answered SERVFAIL and readiness said "complete serving generation
  unavailable: preparing". `fix-11-e2e-conflict-new-x2.log` failed the same
  way twice more (34.8 s, 35.2 s); `fix-10-e2e-conflict-old-capture.log`,
  the same tree built with the earlier capture loop and admission through a
  `GOFLAGS=-overlay`, passed (71.6 s). The immediate refusal reaches the DNS
  check while the DNS-only publication is still being prepared; the two
  seconds per refused request had hidden that window. The shortcut was
  reverted rather than loosening the scenario's DNS assertion.
- `fix-08-drift.log`: `TestReferenceDrift`, `TestAggregatedRolesDrift`,
  `TestOpenAPIDrift`, `TestCLIReferenceDrift` on the final tree: ok. No
  generated artifact changed, so there is no new `chore(gen)` commit.

After the revert (commit `c033fc39`, on top of `929d66ce`, which added the
shortcut):

- `fix-12-servinghttp-race.log`: `internal/servinghttp` `-race -count=2`: ok
  (33.6 s).
- `fix-13-e2e-enrolled.log`: `KUDURRU_E2E=1 go test -race` of
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (108.2 s),
  `TestEnrolledClusterProcessesPromoteAndRejoin` (121.5 s) and
  `TestEnrolledProcessConflictResolution` (68.4 s): 3 of 3 passed, 313.9 s.
- `fix-14-root-follower.log`: `TestRootMemberClosedMemberProviderLoginNamesDurableState`
  and the `TestRootMemberFollower` tests, `-race`: ok.
- `fix-15-lint-servinghttp.log`: `gofmt -l` and scoped `golangci-lint` of
  `internal/servinghttp` on the final tree.

Still open. A conflicted member waits the bounded capture wait before each
refusal (finding 2 of the review, not fixed). Removing that wait first needs a
fix for the window seen in `fix-09` and `fix-11`, where a member that has
just recorded a conflict answers DNS with SERVFAIL while its DNS-only
publication is prepared; that window probably exists today too, hidden behind
the API's waits, and should be checked by the owner of the publication path. Sign-out with own-session revocation, settings and self-service
password and tokens are `primary` in this package; D368 names them as the
next forwarded domains of this lane, and allow/deny domain commands and
unblock decisions may follow. Until then a follower refuses sign-out with
`primary_required`, so a session created on a follower cannot be ended there.
`cluster_fenced` is covered by the lease-adapter test
(`TestCaptureRefusalNamesFencedAndConflictedAuthority/fenced`) and by the
mounted follower runtime over its real TLS listener
(`TestRootMemberClosedMemberProviderLoginNamesDurableState/fenced`), not by a
scenario between processes: no enrolled process test holds a former primary in
the fenced state before its first pull. The dashboard does not yet use `primaryRequired`,
`forwardedOperations`, `localOperations`, the auth-status role or the new
error keys (later dashboard lane). `docs/deploy/sqlite-cluster.md` (docs
lane) still describes `503 unavailable` and a follower naming itself as
leader. The full `./cmd/kudurru` package was not run (integrator runs it
through `hack/go-test.py`).
