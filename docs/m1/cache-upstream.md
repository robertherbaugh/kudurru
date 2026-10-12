# M1 work package: cache-upstream

The response cache and the dnsproxy-based upstream groups
(docs/architecture.md §3.3 `internal/cache` and `internal/upstream`, §7.1
steps 9 and 11, §6.2 `upstreams` and `cache`).

## What was built

### `internal/upstream`: groups, strategies, health, conditional map, ECS

- `upstream.New(Config, Options) (*Set, error)` builds every group from
  config. Servers use dnsproxy address syntax: `ip[:port]`, `udp://`,
  `tcp://`, `tls://`, `https://`, `quic://`, `h3://`, `sdns://`. An optional
  display name goes first, as in `cloudflare=tls://1.1.1.1`. Bootstrap
  resolvers resolve server host names. Each group sets its own timeout
  (default 2 s). Nothing is dialed at construction time.
- The four strategies:
  - `parallel_best` (default) races two randomly chosen healthy servers,
    then races the rest of the group if both fail.
  - `strict` tries servers in configuration order.
  - `random` tries a random server, then the others.
  - `rtt_weighted` picks the first server weighted by 1/EWMA-RTT, then
    falls back in RTT order.
- Health: after 3 consecutive failures (timeout, network error, REFUSED) a
  server goes into exponential backoff, 2 s doubling to 1 min. While in
  backoff it is tried only after the healthy servers, and one success
  restores it. SERVFAIL moves on to the next server but does not count
  against health. When a client gives up or a race is lost, the server is
  not penalised.
- Conditional forwarding: the longest suffix on label boundaries wins. Keys
  may use the dnsmasq form `"[/a.example/b.example/]": group`.
  `ImportDnsmasq(io.Reader)` reads whole `server=/x/ip#port` or
  `[/x/]ip tls://ip` files and returns an `Import` you can `Merge` into a
  `Config`.
- `Set.Route(qname, hint)` returns (group, conditional, err). A hint
  (`Request.UpstreamGroup`) wins. An unknown hint is an error, so a forward
  zone never falls through to the public resolvers. A suffix mapped to
  `default` (dnsmasq `#`) counts as ordinary forwarding and stays cacheable.
- ECS (RFC 7871): `strip` (default), `forward` or `add`. Subnets are cut to
  /24 and /56 at most. Private, CGNAT, loopback and link-local client
  addresses are never added. `PrepareQuery` builds kudurru's own OPT record
  and forwards no other client EDNS option. `FinishReply` adapts an upstream
  or cached reply to the client: ID, question spelling, OPT, DO, EDE relay,
  and an ECS echo.
- `Set.Exchange(ctx, group, *dns.Msg) (*Result, error)` is the API.
  `*upstream.Error` carries every per-server error plus the last
  SERVFAIL/REFUSED message, and `IsTimeout(err)` classifies it.
- Metrics, with `upstream` = `<group>/<display name or configured address>`
  (never a resolved IP): `kudurru_upstream_healthy{upstream}`,
  `kudurru_upstream_requests_total{upstream}`,
  `kudurru_upstream_errors_total{upstream,error}` (error class is timeout,
  network, refused or servfail) and
  `kudurru_upstream_request_duration_seconds{upstream}`.
  `Set.CloseReplacedBy(next)` deletes the series of servers that a reload
  removed.

### `internal/stages/upstream` (stage `upstream`)

The scaffold's internals are replaced with the code above.
`NewStage(*upstream.Set)` picks the group: `Request.UpstreamGroup`, else the
conditional map, else `default`. It sets the following:

- `Response.Upstream` to `<group>/<server>`.
- `Status` to `forwarded`, `servfail_upstream` or `timeout_upstream`.
- `Cacheable` to true only for routing that is not conditional.

On failure it relays the upstream's own SERVFAIL, which keeps its EDE.
Otherwise it synthesises a SERVFAIL with EDE 22 (timeout) or EDE 23
(network). `Swap(next)` hot-swaps the Set on reload. `ECSSubnet` lets the
cache key entries by the subnet that is actually sent. The convenience
constructor `New("default", servers, timeout)` is kept because `cmd/` and
the dnsserver tests still call it.

### `internal/cache` + `internal/stages/cache` (stage `cache`)

