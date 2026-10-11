# M2 work package: querylog

Branch `worktree-wf_ae7e11e2-8a2-5`. Covers docs/architecture.md §11.2 (the whole query-log pipeline, schema, status taxonomy, retention, privacy, the single-node half of the multi-node shapes), the querylog metrics of §11.1, the data side of §10 items 1-5, and D59.

## What was built

### `internal/querylog` (extended)

- **Event.** New fields, all additive: `ID` (stamped by the collector; the SQLite rowid and keyset cursor), `ClientName`, `ClientGroup`, `Zone`, `CNAME`, `ReplyType` (unknown, ip, cname, domain, nodata, nxdomain, servfail, refused, notimp, other, blob, none), `DNSSEC` (unknown, secure, insecure, bogus, indeterminate), `EDE`/`HasEDE`, `ECS`, `TraceID`, `Answer` (short summary). `Flags` gained `Prefetch`, `ECS` and `Traceparent`, plus `Bits()`/`FlagsFromBits()` for the `flags` column (RD, DO, TC, AD, CD, cached, stale, prefetch, ecs, traceparent in that bit order).
- **Collector (`Async`).** `NewAsync` keeps its signature and behaviour. `NewAsyncWith(AsyncOptions)` adds the §11.2 collector duties on the same single goroutine:
  1. Apply the exclude lists and the privacy transform once.
  2. Stamp monotonic ids (`FirstID`, from the writer).
  3. Publish to the live tail.
  4. Batch 1 000 events or 1 s (whichever comes first) into `Sink.Write`.

  The interval runs on an injectable `k8s.io/utils/clock`. Drops are counted per stage. The batch slice is reused, so a sink may keep the events (they are immutable after the transform) but not the slice.
- **Privacy (`Privacy`, `PrivacyFrom(config.Querylog)`).** Modes are `full | anonymize_client | hide_domains | hide_domains_clients | nothing`:
  - `anonymize_client`: either `/24` and `/48` truncation, or `pseudonym`: `anon-` plus 12 hex digits of HMAC-SHA256 under a random key. The key lives only in memory and rotates at UTC midnight, so the database cannot be reversed even by brute-forcing the IPv4 space. A side effect is that pseudonyms change across a restart. This mode also clears client names and ECS.
  - `hide_domains`: replaces the name and CNAME with `hidden` and clears the rule text, because the rule names the domain.
  - `hide_domains_clients`: as `hide_domains`, and also sets the client to `0.0.0.0`.
  - `nothing`: no fact rows, no per-client or per-domain rollups, and no live tail. Per-minute totals, qtypes and upstreams survive.
  - The answer summary is cleared in every mode except `full` (D59).

  Exclude lists:
  - Domains: exact names, `*.suffix` (the suffix itself and everything below it), or `/regex/`.
  - Clients: IP, CIDR or client name.
  - `sudn`: localhost, invalid, test, local, home.arpa, onion, alt, and the loopback, link-local, RFC 1918, ULA and `0`/broadcast reverse zones.

  Excluded events are not logged and not counted in rollups. The DNS Prometheus counters still see them.
- **Live tail (`Tail`).** Fan-out to subscribers with a buffer of 1 000 each, plus a hot ring (default 65 536). `Recent(n, afterID)` serves SSE `Last-Event-ID` resume and `earliest_ts_memory`. A slow subscriber loses events (`dropped_total{stage="tail"}`) rather than slowing the collector.
- **Metrics (`NewMetrics(reg)`)**, one set shared by the collector, tail and writer: `kudurru_querylog_events_total`, `_dropped_total{stage=queue|sink|tail,sink}`, `_batch_rows`, `_write_duration_seconds`, `_db_bytes`, `_wal_bytes`, `_rows`, `_oldest_timestamp_seconds`.
- **Name tables (`QTypeName`, `ParseQType`, `RcodeName`, `ParseStatus`)** so the read API speaks mnemonics without importing miekg/dns (§2).

### `internal/querylog/sqlitelog` (new)

