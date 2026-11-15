### G22-WP6/G23-R3/G23-R4/G24-SYNCTEST hosted qualification gaps and prebuilt-artifact switches — November 10, 2026 (decision row without a number in `out/QUAL-CI/decision.md`)

Lane commits on `wip/v020/qual`, base `33ee04bc`. First pass: `3b0cb519`,
`a2726146`, `f788e7b6`, `b8ca0208`, `00c41d60`, `b162b1aa`. Review-fix pass on
top: `49aae9ee` (`fix(dns)`, the one product change), `06f921b9` (`test(e2e)`),
`52aee771` (`ci(e2e)`), `2ac0d76f` (`ci(helm)`), `bac6e7ab` (`test(kind)`) and
`0048c606` (`docs(architecture)`, line breaks of one paragraph only; the
`fix-*` logs of the final runs name `bac6e7ab`, `fix-unit03-wider.log` names
`0048c606`). No generated file, no new dependency, `go.mod` unchanged. Nothing
below ran on GitHub Actions: this sandbox cannot run workflows, so the hosted
proof is the first run of the ci workflow with these commits.

What now happens:

- **The hosted e2e job has every program its tests can skip without, and fails
  instead of skipping.** It gains the PostgreSQL 17 service and the envtest cache
  of the go job, runs the new `make interop-internal` after `make e2e`, and sets
  `KUDURRU_REQUIRE_BIND`, `KUDURRU_REQUIRE_POSTGRES` and `KUDURRU_REQUIRE_ENVTEST`.
  The first and the last are new (the `KUDURRU_REQUIRE_POSTGRES` rule of D158):
  `kudurrutest.Named` and `kudurrutest.BINDTool` are the one lookup for `named`
  and the dnssec tools in `test/interop`, `internal/rpz`,
  `internal/stages/authoritative` and `internal/zoneview`. Eight tests that ran in
  no hosted job now run there: `TestBINDRPZRuntimeTransfersRefreshRestart`,
  `TestBINDRPZFallbackAndAuthentication`, `TestBINDRPZPolicyReference`,
  `TestBINDSignedSecondaryThroughListeners`,
  `TestBINDStubThroughListenersAndDurableOfflineReferral`,
  `TestCentralAuditTwoNodes`, `TestCentralQuerylogTwoNodes` and
  `TestKubeAuditAcrossReplicas`. `TestKnotCatalogConsumer` is the only test of the
  job that still skips (opt-in, private Docker daemon; G23-R5).
- **The internal BIND step no longer depends on `make e2e` passing** (review
  fix). Its condition is
  `!cancelled() && (success() || steps.e2e.conclusion == 'failure')`: it runs
  when everything before it succeeded or when the end-to-end step itself
  failed, not after a failed setup step or a cancellation. One failing
  end-to-end test therefore no longer removes the five G23-R3 result lines from
  that run's log. The three later steps of the job (recursor against BIND, dex,
  AdGuard Home) keep their default condition, as before this package.
- **A census of the hosted log must read subtest lines.** With `named` hidden and
  no require variable, `TestBINDRPZRuntimeTransfersRefreshRestart`,
  `TestBINDSignedSecondaryThroughListeners` and
  `TestBINDStubThroughListenersAndDurableOfflineReferral` report `--- PASS` while
  every one of their subtests reports `--- SKIP`. With `KUDURRU_REQUIRE_BIND=1`
  the same run fails all five tests.