- `internal/cache` is DNS-agnostic. It is a sharded LRU of packed messages:
  up to 256 shards, each with its own mutex and intrusive list, hashed with
  maphash. The key is (lower-cased qname, qtype, qclass, DO, CD, ECS
  subnet) and `MakeKey` builds it. Each entry tracks expiry, a stale window
  (`serveStale.ttl`, 72 h), the prefetch hit count within
  `prefetch.window`, a one-time prefetch claim and a failure-recheck hold.
  `Coalesce` is singleflight. `Dump(io.Writer)` / `Load(io.Reader)` write
  and read a versioned binary snapshot (`KDRC` v1) of entries still inside
  their stale window, with absolute times and LRU order kept, for the
  local.db hook. `Peek` does a lookup with no side effects (debug API,
  tests).
- `internal/stages/cache` owns the DNS rules:
  - Only cacheable queries are handled: one IN question, QUERY opcode, not
    ANY/AXFR/IXFR. A request with an `UpstreamGroup` hint skips the cache.
  - Positive TTLs are clamped to [minTTL, maxTTL]. Negative TTLs are
    min(SOA TTL, SOA minimum) capped by negativeTTL (RFC 2308), or
    min(minTTL, negativeTTL) when there is no SOA.
  - OPT and the message ID are stripped before storing. TC, SERVFAIL,
    REFUSED and responses that are not `Cacheable` are never stored.
  - TTLs count down on hits. Answers have RA set and AA cleared.
  - Prefetch: the first fresh hit on an entry with at least `threshold`
    hits within `window` and less than max(1 s, TTL/10) left starts a
    background refresh. That hit gets status `prefetch_refresh`.
  - Serve-stale (RFC 8767): an expired hit triggers a refresh. If the
    refresh fails or takes longer than `maxWait` (1.8 s), the stale data
    is served with TTL `answerTTL` (30 s) and EDE 3, status `cached_stale`.
    After a failed refresh, stale data is served at once for `answerTTL`
    without asking upstream again (the RFC 8767 failure recheck timer).
  - Identical misses share one fetch. The fetch runs detached from the
    client, bounded by `FetchTimeout` (5 s). Every waiter gets a
    personalised copy.
- Metrics:
  - `kudurru_cache_entries{kind}` (positive, negative; includes entries
    kept for serve-stale)
  - `kudurru_cache_requests_total`
  - `kudurru_cache_hits_total{kind,stale}`
  - `kudurru_cache_evictions_total{reason}` (capacity, expired, flush)
  - `kudurru_cache_prefetches_total`

### `internal/config` and `internal/pipeline`

- `cache.*` keys: `maxEntries` 200000, `minTTL` 5s, `maxTTL` 24h,
  `negativeTTL` 5m, `prefetch.threshold` 5, `prefetch.window` 2h,
  `serveStale.ttl` 72h, `serveStale.answerTTL` 30s,
  `serveStale.maxWait` 1800ms.
- `upstreams.ecs.*` keys: `mode` strip, `ipv4Prefix` 24, `ipv6Prefix` 56.
- New keys are validated, and `TestSchemaMatchesConfig` is green.
- `pipeline.Request.UpstreamGroup string` was added with a doc comment. The
  zone agent was told to use the same name.

## Wiring for `cmd/kudurru serve`

```go
import (
    "github.com/robertherbaugh/kudurru/internal/cache"
    "github.com/robertherbaugh/kudurru/internal/upstream"
    cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
    upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
)

upMetrics := upstream.NewMetrics(reg)                 // once per process
ucfg := upstream.ConfigFrom(cfg.Upstreams)
// optional: imp, _ := upstream.ImportDnsmasq(f); imp.Merge(&ucfg)
set, err := upstream.New(ucfg, upstream.Options{Logger: logger, Metrics: upMetrics})
if err != nil { return err }
fwd := upstreamstage.NewStage(set)

stages := []pipeline.Stage{&ingress.Stage{...} /*, authoritative, blocking ... */}
var cs *cachestage.Stage
if cfg.Cache.MaxEntries > 0 {
    c := cache.New(cache.OptionsFrom(cfg.Cache, cache.NewMetrics(reg)))
    // storage owner: if blob, ok := localdb.Get("cache"); ok { c.Load(bytes.NewReader(blob)) }
    cs = cachestage.New(c, cachestage.SettingsFrom(cfg.Cache), fwd) // fwd implements cachestage.Subnetter
    run("cache-sweep", func(ctx context.Context) error { return c.Run(ctx, time.Minute) })
    stages = append(stages, cs)
}
stages = append(stages, fwd)
chain, err := pipeline.New(stages...)                 // ingress → … → cache → upstream
logger.Info("pipeline ready", "stages", chain.Stages(), "upstreamGroups", set.Groups())

// Reload (cluster settings changed):
//   next, err := upstream.New(upstream.ConfigFrom(newCfg.Upstreams), sameOpts)
//   prev := fwd.Swap(next)
//   time.AfterFunc(groupTimeout, func() { _ = prev.CloseReplacedBy(next) })
//   if the conditional map or groups changed: cs.Cache().Flush()
// Shutdown, after the listeners stop:
//   cs.Wait(); var buf bytes.Buffer; _ = cs.Cache().Dump(&buf) → local.db; _ = fwd.Set().Close()
```

