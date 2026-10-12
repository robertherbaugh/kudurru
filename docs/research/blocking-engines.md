> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [docs/architecture.md](../architecture.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# Blocking engines research: blocky, AdGuard Home, urlfilter, dnsproxy

Research key: `blocking-engines`  
Date: 2026-10-10  
Method: `git clone --depth 1` of all four repos into `/tmp/kudurru-research/blocking-engines/`, read of README/docs/config examples/core packages, GitHub API for releases/licenses, AdGuard KB page for rule syntax. Anything not confirmed from a primary source is marked **UNVERIFIED**.

---

## 1. Sources, versions, licenses

| Project | Repo | Latest release (GitHub API) | HEAD commit read | License (LICENSE file + GitHub SPDX) | Go version (go.mod) | Stars |
|---|---|---|---|---|---|---|
| blocky | github.com/0xERR0R/blocky | **v0.35.0** (2026-09-05) | `ca450f8` 2026-10-09 | **Apache-2.0** | go 1.26.2 | 7,020 |
| AdGuard Home | github.com/AdguardTeam/AdGuardHome | **v0.107.79** stable (2026-08-18); v1.0.0-b.1 beta (2026-09-30) | `8dedd32` 2026-10-08 | **GPL-3.0** | go 1.27.1 | 37,339 |
| urlfilter | github.com/AdguardTeam/urlfilter | **v0.23.4** (2026-06-25) | `347285f` 2026-08-19 | **GPL-3.0** (674-line GPLv3 text) | go 1.26.5 | 116 |
| dnsproxy | github.com/AdguardTeam/dnsproxy | **v0.86.0** (2026-10-01) | `7fcb1cc` 2026-10-07 | **Apache-2.0** | go 1.26.8 | 3,358 |

Transitive libraries that matter for reuse (GitHub SPDX, verified via API 2026-10-10):

| Library | License | Used by |
|---|---|---|
| github.com/AdguardTeam/golibs | Unlicense (public domain) | dnsproxy, urlfilter, AGH |
| github.com/AdguardTeam/dnscrypt | Unlicense | dnsproxy |
| github.com/miekg/dns | BSD-3-Clause | everyone |
| github.com/0xERR0R/expiration-cache | **MPL-2.0** (file-level copyleft; fine as a dependency) | blocky |
| github.com/ameshkov/dnsstamps | UNVERIFIED (not checked) | dnsproxy |

### 1.1 License consequences for an Apache-2.0 kudurru

- **blocky (Apache-2.0)**: code can be copied, adapted, vendored. Keep NOTICE/attribution.
- **dnsproxy (Apache-2.0)**: can be imported as a Go module (`github.com/AdguardTeam/dnsproxy/upstream`, `/proxy`) or copied. golibs (Unlicense) and dnscrypt (Unlicense) impose nothing.
- **urlfilter (GPL-3.0)** and **AdGuard Home (GPL-3.0)**: importing either into an Apache-2.0 binary makes the combined work GPL-3.0. **Do not import `github.com/AdguardTeam/urlfilter`.** Reading their code for ideas and re-implementing the adblock-DNS grammar from the public spec (the KB page) is fine; copying code is not. Note that AdGuard's own safe-search rule lists (`internal/filtering/safesearch/rules/*.txt`) and the generated blocked-services table (`servicelist.go`) live inside the GPL repo; treat them as GPL data too unless an upstream source with a different license is found (the blocked-services data originates from `AdguardTeam/HostlistsRegistry`, license UNVERIFIED).

---

## 2. blocky (Apache-2.0) in depth

### 2.1 Package layout

```
blocky/
  api/        oapi-codegen generated server+client (docs/api/openapi.yaml)
  cache/      ExpiringCache interface, redis decorator, shards, prefetching/, stringcache/ (block-list matchers)
  config/     YAML config structs, JSON schema (docs/config.schema.json), migration/ for deprecated keys
  lists/      ListCache (group -> matcher), downloader (+ on-disk cache), parsers/ (hosts / domains / wildcard / regex)
  resolver/   one file per chain element (blocking, caching, custom_dns, conditional_upstream, parallel_best, strict, ...)
  querylog/   writers: database (gorm: mysql/postgres/timescale/sqlite), file (csv), dnstap, logger, none
  metrics/    Prometheus registry + event-driven gauges
  redis/      client, pubsub loop, event bridge (blocking-state sync)
  server/     listeners (UDP/TCP/DoT/DoH/DoH3), resolver chain wiring, HTTP API
  trie/       SplitTLD helper (used by wildcard cache)
  evt/        in-process event bus (asaskevich/EventBus)
```

### 2.2 Resolver chain (exact order from `server/server.go`)

Every request passes through a linked chain; each element either answers or calls `next`. Order matters and is documented inline:

```
stats -> rateLimiting -> ecsClient (adopt ECS as client IP) -> clientNames (rDNS/host mapping)
 -> filtering (drop qtypes) -> fqdnOnly -> ede -> queryLogging -> metrics
 -> customDNS -> hostsFile -> rebindingProtection -> blocking
 -> dnssec (validate before cache) -> caching -> dns64 (after cache) -> ecs (add/strip EDNS0 subnet)
 -> conditionalUpstream -> specialUseDomainNames -> upstreamTree (per-client upstream groups)
```

Design points worth copying: blocking runs **above** the cache (so cache hits are re-checked for CNAME/IP matches), DNSSEC runs **above** the cache (only validated data cached), rebinding protection re-inspects cached answers, and custom DNS / hosts / conditional answers are typed (`ResponseType`) so they are never written to the shared cache.

### 2.3 Config format

- Single YAML file or a directory of YAML files merged (`docs/configuration.md` "Multiple configuration files").
- Published JSON schema (`docs/config.schema.json`, generated with invopop/jsonschema) so editors validate (`# yaml-language-server: $schema=...`).
- Deprecated keys are migrated at load with a `config/migration` package (`blackLists -> denylists`, `refreshPeriod -> loading.refreshPeriod`, etc.) and logged.
- Secrets can be `file:/run/secrets/...` for redis password, DB DSN.
- `blocky validate --config` CLI; **no hot reload** of config (SIGUSR1 only prints the configuration; SIGINT/SIGTERM stop). Lists are refreshed on a timer or via `POST /api/lists/refresh`.
- Full annotated example: `docs/config.yml` (622 lines). The `blocking:` section:

```yaml
blocking:
  denylists:
    ads:
      - https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
      - /path/local.txt
      - |
        someadsdomain.com
        *.example.com
        /^banners?[_.-]/
  allowlists:
    ads: [allowlist.txt]
  clientGroupsBlock:
    default: [ads, special]
    laptop*: [ads]              # glob on client name (* and [0-9])
    192.168.178.1/24: [special] # CIDR
    kid-laptop: [ads, adult]    # exact client name
  schedules:
    workhours: {weekdays: [mon,tue,wed,thu,fri], start: "08:00", end: "18:00"}
  listSchedules:
    special: [workhours]
  blockType: zeroIp            # zeroIp | nxDomain | refused | "1.2.3.4, 2001:db8::1"
  blockTTL: 6h
  loading:
    refreshPeriod: 4h
    downloads: {timeout: 5s, readTimeout: 20s, attempts: 3, cooldown: 500ms, cachePath: /var/cache/blocky/lists}
    concurrency: 4
    strategy: blocking         # blocking | failOnError | fast
    maxErrorsPerSource: 5
```

### 2.4 Supported list formats (parsers in `lists/parsers/hosts.go`)

One line parser tries, in order: `HostListEntry` (single domain, or `||domain^` ABP markers stripped, or `/regex/`, or an IP), `HostsFileEntry` (`IP name [aliases]`, `%iface` tolerated), `WildcardEntry` (`*.domain` only; no other `*`). Lines: `#` comments (inline too), `!` comments and `[Adblock ...]` header skipped.

| Format | Example | Supported | Notes |
|---|---|---|---|
| hosts file | `0.0.0.0 ads.example` | yes | IP ignored; every name+alias becomes a block entry |
| domains-only | `ads.example` | yes | exact match of the domain only (not subdomains) |
| ABP basic | `\|\|ads.example^` | partial | markers trimmed, then treated as exact domain; **subdomain semantics of `\|\|` are NOT honoured**, modifiers unsupported |
| ABP exception `@@` | `@@\|\|x^` | **no** | exceptions only via `allowlists:` |
| `$dnsrewrite/$dnstype/$client` | | **no** | |
| wildcard | `*.example.com` | yes | matches domain and all subdomains (prefix `*.` only) |
| regex | `/^banners?[_.-]/` | yes | Go RE2, unanchored by default; doc warns about memory/speed; warning logged if >500 regexes |
| IP literal | `1.2.3.4`, `[2001:db8::1]` | yes | used for **response** IP blocking |
| IDNA | `bücher.example` | yes | UTS#46 mapped to A-label at parse time |

Validation: 255-byte name, 63-byte labels, relaxed label charset (`[a-zA-Z0-9_-]`), trailing root dot trimmed.

### 2.5 Matching data structures (`cache/stringcache/`)

The task mentioned "hash/radix/trie options": in the current tree there is **no user-selectable option**. blocky evolved from `map[string]struct{}` → a radix-like trie (`trie/`, still present, but now used only for `SplitTLD`) → the current fixed chain. All three matchers are built per group and chained:

```go
groupedCache: stringcache.NewChainedGroupedCache(
    regexCache,                                   // /.../ entries
    stringcache.NewInMemoryGroupedWildcardCache(), // *.x entries
    stringcache.NewInMemoryGroupedStringCache(),   // everything else (catch-all, last)
)
```

1. **String cache** (exact domains + IPs): entries are lower-cased, bucketed by **string length**, each bucket is one sorted, concatenated `string`; lookup is `sort.Search` over `len/bucketLen` fixed-width slots (binary search, zero allocation, no per-entry pointer/map overhead). Build is append-then-sort-then-compact (O(n log n)).
2. **Wildcard cache**: keyed by TLD (`SplitTLD`), each TLD holds the same length-bucketed sorted string map for the "rest" plus a **two-bit-per-entry Bloom-ish filter in `[]uint64`** (`maphash`, one word per probe). Lookup walks parent labels broadest-first and returns the matched rule `*.base.tld`. A nil bucket means `*.tld` itself was listed.
3. **Regex cache**: linear scan of compiled `[]*regexp.Regexp`.

Lookup goes **cheapest-first** (string, then wildcard, then regex), and groups already matched are dropped from the remaining search. The whole `map[group]stringCache` is held in an `atomic.Pointer` and replaced copy-on-write on refresh (readers are lock-free). `Contains` returns `map[group]matchedRule` so the block reason can name the rule (`BLOCKED (ads: *.docler.com)`), while the Prometheus label uses group names only (bounded cardinality).

Memory claim "low memory footprint" is from the README; **no published numbers** (UNVERIFIED). Benchmarks exist in-tree (`string_caches_benchmark_test.go`, `wildcard_cache_benchmark_test.go`, `blocking_resolver_bench_test.go`) but no results are published.

### 2.6 List download / refresh / caching pipeline (`lists/`)

- `NewListCache` starts a periodic refresh (`refreshPeriod`, default 4h, `<=0` disables). Refresh is serialised with a 1-slot semaphore; groups are rebuilt one at a time (bounded memory) while sources inside a group are fetched with `concurrency` producers feeding a channel consumer (ThinkChaos/parcour job groups).
- Sources: `http(s)://`, file path, inline text (YAML block scalar). Opened via `SourceOpener`.
- Downloader: `avast/retry-go` with `attempts`/`cooldown`, distinguishes transient (timeout) vs HTTP-status errors; per-URL `timeout`, `readTimeout`, `readHeaderTimeout`, `writeTimeout`.
- **On-disk cache** (`cachePath`): file named `sha256(url)`; stores ETag/Last-Modified in memory and sends `If-None-Match`/`If-Modified-Since`; 304 → serve disk copy; network failure (not 4xx/5xx) → serve stale disk copy; body is tee'd to a temp file and atomically renamed; no fsync (cache is disposable). With `strategy: fast`, groups are **seeded from disk** before the first network refresh so the server answers immediately.
- Error tolerance: `maxErrorsPerSource` (default 5, -1 unlimited) per source; a group fails only if **no** entries parsed at all; on failure the previous in-memory group is kept.
- Init strategies: `blocking` (wait for lists), `failOnError` (exit on error), `fast` (start immediately, load in background).
- Events: `BlockingCacheGroupChanged`, `CachingFailedDownloadChanged` feed Prometheus gauges `blocky_denylist_cache_entries{group}`, `blocky_allowlist_cache_entries{group}`, `blocky_failed_downloads_total`, `blocky_last_list_group_refresh_timestamp_seconds`.

### 2.7 Allowlist precedence, allow-only mode, schedules

From `resolver/blocking_resolver.go` `handleDenylist`:
1. If the question name matches any allowlist of any group assigned to the client → forward (allow wins across groups).
2. Else if **every** group assigned to the client has only allowlists (`allowlistOnlyGroups`) → block ("BLOCKED (ALLOWLIST ONLY)") – an exclusive-allow mode for e.g. kids' devices.
3. Else if any denylist matches → block.
4. After the upstream answer: each A/AAAA/CNAME in the answer is checked (allowlist first, then denylist) – deep CNAME + IP inspection.

Schedules: named `{weekdays, start, end}` windows (overnight if start > end), mapped to list groups via `listSchedules`; multiple schedules OR'ed; unknown schedule name makes the group inactive (fail-closed on typo). Exclusive-allow mode is derived from *configured* groups, not currently active ones.

Runtime toggles: `DisableBlocking(duration, groups)` / `EnableBlocking()` (API, CLI) with auto re-enable timer; state is published over Redis pub/sub channel `blocky_sync_enabled` to other instances (`redis/event_bridge.go`).

### 2.8 Client identification and client groups

`clientGroupsBlock` keys are classified once at config load into an index (`clientGroupsIndex`): exact IDs (`byID` map: IP literal, FQDN, literal name, `default`), pre-parsed CIDRs, FQDN identifiers (resolved to IPs through the chain and cached with TTL in an expiring cache), and glob names (`filepath.Match`). Per query: client names (from `clientLookup`: static `clients:` map, in-memory reverse entries from customDNS/hostsFile, then rDNS via `clientLookup.upstream`, else IP) are looked up, then IP, then CIDRs, then FQDN identifiers; union of groups; fall back to `default`. DoT/DoH clients can self-identify with `id-<name>.example.com` SNI or `/dns-query/<name>` path. The same identifier types select **per-client upstream groups** (`upstreams.groups`) with order IP → name → CIDR (first match wins there).

### 2.9 Block responses

| blockType | A/AAAA | other qtypes | notes |
|---|---|---|---|
| `zeroIp` (default) | `0.0.0.0` / `::` with `blockTTL` | NXDOMAIN | |
| `nxDomain` | NXDOMAIN + synthesized SOA in authority (RFC 2308, TTL = blockTTL) | same | |
| `refused` | REFUSED, no records | same | doc warns stub resolvers fall back to another server |
| custom IPs | matching family from the list | NXDOMAIN (fallback zeroIP handler) | for a "blocked" landing page |

`blockTTL` default 6h. Blocked responses get EDE 15 (Blocked) when `ede.enable`.

### 2.10 Custom DNS, rewrites, conditional forwarding

- `customDNS.mapping` (name → comma-separated IPs, subdomain inheritance, automatic PTR), `customDNS.zone` (RFC 1035 zone text with `$ORIGIN/$TTL/$INCLUDE/$GENERATE`, A/AAAA/CNAME/TXT/SRV), `customDNS.rewrite` (replace a domain suffix before lookup), `filterUnmappedTypes` (NODATA+SOA vs forward), CNAME target resolution through the chain with loop protection.
- `conditional.mapping` (domain → upstreams, longest-suffix match via `searchDomainOrParent` using `dns.NextLabel`), `.` key for unqualified names, `in-addr.arpa` zones for local PTR, `rewrite` + `fallbackUpstream` (split-horizon: try rewritten name on internal resolver, else original name upstream). Conditional groups always init in `blocking` mode.

### 2.11 Upstreams: formats, strategies, protocols, bootstrap

- Format: `[net:]host[:port][/path][#commonName]` with `net` ∈ `tcp+udp` (default), `tcp-tls` (DoT), `https` (DoH, H2; DoH3 via `http3`), `quic` (DoQ RFC 9250); or **DNS Stamps** `sdns://` (plain/DoH/DoT/DoQ; DNSCrypt **not** supported) which may carry SHA-256 cert hashes → automatic certificate pinning (`VerifyPeerCertificate`, session resumption disabled when pinned).
- Strategies (`upstreams.strategy`): `parallel_best` (pick 2 weighted-random resolvers, race, first non-error wins; weight decays for 1h after an error, implemented as single-pass weighted reservoir sampling), `random` (one weighted-random, retry on a different one), `strict` (ordered failover). Timeout default 2s. Per-group `init.strategy` as for lists.
- Bootstrap (`bootstrapDns`): list of `{upstream, ips}` or `resolvFile:` entries; internally a mini chain (filtering → caching with forced prefetch and min 1h TTL → parallel_best) used to resolve upstream and list hostnames; falls back to system resolver.
- Connection handling: DoT connection pool (8 idle, 30s idle timeout), EDNS0 UDP buffer floor 1232 toward upstreams, QUIC keep-alive/idle settings, `connectIPVersion: dual|v4|v6`, custom `userAgent`.
- `upstreamTree` resolver picks the group per client and wraps one strategy resolver per group.

### 2.12 Caching (`resolver/caching_resolver.go`, `cache/`)

- Key `qtype:domain`; value is the packed wire message with OPT stripped. Only `RESOLVED`/`SYNTHESIZED` answers are cached (never conditional/custom/hosts/SUDN).
- `minTime`/`maxTime` clamp (0 = use TTL, maxTime<0 = no caching), `cacheTimeNegative` (default 30m; NXDOMAIN and NODATA), `maxItemsCount` soft limit, `exclude: [/regex/]`, TTL countdown on hit (all sections aged by elapsed), not cached: truncated, CD-flag, ECS with non-full prefix.
- **Prefetching**: keys queried more than `prefetchThreshold` (5) times in `prefetchExpires` (2h) are reloaded on expiry (reload re-enters the chain below the cache with DO bit if DNSSEC validation is on). Metrics `blocky_prefetches_total`, `blocky_prefetch_hits_total`, `blocky_prefetch_domain_name_cache_entries`.
- Store: `0xERR0R/expiration-cache` (MPL-2.0), sharded (`cache.ShardCount()`), with callbacks for hits/misses/puts.
- **Redis shared cache** (`cache/redis.go`): decorator around the local cache. `Put` writes local then enqueues to a buffered channel (1000, drop-on-full with `blocky_redis_cache_buffer_drops_total`); a writer goroutine pipelines batches of 100 every 100ms as `SET key value EX ttl` and `PUBLISH`es `{entries, client-id}`; a subscriber applies other instances' entries locally; at startup `SCAN prefix*` + pipelined `TTL/GET` warms the local cache. Reads are **local-only** (Redis is a replication bus, not a read path). Sentinel supported. Blocking enable/disable is synced through the separate event bridge channel.

### 2.13 EDNS Client Subnet

Two resolvers: `ECSClientResolver` (top of chain) adopts a full-prefix ECS address as the internal client IP when `ecs.useAsClient`; `ECSResolver` (below cache) adds the masked client subnet (`ipv4Mask`/`ipv6Mask`), forwards an incoming ECS when `ecs.forward`, or strips it. Responses for non-full-prefix ECS queries are not cached.

### 2.14 Query logging

Types: `mysql`, `postgresql`, `timescale`, `sqlite` (pure-Go driver, WAL, not on MIPS/loong64/most BSD), `csv` (one file per day), `csv-client` (per day per client), `console`, `dnstap` (Frame Streams over unix/tcp, `CLIENT_RESPONSE` frames with wire messages), `none`. DB writers batch via gorm every `flushInterval` (30s) and prune by `logRetentionDays`. Field selection `fields: [clientIP, clientName, responseReason, responseAnswer, question, duration]`; `ignore.sudn`, `ignore.domains` (exact / `*.wildcard` / `/regex/`). Entry model:

```go
type LogEntry struct {
    Start time.Time; ClientIP string; ClientNames []string; DurationMs int64
    ResponseReason, ResponseType, ResponseCode, QuestionType, QuestionName, Answer string
    BlockyInstance string; QueryWire, ResponseWire []byte; QueryTime, ResponseTime time.Time
    SocketProtocol model.RequestProtocol
}
// DB table adds EffectiveTLDP (publicsuffix) and Hostname (instance), indexes on RequestTS, ClientName, ResponseType
```

Grafana dashboards are shipped (`docs/blocky-grafana.json`, `blocky-query-grafana*.json`).

### 2.15 Prometheus metrics (names from source)

`blocky_build_info`, `blocky_blocking_enabled`, `blocky_denylist_cache_entries{group}`, `blocky_allowlist_cache_entries{group}`, `blocky_last_list_group_refresh_timestamp_seconds`, `blocky_failed_downloads_total`, `blocky_cache_entries`, `blocky_cache_hits_total`, `blocky_cache_misses_total`, `blocky_prefetches_total`, `blocky_prefetch_hits_total`, `blocky_prefetch_domain_name_cache_entries`, `blocky_query_total{client,type}`, `blocky_error_total`, `blocky_request_duration_seconds{response_type}` (histogram), `blocky_response_total{reason,response_code,response_type}`, `blocky_client_response_total`, `blocky_rate_limit_drops_total{protocol}`, `blocky_rate_limit_cap_exhausted_total`, `blocky_rate_limit_active_buckets`, `blocky_redis_cache_buffer_drops_total`, plus DNSSEC metrics. Also an in-memory 24h `GET /api/stats` JSON (summary categories blocked/filtered/forwarded/cached/local/errors, per-hour series, top-20 domains/blocked/clients, list and cache gauges).

### 2.16 REST API and CLI

OpenAPI 3 spec (`docs/api/openapi.yaml`), code-generated server/client. Endpoints: `GET /api/blocking/{enable,disable?duration&groups,status}`, `POST /api/lists/refresh`, `POST /api/query`, `POST /api/cache/flush`, `GET /api/stats`. No auth, no users, no list CRUD (lists live only in YAML). CLI `blocky blocking|query|lists refresh|stats|validate` calls the API.

### 2.17 Other notable features

Per-client token-bucket rate limiting (drops silently, fail2ban-friendly log line, IPv6 /64 aggregation, allowlist), DNS rebinding protection with allowed-domains, DNSSEC validation (trust anchors, NSEC3 iteration cap, upstream query budget, clock skew), DNS64, SUDN (RFC 6761/6762), EDE codes, `fqdnOnly`, `filtering.queryTypes` (also strips `ipv6hint` from HTTPS/SVCB), hosts-file resolver with same loading pipeline, PROXY protocol, `IP_FREEBIND` for VIPs (relevant to keepalived), HTTP/3 DoH.

---

## 3. AdGuard Home (GPL-3.0) in depth

### 3.1 Architecture

AGH = `dnsproxy` (listeners, upstreams, cache, DNS64, rDNS, ratelimit) + `internal/filtering` (urlfilter engines, rewrites, safe search, safe browsing/parental, blocked services) + `internal/client` (persistent/runtime clients) + `internal/querylog` + `internal/stats` + `internal/home` (config, web UI, API) + DHCP. The HTTP request handler wraps dnsproxy: `ratelimitMw.Wrap(logMw.Wrap(s.Wrap(s)))`.

Request pipeline (from `AGHTechDoc.md` "Filtering" and `dnsforward/filter.go`): client settings resolved (`clientRequestFilteringSettings`) → `CheckHost` runs, in order, the `hostCheckers`:

```go
d.hostCheckers = []hostChecker{
    {d.matchSysHosts, "hosts container"},        // /etc/hosts-style storage
    {d.matchHost, "filtering"},                  // allow engine, then block engine (+ $dnsrewrite)
    {d.matchBlockedServicesRules, "blocked services"},
    {d.checkSafeBrowsing, "safe browsing"},
    {d.checkParental, "parental"},
    {d.checkSafeSearch, "safe search"},
}
```

Legacy `rewrites` run before all checkers. First `Matched()` reason wins. After the upstream answer, `filterDNSResponse` checks each CNAME target, A/AAAA IP and HTTPS `ipv4hint/ipv6hint` against the **block** rules (allow rules ignored on the response side, per the tech doc).

### 3.2 Config format

Single `AdGuardHome.yaml`, `schema_version: 34` (`configmigrate.LastSchemaVersion`), migrated at startup by `internal/configmigrate/v1..v34.go`. Top-level keys: `http`, `users`, `dns`, `tls`, `querylog`, `statistics`, `filters`, `whitelist_filters`, `user_rules`, `dhcp`, `filtering`, `clients`, `log`, `os`. The file is primarily **written by the server**; the UI/REST API (`openapi/openapi.yaml`, ~80 paths under `/control/`) is the main mutation path. SIGHUP reloads "stored entities" (`internal/home/signal.go`); there is no file-watcher. **No Prometheus endpoint** in the codebase (zero `prometheus` imports); third-party exporters scrape `/control/stats`.

Key `filtering:` fields (from `filtering.Config` YAML tags): `blocking_mode` (`default|null_ip|custom_ip|nxdomain|refused`), `blocking_ipv4/6`, `blocked_response_ttl` (default 3600), `filters_update_interval` (hours, 0 = off), `protection_enabled`, `protection_disabled_until`, `filtering_enabled`, `rewrites_enabled`, `rewrites: [{domain, answer, enabled}]`, `blocked_services: {schedule, ids}`, `safe_search: {enabled, bing, duckduckgo, ecosia, google, pixabay, yandex, youtube}`, `safebrowsing_enabled`, `parental_enabled`, `safebrowsing_block_host` (default `standard-block.dns.adguard.com`), `parental_block_host` (`family-block.dns.adguard.com`), `safe_fs_patterns` (which local files may be used as lists), `max_http_size`, `*_cache_size`, `cache_time`.

Key `dns:` fields (`dnsforward.Config`): `upstream_dns` (+ `upstream_dns_file`), `bootstrap_dns`, `fallback_dns`, `upstream_mode` (`load_balance|parallel|fastest_addr`), `fastest_timeout`, `allowed_clients`/`disallowed_clients`/`blocked_hosts` (access control), `trusted_proxies`, `cache_enabled`, `cache_size` (bytes), `cache_ttl_min/max`, `cache_optimistic` (+ `cache_optimistic_answer_ttl`, `cache_optimistic_max_age`), `bogus_nxdomain`, `aaaa_disabled`, `enable_dnssec` (sets DO bit; AGH does **not** validate), `edns_client_subnet: {enabled, use_custom, custom_ip}`, `ratelimit` (+ subnet lens, whitelist), `refuse_any`, `handle_ddr`, `ipset`, `use_private_ptr_resolvers`/`local_ptr_upstreams`, `use_dns64`/`dns64_prefixes`, `serve_http3`, `use_http3_upstreams`, `pending_requests_enabled`, `max_goroutines`.

### 3.3 Filter lists: model, download, refresh

```go
type FilterYAML struct {
    Enabled bool; URL string /* URL or abs path */; Name string
    RulesCount int `yaml:"-"`; LastUpdated time.Time `yaml:"-"`
    checksum uint32; white bool
    Filter `yaml:",inline"` // ID rules.ListID (assigned from a time-based idGenerator)
}
```

- Each list is stored on disk as `data/filters/<ID>.txt`; `rulelist.Parser` streams the HTTP body (limited by `max_http_size`) line by line into a temp file, drops comments/blank lines, rejects HTML (`<html`/`<!doctype` first line) and binary bytes, extracts `! Title:`, counts rules, computes **CRC-32** of rule lines. If the checksum is unchanged the file is only `Chtimes`'d; otherwise the temp file is atomically renamed over the old one and the engine is rebuilt.
- Refresh loop (`updatesLoop`) ticks every 5s/1min and updates lists whose `LastUpdated + filters_update_interval` has passed; manual `POST /control/filtering/refresh {whitelist: bool}`. Updates use `TryLock` (skip if already running). If every download fails it is treated as a network error and nothing is reloaded.
- Engine rebuild (`initFiltering`) builds two `RuleStorage`s (block, allow) → two `urlfilter.DNSEngine`s, swaps them under `engineLock`, calls `debug.FreeOSMemory()`. Lists are mmap-free `filterlist.File` (rules parsed lazily from the file on first hit and cached in a `sync.Map`) on Unix; on Windows, bytes in memory.
- `user_rules` (custom rules from the UI) form one additional in-memory list (ID `rulelist.IDUser`... IDs in `rulelist/rulelist.go`).

### 3.4 urlfilter DNS engine (GPL; design notes only)

`DNSEngine` = `ruleIndex map[hostname][]StorageID` for **host rules** (hosts-file and domains-only lines; exact hostname lookup) + a `NetworkEngine` for ABP-style rules restricted to host-level rules (`IsHostLevelNetworkRule`: no `$domain`, no request-type restrictions, only `$important`/`$badfilter`... plus DNS modifiers that are allowed). The network engine has three indexes:

1. **Shortcut index**: from each rule take the longest literal substring (`Shortcut`), slide a 5-byte window over it, store the rule under the window with the fewest existing rules; matching takes every 5-byte substring of the lower-cased request URL (`http://hostname`) and unions candidate IDs. Candidates are then verified by `NetworkRule.Match` (pattern compiled to a Go regexp on first use via `sync.Once`; `||`, `^`, `|`, `*` translated).
2. **Domain index**: rules with `$domain=` keyed by permitted domains (unused for DNS since `$domain` rules are not host-level).
3. **noIndex**: everything else (e.g., regex rules, very short shortcuts) – linear scan.

Matching returns **all** matching network rules; `GetDNSBasicRule` removes `$badfilter`-negated and `$dnsrewrite` rules and picks the highest priority: `@@...$important` > `$important` > `@@` whitelist > specific-over-generic > more modifiers. `DNSResult.DNSRewrites()` applies `@@...$dnsrewrite` exceptions. Host-rule results are split into `HostRulesV4`/`HostRulesV6` so the response IP comes from the matching family.

Modifiers accepted by the parser (`rules/network.go` handler map): `important`, `badfilter`, `dnstype`, `dnsrewrite`, `domain`, `denyallow`, `ctag`, `client`, `respgeo` (+ browser-only ones). Unknown modifier → rule rejected (so browser lists are safe). `$client` accepts names, IPs, CIDRs with `~` negation; `$ctag` uses a fixed vocabulary (`device_*`, `os_*`, `user_admin|child|regular`).

`$dnsrewrite` grammar: shorthand `IP` | `hostname` (CNAME) | `NOERROR|SERVFAIL|NXDOMAIN|REFUSED`; full `RCODE;RRTYPE;VALUE` with RRTYPE ∈ A, AAAA, CNAME, PTR, TXT, MX (`pref exchange`), SRV (`prio weight port target`), HTTPS/SVCB (`prio target params`). CNAME has highest priority; non-NOERROR rcode short-circuits; multiple NOERROR rewrites accumulate per RRTYPE.

### 3.5 Rule syntax (from the AdGuard KB, public spec)

```
||example.org^                       block domain + subdomains
@@||example.org^                     exception
example.org                          domains-only: exact
0.0.0.0 example.org                  hosts style (AGH answers with that IP in blocking_mode=default)
/REGEX/                              regex
||example.org^$client=~Mom|~Dad|Kids  per-client (names quoted if spaces; IP/CIDR allowed)
||example.org^$ctag=device_pc|device_phone
||example.org^$dnstype=~A|~CNAME
*$denyallow=com|net                  block all except
||example.com^$dnsrewrite=NOERROR;A;1.2.3.4
||example.org^$important             beats @@ without $important
||example.com$badfilter              disables the identically-written rule
||*^$respgeo=US                      AdGuard DNS only (GeoIP of answer)
```

### 3.6 Allowlist precedence in AGH

Two engines: the **allow engine** (built from `whitelist_filters`) is consulted first; any hit → `NotFilteredAllowList`, request forwarded, and response-side checks are skipped. Inside the **block engine**, `@@` exception rules compete with block rules by urlfilter priority (`$important` can override `@@`). `$dnsrewrite` results are checked before deciding blocked/allowed. Protection can be paused globally (`protection_disabled_until`) or disabled per client.

### 3.7 Blocking response modes (`dnsforward/msg.go`)

| `blocking_mode` | A/AAAA/HTTPS | other qtypes |
|---|---|---|
| `default` | if rule is hosts-style → that IP; else NULL IP (`0.0.0.0`/`::`) | NODATA |
| `null_ip` | `0.0.0.0`/`::` | empty NOERROR |
| `custom_ip` | `blocking_ipv4`/`blocking_ipv6` | NODATA |
| `nxdomain` | NXDOMAIN | NODATA |
| `refused` | REFUSED | NODATA |

Safe-browsing/parental hits answer with the configured block host (IP or name resolved through the proxy); safe search answers CNAME + resolved IPs. TTL = `blocked_response_ttl`.

### 3.8 Per-client model (`internal/client`)

```go
type Persistent struct {
    Name string; Tags []string; Upstreams []string
    IPs []netip.Addr; Subnets []netip.Prefix; MACs []net.HardwareAddr; ClientIDs []ClientID
    UID UID /* uuidv7 */
    UpstreamsCacheSize uint32; UpstreamsCacheEnabled bool
    UseOwnSettings, FilteringEnabled, SafeBrowsingEnabled, ParentalEnabled bool
    UseOwnBlockedServices bool; BlockedServices *filtering.BlockedServices /* {Schedule *schedule.Weekly, IDs []string} */
    IgnoreQueryLog, IgnoreStatistics bool
    SafeSearchConf filtering.SafeSearchConfig; SafeSearch filtering.SafeSearch /* per-client engine */
}
```

- Identification order (`ApplyClientFiltering`): ClientID (from DoH path `/dns-query/{ClientID}` or DoT/DoQ SNI `<clientid>.<server_name>`) → IP → subnet (sorted most-specific first) → MAC via DHCP lease. Runtime (unconfigured) clients get names from WHOIS/ARP/rDNS/DHCP/hosts (`runtime_sources` toggles) for display only.
- Per-client overrides: filtering/safe-browsing/parental/safe-search on-off (`use_global_settings: false`), own blocked-services set with weekly schedule, own upstreams (with an isolated per-client cache), tags for `$ctag`, exclusion from query log/stats. **No per-client list selection** – list choice is global; per-client differences are expressed with `$client`/`$ctag` modifiers inside rules.
- YAML: `clients.persistent[]` objects with the same fields (`ids`, `tags`, `upstreams`, `use_global_settings`, ...) and `clients.runtime_sources: {whois, arp, rdns, dhcp, hosts}`.

### 3.9 Blocked services, safe search, safe browsing / parental

- **Blocked services**: generated table (`servicelist.go`) of ~hundreds of services, each `{id, name, icon_svg, rules: ["||4chan.org^", ...], group_id}`; selected IDs expand to network rules matched per request; weekly schedule (`schedule.Weekly`, per-weekday `start/end` in a configurable timezone) pauses blocking inside the window. Global + per-client.
- **Safe search**: per provider rule files of `$dnsrewrite=NOERROR;CNAME;forcesafesearch.google.com` style rules (Bing, DuckDuckGo, Ecosia, Google, Pixabay, Yandex, YouTube), compiled into a dedicated `DNSEngine`; the CNAME target is resolved and IPs cached (LRU, `safesearch_cache_size`, TTL `cache_time`).
- **Safe browsing / parental**: hash-prefix lookup against AdGuard's service: SHA-256 of host + up to 3 parent labels (stopping at the public suffix), 2-byte prefixes hex-encoded as labels of a TXT query `<prefix>.<prefix>.sb.dns.adguard.com.` (or `pc.dns.adguard.com.`) over DoH `https://family.adguard-dns.com/dns-query`; TXT answers contain full hashes; results cached. Network-dependent and vendor-hosted.

### 3.10 Rewrites

Legacy `rewrites: [{domain, answer, enabled}]` where `domain` may be `*.example.com` and `answer` an IP, a CNAME target, or the special `A`/`AAAA` (meaning "exception: forward this type upstream"). Internally converted to `$dnsrewrite` rules (`rewrite.Item.toRule`). CNAME loops are detected. API `/control/rewrite/{list,add,delete,update,settings}`.

### 3.11 Upstreams (via dnsproxy)

- Address forms: `1.2.3.4[:53]`, `udp://`, `tcp://`, `tls://` (DoT), `https://host/dns-query` (DoH, H1.1/H2; `h3://` forces HTTP/3; `use_http3_upstreams` races it), `quic://` (DoQ), `sdns://` stamps (incl. **DNSCrypt**).
- dnsmasq-like domain routing: `[/example.local/]192.168.0.1`, `[//]...` for unqualified names, `[/*.host.com/]` subdomains only, `[/sub.host.com/]#` fall back to defaults; more specific wins. Parsed by `proxy.ParseUpstreamsConfig` into `UpstreamConfig{Upstreams, DomainReservedUpstreams, SpecifiedDomainUpstreams, SubdomainExclusions}`.
- Modes: `load_balance` (weighted random by `1/avgRTT`, tries next on error), `parallel` (all upstreams, first success), `fastest_addr` (A/AAAA only: query all, probe returned IPs by ICMP/TCP and return the fastest). `fallback_dns` used when all upstreams fail. Bootstrap servers resolve upstream hostnames (`bootstrap_prefer_ipv6`). Private rDNS upstreams for RFC 6303 ranges.
- `test_upstream_dns` API validates upstream lists before saving.

### 3.12 Caching (dnsproxy `proxy/cache.go`)

- Byte-bounded LRU (`cache_size`, default 64 KiB in dnsproxy; AGH default larger via UI) keyed by packed question (+ ECS subnet in a second cache when ECS is enabled). Entry = `expireUnix(4) | len(2) | packedMsg | upstreamAddr`. Min/max TTL clamp (`cache_ttl_min` capped at 3600 in dnsproxy CLI), OPT/DNSSEC RRs filtered on the way out depending on DO bit.
- **Optimistic cache**: serve expired entries (with `optimistic_answer_ttl`, default 30s) for up to `optimistic_max_age` (12h) while refreshing in the background exactly once per key (`optimisticResolver` single-flight).
- **Pending requests** (`pending_requests_enabled`): identical in-flight queries are coalesced (prevents cache-poisoning amplification).
- No shared/distributed cache; per-client caches when a client has own upstreams.

### 3.13 Query log and statistics

- Query log: in-memory ring buffer (`size_memory` entries) flushed to `querylog.json` (one JSON object per line: `T, QH, QT, QC, ECS, CID, CP (doh/doq/dot/dnscrypt/""), Upstream, Answer (packed bytes), OrigAnswer, IP, Result{Reason, IsFiltered, Rules[], ServiceName, CanonName, IPList, DNSRewriteResult}, Elapsed, Cached, AD`), rotated by `interval` (6h/1d/7d/30d/90d) with gzip of old file, searchable via `GET /control/querylog?older_than&search&response_status&limit&offset`, `ignored` domains, client IP anonymization (`anonymize_client_ip`). Per-client `ignore_querylog`.
- Statistics: `bbolt` database with hourly units; retention 1/7/30/90 days (`interval`); API returns totals, top queried/blocked domains, top clients, top upstreams with avg response time, per-day series. Per-client `ignore_statistics`.

### 3.14 Hot reload

Everything is mutated through the API and applied in-process (filter engine rebuilt async: "old filters continue working until new ones are ready"); the YAML is rewritten by the server (`renameio` atomic). SIGHUP triggers reload of the stored entities; editing YAML by hand requires a restart otherwise.

---

## 4. dnsproxy (Apache-2.0) as a reusable library

Importable packages: `upstream` (all client protocols: plain UDP/TCP, DoT, DoH 1.1/2/3, DoQ, DNSCrypt, DNS stamps, bootstrap resolver, `ExchangeParallel/ExchangeAll`, `Options{Bootstrap, Timeout, VerifyServerCertificate, HTTPVersions, PreferIPv6, InsecureSkipVerify}`), `proxy` (listeners for UDP/TCP/DoT/DoH(+H3)/DoQ/DNSCrypt, `Middleware`/`Handler` chain, cache with optimistic mode, pending-request coalescing, DNS64, bogus-NXDOMAIN, ECS, private rDNS routing, `UpstreamMode`, `MessageConstructor` hook to shape NXDOMAIN/REFUSED/SERVFAIL), `ratelimit` (per-subnet token bucket middleware), `proxyutil`. Depends on `quic-go`, `golibs` (Unlicense), `dnscrypt` (Unlicense), `gonum` (weighted sampling). Config example `config.dist.yaml`; CLI mirrors `proxy.Config`. No Prometheus, no persistence.

---

## 5. Side-by-side comparison

| Capability | blocky | AdGuard Home |
|---|---|---|
| License | Apache-2.0 | GPL-3.0 (+ urlfilter GPL-3.0) |
| List formats | hosts, domains, `*.wildcard`, `/regex/`, ABP `\|\|x^` trimmed to exact | hosts, domains, full ABP-DNS grammar with modifiers, regex |
| Exceptions in lists | no (`allowlists:` only) | `@@` rules, `$badfilter`, `$important`, allow engine |
| Per-client policy | group→lists mapping by name/IP/CIDR/FQDN/glob; schedules per group; per-client upstream groups | global lists; per-client toggles + tags + own upstreams + blocked services + schedule; `$client`/`$ctag` in rules |
| Matching structure | length-bucketed sorted strings (binary search) + TLD-bucketed wildcard w/ bit filter + regex scan; copy-on-write per group | hostname→rule-ID map + 5-byte shortcut index + regexp verify; lazy rule materialisation from files |
| Response inspection | CNAME + A/AAAA against allow/deny | CNAME + A/AAAA + HTTPS hints against block rules |
| Block modes | zeroIp, nxDomain, refused, custom IPs; TTL 6h, SOA on NXDOMAIN | default, null_ip, custom_ip, nxdomain, refused; TTL 3600 |
| Rewrites / local records | customDNS mapping + zone file + rewrite; conditional forwarding w/ fallback | legacy rewrites (wildcard, A/AAAA exception) + `$dnsrewrite` (many RR types) |
| Upstream protocols | UDP/TCP, DoT, DoH(H2/H3 server side), DoQ, sdns (no DNSCrypt), cert pinning | UDP/TCP, DoT, DoH(H1/2/3), DoQ, DNSCrypt, sdns |
| Upstream strategies | parallel_best (race 2, error-weighted), random, strict | load_balance (RTT-weighted), parallel, fastest_addr, fallback list |
| Conditional by domain | `conditional.mapping` (+ rewrite/fallback) | `[/domain/]upstream` dnsmasq syntax, `#` and `*.` forms |
| Cache | TTL clamp, negative, prefetch (threshold/window), exclude regex, max items, **Redis replicated** | byte-bounded LRU, TTL clamp, optimistic serve-stale, pending coalescing, per-client caches |
| ECS | add/forward/strip; use-as-client | enable + custom IP; separate ECS cache |
| DNSSEC | full validation | DO bit only |
| Query log sinks | csv, csv-client, mysql, postgres, timescale, sqlite, dnstap, console | JSON-lines file + in-memory ring, searchable API |
| Stats | 24h in-memory JSON + Prometheus | bbolt hourly, 1-90 days, API |
| Prometheus | yes (rich) | no |
| Config | YAML (+ dir merge, schema, migrations, `file:` secrets); API minimal | YAML written by server (schema_version 34); rich REST API + UI; SIGHUP |
| Hot reload | lists only (timer/API) | API-driven, engine swap async |
| Rate limiting | token bucket per client, silent drop | dnsproxy ratelimit (UDP only), per subnet |
| Clustering | Redis cache + blocking-state sync | none |

### 5.1 Performance (what is actually published)

- Neither project publishes throughput or memory numbers in-repo; both ship Go benchmarks (blocky: `cache/stringcache/*_benchmark_test.go`, `resolver/*_bench_test.go`; urlfilter: `BenchmarkDNSEngine_Match`, `BenchmarkShortcutIndex_AppendMatching_baseFilter`, `BenchmarkDNSEngine_heapAlloc`). A web search (2026-10-10) found no credible third-party benchmark of either; the "Two ceilings" dev.to posts are about an unrelated Go server. **All performance claims are UNVERIFIED.** Design-level expectations: blocky's string cache is a few bytes per entry (no per-entry map overhead), O(log n) lookups; urlfilter's host index is a Go map (higher per-entry cost) and ABP rules add shortcut-index candidates + regexp verification per candidate.

---

## 6. Recommended blocking-engine design for kudurru

### 6.1 Policy model (data model)

```
Source      {id, name, url|path|inline, format: auto|hosts|domains|adblock, refresh, lastFetch, etag, checksum, ruleCount, enabled}
ListGroup   {id, name, deny: [Source], allow: [Source], schedules: [Schedule]}   # blocky "group"
Policy      {id, name, groups: [ListGroup], allowOnly: bool, blockMode, blockTTL,
             safeSearch: {...}, blockedServices: {ids, schedule}, upstreamGroup, queryLog: bool, stats: bool}
Client      {id, name, ids: [ip|cidr|mac|clientID|fqdn|nameGlob], tags: [], policy: Policy, upstreams?: [...]}
Default policy applies to unmatched clients.
```

Combine blocky's **group → client mapping** (the thing people love: "kids group gets adult+ads lists") with AGH's **persistent client object** (name, multiple identifier kinds, tags, per-client toggles, exclusions). Store these as CRDs in Kubernetes (`ListGroup`, `Policy`, `Client`) and as YAML/API objects in single-node mode; the same structs serialise both ways.

Precedence (match blocky, which is explicit and testable): allow (any group of the client) > allow-only block > deny > response-side CNAME/IP deny. Add AGH's `$important` as a rule-level escape hatch within adblock lists, and `@@` exceptions inside lists (treat as allow entries scoped to that list's group).