**Writer** (implements `querylog.Sink`; `Open(ctx, Options)`):
- Opens `<dataDir>/querylog.db` with exactly the §11.2 PRAGMAs, applied per connection before the schema exists, so a fresh file is created with auto_vacuum INCREMENTAL (`TestPragmas` pins every value). There is one writer connection (`_txlock=immediate`) and a `query_only` reader pool.
- **Schema.** §11.2 verbatim: the dictionaries, the `query` fact table with exactly four indexes (the partial `query_status_ts` included), and the `rollup_1m`/`client`/`qtype`/`upstream` `_1m` tables. Hourly tables: domain, list and zone `_1h`, plus the "identical shape" `rollup_1h`, `client_1h`, `qtype_1h`, `upstream_1h`, `rollup_1d`, `client_1d` and `domain_1d`. `meta` holds `schema_version`, `node_id`, `first_ts`, `privacy_mode_at_write`, the derivation watermarks, `last_analyze` and `last_retention`. A newer schema is refused (`ErrSchemaTooNew`).
- **Batch write.** One `BEGIN IMMEDIATE…COMMIT` per batch, using a prepared `INSERT` with explicit collector ids. Dictionary ids come from per-table LRUs (domain 65 536 entries; the others scale from it) and are inserted with `INSERT … ON CONFLICT DO UPDATE … RETURNING id` only on a miss. `last_seen` is rewritten at most once per key per hour, with the remainder written back on close. A rolled-back batch purges the LRUs so no cached id can point at a rolled-back row.
- **Minute accumulators.** Updated after commit. At each minute boundary (plus 2 s) they are flushed as additive upserts, so late events, `Flush` and `Close` can never double-count or lose a minute.
- **Hourly and daily jobs:**
  - At H+1h+90 s, `_1h` is derived from the `_1m` tables, and `domain/list/zone_1h` from the fact table, which survives a crash.
  - `_1d` is derived from `_1h` once every hour of the UTC day is done.
  - Late events for an already-derived hour mark it dirty, and it is re-derived on the next tick.
  - For hours not yet derived, the writer keeps an in-memory per-hour domain/list/zone map. It is rebuilt from the facts on open and lets the read API answer "today" without scanning facts.
- **Retention:**
  - Daily at a random minute between 03:10 and 03:50 local time.
  - The size cap is checked every minute, deleting the oldest rows down to 95 % of `maxDBBytes`.
  - Deletes run in 10 000-row transactions, each followed by `PRAGMA incremental_vacuum`, and release the write lock between batches.
  - Rollups: `_1m` 35 d, `_1h` 400 d.
  - Dictionary rows not seen for 400 days (or since the oldest fact, if older) are pruned together with their per-key `_1d` rows.
  - `wal_checkpoint(TRUNCATE)` runs every 5 min, only when no read is in flight.
  - Weekly `ANALYZE` uses `analysis_limit=1000`. `PRAGMA optimize` and a checkpoint run on close.
- **Run(ctx)** is the always-on maintenance runnable (1 s tick).

**Reader** (`w.Reader()`): the JSON-ready read API (`api.go`). Every response embeds `Scope{scope:"node", nodes:[{name, ok, took_ms}], partial:false}`, so cluster scatter-gather can fill the same shape.

| §10 endpoint | Method | Notes |
|---|---|---|
| `GET /stats/summary` | `Summary(ctx, SummaryQuery{Window})` | total, blocked, percent, cached, forwarded, authoritative, dropped, other, unique domains, qps (last minute and window average), active/total clients, `domains_on_lists` (from `Options.DomainsOnLists`), rcodes, protocols, qtypes, latency avg/p50/p95/p99 from the histogram |
| `GET /stats/timeseries` | `Timeseries(ctx, TimeseriesQuery{Window, Step})` | dense points total/blocked/cached/forwarded/authoritative/other; `Step` 0 picks automatically (≤ 300 points, ≥ 1 h beyond the 35-day minute retention, ≥ 1 d beyond 400 days) |
| `GET /stats/top?dim=` | `TopDomains`, `TopClients`, `TopLists`, `TopZones` (`TopQuery{Window, N, Blocked}`) | blocked or permitted ranking, `total_in_window`, percent, `approximate`, `cutoff_rank` (cluster) |
| `GET /stats/upstreams` | `TopUpstreams` | total, errors (servfail_upstream), timeouts, avg/p95; `cache` and `zone:<name>` appear as upstreams |
| `GET /stats/clients/timeseries` | `ClientTimeseries(ctx, {Window, Step, N})` | top N clients plus `other` per bucket |
| `GET /stats/recent_blocked` | `RecentBlocked(ctx, n)` | one ordered probe of the partial index per blocked status |
| `GET /queries` | `Queries(ctx, QueryFilter)` | client (IP, CIDR, name, `*`), domain (exact or `*`), status list or class, qtype, upstream, list, zone, since/until; keyset `cursor` on id (string, decimal at node scope); `records_total`/`records_filtered`; `earliest_ts_disk`/`_memory` |
| `/queries/suggestions` | `Suggestions(ctx, field, prefix, n)` | domain, client (ip and name), upstream, list, zone; index range scans |
| `SSE /queries/stream` | `Subscribe(ctx) <-chan *querylog.Event` | the collector's tail (after privacy) |
| `GET /querylog/status` | `Status(ctx)` | rows, db/wal bytes, oldest row, privacy, retention, writer stats, subscribers |