- **A node answers an upstream failure across a configuration commit** (review
  fix, the one product change; decision row in `out/QUAL-CI/decision.md`). Run
  with PostgreSQL for the first time, `TestCentralQuerylogTwoNodes` showed that
  a query whose answer is an upstream-failure SERVFAIL got no answer at all when
  it arrived right after a canonical commit (an API token created through the
  admin socket): the query ran on the retired data generation, and the output
  guard of D332 kept forwarded and policy-blocked replies, the empty unsynced
  SERVFAIL and ingress identity, but not the failure of the forwarding path
  (`write failed: serving: dependency changed`). The client waited for its own
  timeout. `retainedDNSResponse` now also keeps that refusal when it is empty:
  status `servfail_upstream` or `timeout_upstream`, response code SERVFAIL, AA
  clear, no answer or authority records, nothing but OPT in the additional
  section, origin unattributed or failure. Every other check of the retained
  path is unchanged. The first pass had moved the test to a fake resolver
  instead; the test forwards to the discard port again, as at `33ee04bc`, and
  additionally requires the SERVFAIL response code.
- **The three un-skipped end-to-end tests, as they now stand.** All three failed
  at `33ee04bc` when run with PostgreSQL and envtest:
  - `TestCentralAuditTwoNodes` looked for `blocking.toggle` under
    `GET /audit?kind=blocking`. That expectation was stale, not a product
    defect: on SQLite the blocking command is one write of `Kudurru/default`
    `status.blocking` audited in its own transaction as `kudurru.status`, and
    `Options.AuditBlockingCommand` (`internal/api/api.go:339`, set only for
    the Kubernetes driver at `cmd/kudurru/serve.go:1352`) records
    `blocking.toggle` only where the driver cannot audit in its transaction.
    The test reads the `kudurru.status` rows. Review fix: enabling is exercised
    again. node-a pauses for 30 seconds, node-b pauses without a timer and then
    enables blocking, and both nodes must show node-a's pause, node-b's pause
    and node-b's enable (`status.blocking.disabled` true to false) from the
    central database.
  - `TestCentralQuerylogTwoNodes`: see the product change above. Its original
    assertion is restored and strengthened.
  - `TestKubeAuditAcrossReplicas` never provisioned AuditLog trust. This guard
    was also stale: F73 makes that trust operator-provisioned ("explicit upgrade
    provisioning required", `completion-plan.md:206`; `docs/deploy/runbooks.md:291-294`;
    `hack/kind-e2e.sh` does the same), and every replica answered
    `audit keys: trusted verification material unavailable`
    (`internal/audit/auditkeys/ring.go:33`). The test runs
    `kudurru audit keys init` for the envtest cluster and creates the
    `kudurru-audit-keys` Secret. No assertion of this test changed.
- **The harnesses run on prebuilt artifacts and name them.**
  `KUDURRU_E2E_LICENSED_BINARY` names a prebuilt `-tags licensetest` binary for
  the six licensed tests, which previously rebuilt it in every test and ignored
  `KUDURRU_E2E_BINARY`; a binary whose build information lacks the tag is refused
  before any node starts. Without the variable the binary is built once per run.
  `kudurrutest.Describe` logs, for every binary, the `kudurru version` output, the
  SHA-256 of the file, and the build tags, cgo setting and commit recorded by the
  Go toolchain; `MainBuild` prints it per package and each licensed test logs it.
  `hack/kind-e2e.sh` honors `BUILD=0 IMAGE=<repository:tag>`; it and
  `hack/compose-e2e.sh` print `kudurru version` and the image ID of the image
  under test.
- **`hack/kind-e2e.sh` refuses an unusable image before it creates a cluster**
  (review fix). `IMAGE` has to be `repository:tag` in both modes, because the
  chart receives the two halves separately: a digest reference or a reference
  without a tag exits 2 before anything runs. With `BUILD=0` an image that the
  local daemon does not have exits 2 instead of being pulled by `docker run`.
  In both modes the script runs `license verify -` in the image with the test
  license key and exits 2 when the image does not accept it, which is what a
  build without `-tags licensetest` does. A run that stops before
  `kind create cluster` no longer prints cluster diagnostics (they would
  describe whatever cluster the current context names) or deletes a cluster.