### 6.2 Data structures

- **Exact/hosts/domains**: blocky's length-bucketed sorted-string set (copy it; Apache-2.0). Keep per group; publish via `atomic.Pointer` snapshot swap.
- **Suffix (`||x^` and `*.x`)**: implement `||` with true subdomain semantics (unlike blocky) using blocky's TLD-bucketed wildcard cache with the bit filter; it already walks parents broadest-first. Alternatively a reversed-label radix tree; blocky's version has lower memory and is tested.
- **Adblock rules with modifiers**: parse the public grammar (`||`, `|`, `^`, `*`, `/re/`, `@@`, `$important`, `$badfilter`, `$dnstype`, `$dnsrewrite`, `$client`, `$ctag`, `$denyallow`) in a clean-room parser. Index: rules whose pattern reduces to a pure suffix go into the suffix structure (most lists are 95%+ `||domain^`); only rules with wildcards in the middle, regex, or modifiers go to a "slow" bucket. For the slow bucket, use urlfilter's idea (5-byte shortcut → candidate IDs → verify) but write it yourself; keep it small.
- **Regex**: linear scan, warn above a threshold (blocky warns at 500).
- **Response-side**: IPs and CNAMEs through the same groups (blocky), plus HTTPS/SVCB hints (AGH).
- Return `matches map[group]rule` so logs/UI show the rule and list that fired (both projects do; Pi-hole-style UI needs it).