Reads are current to the last committed batch:
- A query window is split across tiers (`_1d` → `_1h` → `_1m` or facts), and the writer's unflushed minutes and underived hours are passed into the same SQL statement as a JSON array (`json_each`).
- A flush racing the read is detected with a flush sequence number, and the read is retried.
- The query log page selects ids first, then the wide rows and their dictionary joins. This took the domain-filtered page from 37 ms to 8 ms.

### `internal/config` (additive)

New keys `querylog.enabled` (true), `querylog.sink` (`sqlite`, or `slog`), `querylog.privacy` (`full`), `querylog.anonymize` (`prefix` or `pseudonym`), `querylog.retention.raw` (`720h`; 0 means size-only, otherwise at least 1 h), `querylog.retention.maxDBBytes` (`10GiB`), `querylog.exclude.domains`/`clients` (`[]`) and `querylog.exclude.sudn` (true). All are cluster scope, with D59 defaults.

A new `ByteSize` type (with a `bytes` schema type) reads `10GiB`, `500MB` or a plain integer from YAML and from the environment. Validation covers unknown modes, negative values and bad regexes. The drift test is unchanged and green.

### `internal/stages/log` (additive)

The observer now fills the following:
- client id and first group, and the matched zone (`Request.Zone` exposes `Name()`; a typed-nil handle is tolerated);
- reply type, EDE from the response OPT, and ECS from the request OPT;
- the answer summary (first three RRs, addresses bare, other types as `TYPE rdata`, `(+n)` for the rest, 256 B cap);
- the prefetch flag.

## Wiring for serve (integrator)

This replaces the M1 block at `cmd/kudurru/serve.go` "Query log: slog sink behind the non-blocking collector". The sqlite sink is the default when `querylog.enabled`. `querylog.sink: slog` keeps the M1 behaviour.

```go
// Query log (§11.2): privacy once at ingest, ids, live tail, sqlite or slog sink.
var (
	collector querylog.Collector // stays a nil interface when disabled: the observer checks it
	async     *querylog.Async
	qlReader  *sqlitelog.Reader // hand to the M3 API server
	qlWriter  *sqlitelog.Writer
)
if cfg.Querylog.Enabled {
	qlMetrics := querylog.NewMetrics(reg)
	privacy, err := querylog.PrivacyFrom(cfg.Querylog)
	if err != nil {
		return err
	}
	tail := querylog.NewTail(0, 0, qlMetrics) // 65 536-event ring, 1 000 per subscriber
	var sink querylog.Sink
	var firstID uint64
	switch cfg.Querylog.Sink {
	case "sqlite":
		opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.Node.DataDir, cfg.Node.Name)
		if err != nil {
			return err
		}
		opts.Metrics, opts.Tail, opts.Logger = qlMetrics, tail, logger
		// opts.DomainsOnLists = <blocking engine entry count>  (policy package, M2)
		if qlWriter, err = sqlitelog.Open(ctx, opts); err != nil {
			return err // sqlitelog.ErrSchemaTooNew: refuse to start
		}
		sink, firstID, qlReader = qlWriter, qlWriter.NextID(), qlWriter.Reader()
	default: // "slog"
		sink = &querylog.SlogSink{Logger: logger, Level: slog.LevelInfo}
		if err := sink.Open(ctx, nil, reg); err != nil {
			return err
		}
	}
	async = querylog.NewAsyncWith(querylog.AsyncOptions{
		Sink: sink, Privacy: privacy, Tail: tail, Metrics: qlMetrics, FirstID: firstID, Logger: logger,
	})
	collector = async
}
chain.Observe(&logstage.Observer{Node: cfg.Node.Name, Collector: collector, Metrics: dnsMetrics})

// runner table (always-on, every replica):
if qlWriter != nil {
	add("querylog", false, qlWriter.Run) // minute flush, _1h/_1d, retention, checkpoint, gauges
}

// shutdown, after the DNS listeners have stopped (where collector.Close is today):
if async != nil {
	if err := async.Close(shutdownCtx); err != nil { // drains the queue, then sink.Close():
		logger.Warn("query log flush incomplete", "err", err, "dropped", async.Dropped()) // final rollup flush,
	} //                                                                    last_seen write-back, PRAGMA optimize, checkpoint
}
```

