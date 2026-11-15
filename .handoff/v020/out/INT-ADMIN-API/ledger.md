### INT-ADMIN-API Integration administration API: manifests and Integration writes — November 10, 2026 (D400)

Map entries: INT-5 (`map-packages-g05-g15-remaining.json`, API, CLI, permission,
follower-contract, audit and docs part) and G21-09 (`map-g21-route-and-contract-sweep.json`,
"Integration object writes" part). The dashboard Integrations page is not part of
this package (see the last paragraph).

What now happens. `Integration` is a kind of the generic object routes on both
drivers: `GET/PUT/DELETE /api/v1/objects/integrations/{namespace}/{name}`,
`POST /api/v1/objects/integrations`, `POST /api/v1/apply` and the new
`POST /api/v1/integrations:batchDelete`, and the CLI's `kudurru get|apply|delete
integrations` (alias `intg`). A write takes the same reviewed path as every other
object: the document is sanitized (status and server metadata dropped) and
validated strictly against the CRD schema, written as manager `api`, refused with
`409 managed_by` when a file or another manager owns the object, and bound to the
stored `resourceVersion` by `If-Match` (`412` otherwise). On the complete SQLite
runtime the write runs in one current command: the original credential is
re-authorized against the attributes the command derives from its own read, the
store arms the `deletionPolicy: Delete` cleanup finalizer (D367) and writes the
`integration.create`/`update`/`delete` audit row in the same transaction. An
enrolled follower forwards these writes to its primary as the typed object member
command (`batchDeleteIntegrations` is a new forwarded operation; the provider
commands check, preview, sync, act and release stay primary-only). On Kubernetes
the API reads the object, checks, and applies with `ExpectedResourceVersion`;
the kube audit spool records it. Two checks are specific to the kind, both
decided on the stored object the write is compare-and-swapped against: a write
that binds or changes `spec.credentials` also needs `integrations.secrets`
besides `integrations.write` (a reference can name any credential the node's
allowlist grants), and so does a write that, while credentials are bound,
changes where the host presents them (`spec.type`, `spec.unifi.endpoint`,
`spec.unifi.tlsFingerprint`, `spec.webhook.endpoint`), so a writer without the
verb cannot point the node's credential at its own server, and a new Integration needs the integrations license
feature (`402 license_required`, re-checked at commit on SQLite); replace and
delete need no license. `integrations.write`, `integrations.delete` and
`integrations.secrets` are no longer reserved. Credentials stay references:
the CRD has no value field, strict decoding refuses one (`400`), and reads
return the references only (a holder of `integrations.read` sees the reference
names in use; the manifests route withholds only the rest of the allowlist).
A validation failure of an Integration write that names `spec.credentials` is
answered with a fixed message and the field paths only, because the validator
prints the refused value `%q`-quoted; every other refusal has each
`spec.credentials` string removed in its raw, Go-quoted and JSON-escaped forms.
A value pasted into `file` or `env`, also with a trailing newline, a tab, a
quote, a backslash, a control character or under six bytes, is therefore not
echoed by `PUT`, `POST /apply`, a follower or a log (the first version matched
raw substrings only and leaked those values; review probe
`review-probe02.log`). `GET /api/v1/integrations/manifests` (`integrations.read`,
`kudurru integrations manifests`) returns the compiled provider manifests
(configuration schema, credential keys with `required`, capabilities, I/O
class), the driver, the only namespace whose Integrations run (Kubernetes
release namespace), license coverage, the reference forms this node resolves
(`file`, `env`), and per type `supported`/`reason` and the `refused` spec fields
of this driver (Kubernetes: webhook unsupported, `spec.unifi.inventory`; both
drivers: UniFi push fields). The node's allowlisted file paths and environment
names are included only for a holder of `integrations.secrets`. `kudurru backup`
and a kind-less `kudurru export` keep covering only the DNS objects (they would
otherwise start needing `integrations.read`); `kudurru export integrations`
exports Integrations.

Evidence under `.context/v020-final-20261110/evidence/INT-ADMIN-API/`:

Review fixes (November 13, 2026):

- `fix-prefix01.log`: the new tests run against the reviewed code (source files stashed): `TestIntegrationObjectWrites`, `TestIntegrationObjectCommandAdmission` and `TestIntegrationNeedsSecrets` fail; the first shows the 400 body repeating `"trailing-newline-token-0123456789\n"`, the last reports the endpoint, TLS pin, type and webhook endpoint changes as needing no `integrations.secrets`.
- `fix-race02-api.log`: `go test -race -count=1 -v -run 'TestIntegration|TestObjectCommand' ./internal/api` with envtest, 11 of 11 passed: `TestIntegrationObjectWrites` (1.3 s), `TestIntegrationObjectWritesKube` (10.1 s, the same sequence over kube-apiserver), `TestIntegrationObjectCommandAdmission`, `TestIntegrationNeedsSecrets`, `TestIntegrationCredentialsChanged`, `TestIntegrationManifestsRoute` and the object-command tests. The write sequence now sends six echo values (trailing newline, quote and backslash, tab, control character, `abc!5`, non-ASCII) through `PUT` and `POST /apply` and checks every string of the answer for the raw, Go-quoted and JSON-escaped form; the command path checks the same values; a `writer` without `integrations.secrets` gets `403` for a UniFi endpoint or TLS pin change and `200` for a suspend, an admin moves the endpoint; `POST /apply` refuses a credential binding to the writer and creates and updates as admin; `POST /integrations:batchDelete` is `403` for an operator and deletes two of three names for the writer. `fix-race01-api.log` is a retained earlier run of the same selection without envtest assets, which failed only on that test's wrong expectation of a per-item operator refusal (the route answers `403` as a whole).
- `fix-race03-api-full.log`: `go test -race -count=1 ./internal/api` with envtest, `ok` in 642.2 s (includes `TestOpenAPIDrift`, the follower contract tests and both permission lints).
- `fix-race05-cmd.log`: `go test -race -count=3 -v -run 'TestIntegrationAdminObjectWritesOnCompleteSQLite|TestIntegrationsObjectCLI|TestIntegrationsManifestsCLI' ./cmd/kudurru`, 9 of 9 passed (SQLite test 13.2 s to 14.9 s). The mounted SQLite test now also refuses an unlicensed create with `402 license_required` from the current command and stores nothing, finds `kudurru.io/integration-cleanup` armed on a `deletionPolicy: Delete` create, gets a per-document `403` for an operator's `POST /apply`, creates through `POST /apply` and removes through `POST /integrations:batchDelete` (one success, one missing). `TestIntegrationsObjectCLI` covers `get integrations`, `get intg`, `-o wide`, `apply -f`, `delete integrations`, a kind-less `export` and `backup` that send no Integration list request, and `export integrations`; `TestIntegrationsManifestsCLI` covers the table and `-o json`. `fix-race04-cmd.log` is retained: the same tests plus `TestBackupAndRestore`, `TestConvenienceGolden`, `TestApply*` and the attribute tests passed, but the SQLite test failed with `503 generation_pending` because the new steps sent a request before the cleanup create was published; the test now waits for the published generation, as its other steps do.
- `fix-mutation01-nobackup.log`: with `NoBackup` set to false, `TestIntegrationsObjectCLI` fails (the kind-less export includes the Integration).
- `fix-lint02.log`: golangci-lint v2.15.0 over `./internal/api/... ./cmd/kudurru/...`, 0 issues (`fix-lint01.log` retained: refused to start beside another lane's lint run). `fix-bench-guards01.log`: 14 bench guard tests OK.

Original implementation:

- `race08.log`: `go test -race -count=3 -run 'TestIntegrationAdminObjectWritesOnCompleteSQLite|TestObjectCommandAttributesIntegrationSecrets|TestIntegrationManifestsMatchKubernetesRefusals' ./cmd/kudurru`, 9 of 9 passed (12.2 s to 16.3 s per run of the SQLite test, package 46.4 s). The SQLite test mounts the API on the complete runtime (object commands, captured object reads, the integration host, the license), reads the manifests, refuses an operator-scoped token (`403`), creates an Integration with a file reference, finds its `integration.create` audit row at the returned resourceVersion with no credential value in it, waits for the host to run it and checks it through `POST /integrations/default/second/check` with the UID the write returned (one provider request), replaces and deletes it under `If-Match` (stale `412`, operator delete `403`), and gets `409 managed_by` for a file-managed Integration; no answer contains the credential value.
- `race06-internal.log`: `go test -race -count=1 -p 2 ./internal/api ./internal/cli ./internal/rbac` with envtest assets: every test passed except `TestOpenAPIDrift` (expected before the generated commit; it passes in `drift01-api.log`). This includes `TestIntegrationObjectWrites` (memory store, the Kubernetes write path), `TestIntegrationObjectWritesKube` (the same sequence over kube-apiserver with the Integration CRD, 6.6 s), `TestIntegrationObjectCommandAdmission` (SQLite command: `integrations.secrets` attribute exactly when credentials change, `402` under a community license cut, `412`, no echo), `TestIntegrationCredentialsChanged`, `TestIntegrationManifestsRoute` (`503` without a host, `403` for a viewer, grants withheld without `integrations.secrets`), `TestFollowerOperationsClassifyEveryMutation` (forwarded 34, local 14, primary 49) and `TestPermissionVerbsAreUsedOrReserved`.
- `race05.log`: `TestRootMemberFollowerRefusesUnforwardedMutations` (refused 49, admitted 48) and `TestMemberForwardRouteCoversEveryForwardedRoute` (with `POST /integrations:batchDelete` in the reviewed table) passed 3 of 3; the SQLite object test failed 3 of 3 in that run on a test wait (see below).
- `mutation01.log`: with the `integrations.secrets` check removed `TestIntegrationObjectWrites` fails (a writer without the verb creates the object); with the redaction removed both the ordinary and the command test fail and show the CRD error repeating the pasted value.
- `drift01-api.log` (`TestOpenAPIDrift`, `TestRoutePermissionLint`, `TestPermissionVerbsAreUsedOrReserved`, follower tests) and `drift02-docsgen.log` (`./hack/docsgen`, `TestReferenceDrift`) pass after `chore(gen)`; `race09-cmd.log`: `go test -race -count=1 -run 'TestCLIReferenceDrift|TestAggregatedRolesDrift|TestIntegrationAdmin|TestIntegrationManifests|TestObjectCommandAttributes|TestServingObject|TestMemberForwardRoute|TestRootMemberFollower|TestRootMemberObject|TestBackup|TestExport|TestRestore|TestIntegrationsCLI|TestIntegrationCLI' ./cmd/kudurru` after `chore(gen)`: `ok` in 157.5 s (run without `-v`, so no per-test count is recorded).
- `lint02.log`: golangci-lint v2.15.0 over `./internal/api/... ./internal/cli/... ./cmd/kudurru/... ./ee/integration/...`, 0 issues (`lint01.log` retained: one gosec G602 on a slice index, fixed by `integrationSecretsPair`). `bench-guards01.log`: 14 bench guard tests OK. `gen01.log`, `gen02-dashboard.log`: `make openapi`, `make client-go`, `make docs-gen`, `bun run gen:api`.

Retained failed runs: `race01.log` (`503 generation_pending` on the first request after the test created a token; the test now waits for the published generation), `race02.log` and `race03.log` (the test counted provider requests exactly and reviewed the new instance before its status settled: `409` "review or authority changed"), `race04.log` and `race05.log` (`503` from check and `generation_pending` while the instance and its budget publication were still settling; `race05` waited for a known status the SQLite host does not report for this probe). All were waits in the new test, not product failures; `race08.log` uses `integrationReadyCommand` and waits for the published generation before each request, as the neighboring integration tests do. `race07` was stopped by the tool's background time limit before it ran and has no result.

What remains open. The dashboard Integrations page (manifest-driven forms, conditions, check/sync, the Cloudflare preview plan with digest-bound apply, the inventory view, zone/record publication conditions) and its vitest and Playwright specs are a later dashboard package; this package exposes the routes and fields it needs. The Kubernetes write path has no original-credential proof re-checked at commit (as for every Kubernetes object write); a permission withdrawn between the check and the apply does not stop that apply. No custom role in the SQLite RBAC holds `integrations.write` without `integrations.secrets`, so the split is proven at the API seam with a custom authorizer and on SQLite through the command's attribute check, not with a second built-in role. The destination fields are a fixed list (`integrationDestinationFields`); a provider that later adds a field that decides where a credential is sent must add it there. A holder of `integrations.read` sees the reference names (file paths, environment variable names) that Integrations use, by design (an object without them could not be re-applied); the dashboard should not present the manifests gate as hiding them. The generated `chore(gen)` commit is no longer the last commit of the lane: the review-fix commits change no route, DTO, permission or CLI reference, and `TestOpenAPIDrift` passes in `fix-race03-api-full.log`. Follower forwarding of Integration writes uses the generic object member command and is covered by the route-table and refusal tests, not by a two-member Integration write test. The driver limits in the manifests are a table in `cmd/kudurru/integration_manifests.go`; the Kubernetes half is pinned to `kubeIntegrationRefusal` by a test, the SQLite UniFi push entry is not, and INT-UNIFI-PUSH must update it.
