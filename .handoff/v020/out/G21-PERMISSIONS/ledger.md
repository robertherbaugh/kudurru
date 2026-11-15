### G21-06 Permission table reconciliation and a reverse lint — November 10, 2026 (D393)

The permission table, the role file and the routes now agree. `GET
/api/v1/queries/export` declares `querylog.export` instead of `querylog.read`, so
the download follows the D31 role table: `auditor` and `admin` hold the verb,
`operator` keeps reading the log through `GET /queries` but is refused the export
with 403 and `missing: querylog.export`. No role or generated ClusterRole changed.
The dashboard shows the export link only with `querylog.export`.
`POST /api/v1/queries/{id}/actions` still declares `blocklists.write` (the list
edit is the effect), and the handler also requires `querylog.read` before it reads
the logged query, on both the legacy and the domain command path, audited on
denial and without an existence oracle. The import-run reads (`GET
/imports/existing`, `/imports/runs`, `/imports/runs/{id}`) stay on
`settings.write`; `docs/architecture/access.md` now says they are admin-only. The
design record's `settings.operatorSeesQueryLog` setting does not exist in code;
the operator always reads the query log (recorded in D393 for the integrator to
correct in the design record).

`permissions.yaml` gains a `reserved:` map of the 17 declared verbs that nothing
checks yet, each starting with its target release. Eleven are v0.2 surfaces the
tracker still requires (design record section 10 lists CRUD `/iam/*` and
`/integrations` as v0.2): `groups.read/write/delete`, `users.delete`,
`serviceaccounts.write/delete`, `identityproviders.write/delete` (IAM write API,
G12) and `integrations.write/delete/secrets` (G21-09). The other six
(`zones.sign`, `zones.share`, `clients.write`, `tokens.write`, `cluster.drain`,
`edge.write`) target v1.0; edge blocks use their own `edgeblocks.*` verbs, so no
v0.2 lane owns `edge.write` (`LoadPermissions` validates
the `vN.N: why` form and that each entry is a declared verb). The new reverse lint
`TestPermissionVerbsAreUsedOrReserved` collects uses from data: the mounted route
table (`Server.Routes()`, including multi-resource routes) and the handler checks
it finds by parsing the production Go sources under `internal/`, `cmd/` and `ee/`
(test files skipped). The scan credits only known check shapes: adjacent
string-literal resource/verb arguments to `require`, `allowed`, `authorize`,
`permission`, `zoneRequirement` or `servingHTTPCluster`, and string-literal
`Resource`/`Verb` fields of `perm`, `rbac.Attributes` or the auth stack `Route`
literals (including elided literals in slices of those types). A pair of the same
shape anywhere else (a `Missing{}` hint, an unrelated `f("cache", "read")`) is not
credited and fails the lint, so a new check helper is added to the scan on
purpose. It fails for a declared verb that is
neither used nor reserved, and for a reservation that is already in use, so
routes and permissions added by other packages are covered when they land.

Evidence under `.context/v020-final-20261110/evidence/G21-PERMISSIONS/`:
`unit01-without-change.log` is the new tests run before the route and handler
changes: `TestQueryExportRoleMatrix` (viewer, zoneadmin, operator subtests),
`TestQueryActionNeedsQuerylogRead` (legacy and command) and
`TestPermissionVerbsAreUsedOrReserved` (querylog.export unused) all fail.
`unit02.log`: with the change, `TestQueryExportRoleMatrix`,
`TestQueryActionNeedsQuerylogRead`, `TestPermissionVerbsAreUsedOrReserved`,
`TestLintVerbUseCatchesUnusedAndStale`, `TestRoutePermissionLint`,
`TestLintRoutesCatchesMissingDeclaration`, `TestOpenAPIDrift`,
`TestOpenAPIServedAndDocs`, `TestGeneratedClient` and the existing query export
tests pass (2.6 s). `race03.log`: `go test -race` over `internal/api` (657.8 s), `internal/rbac`, `ee/rbac` (includes `TestAggregatedRolesDrift`) and `hack/docsgen` (`TestReferenceDrift`) passes, 11m06s wall. `race04-cmd.log`: `TestCLIReferenceDrift` and mounted `TestRunServeQueryExport` pass again (8.8 s). `lint01.log`: golangci-lint over `internal/api/...` and `internal/rbac/...` reports 0 issues. `bench01.log`: 13 of 14 bench guards pass; `test_complete_original_recording_and_limits` fails on the pinned `internal/dnsserver/quic_socket_test.go` hash, which commit c510158d changed in this lane before this package (the PR branch restores it in 2133f47e); this package touches no pinned file.
`race02-cmd.log`: `TestCLIReferenceDrift`, `TestRunServeQueryExport` (mounted,
13.7 s), `TestRunServeSQLiteSourceEntriesRestart` and
`TestServingSourceCommandActualSQLite` pass. `vitest02.log`: the Query log page
tests pass (21 of 21) including the hidden export link without
`querylog.export`; `tsc01.log` exit 0. `gen01.log` is the regeneration (OpenAPI,
Go client, ClusterRoles, dashboard types, endpoints reference, CLI reference); only
the two OpenAPI files and `endpoints.md` changed. Retained failures:
`race01.log` (`TestOpenAPIDrift` before the regeneration, the expected drift) and
`vitest01.log` (vitest could not resolve `vite` before `node_modules` was linked
into the worktree; environmental).

Review fixes: `fix-unit00-without-allowlist.log` is
`TestHandlerPermissionScanCreditsOnlyChecks` (a temporary source tree with a
`Missing{}` hint and an unrelated two-string call) run with the allowlists
disabled, the scan behavior before the fix: it fails because `zones.sign` and
`zones.delete` are credited. `fix-unit01.log`: with the allowlist, the new test,
`TestPermissionVerbsAreUsedOrReserved`, `TestLintVerbUseCatchesUnusedAndStale`
and `TestRoutePermissionLint` pass (1.2 s), and the per-verb use counts are
identical to the review run `review-unit01.log` (63 verbs), so every earlier hit
was an allowlisted check. `fix-unit02-rbac.log` (`internal/rbac`, `ee/rbac`
including `TestAggregatedRolesDrift`), `fix-unit03.log` (the lints and
`TestOpenAPIDrift` after the reservation relabel), `fix-unit04-docsgen.log`
(`TestReferenceDrift`) and `fix-race01.log` (`-race -count=2` over the lint, export
role matrix and query action tests, 23.8 s) pass. `fix-lint01.log` is a retained
gosec G301 on the test's 0o755 temporary directory, fixed to 0o750, rerun in
`fix-lint02.log` (the rerun was still waiting for the shared heavy lock when this ledger was written; the integrator should confirm it reports 0 issues); `fix-unit06.log` reruns the two lint tests after the gosec fix (pass).

Open: the 17 reserved verbs still guard nothing; the 11 v0.2 reservations close
only when G12 and G21-09 build their surfaces or the owner retargets them. When
another package starts checking a reserved verb, the lint fails until that change
removes the reservation line; that is intended. The handler scan does not prove
that the checking code is reachable (a `perm{}` in a registration function that
is never called still counts), and a check whose resource or verb is computed is
not seen and would need a reservation or a route declaration.
The design-record text about `settings.operatorSeesQueryLog` is not corrected
here (the integrator owns that file).