- **`make test` hands the installed envtest binaries to every Kubernetes suite**
  (not in the map; found while adding the envtest step). The go job installed
  them into `bin/k8s`, but `internal/api`, `internal/auth/k8s`, `internal/cluster`,
  `internal/controller` and `internal/zonecache` read only `KUBEBUILDER_ASSETS`,
  which nothing exported. `TestLeaseHandoff`, `TestKubeColdStart`,
  `TestKubeDriver`, `TestDeleteZoneKube`, `TestServiceAccountTokens`,
  `TestSubjectAccessReview`, `TestStackEndToEnd` and `TestIdentityOnKubeStore`
  have therefore skipped in every hosted run. The recipe now exports the newest
  `bin/k8s/<version>-<os>-<arch>` (a value from the environment wins) and, with
  `KUDURRU_REQUIRE_ENVTEST=1` (set in the go job), stops before the run when
  there is no kube-apiserver.
- **The publish workflow installs envtest before `make test`** (not in the map).
  `internal/primaryruntime`, new in v0.2, fails without the binaries, so the test
  job in front of publishing would have failed on the first v0.2 tag.
- **G24-SYNCTEST closes no gap; its premise was false** (corrected after
  review). `hack/test_sync_version.py` already ran in every hosted go job and in
  the test job of `publish.yml`: `deploy.TestVersionSyncScript`
  (`deploy/release_test.go:340-346`) executes it and fails when a Python test
  fails, and the hosted go log of run 42736964083 has
  `ok github.com/robertherbaugh/kudurru/deploy`. The map's grep missed the Go
  caller, and commit `00c41d60` repeated the claim ("which no workflow ran").
  The step this package added to the helm job stays as a second, visible run of
  the same five tests (the go job has no `-v`, so the first run leaves no line of
  its own); commit `2ac0d76f` corrects its comment, and the G24-SYNCTEST entry of
  `map-g24-g25-release-docs.json` has a `correction` field. The tracker should
  say "already covered by `TestVersionSyncScript`; a visible duplicate step was
  added", not "now runs in CI".

Evidence under `.context/v020-final-20261110/evidence/QUAL-CI/` (BIND 9.18.50,
PostgreSQL 17.11 started natively on a private port, envtest 1.37.0, Go 1.27.3;
hosted uses BIND 9.18.39 and the `postgres:17-alpine` digest of the go job).
Logs named `fix-*` are from the review-fix pass, `review-*` from the reviewer,
the others from the first pass.

Review-fix pass:

- Upstream failure across a commit, before and after. `fix-e2e01-before.log`: a
  binary built with the product file of `b162b1aa` (through `go build -overlay`)
  fails both tests that need the change. `TestCentralQuerylogTwoNodes` in its
  restored form: `query node-b: ... i/o timeout` (9.93 s), the failure of
  `e2e01`. `TestUpstreamFailureAnsweredAcrossCommits`: `commit 1 of 5, query 16:
  no answer: ... i/o timeout` (5.50 s). `fix-e2e02-after.log`: the fixed binary,
  both tests with `TestCentralAuditTwoNodes` and `TestKubeAuditAcrossReplicas`,
  twice: eight passes, no skip, 110.1 s; the regression test logged 967 and 964
  queries answered SERVFAIL across five commits. `fix-e2e02b-precommit.log`:
  the three changed tests after their last edits, three passes, 31.4 s.
- The output guard. `fix-unit01-before.log`: with the product file of
  `b162b1aa` overlaid, `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation`
  fails in exactly the five new subtests that must be answered
  (`upstream_failure`, `upstream_timeout`, `failed_target`,
  `actual_upstream_failure_empty` and the same with EDNS); the other 25
  subtests pass, among them the 13 new ones that must stay refused (an
  upstream-failure status with a success code, with an authoritative, local,
  mixed or synthesized origin, and the real failure with an answer record, an
  authority record, an additional record or AA added, each with and without
  EDNS). `fix-unit02-after.log`: 13 top-level tests and 40 subtests pass, none
  fails or skips, 143.9 s: the guard test and its neighbors
  `TestRootUpdateOutputActualCompletionAndFinalRefusals`,
  `TestRootUpdateOutputDoesNotRelaxOrdinaryDataGeneration`,
  `TestRootAnchorOutputActualBlockedWriteDrainsBeforeFirstManagedCommit`,
  `TestRootAnchorOutputOriginalLifetimeAndTerminalFailure`,
  `TestAnchorCanonicalLossDrainsOutputAndSurvivesActualRestart`, the three
  `TestServingReload*`, both `TestRunServeUniFiHostname*` (the held queries of
  D332) and the two `TestPrimaryRuntime*` UPDATE tests. The "actual" subtests
  run the real upstream stage against a closed loopback port.