### 6.3 List pipeline

Copy blocky's pipeline wholesale: per-URL conditional GET with ETag/Last-Modified, `sha256(url)` disk cache, stale-on-unreachable, tee-to-temp + atomic rename, retry with cooldown, per-source error budget, group-level "keep old on failure", bounded concurrency, init strategies (`blocking|failOnError|fast` with disk seeding). Add AGH's touches: CRC-32 of rule lines to skip engine rebuilds when content is unchanged, HTML/binary detection, `! Title:` extraction, `max_http_size`, `safe_fs_patterns` for local files, per-list `enabled`, rule counts and last-updated exposed in the API. In Kubernetes, run the fetcher on one elected leader and distribute compiled snapshots (or rely on every replica fetching with the shared disk cache on a PVC — decide in the architecture doc).

### 6.4 Block responses, TTL, EDE

Support `zeroIP` (default), `nxDomain` (with SOA, RFC 2308), `refused`, `customIP` (v4+v6), and AGH's `default` (use the hosts-file IP when the rule carried one). Make TTL configurable per policy (blocky 6h vs AGH 1h defaults – choose something like 10m for responsiveness in a UI-driven product). Emit EDE 15 (Blocked) / 17 (Filtered) like blocky.

### 6.5 Upstreams, cache, logging