`fwd.Servers` (used in today's startup log line) is empty for `NewStage`.
Log `set.Groups()` instead, and use `set.Status()` for the API and probes.

## Measurements

Intel Xeon @ 2.90 GHz, 6 procs visible, shared with five other agents.
Go 1.27.2.

| Benchmark | Result |
|---|---|
| `cache.Get` hit, 200 k entries, random keys (`BenchmarkGet/hit`) | 665 ns/op, 0 allocs (memory-latency bound; hot keys run ~100 ns) |
| `cache.Get` miss | 101 ns/op, 0 allocs |
| `cache.Get` hit, all CPUs (`hit-parallel`) | 122 ns/op aggregate, 0 allocs |
| `MakeKey` | 34 ns/op, 1 alloc |
| `cache.Set` into a full cache (evicts) | 503 ns/op, 1 alloc (112 B) |
| Cache stage hit end to end: key, lookup, unpack, countdown, reply (`BenchmarkServe/hit`) | 1.75 µs/op, 17 allocs, 679 B |
| Cache stage hit, all CPUs | 490 ns/op aggregate |
| Cache stage miss bookkeeping (singleflight, copy, clamp, pack, store) | 9.4 µs/op |
| Memory, 1 M entries, 90 B messages, ~30 B keys (`BenchmarkMemory`) | 293 B/entry total, 174 B overhead/entry: ~293 MB at 1 M, ~59 MB at the 200 k default |
| Binary size (`CGO_ENABLED=0`, linking dnsproxy/upstream) | 20.39 MB → 21.44 MB (+1.05 MB) |

`BenchmarkStrategies`: three UDP fakes on loopback, 2000 sequential
exchanges each, latency per exchange.

| Scenario | parallel_best p50 / p99 | strict | random | rtt_weighted |
|---|---|---|---|---|
| fast (all answer at once) | 65 / 197 µs | 53 / 113 µs | 54 / 115 µs | 55 / 111 µs |
| slow-first (server 1 answers after 2 ms) | 65 / 177 µs | 2226 / 2317 µs | 95 / 2295 µs | 54 / 2193 µs |
| jitter (each server 0-3 ms at random) | 1206 / 3342 µs | 2214 / 3274 µs | 2220 / 3299 µs | 2233 / 3328 µs |

When every server is fast, parallel_best costs about 12 µs and double the
allocations, because it sends two exchanges. When one server is slow or
latency jitters, it cuts the median by 2 ms (slow-first) and nearly in half
(jitter). rtt_weighted learns to avoid a consistently slow server but still
probes it, which shows up in its p99.

## Tests

- `internal/upstream`:
  - In-process UDP+TCP and DoT fakes cover every strategy.
  - Failover; parallel_best racing two, taking the fastest and falling back
    to the rest of the group.
  - Random spread; RTT preference.
  - All servers failing: timeouts, SERVFAIL relay, REFUSED.
  - Backoff and recovery with a fake clock; all servers in backoff still
    get tried.
  - TC → TCP fallback; TCP and DoT transports.
  - Bootstrap host-name resolution; display names and metric labels;
    series cleanup on reload.
  - Routing (hint, longest suffix, dnsmasq keys, `#` exception, root).
  - dnsmasq import; ECS for every mode, end to end through a fake;
    `FinishReply`.
  - Concurrent exchanges under `-race`.
- `internal/stages/upstream`: routing and `Cacheable`, failure statuses and
  EDE, swap, the convenience constructor.
- `internal/cache`: key encoding, lifecycle, the hold, LRU eviction, sweep,
  the prefetch claim, Dump/Load round trip and garbage rejection,
  coalescing, concurrent use.
- `internal/stages/cache`:
  - Miss then hit with TTL countdown.
  - DO, CD and ECS key separation.
  - What is never cached; the hint bypass.
  - TTL clamp table.
  - 50-way coalescing with personalised copies; a client giving up while
    the detached fetch still fills the cache.
  - Prefetch, including a failed prefetch releasing its claim.
  - Serve-stale: maxWait, hold, late refresh, window end; serve-stale
    disabled.
  - Corrupt entry; chain order; a concurrent mix under `-race`.

`make test` (whole module, `-race`) and `golangci-lint run ./...` are clean.

## Surprises

- Two names differed from §11.1. The draft metric was
  `kudurru_cache_misses_total`, but §11.1 lists
  `kudurru_cache_requests_total`, so the code uses that name; the hit ratio
  is hits/requests. The upstream error label is `error`, not `reason`, as
  in §11.1.
- The capacity bound is enforced per shard, so a full cache holds about
  0.5 % fewer than `maxEntries`. This is the usual sharded-LRU trade-off,
  and the memory bound is never exceeded.
- dnsproxy's plain transport honours only its own timeout, not the
  context. `server.do` returns as soon as the context is done and lets the
  abandoned exchange finish in the background, bounded by the per-server
  timeout.
- dnsproxy/upstream newly links `AdguardTeam/golibs`, `AdguardTeam/dnscrypt`
  and `ameshkov/dnsstamps` (all Unlicense) and `robfig/cron/v3` (MIT). All
  are permissive and recorded in NOTICE.
- koanf keeps dotted map keys such as `"corp.example."` and
  `"[/lan/home.arpa/]"` intact under `upstreams.conditional`, so no loader
  change was needed. I checked this with a probe test.

## Recommended architecture.md changes (not edited)

- §7.1: the step 9 (cache) and step 11 (upstream) bullets were overwritten
  by decision text (D62 and D65). Restore them:
  - "**cache**: key (qname, qtype, qclass, DO, CD, ECS subnet); fresh hit →
    `cached` with TTL countdown; prefetch on hot entries
    (`prefetch_refresh`); RFC 8767 serve-stale (`cached_stale`, EDE 3);
    stores only `Cacheable` responses, OPT stripped."
  - "**upstream**: group = `Request.UpstreamGroup` ▸ longest conditional
    suffix ▸ `default`; strategies per §6.2; `Response.Upstream` =
    `<group>/<server>`; `servfail_upstream` / `timeout_upstream` with EDE
    22/23."
- §3.3 pipeline snippet: add `UpstreamGroup string` to `Request`.
- §6.2: document `upstreams.ecs { mode: strip|forward|add, ipv4Prefix: 24,
  ipv6Prefix: 56 }` and the optional `name=` server prefix.
- §11.1: `kudurru_upstream_requests_total` has no `proto` label yet, and
  `kudurru_upstream_responses_total{upstream,rcode}` and
  `kudurru_upstream_inflight{upstream}` are not implemented (see
  follow-ups).

## Follow-ups

- `cmd/kudurru serve`: switch to `upstream.New` + `NewStage`, insert the
  cache stage, start `Cache.Run`, and add the reload and shutdown hooks as
  above (owned by the serve/wiring agent).
- local.db persistence: the storage owner stores the `Dump` bytes on
  shutdown and calls `Load` at start.
- Remaining §11.1 upstream series: `responses_total{upstream,rcode}`,
  `inflight{upstream}`, and a `proto` label on requests.
- An `upstreams.importFile` setting for `ImportDnsmasq`.
- API surface: `Set.Status()` for `/api/v1/upstreams`, plus cache
  flush/inspect endpoints (`Flush`, `Delete`, `Peek`).
- Register the cache and strategy benchmarks with the benchgate
  (`bench/limits.txt`) once the blocking owner agrees on thresholds.
- DNSSEC validation (v0.2) must run before the cache stores anything; the
  stage order already allows this (`cache` → `dnssec` → `upstream`).
