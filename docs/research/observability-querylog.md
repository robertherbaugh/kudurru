> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [docs/architecture.md](../architecture.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# Observability, query logging and dashboard data for kudurru

Research key: `observability-querylog` · Date: 2026-10-10 · Author: research subagent
Status legend: **[V]** verified from primary source (file path / URL given) · **[U]** UNVERIFIED (memory or secondary source)

## 0. Sources examined

| Project | Clone / URL | Commit / version | Date | License |
|---|---|---|---|---|
| Pi-hole FTL | `/tmp/kudurru-research/observability-querylog/ftl` (github.com/pi-hole/FTL) | `0bf029b` = tag **v6.7.1** | 2026-09-19 | EUPL-1.2 (`LICENSE`) — **do not copy code** |
| AdGuard Home | `.../adguardhome` (github.com/AdguardTeam/AdGuardHome) | `8dedd32`; CHANGELOG head `v0.107.80` (2026-09-03), `v1.0.0` TBA; tag `v1.0.0-b.1` exists | 2026-10-08 | GPL-3.0 (`LICENSE.txt`) — **do not copy code** |
| blocky | `.../blocky` (github.com/0xERR0R/blocky) | `ca450f8` | 2026-10-09 | Apache-2.0 |
| Technitium DNS Server | `.../technitium` (github.com/TechnitiumSoftware/DnsServer) | `70495af`; `<Version>15.6</Version>` in `DnsServerCore.csproj` | 2026-10-03 | GPL-3.0 — **do not copy code** |
| golang-dnstap | `.../golang-dnstap` (github.com/dnstap/golang-dnstap) | `ebb538e` = tag **v0.4.0** | 2021-02-09 | Apache-2.0; `dnstap.pb/dnstap.proto` is CC0 |
| CoreDNS | `.../coredns` (github.com/coredns/coredns) | `123829d`; `CoreVersion = "1.14.7"` (`coremain/version.go`) | 2026-10-09 | Apache-2.0 |
| dnsdist docs | https://dnsdist.org/guides/webserver.html, https://dnsdist.org/statistics.html, https://dnsdist.org/reference/dnstap.html, https://www.dnsdist.org/reference/ottrace.html | 2.1.x / 2.2.0 features noted inline | fetched 2026-10-10 | GPL-2.0 (sibling report) |
| Unbound docs | https://unbound.docs.nlnetlabs.nl/en/latest/manpages/unbound.conf.html, `.../unbound-control.html` | latest | fetched 2026-10-10 | BSD |
| Knot DNS docs | https://www.knot-dns.cz/docs/latest/html/modules.html | latest | fetched 2026-10-10 | GPL-3.0 |
| OpenTelemetry semconv | https://opentelemetry.io/docs/specs/semconv/registry/attributes/dns/, `.../dns/dns-metrics/` | Development stability | fetched 2026-10-10 | — |
| Others | SQLite WAL (https://www.sqlite.org/wal.html), Loki push API, ClickHouse MergeTree, Prometheus naming, Kubernetes probes, pihole6_exporter, DNS-collector, Pi-hole privacy-level docs | — | fetched 2026-10-10 | — |

Sibling reports cross-referenced (same directory): `gravity.md` §13, `pihole.md` §10, `technitium.md` §12, `dnsdist-keepalived.md` §1.4/§1.12. This report does not repeat their content except where needed for the pipeline design.

---

## 1. Query logging: how the reference projects do it

### 1.1 Pi-hole FTL (v6.7.1) — shared-memory ring + in-memory SQLite + on-disk SQLite [V]

**Hot path data model** (`src/datastructure.h`): one `queriesData` per query, deliberately 64 bytes on every architecture:
`double response, timestamp; sqlite3_int64 db; unsigned domainID, clientID; int upstreamID(-1 = not forwarded), cacheID, id, CNAME_domainID, ede; uint16 qtype; enum status/type/privacylevel/reply/dnssec; bitfields allowed/complete/blocked/response_calculated/upstream_counted + database.changed/imported`. Strings (domain, client IP/name, upstream IP) live in a shared string pool addressed by offsets; separate `domainsData`, `clientsData` (with per-client `overTime[OVERTIME_SLOTS]` counters), `upstreamsData` (count/failed/responses/rtime) arrays. Everything lives in POSIX shared memory (`src/shmem.c`) so the API process (`pihole-FTL` + CivetWeb) and the dnsmasq threads share it.

**Time buckets** (`src/FTL.h`, `src/overTime.h`): `MAXLOGAGE 24` h, `OVERTIME_INTERVAL 600` s, `OVERTIME_SLOTS = 24*3600/600 + 1 = 145` slots of `{total, blocked, cached, forwarded, timestamp}`. The dashboard graph is literally this array (`src/api/history.c`). QPS is a 30-slot ring (`QPS_AVGLEN 30`). The in-memory window is capped by `webserver.api.maxHistory` (default 86400 s, max 24 h).

**Three-tier storage flow** (comment block at `src/database/query-table.c:160-176`):
1. Every **1 s** the DB thread copies changed queries from shared memory into an **in-memory SQLite** (`:memory:`, or `files.tmp_db` when `database.forceDisk`) — `queries_to_database()`. It snapshots changed rows under the SHM lock, upserts dictionary rows (`domain_by_id`, `client_by_id`, `forward_by_id`, `addinfo_by_id`) with `INSERT OR IGNORE` + id lookup, caches `addinfo` ids in a 1024-slot open-addressing hash (Knuth multiplicative hash), then `REPLACE INTO query_storage VALUES (?1..?13)` in one transaction. Rows that fail are re-queued for the next tick.
2. Every `database.DBinterval` (**default 60 s**) `export_queries_to_disk()` runs `INSERT INTO disk.query_storage SELECT * FROM query_storage WHERE timestamp < now-REPLY_TIMEOUT(30 s) AND id > last_exported` plus the four dictionary tables and `sqlite_sequence`, inside one transaction on the **ATTACHed** on-disk DB. The 30 s grace lets replies complete before the row is frozen on disk.
3. Rows older than 24 h are deleted from the memdb (GC); on-disk rows are deleted **once per day between 03:10–03:50** (`DELETE FROM query_storage WHERE timestamp <= now - maxDBdays*86400`); `ANALYZE` weekly (`DATABASE_ANALYZE_INTERVAL 604800`); MAC-vendor refresh monthly.

**Disk DB pragmas** (`query-table.c:247-333`): `PRAGMA disk.journal_mode=WAL` (if `database.useWAL`, default yes), `PRAGMA disk.synchronous=NORMAL` (explicitly justified: "WAL mode is safe from corruption with synchronous=NORMAL"; at worst the last flush interval is lost), `PRAGMA disk.mmap_size = 268435456` (256 MiB ceiling), busy handler with `DATABASE_BUSY_TIMEOUT 1000` ms.

**Schema** (`src/database/query-table.h`, `MEMDB_VERSION 22`; on-disk DB version 22 in `common.c`):
```sql
CREATE TABLE query_storage ( id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp INTEGER NOT NULL,
  type INTEGER NOT NULL, status INTEGER NOT NULL, domain INTEGER NOT NULL, client INTEGER NOT NULL,
  forward INTEGER, additional_info INTEGER, reply_type INTEGER, reply_time REAL, dnssec INTEGER,
  list_id INTEGER, ede INTEGER );
CREATE TABLE domain_by_id  (id INTEGER PRIMARY KEY, domain TEXT NOT NULL);
CREATE TABLE client_by_id  (id INTEGER PRIMARY KEY, ip TEXT NOT NULL, name TEXT);
CREATE TABLE forward_by_id (id INTEGER PRIMARY KEY, forward TEXT NOT NULL);           -- "ip#port"
CREATE TABLE addinfo_by_id (id INTEGER PRIMARY KEY, type INTEGER NOT NULL, content NOT NULL); -- 1=CNAME domain, 2=list id
CREATE UNIQUE INDEX domain_by_id_domain_idx ON domain_by_id(domain);
CREATE UNIQUE INDEX client_by_id_client_idx ON client_by_id(ip,name);
CREATE UNIQUE INDEX forward_by_id_forward_idx ON forward_by_id(forward);
CREATE UNIQUE INDEX addinfo_by_id_idx ON addinfo_by_id(type,content);
-- memdb only: an index on EVERY query_storage column (timestamp,type,status,domain,client,forward,
-- additional_info,reply_type,reply_time,dnssec,list_id) + unique id index
CREATE VIEW queries AS SELECT ... CASE typeof(domain) WHEN 'integer' THEN (SELECT domain FROM domain_by_id ...) ... FROM query_storage q;
CREATE TABLE counters ( id INTEGER PRIMARY KEY NOT NULL, value INTEGER NOT NULL );   -- 0 total, 1 blocked (lifetime)
CREATE TABLE ftl ( id INTEGER PRIMARY KEY NOT NULL, value BLOB NOT NULL );           -- metadata incl. last timestamp
```
`type` stores the enum for the 16 common RR types or `100 + qtype` for others. Enums (`src/enums.h`): `query_status` 0–18 (UNKNOWN, GRAVITY, FORWARDED, CACHE, REGEX, DENYLIST, EXTERNAL_BLOCKED_IP/NULL/NXRA, GRAVITY_CNAME, REGEX_CNAME, DENYLIST_CNAME, RETRIED, RETRIED_DNSSEC, IN_PROGRESS, DBBUSY, SPECIAL_DOMAIN, CACHE_STALE, EXTERNAL_BLOCKED_EDE15); `reply_type` 0–13 (UNKNOWN, NODATA, NXDOMAIN, CNAME, IP, DOMAIN, RRNAME, SERVFAIL, REFUSED, NOTIMP, OTHER, DNSSEC, NONE, BLOB); `dnssec_status` (UNKNOWN, SECURE, INSECURE, BOGUS, ABANDONED, TRUNCATED).

**Long-term queries** (`src/api/stats_database.c`) are plain SQL over the fact table: history = `SELECT (timestamp/600)*600 AS interval, status, COUNT(*) ... GROUP BY interval,status`; top domains = `SELECT COUNT(*) cnt, d.domain FROM query_storage q JOIN domain_by_id d ... GROUP BY q.domain ORDER BY cnt DESC LIMIT :count`; clients-over-time = `GROUP BY interval, client`; upstreams = `SELECT forward, COUNT(*) ... GROUP BY forward`. There are **no rollup tables** — every long-term dashboard call scans the fact table between `from`/`until` (the UI warns "a lot slower").

**Privacy levels** (`misc.privacylevel`, `src/config/config.c:1399-1405`; docs https://docs.pi-hole.net/ftldns/privacylevels/) are applied at ingest, per query (`query->privacylevel` stored on each row, `dnsmasq_interface.c:1057`), and are irreversible:

| Level | Stored | UI effect |
|---|---|---|
| 0 | everything | — |
| 1 | domain stored/shown as `hidden` | Top Domains, Top Ads disabled |
| 2 | + client stored as `0.0.0.0` | + Top Clients, Clients-over-time disabled |
| 3 | nothing written to long-term DB; no query log | "almost any statistics" disabled, "lose most regex features" |

Lowering the level **restarts FTL** (`config.c:2173`). Additional filters: `webserver.api.excludeClients` / `excludeDomains` (regex) hide from the API without affecting storage.

**API shapes** (`src/api/docs/content/specs/*.yaml`, served as OpenAPI): `GET /api/stats/summary` → `{queries:{total,blocked,percent_blocked,unique_domains,forwarded,cached,frequency,types:{A,AAAA,...}}, ...}`; `GET /api/history` → `{history:[{timestamp,total,cached,blocked,forwarded}]}` (145 × 10-min slots); `GET /api/history/clients?N=` → `{clients:{ip:{name,total}}, history:[{timestamp, data:{ip:count}}]}` with a synthetic `0.0.0.0 "other clients"` bucket; `GET /api/stats/top_domains?blocked=&count=`, `top_clients`, `query_types`, `upstreams`, `recent_blocked?count=`; every one has a `database_*` twin taking `from`/`until`. `GET /api/queries` filters: `from, until, length (default 100), start, cursor (DB id of newest row → stable pagination under live inserts), domain, client_ip, client_name, upstream, type, status, reply, dnssec, disk=true`; response `{queries:[{id,time,type,domain,cname,status,client:{ip,name},dnssec,reply:{type,time},list_id,upstream,ede:{code,text}}], cursor, recordsTotal, recordsFiltered, draw, earliest_timestamp, earliest_timestamp_disk}`. `GET /api/queries/suggestions` returns distinct values for each filter. **No SSE/WebSocket**: grep for `text/event-stream`/`websocket` in `src/api` and `src/webserver` returns nothing; the web UI polls `/api/queries?cursor=` every 2 s (sibling pihole.md).

### 1.2 AdGuard Home (v0.107.80 / 1.0 beta) — NDJSON file + in-memory ring; hourly bbolt stats [V]

**Query log** (`internal/querylog/`): one `logEntry` per query, JSON-encoded one-per-line into `querylog.json`:
```go
type logEntry struct {
  Time time.Time `json:"T"`; QHost string `json:"QH"`; QType string `json:"QT"`; QClass string `json:"QC"`
  ReqECS string `json:"ECS,omitempty"`; ClientID string `json:"CID,omitempty"`; ClientProto ClientProto `json:"CP"` // "doh","dot","doq","dnscrypt",""
  Upstream string `json:",omitempty"`; Answer []byte `json:",omitempty"`; OrigAnswer []byte `json:",omitempty"` // packed wire-format
  IP net.IP `json:"IP"`; Result filtering.Result; Elapsed time.Duration; Cached bool `json:",omitempty"`; AuthenticatedData bool `json:"AD,omitempty"`
}
```
- Ring buffer `container.RingBuffer[*logEntry]` of `size_memory` entries (**default 1000**, `internal/home/config.go:331`); when full and `FileEnabled`, a goroutine JSON-encodes the whole buffer and appends it to the file (`querylogfile.go: flushLogBuffer`). On shutdown the buffer is flushed.
- Rotation: a single `querylog.json` + `querylog.json.1`; an hourly ticker reads the first record's `"T"` and renames when `oldest + interval <= now` (`checkAndRotate`). Allowed intervals: 6 h, 1 d, 7 d, 30 d, 90 d (`checkInterval`), any 1 h–365 d via the newer config API (`validateIvl`). Retention therefore is "≤ 2 × interval" and is **not size-bounded**.
- Search (`search.go`, `qlogfile.go`): memory buffer (reverse order) + files `.1` then current; **binary search by timestamp inside the file** (`seekTS`, max depth 100, probes of up to 16 KiB lines, 1.6 MiB read buffer), `quickMatch` does substring checks on the raw JSON line before full decode. Limits: default `limit 500`, `maxFileScanEntries 50000`. Params: `older_than`, `offset`, `limit`, `search` (domain / client IP / client name / ClientID, strict with quotes), `response_status` (all/filtered/blocked/blocked_services/blocked_safebrowsing/blocked_parental/whitelisted/rewritten/safe_search/processed) or `reason`.
- Output item (`json.go: entryToJSON`, OpenAPI `QueryLogItem`): `reason, elapsedMs (string), time (RFC3339Nano), client, client_proto, cached, upstream, question{type,class,name,unicode_name}, rules[{text,filter_list_id}], answer[{type,value,ttl}], original_answer, answer_dnssec, client_id, client_info{name,whois,disallowed...}, ecs, service_name, status (rcode string)`.
- Privacy: `anonymize_client_ip` zeroes the last 2 bytes of IPv4 / last 10 bytes of IPv6 (`AnonymizeIP`, `querylog/http.go:153`) — for the query log it is applied **at API output time only** (`entriesToJSON`; `querylog.json` on disk keeps the raw IP, and `client_info` is omitted when anonymized), whereas the stats module applies the same function **at ingest** (`dnsforward/stats.go:33`); `ignored` hostnames (exact, `.` = root) and per-client `IgnoreQueryLog` skip logging entirely. `POST /control/querylog_clear` deletes both files.

**Stats** (`internal/stats/`): a `unit` per **hour** (`id = unixSeconds/3600`) holding `nTotal, timeSum(µs), nResult[5] (NotFiltered, Filtered, SafeBrowsing, SafeSearch, Parental), domains{}, blockedDomains{}, clients{}, upstreamsResponses{}, upstreamsTimeSum{}`. On hour change it is gob-serialised (`unitDB`) **truncated to the top 100** domains / blocked domains / clients / upstreams and stored as a bbolt bucket keyed by the 8-byte big-endian unit id; buckets older than `limit` (1/7/30/90 days; any 1 h–365 d) are deleted. `GET /control/stats[?recent=<ms multiple of 1 h>]` → `{time_units:"hours"|"days", top_queried_domains:[{name:count}], top_clients, top_blocked_domains, top_upstreams_responses, top_upstreams_avg_time:[{name:ms}], dns_queries:[], blocked_filtering:[], replaced_safebrowsing:[], replaced_parental:[], num_dns_queries, num_blocked_filtering, num_replaced_safebrowsing, num_replaced_safesearch, num_replaced_parental, avg_processing_time}`. Consequence: cluster-wide or arbitrary-window top lists are **approximate** (sum of per-hour top-100s) — the exact same limitation a rollup design must document.

### 1.3 blocky (Oct 2026 head) — pluggable sinks, no built-in query-log viewer [V]

`querylog/writer.go`: `type Writer interface { Write(*LogEntry); CleanUp() }` with `LogEntry{Start, ClientIP, ClientNames[], DurationMs, ResponseReason, ResponseType, ResponseCode, QuestionType, QuestionName, Answer, BlockyInstance, QueryWire, ResponseWire, QueryTime, ResponseTime, SocketProtocol}`. Sinks (`docs/configuration.md` "Query logging"): `console`, `csv` (one TSV per day, columns = time, client_ip, client_names, duration_ms, reason, question, answer, rcode, response_type, qtype, instance), `csv-client` (file per day per client), `sqlite` (WAL, pure-Go driver, not on MIPS), `mysql`, `postgresql`, `timescale` (`create_hypertable` + `add_retention_policy(drop_after => N days)`), `dnstap` (new; Frame Streams via `unix:/path`, `tcp://host:port`), `none`. Config: `queryLog.type/target/logRetentionDays/creationAttempts(3)/creationCooldown(2s)/fields[clientIP,clientName,responseReason,responseAnswer,question,duration]/flushInterval(30s)/ignore.sudn/ignore.domains`.

DB writer (`database_writer.go`): GORM model `logEntry{RequestTS (indexed), ClientIP, ClientName (indexed), DurationMs, Reason, ResponseType (indexed), QuestionType, QuestionName, EffectiveTLDP (eTLD+1 via publicsuffix), Answer, ResponseCode, Hostname}`; entries are appended to a slice under a mutex and flushed every `flushInterval` in **bulk inserts of 100**; SQLite is limited to `SetMaxOpenConns(1)`; retention = `DELETE WHERE request_ts < now-N days` (docs note SQLite does not shrink; run `VACUUM` manually).

dnstap writer (`dnstap_writer.go`): marshals one `CLIENT_RESPONSE` message per query (requires both wire buffers), pushes the encoded frame into a `chan []byte` of **4096**; when full the frame is dropped, `blocky_dnstap_frames_dropped_total` increments and a warning is logged at powers of ten. Uses `dnstap.NewSocketWriter(addr, &SocketWriterOptions{FlushTimeout, RetryInterval: 10s, Timeout: 30s, Dialer, Logger})`.

Dashboards: none built in; blocky ships Grafana JSON (`docs/blocky-grafana.json` for Prometheus, `docs/blocky-query-grafana.json` / `-postgres.json` for the SQL query log). New since 2026: `statistics.enable` gives an in-memory rolling-24 h `GET /api/stats` (summary, per-type breakdowns, per-hour series, top-20 domains/blocked/clients, list/cache gauges; 503 when disabled).

### 1.4 Technitium DNS Server 15.6 — logger apps + binary minute/hour/day stats [V]

Interfaces (`DnsServerCore.ApplicationCommon/IDnsQueryLogger.cs`, `IDnsQueryLogs.cs`): `Task InsertLogAsync(DateTime timestamp, DnsDatagram request, IPEndPoint remoteEP, DnsTransportProtocol protocol, DnsDatagram response)` and `Task<DnsLogPage> QueryLogsAsync(pageNumber, entriesPerPage, descendingOrder, start, end, clientIpAddress, protocol, responseType, rcode, qname, qtype, qclass)` → `DnsLogPage{pageNumber,totalPages,totalEntries,entries[DnsLogEntry{rowNumber,timestamp,clientIpAddress,protocol,responseType,responseRtt,rcode,question,answer}]}`. Any app implementing `IDnsQueryLogs` becomes the console's Query Logs tab via `GET /api/logs/query?name=&classPath=&pageNumber=&entriesPerPage=&descendingOrder=&start=&end=&clientIpAddress=&protocol=&responseType=&rcode=&qname=&qtype=&qclass=` (`APIDOCS.md:8108`).

Query Logs (SQLite) app (`Apps/QueryLogsSqliteApp/App.cs`):
```sql
CREATE TABLE IF NOT EXISTS dns_logs ( dlid INTEGER PRIMARY KEY, timestamp DATETIME NOT NULL, client_ip VARCHAR(39) NOT NULL,
  protocol TINYINT NOT NULL, response_type TINYINT NOT NULL, response_rtt REAL, rcode TINYINT NOT NULL,
  qname VARCHAR(255), qtype SMALLINT, qclass SMALLINT, answer TEXT );
-- 13 indexes: timestamp, client_ip, protocol, response_type, rcode, qname, qtype, qclass,
-- (timestamp,client_ip), (timestamp,qname), (client_ip,qname), (qname,qtype),
-- index_all(timestamp,client_ip,protocol,response_type,rcode,qname,qtype,qclass)
```
Protocol codes 0 UDP, 1 TCP, 2 TLS, 3 HTTPS, 5 QUIC, 253 UdpProxy, 254 TcpProxy; response types 1 Authoritative, 2 Recursive, 3 Cached, 4 Blocked (+ UpstreamBlocked, CacheBlocked in the API enum). Pipeline: `Channel.CreateBounded<LogEntry>(maxQueueSize 200000)` → consumer thread drains up to `BULK_INSERT_COUNT 1000` per transaction with a prepared `INSERT`, `BULK_INSERT_ERROR_DELAY 10 s` on failure; cleanup `maxLogDays`/`maxLogRecords` deleting in `BULK_REMOVE_COUNT 10000` batches, optional `VACUUM` (`enableVacuum`), `useInMemoryDb`. Timestamps are stored as text `yyyy-MM-dd HH:mm:ss.FFFFFFF`. Pagination is `SELECT COUNT(*) ... ; SELECT ... ORDER BY dlid [DESC] LIMIT @limit OFFSET @offset` (offset pagination; `qname` with `*` → `LIKE`). A `LogExporterApp` exports entries to file (JSON), HTTP (POST with headers) or syslog (UDP/TCP) with `maxQueueSize 1000000`.

Stats (`DnsServerCore/Dns/StatsManager.cs`): `StatCounter[60] _lastHourStatCounters` (one per minute), `HourlyStats` files `stats/yyyyMMddHH.stat` (binary, 60 minute counters), `StatCounter` daily files `yyyyMMdd.dstat`; cleanup hourly. `StatCounter` fields: `_totalQueries, _totalNoError, _totalServerFailure, _totalNxDomain, _totalRefused, _totalAuthoritative, _totalRecursive, _totalCached, _totalBlocked, _totalDropped, _totalClients`, dictionaries `_queryDomains`, `_queryBlockedDomains`, `_queryTypes`, `_protocolTypes`, `_clientIpAddressesUdpTcp`; files are truncated to the top 1000 per category (sibling technitium.md). Dashboard API `GET /api/dashboard/stats/get?type=LastHour|LastDay|LastWeek|LastMonth|LastYear|Custom&start&end&utc&node=<name>|cluster` returns `{stats{totalQueries,...,totalClients,zones,cachedEntries,allowedZones,blockedZones,allowListZones,blockListZones}, mainChartData{labelFormat,labels[],datasets[]}, queryTypeChartData, protocolTypeChartData, topClients, topDomains, topBlockedDomains}`; `node=cluster` fans out to every node and `topStatsData.Merge(await task, limit)` merges the per-node top-N (`WebServiceDashboardApi.cs:866`). Prometheus text at `/api/dashboard/metrics/text` uses **unprefixed** names (`uptime_seconds, start_time, queries_total, no_error_total, server_failure_total, nx_domain_total, refused_total, authoritative_total, recursive_total, cached_total, blocked_total, dropped_total, clients_total`) — lifetime counters only, no labels.

### 1.5 dnstap — the structured event format kudurru should emit [V]

Schema (`golang-dnstap/dnstap.pb/dnstap.proto`, proto2, CC0):
```
message Dnstap { optional bytes identity=1; optional bytes version=2; optional bytes extra=3; required Type type=15 (MESSAGE=1); optional Message message=14; }
enum SocketFamily { INET=1; INET6=2 }   enum SocketProtocol { UDP=1; TCP=2; DOT=3; DOH=4 }   // v0.4.0 vintage; newer dnstap.pb adds DOQ/DNSCrypt [U]
message Message {
  required Type type=1;   // AUTH_QUERY/RESPONSE, RESOLVER_QUERY/RESPONSE, CLIENT_QUERY/RESPONSE, FORWARDER_QUERY/RESPONSE,
                          // STUB_QUERY/RESPONSE, TOOL_QUERY/RESPONSE, UPDATE_QUERY/RESPONSE (1..14)
  optional SocketFamily socket_family=2; optional SocketProtocol socket_protocol=3;
  optional bytes query_address=4; optional bytes response_address=5; optional uint32 query_port=6; optional uint32 response_port=7;
  optional uint64 query_time_sec=8; optional fixed32 query_time_nsec=9; optional bytes query_message=10; optional bytes query_zone=11;
  optional uint64 response_time_sec=12; optional fixed32 response_time_nsec=13; optional bytes response_message=14; }
```
Recommended fields per type are listed in the proto comments (e.g. CLIENT_RESPONSE: socket_family/protocol, query_time, response_message, response_time). Transport is **Frame Streams** (`github.com/farsightsec/golang-framestream v0.3.0`, content type `protobuf:dnstap.Dnstap`) over a Unix socket, TCP or TLS; unidirectional or bidirectional (handshake) mode.

Go library (`golang-dnstap` v0.4.0): `Encoder/Decoder`, `FrameStreamInput/Output` (files), `FrameStreamSockInput/Output`, `NewSocketWriter(addr, &SocketWriterOptions{Timeout, FlushTimeout, RetryInterval, Dialer, Logger})` (reconnecting writer), text/YAML/JSON/quiet formatters. CLI `dnstap`: `-u <unix>` / `-l <tcp>` inputs, `-r <file>`, `-w <file>` output, `-T tcp:port` / `-U unix` relays, `-q|-y|-j` formats, `-t` timeout, `-a` append.

Producers and their knobs:
- **CoreDNS** `plugin/dnstap`: `dnstap SOCKET [full] [writebuffer] [queue] { identity; version; extra "<metadata template>"; skipverify }` or `dnstap listen SOCKET` (accept sinks, TLS/mTLS). Queue **10 000 × multiplier**, drops counted and logged every 10 s (`io.go`), 1 MiB TCP write buffer, 1 s flush, 4 s dial timeout, reconnect on error. Emits `CLIENT_QUERY` before the chain and `CLIENT_RESPONSE` after; `forward` emits `FORWARDER_QUERY/RESPONSE`.
- **Unbound** (`unbound.conf` `dnstap:` section): `dnstap-enable (no)`, `dnstap-bidirectional (yes)`, `dnstap-socket-path`, `dnstap-ip` (TCP/TLS instead of unix), `dnstap-tls (yes)` + `dnstap-tls-server-name/cert-bundle/client-key-file/client-cert-file`, `dnstap-send-identity/-version (no)`, `dnstap-identity/-version`, `dnstap-sample-rate (0 = all; else 1/N)`, and six per-type switches `dnstap-log-{resolver,client,forwarder}-{query,response}-messages (all no)`.
- **Knot DNS** `mod-dnstap`: `sink` (file / `unix:` / `tcp:` addr@port, required), `identity` (FQDN), `version`, `log-queries (on)`, `log-responses (on)`, `responses-with-queries (off)`.
- **dnsdist** (since 1.3.0): `newFrameStreamUnixLogger(path, {bufferHint, flushTimeout, inputQueueSize, outputQueueSize, queueNotifyThreshold, reopenInterval, connectionCount=1})`, `newFrameStreamTcpLogger(ip:port, ...)`, `DnstapLogAction(identity, logger[, alterFunction])`, `DnstapLogResponseAction(...)`, `DnstapMessage:setExtra()`; also raw protobuf via `newRemoteLogger()`.
- Also BIND 9.11+, PowerDNS Recursor 4.3+, NSD 4.1.26+, Knot Resolver 1.2.5+ (https://dnstap.info implementations list), blocky (above).

Consumers: `dnstap` CLI; `dnstap-ldns` (C) and `fstrm_capture` (fstrm) [U: not re-verified]; BIND `dnstap-read`; **DNS-collector** (github.com/dmachard/DNS-collector, MIT; inputs dnstap/pcap, outputs ClickHouse, Kafka, Loki, Elasticsearch, Syslog, Prometheus, InfluxDB, Fluentd, Redis, …; transforms normalize/geoip/anonymize/filter; v3.0.0 added ClickHouse batch JSONEachRow; v3.1.0 claimed Sept 2026 [U]). Its Loki example: `dnstap{listen-ip:0.0.0.0, listen-port:6000, tls-support:true}` → `lokiclient{server-url: http://loki:3100/loki/api/v1/push, job-name: dnscollector, text-format: "localtime identity qr queryip family protocol qname qtype rcode"}`.

**Why kudurru should emit dnstap:** it is the one format every serious DNS server and every DNS telemetry collector already speaks; it carries the verbatim wire messages (so consumers can compute anything later), it is cheap (protobuf + framing, async), and it lets kudurru offload heavy long-term analytics to DNS-collector/ClickHouse without inventing a custom export protocol.

### 1.6 Unbound and Knot statistics [V]

Unbound `unbound-control stats[_noreset]` (resets per call unless `statistics-cumulative: yes`; `extended-statistics: yes` needed for most): per-thread and `total.*` — `num.queries`, `num.queries_ip_ratelimited`, `num.queries_cookie_{valid,client,invalid}`, `num.queries_{discard_timeout,replyaddr_limit,wait_limit,timed_out}`, `num.cachehits`, `num.cachemiss`, `num.prefetch`, `num.expired` (served-stale), `num.recursivereplies`, `requestlist.{avg,max,overwritten,exceeded,current.all,current.user,current.replies}`, `recursion.time.{avg,median}`, `tcpusage`, `query.queue_time_us.max`; `time.{now,up,elapsed}`; `mem.cache.{rrset,message}`, `mem.mod.{iterator,validator}`, `mem.streamwait`, `mem.http.{query,response}_buffer`, `mem.quic`; `histogram.<s>.<us>.to.<s>.<us>` (40 doubling buckets, recursion time only — cache hits are **not** in the histogram); `num.query.type.<T>`, `num.query.class.<C>`, `num.query.opcode.<O>`, `num.query.{tcp,tcpout,udpout,tls,tls.resume,https,quic,ipv6}`, `num.query.flags.{QR,AA,TC,RD,RA,Z,AD,CD}`, `num.query.edns.{present,DO}`, `num.answer.rcode.<RCODE>`, `num.answer.rcode.nodata`, `num.answer.{secure,bogus}`, `num.rrset.bogus`, `num.valops`, `unwanted.{queries,replies}`, `{msg,rrset,infra,key}.cache.count`, `num.query.ratelimited`, `num.query.authzone.{up,down}`, `num.query.aggressive.{NOERROR,NXDOMAIN}`, `num.query.subnet`, `num.query.subnet_cache`, `num.query.cachedb`, `num.rpz.action.<action>`. Exporter: letsencrypt/unbound_exporter (port 9167, `-unbound.host unix:///run/unbound.ctl`, regex-maps stats → `unbound_*`; only `unbound_up` named in README; the full metric names are [U]).

Knot `mod-stats` counters (each becomes a label set): `request-protocol` (udp4, tcp4, quic4, tls4, udp6, …, `*-xdp`), `server-operation` (query, update, notify, axfr, ixfr, invalid), `request-bytes` / `response-bytes`, `edns-presence`, `flag-presence` (TC, DO), `response-code` (NOERROR … BADCOOKIE, other), `request-/response-edns-option`, `reply-nodata` (A, AAAA, other), `query-type` (per RR type + other), `query-size` / `reply-size` (16-byte buckets). Output: `statistics: { timer, file (rundir/stats.yaml), append }` or `knotc stats`; Prometheus via third-party `knot_exporter` [U].

---

## 2. kudurru query-log pipeline (proposal)

### 2.1 Event record (Go)

```go
// QueryEvent is created once per client query; immutable after the response is written.
type QueryEvent struct {
    ID         uint64        // node-local monotonic (also SQLite rowid)
    TS         int64         // query receive time, unix µs
    LatencyUS  int32         // -1 if no response
    Node       uint16        // dictionary id of this node (pod name / hostname)
    Proto      uint8         // 0 udp,1 tcp,2 dot,3 doh,4 doq,5 dnscrypt
    Family     uint8         // 4 / 6
    ClientIP   netip.Addr    // after privacy transform
    ClientID   uint32        // dict_client id (ip|name|group snapshot)
    QName      string        // lower-cased, no trailing dot; dict_domain id resolved by writer
    QType      uint16; QClass uint16
    Status     uint8         // kudurru status enum (see 2.4) — "why this answer"
    RCode      uint8         // 0..23
    ReplyType  uint8         // ip, cname, nodata, nxdomain, ..., blob (mirrors FTL)
    Upstream   uint32        // dict_upstream id or 0
    ZoneID     uint32        // authoritative/forward zone that matched (dict_zone) or 0
    ListID     uint32        // blocklist / rule that matched, 0 if none
    CNAME      string        // CNAME-chain domain that caused a block (optional)
    DNSSEC     uint8         // unknown, secure, insecure, bogus, indeterminate
    EDE        int16         // extended DNS error code or -1
    Flags      uint16        // RD, DO, TC, AD, CD, cached, stale, prefetch, ecs-present, had-traceparent
    ECS        netip.Prefix  // optional
    TraceID    [16]byte      // zero if no trace
    AnswerSummary string     // first ~3 RRs, truncated (optional, privacy-dependent)
    QueryWire, ResponseWire []byte // only retained while dnstap/full sinks exist; never stored in SQLite
}
```
Size without wire buffers ≈ 160 B in memory; the SQLite row (ints only, strings dictionary-encoded) is ≈ 40–60 B before indexes.

### 2.2 Stages

```
listener workers ──(per-worker lock-free SPSC ring, 4096)──► collector goroutine
      │                                                          │
      │  counters/histograms updated inline (atomic)             ├─► hot ring (last N events, default 65 536; serves live tail + "last 24 h" stats)
      │                                                          ├─► rollup accumulators (1-minute buckets in memory)
      └─ never blocks on logging: ring full ⇒ drop + metric      ├─► batch writer (SQLite, single writer goroutine)
                                                                 └─► sink fan-out (dnstap / ClickHouse / Loki / webhook), each with its own bounded queue + drop counter
```
- **Never block the DNS path.** Every hand-off is a non-blocking enqueue; overflow increments `kudurru_querylog_dropped_total{stage}` (blocky/CoreDNS pattern, not Technitium's bounded channel with blocking semantics).
- **Collector** assigns IDs, applies the privacy transform *once* (so every downstream sees identical data), resolves dictionary ids through an LRU (FTL's addinfo cache idea), updates rollups, and appends to the hot ring.
- **Batch writer** flushes when `len(batch) >= 1000` **or** `1 s` elapsed, whichever first (Technitium 1000, FTL 1 s). One `BEGIN IMMEDIATE … COMMIT` per batch, prepared statements, `INSERT OR IGNORE` into dictionaries only on cache miss. Row ids are allocated by the collector so the writer is idempotent on retry.
- **Rollup flush** at minute boundary: upsert `rollup_1m` rows for the closed minute (one transaction); hour/day rollups are recomputed from `rollup_1m` by a scheduled job (`INSERT … SELECT … GROUP BY` for the closed hour/day).
- **Grace**: a query without a response yet is written with `latency_us=-1,status=in_progress`; the writer keeps an "open" map (max 30 s, FTL `REPLY_TIMEOUT`) and issues `UPDATE … WHERE id=?` when the response arrives, or writes the final row only after completion (simpler; default) — choose the latter: hold the event in the open map until response or 30 s timeout, then emit once. Live tail sees the event from the hot ring immediately, with status `in_progress`.

### 2.3 SQLite schema (one file per node, e.g. `/var/lib/kudurru/querylog.db`)

```sql
PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA auto_vacuum=INCREMENTAL;
PRAGMA wal_autocheckpoint=2000; PRAGMA mmap_size=268435456; PRAGMA busy_timeout=1000; PRAGMA temp_store=MEMORY;

CREATE TABLE meta (key TEXT PRIMARY KEY, value) WITHOUT ROWID;      -- schema_version, node_id, first_ts, privacy_mode_at_write

CREATE TABLE dict_domain   (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, etld1 TEXT, first_seen INTEGER, last_seen INTEGER);
CREATE TABLE dict_client   (id INTEGER PRIMARY KEY, ip TEXT NOT NULL, name TEXT, grp TEXT, first_seen INTEGER, last_seen INTEGER, UNIQUE(ip,name));
CREATE TABLE dict_upstream (id INTEGER PRIMARY KEY, addr TEXT NOT NULL UNIQUE);     -- "tls://1.1.1.1:853", "cache", "zone:example.com"
CREATE TABLE dict_zone     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE dict_list     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);     -- blocklist / rule source
CREATE TABLE dict_node     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);

CREATE TABLE query (
  id          INTEGER PRIMARY KEY,          -- rowid, monotonic per node (time-ordered)
  ts          INTEGER NOT NULL,             -- unix µs
  latency_us  INTEGER,                      -- NULL if unanswered
  node        INTEGER NOT NULL REFERENCES dict_node,
  proto       INTEGER NOT NULL,             -- 0 udp 1 tcp 2 dot 3 doh 4 doq 5 dnscrypt
  client      INTEGER NOT NULL REFERENCES dict_client,
  domain      INTEGER NOT NULL REFERENCES dict_domain,
  qtype       INTEGER NOT NULL, qclass INTEGER NOT NULL DEFAULT 1,
  status      INTEGER NOT NULL,             -- see 2.4
  rcode       INTEGER NOT NULL,
  reply_type  INTEGER NOT NULL,
  upstream    INTEGER REFERENCES dict_upstream,
  zone        INTEGER REFERENCES dict_zone,
  list        INTEGER REFERENCES dict_list,
  cname       INTEGER REFERENCES dict_domain,
  dnssec      INTEGER NOT NULL DEFAULT 0,
  ede         INTEGER,
  flags       INTEGER NOT NULL DEFAULT 0,
  ecs         TEXT,
  trace_id    BLOB,                         -- 16 bytes, NULL unless traced
  answer      TEXT                          -- short summary, NULL in restricted privacy modes
);
-- Deliberately few indexes (every index costs ~10-15 B/row and write amplification):
CREATE INDEX query_ts        ON query(ts);
CREATE INDEX query_client_ts ON query(client, ts);
CREATE INDEX query_domain_ts ON query(domain, ts);
CREATE INDEX query_status_ts ON query(status, ts) WHERE status >= 16;   -- partial: blocked/error rows only (recent-blocked list)

-- Rollups. Minute granularity for cheap dimensions; hourly for domains (high cardinality).
CREATE TABLE rollup_1m (
  ts INTEGER NOT NULL, node INTEGER NOT NULL,
  total INTEGER NOT NULL, blocked INTEGER NOT NULL, cached INTEGER NOT NULL, forwarded INTEGER NOT NULL, authoritative INTEGER NOT NULL, dropped INTEGER NOT NULL,
  rc_noerror INTEGER, rc_nxdomain INTEGER, rc_servfail INTEGER, rc_refused INTEGER, rc_other INTEGER,
  udp INTEGER, tcp INTEGER, dot INTEGER, doh INTEGER, doq INTEGER,
  lat_sum_us INTEGER, lat_cnt INTEGER,
  lat_b0 INTEGER, lat_b1 INTEGER, lat_b2 INTEGER, lat_b3 INTEGER, lat_b4 INTEGER, lat_b5 INTEGER, lat_b6 INTEGER, lat_b7 INTEGER, lat_b8 INTEGER, lat_b9 INTEGER, lat_b10 INTEGER, lat_b11 INTEGER,
  -- bucket upper bounds (ms): 0.5,1,2,5,10,25,50,100,250,500,1000,+Inf (matches Prometheus histogram so both agree)
  PRIMARY KEY (ts, node)) WITHOUT ROWID;
CREATE TABLE rollup_client_1m   (ts INTEGER, node INTEGER, client INTEGER, total INTEGER, blocked INTEGER, PRIMARY KEY (ts,node,client)) WITHOUT ROWID;
CREATE TABLE rollup_qtype_1m    (ts INTEGER, node INTEGER, qtype INTEGER, total INTEGER, PRIMARY KEY (ts,node,qtype)) WITHOUT ROWID;
CREATE TABLE rollup_upstream_1m (ts INTEGER, node INTEGER, upstream INTEGER, total INTEGER, errors INTEGER, timeouts INTEGER, lat_sum_us INTEGER, lat_cnt INTEGER,
                                 lat_b0 INTEGER, lat_b1 INTEGER, lat_b2 INTEGER, lat_b3 INTEGER, lat_b4 INTEGER, lat_b5 INTEGER, lat_b6 INTEGER, lat_b7 INTEGER, lat_b8 INTEGER, lat_b9 INTEGER, lat_b10 INTEGER, lat_b11 INTEGER,
                                 PRIMARY KEY (ts,node,upstream)) WITHOUT ROWID;
CREATE TABLE rollup_domain_1h   (ts INTEGER, node INTEGER, domain INTEGER, total INTEGER, blocked INTEGER, PRIMARY KEY (ts,node,domain)) WITHOUT ROWID;
CREATE TABLE rollup_list_1h     (ts INTEGER, node INTEGER, list INTEGER, hits INTEGER, PRIMARY KEY (ts,node,list)) WITHOUT ROWID;
CREATE TABLE rollup_zone_1h     (ts INTEGER, node INTEGER, zone INTEGER, total INTEGER, PRIMARY KEY (ts,node,zone)) WITHOUT ROWID;
-- Hour/day tables have identical shapes (rollup_1h, rollup_client_1h, rollup_qtype_1h, rollup_upstream_1h, rollup_1d, rollup_client_1d, rollup_domain_1d).
CREATE INDEX rollup_domain_1h_ts ON rollup_domain_1h(ts);
```
Status taxonomy (`status` column; keep ≥16 = "not answered normally" so the partial index works): 0 unknown, 1 forwarded, 2 cached, 3 cached_stale, 4 authoritative, 5 local_record, 6 prefetch_refresh, 7 special_domain, 8 retried, 9 in_progress, 16 blocked_list, 17 blocked_regex, 18 blocked_exact, 19 blocked_cname, 20 blocked_upstream (0.0.0.0/NXRA/EDE15), 21 blocked_rpz, 22 blocked_safesearch, 23 blocked_rate_limit, 24 refused_acl, 25 dropped, 26 servfail_upstream, 27 timeout_upstream, 28 dnssec_bogus. The API returns the symbolic names; the UI colours by class (`permitted / cached / blocked / error`) like Pi-hole.

### 2.4 Sizing (back-of-envelope, assume SSD, 4 KiB pages)

| | 100 qps | 1 000 qps |
|---|---|---|
| Rows/day | 8.64 M | 86.4 M |
| Fact row incl. 4 indexes | ≈ 45 B data + ≈ 55 B index ≈ **100 B** | same |
| Fact table growth/day | ≈ 0.86 GB | ≈ 8.6 GB |
| `rollup_1m` (1 row/min/node, ≈ 150 B) | 0.2 MB/day | 0.2 MB/day |
| `rollup_client_1m` (≤ 1 row/client/min; 200 clients) | ≤ 40 MB/day | ≤ 40 MB/day (clients, not qps, drive it) |
| `rollup_domain_1h` (unique domains/h; say 20 k) | ≈ 15 MB/day | ≈ 50–150 MB/day |
| Write load | 1 txn/s, ≈ 100 rows | 1 txn/s, ≈ 1 000 rows (SQLite handles 50–100 k rows/s per txn on SSD [U]) |
| WAL growth → auto-checkpoint (2000 pages ≈ 8 MB) | every ≈ 90 s | every ≈ 9 s |
| Suggested raw retention default | 30 days (≈ 26 GB) → set `max_db_bytes` 10 GB by default and let size win | 2 days raw (≈ 17 GB) + external sink |

Pi-hole users typically see ≈ 100 B/row on disk with FTL's heavier indexing (sibling pihole.md / forum data [U]); the estimate above is consistent. Memory: hot ring 65 536 × 160 B ≈ 10 MB; dictionary LRUs ≈ 1–5 MB; rollup accumulators negligible.

### 2.5 Retention, checkpoints, vacuum

- Retention job daily at a randomised minute 03:10–03:50 local (FTL) **and** whenever `max_db_bytes` is exceeded: `DELETE FROM query WHERE id < (SELECT id FROM query WHERE ts >= :cutoff ORDER BY id LIMIT 1)` in batches of 10 000 rows (Technitium) with `PRAGMA incremental_vacuum(2000)` between batches so pages are returned without a full `VACUUM`. Dictionaries are pruned by `last_seen < cutoff` after the fact rows are gone. Rollups: `rollup_*_1m` 35 days, `_1h` 400 days, `_1d` forever (tiny).
- Checkpoints: `wal_autocheckpoint=2000` plus a `PRAGMA wal_checkpoint(TRUNCATE)` from the writer goroutine every 5 min **only when no API read is in flight** (SQLite doc: a checkpoint cannot complete while readers are active → WAL grows without bound; "reader gaps" are the mitigation). API reads use a separate read-only connection pool with `busy_timeout` and short statements; long exports stream in pages of `cursor` rows.
- `ANALYZE` weekly; `PRAGMA optimize` on close (FTL).
- Durability contract (document it): with `synchronous=NORMAL` a crash can lose the last ≤ 1 s batch; power loss can roll back un-checkpointed WAL transactions but never corrupts (https://www.sqlite.org/wal.html).
- Do **not** place the DB on NFS/SMB (WAL shared-memory requirement); in Kubernetes use a local `emptyDir`/hostPath or a block PVC, never RWX NFS.

### 2.6 Privacy modes (mirror Pi-hole levels, applied at ingest, irreversible)

| `querylog.privacy` | Client | Domain | Stored rows | Dashboard panels disabled |
|---|---|---|---|---|
| `full` (0) | real IP + name | real | all | — |
| `anonymize_client` (1a) | IPv4 /24, IPv6 /48 zeroed (AdGuard: last 2 / last 10 bytes) **or** HMAC(daily key, ip) when `querylog.privacy_hash: true` (stable per-day pseudonym for per-client stats without identity) | real | all | Clients tab shows pseudonyms |
| `hide_domains` (1) | real | `hidden` (dict id 1) | all | Top domains / blocked domains / search by domain |
| `hide_domains_clients` (2) | `0.0.0.0` (dict id 2) | `hidden` | all | + Top clients / clients over time |
| `nothing` (3) | — | — | **no fact rows, no rollups per client/domain**; only `rollup_1m` totals/rcodes/qtypes/latency | Query log, all top lists |

Plus orthogonal filters (AdGuard `ignored`, blocky `ignore.sudn/domains`, Pi-hole `excludeClients/excludeDomains`): `querylog.exclude.clients: [cidr|name]`, `querylog.exclude.domains: [exact|*.wildcard|/regex/]`, `querylog.exclude.sudn: true` (drop `.local`, `.arpa` reverse of RFC1918, etc. from the log but keep counters), per-client group flag `log: false`. Sinks inherit the transformed event; **dnstap is a separate switch** (`sinks.dnstap.raw: true` ships verbatim wire data regardless of privacy mode and must be an explicit admin decision with an audit entry). Changing privacy to a stricter level is immediate; relaxing it takes effect only for new rows (document like Pi-hole).

### 2.7 External sinks (fan-out, each bounded)

```yaml
querylog:
  enabled: true
  privacy: full            # full|anonymize_client|hide_domains|hide_domains_clients|nothing
  retention: { raw: 720h, max_db_bytes: 10GiB, rollup_1m: 840h, rollup_1h: 9600h }
  ring_size: 65536
  batch: { max_rows: 1000, max_wait: 1s }
  sinks:
    dnstap:   { enabled: true, target: unix:/run/kudurru/dnstap.sock, raw: true, identity: "${NODE_NAME}", types: [client_query, client_response, forwarder_query, forwarder_response], queue: 10000 }
    clickhouse: { enabled: false, url: http://clickhouse:8123, database: kudurru, table: query, batch: 5000, flush: 5s, auth_secret: ch-creds }
    loki:     { enabled: false, url: http://loki:3100/loki/api/v1/push, labels: { app: kudurru, node: "${NODE_NAME}" }, batch: 1000, flush: 2s }
    webhook:  { enabled: false, url: https://example/ingest, format: ndjson, batch: 500, flush: 5s, headers_secret: hook }
    otlp_logs: { enabled: false, endpoint: otel-collector:4317 }
```
- **dnstap**: use `golang-dnstap` `SocketWriter` (reconnecting, `FlushTimeout 1s`), queue 10 000 (CoreDNS), `kudurru_dnstap_dropped_total`. Emit `CLIENT_QUERY` on receive and `CLIENT_RESPONSE` on send; `FORWARDER_QUERY/RESPONSE` around each upstream exchange; `identity` = node name, `version` = `kudurru/<ver>`, `extra` = `status=<name> list=<id> trace=<hex>` (CoreDNS uses `extra` for metadata templates). Also offer `dnstap listen` mode so `kudurru` can **receive** dnstap from dnsdist/Unbound sidecars and merge them into the same log (CoreDNS listener pattern).
- **ClickHouse**: HTTP `INSERT INTO kudurru.query FORMAT JSONEachRow`; suggested DDL `ENGINE = MergeTree ORDER BY (node, client, ts) PARTITION BY toYYYYMMDD(ts) TTL ts + INTERVAL 90 DAY DELETE` (ClickHouse docs: ORDER BY drives reads, partitions do not; TTL removes rows in background merges). This is the cluster-wide long-term store.
- **Loki**: `POST /loki/api/v1/push` JSON `{"streams":[{"stream":{labels},"values":[["<ns>","<line>",{structured metadata}]]}]}` (timestamps are strings); keep labels low-cardinality (`app,node,status_class`) and put `client`, `domain`, `trace_id` into structured metadata (Loki docs), lines as JSON for LogQL `| json`.
- **webhook**: NDJSON batches, `Content-Type: application/x-ndjson`, HMAC signature header, exponential backoff, at-most-once (drop after N retries, count it). Mirrors Technitium LogExporterApp's HTTP/syslog targets.

### 2.8 Dashboard API (all under `/api/v1`, JSON, `took_ms` in every body; `scope=node|cluster`, `node=<name>`)

| Endpoint | Params | Returns |
|---|---|---|
| `GET /stats/summary` | `from,until` (default last 24 h, served from hot ring + rollups) | `{queries:{total,blocked,percent_blocked,cached,forwarded,authoritative,dropped,unique_domains,unique_clients,qps_1m,qps_avg}, rcodes:{NOERROR,NXDOMAIN,SERVFAIL,REFUSED,other}, types:{A,AAAA,PTR,HTTPS,...,other}, protocols:{udp,tcp,dot,doh,doq}, latency:{p50_ms,p95_ms,p99_ms,avg_ms}, blocking:{enabled,lists:{total,enabled,domains}}, nodes:[{name,ok,took_ms}]}` |
| `GET /stats/timeseries` | `from,until,step` (`auto|1m|5m|1h|1d`; auto = ≤ 300 points), `series` (csv of `total,blocked,cached,forwarded,authoritative,rcode.*,proto.*,qtype.*,latency.p95`), `group_by` (`none|node|client|upstream|qtype`, top N + `other`) | `{step:60, start, points:[{ts, total, blocked, cached, forwarded, ...}], groups?:{key:[...]}}` |
| `GET /stats/top` | `dim=domain|blocked_domain|client|blocked_client|upstream|list|zone|qtype|rcode`, `n` (≤ 100), `from,until`, `exact=false` | `{items:[{key, name?, total, blocked, percent}], total_in_window, approximate:boolean, cutoff_rank?}` |
| `GET /stats/clients/timeseries` | `from,until,step,n` | Pi-hole clients-over-time shape plus `other` bucket |
| `GET /stats/upstreams` | `from,until` | `{upstreams:[{addr,total,errors,timeouts,avg_ms,p95_ms,healthy,state}]}` |
| `GET /stats/recent_blocked` | `n` | `[{ts,domain,client,list}]` (uses partial index) |
| `GET /queries` | `from,until,cursor,limit (≤ 1000),dir=desc`, filters `domain (wildcard * and _)`, `client`, `client_name`, `upstream`, `qtype`, `status` (csv), `status_class` (`permitted|blocked|cached|error`), `rcode`, `reply`, `dnssec`, `zone`, `list`, `proto`, `node`, `trace_id`, `q` (free text over domain+client), `source=memory|disk|auto` | `{queries:[{id,ts,node,proto,client:{ip,name,group},domain,unicode?,cname,qtype,status,rcode,reply:{type,time_ms,answer},upstream,zone,list:{id,name},dnssec,ede:{code,text},ecs,trace_id}], cursor_next, cursor_prev, records_total, records_filtered, earliest_ts_memory, earliest_ts_disk}` — **keyset pagination on `id`** (FTL cursor), never OFFSET |
| `GET /queries/suggestions` | `field, prefix, from,until` | distinct values from dictionaries (bounded, ≤ 100) |
| `GET /queries/export` | same filters, `format=ndjson|csv|dnstap`, streams with `Transfer-Encoding: chunked`; privacy transform applies | file |
| `GET /queries/stream` (SSE) | same filters; `Accept: text/event-stream`; `Last-Event-ID` = last seen id | events `id:<id>\nevent:query\ndata:<json>\n\n`; heartbeat `: ping` every 15 s; server fans out from the hot ring with a per-connection buffer of 1 000 and `event: dropped` when overrun; cluster scope multiplexes node streams and adds `node` |
| `WS /queries/stream` | same, for UIs that prefer WebSocket | same frames |
| `POST /queries/{id}/actions` | `allow|deny|whois|flush_cache` | per-row quick actions (Pi-hole) with audit entry |
| `DELETE /querylog` | `before=<ts>`, `client=`, `domain=` | GDPR-style targeted deletion + audit |
| `GET /querylog/status` | — | `{rows, bytes, wal_bytes, oldest_ts, dropped:{ring,writer,sinks{...}}, last_checkpoint, privacy, retention}` |

Semantics to copy from Pi-hole: `records_total/records_filtered`, stable cursor, `earliest_*` for the date picker, per-field suggestions, free-text wildcards; from AdGuard: `older_than` style cursor is unnecessary with ids, but keep `source=auto` (memory first, then disk); from Technitium: `node=cluster` on every stats call.

---

## 3. Metrics

### 3.1 What the references expose [V unless marked]

| System | Metric families (labels) |
|---|---|
| **blocky** (`resolver/metrics_resolver.go`, `docs/prometheus_grafana.md`) | `blocky_build_info{version,build_time}`, `blocky_blocking_enabled`, `blocky_denylist_cache_entries{group}`, `blocky_allowlist_cache_entries{group}`, `blocky_query_total{client,type}`, `blocky_error_total`, `blocky_request_duration_seconds{response_type}` buckets `0.005…2.0`, `blocky_response_total{reason,response_code,response_type}`, `blocky_client_response_total{client,response_type}`, `blocky_cache_entries`, `blocky_cache_hits_total`, `blocky_cache_misses_total`, `blocky_last_list_group_refresh_timestamp_seconds`, `blocky_prefetches_total`, `blocky_prefetch_hits_total`, `blocky_prefetch_domain_name_cache_entries`, `blocky_failed_downloads_total`, `blocky_dnssec_validation_total{result}`, `blocky_dnssec_cache_hits_total`, `blocky_dnssec_validation_duration_seconds`, `blocky_redis_cache_buffer_drops_total`, `blocky_rate_limit_drops_total{protocol}`, `blocky_rate_limit_cap_exhausted_total`, `blocky_rate_limit_active_buckets`, `blocky_dnstap_frames_dropped_total`. Docs explicitly warn: `reason` is bounded to group names (not rules) and `client` is **unbounded** (reverse-DNS names) — their only mitigation is `metric_relabel_configs` drop. |
| **AdGuard Home** (no native Prometheus; ebrianne/adguard-exporter port 9617 [U names from Grafana dashboard 13330]) | `adguard_num_dns_queries`, `adguard_num_blocked_filtering`, `adguard_num_replaced_parental`, `adguard_num_replaced_safebrowsing`, `adguard_num_replaced_safesearch`, `adguard_avg_processing_time`, `adguard_top_queried_domains{domain}`, `adguard_top_blocked_domains{domain}`, `adguard_top_clients{client}`, `adguard_query_types{type}`, `adguard_running`, `adguard_protection_enabled` — i.e. a scrape of `/control/stats` (24 h-window gauges, not counters). |
| **Pi-hole** (pihole6_exporter, port 9666, app password) | `pihole_query_by_type{query_type}`, `pihole_query_by_status{query_status}`, `pihole_query_replies{reply_type}`, `pihole_query_count{category=total|blocked|unique|forwarded|cached}`, `pihole_client_count{category}`, `pihole_domains_being_blocked`, `pihole_query_upstream_count{ip,port,name}` (24 h gauges) + 1-minute variants `pihole_query_{type,status,upstream,reply,client}_1m`. |
| **CoreDNS** (`plugin/metrics/vars/vars.go`, 1.14.7) | `coredns_build_info{version,revision,goversion}`, `coredns_panics_total`, `coredns_dns_requests_total{server,zone,view,proto,family,type}`, `coredns_dns_request_duration_seconds{server,zone,view}` (native histogram enabled), `coredns_dns_request_size_bytes{server,zone,view,proto}`, `coredns_dns_do_requests_total{server,zone,view}`, `coredns_dns_response_size_bytes{...}`, `coredns_dns_responses_total{server,zone,view,rcode,plugin}`, `coredns_dns_https_responses_total{server,status}`, `coredns_dns_https3_responses_total`, `coredns_dns_quic_responses_total{server,status}`, `coredns_plugin_enabled{server,zone,view,name}`; cache: `coredns_cache_entries{server,type,zones,view}`, `coredns_cache_requests_total`, `coredns_cache_hits_total{type}`, `coredns_cache_misses_total` (deprecated), `coredns_cache_prefetch_total`, `coredns_cache_drops_total`, `coredns_cache_served_stale_total`, `coredns_cache_evictions_total`; forward: `coredns_forward_healthcheck_broken_total`, `coredns_forward_max_concurrent_rejects_total` (+ `coredns_forward_requests_total{to}`, `_responses_total{to,rcode}`, `_request_duration_seconds{to,rcode}`, `_healthcheck_failures_total{to}` [U]); health: `coredns_health_request_duration_seconds`, `coredns_health_request_failures_total`. `type` label is bounded to a fixed list + `other`; unknown-zone queries are tagged `zone="dropped"`. |
| **dnsdist** (`/metrics`, `dnsdist.org/guides/webserver.html`, `statistics.html`) | globals `dnsdist_queries`, `dnsdist_responses`, `dnsdist_servfail_responses`, `dnsdist_rdqueries`, `dnsdist_empty_queries`, `dnsdist_noncompliant_queries`, `dnsdist_cache_hits/misses`, `dnsdist_self_answered`, `dnsdist_downstream_timeouts`, `dnsdist_downstream_send_errors`, `dnsdist_no_policy`, `dnsdist_dyn_blocked`, `dnsdist_acl_drops`, `dnsdist_frontend_{noerror,nxdomain,servfail}`, `dnsdist_rule_{drop,nxdomain,refused,servfail,truncated}`, `dnsdist_rule_hits{rule}`; latency buckets `dnsdist_latency0_1, 1_10, 10_50, 50_100, 100_1000, latency_slow`, histogram `dnsdist_latency` (ms, le = 1,10,50,100,1000), `dnsdist_latency_avg{100,1000,10000,1000000}` + `_tcp_`, `_dot_`, `_doh_`, `_doq_` variants; process `dnsdist_uptime, real_memory_usage, fd_usage, cpu_{sys,user}_msec, cpu_iowait, cpu_steal, security_status`; kernel `dnsdist_udp{,6}_{in_errors,in_csum_errors,noport_errors,recvbuf_errors,sndbuf_errors}`, `dnsdist_tcp_listen_overflows`; pipes `dnsdist_{doh,doq,tcp}_{query,response}_pipe_full`; per-backend `dnsdist_server_{status,queries,responses,drops,latency,tcplatency,outstanding,order,weight,tcp*,tlsresumptions}{server,address}`; per-frontend `dnsdist_frontend_{queries,responses,noncompliantqueries,tcpcurrentconnections,tls*,http_connects,doh_http_method_queries{method},doh_http_version_queries{version},doh_responses{type},doh_version_status_responses{httpversion,status}}{frontend,proto,thread}`; per-pool `dnsdist_pool_{servers,active_servers,cache_size,cache_entries,cache_hits,cache_misses,...}{pool}`; `dnsdist_info{version}`. Note the **non-`_total` counter names** (predates conventions). `/jsonstat?command=stats` returns the same as a flat dict; `/api/v1/servers/localhost` returns frontends/pools/servers/rules objects; `/api/v1/servers/localhost/rings` returns recent query/response ring entries (`requestor, name, qtype, protocol, latency µs, rcode, backend`). |
| **Unbound** | native stats listed in §1.6; exporter names [U]. |
| **Technitium** | unprefixed lifetime counters (§1.4). |
| **Gravity** (sibling gravity.md) | `gravity_dns_queries{queryType,handler,zone}`, `gravity_dns_query_duration`, `gravity_dns_records{zone}`, role gauges, plus embedded etcd and blocky registries on `:8009`. |

### 3.2 kudurru metric set (proposal; Prometheus naming rules: single prefix, base units, `_total`, `_info`; no unbounded labels)

Global label policy: every series gets `node` from the Prometheus scrape (pod label), **not** from kudurru. Bounded label values only: `proto ∈ {udp,tcp,dot,doh,doq,dnscrypt}`, `family ∈ {4,6}`, `qtype ∈ {A,AAAA,PTR,MX,TXT,SRV,SOA,NS,CNAME,HTTPS,SVCB,DS,DNSKEY,RRSIG,NSEC,NSEC3,NAPTR,CAA,ANY,AXFR,IXFR,other}`, `rcode ∈ {NOERROR,NXDOMAIN,SERVFAIL,REFUSED,FORMERR,NOTIMP,NOTAUTH,other}`, `status` = taxonomy in §2.3 (≤ 32), `status_class ∈ {permitted,cached,blocked,error}`, `zone` = configured zones only (unknown → `_other`), `upstream` = configured upstreams (name, not raw IP), `list` = configured lists (name), `view`/`group` = configured client groups (≤ 64; else `_other`), `role` for cluster roles, `sink ∈ {sqlite,dnstap,clickhouse,loki,webhook}`. **`client` is never a label**; per-client data lives in the query log / rollups and in the dashboard API. A `metrics.client_labels: top_n` option (default 0) may expose `kudurru_client_queries_total{client}` for the N most active clients re-evaluated hourly, with the rest in `_other` — opt-in only.

```
# Build / state
kudurru_build_info{version,revision,goversion}  gauge 1
kudurru_up_since_seconds  gauge                     kudurru_config_reloads_total{result}  counter
kudurru_role_active{role}  gauge                    kudurru_leader{role}  gauge 0/1
kudurru_blocking_enabled{group}  gauge              kudurru_blocklist_entries{list}  gauge
kudurru_blocklist_last_update_timestamp_seconds{list}  kudurru_blocklist_update_failures_total{list}
kudurru_zone_records{zone,kind}  gauge              kudurru_zone_serial{zone}  gauge
kudurru_zone_transfers_total{zone,direction,result}  kudurru_dnssec_key_expiry_timestamp_seconds{zone,key}

# Traffic (the core five)
kudurru_dns_requests_total{proto,family,qtype,zone}                 counter
kudurru_dns_responses_total{proto,rcode,status,zone}                counter
kudurru_dns_request_duration_seconds{proto,status_class}            histogram, buckets .0005,.001,.002,.005,.01,.025,.05,.1,.25,.5,1,+Inf (+ native)
kudurru_dns_request_size_bytes{proto} / kudurru_dns_response_size_bytes{proto}  histogram (CoreDNS buckets)
kudurru_dns_inflight_requests{proto}                                 gauge
kudurru_dns_dropped_total{reason=acl|ratelimit|overload|malformed|truncated}  counter
kudurru_dns_do_requests_total                                         counter   (DO bit)
kudurru_dns_edns_total{feature=ecs|cookie|padding|traceparent}       counter
kudurru_dns_tcp_connections{proto}  gauge   kudurru_dns_tls_handshakes_total{proto,result}  counter   kudurru_dns_doh_http_responses_total{status}

# Blocking
kudurru_block_decisions_total{action=block|allow|rewrite,source=list|regex|exact|cname|rpz|safesearch|upstream,group}  counter
kudurru_block_list_hits_total{list}  counter

# Cache
kudurru_cache_entries{kind=message|rrset|negative}  gauge   kudurru_cache_requests_total  kudurru_cache_hits_total{kind,stale}  kudurru_cache_evictions_total{reason}  kudurru_cache_prefetches_total

# Upstreams / forwarding
kudurru_upstream_requests_total{upstream,proto}  counter   kudurru_upstream_responses_total{upstream,rcode}  counter
kudurru_upstream_request_duration_seconds{upstream}  histogram   kudurru_upstream_errors_total{upstream,error=timeout|refused|network|tls|servfail}  counter
kudurru_upstream_healthy{upstream}  gauge   kudurru_upstream_inflight{upstream}  gauge

# Query-log pipeline & sinks
kudurru_querylog_events_total  counter   kudurru_querylog_dropped_total{stage=ring|writer|sink,sink}  counter
kudurru_querylog_batch_rows  histogram   kudurru_querylog_write_duration_seconds  histogram
kudurru_querylog_db_bytes  gauge   kudurru_querylog_wal_bytes  gauge   kudurru_querylog_rows  gauge   kudurru_querylog_oldest_timestamp_seconds  gauge
kudurru_sink_sent_total{sink}  kudurru_sink_errors_total{sink}  kudurru_sink_queue_length{sink}  kudurru_sink_last_success_timestamp_seconds{sink}

# Cluster / store / API
kudurru_store_operations_total{op,result}  kudurru_store_operation_duration_seconds{op}  kudurru_cluster_members  kudurru_cluster_leader_changes_total
kudurru_api_requests_total{method,route,code}  kudurru_api_request_duration_seconds{route}  kudurru_auth_events_total{kind=login|logout|refresh|denied,method=oidc|token|basic}
kudurru_audit_events_total{result}
kudurru_health_check_duration_seconds / kudurru_health_check_failures_total   (CoreDNS self-probe)
```
Cardinality budget: with 20 zones × 6 protos × 20 qtypes the requests counter is 2 400 series per node; keep it there by dropping `zone` from `_duration_` and `_size_` histograms. Target < 10 k series per node total. Expose on a separate `:9153/metrics` listener (CoreDNS default) that is never on the DNS port; ship a `ServiceMonitor`/`PodMonitor` manifest and `metrics.auth: none|bearer`. Also serve `/metrics` in OpenMetrics with exemplars (trace ids on the latency histogram) when tracing is on.

### 3.3 Grafana dashboard outline ("kudurru / Overview", one dashboard, `node` and `cluster` variables)

1. **Header stat tiles**: QPS (`sum(rate(kudurru_dns_requests_total[1m]))`), blocked % (`sum(rate(...responses_total{status_class="blocked"}[5m]))/sum(rate(...requests_total[5m]))`), cache hit % , p95 latency (`histogram_quantile(0.95, sum by (le)(rate(kudurru_dns_request_duration_seconds_bucket[5m])))`), SERVFAIL rate, upstreams healthy `sum(kudurru_upstream_healthy)`/`count(...)`, leader (`kudurru_leader`), nodes up.
2. **Traffic row**: stacked QPS by `status_class`; QPS by `proto`; requests by `qtype` (top 10 + other); rcodes over time.
3. **Latency row**: heatmap of `kudurru_dns_request_duration_seconds_bucket`; p50/p95/p99 by `status_class`; upstream latency p95 by `upstream`.
4. **Blocking row**: block decisions by `source`; top lists by `kudurru_block_list_hits_total`; list freshness (`time() - kudurru_blocklist_last_update_timestamp_seconds`), list update failures.
5. **Cache row**: entries, hit/miss/stale, evictions, prefetches.
6. **Upstreams row**: table `upstream, healthy, rps, error %, p95`; errors by type.
7. **Zones / authoritative row**: queries by zone (top 10), transfers in/out, DNSSEC key expiry countdown, serial per node (detect divergence: `count by (zone)(count by (zone,kudurru_zone_serial)(kudurru_zone_serial)) > 1`).
8. **Pipeline row**: querylog drops by stage/sink, write duration, DB/WAL bytes, sink queue length and last success age.
9. **Cluster / API row**: store op latency, leader changes, API 5xx, auth denials, audit events.
10. **Front tier (dnsdist)**: `dnsdist_queries` rate, `dnsdist_server_status{server}`, `dnsdist_latency` histogram, `dnsdist_dyn_blocked`, `dnsdist_downstream_timeouts`, `dnsdist_pool_cache_hits` (import Grafana dashboard 13692 as a starting point) — placed on the same board so operators see dnsdist → kudurru → upstream in one view; keepalived VIP ownership as a stat (from a `keepalived_exporter` or a kudurru-exported `kudurru_vip_master{vip}` gauge driven by the keepalived notify script [design choice]).
Alert rules to ship: no leader > 2 m, SERVFAIL > 5 % for 5 m, upstream unhealthy > 2 m, querylog drops > 0 for 10 m, DB bytes > 90 % of cap, zone serial divergence > 10 m, DNSSEC key expiring < 7 d, p95 > 250 ms for 10 m, dnsdist backend down.

---

## 4. Health, readiness, audit logging, tracing

### 4.1 Probe semantics for a DNS pod (Kubernetes: startup → liveness/readiness; readiness failure removes the Pod from Service endpoints without restart; liveness failure restarts; startup gates both)

Endpoints on the ops listener (`:8081`, no auth, plain HTTP; CoreDNS uses `:8080/health` and `:8181/ready`; Gravity uses `/healthz/live|ready` on the metrics port):

| Endpoint | 200 when | 503 body | Use |
|---|---|---|---|
| `GET /healthz/startup` | config parsed, store opened/joined (or standalone), listeners bound | `{"not_ready":["store","listener:udp:53"]}` | `startupProbe` with generous `failureThreshold` (large zones / blocklists can take a minute) |
| `GET /healthz/ready` | startup ok **and** every role reports ready: zones loaded (all configured zones have a serial), blocklists either loaded or `blocking.allow_degraded=true` (serve with stale lists; expose `kudurru_blocklist_stale`), store is readable, for `secondary` zones the first AXFR/IXFR succeeded or an on-disk copy exists, and the self-query succeeded within 2 s. Upstream health is **not** a readiness condition (a node must keep answering authoritative/cached data when the Internet is down); it is a metric and a dashboard warning. During `lameduck` (pre-stop) readiness returns 503 while liveness still returns 200 (CoreDNS). | list of unready components | `readinessProbe` periodSeconds 5 |
| `GET /healthz/live` | process event loop responsive: an internal self-check sends an `A health.kudurru.internal.` query to its own UDP socket every second and records `kudurru_health_check_duration_seconds` (CoreDNS `overloaded.go`); live=200 unless the loop is deadlocked (no self-check completed in 30 s) or a fatal store error. Never fail liveness on load (Kubernetes docs: misconfigured liveness causes cascading restarts). | `{"reason":"self-check timeout"}` | `livenessProbe` failureThreshold 6 |
| `GET /healthz` | JSON roll-up `{status:"ok|degraded|fail", components:{...}, version, node, leader, uptime}` | — | humans, `kudurru health` CLI, Docker `HEALTHCHECK` |
| gRPC `grpc.health.v1.Health/Check` | same as ready | — | optional, for `grpc:` probes |

Also answer `CHAOS TXT id.server./version.bind./hostname.bind.` (NSID RFC 5001 on EDNS) so dnsdist health checks (`health_checks.qname: health.kudurru.internal.` per sibling dnsdist report) and humans can identify the node; dnsdist's `Server:setHealthCheckResponseValidator` (2.1) can verify the answer. keepalived `track_script` should call `kudurru health --ready --timeout 1s` (exit code) rather than a raw port check. Pre-stop: `kudurru lameduck 5s` → readiness 503 → dnsdist/kube-proxy stop sending → drain → exit.

### 4.2 Structured audit log of configuration changes

Every mutating API call / CRD reconciliation / CLI command produces one immutable `AuditEvent`:
```json
{"id":"01J9…ULID","ts":"2026-10-10T21:03:11.123Z","node":"kudurru-0","actor":{"kind":"user|token|operator|system","id":"oidc:sub|token-id|k8s:controller","name":"alice","groups":["admins"],"auth":"oidc|api_token|basic|mtls"},
 "action":"zone.record.update","resource":{"kind":"zone","id":"example.com","sub":"www A"},
 "result":"success|denied|error","reason":"rbac: missing zones:write",
 "diff":{"before":{"ttl":300,"rdata":"10.0.0.1"},"after":{"ttl":60,"rdata":"10.0.0.2"}},
 "request":{"id":"req-…","ip":"192.0.2.10","user_agent":"kudurru-cli/0.3","method":"PUT","path":"/api/v1/zones/example.com/records/www/A","trace_id":"4bf9…"},
 "prev_hash":"sha256:…","hash":"sha256:…"}
```
- Action vocabulary `<resource>.<verb>`: `auth.login/logout/token.create/token.revoke`, `config.update`, `zone.create/update/delete/transfer/sign`, `zone.record.create/update/delete`, `blocklist.create/update/delete/refresh`, `group.*`, `client.*`, `integration.*`, `querylog.privacy.change`, `querylog.delete`, `querylog.export`, `cluster.join/leave/promote`, `user.role.change`.
- Secrets are redacted (`"***"`), diffs are limited to 4 KiB with `truncated:true`.
- Storage: append-only `audit` table in a **separate** SQLite file (`audit.db`, WAL) with `(id TEXT PRIMARY KEY, ts, actor_id, action, resource_kind, resource_id, result, json BLOB, hash, prev_hash)` and indexes on `ts`, `actor_id`, `(resource_kind,resource_id)`; hash chain per node for tamper evidence; retention default 400 days; **also** emitted as a JSON line on stdout (`"log":"audit"`) so Kubernetes log pipelines pick it up, and optionally to the webhook/Loki/OTLP sinks and as Kubernetes `Event` objects on the owning CRD when running under the operator. In cluster mode audit rows replicate through the store so any node can serve `GET /api/v1/audit?actor&action&resource&from&until&cursor` (RBAC `audit:read`; `audit:export` for NDJSON).
- Technitium logs login/logout to its system log and Pi-hole has no audit trail; this is a differentiator that OIDC+RBAC make necessary.

### 4.3 OpenTelemetry tracing of one query: dnsdist → kudurru → upstream

Facts [V]: dnsdist 2.1.0 added experimental OpenTelemetry tracing (`setOpenTelemetryTracing(true)` / YAML `logging.open_telemetry_tracing.enabled`), per-query enablement with `SetTraceAction(true, {useIncomingTraceparent=, sendDownstreamTraceparent=, stripIncomingTraceparent=})`; trace/span ids travel in an **EDNS `TRACEPARENT` option, code 65500 by default** (`traceparent_edns_option_code`), per `draft-edns-otel-trace-ids` (also supported by PowerDNS Recursor ≥ 5.3.0); export via protobuf `RemoteLogResponseAction(..., delay=true)` or, since 2.2.0, **OTLP** (`remote_logging.otlp_loggers: [{name, address: http://collector:4318/v1/traces}]`, Lua `newOtlpLogger(url,{interval=1})`), `withTraceSpan(name, fn)` / `setSpanAttribute(k,v)` (2.2.0). OTel semconv DNS registry (Development): `dns.question.name` (string, as queried, no normalisation), `dns.answers` (string[] of resolved IPs); metric `dns.lookup.duration` (histogram, s, buckets `0.005…10`, required attr `dns.question.name`, conditional `error.type`).

Design:
1. **Ingress (dnsdist)**: `SetTraceAction` on a selector (e.g. 0.1 % sample via `ProbaRule(0.001)`, plus `RCodeRule(SERVFAIL)` on responses for tail sampling) with `sendDownstreamTraceparent=true`; OTLP logger to the collector. Spans: `dnsdist.query` with `dns.question.name`, `dns.question.type`, `network.peer.address`, `dnsdist.pool`, `dnsdist.backend`, `dnsdist.cache_hit`.
2. **kudurru** parses EDNS option 65500 (configurable `tracing.edns_option_code`, `tracing.accept_incoming: true` only from trusted dnsdist/proxy addresses — otherwise ignore to prevent trace-id spoofing), strips it from the client view, and starts a server span `kudurru.dns.query` (kind SERVER) with: `dns.question.name`, `dns.question.type`, `dns.rcode`, `kudurru.status`, `kudurru.zone`, `kudurru.list`, `network.transport`, `network.peer.address` (subject to privacy mode), `client.id` (group), `server.address`. Child spans (INTERNAL): `kudurru.acl`, `kudurru.blocking.match` (attr `list`, `rule_kind`), `kudurru.cache.lookup` (`hit`, `stale`), `kudurru.zone.lookup`, `kudurru.dnssec.validate`, and one CLIENT span per upstream attempt `kudurru.upstream.exchange` with `server.address`, `server.port`, `network.protocol.name=dot|doh|udp`, `dns.rcode`, `error.type`. When forwarding, kudurru re-adds the TRACEPARENT option only to upstreams flagged `trace_propagation: true` (another kudurru, PowerDNS Recursor, dnsdist); never to public resolvers.
3. **Export**: OTel Go SDK, OTLP gRPC/HTTP (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME=kudurru`, resource attrs `k8s.pod.name`, `kudurru.role`), sampler `parentbased_traceidratio` (default 0.001) + a local tail sampler that force-keeps spans with `error`, `status_class=error`, or latency > 250 ms. Head sampling is inherited from dnsdist's decision via the `traceparent` sampled flag (beta1 added the flags field).
4. **Correlation**: the 16-byte `trace_id` is stored in the query-log row and shown as a link in the UI (`tracing.ui_url_template: https://grafana/explore?...{trace_id}`); the Prometheus latency histogram gets exemplars with `trace_id`; logs carry `trace_id`/`span_id` fields.
5. Also emit the semconv metric `dns.lookup.duration` from the OTel meter when `tracing.metrics: true` for users on an OTel-only stack (do not make it the primary metrics path).

---

## 5. Multi-node: how stats aggregate across replicas

Reference behaviours: Pi-hole has **no** multi-server view at all; AdGuard Home none; blocky relies on Prometheus/Grafana (its `hostname` column records the instance so a shared Postgres can be queried cluster-wide); Technitium fans out every dashboard call with `node=cluster` and merges per-node top-N (`DashboardStats.Merge(stats, limit)`), while logs/stats stay per node and are not replicated (sibling technitium.md); Gravity writes 30 s metric samples into etcd (`/tsdb/<key>/<node>/<ts>`, 30 min lease) and charts from there (sibling gravity.md §tsdb — costly Raft writes, 30 min window).

kudurru design:
1. **Per-node truth, cluster-wide views.** Each node owns its `querylog.db` (raw + rollups). The API server (any node) answers `scope=cluster` by scatter-gather over the cluster membership list with a 2 s per-node timeout; partial results are returned with `nodes:[{name,ok:false,error}]` and `partial:true` rather than failing (Technitium logs and continues).
2. **Exact aggregates for additive data**: time-series and totals are sums of per-node rollups (same bucket boundaries everywhere: UTC-aligned minutes; latency histograms have identical bucket bounds so they add). Unique counts (`unique_domains`, `unique_clients`) are **union** sizes — nodes return the id sets for small windows (≤ 10 k) or HyperLogLog sketches for large ones; the response marks `approximate:true`.
3. **Top-N across nodes**: ask each node for `top n*4` (or `n+margin`), merge, re-rank; report `approximate:true` with `cutoff_rank` when any node's list was truncated (AdGuard's per-hour top-100 problem made explicit instead of hidden). For "exact" mode the API runs the aggregate from the optional central store (ClickHouse sink) when configured.
4. **Query search across nodes**: scatter-gather with per-node keyset cursors encoded into one opaque cluster cursor (`base64(json{node:id,...})`), k-way merge by `ts`; live tail multiplexes per-node SSE streams. When a ClickHouse sink exists, `source=central` searches it instead (cheaper for 30-day searches).
5. **Prometheus is the canonical cluster-wide time-series**: Grafana `sum by (…)` across the `node` label; the built-in dashboard uses rollups so it works without Prometheus, and the dashboard offers "open in Grafana" deep links when `observability.grafana_url` is set.
6. **Do not** store per-node metric samples in the replicated store (Gravity's `tsdb` pattern): it multiplies Raft writes by nodes × metrics and still cannot serve the query log. Replicate only configuration, zones, clients/groups, audit, and small "node status" heartbeats (`{node, version, roles, ready, qps_1m, last_seen}`) that power the Nodes page.
7. **Node identity**: `node.name` defaults to hostname / `POD_NAME`; it is a dictionary id in every table, a label in dnstap `identity`, Loki labels, ClickHouse rows and the API. Per-node vs cluster toggle is a first-class UI control (Technitium's `node=` selector), and every dashboard panel shows which nodes contributed.
8. **Dashboard freshness**: cluster summary caches per-node responses for 5 s; nodes answer summary from the hot ring + in-memory minute accumulators (no SQLite read) so a 10-node cluster summary costs 10 cheap RPCs.

---

## 6. What kudurru should borrow

1. **FTL's three tiers**: in-memory structures for the last 24 h, async flush to SQLite with WAL + `synchronous=NORMAL` + a 30 s completion grace, dictionary tables for strings, daily retention at a randomised night minute, weekly `ANALYZE`, and the id-cursor pagination contract with `records_total/filtered`, `earliest_timestamp`, suggestions, `disk=true` escape hatch and quick row actions.
2. **FTL's privacy levels** as ingest-time, irreversible modes (0–3) plus exclude filters; AdGuard's IP anonymisation (/24, /48) and `ignored` list; blocky's `ignore.sudn`.
3. **Technitium's bounded channel + 1 000-row transactions**, delete-in-10 000-row batches, `maxLogRecords`/`maxLogDays` dual retention, the `node=cluster` selector with per-node merge, numeric enum columns, `IDnsQueryLogger`/`IDnsQueryLogs` style plug-in surface for alternative log backends, and `LogExporterApp`'s file/HTTP/syslog targets.
4. **AdGuard's hourly stat units** idea — but as SQL rollup tables (minute/hour/day) rather than gob blobs, and report `approximate:true` when top-N merging is lossy; AdGuard's `StatsResp` field names are a good baseline for the summary JSON.
5. **blocky's sink abstraction** (`Writer{Write, CleanUp}`), bounded per-sink queues with drop counters and power-of-ten warnings, `flushInterval`, `fields` selection, eTLD+1 column, instance/hostname column, Timescale hypertable + retention policy for Postgres users, shipped Grafana JSON dashboards, and its honest cardinality notes.
6. **dnstap** as the native structured export (CLIENT_* and FORWARDER_* messages, `identity`/`version`/`extra`), CoreDNS's 10 000-deep queue + listener mode + `extra` metadata templating, Unbound's per-message-type switches and `dnstap-sample-rate`, dnsdist's frame-stream logger options; point users at DNS-collector for ClickHouse/Loki/Kafka.
7. **CoreDNS metric shapes** (`_requests_total{proto,family,type,zone}`, `_request_duration_seconds` native histogram, `_responses_total{rcode,plugin}`, bounded `type` list + `other`, `zone="dropped"`), its `/health` self-probe metric, `/ready` with the list of unready plugins and `lameduck`.
8. **dnsdist's observability surface** — `/metrics`, `/jsonstat`, `/api/v1/servers/localhost` object model (frontends/pools/servers/rules), rings endpoint, per-backend health-check failure breakdown — and its OpenTelemetry EDNS TRACEPARENT propagation (option 65500) + OTLP export, so a single trace spans dnsdist → kudurru → upstream.
9. **Gravity's split of ops port vs API port** (`/healthz/live|ready` + `/metrics` on a non-DNS, non-API listener) and per-role log levels.
10. **Prometheus naming/cardinality rules** (prefix, base units, `_total`, `_info`, no unbounded labels) and the semconv attribute names `dns.question.name`, `dns.answers`, `error.type`.

## 7. What kudurru should avoid

1. **Copying code or schema text from FTL (EUPL), AdGuard Home (GPL-3) or Technitium (GPL-3)** — kudurru is source-available (PolyForm Noncommercial; Apache-2.0 when this note was written); re-derive designs (this report describes behaviour, not code).
2. **Indexing every column** (FTL memdb, Technitium's 13 indexes incl. an 8-column composite): write amplification and disk growth with little query benefit; use 3–4 indexes plus rollups and a partial index for blocked rows.
3. **Full-table scans for long-term dashboards** (FTL `database_*` endpoints) and **OFFSET pagination** (Technitium `LIMIT/OFFSET` after `COUNT(*)`); use rollups and keyset cursors.
4. **Text-only query logs** (AdGuard NDJSON with binary-searched timestamps, blocky CSV) as the primary store — no indexes, no aggregation, 50 000-line scan limits, interval-based not size-based rotation.
5. **Lossy binary stat blobs truncated to top-100/1000** (AdGuard units, Technitium `.stat` files) presented as exact; if truncation is unavoidable, surface `approximate`.
6. **Unbounded metric labels** (blocky `client`, AdGuard exporter `domain`/`client` gauges, pihole6_exporter `query_client_1m`): never label by client/domain by default.
7. **Non-standard metric names** (dnsdist counters without `_total`, Technitium unprefixed `queries_total`), per-24 h gauges masquerading as counters (exporters scraping `/stats`), and milliseconds instead of seconds.
8. **Metrics/health on the admin API port behind auth** (dnsdist requires API key/basic auth on `/metrics` by default; Technitium's health endpoint lives on the admin API port at `/api/dnsClient/healthCheck`, loopback-unauthenticated only since 15.5): keep probes and scrapes unauthenticated on a dedicated ops listener, bound to the pod/cluster network.
9. **Blocking the DNS path on logging** (synchronous DB writes, unbounded channels): every hand-off must be non-blocking with drop accounting.
10. **Liveness tied to upstream reachability or load**; **readiness tied to the Internet** — a DNS pod must keep serving zones and cache when upstreams are down.
11. **Per-node metrics written into the replicated store** (Gravity `tsdb`): keep Raft for config, use Prometheus/ClickHouse for time series.
12. **Polling-only live views** (Pi-hole 2 s polling): provide SSE/WebSocket with a bounded per-client buffer and heartbeats.
13. **Trusting inbound EDNS TRACEPARENT from arbitrary clients** (trace-id injection); accept only from configured proxies, strip before forwarding to public upstreams.
14. **Reversible or output-time-only privacy** (AdGuard's query log anonymises client IPs only when serving the API — the NDJSON file still holds raw IPs; FTL keeps `privacylevel` per row): anonymise at ingest so sinks and backups never contain what the operator chose to hide — and keep the raw-dnstap switch separate and audited.

## 8. Open questions for the architecture document

- Single SQLite per node vs. one DB per day (easier retention by file drop, harder cross-day queries). Recommendation: single file + batched deletes + `incremental_vacuum`; revisit if `max_db_bytes` > 50 GB becomes a real use case.
- Whether to store an `answer` summary at all by default (privacy and size) — AdGuard stores full packed answers, Pi-hole stores none. Proposal: store first 3 RRs truncated to 255 chars only in `privacy: full`.
- Hot ring vs. in-memory SQLite (FTL): the ring + accumulators are simpler and faster; the trade-off is that complex filtered queries over the last 24 h hit SQLite (WAL readers are cheap).
- dnstap message types to emit for authoritative answers: `AUTH_QUERY/AUTH_RESPONSE` when the zone is local vs `CLIENT_*` when resolving/forwarding — choose by role of the answer, and document it, since DNS-collector keys dashboards on `type`.
- Exact cluster-wide top-N: require the ClickHouse sink, or add a lightweight "merge job" that pulls per-node `rollup_domain_1h` into the leader nightly? Proposal: optional nightly pull into the leader's `rollup_*_1d` tables keyed by node (small), exact for days, approximate for sub-day windows.
- Whether to ship a tiny bundled OTel collector config / Grafana provisioning in the Helm chart.