Notes:
- `Run` returns on context cancel without closing anything. `async.Close` owns the final flush, so the order is runner stops, then `async.Close`. A `Run` that is still mid-retention sees `ErrClosed` and stops quietly.
- `querylog.Collector` must stay a nil interface when the log is off. Passing a nil `*Async` would make the observer call a nil pointer.
- The kudurru Helm chart's `persistence.querylog` PVC maps to `node.dataDir`; nothing else is needed.

## Decisions and deviations

- **Rollup `ts` is unix seconds of the UTC bucket start.** `query.ts` stays unix µs as specified. The spec does not fix rollup units; seconds keep the WITHOUT ROWID keys small and match the API.
- **`query.proto` 6 = doh3.** The schema comment reserves 5 for dnscrypt, so DoH3 takes 6 rather than colliding. In the rollup protocol columns, `doh` counts DoH and DoH3.
- **Status-class buckets in `rollup_1m`:**
  - forwarded: forwarded, retried, prefetch_refresh
  - authoritative: authoritative, local_record, special_domain
  - cached: cached, cached_stale
  - blocked: 16-23
  - dropped: dropped
  - other: total minus the above (refused_acl, servfail, timeout, bogus, unknown).
- **`rollup_list_1h.hits` counts blocked queries**, matching `kudurru_block_list_hits_total`. The schema has a single column. `TopLists(permitted)` (allowlist matches) is answered from the pending-hour cache and the fact table, so it is bounded by raw retention. See the follow-ups.
- **Upstream dictionary keys.** The pipeline's `Response.Upstream` (the group name today), `cache` for cache hits, and `zone:<zone>` for local answers. Blocked queries have no upstream.
- **Dictionary pruning.** Pruning domains by raw retention alone would orphan the 400-day `_1h` and forever `_1d` rollups that reference them. So keys are pruned only when unseen for 400 days (or since the oldest fact), and their per-key `_1d` rows go with them. The aggregate `rollup_1d` is kept forever. Without AUTOINCREMENT, a pruned max id can be reused, but by then every reference to it has expired.
- **"30 s completion grace."** The M1 pipeline emits an event only when the response is final, so no event is ever in progress. The grace survives as the additive minute flush plus the 90 s delay before an hour is derived.
- **Per-policy `queryLog: false`** has no seam yet. Policies arrive with the policy package. See the follow-ups.

## Measurements

The machine is shared (8 vCPU, 4 other agents); modernc.org/sqlite is pure Go. Commands: `KUDURRU_MEASURE=1 go test -run TestMeasureIngest -v ./internal/querylog/sqlitelog/` and `go test -bench . -run XXX ./internal/querylog/sqlitelog/`.

The traffic mix has 200 clients, 20 000 domains with a cubic head, 15 % blocked, 45 % cache hits and two-address answers. It is fed in 1 000-row batches on a fake clock, and only `Write` and `Maintain` are timed.

| Run | Rows | Write time | Capacity | Duty at that rate | Max batch | DB bytes/row (all tables) | Fact table + 4 indexes |
|---|---|---|---|---|---|---|---|
| 1 000 ev/s × 300 s, privacy full | 300 000 | 9.7 s | **31 000 rows/s** | 3.2 % | 58 ms | 146.9 B | 74.6 + 66.5 = **141.1 B** |
| 10 000 ev/s × 60 s, privacy full | 600 000 | 21.1 s | **28 500 rows/s** | 35 % | 59 ms | 144.0 B | 74.6 + 66.4 = **141.1 B** |
| 10 000 ev/s × 60 s, anonymize_client (no answer) | 600 000 | 20.5 s | 29 300 rows/s | 34 % | 54 ms | 118.4 B | 49.0 + 66.4 = **115.5 B** |