- Import **`github.com/AdguardTeam/dnsproxy/upstream`** (Apache-2.0) for all client protocols incl. DNSCrypt/H3/stamps/bootstrap, rather than reimplementing (blocky's upstream code is also Apache-2.0 but lacks DNSCrypt). Implement strategies ourselves: blocky's `parallel_best`/`random`/`strict` plus dnsproxy's RTT-weighted `load_balance`; `fastest_addr` is niche.
- Conditional forwarding: blocky's map + longest-suffix walk, and accept the dnsmasq `[/domain/]` string form for import convenience.
- Cache: blocky's design (TTL clamp, negative TTL, prefetch with threshold/window, exclude regex) + dnsproxy's optimistic serve-stale and in-flight coalescing. For multi-replica k8s, blocky's Redis write-through + pub/sub decorator is a proven pattern; kudurru can swap Redis for its own replication layer (the decorator interface `ExpiringCache[T]` + `ReloadPublishable` is clean).
- Query log: blocky's writer interface and sinks (csv/sqlite/postgres/timescale/mysql/dnstap) are directly reusable; add AGH-style searchable API over the local store for the dashboard. Carry `BlockyInstance`-style instance ID for clustered logs.
- Metrics: adopt blocky's metric names/labels as the baseline (bounded cardinality: group names, response types; never per-domain labels).

### 6.6 Library decisions

| Need | Decision |
|---|---|
| Adblock-DNS rule engine | **Write clean-room** (urlfilter is GPL-3.0). Grammar from the AdGuard KB. |
| Exact/suffix matchers, list loader, parsers, caching resolver, Redis decorator, query-log writers, metrics | **Copy/adapt from blocky** (Apache-2.0, attribute in NOTICE). |
| Upstream clients (DoH/DoT/DoQ/DNSCrypt/stamps/bootstrap) | **Import dnsproxy/upstream** (Apache-2.0). Consider `dnsproxy/proxy` listeners too, but its `Handler` model is less composable than blocky's chain; evaluate. |
| DNS wire | miekg/dns (BSD-3). |
| Expiring cache | `0xERR0R/expiration-cache` is MPL-2.0 – acceptable as a dependency, or replace with an internal sharded TTL cache. |
| Safe browsing/parental | Do **not** depend on AdGuard's hash-prefix service (vendor-hosted, GPL client code). If wanted, implement the open protocol against a self-hosted list, or treat it as a pluggable "threat intel" integration. |
| Blocked services catalogue | Do not copy AGH's generated table (GPL repo). Build kudurru's own catalogue (`service → [suffixes]`) in a data repo with a permissive license. |

---

## 7. What kudurru should borrow

1. blocky's resolver **chain architecture** and its exact ordering rationale (blocking and DNSSEC above the cache; typed responses; local answers never cached).
2. blocky's **ListGroup → client mapping** with `default`, glob names, CIDRs, FQDN identifiers, schedules per group, allow-only mode, and the allow-wins-across-groups rule.
3. blocky's **string/wildcard caches** (length-bucketed sorted strings + TLD-bucketed wildcard with bit filter, copy-on-write snapshots, matched-rule reporting).
4. blocky's **download pipeline** (conditional GET, disk cache with stale fallback, atomic rename, error budgets, init strategies, disk seeding) and AGH's checksum-based rebuild skipping + HTML/binary guards.
5. blocky's **Redis write-through + pub/sub** cache decorator and blocking-state bridge as the template for replica synchronisation.
6. blocky's **caching semantics** (min/max/negative/prefetch/exclude) + dnsproxy's **optimistic cache and pending-request coalescing**.
7. blocky's **query-log writer abstraction and sinks** (csv, sqlite WAL, postgres/timescale/mysql, dnstap) and its **Prometheus metric set** with bounded labels.
8. AGH's **persistent client object** (multiple identifier kinds incl. DoH-path/DoT-SNI ClientID and MAC via DHCP, tags, per-client toggles, exclusion from logs/stats, own upstreams) and its **runtime client discovery** sources.
9. AGH's **adblock-DNS grammar** (`||`, `@@`, `$important`, `$badfilter`, `$dnstype`, `$dnsrewrite`, `$client`, `$ctag`, `$denyallow`) so users can paste any AdGuard/Hagezi/OISD list unchanged – re-implemented clean-room.
10. AGH's **blocking-mode `default`** (honour hosts-file IP), its **blocked-services with weekly schedule** concept, its **rewrites API** shape, and its **searchable query-log API**.
11. AGH's **config schema versioning + migrations**, blocky's **JSON schema for YAML** and `file:` secret references.
12. dnsproxy's **upstream package** and `[/domain/]upstream` string syntax as an import format.
13. blocky's `freeBind` listener option (needed with keepalived VIPs) and PROXY-protocol support (needed behind dnsdist).

## 8. What kudurru should avoid

1. **Importing urlfilter or any AdGuard Home package** – GPL-3.0 would relicense the project.
2. blocky's treatment of `||domain^` as an exact match (silently changes list semantics) and its lack of in-list `@@` exceptions.
3. AGH's global-only list selection (no per-client list groups) and its config file being owned by the server (hand edits lost; not GitOps-friendly). kudurru should make the API and the declarative config (CRDs/YAML) the same source of truth with a clear precedence.
4. AGH's dependency on AdGuard-hosted safe-browsing/parental services and vendor-generated data tables.
5. AGH's absence of Prometheus and its bbolt stats store (single-node, not clusterable); blocky's absence of list CRUD/auth in the API.
6. Unbounded Prometheus labels (per-domain, per-client-IP) – both projects deliberately avoid them; keep it that way.
7. blocky's REFUSED block mode as a default (stub resolvers fail over to another server).
8. Rebuilding the whole engine on every refresh when content is unchanged (AGH's CRC-32 check solves this; blocky rebuilds per group every period).
9. Regex-heavy lists in the hot path without a warning/limit.
10. dnsproxy's `fastest_addr` probing (ICMP/TCP probes from the resolver) – surprising in corporate networks; keep optional at most.

## 9. Open questions

- Should compiled list snapshots be replicated between k8s replicas (leader fetches, followers receive) or should each replica fetch with a shared PVC disk cache? Affects memory and egress.
- Where does the clean-room adblock parser draw the line on modifiers: implement `$client`/`$ctag` (overlaps with kudurru's policy model) or map them onto Policies at import time?
- Query-log store for the dashboard: sqlite per instance + aggregation, or Postgres/Timescale as the only clustered option (blocky supports both)?
- License of `AdguardTeam/HostlistsRegistry` (source of AGH's blocked-services data) – UNVERIFIED; if permissive, it could seed kudurru's service catalogue.
- Is `expiration-cache` (MPL-2.0) acceptable in the dependency policy, or write an internal TTL cache?

## 10. File references (clones under /tmp/kudurru-research/blocking-engines/)

- blocky: `docs/config.yml`, `docs/configuration.md`, `docs/interfaces.md`, `docs/api/openapi.yaml`, `resolver/blocking_resolver.go`, `cache/stringcache/{string_caches,wildcard_cache,in_memory_grouped_cache,chained_grouped_cache}.go`, `lists/{list_cache,downloader,download_cache,sourcereader}.go`, `lists/parsers/{hosts,lines}.go`, `resolver/{caching_resolver,parallel_best_resolver,strict_resolver,upstream_resolver,bootstrap,ecs_resolver,custom_dns_resolver,conditional_upstream_resolver}.go`, `cache/redis.go`, `redis/event_bridge.go`, `querylog/*.go`, `metrics/*.go`, `server/server.go`.
- AdGuard Home: `internal/filtering/{filtering,filter,dnsrewrite,rewrites,blocked,reason,result}.go`, `internal/filtering/rulelist/parser.go`, `internal/filtering/safesearch/safesearch.go`, `internal/filtering/hashprefix/hashprefix.go`, `internal/client/{persistent,storage,index,client}.go`, `internal/dnsforward/{config,filter,msg,clientid}.go`, `internal/querylog/{entry,qlog}.go`, `internal/stats/stats.go`, `internal/home/{config,clients}.go`, `internal/configmigrate/configmigrate.go`, `openapi/openapi.yaml`, `AGHTechDoc.md`.
- urlfilter: `dnsengine.go`, `networkengine.go`, `dnsresult.go`, `rules/{network,host,dnsrewrite,clients,match,request}.go`, `filterlist/{rulestorage,rulescanner}.go`, `internal/lookup/{shortcut,domain}.go`.
- dnsproxy: `README.md`, `config.dist.yaml`, `proxy/{upstreammode,exchange,upstreams,cache,optimisticresolver,pending}.go`, `upstream/{upstream,parallel}.go`, `ratelimit/ratelimit.go`.
- Web: https://adguard-dns.io/kb/general/dns-filtering-syntax/ (rule grammar), GitHub REST API for releases/licenses.
