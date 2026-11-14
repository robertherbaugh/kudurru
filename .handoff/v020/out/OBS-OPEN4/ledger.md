### OBS-OPEN4 What verification left open in dnstap listen and tracing — November 12, 2026 (D382, D372)

**dnstap listen: each time is clamped.** A message can carry two times, and either one can be wrong while the other is sane. Before this change, `unskew` checked only the response time, or the query time when there was no response time. As a result, a `CLIENT_RESPONSE` with a sane response time and a query time 50 years ahead (or from 1971) produced a row dated in 2076 or 1971. Its dedup window was built from that time too, so a relayed response to a query this node had already logged was recorded as a second row. Now each time is checked on its own against the receive time, with a bound of 5 minutes:
- When one time is wild and the other is sane, the wild one takes the value of the sane one, and the row has no latency.
- Only when every time present is wild does the message move to the receive time, keeping its latency (the earlier behavior).

The row is dated from the corrected times, and `record` builds the dedup window from the same corrected times. Every changed message is counted in `kudurru_querylog_dnstap_clock_skew_total`.

**dnstap listen: bounded accept log.** A failed Accept is still counted in `accept_failed` and retried after a wait that doubles from 5 ms to 1 s, and Close still ends the wait at once. The log now gets at most one warning a minute (`acceptLogEvery`), carrying `failures=` with the number of failures since the previous warning, so none is lost from the log. Before, a listener whose Accept failed for good logged one warning per second without end.

**Tracing: the tail cap does what it says.** "Capped at 100 per second" is now true for any one second. The tracer keeps a ring of the last `TailLimit` keep times, and a failed or slow trace is kept only when fewer than the limit were kept in the second before it. Before, the window was a fixed calendar second, so up to 200 could pass across a boundary. A tail trace that the full export queue drops (`queue_full`) now gives its own allowance back, so the cap counts only tail traces that reach the queue. Each allowance carries a number, and the return removes that trace's entry from the ring (the newer entries move down one place). A first version removed the newest entry instead; when an earlier tail trace was dropped after a later one had been allowed, that left the dropped trace's older time in the window, which then expired early and let more than `TailLimit` tail traces through in one second (found in review). An entry whose second has already passed is no longer in the ring, and nothing is given back. The schema help ("capped at 100 per second") was already accurate under the new rule and is unchanged. `docs/architecture/observability.md` and `docs/deploy/tracing.md` now say "in any one second" and explain the `queue_full` refund (the architecture page says the returning trace's own keep time leaves the window).

**Tracing: the cache test cannot be passed by a plain miss.** `TestRunServeTracingThroughCacheAndRace` now requires all of the following:
- The wave of eight identical queries asks each server exactly once (one coalesced fetch), and none of the wave's traces says `prefetch_refresh`.
- Exactly one of the nine traces has root `kudurru.status` = `prefetch_refresh`. A plain miss would say `forwarded`.
- `kudurru_cache_prefetches_total`, read from the mounted metrics listener, goes from 0 to 1.
- The refresh asks each server exactly once more.

The earlier check (one or two exchange spans in total) is kept. To make the counter readable, the test now binds the metrics listener to a free port. The late query is sent 1.2 s after the wave was answered, and the wait for exported traces comes only after it. A first version waited for the wave's traces to be exported (up to the tracer's 2-second flush tick) before its 1.2 s sleep, so the late query stayed inside the entry's 2-second TTL only when the wave happened to land shortly before a tick (found in review: delaying the wave by 0.5, 1 or 1.5 s made it fail every time). The coalesced fetch stores the entry before any query of the wave is answered, so at 1.2 s the entry is at least that old (within the 1 s prefetch trigger) and has at most 0.8 s left.

**Tracing off costs nothing; tracing on is measured.** Measuring against the commit before tracing (406d0091) found a real cost with tracing off. The tracing fields had grown `pipeline.Request` from 440 to 456 bytes, past Go's 448-byte size class, so every query allocated 32 bytes more (712 to 744 B/op through the full stage order). The small fields now share one word (the three bools and `TraceSpan`), so tracing adds only the `Trace` pointer and `Request` is 440 bytes again. `TestRequestKeepsItsSizeClass` guards this.