| Benchmark | Result |
|---|---|
| `BenchmarkWriteBatch` (one 1 000-row transaction) | 24.7 ms, 40 400 rows/s, 146 B/row |
| `BenchmarkQueriesPage` (100 rows, domain filter, 200 000 rows) | 7.9 ms (37 ms before the ids-first rewrite) |
| `BenchmarkSummary24h` (last 24 h, 1 q/s over the 20 000-domain mix) | 82 ms, dominated by the exact unique-domain count over `rollup_domain_1h` |
| Retention: 25 000 expired rows of 30 000 (3 batches + incremental_vacuum) | page_count 865 → 274, freelist 0 |
| Size cap: 40 000 rows, cap at half the file | 30 000 oldest rows deleted, 4.6 MB → 1.9 MB |

What the numbers say:
- **~100 B/row is not met; the measured cost is ~141 B/row (full) and ~116 B/row (no answer).**
  - The answer summary costs ~25 B/row.
  - The four indexes cost 66 B/row, not the ~40-60 the sizing assumed. `ts` as unix µs needs SQLite's 8-byte integer encoding (values > 2^47), and it appears in the table and in three indexes, about 32 B/row.
  - `query_client_ts` and `query_domain_ts` take random-order inserts, so their pages sit near the B-tree fill average rather than full.
  - Adjusted sizing: about 1.2 GB/day at 100 qps, so the 10 GiB cap holds about 8 days at 100 qps rather than 30. `maxDBBytes` wins as designed. D59's "30 d capped at 10 GiB" still holds as a cap.
- Under `-race` the pure-Go SQLite is roughly 20× slower, so the retention and measurement tests shrink their row counts and the delete batch (`Writer.deleteBatch`) when `raceEnabled`; the logic exercised is the same.
- **Capacity.** The writer sustains about 28-31k rows/s on one core of this shared machine. 1 000 qps costs about 3 % of a core and 10 000 qps about 35 %. A batch never held the write lock longer than 60 ms, so the 10 000-event queue absorbs it.

## Surprises

- The §11.2 sizing (~100 B/row) undercounts the indexes. See Measurements; storing `ts` as unix ms would save about 8 B/row, but the schema is fixed by the spec. The integrator may want to record the measured figure in §11.2.
- The sort for "ORDER BY id DESC LIMIT" on an index-filtered query fetched every matching wide row. Selecting ids first made the page 4.7× faster.
- `TestMeasureRecordSetChanges` in `internal/controller` failed once in the full `-race` run (2 002 status writes against 2 000 expected). That package does not import any package touched here. It passes when rerun on its own (`go test -race -run TestMeasureRecordSetChanges ./internal/controller/`), so it is a timing-sensitive measurement flaking on a machine shared by five agents.

## Follow-ups

| Item | Target |
|---|---|
| `allowed` column (or `rollup_list_allow_1h`) so permitted list rankings do not read facts beyond the pending hours: architecture change to §11.2 | M3 |
| Unique domains over long windows: HLL sketch per hour (also needed for `scope=cluster` unions over 10 k) or a per-minute cache; currently 82 ms for 24 h at 1 q/s, roughly linear in distinct domains per hour | M3 |
| Per-policy `queryLog: false`: a `NoLog` bit on `pipeline.Response` set by the policy stage, honoured in `stages/log` | M2 policy package |
| `Options.DomainsOnLists` from the blocking engine for the fourth tile | M2 policy package (integrator) |
| Rebuild minute rollups from the facts after a crash (today a crash loses up to one minute of rollups; facts lose at most one batch) | M3 |
| `DELETE /querylog` (targeted purge with audit) and `GET /queries/export`: the Writer has the batched-delete machinery; needs API and RBAC | M3 |
| HTTP layer: huma handlers over the Reader, `took_ms`, SSE framing with `Last-Event-ID` → `Tail.Recent(n, id)`, `scope=cluster` scatter-gather filling `Scope.Nodes`/`Partial` and merging top-N from n×4 | M3 |
| dnstap/ClickHouse/Loki sinks behind `querylog.Sink` with their own bounded queues (`dropped_total{stage="sink",sink}` is ready) | v0.2 |
| `SlogSink` still imports miekg/dns for type names; switch it to `querylog.QTypeName`/`RcodeName` | M3 cleanup |
