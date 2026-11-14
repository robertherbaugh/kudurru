### OBS-4 OpenTelemetry tracing and dnsdist TRACEPARENT — November 10, 2026 (D382)

With `tracing.enabled` (host-only, restart) the chain the DNS transport calls
starts one trace per standard query. Before any stage runs it removes every
EDNS TRACEPARENT option (code 65500, `tracing.ednsOptionCode`), whoever sent
it. The option continues the sender's trace only when the query arrived with
an authenticated PROXYv2 header (`Client.ProxiedFrom`: listed peer plus the
shared-secret TLV, D45/D56/D72) and `tracing.acceptTraceparent` is on; from
any other client and from the API's synthetic dig it is ignored and counted
as untrusted. UPDATE and NOTIFY are never touched, so their TSIG digests stay
intact. The root span `kudurru.dns.query` (server) is the child of the edge's
span; every stage runs inside `kudurru.<stage>`, and every upstream attempt is
a `kudurru.upstream.exchange` client span. Upstream queries still never carry
client options; a forward group with `upstreams.groups.<name>.tracePropagation`
gets a fresh option whose parent id is the attempt span, only for a sampled
trace (head sampler or continued edge trace) and with the sampled flag: a
receiver reads presence as the decision to trace, so sending it for every
traced query would make the next hop keep everything. A trace kept later by
the tail rule records its attempts but has no downstream link. Recursive
groups refuse the flag.