- The final head `bac6e7ab` on a clean tree, with the environment of the hosted
  e2e job (three require variables, DSN, `KUDURRU_NAMED`).
  `fix-e2e03-make-e2e-hosted-env.log`: `make e2e`, exit 0; `test/e2e` 313.6 s,
  `test/interop` 27.8 s; 24 top-level tests pass, none fails, one skips
  (`TestKnotCatalogConsumer`); 52 subtests pass, none skips.
  `TestCentralAuditTwoNodes` 15.89 s, `TestCentralQuerylogTwoNodes` 9.82 s,
  `TestKubeAuditAcrossReplicas` 24.07 s,
  `TestUpstreamFailureAnsweredAcrossCommits` 4.92 s (921 queries),
  `TestEnrolledClusterProcessesPromoteAndRejoin` 75.57 s (it passed here; see
  the retained `review-05`). `fix-bind01-interop-internal.log`:
  `make interop-internal`, exit 0: the five tests and their 25 subtests pass,
  no skip (`internal/rpz` 2.2 s, `internal/stages/authoritative` 1.4 s,
  `internal/zoneview` 1.2 s).
- `fix-build02-head.log` and `fix-e2e04-prebuilt-head.log`: the four changed or
  new tests on binaries built with the release flags from `bac6e7ab`
  (`make build`, 81,146,016 bytes, under the 100,000,000 budget; SHA-256 in the
  build log): four passes, no skip, 54.6 s; the licensed tests log
  `kudurru 0.2.0 (bac6e7ab, ...)`, the checksum, `tags licensetest` and the
  full commit. `fix-e2e05-regression-x5-4cpu.log`: the regression test five
  times under `taskset -c 0-3` with `GOMAXPROCS=4`: five passes, 22.7 s, 911 to
  948 queries each.
- `fix-lint01.log`: scoped golangci-lint v2.15.0 on `./cmd/kudurru/...` and
  `./test/e2e/...`, 0 issues. `fix-vet01.log`: gofmt, `go vet` of both
  packages, `CGO_ENABLED=0 go build ./...` and `bash -n hack/kind-e2e.sh` clean.
- `fix-unit03-wider.log`: at the final head `0048c606`, every test of the 23
  `cmd/kudurru` test files that expect a SERVFAIL or serve DNS with an
  unreachable upstream: 54 top-level tests and 85 subtests pass, none fails or
  skips, 395.1 s. Among them are the mounted `runServe` tests of direct anchors
  (bootstrap, follower revocation, recursive DNSKEY), stored TSIG on SQLite and
  Kubernetes, Kubernetes dynamic UPDATE, offline snapshot and recovery, the
  held UniFi queries, and the `TestServingBlockingRefresh*`,
  `TestServingClientExemption*`, `TestServingListScope*`,
  `TestServingMigration*`, `TestServingRuntime*` and `TestServingZoneNotify*`
  publication tests.
