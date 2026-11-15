### OBS-5 ClickHouse and Loki query-log sinks — November 10, 2026 (D371)

`querylog.sinks` gains two types beside `slog`, both built on the sink host of
OBS-1. `type: clickhouse` sends one `INSERT INTO <db>.<table> FORMAT
JSONEachRow` per batch to the ClickHouse HTTP interface
(`internal/querylog/sinks/clickhouse`); `type: loki` sends one JSON push per
batch to `/loki/api/v1/push` (`internal/querylog/sinks/loki`). No SDK is used:
the shared client (`internal/querylog/sinks/httpbatch`) is the standard library
HTTP client, one POST per batch on its own connection, gzip by default,
redirects never followed. Both export the same flat JSON record per query
(`internal/querylog/sinks/record`), built from the event after the exclusions
and the privacy transform, so a destination holds exactly what the local query
log keeps and `nothing` exports nothing. Loki streams carry only the fixed
labels of the settings, `node` and `status_class`; client, client name,
domain, query type, status and trace ID are structured metadata.

A batch is marked retryable (and repeated within the sink's attempt budget)
only when no connection was established or the destination answered 408, 429
or 5xx, with `Retry-After` honored up to the host's 30 s cap. A 5xx is not
proof that nothing was stored (a gateway's 502 or 504, a ClickHouse insert that
failed part way); the repeat is accepted because the documented table and Loki
drop the duplicate, and `attempts: 1` turns it off. A body of 64 KiB or more
asks for `100 Continue` first, and the client's connections hide the write
error of a peer reset, so that an answer a destination sent before resetting
the connection under the body is read (ClickHouse sends `100 Continue`, then
refuses a wrong password and resets). A 2xx is believed only once the
transport reports the body written whole on a connection that was not reset
(bounded at 5 s); otherwise it is an unknown outcome, dropped and not repeated. Every other
answer, and a request that was sent and got no answer, drops the batch and is
counted per sink in `kudurru_querylog_dropped_total{stage="sink",sink}` and
`kudurru_sink_errors_total{sink}`. Errors name the status and, for ClickHouse,
the numeric `X-ClickHouse-Exception-Code`; the response body is never read.

Settings hold credential references only (`passwordRef`, `bearerTokenRef`,
`caRef`); a URL with user information or a query is refused without repeating
it. The settings validator refuses everything a sink factory would refuse, and
refuses a Secret reference on any driver but Kubernetes. At runtime a file or
environment variable must be granted by the node's own
`credentials.allowedFiles` / `credentials.allowedEnvironment`; on Kubernetes a
Secret is read with the server's ServiceAccount, only in its own namespace and
only with the label `kudurru.io/query-log-sink: "true"`
(`cmd/kudurru/querylog_sink_credentials.go`). A password or token is read when
the sink opens and again after a 401 or 403; a changed value makes the refused
batch retryable, which is how rotation is picked up. `caRef` is read only at
open. A referenced value holds at most 64 KiB (the resolver's bound, which the
CA check now matches); a larger one is refused with `credential is
unavailable: it holds more than 64 KiB` (`credentials.ErrTooLarge`, still
`ErrUnavailable`). The help of `credentials.allowedFiles` and
`credentials.allowedEnvironment` and the operator page say that a grant is not
bound to a destination. `docs/deploy/query-log-sinks.md` documents the record, the ClickHouse
table DDL, the Loki label and structured-metadata mapping, credentials and
retry rules; tests keep the page and the code in step.
`docs/architecture/observability.md` gains the HTTP sink flow and diagram.

Evidence under `.context/v020-final-20261110/evidence/OBS-HTTP-SINKS/`:

- `root-race06` (`-race -count=1`, regex over every query-log test, the reload
  subscriber tests, `TestForwardedShapes`, `TestCLIReferenceDrift`,
  `TestNoTemplateExecution` in `./cmd/kudurru`): exit 0, 22 top-level passes,
  no skips, 148 s. Includes the mounted
  `TestRunServeQuerylogClickHouseAndLokiSinks` (SQLite: the settings command
  adds both sinks with granted files, ClickHouse behind TLS with a private CA;
  real UDP queries exported and compared with `GET /queries`; 503 repeated
  within the budget, 400 dropped at once; rotated password picked up; no
  credential value under the data directory) and
  `TestRunServeKubeQuerylogSinksReadSecretCredentials` (envtest: Secret keys
  named in the settings file, the Secret rotated through the API server, an
  unlabeled Secret never sent).
- `root-race07` (`-race -count=2`, the five new root tests): exit 0, 10 of 10
  passes (`TestRunServeQuerylogClickHouseAndLokiSinks` 20.2 s and 19.2 s,
  `TestRunServeKubeQuerylogSinksReadSecretCredentials` 10.0 s and 9.1 s).
- `lib-race01`: `-race -count=1` over `./internal/querylog/...` and
  `./internal/config/...` (11 packages ok, among them `sinks`, `clickhouse`,
  `httpbatch`, `loki`, `record`; `sqlitelog` 330 s), plus
  `TestCandidateReturnsItsQueryLogSinkReferences` in `internal/servingbuild`
  and `Settings|Schema|OpenAPIDrift` in `internal/api`: all exit 0. The two
  `TestLive*` tests skip there (no server URLs set).
- `live01` to `live03`, `live-probe01`, `live-setup.txt`: the optional
  `TestLiveClickHouseReadsTheDocumentedTable` and
  `TestLiveLokiServesTheDocumentedMapping` against real ClickHouse
  26.11.1.1867 and Loki 3.7.9 run as plain local processes on loopback (no
  container). `live01` is a retained failure caused by the test's own database
  renaming (fixed before commit); `live02` and `live03` (`-count=2`) pass.
  `live-probe01` records the status and exception codes the operator page
  names (403/516 wrong password, 403/497 missing grant, 404/60 and 404/81
  missing table or database, 400/41 unparsable row; Loki 204 for repeated
  pushes, 400 for an entry too old).
- `mutation01` (six mutations of `httpbatch/client.go`: an unknown outcome
  marked retryable, redirects followed, 4xx retryable, 5xx not retryable, the
  refused credential not reread, the response body in the error) and
  `mutation02` (three overlays of `querylog_sink_credentials.go`: an ungranted
  file read, a Secret used without the label, no Secret reader on Kubernetes):
  every mutation is caught by at least one test; the restored tree passes.
- `lint01`: retained failure, six gosec G101 findings on made-up test
  credentials and one revive unused-parameter in tests; fixed with annotated
  `//nolint:gosec // G101: ...` comments and `_`. `lint02` (retained) then
  showed two more G101 findings (the Secret label name, a rotated test
  password), fixed the same way; `lint03` was refused because another lane's
  golangci-lint was running; `lint04` (`--max-issues-per-linter=0
  --max-same-issues=0` over `./internal/querylog/...`, `./internal/config/...`,
  `./cmd/kudurru/...`): 0 issues. `light-race02`: `-race` over
  `./internal/querylog/sinks/...` and `go vet` after the fix, exit 0.
- `drift01` (after `gen03-make-generate`, `gen04-docs-gen`): `TestOpenAPIDrift`,
  `TestReferenceDrift`, `TestAggregatedRolesDrift`,
  `TestSettingsAdmissionContainsOnlyPublicClusterKeys`, `TestCLIReferenceDrift`,
  `internal/store/validator` and `deploy`: all pass. `size01`: `bin/kudurru`
  81,350,816 bytes with `CGO_ENABLED=0` (budget 100,000,000). `docs-site01`:
  Hugo builds 164 pages with `--panicOnWarning`.
- Retained interrupted runs: `root-race01-withdrawn` (terminated), `root-race02`
  (a mount-test PATCH refused with 400 before the validator and test were
  aligned), `root-race05` (cut off by the machine restart; rerun as
  `root-race06`).

Review fixes (after the accept-with-fixes review of 8c090646), logs `fix-*`:

- `fix-mutation01` to `fix-mutation03`: with the `Expect` header removed,
  `TestPostLargeBatchSeesARefusalAtTheHeaders` fails (69,481 body bytes
  written into a refusal, the early 503 lost in 8 of 20 attempts against the
  new `sinktest` mode `AnswerBeforeBody`). `fix-light-race01`: the fixed tree,
  `-race -count=5`, passes.
- `fix-live-probe01`: real ClickHouse 26.11.1.1918 on loopback, wrong
  password, 20 posts per case with `Expect` only: the 403 was still lost in 0,
  1, 1 and 5 of 20 at 1000 rows, 5000 rows gzip, 5000 and 10000 rows plain,
  because ClickHouse sends `100 Continue` before it checks the credential and
  then resets the connection (`fix-live-setup.txt`, curl trace). This led to
  the connection wrapper (`answerConn`). `fix-mutation06` and `fix-mutation07`:
  with the write error passed on again, `TestPostLargeBatchSeesARefusalAfterContinue`
  (new mode `AnswerAfterContinue`) loses the 503 in 3 of 20 and 22 of 50
  attempts. `fix-live-probe02` and `fix-live-probe03` (100 posts per case):
  every 403 arrives; a correct password stores each batch in 37 to 91 ms.
- `fix-live01`, `fix-live02`: `TestLiveClickHouseReadsTheDocumentedTable` and
  `TestLiveLokiServesTheDocumentedMapping` against ClickHouse 26.11.1.1918 and
  Loki 3.7.9 (plain processes, `KUDURRU_REQUIRE_SINK_SERVERS=1`) pass; a
  1.6 MB push to Loki through the client (the `100 Continue` path) is stored
  in 106 ms plain and 219 ms gzip, so Loki answers `100 Continue` without the
  2 s wait. The probe file (`fix-live-probe.go.txt`) was applied with
  `-overlay` only.
- `fix-light-race04` to `fix-light-race07`: retained flaky runs of the first
  version of `TestPostLargeBatchSeesARefusalAfterContinue` under machine load
  (another lane's `-race -count=30`): its "a body was cut" check failed in 5
  of 40 (`fix-light-race06`) and 2 of 40 (`fix-light-race07`) runs. The cause
  was a real gap: Go's transport returns an answer that arrives while the body
  is still being written, so a 2xx at the headers could be taken for a stored
  batch. Fixed by waiting for the transport's `WroteRequest` report before a
  2xx counts; `fix-light-race08` (`-race -count=40`) passes 40 of 40, and
  `fix-mutation08` (every 2xx believed) fails at the first attempt.
  `fix-light-race09`: `-race -count=3` over `internal/credentials` and
  `./internal/querylog/sinks/...`, all pass.
- `fix-mutation04` (CA bound back to 1 MiB) and `fix-mutation05`
  (`internal/credentials` without `ErrTooLarge`) fail the new size checks.
- `fix-light-race02`, `fix-light-race03`: `-race` (`-count=2`) over
  `internal/credentials` and `./internal/querylog/sinks/...`, all pass.
  `fix-build-vet01`: `CGO_ENABLED=0 go build ./...`, `go vet`, `gofmt -l`
  clean.
- `fix-lint01`: retained failure, two staticcheck QF1008 findings in
  `sinktest` (embedded `Reader` in a selector), fixed in the last source
  commit; `fix-lint02` was refused (another lane's golangci-lint was running);
  `fix-lint03` over `./internal/querylog/...`, `./internal/credentials/...`,
  `./internal/config/...`: 0 issues.
- `fix-gen01-docs-gen` (`make docs-gen`, only the two `credentials.*` rows of
  `docs-site/content/reference/configuration.md` changed), then
  `fix-drift01` (`TestReferenceDrift`,
  `TestSettingsAdmissionContainsOnlyPublicClusterKeys`, `internal/api`
  `Settings|Schema|OpenAPIDrift`, `-race` over `internal/config`,
  `internal/store/validator`, `deploy`) and `fix-drift02`
  (`TestReferenceDrift` on the final tree): all pass.
- `fix-root-race01` (before the last source commit) and `fix-root-race02`
  (final tree), `-race -count=2` in `./cmd/kudurru` over `TestQuerylogRuntime*`,
  `TestQuerylogSink*`, `TestRunServeQuerylogClickHouseAndLokiSinks`,
  `TestRunServeKubeQuerylogSinksReadSecretCredentials`, `TestCLIReferenceDrift`
  and `TestProviderStatic*` (the stored-provider users of the resolver): 14
  top-level tests, 28 of 28 passes each, 86 s.
- `make test-sinks` (Makefile, CONTRIBUTING.md) starts ClickHouse and Loki in
  Docker and runs the live tests; Docker is not running on this machine, so
  the target itself was checked only with `make -n`.

Open: no dashboard view of sink state or credential errors (state is metrics
and log warnings only; `querylog.SinkHost.Status()` is not mounted on a route);
the settings mock `dashboard/src/mocks/data/settings-schema.json` does not
carry the new nested objects; no Grafana panels or alerts for the per-sink
series. The ClickHouse table is created by the operator, never by the server.
The `caRef` bundle is read only when a sink opens, so a rotated CA needs a
settings change or a restart. `make test-sinks` exists but was never run (no
Docker daemon here) and is not wired into CI; its images
(`clickhouse/clickhouse-server:25.8`, `grafana/loki:3.7.9`) are unverified
defaults. 502 and 504 are still repeated like any 5xx. The full `./cmd/kudurru` suite, `test/e2e`, the performance gate and hosted CI
were not run in this lane. Mixed-version acceptance of `querylog.sinks`
entries of the new types by older nodes was not tested.