The sampling rule (D382): a trace is kept when it continues a trusted edge
trace, when the head sampler picked it (`tracing.sampleRate` per million,
default 1000, OpenTelemetry's trace-id ratio rule), when resolution failed
(the response status is upstream failure or timeout, DNSSEC bogus or an
unsynced zone, or a pipeline error; the status decides, not the rcode, so
refusals, drops, a cached SERVFAIL and the drain SERVFAIL of the health names
never count) or when it took at least `tracing.slowThreshold` (250 ms). Traces
kept only by the failed or slow rule are capped at 100 per second per node
(`tracing.Config.TailLimit`, not a setting) and the excess counts as
`kudurru_tracing_traces_total{result="tail_limited"}`; sampled and
edge-continued traces are not capped. An edge's TRACEFLAGS are not used: the pinned
dnsdist 2.1.3 sends 00 for the queries it traces (live probe), so the option's
presence is the parent's decision. Only a kept trace stamps `trace_id` on the
query event (querylog.db, `GET /queries`, the tail, every sink) and an
OpenMetrics exemplar on `kudurru_dns_request_duration_seconds`. The root span
carries the question name only under `querylog.privacy` full or
anonymize_client and the client address only under full. Kept traces are
offered to a bounded queue (`tracing.queueSize`, 2048) without blocking and
posted once, in batches every two seconds, as OTLP/HTTP protobuf written by
hand with `protowire` through the query-log HTTP batch client (bearer token
and CA from file or environment references). A full queue or a refused or
unanswered batch drops and counts (`kudurru_tracing_traces_total`). Tracing off
reads and removes nothing and costs one nil check per stage. With tracing on,
spans and attributes are recorded into arrays inline in the trace (16 spans,
32 attributes) and cut into per-span attribute slices only when a kept trace
is read for export, so a query that is not kept costs two allocations more
than tracing off over the full stage order (the trace, about 5 KB, and the
upstream stage's trace context) and about 3 µs. The tracer is in
house (`internal/tracing`, about 20 KB of code in the binary); no module enters
the binary. `go.opentelemetry.io/proto/otlp` (Apache-2.0, already in the module
graph) moves to a direct requirement because the tests decode the hand-written
encoding with it. `make size`: 82,182,304 bytes of 100,000,000.

Evidence under `.context/v020-final-20261110/evidence/OBS-TRACING/`:

- `live01-dnsdist-traceparent-probe.log`: the pinned dnsdist 2.1.3 image with
  `SetTraceAction(true, {sendDownstreamTraceparent=true})` sends option 65500,
  27 octets, version 0, reserved 0, trace id, span id, flags `00`.
- `race05-cmd-final.log` (`go test -race -v`, `./cmd/kudurru`, live env set):
  `TestRunServeTracingTrustedEdgeTraceparent` and
  `TestRunServeTracingLiveDnsdist` pass (8.97 s and 8.25 s) with `TestCLIReferenceDrift`,
  `TestNoTemplateExecution` (16.70 s),
  `TestRunServeClientGroupRateLimitExemptNativeTransports` (10.63 s),
  `TestRunServeQuerylogReloadOriginalNativePublicationAndTail` (14.06 s),
  `TestRunServeQuerylogDisabledRestartRetainsHistoryWithoutAdmission` (10.88 s)
  and `TestRunServeQuerylogSinksStalledFailingAndReload` (15.82 s): exit 0, 8
  passes, no skips, 86.5 s. This run started compiling about a minute before
  `fix(config): read zero tracing option code and timeout as defaults` was
  written, so it may predate that validation-only relaxation; `internal/config`
  passed after it. The mounted test runs `runServe` with querylog on, PROXYv2
  with a secret, two upstream doubles and an OTLP/HTTP double that decodes
  with the generated OTLP types: the trusted query's trace arrives under the
  edge's trace id with the edge span as parent, a client span under the
  upstream, cache, postprocess and ingress stage spans, and the name, client
  and upstream attributes; the flagged group receives TRACEPARENT whose parent
  is the exported attempt span, the default group never; the same option from
  an ordinary client (127.0.0.2) is removed, ignored and never exported; GET
  /queries and the reopened querylog.db carry the trace ids (none for the
  untrusted row, whose `traceparent` flag is set); the metrics count 3 accepted
  and 1 untrusted; an exemplar links an exported trace; with the collector
  stalled 40 edge queries are each answered within one second while
  `queue_full` counts the overflow. The live test puts dnsdist 2.1.3 (PROXYv2
  TLV 0xE0, `SetTraceAction`) in front of `runServe` and finds the exported
  root span under dnsdist's trace id and span, and that trace id on the query
  row. Earlier passes: `race03.log` (mounted, 8.1 s), `live02-race-dnsdist.log`
  (live 8.10 s, mounted 8.97 s).
- `race04-packages.log`: `go test -race` over `internal/tracing/...`,
  `internal/pipeline/...`, `internal/upstream/...`, `internal/stages/...`,
  `internal/config/...`, `internal/dnsserver/...`, `internal/servingbuild/...`,
  `internal/querylog/...`: exit 0, 30 packages ok, no failure. New unit tests:
  `TestParseOptionVersions`, `TestTakeOptionRemovesEveryCopy`,
  `TestBeginTrustRule` (includes the bytes dnsdist sent), `TestRatioSamplerIsDeterministic`,
  `TestTailKeep`, `TestSpanTreeAndBounds`, `TestNilTraceIsInert`,
  `TestSlowExporterNeverBlocksFinish`, `TestCloseDrainsAndFailuresAreCounted`,
  `TestNewRefusesBadConfig`, `TestBeginAllocations` (one bare span: the
  Trace is the only allocation), `TestEncodingDecodesWithTheGeneratedTypes`,
  `TestExporterPostsOTLPHTTP`, `TestChainTracesTrustedEdgeQuery`,
  `TestChainIgnoresUntrustedTraceparent`, `TestChainWithoutTracerLeavesTheQuery`,
  `TestTracePropagationOnlyToFlaggedGroups`, `TestRecursiveGroupRefusesPropagation`,
  `TestObserverStampsKeptTraces`.
- Review fixes (after the accept-with-fixes review, `review-*.log`):
  - `fix-alloc01-before.log`: the new `TestTracedChainAllocations` (every
    stage of `Order`, two attempt spans with three attributes each, ratio 0)
    failed before the change: 33 allocations against 12 with tracing off (21
    more). `fix-alloc02-after.log`: passes, and `BenchmarkTracedChain` shows
    off 1056 ns, 744 B, 12 allocs/op against unkept 4287 ns, 5640 B, 14
    allocs/op (20,000 iterations; the reviewer's probe had seen 23 against 11).
    `TestAttributesPastTheInlineArray` and `TestContextCarriesTraceAndParent`
    cover growth past the inline arrays, the per-span cap and the one-value
    trace context.
  - `fix-prop01-before.log`: `TestTracePropagationOnlyForSampledTraces` (ratio
    0, a flagged group) failed: the unsampled trace sent TRACEPARENT with flags
    00. `fix-prop02-after.log`: `internal/upstream` passes under `-race`.
  - `fix-keep01-before.log`: `TestTailKeepFollowsStatusNotRcode` failed for
    `special_domain` and `cached` SERVFAIL answers (kept); the four failure
    statuses are kept. `fix-keep02-after.log`: `internal/tracing/...` and
    `internal/pipeline` pass under `-race`, with `TestTailKeepIsCappedPerSecond`
    (cap of 3 in one second, 7 counted `tail_limited`, edge trace exempt, reset
    after a second, negative limit off; written before the cap existed, so its
    first state was a compile failure, not a logged run).
  - `fix-race01-cmd.log` (`go test -race -count=2 -run TestRunServeTracing
    ./cmd/kudurru`): `TestRunServeTracingThroughCacheAndRace` (cache and
    prefetch on, a `parallel_best` group of a fast and a 300 ms server, eight
    concurrent identical queries and a prefetching hit; 6.70 s and 6.68 s) and
    `TestRunServeTracingTrustedEdgeTraceparent` (6.98 s and 8.99 s) pass twice;
    `TestRunServeTracingLiveDnsdist` skipped (live env not set). This test is
    new coverage of paths that were already correct; it did not find a defect.
  - `fix-lint01.log`: golangci-lint v2.15.0 over `internal/tracing/...`,
    `internal/pipeline/...`, `internal/upstream/...`, `internal/config/...`:
    0 issues. The help text of `tracing.enabled` and `tracing.sampleRate` (and
    the matching code comments) now says a continued edge trace is always kept
    and its flags are not used; `fix-gen01.log` regenerated the settings
    reference and CRDs into the final `chore(gen)` commit. `fix-lint02-cmd.log`:
    golangci-lint over `./cmd/kudurru/...`, 0 issues. `fix-race02-pkgs.log`
    (`-race -count=2`: `internal/tracing/...`, `internal/pipeline`,
    `internal/upstream`) and `fix-race03-pkgs.log` (`-race`:
    `internal/stages/log`, `internal/stages/cache`, `internal/config`): all ok.
    `fix-drift01.log`: `TestReferenceDrift`, `TestOpenAPIDrift`,
    `TestCLIReferenceDrift` and `TestAggregatedRolesDrift` pass after the new
    `chore(gen)` commit.
- `neg01-trust-check-removed.log`: with the trust check replaced by `true`,
  `TestChainIgnoresUntrustedTraceparent` fails (the untrusted option was
  honored); the code was restored.
- `lint02.log`: scoped golangci-lint v2.15.0 over the touched packages and
  `./cmd/kudurru/...`, 0 issues. `size01.log`: `make size` 82,182,304 bytes
  (82 %, 17,400 KiB headroom); `size02-tracing-symbols.log`: 60 symbols,
  20,769 bytes in `internal/tracing*`. `drift01.log`: `TestReferenceDrift`,
  `TestOpenAPIDrift`, `TestAggregatedRolesDrift` pass after the generated
  commit; `gen02-docs-gen.log` ran `TestCLIReferenceDrift` (pass).
- Retained failures: `race01.log` (the propagation assertion failed: the store
  validator's generated Kudurru CRD pruned the new `tracePropagation` field;
  `make generate`, `gen01-make-generate.log`, fixed it) and `race02.log` (the
  same failure with diagnostics: routed to the flagged group, three traces
  exported, no option). `lint01.log`: eight gosec findings, fixed in
  `fix(tracing): write ids with PutUint64`. `size00-mislabeled-same-head-not-a-base.log`
  measured the same head twice and is not a baseline.

Still open: the edge supervisor (`kudurru edge`) does not render dnsdist
tracing (`setOpenTelemetryTracing`, `SetTraceAction`); operators add the two
lines by hand, and dnsdist 2.1.3 exports its own spans only through protobuf
remote loggers (OTLP loggers arrive in dnsdist 2.2). Only PROXYv2 peers are
trusted, so a kudurru receiving TRACEPARENT from another kudurru's
`tracePropagation` group ignores it unless the query came through its own
authenticated PROXYv2 path. There is no dashboard trace link yet (the
`trace_id` field is in the Queries API and the tail) and no settings-page
surface; the dashboard settings-schema mock does not list `tracing.*` or
`tracePropagation`. Exports are a single attempt per batch (no OTLP retry on
429/503). The tail cap is a fixed 100 per second, not a setting.
Not run: the full `./cmd/kudurru` suite (hack/go-test.py), test/e2e,
the benchgate performance comparison, hosted CI.
