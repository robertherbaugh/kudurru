> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [dev-docs/design-record.md](../design-record.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# Research: BeryJu/gravity

Source material for the kudurru architecture document. Researched 2026-10-10 from a
`git clone --depth 1` of https://github.com/BeryJu/gravity (HEAD `b873d1f`, 2026-10-09),
the GitHub REST API, and https://gravity.beryju.io. Anything not verified from those is
marked UNVERIFIED.

---

## 0. Fast facts

| Item | Value | Source |
|---|---|---|
| Repo | https://github.com/BeryJu/gravity | GitHub API |
| Tagline | "Fully-replicated DNS, DHCP and TFTP Server backed by etcd" | README.md |
| **License** | **GPL-3.0** (not Apache/MIT) | `LICENSE`, GitHub API `license.spdx_id` |
| Created | 2022-07-06 | GitHub API |
| Stars / forks / open issues | 930 / 32 / 57 (issues+PRs) | GitHub API, 2026-10-10 |
| Latest release | **v0.33.0**, published 2026-05-09 | GitHub releases API; `Makefile` `VERSION = "0.33.0"`; `web/package.json` 0.33.0 |
| Last push | 2026-10-09 (dependabot bump) | GitHub API |
| Language / toolchain | Go `1.26.6` (go.mod), built in `golang:1.27` image; Node 26 for web | `go.mod`, `Dockerfile` |
| Module path | `beryju.io/gravity` | `go.mod` |
| Container | `ghcr.io/beryju/gravity:{stable,latest,vX.Y.Z,<sha>,*-debug}`; linux/amd64 + linux/arm64; SBOM + provenance attestations | `.github/workflows/build.yml`, `release-publish.yml` |
| Sidecar image | `ghcr.io/beryju/gravity-external-dns` (external-dns webhook provider) | `release-publish.yml` |
| Docs site | Hugo + Docsy theme, Netlify | `docs/hugo.yaml`, `docs/netlify.toml` |
| Size | ~15.2k LOC Go (excl. tests/generated), ~11.4k LOC TypeScript, 81 `_test.go` files | `wc -l` on clone |
| Ports | 53 DNS, 67 DHCP, 69 TFTP, 8008 API/UI, 8009 metrics, 8010 debug/pprof, 2379/2380 etcd client/peer (client listen default is actually `2381` in code, see §14), 8100 internal blocklist server, `/var/run/gravity.sock` | code |

**Licensing consequence for kudurru:** Gravity is GPL-3.0. kudurru (Kudurru Community License; Apache-2.0 when this note was written) can borrow
*designs, key layouts, API shapes and UX ideas* but must not copy code. Note Gravity's own
dependencies: blocky (Apache-2.0), CoreDNS (Apache-2.0), miekg/dns (BSD), etcd (Apache-2.0),
insomniacslk/dhcp (BSD-3), goja (MIT), swaggest/rest (MIT) — all usable by kudurru directly.
(Dependency licenses are from memory of those projects, UNVERIFIED here.)

---

## 1. Architecture overview

Gravity is a **single static Go binary** (`cmd/server/main`) that runs an *instance*. An
instance starts an **embedded etcd** (optional) and then a set of **roles**. Every role is
a Go package registered at init time; which roles run on a node is decided per node
(`BOOTSTRAP_ROLES` env on first start, then persisted in etcd under
`/gravity/instance/<id>/roles`).

```
cmd/server/main      -> cobra root: `gravity server` (default CMD), `gravity cli ...`,
                        `gravity generateSchema`, `gravity completion`
cmd/cli              -> `gravity cli` subcommands (talks to API over unix socket)
cmd/external-dns     -> separate binary: external-dns webhook provider
pkg/extconfig        -> env-var config (Netflix/go-env), zap logger, version, Sentry/Pyroscope
pkg/instance         -> Instance lifecycle, role registry, event bus, JS hooks (goja),
                        migrations, instance info keepalive, cluster/instance API
pkg/storage          -> etcd client wrapper (namespace prefix, hooks, Key builder)
pkg/storage/watcher  -> generic etcd prefix -> in-memory map cache (list+watch)
pkg/roles/<role>     -> api, backup, debug, dhcp, discovery, dns, etcd, monitoring, tftp, tsdb
pkg/convert          -> bind / ms_dhcp / opnsense importers
pkg/externaldns      -> external-dns webhook server (generated from external-dns OpenAPI)
api/                 -> openapi-generator Go client (used by CLI, external-dns, importers,
                        and the server itself when it needs to call "itself")
web/                 -> Lit + PatternFly 4 SPA, built with rollup, embedded via go:embed
schema.yml           -> generated OpenAPI 3.0.3 spec (2,882 lines)
protobuf/            -> User/Token/Permission/MetricsRecord protobufs stored in etcd
internal/resources   -> embedded blocklists, iPXE binaries, MAC OUI DB (downloaded at build)
hack/                -> docker-compose for 3-node cluster, e2e DHCP relay/client containers
extras/              -> Grafana dashboard JSON, Prometheus alert rules
```

### 1.1 Role interface and registry

`pkg/roles/role.go`:

```go
type Role interface {
    Start(ctx context.Context, config []byte) error
    Stop()
}
type MigratableRole interface { Role; RegisterMigrations() }
type RoleConstructor func(Instance) Role
func Register(name string, constructor RoleConstructor)   // called from each role's init()
```

Each role gets a `roles.Instance` facade (`RoleInstance`) that exposes `KV()`, `Log()`,
`Context()`, `DispatchEvent()`, `AddEventListener()`, `ExecuteHook()`, `Migrator()`.
Roles are *constructed once* but *Start/Stop'd repeatedly* — every time the role's config key
changes in etcd the instance stops and restarts that role (see §1.3). This is why
`RoleInstance` swaps its context/kv under a lock (`rebind`) instead of being replaced.

Roles register with `pkg/instance/roles.go` via blank imports:
`api, backup, debug, dhcp, discovery, dns, etcd, monitoring, tftp, tsdb`.

### 1.2 Instance boot sequence (`pkg/instance/instance.go`)

1. `extconfig.Get()` parses env, derives `INSTANCE_IDENTIFIER` (hostname) and
   `INSTANCE_IP` (auto-detected if unset).
2. Sentry + Pyroscope init (Sentry DSN is **enabled-by-flag but has a hard-coded default
   DSN pointing at beryju.org** — `SENTRY_ENABLED` defaults false).
3. If `etcd` is in `BOOTSTRAP_ROLES`: construct + start the embedded etcd role, wait for
   `ReadyNotify()`; if joining, wait until promoted from learner.
4. `bootstrap()`:
   - `keepAliveInstanceInfo`: creates an etcd `concurrency.Session` (lease) and writes
     `/gravity/instance/<id>` = `{version, roles, identifier, ip, memoryBytes}` bound to that
     lease — so a dead node's record disappears automatically.
   - `setupInstanceAPI`: registers `/api/v1/cluster`, `/cluster/instance`,
     `/cluster/roles/restart`.
   - Reads `/gravity/instance/<id>/roles` (fallback env) and constructs each role.
   - `checkFirstStart`: if `/gravity/role/cluster` key is absent -> this is a brand-new
     cluster: run `IMPORT_CONFIGS` auto-import, dispatch `instance.first_start` (roles
     react: API creates admin user, DNS creates root zone `.`, discovery creates a subnet
     for the local CIDR), then write `{"setup":true}`.
   - For every role, goroutine `startWatchRole`: read `/gravity/instance/role/<roleId>`
     (the role config JSON), run migrations, `role.Start(ctx, config)`, then **watch** that
     key forever; any PUT/DELETE dispatches `role.restart` which does Stop + Start with the
     new config.
   - After all roles started: dispatch `roles.started` (monitoring role flips readiness).

Note: role config key is `/gravity/instance/role/<role>` — **global**, not per-instance
(despite the `instance` prefix). All nodes share e.g. the DNS port and OIDC config.

### 1.3 Event bus

In-process, synchronous, per-topic listeners keyed by role id (`pkg/instance/events.go`).
Not replicated. Used for: API mux setup (`roles.api.mux_setup` — every role registers its
HTTP routes on the shared swaggest service), first start, role restart, DHCP lease put ->
DNS record creation, TSDB inc/set/write, backup run, debug mux setup.

Event payload = `map[string]interface{}` plus optional `RelatedObjectKey` /
`RelatedObjectOptions []clientv3.OpOption` (used to pass the DHCP lease's etcd lease ID so
DNS records expire with the DHCP lease).

### 1.4 Scripting hooks (goja)

`RoleInstance.ExecuteHook` spins up a fresh **goja** JS VM per call, injects
`gravity.{log,node,version,role}`, `net.parseIP`, `strconv.toBytes`, role-specific env
(`dhcp.Opt`, `dhcp.GetString`), evaluates the user's source string, and calls the named
function (`onDNSRequestBefore/After`, `onDHCPRequestBefore/After`). Hook source is a string
field on the Zone / Scope object. Duration is a Prometheus histogram. A new VM per request
is simple but expensive; there is no caching/compilation of the script.

---

## 2. Storage layer

### 2.1 Client (`pkg/storage/client.go`)

Wraps `clientv3.Client` with:
- `namespace.NewKV/NewWatcher/NewLease(prefix)` — every key is automatically under
  `ETCD_PREFIX` (default `/gravity`), including leases.
- `trace.NewKV` — in debug mode DPanics on any op issued outside a transaction wrapper
  (observability hook).
- `StorageHook{GetPre,PutPre,DeletePre,GetPost,PutPost,DeletePost}` — chainable
  pre/post hooks; used by the **migration system** to rewrite keys/values on the fly while
  a cluster is mixed-version (`WithHooks` returns a child client).
- `PutObj/Unmarshal/Marshal` — JSON by default, protobuf if the type implements
  `proto.Message` (User, Token, MetricsRecord are protobuf; everything else JSON).
- Dial timeout 2s, keepalive 2s.

### 2.2 Key builder (`pkg/storage/key.go`)

`c.Key("dns","zones","example.com.").Prefix(true).String()` -> `/dns/zones/example.com./`
(the `/gravity` namespace is added by the client). `KeyFromString`, `Up()`, `Add()`,
`Copy()`. Trailing slash = prefix query marker.

### 2.3 Watcher cache (`pkg/storage/watcher/watcher.go`)

Generic `Watcher[T]`:
```go
watcher.New[T](constructor func(*mvccpb.KeyValue)(T,error), client, prefixKey,
    WithPrefix[T]()                 // keep nested keys (default: only direct children)
    WithAfterInitialLoad[T](func()) // e.g. recalc DHCP scope usage after leases loaded
    WithBeforeUpdate[T](func(old T, mvccpb.Event_EventType))  // e.g. stop sub-watchers, metrics
    WithKeyFunc[T](func(string)string)) // collapse record UIDs into one map entry
```
`Start(ctx)`: `Get(prefix, WithPrefix)` to load initial state (retry every 1s on failure),
then `Watch(prefix, WithPrefix)` in a goroutine; every event mutates a `map[string]T` under
an RWMutex. `Iter()`, `IterRelativeKey()`, `Get/GetOK/GetPrefix`. Prometheus counter
`gravity_watcher_events{key_prefix,type}`.

**This is the replication primitive.** Zones, records-per-zone, DHCP scopes, DHCP leases,
discovery subnets are all `Watcher`s. Every node holds the full dataset in RAM and is
updated by etcd watch within milliseconds of a write.

Caveats visible in code: no handling of watch-channel close / compaction
(`ErrCompacted`) — if the watch dies the goroutine ends silently and the cache goes stale
until the role restarts. No resync. (Issue #2148 "concurrent DISCOVER on multiple nodes
... stale local watcher state" is this class of bug.)

### 2.4 Migrations (`pkg/instance/migrate`)

Cluster version = **lowest** semver among `/gravity/instance/*` entries. Each role may
register `Migration{Check(clusterVersion), Hook() -> hooked client, Cleanup(), Name()}`.
While the cluster still contains older nodes a migration *hooks the KV client* (e.g. write
both old and new key formats); once all nodes are upgraded `Cleanup()` runs. Example in
PR #2387: moving DHCP reservations to scope-qualified keys with legacy migration.

---

## 3. etcd key layout (all under `ETCD_PREFIX`, default `/gravity`)

| Key | Value | Lease/TTL | Notes |
|---|---|---|---|
| `/role/cluster` | `{"setup":true}` | – | first-start marker |
| `/instance/<id>` | `InstanceInfo{version,roles[],identifier,ip,memoryBytes}` | etcd session lease (auto-removed on death) | cluster node list |
| `/instance/<id>/roles` | `"dns;dhcp;api"` | – | roles for that node (written by join API or by node) |
| `/instance/role/<role>` | role config JSON (`{"port":53}`, API `{oidc,cookieSecret,port,sessionDuration}` …) | – | **global** role config; watched -> role restart |
| `/dns/zones/<zone.>` | `Zone{handlerConfigs[],defaultTTL,authoritative,hook}` | – | zone name includes trailing dot; `.` is root zone |
| `/dns/zones/<zone.>/<name>/<TYPE>` | `Record{data,ttl,mxPreference,srv*,soa*}` | optional lease | single-answer record; `name`=`@` apex, `*` wildcard, `*.*` multi-level |
| `/dns/zones/<zone.>/<name>/<TYPE>/<uid>` | same | optional lease (cache TTL / DHCP lease) | multi-answer; uid = MAC for DHCP, `external-dns-<name>-<target>` for external-dns, answer index for forwarder cache |
| `/dhcp/scopes/<name>` | `Scope{subnetCidr,options[],ttl,default,ipam{},dns{},hook}` | – | |
| `/dhcp/leases/<identifier>` | `Lease{address,hostname,scopeKey,expiry,…}` | etcd lease = DHCP TTL (none for reservations, `expiry=-1`) | identifier = MAC (or client-id) |
| `/dhcp/ipam/<scope>` | – | – | `concurrency.Mutex` lock key for IPAM |
| `/discovery/subnets/<id>` | `Subnet{cidr,dnsResolver,discoveryTTL}` | – | also used as mutex key for scan |
| `/discovery/devices/<ip>` | `Device{mac,ip,hostname}` | lease = discoveryTTL | |
| `/tftp/files/<clientIP>/<path>` or `/tftp/files/shared/<path>` | file bytes (<=10 MB) | – | |
| `/api/users/<username>` | protobuf `User{password(bcrypt),permissions[]}` | – | |
| `/api/tokens/<token>` | protobuf `Token{key,username}` | – | token string **is** the key (plaintext at rest) |
| `/api/sessions/<sid>` | gorilla session via `api7/etcdstore` | MaxAge | sessions replicated cluster-wide |
| `/backup/status/<instance>` | `BackupStatus` | – | |
| `/tsdb/<metricKey...>/<node>/<unixTs>` | protobuf `MetricsRecord{value}` | lease = `expire` (30 min default) | in-etcd time series |
| `/tsdb/system/{memory,cpu}/<node>/<ts>` | same | same | |
| `/tsdb/dns/handler/<type>/<node>/<ts>`, `/tsdb/dns/zones/<zone.>/<node>/<ts>` | per-zone / per-handler query counters for UI charts | same | zone/handler metric keys are namespaced under `/tsdb/`; `APIZonesGet` filters names containing `/` because record keys nest under the zone prefix |

Export/import: `POST /api/v1/cluster/export {safe}` dumps every key as
`{key, value(base64)}`; `safe=true` strips sessions/tokens/users/API role config/tsdb.
`POST /cluster/import` puts them back. Same JSON format for `IMPORT_CONFIGS` env.

---

## 4. Clustering / replication

### 4.1 Embedded etcd (`pkg/roles/etcd/role.go`)

- `embed.NewConfig()` with `Dir=$DATA_PATH/etcd`, auto-compaction `periodic/60m`,
  `MaxRequestBytes=10MB` (for TFTP files), `Name=INSTANCE_IDENTIFIER`.
- Client URLs: `http://localhost:2379` + `http://<INSTANCE_IP>:<ETCD_CLIENT_PORT>`
  (**plain HTTP, no client auth**). Code default for `ETCD_CLIENT_PORT` is `2381` (docs say
  2379); localhost:2379 is always also bound.
- Peer URLs: `https://<INSTANCE_IP>:2380` with **`PeerAutoTLS=true`** (self-signed certs in
  `$DATA_PATH/cert/peer/`, `ClientCertAuth=true`, validity 1 year).
- Initial cluster = self, unless joining.
- Leader watch: on `LeaderChangedNotify()`, if this member became leader, start the
  `LeaderClusterReconciler`; otherwise stop it.

### 4.2 Join flow (token-based, via the HTTP API — not raw etcd)

1. Admin uses UI wizard (`ClusterJoinWizard`): it creates a user
   `gravity-system:cluster:join` with the single permission `POST /api/v1/etcd/join`,
   mints a token for it, and renders a docker-compose file with
   `ETCD_JOIN_CLUSTER=<token>,http://<existing-node-ip>:8008` and `BOOTSTRAP_ROLES=...`.
   (Manual alternative: `ADMIN_TOKEN` env on node 1 + hack/cluster compose.)
2. New node boots; `prepareJoin`: if `$DATA_PATH/etcd/member` already exists, skip (already
   a member). Otherwise call `POST /api/v1/etcd/join
   {peer:"https://<myip>:2380", identifier, roles}` on the existing node with the bearer
   token, retrying with backoff up to 20 times on HTTP 500.
3. Existing node (`APIClusterJoin`): refuses (`Unavailable`) if the cluster is unhealthy
   or another learner is still catching up; triggers a backup snapshot; writes
   `/instance/<newid>/roles`; calls `MemberAddAsLearner(peer)`; returns
   `etcdInitialCluster` = all current `name=peerURL` pairs + the new one.
4. New node sets `ClusterState=existing`, `InitialCluster=<that>`, starts etcd, then
   `waitForPromotion` polls `IsLearner()` with backoff (50 attempts).
5. **Leader's reconciler** (every 5s): lists members, computes `ClusterStatus` by dialling
   each member's non-localhost client URL for `Status()`; if a learner exists and its
   `Revision >= 90%` of leader revision -> `MemberPromote`.
6. On join failure the node wipes `$DATA_PATH/etcd/member` and comes up as a standalone
   single-node cluster (a known footgun: a mistyped token silently yields a split brain of
   two independent clusters; several issues #1093, #1389, #894 are this).

Remove: `DELETE /api/v1/etcd/remove?peerID=<hex>` (backup first, then `MemberRemove` async)
or `gravity cli etcdctl member remove <id>`. `POST /api/v1/etcd/move_leader`.
`GET /api/v1/etcd/members` -> `{name,id,isLearner,isLeader}` (only registered when the
etcd role runs; **404 when using external etcd -> UI nodes page spins forever, issue #1949**).

### 4.3 External etcd

Omit `etcd` from `BOOTSTRAP_ROLES` and set `ETCD_ENDPOINT=host:port`. Docs explicitly
recommend this for multi-node Kubernetes ("built-in cluster management is optimized for
static nodes and simplicity"), pointing to etcd operators (aenix, operatorhub) or
"Kubernetes' etcd". No TLS/auth options for the client are exposed via env (UNVERIFIED
whether `ETCD_ENDPOINT` accepts `https://`; the code passes the string straight to
`clientv3.Config.Endpoints` without TLS config, so mTLS etcd is not supported).

### 4.4 Quorum behaviour

Docs: odd node counts recommended; with 2 nodes losing one = no quorum. Mitigation: all
roles keep in-memory `Watcher` caches, so **DNS keeps answering from memory (if the zone
has the `memory` or `forward_*` handlers) even when etcd is unavailable**; DHCP falls back
to in-process serialization if the etcd mutex can't be acquired (5s timeout). Writes
(new leases, cache fills, API) fail during quorum loss.

### 4.5 Multi-site

Marketing claim: "seamless multi-site deployment without external databases". Reality: a
single etcd Raft group spanning sites (latency-sensitive, every write crosses WAN). No
primary/secondary or zone-transfer concept at all; no AXFR/IXFR, no NOTIFY.

---

## 5. DNS role (`pkg/roles/dns`)

### 5.1 Server

`miekg/dns` `ServeMux` with a single root handler; one UDP + one TCP `dns.Server` per
listen address (`INSTANCE_IP` list, or `INSTANCE_LISTEN`). Role config: `{port}` (default
53). No DoT/DoH/DoQ, no DNSSEC signing or validation, no EDNS client subnet, no AXFR,
no rate limiting, no ACLs. Middleware chain: `recoverMiddleware` (SERVFAIL on panic) ->
`loggingMiddleware` (one zap `Info` line per query with client, rcode, names, types,
answers, runtime — this *is* the "query log") -> handler.

### 5.2 Zone model

```go
type Zone struct {
    Name           string                   // "example.com." (trailing dot; "." = root)
    HandlerConfigs []map[string]interface{} // ordered chain, each has "type"
    DefaultTTL     uint32                   // 3600
    Authoritative  bool                     // sets AA bit, enables internal SOA
    Hook           string                   // JS source
    records *watcher.Watcher[map[string]*Record] // per-zone cache keyed by "/zone/name/TYPE" -> uid->Record
    h []Handler
}
```
Zones are not typed (no primary/secondary/stub/forward enum) — behaviour is entirely the
handler chain. UI presets: *Gravity-hosted* `[memory, etcd]`; *Forwarder (direct)*
`[forward_ip]`; *Forwarder (Blocky)* `[forward_blocky, forward_ip]`; *Import* `[etcd]`.
On first start the root zone `.` is created as `[memory, etcd, forward_ip{to:[FALLBACK_DNS
=1.1.1.1:53], cache_ttl:3600}]`.

Zone selection (`rootHandler`): iterate all zones, pick the **longest zone name that is a
suffix of the qname** (case-insensitive), else root zone `.`; else NXDOMAIN. No
fall-through between zones. Iteration cap 20 (CNAME chasing re-enters `rootHandler`).

### 5.3 Record model

```go
type Record struct {
    Name, Type string      // from key; Type is "A","AAAA","PTR","SRV","MX","CNAME","TXT","SOA"
    Data string            // IP / target / text (TXT multi-string joined with "\n")
    uid string             // optional 3rd key segment
    TTL uint32
    MXPreference uint16; SRVPort, SRVPriority, SRVWeight uint16
    SOAMbox string; SOASerial, SOARefresh, SOARetry, SOAExpire uint32
}
```
**Only 8 RR types.** No NS, CAA, HTTPS/SVCB (issue #1335), NAPTR, DNSKEY, etc. One value per
key; multiple values = multiple uids. `FQDN(qname)` reconstructs the owner name,
substituting wildcard labels from the query.

### 5.4 Handler chain

```go
type Handler interface {
    Handle(w *utils.FakeDNSWriter, r *utils.DNSRequest) *dns.Msg  // nil = "no answer, try next"
    Identifier() string
}
var HandlerRegistry  // map type -> constructor(zone, rawConfig)
```
`Zone.resolve`: run `onDNSRequestBefore` hook; prepend internal `int_soa` handler; for each
handler, `Handle()`; first non-nil reply wins: strip upstream OPT records, mirror client
EDNS0 size/DO, `SetReply`, run `onDNSRequestAfter`, write, update metrics
(`gravity_dns_queries{queryType,handler,zone}`, `gravity_dns_query_duration`) and tsdb
counters. If none reply -> empty NOERROR reply.

| Handler | Behaviour | Config |
|---|---|---|
| `int_soa` (implicit) | if zone `Authoritative` and qtype SOA: get-or-create `@/SOA/0` with defaults (serial 1337, refresh 600, retry 15, expire 5) | – |
| `etcd` | direct etcd `Get` for `/zone/<rel>/<TYPE>` (prefix), then wildcard search replacing labels left-to-right with `*`, then CNAME lookup + recursive resolution through `rootHandler`; returns nil if no answer of the queried type and more handlers follow | – |
| `memory` | same algorithm but `lookupKey` reads the zone's `Watcher` map (prefix queries iterate the map) | – |
| `forward_ip` | `dns.Client.ExchangeContext` to a **random** resolver from `to` (2s timeout, `net` udp/tcp/tcp-tls); SERVFAIL on error; NXDOMAIN if 0 answers; **writes every answer RR into etcd** as a cached record with uid=index and etcd lease=TTL (`cache_ttl`: 0 = use answer TTL, -1 = never, -2 = forever/import mode) | `to[]`, `cache_ttl`, `net` |
| `forward_blocky` | embeds a full **blocky** `server.Server` (see §6); falls through on NXDOMAIN; caches answers to etcd like forward_ip | `to[]`, `blocklists[]`, `allowlists[]`, `config` (raw blocky YAML), `cache_ttl` |
| `coredns` | starts a **CoreDNS caddy instance** from a Corefile string, extracts the `dnsserver.Server` via `reflect`/`unsafe`, calls `ServeDNS`; k8s_gateway plugin compiled in; must use a port != 53 | `config` (Corefile) |

Design consequence: the "DNS cache" is etcd itself. Every forwarded answer becomes a write
to the Raft log replicated to all nodes, with a lease per record. That gives the
"replicated cache" marketing point but makes cache fill cost ~ a quorum write per RR, and
TTL expiry is etcd lease expiry granularity. Cache-hit path is the `memory` handler.

### 5.5 Concrete flow: new record from API to answer on every replica

1. `POST /api/v1/dns/zones/records?zone=example.com.&hostname=www&uid=` body
   `{"type":"A","ttl":300,"data":"10.0.0.5"}` hits the swaggest usecase `dns.put_records`
   on whichever node the client called (any node runs the `api` role; auth via session
   cookie/bearer token/OIDC JWT; permission check on path+method).
2. Handler `Get`s `/gravity/dns/zones/example.com.` to validate the zone exists, builds a
   `Record`, ensures trailing dot for PTR/CNAME, and `Put`s
   `/gravity/dns/zones/example.com./www/A` = `{"data":"10.0.0.5","ttl":300}` (no lease,
   `expiry=-1`). A uid would add a 4th segment.
3. The local etcd client sends the Put to the etcd leader (gRPC); the leader appends to
   Raft, replicates to followers, commits on quorum, applies to the MVCC store; the API
   returns 200 only after commit.
4. Every node's DNS role has a `Watcher` on `/gravity/dns/zones/` (zones) and, per zone,
   a `Watcher` on `/gravity/dns/zones/example.com./` with `WithPrefix` + `KeyFunc` that
   strips the uid. etcd pushes the PUT event over each node's watch stream; the watcher
   constructs a `Record` (`recordFromKV`), merges it into
   `entries["/dns/zones/example.com./www/A"][uid]`, increments `gravity_dns_records{zone}`.
   Latency = Raft commit + watch delivery (ms on LAN).
5. A query `www.example.com. A` arrives on any node: `rootHandler` picks zone
   `example.com.`; handler chain `[memory, etcd, ...]`: `memory` looks up key
   `/dns/zones/example.com./www/A` in the map -> builds `dns.A{Hdr{Name:
   "www.example.com.", Ttl:300}, A:10.0.0.5}`; reply with AA if zone authoritative. If the
   watcher had not yet caught up, `etcd` handler would do a linearizable `Get` and find it.
6. Deleting: `DELETE .../records?zone&hostname&type&uid` -> etcd `Delete` -> DELETE watch
   event -> entry removed from every node's map.

No serial bump, no NOTIFY, no secondaries: replication *is* etcd.

### 5.6 DHCP -> DNS integration

On `roles.dhcp.lease.put` with a hostname and scope `dns.zone` configured, the DNS role
creates `<hostname>/A/<mac>` in the forward zone and
`<reverse-label>/PTR/<mac>` in the matching `in-addr.arpa.` zone, TTL = zone default,
**etcd lease = the DHCP lease's lease ID** (via `RelatedObjectOptions`) so forward and
reverse records expire together with the DHCP lease. Reservations (`expiry=-1`) produce
permanent records.

### 5.7 Importers

`pkg/convert/bind` (zone file via miekg `ZoneParser`), `pkg/convert/opnsense` (Unbound
host overrides XML), `pkg/convert/ms_dhcp` (netsh dump). Exposed as
`POST /api/v1/dns/zones/import {type:"bind"|"opnsense", payload, zone}` and
`gravity cli convert bind <file>`. Importers call the **public API client against the
same node** (the `APIConfigMiddleware` copies the inbound Authorization/Cookie headers
into an `api.Configuration` in context) — a slightly odd self-call pattern.

### 5.8 external-dns webhook provider

Separate binary/image implementing the external-dns webhook OpenAPI (generated
`go-server`). `GET /records` lists all records of the filtered zones via the Gravity API,
grouping by (name,type) and merging targets; `POST /records` applies `Changes` by
delete+put per target with uid `external-dns-<dnsName>-<target>`. Env: `GRAVITY_URL`,
`GRAVITY_TOKEN`, `DOMAIN_FILTER`. Status "alpha" in docs. Issue #1920 (wrong content
type, fixed in 0.30.0) and #2434-era TTL fix in 0.33.0 show it is lightly used.

---

## 6. Blocking: the blocky embedding

Gravity does **not** implement blocking itself. `forward_blocky` constructs
`github.com/0xERR0R/blocky v0.35.0` `config.Config` programmatically and starts a
`server.NewServer(ctx, cfg)` **in-process** (no sockets; it calls `srv.OnRequest(ctx, w,
msg)` directly). Per zone — so N blocky instances if N zones use it, each with its own list
cache (a memory hazard; docs warn "200 zones with Blocky and CoreDNS will increase
requirements").

Generated config when no raw `config:` is given:
- `upstreams.groups.default` = parsed `to[]`; timeout 2s; `bootstrapDns` = `FALLBACK_DNS`.
- `blocking.blockType = zeroIP`; `denylists.default` = 6 default lists (AdGuard SDNS,
  AdguardDNS, Easylist, StevenBlack, adaway, big.oisd.nl) that are **downloaded at Docker
  build time, embedded with go:embed, and served to blocky over a loopback HTTP server on
  127.0.0.1:8100** (`handler_forward_blocky_listserver.go`) so startup doesn't need
  internet; user-supplied `blocklists[]` replace the defaults (http(s) URLs or inline
  domain text); `allowlists[]`; `clientGroupsBlock.default=[default]`; download attempts 3.
- Logging: blocky's own query log disabled (`QueryLogTypeNone`), level fatal; metrics:
  blocky's private registry is reached via `//go:linkname blockyReg
  github.com/0xERR0R/blocky/metrics.reg` and exposed on `/metrics` alongside Gravity's.
- Startup is async (`go waitForStart()`); until blocky is ready the handler returns nil so
  queries fall through to the next handler (`forward_ip`) **unblocked**.

Limitations surfaced in issues: no per-client groups exposed (#598), no UI to see which
list blocked a query (#598), blocklist state is per-node and not replicated, list refresh
cadence is blocky's default, no custom block/allow rules UI, no "why was this blocked"
(#964 asks where blocking sits in the chain). List download IPv6-only failure (#846).

---

## 7. DHCP role (`pkg/roles/dhcp`)

- Server loop adapted from CoreDHCP (`handler4.Serve` with `ipv4.PacketConn`, one per
  listen IP/interface). IPv4 only (no DHCPv6).
- **Scope** = L2 network: `subnetCidr`, `options[]` (`tagName|tag` + `value|value64|
  valueHex`), `ttl` (default 7d), `default` flag, `ipam{type:internal, range_start,
  range_end, should_ping}`, `dns{zone, search[], addZoneInHostname}`, `hook`. Scope
  selection: longest-prefix match with biases (+2 client IP, +1 giaddr/relay, +0 local
  interface IP), fallback `default`.
- **Lease** keyed by device identifier (MAC/client-id): `address, hostname,
  addressLeaseTime, scopeKey, dnsZone, expiry (-1 = reservation), description`. Stored
  with an etcd lease == DHCP lifetime, so leases vanish from etcd (and from every node's
  cache, and the linked DNS records) on expiry.
- **IPAM** interface (`NextFreeAddress, IsIPFree, UseIP, FreeIP, UpdateConfig,
  UsableSize`) with one implementation `internal`: linear scan from `range_start`, checks
  the replicated lease cache, optional ICMP ping; guarded by an **etcd distributed mutex**
  `/dhcp/ipam/<scope>` (5s timeout, falls back to local mutex). DISCOVER creates the lease
  with a 30s `leaseNegotiateTimeout` via `Txn(If CreateRevision==0)` (create-if-absent) —
  added in 0.29.0 after #2148 (two nodes answering the same DISCOVER). Still **all nodes
  answer DHCP** on a shared L2 (issue #493 asks for leader election; docs say "Gravity
  assumes to be the main and only DHCP server").
- Options encoded via `insomniacslk/dhcp`. Defaults: subnet mask from CIDR, DNS server =
  responding instance IP, hostname echo (+zone), lease time = scope TTL.
- Wake-on-LAN from UI (`POST /dhcp/scopes/leases/wol`), MAC OUI vendor lookup (tshark manuf
  DB embedded), importer from Microsoft DHCP (`netsh` export).
- Kubernetes: DHCP broadcast doesn't traverse CNI; docs recommend a relay on the router
  pointing at the LoadBalancer IP.

Open DHCP issues are the largest bug cluster: reservations not honoured (#880, #872,
#1535), reservation cross-scope collisions (PR #2387 open), Windows clients (#776),
`value64` encoding (#768), default scope hijacking (#1077), global options (#1076).

---

## 8. Other roles

| Role | What | Notes |
|---|---|---|
| **api** | gorilla/mux + `swaggest/rest` web service; serves UI, OpenAPI, auth, cluster export/import, node logs (last 300 zap entries from an in-memory ring), tools (ping/traceroute/portmap via nmap); also an **unauthenticated unix socket** `/var/run/gravity.sock` with full admin perms for the CLI | config `{port:8008, oidc, cookieSecret(auto), sessionDuration, listenOverride}` |
| **monitoring** | separate HTTP server :8009: `/metrics` (default registry + blocky registry), `/healthz/live` (always 200), `/healthz/ready` (200 after all roles started) | used for k8s probes |
| **tsdb** | in-etcd metrics: roles dispatch `tsdb.inc/set`; every `scrape` (30s) the role writes `/tsdb/<key>/<node>/<ts>` with lease `expire` (30 min); `GET /api/v1/tsdb/metrics?role&category&node&since` powers UI charts (DNS requests per zone/handler, CPU, memory, DHCP usage) | replaces a real TSDB for the dashboard; writes N keys per node per 30s into Raft |
| **backup** | cron (`0 */24 * * *`) etcd `Snapshot()` -> `$DATA/backup/gravity-local-snapshot` always; optional S3 upload via minio-go (`endpoint,bucket,path,accessKey,secretKey`, falls back to env/EC2 creds); path `<path>/gravity/<version>/<instance>/gravity-snapshot-Y_M_D`; status in etcd + Prometheus gauges; triggered before join/remove | restore = `gravity cli snapshot restore` (etcdutl) + manual dir rename + `member update --peer-urls`; cluster membership not in snapshot |
| **tftp** | `pin/tftp` server; reads/writes etcd keys namespaced by client IP or `shared/`; `bundled/` = embedded iPXE/netboot.xyz binaries; `local/` = node-local dir if `enableLocal` | 10 MB cap (etcd MaxRequestBytes) |
| **discovery** | periodic `nmap -sn` ping scan per subnet (needs raw sockets / root), devices stored with TTL, "apply" creates DNS record and/or DHCP lease; scan guarded by etcd mutex so only one node scans | requires `user: root` in compose; disabled by `LISTEN_ONLY` |
| **debug** | :8010 pprof, fgprof, sentry test; `-tags=debug` image bundles delve (`gravity cli debug dlv attach 1`) | |
| **etcd** | §4 | |

---

## 9. REST API

- Framework: `swaggest/rest` + `swaggest/usecase` + `swaggest/openapi-go`. Each endpoint is
  a `usecase.NewInteractor(func(ctx, input, *output) error)` with struct tags (`query:`,
  `json:`, `required:`, `maxLength:`) that **generate the OpenAPI 3.0.3 schema at runtime**
  (`GET /api/v1/openapi.json`) and at build time (`gravity generateSchema schema.yml`).
  Errors use `usecase/status` codes mapped to HTTP (`InvalidArgument`->400, `NotFound`->404,
  `Unauthenticated`->401, `Unavailable`->503, `Internal`->500) with a `RestErrResponse`.
- Clients generated by `openapi-generator-cli` (Go `api/` committed in-repo; TypeScript
  `gravity-api` published to npm and consumed by the web UI; `go get
  beryju.io/gravity@vX`). `make release` regenerates both and publishes npm.
- RapiDoc UI served at `/api/` and Swagger in Hugo docs (`/schema.yaml`).
- Style: **flat, non-RESTful** — resources identified by query params, `POST` = upsert,
  `DELETE` with query params, no pagination, no filtering beyond name/hostname/type/uid, no
  ETags/optimistic concurrency, no bulk endpoints (UI bulk delete loops).
- Full path list (43 paths):
  `auth/{config,login,me,tokens,users}`, `backup/{start,status}`,
  `cluster`, `cluster/{export,import,instance,node/logs,roles/restart}`,
  `dhcp/scopes`, `dhcp/scopes/{import,leases,leases/wol}`,
  `discovery/{devices,devices/apply,devices/delete,subnets,subnets/start}`,
  `dns/zones`, `dns/zones/{import,records}`,
  `etcd/{join,members,move_leader,remove}`,
  `roles/{api,backup,dhcp,discovery,dns,monitoring,tftp,tsdb}` (GET/POST role config),
  `tftp/files`, `tftp/files/download`, `tools/{ping,portmap,traceroute}`, `tsdb/metrics`.
- Example shapes:
  ```yaml
  # GET /api/v1/dns/zones -> {zones:[{name,handlerConfigs,defaultTTL,authoritative,hook,recordCount}]}
  # POST /api/v1/dns/zones?zone=example.com. {handlerConfigs,defaultTTL,authoritative,hook}
  # GET /api/v1/dns/zones/records?zone=&hostname=&type=&uid= -> {records:[{uid,fqdn,hostname,type,ttl,data,mxPreference,srv*,soa*}]}
  # POST /api/v1/dhcp/scopes?scope=lan {subnetCidr,options,ttl,default,ipam,dns,hook}
  # GET /api/v1/dhcp/scopes/leases?scope=lan -> {leases:[{identifier,address,hostname,addressLeaseTime,scopeKey,dnsZone,expiry,description,info{vendor,...}}]}
  # GET /api/v1/cluster -> {clusterVersion, clusterVersionShort, instances:[{version,roles,identifier,ip,memoryBytes}]}
  ```
- Rough edges found: `recordCount` computed by fetching **all record keys of all zones**
  on every zones list; role-config GET returns the in-memory config including
  `cookieSecret`/OIDC `clientSecret` to any user with GET permission.

---

## 10. Auth, users, RBAC

- **Local users**: bcrypt password, stored as protobuf in etcd. Default `admin` created on
  first start with random password printed to stdout (or `ADMIN_PASSWORD`), optional
  `ADMIN_TOKEN`. `gravity cli users add <name>` / `tokens add <user>`.
- **API tokens**: random key; the key string is the etcd key under `/api/tokens/` (lookup
  by exact key; **stored in plaintext**, exported by non-safe export). `Authorization:
  Bearer <token>`.
- **OIDC** (`coreos/go-oidc` v3, authorization-code flow, no PKCE visible): role config
  `oidc{clientID, clientSecret, issuer, redirectURL (supports $INSTANCE_IDENTIFIER /
  $INSTANCE_IP placeholders), scopes[], tokenUsernameField}`. Callback reads `email`
  claim -> session user = email with **full admin permissions `/* *`** (no group/claim ->
  role mapping; every OIDC login is admin). When OIDC is configured the UI auto-redirects
  to SSO; `?local` bypasses. Provider discovery retried 50x with backoff at role start.
- **OIDC JWT bearer**: if OIDC configured, a bearer JWT verified by the provider is
  accepted; the claim named `tokenUsernameField` must match an existing local user record
  (so permissions come from the local user).
- **Sessions**: gorilla/sessions backed by **etcd** (`api7/etcdstore`), cookie secret
  auto-generated and persisted into the API role config; MaxAge from `sessionDuration`
  (24h default). Works across nodes.
- **Permissions**: per-user list of `{path, methods[]}`; longest-matching path (exact or
  `prefix*`) wins; `*` method wildcard. Checked in `AuthProvider.ServeHTTP` for everything
  under `/api` except `auth/me|config|login` and `openapi.json`. No named roles, no groups,
  no object-level scoping (e.g. "zone X only" is possible only because zone is a query
  param and the path doesn't include it — i.e. **not** possible). Docs example for
  "read-only": `[{"path":"/*","methods":["get","head"]}]`.
- Unix socket bypasses auth entirely (synthetic user `gravity-socket`).

---

## 11. Web UI

- **Stack**: TypeScript, **Lit 3** web components, **PatternFly 4** CSS (`@patternfly/
  patternfly` 4.224), Chart.js 4 (+date-fns adapter), CodeMirror 6 (handler YAML, hook JS,
  compose output), RapiDoc, `yaml`, `ip-num`. Hash-based custom router
  (`ak-router-outlet`, `Route(RegExp)` with lazy `import()` per page). Tooling: rollup 4 +
  esbuild, oxlint/oxfmt, `lit-analyzer`, TypeScript 6. Heavily derived from the **authentik**
  admin UI (same author; elements are prefixed `ak-`, config packages are
  `@goauthentik/oxlint-config`).
- **Build & embed**: `make web-build` -> `web/dist/`; `web/static.go` has
  `//go:embed dist/*`; the API role serves `/ui/` from a tiny Go-templated `index.html` and
  `/ui/static/*` from the embedded FS (or `./web/dist` on disk in `DEBUG`). Docker stage 1
  builds the UI, stage 3 copies `dist/` into the Go build context. Single binary, no CDN.
- **Pages**: Overview (cards: instance, version, backup, DNS zones, DHCP scopes, admin
  status; charts: DNS requests, CPU, memory, DHCP usage table), DNS Zones -> Records
  (table with search, per-zone wizard with presets, structured handler editor added in
  0.30.0 replacing raw YAML), DHCP Scopes -> Leases (wizard, WoL button, vendor from OUI),
  Discovery (subnets, devices, apply), TFTP files, Cluster (Nodes, Node logs, Roles config
  forms per role, Join wizard emitting docker-compose), Auth (Users, Tokens), Tools
  (ping/traceroute/portmap). Dark mode via `prefers-color-scheme` (issue #796 asked for a
  toggle).
- **No query log viewer** (only the 300-line node log ring with JSON fields), no per-client
  stats, no "top blocked domains" — the dashboard is about cluster/roles, not DNS
  analytics. The charts come from the in-etcd `tsdb` role (30-min retention).

---

## 12. CLI

Single binary; `gravity cli` (cobra) talks to the API via `unix:///var/run/gravity.sock`
by default (`-s/--host`, `-t/--token` for remote http). Subcommands: `health`, `export
[file] [--safe]`, `import <file>`, `users add <name>`, `tokens add <user>`, `convert
{bind,ms_dhcp,opnsense} <file>`, `snapshot {status,restore}` (etcdutl embedded),
`etcdctl ...` (**the entire etcdctl command tree linked in via `//go:linkname` to etcdctl's
unexported `rootCmd`**), `debug dlv` (debug image only). Bash completion installed in the
image. Separate `gravity-cli` binaries for darwin/linux amd64/arm64 attached to releases.
There is no declarative "apply a YAML of zones" command; config-as-code is only the raw
key export/import JSON.

---

## 13. Observability

- **Logs**: zap JSON to stdout; `LOG_LEVEL=info,etcd=error,dhcp=debug` per-role levels
  (0.28.2+); query logging at Info is on by default (compose caps json-file logs). In-memory
  ring of 300 entries served via API/UI per node.
- **Metrics**: Prometheus on :8009. Named: `gravity_dns_queries{queryType,handler,zone}`,
  `gravity_dns_query_duration`, `gravity_dns_records{zone}`, `gravity_dhcp_requests`,
  `gravity_watcher_events`, backup status/size/duration, instance role gauges, hook
  durations, plus full etcd server metrics and blocky metrics. `extras/prometheus/rules.yaml`
  has etcd alert rules (no leader, even member count, leader churn, gRPC failures);
  `extras/grafana/dashboard.json`.
- **Tracing/profiling**: Sentry transactions/spans throughout (DNS request, handler,
  etcd get, watcher load…); optional Pyroscope continuous profiling (`PYROSCOPE_*`,
  0.30.0); pprof/fgprof on :8010; delve in `-debug` images; CI benchmark regression gate
  (`make bench`, alert at 200%).

---

## 14. Configuration (environment variables, verified from `pkg/extconfig/config.go`)

| Var | Default | Purpose |
|---|---|---|
| `BOOTSTRAP_ROLES` | `dns;dhcp;api;etcd;discovery;backup;monitoring;tsdb;tftp` | roles on first start (persisted to etcd afterwards; `debug` opt-in) |
| `LOG_LEVEL` | `info,etcd=error` | root level + `role=level` pairs |
| `DATA_PATH` | `./data` | etcd dir, certs, backup, tftp local |
| `INSTANCE_IDENTIFIER` | hostname | etcd member name, instance key |
| `INSTANCE_IP` | auto-detected | comma/pipe-separated list; first = primary (advertised) |
| `INSTANCE_INTERFACE` | derived from IP | DHCP interface |
| `INSTANCE_LISTEN` | – | override listen address (set `0.0.0.0` in k8s/bridge Docker) |
| `LISTEN_ONLY` | false | don't answer DHCP, don't run discovery |
| `FALLBACK_DNS` | `1.1.1.1:53` | root zone forwarder + blocky bootstrap |
| `IMPORT_CONFIGS` | – | `file:///x.json|base64...` applied on first start |
| `ETCD_PREFIX` | `/gravity` | namespace |
| `ETCD_ENDPOINT` | `localhost:2379` | client endpoint (external etcd) |
| `ETCD_CLIENT_PORT` | **2381** in code (docs say 2379) | advertised client port |
| `ETCD_PEER_PORT` | `2380` | |
| `ETCD_JOIN_CLUSTER` | – | `<token>,http://<node>:8008` |
| `ADMIN_PASSWORD`, `ADMIN_TOKEN` | – | first-start bootstrap |
| `SENTRY_ENABLED`/`SENTRY_DSN` | false / vendor DSN | |
| `PYROSCOPE_ENABLED/SERVER/USERNAME/PASSWORD` | false | |
| `DEBUG`, `CI` | false | dev mode (console logs, UI from disk, socket in cwd) |

Everything else (ports, OIDC, S3, DHCP timeouts, tsdb intervals) is **role config stored
in etcd**, edited via UI/API, and takes effect by restarting the role on all nodes. There
is **no config file**. Env quoting pitfall (#2084): values with quotes crash-loop.

---

## 15. Deployment

### Docker / compose (official)
```yaml
services:
  gravity:
    hostname: gravity1          # becomes INSTANCE_IDENTIFIER; must be stable+unique
    image: ghcr.io/beryju/gravity:stable
    restart: unless-stopped
    network_mode: host          # for DHCP broadcast + binding INSTANCE_IP
    user: root                  # nmap raw sockets / port 67
    volumes: [data:/data]
    logging: {driver: json-file, options: {max-size: "10m", max-file: "3"}}
```
Image: debian:stable-slim + nmap + bash-completion; non-root `/data` ownership but runs as
root by default. Requirements: 1 CPU, 1 GB RAM; amd64/arm64 only (armv7 unsupported, #510).

### Kubernetes (official docs = one YAML, no Helm chart, no operator, no CRDs)
- Single node: `StatefulSet` (1 replica, PVC 1Gi at `/data`) + `Service type:
  LoadBalancer` exposing 53 UDP/TCP, 67, 69, 8008; env `BOOTSTRAP_ROLES` without etcd?
  (the sample sets `dns;dhcp;api;discovery;backup;monitoring;tsdb;tftp` i.e. **no `etcd`
  role, yet no `ETCD_ENDPOINT`** — the sample as written would try `localhost:2379`;
  UNVERIFIED whether the doc intends embedded etcd), `INSTANCE_IP=<LB IP>`,
  `INSTANCE_LISTEN=0.0.0.0`; probes on `http-metrics` `/healthz/live|ready`.
- Multi-node: "do not use the built-in etcd"; use an etcd operator or external etcd, point
  `ETCD_ENDPOINT` at it. No leader election, no CRDs, no RBAC objects, no NetworkPolicy, no
  PodDisruptionBudget guidance. The *only* k8s-native integration is the external-dns
  webhook sidecar (Helm values snippet in docs) and the compiled-in CoreDNS `k8s_gateway`
  plugin (usable through the `coredns` handler, UNVERIFIED in practice).
- Issues: #837 (StatefulSet cluster bootstrap impossible with join tokens), #927 (etcd
  can't bind LB IP across clusters), #550 (host port 2380 conflicts with node etcd),
  #652 (which ports / hostNetwork?), #1949 (nodes page 404 with external etcd).

### Release / CI
- `release.yml` (manual `workflow_dispatch` with version) bumps Makefile, regenerates
  schema + Go/TS clients + external-dns server stubs, publishes npm `gravity-api`, tags,
  drafts GitHub release with auto notes. `release-publish.yml` builds `:stable` + `:vX`
  images for both binaries with SBOM/provenance, uploads CLI binaries, runs benchmarks.
- Cadence (from tags/releases): 100+ tags since 2022; 2025: 0.26 (Jan) 0.27 (May,
  etcd 3.6, external-dns) 0.28 (Sep, join robustness, member remove) 0.28.7 (Oct);
  2026: 0.29 (Jan 3, IPAM rework) 0.30 (May 3) 0.32 (May 3-6) 0.33 (May 9). Nothing
  released May–Oct 2026; main receives mostly dependabot PRs (3 bumps on 2026-10-09).
  Effectively a **single-maintainer project (BeryJu, author of authentik)** with bursts.
- Tests: unit tests need a real etcd (`hack/tests/docker-compose.yml` with etcd 3.7.2,
  seaweedfs S3, CoreDNS, nginx, **dex** for OIDC); e2e builds a coverage-instrumented image
  and runs DHCP relay/client containers; codecov; golangci-lint; CodeQL.

---

## 16. Pain points from GitHub issues (searched: etcd, cluster, memory, performance, kubernetes, blocky, helm, DNSSEC, DoT, AXFR; 44 open issues)

Clustering / etcd
- #894, #1093, #1389 (open/closed 2024): join silently does nothing or main instance
  boot-loops; the join-failure path wipes `member/` and starts a fresh single-node cluster.
- #837: no way to bootstrap N replicas declaratively (StatefulSet) — join is imperative
  and token-based.
- #927: `INSTANCE_IP` must be a local bind address, so multi-cluster/LB-IP peers can't
  work; embedded etcd advertises the same IP it binds.
- #1949 (open): UI depends on `/api/v1/etcd/members` which only exists with embedded etcd.
- #533/#1383: node removal/disabled indicator missing (remove added in 0.28.0 via API).
- #1703: request for Consul as alternative KV — the storage layer is etcd-specific
  (leases, txn, watch, concurrency mutex, snapshot), so not pluggable.
- #2148: stale watcher cache allowed two nodes to downgrade a lease to the 30s negotiate
  TTL — classic multi-writer race on replicated-cache designs.
- Memory: 0.27.0 notes etcd 3.6 "up to 50% lower memory"; #843 regex blocklists "memory
  made it untenable"; each blocky-handler zone holds its own list copies.

DNS behaviour
- #1085, #838, #932, #1950, #1985, #1464: CNAME handling bugs (recursion flag, wildcard A
  returned for CNAME query, CNAME->AAAA), SRV timeouts after 0.27 (#1682), AAAA-only
  answers (#1078), HTTPS/SVCB records unsupported so browsers bypass overrides (#1335),
  no recursive resolver (#599), no DoT (#808), no DNSSEC (0 issues — users don't expect it),
  no default/global forwarder: every zone must repeat `forward_*` config (#918).
- Blocking: #598 better list management (per-domain allow/deny, which list matched),
  #964 unclear where blocking sits in the chain, #846 IPv6-only download failure, #513
  forward_blocky skipped when `to` equals forward_ip's.

DHCP
- Reservations unreliable (#880, #872, #1535, PR #2387 open), Windows clients (#776),
  option encoding (#768), default scope steals clients (#1077), global options (#1076),
  TTL mismatches (#523), HA leader election wanted (#493), locally-administered MAC filter
  (#2047).

Ops / docs / UI
- #489 (7 comments, open since 2023): configuration docs too thin, want copy-paste examples
  and config-as-code; #1169 token auth docs; #2353 default credentials not visible in logs;
  #2084 quoted env values crash; #959 discovery needs raw-socket caps; #899 LXC/FUSE UI
  timeouts; #1258/#1329 UI validation breakage after schema changes (required `hook`).
- Performance search returned 0 issues; nobody reports qps problems (home/SMB scale).

---

## 17. What kudurru should borrow

1. **Single binary + role model.** Roles registered via init, constructed once, started/
   stopped on config change, each role owning its API routes via an in-process "mux setup"
   event. Clean, testable, and the same binary serves Docker single-node and k8s.
2. **Instance info with a lease** (`/instance/<id>` bound to an etcd session) — cheap,
   correct membership/liveness list; plus "cluster version = min(node versions)" driving
   **version-gated migrations that hook the storage client** during mixed-version rolls.
3. **Generic list+watch cache (`Watcher[T]`)** as the replication primitive, but add
   compaction/resync handling, and treat it as a *read cache with a single writer per
   object* (see avoid #1).
4. **etcd key layout ideas**: zone as a key, records as children
   `<zone>/<name>/<TYPE>/<uid>` with per-record leases for dynamic/DHCP/cache data; link
   DHCP lease and DNS records to the *same* etcd lease so they expire together.
5. **Handler-chain-per-zone** (ordered pluggable resolvers returning "no answer -> next")
   is a nice unification of authoritative/forward/filter behaviours and maps well onto
   kudurru's "handlers/plugins". Keep the registry pattern; add typed configs.
6. **Longest-suffix zone routing with a root `.` zone** as the default forwarder; wildcard
   search by progressively replacing labels.
7. **Join via the application API, not raw etcd** (token with a single-permission system
   user, returns `initialCluster`; add-as-learner then promote when caught up; take a
   snapshot before any membership change). kudurru should keep this UX *and* add a
   declarative path (see avoid #3).
8. **Runtime-generated OpenAPI from typed usecases** + generated TS/Go clients consumed by
   the UI and CLI (`swaggest` works; kudurru could also use huma/ogen). Serve RapiDoc.
9. **Replicated sessions in the KV store** and OIDC via go-oidc with
   `$INSTANCE_IDENTIFIER` placeholders in redirect URL (multi-node SSO without sticky
   sessions).
10. **Unix-socket admin API for the CLI** (plus bash completion in image) and bundling
    `etcdctl`/`etcdutl` for ops.
11. **Build-time embedded default blocklists served to the blocker over loopback** so first
    boot works offline; expose the embedded library's metrics registry.
12. **Kubernetes probes split from the API port** (`/healthz/live|ready` on the metrics
    port), readiness only after all roles start.
13. **Cluster export/import as key/value JSON** with a `safe` mode that strips secrets, and
    `IMPORT_CONFIGS` for first-boot seeding; importers for BIND/OPNsense/MS DHCP.
14. **JS hooks (goja)** for DHCP option tweaks (UniFi option 43, iPXE) — very relevant to
    the UniFi integration requirement; kudurru should cache compiled programs.
15. **Per-role log levels** (`LOG_LEVEL=info,dns=debug`), structured query log line.
16. **Docs structure**: per-role `_index.md` + `role_config.md`, migration guides, Hugo/Docsy.

---

## 18. What kudurru should avoid

1. **Using etcd as the DNS cache.** Every forwarded RR becomes a quorum write with a lease;
   cache fill throughput is bounded by Raft, and the "replicated cache" causes the CNAME/
   AAAA/SRV bugs (answers reassembled from individually cached RRs). Keep caches local
   (per node, in memory) and replicate only authoritative/config data.
2. **Multi-writer replicated state without ownership.** All nodes answer DHCP and write
   leases; races needed a distributed mutex + create-if-absent txn and still produced
   #2148. kudurru: elect a *writer* (leader election via k8s Lease or etcd election) for
   DHCP/IPAM per scope, or shard ownership; dnsdist/keepalived VIP can steer traffic.
3. **Imperative-only clustering.** No declarative bootstrap (StatefulSet with N replicas
   can't self-form a cluster), join failure silently forks a new cluster, nodes page
   breaks with external etcd. kudurru: support `--initial-cluster`/peer discovery via
   headless Service DNS, make external etcd first-class (TLS/mTLS/auth), never fall back
   to "new cluster" on join failure.
4. **Global role config keyed as `/instance/role/<role>`** — misleading name and no
   per-node overrides (ports, interfaces, listen addresses differ per node in practice).
   Separate cluster-wide vs node-scoped config explicitly.
5. **Untyped `map[string]interface{}` handler configs** parsed ad hoc (string-or-array `to`,
   int-or-string `cache_ttl`), and raw YAML pass-through to blocky/CoreDNS. Typed, validated
   schemas per handler; surface validation errors in the API/UI (Gravity logs a warning and
   silently skips the handler).
6. **Zones without types/metadata**: no SOA/serial management (hard-coded serial 1337,
   expire 5s), no NS records, only 8 RR types, no AXFR/IXFR/NOTIFY, no DNSSEC, no secondary
   zones. kudurru's boundary-stone metaphor demands first-class primary/secondary with
   serials, transfers and TSIG.
7. **Flat RBAC on URL paths** with secrets (cookie secret, OIDC client secret, S3 keys)
   returned by GET role-config, plaintext API tokens as etcd keys, OIDC logins auto-admin
   with no claim/group mapping, an unauthenticated socket with admin rights. kudurru:
   named roles/groups, OIDC group->role mapping, hashed tokens, secret redaction, object-
   level scoping (zone-level permissions).
8. **No query log store / analytics**: only a 300-line in-memory log ring and a 30-minute
   in-etcd "tsdb" that writes N keys per node every 30s into Raft. Use a proper local
   store (e.g. SQLite/DuckDB/Parquet per node or ClickHouse/Prometheus remote) for the
   Pi-hole-style dashboard; keep etcd for config.
9. **One embedded blocky instance per zone** and per-node list state; no UI for which list
   matched, no per-client groups. Model blocking as a first-class cluster-wide object
   (lists, groups, client assignments) and apply it once in the pipeline.
10. **Per-request goja VM creation**, `reflect/unsafe` into CoreDNS, `go:linkname` into
    etcdctl/blocky internals — fragile across dependency upgrades.
11. **Hard-coded vendor Sentry DSN** in the binary; opt-in telemetry must be clearly
    separate and empty by default.
12. **Watch loop without compaction/resync handling**; one crash of the watch stream =
    stale node until restart.
13. **Running as root with host networking by default** for nmap/DHCP. Provide
    capability-scoped (`CAP_NET_RAW`, `CAP_NET_BIND_SERVICE`) images and make discovery
    optional.
14. **`recordCount` by scanning all keys**, zone list = full prefix read — design list
    endpoints with pagination and counters maintained incrementally.
15. **GPL-3.0 code**: do not copy; re-implement.

---

## 19. Open questions / UNVERIFIED

- Whether `ETCD_ENDPOINT` can target a TLS/auth-protected etcd (code shows no TLS config
  path; likely not).
- Intended etcd mode in the single-node Kubernetes manifest (sample omits `etcd` from
  `BOOTSTRAP_ROLES` but sets no `ETCD_ENDPOINT`).
- Real-world qps/latency numbers (no benchmarks published beyond CI go-bench history at
  https://beryju.github.io/gravity/dev/bench/ — UNVERIFIED URL).
- Default blocklist refresh behaviour inside embedded blocky (uses blocky defaults; not
  configured by Gravity).
- Whether `coredns` handler + `k8s_gateway` is used by anyone; no issues reference it.
- Dependency licenses quoted in §0 are from general knowledge, not re-verified here.

## 20. Sources

- Clone: /tmp/kudurru-research/gravity/gravity (HEAD b873d1f, 2026-10-09)
- Files cited: README.md, LICENSE, go.mod, Makefile, Dockerfile, docker-compose.yml,
  schema.yml, docs/content/docs/**, cmd/**, pkg/extconfig/config.go, pkg/instance/*.go,
  pkg/storage/**, pkg/roles/{api,auth,dns,dhcp,etcd,backup,monitoring,tsdb,tftp,discovery,
  debug}/**, pkg/externaldns/*.go, web/package.json, web/rollup.config.mjs, web/src/App.ts,
  web/static.go, .github/workflows/{build,release,release-publish}.yml, hack/**, extras/**
- GitHub API: /repos/BeryJu/gravity, /releases, /tags, /search/issues (queries listed §16)
- https://gravity.beryju.io/ landing page