- `fix-kind01-stub.log`: `hack/kind-e2e.sh` at the final head with stubs for
  docker, kind, kubectl and helm. The docker stub executes the real release
  and `licensetest` binaries of that head for `docker run`, so the version line
  and the license check are real. Six cases: a digest reference and a reference
  without a tag exit 2 with no command run; `BUILD=0` with an image the daemon
  lacks exits 2 after one `docker image inspect`; `BUILD=0` with the release
  build exits 2 at the license check (`license key is signed with a key this
  build does not trust`), before `kind create cluster` and without cluster
  diagnostics; `BUILD=0` with the `licensetest` build behind
  `registry.example:5000` and `BUILD=1` both reach `kind create cluster` (the
  stub fails there on purpose). The same log lists how accepted references are
  split for the chart.
- `fix-actionlint02-head.log`: actionlint 1.7.12 on `ci.yml` and `publish.yml`
  at the final head, exit 0. `fix-actionlint01.log` has the control: a copy with
  the step id misspelled in the new condition is rejected
  (`property "e2x" is not defined`).
- `fix-deploy02-head.log` (and `fix-deploy01-sync-and-workflow.log` before the
  commits): the helm step (5 tests, OK) and, through Go,
  `TestVersionSyncScript`, `TestWorkflowActionsPinned` and
  `TestGoreleaserConfig` pass.

First pass (the two e2e logs marked superseded ran the first form of the two
central tests):

- `e2e07-make-e2e-hosted-env.log` (superseded by `fix-e2e03`): `make e2e` at
  `b162b1aa` on a clean tree with the environment of the hosted job. Exit 0;
  `test/e2e` 308.4 s, `test/interop` 27.2 s; 23 top-level tests pass, none
  fails, one skips (`TestKnotCatalogConsumer`); 52 subtests pass, none skips.
  All six licensed tests log the same harness-built binary.
- `bind02-interop-internal.log`: `make interop-internal` with
  `KUDURRU_REQUIRE_BIND=1`: the five tests pass (`internal/rpz` 2.2 s,
  `internal/stages/authoritative` 1.4 s, `internal/zoneview` 1.2 s).
  `bind01-baseline.log` is the same result before the change.
- `e2e09-three-on-4-cpus.log` (first form of the two central tests): the three
  un-skipped tests twice under `taskset -c 0-3` with `GOMAXPROCS=4`: six
  passes, 96.2 s.
- `e2e10-licensed-prebuilt-rerun.log` (first form of the two central tests):
  all six licensed tests and `TestCentralQuerylogTwoNodes` on binaries built
  with the release flags from `b162b1aa` (`make build`, 81,146,016 bytes;
  SHA-256 in `build02-prebuilt.log`). Each licensed test logs
  `kudurru 0.2.0 (b162b1aa, ...)`, the checksum, `tags licensetest` and the full
  commit. Seven passes, no skip, 246.6 s.
- Controls. `e2e03`: `KUDURRU_REQUIRE_ENVTEST=1` without binaries fails
  `TestKubeAuditAcrossReplicas`; `e2e04`: without the variable it skips as before.
  `e2e05`: `KUDURRU_E2E_LICENSED_BINARY` naming the untagged release build fails
  both selected tests at once, with the reason, before any node starts. `e2e06`:
  `KUDURRU_REQUIRE_POSTGRES=1` without a DSN fails both central tests.
  `bind05`: a `KUDURRU_NAMED` that does not exist fails the five BIND tests.
  `bind06-control-no-named-required.log` and `-optional.log`: `named` hidden by a
  private mount namespace; required: five failures; optional: the three parent
  passes over skipped subtests described above, and two skips.
- `unit01-kudurrutest.log`: seven new harness tests pass under race
  (`TestUnavailableSkipsUnlessRequired`, `TestNamedSkipsOrFailsWithoutBIND`,
  `TestNamedSearchOrder`, `TestBINDToolSkipsOrFailsWhenMissing`,
  `TestDescribeNamesTheArtifact`, `TestBuildLicensedPrebuilt`,
  `TestBuildPrebuilt`).
