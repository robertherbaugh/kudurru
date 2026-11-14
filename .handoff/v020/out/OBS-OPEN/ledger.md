### OBS-OPEN Open items of the three observability packages — November 11, 2026 (no decision number)

This package closes what the merged OBS-FANOUT, OBS-HTTP-SINKS and
OBS-DNSTAP-EMIT packages left open. It made no design decision that needs a
decision-log row; the one new surface (a read-only status route) follows the
existing `GET /querylog/status` pattern.

**OBS-FANOUT nit 1 (shutdown log).** When the collector itself does not flush
within the shutdown bound, `querylogRuntime.close` now calls `stopSinks`
instead of `closeSinks`. The bound has already ended there, so whether an idle
sink finishes in that instant is a race; the sinks are no longer named one by
one as `additional query-log sink did not drain`. When any sink was running,
one warning, `additional query-log sinks stopped because the query log did not
flush`, lists every sink and the events still queued, beside the caller's
`query log flush incomplete`. A normal shutdown in which only an exporter did
not drain still logs the per-sink warning. The operator and architecture
pages name the new line.

**OBS-FANOUT nit 2 (untested "sink host stopped").** A new host test holds
the collector in a primary write, closes the sink host, lets the collector go
on, and requires the two late events to be counted for the sink with the
reason `sink host stopped` and never written. The comment's claim is now
covered.

**OBS-HTTP-SINKS fix verification.** I reviewed the fix commits (Expect
100-continue for large bodies, answerConn, the WroteRequest check, the 64 KiB
CA bound and `ErrTooLarge`, the help texts, the 5xx wording, `make
test-sinks`) and probed the client. Two defects were found and fixed:
(1) a server that answers a large batch with 2xx at its headers, without
"100 Continue" and without reading the body (a stock Go handler that ignores
the body does this), made the transport skip the body and still report the
request as written, so the client believed a success for a batch that was
never sent. Post now counts the body bytes the transport read; a 2xx counts as
stored only for a body read and written whole. (2) With a sink `timeout` of 1
to 4 s (1 s is the allowed minimum), the 2 s "100 Continue" wait left no time
to send the body to a server that ignores Expect, so every large batch to it
ended as an unknown outcome and was dropped. An attempt with 4 s or less left
now sends the body at once without Expect; a destination that refuses such
a request at its headers is still heard, because answerConn hides the write
error of the reset (`TestPostShortAttemptSeesARefusalAtTheHeaders`). The
architecture page (`docs/architecture/observability.md`) and the `timeout`
row of `docs/deploy/query-log-sinks.md` now say when Expect is skipped and
that a 2xx to the headers alone is an unknown outcome. The rest of the fix held up: the
answerConn write-error hiding is limited to EPIPE/ECONNRESET and marks the
connection cut, a cut connection never yields a believed 2xx, a reset with no
answer is still an unknown outcome, and `ErrTooLarge` is still
`ErrUnavailable` for every caller (no caller compares the error with `==`).

**OBS-DNSTAP-EMIT verifier's new problem.** The mounted dnstap test now waits
for the first stream (raw off) to finish when the raw change replaces the sink
and requires exactly the two frames of `visible.example.`. Under
`hide_domains_clients` a frame of the excluded client or domain looks like any
other frame, so counting is what makes the raw-off phase prove exclusion.

**Implementer's remaining list: sink state API.** `GET /api/v1/querylog/sinks`
(permission `querylog.read`) returns the answering node's name and, per sink
that is running or still draining, type, raw flag, queued, queue size, sent,
dropped, errors and last success. serve mounts it over the runtime's sink
host. It reports only the answering node.

Evidence under `.context/v020-final-20261110/evidence/OBS-OPEN/`:

- `probe01-httpbatch.log`: the two defects on the merged code, through
  throwaway probe tests: a 2xx without the body returned `err=<nil>`, and a
  1 s attempt to a server that ignores Expect ended with `gave no answer
  after the batch was sent ... context deadline exceeded`. `probe02` is the
  same probes after the fix (an unknown outcome, and success).
- `race01-sinks.log`: `go test -race -count=3 ./internal/querylog/sinks/...`,
  all seven packages ok (httpbatch 26.4 s), including the new
  `TestPostSuccessForABodyNeverSentIsNotBelieved` and
  `TestPostShortAttemptDoesNotWaitForContinue`.
- `mutation01-httpbatch.log`: without the body count the first new test
  fails (`<nil>`); with Expect regardless of the deadline the second fails
  (deadline exceeded).