After that fix, the paired measurement shows no change with tracing off. The base was reconstructed with a `go test -overlay` that puts back every Go file that changed since 406d0091, with no extra worktree. The two builds were interleaved, five fresh processes each, medians:

| Benchmark | Allocations and bytes per op, base vs HEAD | Time |
|---|---|---|
| Full stage order, untraced (`BenchmarkOpen4UntracedChain`, a measurement-only file in the overlay) | 12 allocs, 712 B on both | 768.5 vs 779.9 ns |
| `internal/stages/cache` `BenchmarkServe` hit, hit-parallel and miss | equal (miss: 4,773 vs 4,772 B) | within -7 % to +5 % |
| `internal/upstream` `BenchmarkStrategies`, all 12 cases | equal to within 7 bytes and one alloc (jitter/parallel_best 205 vs 204) | within noise in both directions |
| `internal/dnsserver` `BenchmarkUDPSockets` sockets=1 | 33 allocs and 2,308 B on both, 0 lost | 239,167 vs 240,836 q/s |
| `internal/dnsserver` `BenchmarkUDPSockets` sockets=6 | 33 allocs on both; 2,354 vs 2,362 B | 359,006 vs 355,972 q/s |

The only outlier is one p50 figure, slow-first/random 89 vs 183 µs. That strategy picks a random server among a slow and a fast one, so its p50 is bimodal; its ns/op is 1.4 % lower on HEAD.

With tracing on (`BenchmarkTracedChain`, which now has a `kept` case), medians:
- off: 981 ns, 712 B, 12 allocs.
- unkept: 4,099 ns, 5,608 B, 14 allocs.
- kept: 4,761 ns, 5,609 B, 14 allocs.

So tracing on costs about 3 µs and 4.9 KB per unkept query, and 3 to 4 µs per kept query on the query path. These numbers are in a new "What tracing costs a query" section of `docs/deploy/tracing.md` and in `docs/architecture/observability.md`.