- envtest export. `envtest01-unset.json` and `envtest06-unset-api.json`: without
  the variable the eight tests skip. `envtest03`, `envtest04`, `envtest05`: with it
  they pass (`internal/auth/k8s` 15 passes, `internal/cluster` 21,
  `internal/zonecache` 32, `internal/controller` 38 with the opt-in
  `TestMeasureKube` skipped, `internal/api -run Kube` 5). Package times in the
  hosted go logs of run 42736964083 (`hosted-33ee04bc/go1.log`, `go2.log`:
  `internal/cluster` 2.3 s, `internal/auth/k8s` 4.6 s, `internal/zonecache` 12.6 s)
  match the local run without the variable (2.6, 4.5 and 13.4 s), not the run with
  it (7.0, 10.3 and 21.4 s), and the step environment in that log has no
  `KUBEBUILDER_ASSETS`. `maketest01-envtest-export.log`: the recipe in four
  configurations. `envtest02-primaryruntime-no-assets.log`: the failure the
  publish workflow would have hit.
- Workflows and scripts. `actionlint03.log`: actionlint 1.7.12 on `ci.yml` and
  `publish.yml`, exit 0 (`actionlint01-baseline.log` before the change);
  `yaml01-parse.log`: both files parse, with the job and step lists;
  `deploy01-workflow-tests.log`: `TestWorkflowActionsPinned`,
  `TestGoreleaserConfig` and `TestVersionSyncScript` pass; `sync02-ci-step.log`:
  the new helm step (5 tests, 0.1 s) and `make version-check` (PASS);
  `kind01-stub.log`: `hack/kind-e2e.sh` with logging stubs for docker, kind,
  kubectl and helm: `BUILD=0` builds nothing, both modes print the version and
  image ID before `kind create cluster`. `lint01.log`: scoped golangci-lint
  v2.15.0, 0 issues; `vet01.log`: gofmt, `go vet`, `go build ./...` and
  `bash -n` clean.
- Hosted baseline `hosted-33ee04bc/e2e.log` (run 42736964083): the four skips this
  package starts from.
- First-pass observations. `obs01-sqlite-blocking-audit.log`: on SQLite,
  `POST /api/v1/dns/blocking` is audited only as `kudurru.status` on
  `Kudurru/default`; six `GET /audit?kind=blocking` answers stay empty, while
  the API description of `kind` and the dashboard's audit kind filter list
  `blocking`, and Kubernetes records `blocking.toggle` (proposed follow-up row
  below). `obs02-servfail-dropped-across-commit.log`: one SQLite node at
  `33ee04bc`; the query sent right after `admin create-token` is dropped
  (`write failed err="serving: dependency changed"`), later ones get SERVFAIL
  at once, and a forwarded answer in the same position is delivered. This is
  the behavior that commit `49aae9ee` changes.

Retained failed runs:

- `e2e01-three.log` (tree at `33ee04bc` plus the switches): the three un-skipped
  tests fail, 176.3 s. It shows the missing `kind=blocking` event, the unanswered
  query of node-b and `audit keys: trusted verification material unavailable` on
  every replica. Two generated first-boot passwords of deleted throwaway nodes
  were redacted from this log; nothing else was changed.
- `fix-e2e01-before.log` and `fix-unit01-before.log`: the intended failures
  without the product change, described above. Three generated first-boot
  passwords of deleted throwaway nodes were redacted from the first (the script
  is `scripts/fix-redact.py`); nothing else was changed. The two tests now give
  their nodes a fixed admin password, so a later failure prints none.