- `mutation02-sinkhost.log`: `TestSinkHostClosedUnderARunningCollectorCountsLateEvents`
  passes 20 of 20 under `-race`; it fails when Close does not set `stopping`
  (reason `sink replaced`) and when `missed` does not count (timeout).
- `root-race01.log`: `TestQuerylogRuntimeCloseWithoutCollectorFlushReportsSinksOnce`,
  `-race -count=20`, ok (25.8 s). `mutation03-runtime.log`: with the old
  per-sink path, 3 of 3 runs fail, logging `did not drain` for both idle
  sinks with `queued:0`.
- `root-race02-dnstap-mount.log`: `TestRunServeQuerylogDnstapSinkPrivacyAndRaw`,
  `-race -count=3`, 3 of 3 pass (9.0 to 9.8 s). `mutation04-dnstap-mount.log`
  and its rerun `mutation05-dnstap-mount.log`: without the client exclusion
  the new count check fails with 4 frames.
- `root-race03-sinks-mount.log`: `TestRunServeQuerylogSinksStalledFailingAndReload`
  with the new `GET /querylog/sinks` checks, `-race -count=2`, 2 of 2 pass
  (15.1 s and 16.0 s).
- `gen01.log`: `make openapi`, `make client-go`, `bun run gen:api` and
  `make docs-gen` after adding the route (commit `chore(gen): regenerate for
  OBS-OPEN`). `drift01.log` is empty: that run was cut off by the restart.
  `drift02.log`: TestOpenAPIDrift, TestCLIReferenceDrift, TestReferenceDrift
  and TestAggregatedRolesDrift pass on the final tree.
- `race02-light.log`: `go test -race` over `./internal/querylog/`,
  `./internal/querylog/sinks/...` and `./internal/api/`, all ok (api 270 s).
- `lint01.log`: golangci-lint v2.15.0 over `./internal/querylog/...`,
  `./internal/api/...` and `./cmd/kudurru/...`, 0 issues.
- `final-race01.log`: `-race -run 'TestQuerylog|TestRunServeQuerylog|TestServingSettingsRawDnstap|...'`
  on `./cmd/kudurru`, 20 of 20 pass in 98.7 s, including the dnstap mount
  (9.7 s), the ClickHouse/Loki mount (16.8 s) and the stalled/failing sinks
  mount with the new route (17.1 s).

- Review fixes (after the review of `ff50ce3b`): `fix-race01-short-refusal.log`:
  `TestPostShortAttemptSeesARefusalAtTheHeaders`,
  `TestPostShortAttemptDoesNotWaitForContinue` and
  `TestPostLargeBatchSeesARefusalAtTheHeaders`, `-race -count=3`, 9 of 9 pass
  (the new test 0.43 to 0.48 s; 20 posts with a 2 s deadline each for linger
  0 and 300 ms, all heard as 503, none with Expect).
  `fix-mut01-answerconn-off.log`: with answerConn no longer hiding
  EPIPE/ECONNRESET, the new test fails, losing the immediate-reset 503 in 17
  of 20 attempts (the 300 ms linger case still passes there, as the client
  finishes writing before the reset). `fix-mut02-expect-always.log`: with
  Expect sent regardless of the time left, it fails on the Expect header.
  `fix-test02-sinks-docs.log`: `go test ./internal/querylog/sinks/...`, all
  seven packages ok after the doc edits (the ClickHouse, Loki and live tests
  read the operator page). `fix-lint01.log`: golangci-lint v2.15.0 over
  `./internal/querylog/sinks/httpbatch/...`, 0 issues. The review fixes
  change no generated file, so `chore(gen): regenerate for OBS-OPEN` is no
  longer the last commit; it is still the only generated commit.

What remains open: the dashboard does not show the new route yet, and the
settings-schema mock still lacks the clickhouse, loki and dnstap objects
(dashboard lane). The route reports the answering node only; a cluster-wide
view would need fan-out to the peers. Sink status has no last-error field;
errors are in the logs. `make test-sinks` was still not executed (the lab
Docker daemon runs without port publishing and the brief forbids pulling
non-digest images), and I did not rerun the live ClickHouse/Loki probes after
the 2xx body-count change; the change only refuses successes for bodies the
transport did not read whole, which real ClickHouse and Loki always read.
FORWARDER_* dnstap messages remain summaries, raw responses remain the
pipeline's answer before truncation, dnstap listen (OBS-3) is not built, and
the migration settings import is not gated by querylog.raw (importers produce
no querylog keys today); these were disclosed design limits, not defects.
Not run: the full `./cmd/kudurru` suite, test/e2e, hosted CI.