Evidence under `.context/v020-final-20261110/evidence/OBS-OPEN4/`:
- `unit01-clamp-before-fix.log`: on the old `unskew`, `TestListenClampsEachTimeOfAMessage` fails (rows dated 2076-11-12 and 1971-01-01, clock_skew 1 of 3) and `TestListenDedupWindowUsesTheClampedTime` fails (1 row, duplicate 1 of 2).
- `unit02-clamp-after-fix.log`: both pass, together with the existing `TestListenShiftsASkewedClock`.
- `unit03-acceptlog-before-fix.log`: on the old listen.go (overlay), `TestListenBoundsTheLogOfAFailingAccept` fails with 10 log lines for 10 failures in 3 s.
- `unit04-acceptlog-after-fix.log`: it passes with 1 line. `TestListenAcceptLogCarriesTheSuppressedCount` passes with lines [1 8 1] for 10 failures at a 700 ms interval. `TestListenRetriesAcceptErrors` and `TestListenRetryWaitEndsAtClose` still pass.
- `unit05-tailcap-before-fix.log`: `TestTailCapHoldsInAnyOneSecond` fails ("a fourth tail trace was kept 2 ms after the first three") and `TestTailCapIsNotSpentOnQueueFull` fails ("trace 3 was limited").
- `unit06-tailcap-after-fix.log`: both pass, together with `TestTailKeepIsCappedPerSecond` and `TestTailKeep`.
- `unit07-sizeclass-before-fix.log`: on the previous pipeline.go, `TestRequestKeepsItsSizeClass` fails ("Request is 456 bytes").
- `unit08-alloc-guards.log`: `TestTracedChainAllocations` and `TestRequestKeepsItsSizeClass` pass.
- Race runs: `race01-dnstap.log` (`internal/querylog/sinks/dnstap` 18.4 s and `internal/querylog`); `race02-tracing.log` (`internal/tracing`, `otlp`); `race04-pipeline.log` (`internal/pipeline`, `stages/log`, `stages/upstream`); `race07-light-final.log` (pipeline, tracing, otlp and dnstap at the final source). All are ok.
- `race03-cmd-tracing.log` is retained as a failed run. The first version of the strengthened test read `/metrics` from `127.0.0.1:0`, because the mounted config does not bind the metrics listener to a fixed port. The test now binds it with `offlineFreeAddress`.
- `race05-cmd-tracing.log`: `-race -count=2 -run '^TestRunServeTracing'` on ./cmd/kudurru gives `TestRunServeTracingThroughCacheAndRace` (6.69 s, 6.69 s) and `TestRunServeTracingTrustedEdgeTraceparent` (6.95 s, 6.94 s) passing twice. `TestRunServeTracingLiveDnsdist` is skipped, as always without `KUDURRU_TRACING_LIVE`. ok 28.4 s, no race report.
- `race06-cache-test-plain-miss-mutation.log`: the same test with the late query moved past the entry's TTL (a plain miss, through an overlay) fails with "kudurru_cache_prefetches_total = 0 after the hit, want 1", as intended.
- `fix-unit01-returntail-before-fix.log`: on the earlier `returnTail` (the newest entry removed), the new `TestTailCapHoldsWhenAnEarlierTraceIsDropped` fails ("a third tail trace (after B and C, 150 ms apart) reached the queue within one second at TailLimit 2"); the other tail tests pass.
- `fix-unit02-returntail-after-fix.log`: all five tail tests pass (`TestTailKeepIsCappedPerSecond`, `TestTailCapHoldsInAnyOneSecond`, `TestTailCapIsNotSpentOnQueueFull`, `TestTailCapHoldsWhenAnEarlierTraceIsDropped`, `TestTailKeep`).
- `fix-race01-tracing-pipeline.log`: `-race -count=2` on `internal/tracing`, `internal/tracing/otlp` and `internal/pipeline` after the fix, all ok.
- `fix-race02-cache-test.log`: `-race -count=6 -run '^TestRunServeTracingThroughCacheAndRace$'` on ./cmd/kudurru at the final test, 6 of 6 pass (6.66 to 6.72 s), ok 41.2 s.
- `fix-probe01-phase-{0ms,500ms,1000ms,1500ms,1900ms}.log`: the final test through a temporary `-overlay` copy that sleeps before the wave (nothing written to the worktree), `-race -count=2` each: 10 of 10 pass (6.69 to 8.72 s). The review's probe of the previous version (`review-probe02-phase.log`) failed at 500, 1000 and 1500 ms.
- `fix-lint01.log`: 0 issues on `internal/tracing/...` and `cmd/kudurru/...` with both fixes. `fix-benchguard01.log`: the bench guards (14 tests) pass.
- `bench01-paired.log` and `bench01-summary.txt`: the paired measurement above.
- Lint: `lint01.log` is 0 issues on dnstap, tracing, pipeline and cmd/kudurru. `lint02.log` is 0 issues on the same packages at 341a1173 (before the review fixes; see `fix-lint01.log`).

What remains open:
- The benchmarks ran on the shared development machine while other lanes ran heavy jobs, so the timings are good only to a few percent. The allocation and byte counts are exact. `hack/benchgate` and `hack/bench-release.py` were not run against the committed calibration (that needs the named runner class).
- The kept-trace cost covers the query path only. The export goroutine's encoding is not included, and the benchmark's exporter drops each batch.
- The accept log bound is per listener. With several listeners failing at once, each logs once a minute. Failures suppressed after the last warning are logged only when another failure comes a minute or more later; if the failures stop sooner, that remainder appears only in `accept_failed` (review nit, not changed).
- The benchmarks were not rerun after the `returnTail` fix. It adds one 8-byte field to `Trace` (4,208 to 4,216 bytes, still in the same size class) and touches only the tail-keep path.
- The live dnsdist tests (`TestDnstapLiveEdgeOneRowPerClientQuery`, `TestRunServeTracingLiveDnsdist`) were not rerun. The changes do not touch the paths they exercise beyond the time clamp, which unit tests cover.
- The full ./cmd/kudurru suite, test/e2e and hosted CI were not run.