- `review-05-make-e2e-hosted-env.log` (the reviewer's only full `make e2e` with
  the hosted environment, at `b162b1aa`): exit 2, 22 pass, 1 fail, 1 skip.
  `TestEnrolledClusterProcessesPromoteAndRejoin` failed after 21.1 s:
  `static process PUT /objects/dnsrecordsets/default/via-follower: status 503
  want 200, error completion_unknown`; both node logs are empty (the test sets
  the log level to error, and the primary logs nothing in this case). This
  package does not touch that test, `cmd` outside the output guard, or `ee`.
  The cause was not reproduced here; read from the code it is the limit that
  the HA-FIX ledger records as open ("the same guard can drop the
  acknowledgment of any single forwarded write that coincides with another
  primary commit"): the primary commits the forwarded command, and
  `MemberRequest.withOutput` (`internal/store/sqlite/member_request.go`) then
  requires the store authority, which includes the installed generation, to
  equal the one recorded at the command's completion. Any commit in between,
  for example the status write that follows an object change, yields
  `replication.ErrCAS`; the member server aborts the response
  (`internal/cluster/replica/member_server.go`, `panic(http.ErrAbortHandler)`),
  and the follower answers `completion_unknown`
  (`cmd/kudurru/serving_member_objects.go`, `objectForwardError`). The write
  was committed, nothing is replayed, and the test stops because it requires
  `200`. HA-FIX `process02-enrolled.log` is the same answer in the sibling
  test. Seen once in the nine runs of this test in the QUAL-CI logs (seven
  passes: `e2e07`, `e2e08`, `e2e10`, three in `review-12` and
  `fix-e2e03-make-e2e-hosted-env.log`; the ninth is the port failure below).
  **How to read the first hosted run:** this line in the hosted e2e log is
  that product limit, not a regression of this package, and since commit
  `52aee771` it no longer hides the BIND results of the same run; it does fail
  the step that carries G23-R4, so a hosted green needs either a rerun or the
  HA lane's answer first.
- `review-12-flake-promote-rejoin-x4.log`, run 3 of 4, and
  `e2e08-licensed-prebuilt.log`: a node could not bind a port that the harness
  had reserved earlier (`enrolled-b: kudurru exited before it was ready ... api:
  listen 127.0.0.1:34063: bind: address already in use` after 8.1 s;
  `conflict-c`, metrics port, after 11.3 s). `kudurrutest.FreePort` closes its
  listener and the node binds seconds later, up to about 100 s for a node that
  a test starts late; other lanes were running tests on this machine both
  times. Pre-existing and not changed here. **How to read it:** `exited before
  it was ready` with `bind: address already in use` in the node log is this
  harness race; a rerun is the remedy until the harness retries or allocates
  outside the ephemeral range.

Rows the integrator owns (this lane may not edit `dev-docs/followups.md`, the
decision log or the tracker; proposed text):

- Decision log: the row in `out/QUAL-CI/decision.md` (upstream-failure refusals
  across canonical commits). If it is rejected, drop commit `49aae9ee` together
  with `0048c606` (the rewrap of the paragraph it extends): the tree without
  them has the reviewed first-pass form of the query log test (fake resolver),
  and nothing else in this package depends on them.
- `dev-docs/followups.md`, section "API":
  `| Audit kind filter for blocking changes on SQLite: GET /audit?kind=blocking is always empty there, because the blocking command is audited inside its own transaction as kudurru.status on Kudurru/default and Options.AuditBlockingCommand is set only for the Kubernetes driver (which records blocking.toggle). The kind description of GET /audit (internal/api/authroutes.go:59) and the dashboard's audit kind filter (dashboard/src/pages/AuditPage.tsx:49) offer "blocking" on both drivers and do not offer "Kudurru". Decide: also record blocking.toggle on SQLite (a second row for one effect), or keep one row and make the description and the filter say which kind a driver uses | QUAL-CI, evidence obs01-sqlite-blocking-audit.log; TestCentralAuditTwoNodes reads the kudurru.status rows and passes either way | v0.2 (G21 contract) or the next patch release |`
- `dev-docs/followups.md`, section "Dataplane":
  `| DNSSEC validation failures across canonical commits: a query that ends in dnssec_bogus on a data generation that a commit just retired is still refused at output (no answer; the client times out), as upstream failures were before the QUAL-CI fix. Read from retainedDNSResponse, not exercised. Deciding it needs a mounted test with a signed zone and the anchor-retirement cases of D332 | QUAL-CI decision row | v0.2.x |`
- `dev-docs/followups.md`, section "Store and drivers" (the HA-FIX ledger
  already states the limit; this adds the test that meets it):
  `| A write forwarded through an enrolled follower answers 503 completion_unknown although the primary committed it, when another primary commit lands between the command's commit and its acknowledgment (sqlite MemberRequest.withOutput compares the store authority, which includes the installed generation; the member server then aborts the response). The status write that follows an object change can be that commit. TestEnrolledClusterProcessesPromoteAndRejoin met it once in a full make e2e (PUT of a record set through the follower, QUAL-CI review-05); TestEnrolledClusterProcessesPartitionCatchupAndRestart met it in HA-FIX process02 | QUAL-CI review-05-make-e2e-hosted-env.log; HA-FIX ledger "What remains open" | before a hosted e2e green is credited: HA lane to make the acknowledgment tolerate an unrelated later commit, or to say in the tracker that this explicit answer is accepted and how the test is read |`
- `dev-docs/followups.md`, section "Build, CI, docs":
  `| e2e harness port reservation: kudurrutest.FreePort closes its listener and the node binds the port seconds later (up to about 100 s for a node that starts late in a test). Another process can take the port in between: "kudurru exited before it was ready ... bind: address already in use". Seen twice on a shared machine (QUAL-CI e2e08: conflict-c metrics port; review-12 run 3 of 4: enrolled-b API port), not yet on a hosted runner. Fix candidates: retry a node start on EADDRINUSE with fresh ports, or allocate outside the ephemeral range | QUAL-CI review | v0.2.x |`
- Tracker text for G24-SYNCTEST: "already covered: `deploy.TestVersionSyncScript`
  runs `hack/test_sync_version.py` in every hosted go job and before
  publishing; QUAL-CI added a visible duplicate step to the helm job", not
  "now runs in CI".

What remains open: the hosted run itself (the acceptance of G23-R3, G23-R4
and G24-SYNCTEST is a hosted log line, the eight Kubernetes tests have never
run on a hosted runner, and the new step condition is checked by actionlint
only: that the BIND step runs after a failed `make e2e` step is unobserved);
the product change has a decision row without a number and is two commits
(`49aae9ee`, `0048c606`) that can be dropped together; `dnssec_bogus` refusals are still dropped in the
same window after a commit (read from the code, not changed, not exercised);
a forwarded write through an enrolled follower can answer `completion_unknown`
and fail `make e2e` (HA lane; `review-05`), and the harness port race can fail
it too (`review-12`, `e2e08`), so one hosted red on either signature is
expected now and then until they are fixed; the `kind=blocking` audit filter
on SQLite is a G21 contract question that the adapted test does not depend on;
`hack/kind-e2e.sh` was not run against Docker or kind in either mode (this
sandbox has no Docker daemon and no `kind`), so the second bullet of G22-WP6
and the new image checks rest on `bash -n`, the stub runs with real binaries
and, for `BUILD=1`, the next hosted run of the kind workflow, which will be the
first real execution of the license check; the new log lines of
`hack/compose-e2e.sh` were not run against Docker either; the other envtest
suites keep their own skip logic and rely on the `make test` check rather than
on a switch in each test; `TestKnotCatalogConsumer` still skips in hosted CI;
three tests fall back to a hard-coded `bin/k8s/1.37.0-linux-amd64`
(`internal/primaryruntime`, `internal/iammigrate`, `cmd/kudurru`
`TestRunServeKubeBootstrapAfterRecovery`), which `make test` no longer reaches
because it exports the variable, but a plain `go test` without it will fail or
skip once `make envtest` resolves `1.37.x` to a later patch (only 1.37.0 is
published today); the full `cmd/kudurru` package was not run here (the
integrator runs it through `hack/go-test.py`); the optional `make g22` target
was not added; the G22-WP8 final pass on the final head is not part of this
package.
