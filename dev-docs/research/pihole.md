> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [dev-docs/design-record.md](../design-record.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# Pi-hole v6 — research report for kudurru

Key: `pihole` · Date: 2026-10-10 · Author: research subagent
Scope: Pi-hole v6 (FTL, web, core, docs) with focus on *why* the dashboard is loved, exactly what it shows, the v6 REST API, storage/stat algorithms, web stack, auth, config model, and community criticism.

## 0. Sources, versions, licences

| Repo | Clone @ HEAD (depth-1, 2026-10-10) | Latest tag / release | Licence |
|---|---|---|---|
| `github.com/pi-hole/FTL` | `0bf029b` 2026-09-19 "Merge pull request #3105 from pi-hole/release/v6.7.1" | **FTL v6.7.1** (2026-09-19) | **EUPL-1.2** (`LICENSE`) |
| `github.com/pi-hole/web` | `b2a4078` 2026-07-06 "Web v6.6 (#3808)" | **Web v6.6** (2026-07-06) | EUPL-1.2 (`LICENSE`, `package.json: "license": "EUPL-1.2"`) |
| `github.com/pi-hole/pi-hole` (core) | `f47b8ed` 2026-07-06 "v6.4.3 (#6618)" | **Core v6.4.3** | EUPL-1.2 |
| `github.com/pi-hole/docs` | `d4394ca` 2026-10-10 | n/a | CC BY-SA 4.0 |
| Docker image `pihole/pihole` | — | tag `2026.07.0`/`2026.07.2` (July 2026); `2026.09.0` reported by a third party for FTL 6.7.1 (UNVERIFIED) | — |

Milestones (primary: pi-hole.net blog + discourse):
- 2025-02-18 — "Introducing Pi-hole v6": Core/FTL/Web v6.0, Docker `2025.02.0`. lighttpd+PHP removed; REST API + web server + Lua embedded in `pihole-FTL`; `pihole.toml`; native HTTPS; Antigravity (subscribed allowlists); Alpine Docker image. "The upgrade is strictly a one-way operation." (https://pi-hole.net/blog/2025/02/18/introducing-pi-hole-v6/)
- 2026-07-06 — FTL v6.7, Web v6.6, Core v6.4.3: six security advisories incl. **RCE via CivetWeb configuration injection (GHSA-8j7w-m3cr-6q6x)**, session-expiration bypass, missing rate limiting; embedded dnsmasq v2.93, SQLite 3.53.1; reworked static-DHCP-lease editor. (https://discourse.pi-hole.net/t/pi-hole-ftl-v6-7-web-v6-6-and-core-v6-4-3-released/86679)
- 2026-09-19 — FTL v6.7.1: closes further root paths via `webserver.advancedOpts` and `misc.dnsmasq_lines`; both now settable only from `pihole.toml`/env, not API/CLI (confirmed in source: `FLAG_API_READ_ONLY` / `FLAG_API_CLI_READ_ONLY` in `src/config/config.h`). Date/scope from a third-party blog; the commit date in the clone confirms the 2026-09-19 release merge.

Embedded components (verified in source tree): dnsmasq fork `pi-hole-v2.93` (`CMakeLists.txt: set(DNSMASQ_VERSION pi-hole-v2.93)`), SQLite3 amalgamation (`src/database/sqlite3.c`), CivetWeb (`src/webserver/civetweb/`), cJSON, Lua 5.4 (`src/lua/`), tomlc17 (`src/config/tomlc17/`), TRE regex, miniz (teleporter zip), nettle (balloon hash, HMAC-SHA1, base64).

**License constraint for kudurru:** EUPL-1.2 is a copyleft license. Its compatibility appendix lets EUPL code be *relicensed into* GPL/AGPL/MPL/etc., but **Apache-2.0 is not on the list**, and EUPL-1.2 derivative works must remain EUPL (or a compatible license). kudurru (Kudurru Community License; Apache-2.0 when this note was written) must **not copy Pi-hole source, templates, CSS or OpenAPI YAML**. API *shapes*, data-model ideas and UX patterns are not copyrightable expression; re-derive them.

---

## 1. Architecture at a glance (v6)

```
┌──────────────────────── pihole-FTL (single C binary, EUPL) ────────────────────────┐
│ dnsmasq fork ─┬─ hooks (dnsmasq_interface.c) ──► in-memory stats (POSIX shm)       │
│               │                                   queries/clients/domains/upstreams│
│               │                                   overTime[145] ring, counters      │
│               ├─ gravity.db (SQLite) ◄─ blocking decisions, groups, lists          │
│               └─ memdb (:memory: SQLite "query_storage") ──(every 60 s)──► disk DB │
│ CivetWeb HTTP(S) ─► /api/* (C handlers, cJSON) + Lua Server Pages (*.lp) + static  │
│ NTP server/client, DHCP (dnsmasq), TOML config, inotify, GC thread, DB thread      │
└────────────────────────────────────────────────────────────────────────────────────┘
      ▲                                      ▲
      │ pihole.toml + FTLCONF_* env           │ /admin/*.lp  (web repo: jQuery/AdminLTE)
      │ pihole-FTL --config key value         │ browser JS calls /api/* with sid cookie+CSRF
core (bash): `pihole` CLI, gravity.sh (list fetch/parse into gravity.db), installer, manpages
```

Key design facts:
- **"Fast": stats are read directly from memory by coupling our codebase closely with dnsmasq** (FTL README). Every query event is a C hook inside the resolver; there is no log parsing in v6 (v4 parsed `pihole.log`).
- The web UI is **static Lua templates + jQuery**; all data comes from the JSON API, so the UI and third-party clients are symmetric. OpenAPI 3.0.2 spec (≈9.1k lines, `src/api/docs/content/specs/*.yaml`) is **compiled into the binary** and served at `/api/docs` with RapiDoc.
- The core repo is still bash: `gravity.sh` downloads/parses lists into `gravity.db`; the `pihole` CLI mostly wraps the API (`pihole api <endpoint>` exists) and `pihole-FTL --config`.

---

## 2. Why the dashboard is beloved — extracted UX principles

Reading the templates/JS and community threads, the appeal reduces to these principles (each mapped to a concrete mechanism):

1. **Four big numbers answer "is it working?" in one second.** Total Queries / Queries Blocked / Percentage Blocked / Domains on Lists — colored AdminLTE "small-box" tiles, each with a footer link that *drills into the query log pre-filtered* (`queries?upstream=blocklist`, `queries`, `groups-lists`, `network`).
2. **Everything is a link into the query log.** Top-domain rows → `queries?domain=X&upstream=permitted|blocklist`; top-client rows → `queries?client_ip=X[&upstream=blocklist]`; clicking a bar in the 24 h chart → `queries?from=T-300&until=T+300`. The log page reads these GET params and pre-populates its filters.
3. **Blocked vs permitted is the universal color code.** CSS classes `queries-blocked` (red), `queries-permitted` (green), `queries-cached`, `queries-other`; status icons in the log; `blocked-row`/`allowed-row` row tints; progress bars in top lists use the same classes. Themes only re-skin these semantic classes.
4. **Live, but cheap.** Each widget polls its own endpoint at a tuned interval (summary 1 s, query log 2 s when live, blocking status 10 s, system 20 s, pies/top lists 60 s, history 10 min). Summary spikes (count doubled & >100) trigger a refresh of the heavy charts. Failure backoff ×3; chart retries ×0.1 up to 5 times.
5. **Act from the row.** Every query-log row has an "Allow"/"Deny" button; one click POSTs `/api/domains/{allow|deny}/exact` with `comment: "Added from Query Log"`, shows a modal with spinner→check, and updates sidebar counters.
6. **Explain each decision.** Expanding a log row shows: exact receive time (ms), client name+IP, DNSSEC status (lock icons), EDE text, *why* (Blocked (gravity) / Blocked (regex) / Forwarded, reply from X / Served from cache / Served by cache optimizer / CNAME inspection chain), matched list link ("search lists"), TTL, reply type, DB id.
7. **Sidebar always shows liveness**: Status dot (Active / Blocking disabled / DNS server failure), queries-per-second, CPU, memory, plus counts badges (groups, clients, allow/deny rules, lists, gravity size) refreshed every 2 min.
8. **Temporary disable with a visible countdown** (10 s / 30 s / 5 min / custom / indefinitely) and the sidebar label turns into "Enable Blocking (0:04:59)".
9. **Progressive disclosure in settings**: Basic/Expert toggle per settings page; an "All settings" page (expert only) with an "only changed" filter; each control carries `data-configkeys="dns.dnssec"` so UI ↔ TOML key is 1:1.
10. **Self-hosted API docs** at the exact version running, and errors in a uniform `{error:{key,message,hint}}` envelope with human hints.
11. **Diagnosis messages** (`/api/info/messages`) surface resolver/health problems as a badge in the navbar instead of expecting users to read logs.
12. **Minimal mental model**: Groups → (Clients, Domains, Lists). Two list types (allow/block), two domain kinds (exact/regex). Nothing else to learn.

---

## 3. The dashboard, exactly (web `index.lp` + `scripts/js/index.js`)

### 3.1 Stat tiles (row 1)

| Tile | Element | Source | Footer link |
|---|---|---|---|
| Total Queries (bg-aqua) | `#dns_queries`; footer "N active clients" (`#active_clients`, title "M total clients") | `GET /api/stats/summary` → `queries.total`, `clients.active`, `clients.total` | `network` |
| Queries Blocked (bg-red) | `#blocked_queries` | `queries.blocked` | `queries?upstream=blocklist` |
| Percentage Blocked (bg-yellow) | `#percent_blocked` (1 decimal) | `queries.percent_blocked` | `queries` |
| Domains on Lists (bg-green) | `#gravity_size`; tooltip "Lists updated 3 hours ago (date)" or "Error! Update gravity" if negative | `gravity.domains_being_blocked`, `gravity.last_update` | `groups-lists` |

Also from summary: `queries.frequency` (QPS) is written into the sidebar.

### 3.2 "Total queries" chart (24 h, 10-minute buckets)
- Chart.js **stacked bar**, x = time axis (moment adapter, `HH:mm`), 4 datasets in fixed order: *Other*, *Blocked*, *Cached*, *Forwarded* (`other = total - (blocked+cached+forwarded)`).
- Data: `GET /api/history` → `history[]` of `{timestamp,total,cached,blocked,forwarded}`; 145 slots (`OVERTIME_SLOTS = 24*3600/600 + 1`).
- Tooltip title "Queries from 13:10:00 to 13:19:59"; each label shows count and % of the bucket; zero rows hidden.
- **chartjs-plugin-zoom**: Ctrl+wheel / pinch zoom on y only, pan y, min range 10, "Reset zoom" button appears when zoomed.
- Click a bar → `queries?from=&until=` (±300 s around bucket center).
- Chart removed entirely when `history` is empty (fresh install or privacy level).

### 3.3 "Client activity" chart
- Same stacked bar, one dataset per client (top N by total; N = `webserver.api.maxClients` default 10, `?N=` override), colors from `THEME_COLORS[16]` then random.
- Data: `GET /api/history/clients` → `{clients:{ip:{name,total}}, history:[{timestamp,data:{ip:count}}]}`; last pseudo-client `"0.0.0.0"` named `"other clients"` aggregates the remainder.
- `webserver.api.client_history_global_max=true` ranks clients once globally; `false` re-ranks per slot.

### 3.4 Pies (doughnuts) — "Query Types" and "Upstream servers"
- `GET /api/stats/query_types` → `{types:{A,AAAA,ANY,SRV,SOA,PTR,TXT,NAPTR,MX,DS,RRSIG,DNSKEY,NS,SVCB,HTTPS,OTHER}}`; zero types dropped; values converted to %.
- `GET /api/stats/upstreams` → `{upstreams:[{ip,name,port,count,statistics:{response,variance}}], forwarded_queries, total_queries}`; label = `name||ip` + `#port`; includes pseudo-upstreams for cache/blocklist (port -1). Legend is custom HTML (`htmlLegendPlugin`) with hover-to-highlight, click-to-hide, and click-through to `queries?upstream=…`.
- Pies update with `chart.update("none")` to avoid re-animation.

### 3.5 Top lists (four tables: Domain|Hits|Frequency and Client|Requests|Frequency)
- `GET /api/stats/top_domains[?blocked=true]` → `{domains:[{domain,count}], total_queries, blocked_queries}`; `GET /api/stats/top_clients[?blocked=true]` → `{clients:[{ip,name,count}], …}`. Default `count=10`.
- "Frequency" = `utils.colorBar(percentage, sum, cssClass)` — a mini progress bar titled "12.3% of 4567".
- Privacy level ≥1 removes domain tables, ≥2 removes client tables; otherwise "- No data -" row.
- `webserver.api.excludeDomains` / `excludeClients` (regex arrays) hide entries from top lists and query log.

### 3.6 Refresh cadence (web `footer.js`)
```js
const REFRESH_INTERVAL = {
  logs: 500, summary: 1000, query_log: 2000, blocking: 10_000, metrics: 10_000,
  system: 20_000, query_types: 60_000, upstreams: 60_000, top_lists: 60_000,
  messages: 60_000, version: 120_000, ftl: 120_000, hosts: 120_000,
  history: 600_000, clients: 600_000 };
```
(Community noticed top lists went from 10 s in v5 to 60 s in v6.)

### 3.7 Navigation (sidebar.lp)
Main: Dashboard, Query Log · Group Management: Groups, Clients, Domains, Lists (with count badges) · DNS Control: Disable Blocking (Indefinitely / 10 s / 30 s / 5 min / Custom) · System: Settings (System, DNS, DHCP, Web interface/API, Privacy, Teleporter, Local DNS Records, All settings*) · Tools (Pi-hole diagnosis, Tail log files (pihole.log/FTL.log/webserver.log), Update Gravity, Search Lists, Interfaces, Network) · Donate. Navbar shows hostname and a warning-count badge.

---

## 4. Query log (web `queries.lp` + `queries.js`)

**Table** (DataTables, server-side processing, `stateSave` of page length, default 25, menu 10/25/50/100/500/1000, sorted by time desc):

| # | Column | Render |
|---|---|---|
| 0 | Time | `Y-MM-DD HH:mm:ss z` (date wraps on small screens) |
| 1 | (status icon) | fa icon + color from status (see 4.2); row gets `blocked-row`/`allowed-row` |
| 2 | Type | A/AAAA/… (`TYPE66` for unknown) |
| 3 | Domain | DNSSEC lock icon prefix if `dns.dnssec` on; CNAME-blocked shows "(blocked <cname>)"; footer input = column search (wildcard `*`, `_` single char) |
| 4 | Client | hostname if known else IP; IP-aware sort; footer search matches ip OR name |
| 5 | ⏱ reply time | µs / ms / s auto-unit; "✕" if no reply |
| 6 | Action | **Allow** (green) for blocked statuses; **Deny** (red) for FORWARDED/CACHE/CACHE_STALE/IN_PROGRESS; none for external/special |

**Advanced filtering box** (collapsed): date-time range picker (5-min increments; presets Last 10 Minutes, Last Hour, Today, Yesterday, Last 7/30 Days, This Month, Last Month, This Year, All Time; min = earliest timestamp on disk/in memory), checkbox **"Query on-disk data — a lot slower but necessary for >24 h; disables live update"**, and 8 select2 filters: Domain*, Client (by IP)*, Client (by name)*, Upstream*, Type, Status, Reply, DNSSEC status (* = free text with wildcards). Options come from `GET /api/queries/suggestions` → `{suggestions:{domain[],client_ip[],client_name[],upstream[],type[],status[],reply[],dnssec[]}}`.

**Live update**: checkbox "Live update" → re-fetch every 2 s with `cursor=null` (newest first). "Refresh" applies filters. Note on page: "Queries for pi.hole and the hostname are never logged."

**Row expansion** (click): receive time with ms, client, DNSSEC, EDE text, status text, CNAME chain, "Query was blocked/allowed, search lists" link (`search?domain=`), TTL (humanized), reply type, DB id.

### 4.1 `GET /api/queries` contract (OpenAPI `queries.yaml` + `api/queries.c`)
Params: `from`, `until` (unix seconds), `length` (default 100; capped at `API_QUERIES_MAX_ROWS`), `start` (offset), `cursor` (DB id of newest row to show; **stable pagination across live inserts**), `domain`, `client_ip`, `client_name`, `upstream`, `type`, `status`, `reply`, `dnssec`, `disk=true` (query the attached on-disk DB instead of memdb), plus DataTables-style `draw`, `order[0][column]`, `columns[3][search][value]` (domain), `columns[4][search][value]` (client ip or name; `*`→`%`, auto-wrapped in `%…%`, `ESCAPE '\'`).
Special `upstream` values: `cache`, `blocklist`, `permitted` (used by dashboard drill-downs).
Response:
```json
{"queries":[{"id":112421354,"time":1581907991.539,"type":"A","domain":"community.stoplight.io",
  "cname":null,"status":"FORWARDED","client":{"ip":"192.168.0.14","name":"desktop.lan"},
  "dnssec":"INSECURE","reply":{"type":"IP","time":19},"list_id":null,
  "upstream":"localhost#5353","ede":{"code":0,"text":null}}],
 "cursor":175881,"recordsTotal":1234,"recordsFiltered":1234,"draw":1,
 "earliest_timestamp":1581900000.0,"earliest_timestamp_disk":1580000000.0,"took":0.003}
```
Implementation: one SQL over `query_storage q LEFT JOIN domain_by_id d, client_by_id c, forward_by_id f, addinfo_by_id a` with bound params; the JSON is **streamed row by row** ("prevents /api/queries from exhausting memory"), regex exclusion filters applied in C while streaming.

### 4.2 Status taxonomy (enum `query_status`, DB ints 0–18)
UNKNOWN(0) · GRAVITY(1) · FORWARDED(2) · CACHE(3) · REGEX(4) · DENYLIST(5) · EXTERNAL_BLOCKED_IP(6) · EXTERNAL_BLOCKED_NULL(7) · EXTERNAL_BLOCKED_NXRA(8) · GRAVITY_CNAME(9) · REGEX_CNAME(10) · DENYLIST_CNAME(11) · RETRIED(12) · RETRIED_DNSSEC(13) · IN_PROGRESS(14) · DBBUSY(15) · SPECIAL_DOMAIN(16) · CACHE_STALE(17) · EXTERNAL_BLOCKED_EDE15(18).
Reply types 0–13: UNKNOWN, NODATA, NXDOMAIN, CNAME, IP, DOMAIN, RRNAME, SERVFAIL, REFUSED, NOTIMP, OTHER, DNSSEC, NONE, BLOB. DNSSEC: UNKNOWN, SECURE, INSECURE, BOGUS, ABANDONED, TRUNCATED. Query types 1–16 (A…HTTPS) with unknown RR types stored as `100+rrtype`.
The web maps each status to {text, icon, color, action button, isCNAME, blocked}: e.g. "Blocked (gravity)", "Forwarded, reply from 1.1.1.1#53", "Served from cache", "Served by cache optimizer", "Already forwarded, awaiting reply", "Blocked (external, EDE15)".

---

## 5. Long-term data

- **API exists**: `/api/history/database?from&until` (10-min buckets computed by SQL `GROUP BY (timestamp/600)*600,status`), `/api/history/database/clients`, `/api/stats/database/{summary,top_domains,top_clients,query_types,upstreams}?from&until[&blocked]` (SQL `COUNT(*) … GROUP BY q.domain ORDER BY cnt DESC LIMIT :count`).
- **UI dropped**: `grep stats/database|history/database web/scripts/js` → no hits. v5's "Long-term data → Graphics / Query Log / Top Lists" pages were **not ported**. Only the unified Query Log with the date range + "on-disk" checkbox remains.
- Community: discourse thread "Long-term data graphics and top lists in v6" (Feb–Jul 2025): "I use the graphs to find anomalies", "I can not update to v6, if there is no long-term data!"; DL6ER (2025-07-15): API "rewritten from scratch", no core dev working on it, contributions welcome. Third-party fill-ins: PiHoleLongTermStats (Dash/Plotly over the DB file), Grafana + pihole-exporter.

---

## 6. Group management data model (gravity.db, schema v20)

From `pi-hole/advanced/Templates/gravity.db.sql` (verbatim semantics, not copied text):

| Table | Columns | Notes |
|---|---|---|
| `group` | id, enabled, name UNIQUE, date_added, date_modified, description | row 0 = "Default", recreated by trigger if deleted |
| `domainlist` | id, type (0 allow-exact, 1 deny-exact, 2 allow-regex, 3 deny-regex), domain, enabled, dates, comment; UNIQUE(domain,type) | |
| `adlist` | id, address, enabled, dates, comment, date_updated, number, invalid_domains, status, abp_entries, type (0 block, 1 allow=antigravity); UNIQUE(address,type) | status 1 OK, 2 unchanged/local copy, 3 unavailable/local copy, 4 invalid |
| `gravity` / `antigravity` | domain, adlist_id | the parsed list contents |
| `client` | id, ip (IP, CIDR, MAC, hostname, or `:iface`), dates, comment | |
| `*_by_group` | (adlist_id|domainlist_id|client_id, group_id) PK, ON DELETE CASCADE | insert triggers auto-add group 0 |
| `info` | property/value | version, gravity_restored |
| views | `vw_allowlist`, `vw_denylist`, `vw_regex_allowlist`, `vw_regex_denylist`, `vw_gravity`, `vw_antigravity`, `vw_adlist` | each joins group enabled flags so FTL queries "domain X in group G" directly |

Semantics (docs/group_management): a domain/list is active for a client iff both are enabled, share a group, and the group is enabled. Removing a client from Default = unblocked. Blocking logic runs per client-group-set; FTL caches per (domain, client, qtype) in `DNSCacheData`.

**Per-client group resolution order** (`gravity-db.c: get_client_groupids`):
1. `SELECT … subnet_match(ip,?) … ORDER BY matching_bits DESC LIMIT 1` — exact IP or **longest-prefix CIDR**; warns if two subnets tie.
2. If none and `resolver.macNames`: MAC from network table (or in-memory ARP) → `client WHERE ip = <MAC> COLLATE NOCASE`.
3. Hostname (in-memory name or PTR) → `client WHERE ip = <hostname>`.
4. Interface → `client WHERE ip = ':eth0'`.
Result cached on the client object; `FTL_reload_all_domainlists()` on changes (`pihole reloadlists` / API write triggers).

**API** (`/api/{groups,clients,domains,lists}`): uniform CRUD — `GET collection|item`, `POST collection` (item may be an array: `{"domain":["a.com","b.com"]}` → `processed:{success[],errors[]}`), `PUT item` (replace; can rename/move type/kind), `DELETE item` (204), `POST …:batchDelete` with array body. Objects carry `comment`, `groups:[int]`, `enabled`, read-only `id`, `date_added`, `date_modified` (lists add `date_updated`, `number`, `invalid_domains`, `abp_entries`, `status`; domains add `unicode`, `type`, `kind`). Domains path grammar: `/api/domains/{allow|deny}/{exact|regex}/{domain}`. `GET /api/clients/_suggestions` lists unconfigured seen devices `{hwaddr, macVendor, lastQuery, addresses, names}` to populate the "Known clients" dropdown.

**Web pages**: Groups (Name, Comment; table ID/toggle/Name/Status/Comment), Clients (Known clients select + Comment + multi-select Group assignment; table ID/toggle/Client/Comment/Groups), Domains (tabs Domain | RegEx; "Add domain as wildcard" converts to regex; buttons "Add to denied domains" / "Add to allowed domains"; 4 type filter checkboxes), Lists (Address + Comment + Groups; add as Block or Allow; table with status icon, health text, entries/ABP/invalid counts, last update).

---

## 7. Local DNS records, CNAMEs, and other dnsmasq-backed config

Not a table — **arrays in `pihole.toml`**, edited through generic array endpoints:
```toml
[dns]
  hosts = [ "1.1.1.1 abc-custom.com def-custom.de" ]      # HOSTS form "IP NAME [NAME…]"
  cnameRecords = [ "brücke.com,äste.com,2", "pihole.mydomain.net,pi.hole" ]  # "<cname>,<target>[,TTL]"
  hostRecord = ""         # "<name>[,<name>…],[IPv4],[IPv6][,TTL]"
  revServers = [ "true,192.168.0.0/24,192.168.0.1,fritz.box" ]  # conditional forwarding
  [dns.domain]  name = "lan"  local = true
```
Web page "Local DNS Records" has two tables (Domain|IP; Domain|Target|TTL) with inline add rows; JS does `PUT /api/config/dns/hosts/<urlencoded "ip name">` and `DELETE /api/config/dns/cnameRecords/<item>`. Footer notes: hosts changes flush cache without restart; CNAME changes **restart the DNS server**; CNAME target must be a name Pi-hole already knows (no upstream chase).

---

## 8. "Disable blocking for N" control

`GET /api/dns/blocking` → `{"blocking":"enabled|disabled|failed|unknown","timer":12.5|null}`.
`POST /api/dns/blocking` body `{"blocking":false,"timer":300}`; timer null = permanent; posting the same state resets/clears the timer. FTL keeps one timer (`set_blockingmode_timer`) that flips `dns.blocking.active` back. UI: sidebar submenu + custom modal; countdown label; status polled every 10 s. CLI: `pihole disable 5m`.

---

## 9. FTL REST API v6 — conventions and route table

Base `http(s)://pi.hole/api`, served by FTL itself (requests to `/admin/api` get a 400 with hint "The API is hosted at pi.hole/api, not pi.hole/admin/api"). Spec `info.version: "6.0"`, tags: Authentication, Metrics, DNS control, DHCP server control, Group/Domain/Client/List management, FTL information, Pi-hole configuration, Network information, Actions, PADD.

Conventions:
- Every JSON reply carries `"took": <seconds>`.
- Errors: `{"error":{"key":"bad_request|unauthorized|database_error|…","message":"…","hint":<string|object|null>}}`; 400/401/403/404/429/500. Example hint object: `{"argument":"abc.com","enabled":true,"sql_msg":"UNIQUE constraint failed: domainlist.domain, domainlist.type"}`.
- Methods: GET read, POST create (201 + `Location` header), PUT create-or-replace, PATCH partial update (config only), DELETE (204). Collection-wide delete via `POST /x:batchDelete`.
- Auth required on all routes except `/api/auth` (GET/POST), `/api/info/client`, `/api/info/login`, `/api/docs`, `/api/endpoints`(auth) …; `GET /api/endpoints` lists the route table.

Route table (from `src/api/api.c`):
| Area | Routes |
|---|---|
| Auth | `GET/POST/DELETE /auth`, `GET /auth/sessions`, `DELETE /auth/session/{id}`, `GET /auth/app`, `GET /auth/totp` |
| DNS control | `GET/POST /dns/blocking` |
| Lists | `/clients[/{client}]`, `/clients/_suggestions`, `/clients:batchDelete`; `/domains[/{type}[/{kind}[/{domain}]]]`, `/domains:batchDelete`; `/groups[/{name}]`, `/groups:batchDelete`; `/lists[/{list}]?type=`, `/lists:batchDelete`; `GET /search/{domain}` |
| Metrics (memory) | `/stats/summary`, `/stats/query_types`, `/stats/upstreams`, `/stats/top_domains`, `/stats/top_clients`, `/stats/recent_blocked`, `/history`, `/history/clients`, `/queries`, `/queries/suggestions` |
| Metrics (disk) | `/stats/database/{summary,query_types,upstreams,top_domains,top_clients}`, `/history/database`, `/history/database/clients` (all need `from`&`until`) |
| Info | `/info/client`, `/info/login`, `/info/system`, `/info/database`, `/info/sensors`, `/info/host`, `/info/ftl`, `/info/version`, `/info/messages[/{id}]`, `/info/messages/count`, `/info/metrics` |
| Logs | `/logs/dnsmasq`, `/logs/ftl`, `/logs/webserver` (FIFO tail, `?nextID=`) |
| Config | `GET/PATCH /config[?detailed=true]`, `GET /config/{element}`, `PUT/DELETE /config/{element}/{value}` (array items), `GET /config/_properties` |
| Network | `/network/gateway`, `/network/routes`, `/network/interfaces`, `/network/devices[/{id}]` |
| DHCP | `GET /dhcp/leases`, `DELETE /dhcp/leases/{ip}` |
| Actions | `POST /action/gravity` (streams gravity.sh output), `/action/restartdns`, `/action/flush/logs`, `/action/flush/arp`, `/action/flush/network` |
| Backup | `GET/POST /teleporter` (zip) |
| Misc | `GET /padd` (compact JSON for the terminal dashboard), `GET /docs` |

`/api/stats/summary` shape (verbatim fields): `queries{total,blocked,percent_blocked,unique_domains,forwarded,cached,frequency,types{…},status{…},replies{…}}`, `clients{active,total}`, `gravity{domains_being_blocked,last_update}`.
`/api/info/ftl` includes `database{gravity,antigravity,groups,lists,clients,domains{allowed{exact{total,enabled},regex{…}},denied{…}}}`, `privacy_level`, `clients{total,active}`, `allow_destructive`, `%cpu/%mem`, pid, uptime — this feeds the sidebar badges.
`/api/padd` bundles: blocking, queries{total,blocked,percent_blocked,query_frequency}, active_clients, gravity_size, top_domain/top_blocked/top_client, recent_blocked, cache{size,inserted,evicted}, system (cpu/mem/uptime), host_model, iface{v4,v6,rx/tx bytes}, version, config{dhcp_*,dns_port,dns_num_upstreams,dns_dnssec,privacy_level}, sensors — a single call for a kiosk/TUI.

Teleporter zip contains JSON exports of gravity tables (`group.json`, `client.json`, `adlist.json`, `blacklist.exact.json`, `whitelist.regex.json`, `*_by_group.json`…), `pihole.toml`, `dhcp.leases`, and legacy files; `POST /api/teleporter` accepts multipart with an `import` JSON selecting which tables to restore and triggers an FTL restart. This is the mechanism nebula-sync/orbital-sync use to replicate config between Pi-holes.

---

## 10. How FTL stores history and computes stats cheaply

### 10.1 In-memory layer (POSIX shared memory, `shmem.c`, `datastructure.h`)
Shared between the dnsmasq process and FTL threads/forks; guarded by one lock (`lock_shm()`), per-object magic bytes, recycle lists.
- `queriesData` (**64 bytes, architecture-independent layout**): `double response, timestamp; sqlite3_int64 db; unsigned domainID, clientID; int upstreamID, cacheID, id, CNAME_domainID, ede; uint16 qtype; enum status/type/privacylevel/reply/dnssec; bitfields allowed/complete/blocked/response_calculated/upstream_counted/db.changed/db.imported`. Strings live in a shared string pool addressed by offsets (`ippos`, `namepos`, `domainpos`) — no pointers, so forks and 32-bit builds share layout. "~2.4 MB saved per 300K queries" by field ordering.
- `clientsData` (~684 B): counts (`count`, `blockedcount`), hwaddr, `groupspos` (int array of group ids), `rate_limit`, `firstSeen/lastQuery`, **`int overTime[145]`** per client (cold, at the end for cache locality).
- `domainsData` (48 B): `count`, `blockedcount`, `cname_refcount`, hash, `lastQuery`.
- `upstreamsData`: `count`, `failed`, `responses`, `rtime`, `rtuncertainty` (running mean/variance of RTT), port.
- `DNSCacheData` (36 B): per (domain, client, qtype) blocking decision cache with `blocking_status`, `force_reply`, `list_id`, `expires` (seconds since `SHM_TIME_EPOCH` 2026-01-01 to dodge Y2038), regex CNAME target.
- Lookup tables: open-addressing hash tables for clients/domains/dns-cache (`lookup-table.c`), `queryIDMap` dnsmasq-id → index.
- `countersStruct`: `queries, upstreams, clients, domains, …_MAX, reply_*`, `database{gravity,antigravity,clients,groups,lists,domains{allowed/denied × exact/regex × total/enabled}}`, `querytype[TYPE_MAX]`, `status[QUERY_STATUS_MAX]`, `reply[QUERY_REPLY_MAX]`. **Summary = read counters; O(1).**
- `ShmSettings.qps[QPS_AVGLEN]` ring for queries-per-second.

### 10.2 overTime ring (`overTime.c`)
`OVERTIME_INTERVAL = 600 s`, `MAXLOGAGE = 24 h`, `OVERTIME_SLOTS = 145`. Each slot `{timestamp(center), total, blocked, cached, forwarded}`; slot index = `(ts_centered - overTime[0].timestamp)/600`. Every query increments its slot and the client's `overTime[slot]` at the moment status is set (`change_clientcount`). The GC thread (`gc.c`, interval 600 s, delay 60 s) expires queries older than `webserver.api.maxHistory` (default 86400 s), decrements client/domain/upstream/type/status/reply counters, compacts the queries array (`queries_offset`), then `moveOverTimeMemory(mintime)` shifts slots. `/api/history` = copy 145 structs under lock → JSON. `/api/history/clients` = per slot read `client->overTime[slot]`; top-N by `qsort` on a temp (id,count) array.

### 10.3 Top-K (`stats.c: get_top_domains`)
Min-heap of size `4K` (or all N if small) over `counters->domains`, filtered by regex `excludeDomains`, then sorted desc → O(N log K) with O(K) memory (comment: "reducing memory from O(N) to O(K)"). Same for clients and upstreams (upstreams also emit mean RTT and stddev via `rtime`/`rtuncertainty`).

### 10.4 Query database: memdb + disk (`database/query-table.c`)
- On start FTL opens an **in-memory SQLite** (`:memory:`, or `files.tmp_db` on disk if `database.forceDisk=true`) with table `query_storage` + indexes on every column + the four linking tables, then **`ATTACH`es** the on-disk `/etc/pihole/pihole-FTL.db` as schema `disk` (WAL if `database.useWAL`, `synchronous=NORMAL`, `mmap_size=256 MB`).
- Queries are written to memdb as they complete (`REPLACE INTO query_storage …`), with string→id dedup through `INSERT OR IGNORE INTO domain_by_id/client_by_id/forward_by_id/addinfo_by_id` plus a small process-local id cache.
- The DB thread every `database.DBinterval` (60 s) runs `INSERT INTO disk.query_storage SELECT * FROM query_storage WHERE id > (SELECT IFNULL(MAX(id),-1) FROM disk.query_storage)` and copies the linking tables; hourly `delete_old_queries_from_db` (`database.maxDBdays`, **default 91 days** in current `pihole.toml`; docs page still says 365); periodic `ANALYZE`.
- On startup `DBimport=true` loads the last `maxHistory` seconds back into shared memory and memdb (since FTL v6.5 asynchronously so DNS starts immediately).
- **Schema** (`MEMDB_VERSION 22`): `query_storage(id INTEGER PK AUTOINCREMENT, timestamp INT, type INT, status INT, domain INT→domain_by_id, client INT→client_by_id(ip,name), forward INT→forward_by_id, additional_info INT→addinfo_by_id(type,content) [1=CNAME domain, 2=list id], reply_type INT, reply_time REAL, dnssec INT, list_id INT, ede INT)`; view `queries` resolves ids back to text for user scripts; `counters(id,value)` lifetime totals (0 total, 1 blocked); `ftl(id,value)` metadata; `network(id,hwaddr UNIQUE,interface,firstSeen,lastQuery,numQueries,macVendor)` + `network_addresses(network_id,ip UNIQUE,lastSeen,name,nameUpdated)`; `message(id,timestamp,type,message,blob1..5)` diagnostics; `session(id,login_at,valid_until,remote_addr,user_agent,sid,csrf,tls_login,tls_mixed)` for session restore across restarts.
- Query log reads hit memdb (fast, ≤24 h) unless `disk=true`; `earliest_timestamp` / `earliest_timestamp_disk` tell the UI what range exists.

### 10.5 Privacy levels (`misc.privacylevel`)
0 show all · 1 hide domains (stored as `hidden`) · 2 hide domains+clients (`0.0.0.0`) · 3 anonymous (no query log, no DB logging). Applied at ingest, irreversible.

---

## 11. Web stack in v6

- **Server**: CivetWeb inside FTL; **Lua Server Pages** (`*.lp`, `<? … ?>` / `<?= … ?>`) replace PHP. The `pihole` Lua library (`src/lua/ftl_lua.c`) exposes only: `ftl_version, hostname, fileversion (cache-busting ?v=), webtheme, webhome, include, boxedlayout, needLogin, api_url, format_path`. Templates just assemble HTML; **no data is rendered server-side** — JS fetches everything. Request handler (`lua_web.c`) redirects unauthenticated `.lp` requests to `/admin/login`, exempts ACME challenges, refuses paths outside `webhome` unless `webserver.serve_all`.
- **Front-end** (`web/package.json`): AdminLTE 2.4.18, Bootstrap 3.4.1, jQuery 3.7.1, Chart.js 4.5.1 (+ chartjs-adapter-moment 1.0.1, chartjs-plugin-zoom 2.2.0, chartjs-plugin-deferred 2.0.0, hammerjs 2.0.8), moment 2.30.1, DataTables 1.10.21 (+buttons 1.7.1, select 1.3.1), daterangepicker 3.1.0, select2 4.0.13, bootstrap-select 1.13.18, bootstrap-toggle 2.2.2, bootstrap-notify 3.1.3, bstreeview 1.2.0, icheck-bootstrap 3.0.1, jquery-confirm 2.7.0, nprogress 0.2.0, qrious 4.0.2 (TOTP QR), animate.css 4.1.1, Font Awesome 6.7.2, modernized-waitme. No bundler; vendored under `vendor/`; lint with xo/eslint + prettier + autoprefixer. All pages are `.lp` + one JS file each (`scripts/js/*.js`, 29 files).
- **Themes** (`src/api/theme.c`, `style/themes/*.css`): `default-auto` ("Pi-hole auto", follows OS), `default-light` ("Pi-hole day"), `default-dark` ("Pi-hole midnight"), `default-darker` ("Pi-hole deep-midnight"), `high-contrast`, `high-contrast-dark`, `lcars` ("Star Trek LCARS"). Each has a brand color; theme is a server config (`webserver.interface.theme`) plus `boxed` layout flag. Chart colors are read from CSS (`utils.getCSSval("queries-blocked","background-color")`) so themes restyle charts without JS changes.
- **Security headers** (default `webserver.headers`): `X-DNS-Prefetch-Control: off`, strict CSP (`default-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' …`), `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`.
- **TLS**: `webserver.port = "80o,443os,[::]:80o,[::]:443os"` (o = optional, s = TLS), auto self-signed cert (`webserver.tls.cert`, validity 47 days, auto-renew 2 days before expiry), ACME-challenge passthrough; `webserver.paths.prefix` for reverse proxies (traefik strip-prefix example in comments); `webserver.acl` CIDR allow/deny list; `webserver.threads` (default 50).

---

## 12. Authentication model

- **Single shared password**, no users, no roles. Hash: **Balloon hashing (SHA-256, nettle)** with random salt, stored as `webserver.api.pwhash` (`$BALLOON-SHA256$…` style string; legacy double-SHA256 from v5 migrated). Empty hash = auth disabled (API returns `API_AUTH_EMPTYPASS`, UI skips login).
- **Session**: `POST /api/auth {"password":"…"[, "totp":123456]}` → `{"session":{"valid":true,"totp":false,"sid":"<base64 128-bit>","csrf":"<base64>","validity":1800,"message":null}}`. SID from `getrandom()`; sessions stored in a fixed array (`webserver.api.max_sessions`, default 16; 429 `no_seats` when full) and persisted to the `session` table if `webserver.session.restore`. Timeout `webserver.session.timeout` (default 1800 s), sliding. Sessions **bound to client IP** (`remote_addr`, plus `X-Forwarded-For` recorded).
- **Presenting the SID** (checked in this order): form field/JSON body `sid` → header `sid` or `X-FTL-SID` → cookie `sid` (HttpOnly; **then `X-FTL-CSRF` header required**) → query string `?sid=`.
- **Rate limiting** on login (429 `rate_limiting`), TOTP replay protection (`totp_reused`), and `GET /api/auth/sessions` + `DELETE /api/auth/session/{id}` for a "Currently active sessions" table in Settings → Web interface/API (shows IP, UA, TLS, app/cli flags, login/last-active/valid-until).
- **2FA**: `GET /api/auth/totp` suggests a secret (`otpauth` params: issuer, account, SHA1, 6 digits, 30 s, sample codes) → user scans QR (qrious) → confirms a code → `PATCH /api/config {"webserver":{"api":{"totp_secret":"…"}}}`. RFC 6238 HMAC-SHA1, base32 secret (20 bytes), write-only config item.
- **App password**: `GET /api/auth/app` returns a random password + its hash once; client stores the hash into `webserver.api.app_pwhash`. App sessions skip 2FA and **cannot change config unless `webserver.api.app_sudo=true`**. Generating a new app password invalidates all sessions.
- **CLI password**: `webserver.api.cli_pw=true` writes a per-boot random password to `/etc/pihole` so `pihole …` commands can call the API; such sessions cannot modify config.
- **Config items with flags**: `FLAG_INVALIDATE_SESSIONS` (password change), `FLAG_WRITE_ONLY` (totp_secret), `FLAG_RESTART_FTL`, `FLAG_ENV_VAR` (set by env → read-only), `FLAG_API_READ_ONLY` / `FLAG_API_CLI_READ_ONLY` (webroot/webhome/advancedOpts/dnsmasq_lines — host-only after the 2026 RCEs).
- `webserver.api.allow_destructive` gates flush/restart actions; `webserver.api.excludeClients/Domains` are privacy filters.

Gaps relative to kudurru's requirements: **no OIDC, no multi-user, no RBAC**; "app password" is the only scoped credential (read-mostly vs sudo).

---

## 13. Configuration model — `pihole.toml`

- Single file `/etc/pihole/pihole.toml`, **written by FTL** with every key present, grouped by section, each with a help comment, allowed values, and `### CHANGED, default = …` markers on non-default lines (and `### CHANGED (env)` when forced by environment). Header: `# Pi-hole configuration file (v6.5-…) … Last updated on …`.
- Sections: `[dns]` (upstreams, CNAMEdeepInspect, blockESNI, EDNS0ECS, ignoreLocalhost, showDNSSEC, analyzeOnlyAandAAAA, piholePTR, replyWhenBusy, blockTTL, hosts, domainNeeded, expandHosts, bogusPriv, dnssec, interface, hostRecord, listeningMode LOCAL|SINGLE|BIND|ALL|NONE, queryLogging, cnameRecords, port, localise, revServers, `[dns.domain]`, `[dns.cache]` size/optimizer/upstreamBlockedTTL/rrtype, `[dns.blocking]` active/mode NULL|IP|IP_NODATA_AAAA|NX|NODATA/edns NONE|CODE|TEXT, `[dns.specialDomains]` mozillaCanary/iCloudPrivateRelay/designatedResolver, `[dns.reply.host|blocking]` force4/IPv4/force6/IPv6, `[dns.rateLimit]` count/interval), `[dhcp]` (active,start,end,router,netmask,leaseTime,ipv6,rapidCommit,multiDNS,logging,ignoreUnknownClients,hosts[]), `[ntp.ipv4|ipv6|sync(.rtc)]`, `[resolver]` (resolveIPv4/6, macNames, networkNames, refreshNames), `[database]` (DBimport, maxDBdays, DBinterval, useWAL, forceDisk, `[database.network]` parseARPcache/expire), `[webserver]` (domain, acl, port, threads, headers[], serve_all, advancedOpts[], `[session]` timeout/restore, `[tls]` cert/validity, `[paths]` webroot/webhome/prefix, `[interface]` boxed/theme, `[api]` max_sessions/prettyJSON/pwhash/password(pseudo)/totp_secret/app_pwhash/app_sudo/cli_pw/excludeClients/excludeDomains/maxHistory/maxClients/client_history_global_max/allow_destructive/`[temp]` limit/unit), `[files]` (database, tmp_db, gravity, gravity_tmp, macvendor, pcap, `[log]` ftl/dnsmasq/webserver), `[misc]` (privacylevel, delay_startup, nice, addr2line, etc_dnsmasq_d, dnsmasq_lines[], extraLogging, readOnly, normalizeCPU, hide_dnsmasq_warn, hide_connection_error, `[check]` load/shmem/disk), `[debug]` (31 boolean flags + `all`).
- **Five equivalent write paths** with one validator (`struct conf_item {k, p[], e, h, a (allowed values JSON), t (type), f (flags), v, d (default), c (validator fn)}`): TOML file (watched via inotify), `pihole-FTL --config dns.dnssec true`, `PATCH /api/config` (JSON mirror of TOML: `{"config":{"dns":{"dnssec":true}}}`; `?restart=false` to defer), `PUT/DELETE /api/config/{element}/{value}` for array items, and env vars `FTLCONF_<section>_<key>` (e.g. `FTLCONF_dns_upstreams`, multi-value via `;` or newline-separated). **Env-set values become read-only everywhere** ("single source of truth"). `GET /api/config?detailed=true` returns per-key `{value, default, description, allowed, flags{restart_dnsmasq, env_var, …}}` — the web's "All settings" page is generated from this.
- FTL rewrites TOML atomically, keeps rotated copies, migrates v5 `setupVars.conf`/`pihole-FTL.conf`, and emits dnsmasq config (`/etc/pihole/dnsmasq.conf`) from it; `misc.etc_dnsmasq_d` optionally includes legacy `/etc/dnsmasq.d`.

Docs reference page is auto-generated from the same metadata (TOML / CLI / Docker-compose env tabs per key) — a good pattern.

---

## 14. Deployment patterns

- **Docker** (`docs/docker/index.md`): `pihole/pihole:latest`, ports 53/tcp+udp, 80, 443, optional 67/udp (DHCP), 123/udp (NTP); env `TZ`, `FTLCONF_webserver_api_password` (or `WEBPASSWORD_FILE` secret), `FTLCONF_dns_listeningMode: 'ALL'` for bridge networks; volume `./etc-pihole:/etc/pihole`; `cap_add: NET_ADMIN` (DHCP), `SYS_TIME` (NTP sets clock), `SYS_NICE`. Random admin password printed to logs if unset. Image is Alpine-based since v6.
- Bare metal: `curl -sSL https://install.pi-hole.net | bash`; systemd unit for `pihole-FTL`; `pihole -up` updates core/web/FTL together (version triplet must match: `pihole -v`).
- **No clustering/HA**: devs: "There are no plans to include any high-availability features from our side." Community stacks: keepalived VIP + nebula-sync (v6, API/teleporter-based, hourly cron, no UI) or orbital-sync, gravity-sync (v5 only); "Pi-hole HA" (RamSet/pihole-ha-cluster, Apache-2.0 per search snippet — UNVERIFIED) adds DHCP failover + VIP + gravity sync over HTTP; dnsdist in front of N Pi-holes is a known pattern (one forum user ran 12 behind dnsdist). YAPD (May 2026, github.com/leufrasiojunior/yadp) is a third-party multi-instance dashboard that aggregates query activity and diffs groups/clients/domains/lists/config across instances with "drift detection" before sync.

---

## 15. Community criticism (what to fix, not repeat)

| Theme | Evidence |
|---|---|
| **No multi-instance view / no HA** | Devs: HA "out of scope"; feature requests "Aggregated site for multiple piholes", users with 2–3 Pi-holes want "combined stats & manage both through a single interface"; which Pi-hole blocked a request means checking each one. Ecosystem of sync scripts exists precisely because the product lacks it. |
| **Long-term graphs removed in v6** | Thread 77046; "I can not update to v6 if there is no long-term data"; devs: not ported, no one working on it (Jul 2025). |
| **Stats too coarse** | 10-minute buckets only; 24 h in memory; no per-group or per-list stats; no "which list caused the block" aggregation (only per-query `list_id`); no latency percentiles (only mean/stddev per upstream). |
| **Breaking API change v5→v6** | Home Assistant core integration broke (fixed ~HA 2025.8 per community), Homarr, Flutterhole, PADD all needed rewrites; `/admin/api.php` → `/api` move; old env vars removed in Docker. |
| **Single password, IP-bound sessions** | Session invalidates on roaming IP; no users/roles; app-password is the only delegation; 2FA lockout needs CLI recovery. |
| **Performance on tiny hardware / HTTPS** | Threads "PiHole v6 WebUI is super slow and CPU over 100%", "Web interface slow after update from 5 to 6" (Pi Zero / Model B; TLS overhead; large default date range on Safari). |
| **Security record of embedding a web server** | 2026: RCE via CivetWeb config injection, log injection → Lua eval, session-expiration bypass, missing rate limiting, CRLF via group name; fix was to make code-bearing config keys host-only. |
| **UI nits** | Top lists refresh 10 s→60 s; temperature dropped from UI; static DHCP lease editor was a raw text field until Web v6.6; filters reset on refresh (older threads). |
| **Blocking engine limits** | Hosts-format lists + ABP-style entries (`abp_entries` counted) but no full adblock syntax; regex is POSIX ERE via TRE with Pi-hole extensions; CNAME targets must be cached locally. |

---

## 16. What kudurru should borrow

1. **The four-tile → chart → top-lists → query-log drill-down structure**, with every number clickable into a pre-filtered log. Keep the exact tiles (total, blocked, % blocked, domains on lists) plus active/total clients in the footer.
2. **Stacked 10-minute bars with blocked/cached/forwarded/other overlay** and a per-client stacked chart; click-bucket → ±5 min log view. 145-slot ring per server is cheap; for kudurru precompute buckets per node and sum across nodes.
3. **Query log contract**: server-side pagination with an **id cursor** (stable under live inserts), `recordsTotal/recordsFiltered`, `earliest_timestamp`, filter suggestions endpoint, wildcard search on domain/client, `disk=true` escape hatch, live toggle at 2 s, and per-row Allow/Deny with a confirmation toast. Keep the rich **status taxonomy** (gravity/regex/exact/external/CNAME/cache/stale/retried/in-progress/special) and expose `list_id`, `cname`, `ede`, `dnssec`, `reply.time`.
4. **Semantic CSS classes for blocked/permitted/cached/other** that themes re-skin; chart colors read from CSS; 7-theme model incl. auto, high-contrast and one fun theme.
5. **Groups → clients/domains/lists with a Default group 0**, client matching by IP → longest-prefix CIDR → MAC → hostname → interface, with a tie warning. Multi-add arrays with `processed{success,errors}`, `:batchDelete`, `comment` on everything, `enabled` toggles, list health status codes (OK / unchanged / unavailable-used-cache / invalid) and counts (`number`, `invalid_domains`, `abp_entries`, `date_updated`).
6. **Disable-blocking timer** semantics (`{blocking:false,timer:300}`, GET returns remaining seconds, re-POST resets) and the sidebar countdown.
7. **Uniform API envelope**: `took`, `{error:{key,message,hint}}`, 201+Location, 204 on delete, 429 on login brute force, self-hosted OpenAPI at the running version, `GET /api/endpoints`.
8. **Single config document with metadata** (type, default, allowed values, help, flags like restart-required/env-locked/write-only/host-only) driving: the file format, CLI `--config`, `PATCH /config`, env overrides `KUDURRU_<section>_<key>` that become read-only, and an auto-generated "All settings" page and docs page. The `### CHANGED, default = …` annotation when writing files back is excellent for ops.
9. **Shared-memory style hot path**: fixed-size, pointer-free records; counters by type/status/reply updated at ingest; O(1) summary; heap top-K; GC thread that decrements counters as it expires queries. In Go: ring buffers + atomic counters, not a DB query per dashboard poll.
10. **memdb + periodic flush**: last 24 h in memory (SQLite/in-process index), batch export every 60 s to durable storage, string-interning tables (`domain_by_id` etc.), `maxDBdays` retention, `ANALYZE`-style maintenance. For kudurru: per-node local store + cluster-wide aggregation.
11. **Privacy levels** at ingest (0–3) and `excludeClients/excludeDomains` regex filters.
12. **Diagnosis messages** table + navbar badge (regex errors, subnet overlaps, hostname warnings, rate-limit events, disk/shmem/load warnings, cert mismatch, inaccessible lists, NTP).
13. **Teleporter-style export/import** as a zip of JSON tables + config (the basis of every sync tool) — kudurru should make this a first-class, versioned backup format *and* the seed for secondary bootstrap.
14. **`/api/padd`-style single compact endpoint** for kiosks/TUIs and the `kudurru api <endpoint>` CLI helper.
15. **Reverse-proxy affordances**: path prefix, ACME passthrough, strict CSP defaults, ACLs, optional TLS per listener (`80o,443os`).
16. **Settings Basic/Expert toggle** and `data-configkeys` binding of every control to a config path.

## 17. What kudurru should avoid

1. **Copying any EUPL code/assets** (FTL, web templates/CSS/JS, OpenAPI YAML). Re-derive.
2. **Single-node mental model**: no cluster identity in stats, no "which node answered", no aggregated or per-node toggles. kudurru's dashboard must be multi-node from day one (node selector + "all nodes"), with per-node health in the sidebar.
3. **Dropping long-term analytics**: keep `/history/database`-class endpoints *and* a UI for them (date ranges, top lists by period, per-group/per-list breakdowns) — the single most-requested missing v6 feature.
4. **Coarse, fixed 10-minute buckets as the only resolution**: store 1-minute (or configurable) buckets for the last 24 h and roll up for longer ranges; add latency percentiles per upstream.
5. **One shared password, IP-bound sessions, no roles**: use OIDC + local users, RBAC (viewer/operator/admin + per-zone), API tokens with scopes instead of a single "app password + sudo flag"; do not bind sessions to IP by default.
6. **Breaking API changes without a compatibility story**: version the API path (`/api/v1`), publish a changelog, keep deprecated routes for one major.
7. **Embedding an HTTP server whose config can be mutated through the API** (CivetWeb `advancedOpts`, dnsmasq raw lines): never let the API inject raw config lines into a privileged subsystem; keep code-bearing settings host-only from the start.
8. **jQuery/AdminLTE 2 + Bootstrap 3 + 25 vendored libs** with no bundler: pick a modern typed front-end with a component library and tree-shaken Chart.js/ECharts; reuse Pi-hole's *information architecture*, not its stack.
9. **Polling 8+ endpoints per page at different intervals**: use one summary endpoint plus SSE/WebSocket push for live log and status; keep Pi-hole's cadence as the fallback.
10. **CNAME limitation** ("target must already be in cache") and config-array-encoded records (`"IP NAME"`, `"cname,target,ttl"` strings): kudurru records should be typed objects in zones (RRsets), not stringly-typed TOML arrays.
11. **Unbounded growth of the vendored C stack**: don't fork a resolver; wrap/embed a Go resolver and integrate dnsdist/keepalived as sidecars with declared contracts.
12. **Hidden defaults drift between docs and code** (maxDBdays 365 in docs vs 91 in code): generate docs from the config metadata like Pi-hole's configfile.md pipeline, but test the generation.

## 18. Open questions / UNVERIFIED

- Exact FTL v6.7.1 changelog (only a third-party blog summarized it; the GitHub release page was not fetched).
- Docker tag `2026.09.0` for the 6.7.1 bundle (third-party claim).
- "Pi-hole HA" licence/maturity and v6 support (search snippets only).
- Home Assistant 2025.8 restoring the core Pi-hole integration (community claim).
- `API_QUERIES_MAX_ROWS` numeric value (defined in a header not read).
- Whether Web v6.6 still supports Internet Explorer-era browsers via Bootstrap 3 (irrelevant for kudurru).
