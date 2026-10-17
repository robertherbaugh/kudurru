> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [dev-docs/design-record.md](../design-record.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# Research: integration / extension model for kudurru

Researched 2026-10-10. Sources: shallow clones under `/tmp/kudurru-research/integrations/`
(external-dns `33ef223` 2026-10-05, coredns `123829d` 2026-10-09, caddy `d4165a4`, go-plugin
`9d5088a` 2026-10-08, Technitium DnsServer `70495af` 2026-10-03, gravity `b873d1f` 2026-10-09,
cloudflare-go `05ca1e4` 2026-10-01, paultyng/go-unifi `12971e0` 2026-04-28, unpoller/unifi
`23842b3` 2026-10-07, libdns/{libdns,unifi,cloudflare}), the Go module proxy, the official
UniFi Network API OpenAPI document (`developer.ui.com/network/v10.6.106/openapi.json`, downloaded
and parsed locally), Cloudflare developer docs, and the sibling reports in this directory
(`gravity.md`, `technitium.md`, `pihole.md`, `blocking-engines.md`). Anything not verified from
those is marked **UNVERIFIED**.

---

## 0. Fast facts

| Project / lib | Version seen | License | Extension model | Relevance |
|---|---|---|---|---|
| kubernetes-sigs/external-dns | HEAD 2026-10-05; latest tag **v0.23.0** (2026-09-18, Go proxy) | Apache-2.0 | in-tree providers + **webhook provider** (HTTP sidecar) + registries (TXT/CRD/DynamoDB) | the de-facto out-of-process DNS-sync contract; TXT ownership design |
| coredns/coredns | HEAD 2026-10-09, go 1.26, 75 in-tree plugins | Apache-2.0 | compile-time `plugin.cfg` middleware chain | DNS request pipeline model |
| caddyserver/caddy v2 | HEAD 2026-10, go 1.26 | Apache-2.0 | compile-time module registry + JSON config namespaces, xcaddy | config-driven module loading pattern |
| hashicorp/go-plugin | **v1.8.0** (2026-04-29) | **MPL-2.0** (copyright now "IBM Corp.") | out-of-process subprocess + net/rpc or gRPC | isolation model for untrusted plugins |
| Technitium DnsServer | ApplicationCommon SDK **10.2**; server v15.x | **GPL-3.0** | in-process .NET assemblies implementing 8 hook interfaces, app store | richest DNS-specific hook set (ideas only, no code) |
| BeryJu/gravity | v0.33.0; HEAD 2026-10-09 | **GPL-3.0** | "roles" registered at init, config from etcd, goja JS hooks, external-dns webhook sidecar | role model + an existing external-dns adapter for a DNS server |
| cloudflare/cloudflare-go | **v7.12.0** (2026-10-01); module `github.com/cloudflare/cloudflare-go/v7`; go 1.22 | Apache-2.0 | Stainless-generated SDK | official Go SDK (v7, *not* v4/v5) |
| paultyng/go-unifi | archived (README banner) | MPL-2.0 | generated from controller JAR | legacy UniFi API models; use a fork |
| filipowm/go-unifi | **v1.11.2** (2026-06-27, Go proxy) | MPL-2.0 (fork of above, UNVERIFIED) | same | maintained fork used by terraform-provider-unifi forks |
| unpoller/unifi | **v6.3.1** (2026-10-07) | MIT | read-only client; legacy + Integration/v1 + cloud connector | best MIT option for *reading* UniFi inventory |
| libdns/libdns | **v1.1.1** (2025-08-13) | MIT | Go interface set for DNS record CRUD; ~100 providers | candidate "RecordSink" shape |
| libdns/unifi | 2026-09-10 | MIT | libdns provider over UniFi DNS-policy API | proves UniFi can be a record *sink* too |
| UniFi Network API (official) | spec **v10.6.106**, OpenAPI 3.1.0, 44 paths; versions published back to v9.1.120 | n/a | `X-API-Key`, `/proxy/network/integration/v1/...` | the supported inventory source |

---

## 1. Extension models compared

### 1.1 Technitium "DNS Apps" (in-process, typed hook interfaces)

From `DnsServerCore.ApplicationCommon/` (10 files, NuGet package version 10.2). Each app is a
folder `<config>/apps/<AppName>/` with DLLs + `dnsApp.config` (JSON with comments), loaded in an
isolated `AssemblyLoadContext`; every class implementing `IDnsApplication` is instantiated and
its other interfaces are discovered with `is` checks. Hook set (complete):

| Interface | Called when | Return |
|---|---|---|
| `IDnsApplication` (+`IDisposable`) | `InitializeAsync(IDnsServer, string? config)` on load and on every config save | - |
| `IDnsApplicationPreference` | ordering among apps on the same hook (`byte Preference`, lower first) | - |
| `IDnsRequestController` | before any processing: `GetRequestActionAsync(request, remoteEP, protocol)` | Allow / Drop |
| `IDnsRequestBlockingHandler` | blocking stage: can answer as blocked, report allowed, or pass | datagram or null |
| `IDnsAuthoritativeRequestHandler` | authoritative stage, before zones | datagram or null |
| `IDnsAppRecordRequestHandler` | only when an `APP` record (private type 65282) is hit; app gets `appRecordData` JSON + `ApplicationRecordDataTemplate` shown in UI | datagram |
| `IDnsPostProcessor` | after answer is built; may rewrite/drop | datagram |
| `IDnsQueryLogger` | async per-query log sink | - |
| `IDnsQueryLogs` | makes the app a *queryable* log backend for the console's Query Logs tab | `DnsLogPage` |

Host services given to the app (`IDnsServer : IDnsClient`): `DirectQueryAsync` (re-enter the
pipeline), `IsLocallyServedZone`, `WriteLog`, `ApplicationName/Folder`, `ServerDomain`,
`ResponsiblePerson`, `DnsCache`, `Proxy`, `IPv6Mode`, `UdpPayloadSize`.

Operational properties: apps run with full trust (the v15.3 security note removed "Apps: Delete"
from DNS Administrators because installing an app is privilege escalation); app exceptions are
caught and the query continues; store is a static JSON manifest (`apps2.json`) with per-version
`serverVersion` minimums; install/update/config via `/api/apps/*`; config replicated by the
cluster feature. See `technitium.md` §11 for the 27 official apps.

**Take-away:** the *hook taxonomy* (request-controller / blocking / authoritative / record-type
handler / post-processor / query-log sink / log reader) is the best DNS-specific list available.
The *delivery* (download arbitrary DLLs into the daemon) is the part to avoid.

### 1.2 CoreDNS plugins (compile-time middleware chain)

`plugin.cfg` is a plain ordered list `<name>:<package>`; `go generate` writes
`core/plugin/zplugin.go` (blank imports) and `core/dnsserver/zdirectives.go` (`var Directives =
[]string{...}`). Order is global and *is* the request pipeline: "Every plugin will feel the effects
of all other plugin below (after) them during a request, but they must not care what plugin above
them are doing." External plugins = edit `plugin.cfg`, rebuild. Current chain head to tail:
`root, metadata, geoip, cancel, proxyproto, quic, grpc_server, https, https3, timeouts,
multisocket, reload, nsid, bufsize, bind, debug, trace, ready, health, pprof, shed, prometheus,
errors, log, dnstap, local, dns64, siit, any, chaos, loadbalance, tsig, rewrite, autopath, acl,
cache, header, dnssec, tls, minimal, template, transfer, dynupdate, hosts, route53, azure,
clouddns, k8s_external, kubernetes, file, auto, secondary, etcd, loop, forward, grpc, erratic,
whoami, on, sign, view, nomad` (`plugin.cfg`).

Core interface (`plugin/plugin.go`):

```go
type Plugin func(Handler) Handler
type Handler interface {
    ServeDNS(context.Context, dns.ResponseWriter, *dns.Msg) (int, error)
    Name() string
}
```

Return-code contract: SERVFAIL/REFUSED/FORMERR/NOTIMP mean "nothing written yet"; any other
rcode means the response has been written. `NextOrFailure` chains to the next handler; a
`fallthrough [zones...]` directive lets a zone-owning plugin pass a subset of names on. Plugins
mutating a response must copy the `*dns.Msg` first. Context carries `Key` (server), `LoopKey`,
`HTTPRequestKey` for DoH. Per-server `Config.registerHandler` keeps a `registry` map so plugins can
find each other by name (`c.Handler("prometheus")`), and `Viewer` plugins add filter funcs in
`plugin.cfg` order (`register.go:178`). A newer `SetDirectives([]string)` lets an embedding host
replace the order at runtime before start (`core/dnsserver/directives.go`).

**Take-away:** a single, fixed, documented stage order is what makes CoreDNS plugins composable.
kudurru should fix its stage order in code and let integrations attach to named stages, not
reorder them.

### 1.3 Caddy modules (compile-time registry + config-driven instantiation)

`modules.go`:

```go
type Module interface{ CaddyModule() ModuleInfo }
type ModuleInfo struct { ID ModuleID; New func() Module }   // ID e.g. "http.handlers.reverse_proxy"
func RegisterModule(instance Module)                        // panics on dup / reserved "caddy","admin"
type Provisioner interface{ Provision(Context) error }
type Validator   interface{ Validate() error }
type CleanerUpper interface{ Cleanup() error }
type App interface{ Start() error; Stop() error }           // top-level long-running modules
```

Config is JSON; a parent struct declares where child modules plug in with struct tags, e.g.
`HandlersRaw []json.RawMessage `json:"handle" caddy:"namespace=http.handlers inline_key=handler"``
and calls `ctx.LoadModule(parent, "HandlersRaw")`, which reads the `handler` key of each JSON
object, looks up `namespace+"."+name` in the registry, `New()`s it, strictly unmarshals the JSON
into it, then runs `Provision`/`Validate`. Namespaces (`caddy.storage`, `http.handlers`,
`http.matchers`, `dns.providers`, `tls.issuance`...) are the discoverability surface
(`caddy list-modules`). xcaddy builds a binary with extra modules via `--with
github.com/org/mod[@ver][=path]` (it generates a `main.go` with blank imports and optional
`replace` directives); plugins are compile-time Go modules, never runtime loaded.

The DNS-provider namespace in Caddy (`dns.providers.*`) wraps **libdns** providers; this is the
same interface set listed in §1.7.

**Take-away:** "namespace + inline key + strict JSON unmarshal + Provision/Validate/Cleanup
lifecycle" is the cleanest config-to-instance pattern in Go; kudurru can use it for
`integrations: [{type: cloudflare, ...}]` in YAML and for CRD `spec` decoding identically.

### 1.4 HashiCorp go-plugin (out-of-process)

v1.8.0, MPL-2.0 (note: copyright line is "IBM Corp. 2016, 2026" after the HashiCorp acquisition;
license unchanged). Host launches the plugin binary as a subprocess; handshake via env-var "magic
cookie" + `ProtocolVersion` (or `VersionedPlugins map[int]PluginSet` for negotiation); transport is
net/rpc over yamux or gRPC over HTTP/2; `AutoMTLS` generates one-time certs so only the launching
host can talk to the plugin; `SecureConfig{Checksum, Hash}` verifies the binary; `Reattach` allows
host upgrade with plugin still running; `Discover(glob, dir)` finds binaries; stdout/stderr and
hclog are piped back; a `GRPCBroker` lets the plugin call back into the host (bidirectional).

```go
type GRPCPlugin interface {
    GRPCServer(*GRPCBroker, *grpc.Server) error
    GRPCClient(context.Context, *GRPCBroker, *grpc.ClientConn) (interface{}, error)
}
```

Grafana backend plugins use exactly this: `plugin.json` has `"backend": true` and
`"executable": "gpx_foo"`, Grafana starts the binary and talks gRPC (pluginv2) with AutoMTLS on by
default (Grafana plugin-tools reference). Cost: a .proto per interface, a client and a server
implementation per interface ("step 2 is the most tedious"), and local-only (the README says
plugins over a real network are unsupported).

**Take-away:** right model when third-party code must not share the process (crash isolation,
credentials isolation, language freedom). Too heavy as the *primary* model for a DNS server that
mostly wants a dozen first-party integrations; use the HTTP webhook contract (§1.6) for the
out-of-process tier instead, which gives the same language freedom with no proto/broker machinery.

### 1.5 Gravity "roles"

`pkg/roles/role.go`:

```go
type Role interface { Start(ctx context.Context, config []byte) error; Stop() }
type MigratableRole interface { Role; RegisterMigrations() }
type RoleConstructor func(Instance) Role
func Register(name string, constructor RoleConstructor)      // init()-time, in-tree only
type Instance interface { KV() *storage.Client; Log(); DispatchEvent(topic, *Event);
    AddEventListener(topic, EventHandler); Context(); ExecuteHook(HookOptions, ...interface{}) interface{}; Migrator() }
```

Which roles run on a node comes from `BOOTSTRAP_ROLES` then etcd (`/gravity/instance/<id>/roles`);
`Instance.bootstrap()` constructs each role, runs migrations, calls `Start(ctx, rawConfig)` where
`rawConfig` is the role's JSON blob watched from etcd (so role config changes restart the role via
the `instance.root.role_restart` event). `ErrRoleNotConfigured` lets an enabled-but-unconfigured
role idle. Event bus topics are strings (`instance.root.bootstrapped`, `role_started`, DNS/DHCP
events); `ExecuteHook` runs user-supplied JavaScript (goja) with a small env (`gravity.log`,
`net.parseIP`, `strconv.toBytes`) - used for DHCP scope hooks. Gravity's external-dns adapter is a
separate binary (`cmd/external-dns`, image `ghcr.io/beryju/gravity-external-dns`) configured by
env (`GRAVITY_URL`, `GRAVITY_TOKEN`, `DOMAIN_FILTER`, `LISTEN_API=localhost:8888`,
`LISTEN_METRICS=0.0.0.0:8080`); it maps each external-dns endpoint target to a Gravity record UID
`external-dns-<dnsName>-<target>` (`pkg/externaldns/server.go:194-198`) so deletes are exact, and
groups records back into endpoints on `GET /records`.

**Take-away:** "role = in-tree package registered by name, started with a config blob, restarted
on config change, with an event bus" is almost exactly what kudurru's *integration* runtime needs.
The JS-hook idea is nice for small per-record transforms but adds a runtime; keep it optional.

### 1.6 external-dns webhook provider (out-of-process HTTP contract)

The contract (`docs/tutorials/webhook-provider.md`, `api/webhook.yaml` v0.15.0, code in
`provider/webhook/`):

| Method | Route | Purpose | Success |
|---|---|---|---|
| GET | `/` | negotiate; returns `{"filters": [".example.com", ...]}` (DomainFilter) | 200 |
| GET | `/records` | current records as `[]Endpoint` | 200 |
| POST | `/adjustendpoints` | provider canonicalises candidate endpoints (adds/normalises `providerSpecific`) | 200 + `[]Endpoint` |
| POST | `/records` | apply `Changes{create, updateOld, updateNew, delete}` | **204** |
| GET | `/healthz`, `/metrics` | on a *separate* listener (0.0.0.0:8080) | |

Media type on `Accept`/`Content-Type`: `application/external.dns.webhook+json;version=1`.
Recommended listen: `localhost:8888`, as a sidecar in the external-dns pod. Only `5xx` is retried
(treated as a soft error); `4xx`/`3xx` are permanent failures; request/response bodies capped at
32 MiB (`--webhook-provider-max-body-size`); read/write timeouts are flags. Any in-tree provider
can be run as a webhook server with `--webhook-server --provider=aws`, which is how Gravity-style
adapters are tested. Custom annotations `external-dns.kubernetes.io/webhook-<x>` flow through as
`providerSpecific` entries.

The JSON shape of `Endpoint` (`endpoint/endpoint.go`, also the `DNSEndpoint` CRD spec):

```json
{"dnsName":"foo.example.com","targets":["1.2.3.4"],"recordType":"A","setIdentifier":"",
 "recordTTL":60,"labels":{"owner":"k8s-prod"},"providerSpecific":[{"name":"cloudflare/proxied","value":"true"}]}
```

`recordType` enum in the CRD: `A;AAAA;CNAME;DNAME;MX;NAPTR;NS;PTR;SRV;TLSA;TXT`. In-tree
`Provider` interface mirrors the routes exactly:

```go
type Provider interface {
    Records(ctx) ([]*endpoint.Endpoint, error)
    ApplyChanges(ctx, *plan.Changes) error
    AdjustEndpoints([]*endpoint.Endpoint) ([]*endpoint.Endpoint, error)
    GetDomainFilter() endpoint.DomainFilterInterface
}
```

**Take-away:** kudurru should *speak* this contract natively (be an external-dns webhook target
without a sidecar, like Gravity but in-process) **and** reuse its shape - `Endpoint`, `Changes`,
negotiate/records/adjust/apply - as the wire format for kudurru's own third-party integration
webhooks. Any team that has written an external-dns webhook provider can then write a kudurru
sink with near-zero learning.

### 1.7 libdns (Go interface set, MIT)

`github.com/libdns/libdns` v1.1.1 defines zone-scoped record CRUD used by Caddy, lego-adjacent
tooling and ~100 providers:

```go
type RecordGetter   interface{ GetRecords(ctx, zone string) ([]Record, error) }
type RecordAppender interface{ AppendRecords(ctx, zone string, recs []Record) ([]Record, error) } // never modifies existing
type RecordSetter   interface{ SetRecords(ctx, zone string, recs []Record) ([]Record, error) }    // RRset-replace semantics per (name,type)
type RecordDeleter  interface{ DeleteRecords(ctx, zone string, recs []Record) ([]Record, error) } // exact match; empty Type/TTL/Value = wildcard
type ZoneLister     interface{ ListZones(ctx) ([]Zone, error) }                                   // optional
```

Records are typed structs (`Address`, `CNAME`, `TXT`, `SRV`, `MX`, ... plus opaque `RR`) with a
`RR()` method; `zone` is an FQDN with trailing dot, names are relative. `SetRecords` is documented
with explicit RRset-replacement examples and an `AtomicErr` sentinel for providers with
transactional APIs. `libdns/cloudflare` and `libdns/unifi` exist (MIT).

**Take-away:** libdns is the right *inner* abstraction for the Cloudflare sink's provider layer
(and gives ~100 providers nearly free), but it has no ownership, no plan/diff, no domain filter,
and no "provider-specific property" channel (proxied flag has to be carried out of band - the
Cloudflare libdns provider exposes it via a `ProviderData`/comment convention, UNVERIFIED). Use
libdns *inside* a kudurru sink, with external-dns-style `Endpoint`+ownership on the outside.

### 1.8 Grafana / Home Assistant integration patterns (metadata + lifecycle)

- **Grafana `plugin.json`**: `id` (`^[0-9a-z]+-([0-9a-z]+-)?(app|panel|datasource)$`), `type`,
  `name`, `info{author,version,links,logos,screenshots}`, `dependencies{grafanaDependency,
  plugins}`, `backend`/`executable` (go-plugin subprocess), `routes` (server-side proxy routes that
  inject secrets so the browser never sees them), `includes` (bundled dashboards with RBAC
  gating), `iam` (RBAC permissions granted to the plugin's service account), capability flags
  (`metrics`, `logs`, `streaming`, `alerting`). Secrets are stored in `secureJsonData`
  (encrypted, write-only) separate from `jsonData` (UNVERIFIED here, well-known pattern).
- **Home Assistant `manifest.json`**: `domain` (== directory name, permanent id), `name`,
  `version` (custom only), `codeowners`, `config_flow: true` (UI setup wizard), `dependencies` /
  `after_dependencies` (load order), `requirements` (pip deps), `integration_type` (`hub`,
  `device`, `service`, `helper`, ...), `iot_class` (`local_push`, `local_polling`, `cloud_push`,
  `cloud_polling`, `assumed_state`, `calculated`), `loggers`, `quality_scale`, and **discovery
  keys** (`zeroconf`, `ssdp`, `dhcp` (hostname glob + MAC/OUI), `mqtt`, `bluetooth`, `usb`) that
  auto-start a config flow when a matching device appears. MQTT discovery topic:
  `<prefix=homeassistant>/<component>/[<node_id>/]<object_id>/config` (per-entity) or
  `homeassistant/device/<id>/config` with `dev`, `o`, `cmps` (device-based, newer); `unique_id`
  required.

**Take-away:** every integration should carry a small manifest (id, display name, version,
`iot_class`-like polling/push declaration, capabilities, config JSON schema, discovery hints) that
the UI renders generically, and credentials should live in a write-only "secure" bucket.

### 1.9 Summary matrix

| Criterion | Technitium Apps | CoreDNS | Caddy | go-plugin | external-dns webhook | Gravity roles | libdns |
|---|---|---|---|---|---|---|---|
| Isolation from host | none (in-proc, full trust) | none | none | process | process/pod | none | none |
| Language freedom | .NET only | Go | Go | any (gRPC) | any (HTTP) | Go | Go |
| Runtime install | yes (zip) | no (rebuild) | no (xcaddy) | yes (binary) | yes (deploy container) | no | no |
| Config schema story | JSON text, template string | Corefile directive | JSON + strict decode | none | flags/env | JSON blob in etcd | struct fields |
| DNS-pipeline hooks | 8 typed stages | ordered chain | n/a | n/a | n/a (records only) | n/a | n/a |
| Record sync semantics | n/a | n/a | n/a | n/a | plan + ownership | UID per target | RRset set/append/delete |
| Fit for kudurru | hook taxonomy | stage ordering | config loading | optional 3rd tier | wire contract | runtime model | provider layer |

---

## 2. Target integration (1): Cloudflare

### 2.1 API v4 facts (verified 2026-10-10)

- Base: `https://api.cloudflare.com/client/v4`. Auth: `Authorization: Bearer <token>` (API
  tokens preferred; legacy `X-Auth-Email`/`X-Auth-Key` discouraged). New tokens carry the `cfut_`
  prefix for secret scanners. Verify with `GET /user/tokens/verify` -> `{"success":true,
  "result":{"id":"...","status":"active"}}`.
- Token scoping (`fundamentals/api/get-started/create-token`): template **"Edit zone DNS"**;
  permissions are `Zone / DNS / Edit` (full CRUDL on records; Edit implies Read) plus `Zone / Zone /
  Read` to resolve zone IDs; **Zone Resources** restrict to specific zones ("Any other zone will
  return an error"); optional **client IP filtering** and **TTL** (expiry). Account-owned tokens
  exist for service use. Limits: 50 user tokens per user, 500 account tokens per account.
- Rate limits (`fundamentals/api/reference/limits`): **1,200 requests per 5 minutes per user**,
  cumulative across dashboard + keys + tokens; exceeding blocks *all* calls for 5 min with HTTP
  429; **200 req/s per IP**; GraphQL 320/5 min. Response headers: `Ratelimit:
  "default";r=50;t=30` (remaining, seconds to reset), `Ratelimit-Policy: "burst";q=100;w=60`,
  `retry-after` on 429. Enterprise can request higher limits.
- Records (`api/resources/dns/subresources/records`):
  - `GET /zones/{zone_id}/dns_records` filters: `name.{exact,contains,startswith,endswith}`,
    `type`, `content.*`, `tag.{exact,contains,startswith,endswith,present,absent}` (value form
    `name:value`), `tag_match=any|all`, `comment.*`, `search`, `match=any|all`, `proxied`,
    `order`, `direction`, `page`, **`per_page` 1..5,000,000**, `include_shadow_metadata`. Needs
    DNS Read or Write.
  - `POST /zones/{zone_id}/dns_records` (DNS Write): body `name` (full FQDN, punycode, 1-255),
    `type`, `ttl` (**1 = automatic**, else 60..86400; 30 min on Enterprise), `content`, `proxied`,
    `comment`, `tags[]`, `settings{ipv4_only,ipv6_only}`, `private_routing`. `PUT` = overwrite,
    `PATCH` = partial, `DELETE /dns_records/{id}`. Export/Import BIND via `/dns_records/export|import`.
  - Record object: `id, name, type, content, proxied, proxiable, ttl, comment, tags[],
    settings{}, created_on, modified_on, comment_modified_on, tags_modified_on, meta{is_glue,
    shadowed_by[], ...}`.
  - **Comments**: 100 chars (Free) / 500 (paid), one per record, no CR/LF, all zone types.
    **Tags**: `name:value`, name <=32 chars `[A-Za-z0-9_-]` case-insensitive, value <=100 chars,
    <=20 tags/record, **not available on Free plan**; tags starting `cf-` are reserved.
  - **Batch** `POST /zones/{zone_id}/dns_records/batch` body `{deletes:[{id}], patches:[{id,...}],
    puts:[{id,name,type,content,...}], posts:[{...}]}`; executes **deletes -> patches -> puts ->
    posts**, in-list order; one DB transaction, rollback on first error (first error returned);
    **edge propagation is not atomic**; limit **200 ops (Free) / 3,500 (Pro/Business/Enterprise)**
    per request; available on all plans; supports tags/comments.
- Response envelope (all endpoints): `{"success":bool,"errors":[{"code":int,"message":str}],
  "messages":[],"result":...,"result_info":{page,per_page,count,total_count}}` (well-known;
  envelope body of the create page was truncated in fetch - UNVERIFIED from page text, verified by
  SDK types `RecordBatchResponseEnvelope`).

### 2.2 Official Go SDK: cloudflare-go **v7**

- Module `github.com/cloudflare/cloudflare-go/v7`, latest **v7.12.0** (2026-10-01); Go proxy also
  lists v6.10.0, v5.1.0, v4.6.0, and legacy v0.119.0 (the hand-written `cloudflare-go` v0.x line).
  Apache-2.0; `go 1.22`. Generated by Stainless from Cloudflare's OpenAPI; **every minor release
  may contain breaking changes** in some package (v7.5.0, v7.7.0, v7.8.0, v7.9.0, v7.12.0 all did
  - none in `dns` so far). external-dns pins `v7.7.0` with a comment linking an SDK issue (#4300).
- Client: `cloudflare.NewClient(option.WithAPIToken(tok))`; default token from
  `CLOUDFLARE_API_TOKEN`. Errors: `*cloudflare.Error{StatusCode, Request, Response, ...}` via
  `errors.As`; transport errors unwrapped. Retries: **2 by default** with short exponential backoff
  on connection errors, 408/409/429/5xx (`option.WithMaxRetries`); no request timeout by default
  (`option.WithRequestTimeout`). Pagination: `List()` + `GetNextPage()` or `ListAutoPaging()`.
- DNS surface (`dns/record.go`): `Records.New/Update(PUT)/Edit(PATCH)/Delete/Get/List/
  ListAutoPaging/Batch/Export/Import/Scan*`. Batch params are typed unions per record type
  (`BatchPatchARecordParam`, `BatchPutUnionParam`, `RecordBatchParamsPostUnion`); not every type
  is in every union (external-dns falls back to per-record calls for e.g. CAA on PUT).

### 2.3 How external-dns drives Cloudflare today (reference behaviour)

`provider/cloudflare/*.go` (HEAD): zones listed then filtered by `--zone-id-filter`; records
fetched with `--cloudflare-dns-records-per-page` (max 5,000) ; changes grouped per zone into
`batchCollections{deletes, puts, posts}` and sent in chunks of `--batch-change-size` (default
**200**) with `--batch-change-interval` (default **1s**) between chunks; a failed chunk is
replayed record-by-record; SRV/TLSA use structured `data` fields; proxied flag from
`providerSpecific cloudflare/proxied` (annotation `external-dns.kubernetes.io/cloudflare-proxied`)
or `--cloudflare-proxied` default, forced false for TLSA; comments via
`--cloudflare-record-comment` / annotation `cloudflare-record-comment`; tags via annotation
`cloudflare-tags: "owner:frontend-team, env:dev"`; region key, custom hostnames (Cloudflare for
SaaS) also supported. `convertCloudflareError` turns 429/5xx/EOF/"rate limit" strings into
`provider.SoftError` so the controller retries next loop instead of failing. Token may be read from
a file with `CF_API_TOKEN=file:/path` (whitespace trimmed).

### 2.4 Ownership tracking: the external-dns TXT registry design (document for kudurru)

Purpose: a provider zone is shared by humans, other controllers and multiple clusters; the
controller must only touch what it created. external-dns persists per-record metadata in the
*same* provider as TXT records (`registry/txt/registry.go`, `endpoint/labels.go`,
`registry/mapper/mapper.go`, `docs/registry/txt.md`):

- **Value format** (plain): `"heritage=external-dns,external-dns/owner=<ownerID>,external-dns/resource=<kind/ns/name>"`
  - keys sorted; `heritage` is mandatory and must equal `external-dns` or the TXT is ignored
    (`ErrInvalidHeritage`) and treated as a foreign record; everything prefixed
    `external-dns/` becomes a label. Optional AES-256-GCM encryption of the whole value
    (`--txt-encrypt-enabled`, 32-byte key; nonce stored as a transient label so re-serialisation
    is stable).
- **Name mapping** (v0.18+ single "new format"): TXT name = `<prefix><name>` with `%{record_type}`
  substituted, e.g. `--txt-prefix="%{record_type}-"` gives `a-www.example.com` for the A record and
  `cname-www.example.com` for a CNAME; suffix form also exists (mutually exclusive); apex records
  need a prefix ending in `.` (`someprefix-%{record_type}.` -> `cname-abc-.ex.com`); wildcard
  `*` is replaced with `--txt-wildcard-replacement`; prefix/suffix may never change after deploy
  (orphans the metadata). Supported types for the prefix: A, AAAA, CNAME, NS, MX, PTR, SRV, NAPTR,
  TXT, DNAME, TLSA. Prefix/suffix count against the 63-byte label limit.
- **Read path** (`Records()`): fetch all provider records; split TXT registry records from
  data records; parse labels; key by `(dnsName, recordType, setIdentifier)`; attach labels to the
  matching data record; fall back to the type-less legacy key; migrate `--migrate-from-txt-owner`
  old owner to new; keep an optional in-memory cache (`--txt-cache-interval`) to save provider
  reads.
- **Write path** (`ApplyChanges()`): filter Update/Delete to records whose `owner` label equals
  this instance's `--txt-owner-id`; stamp `owner` on creates; emit the paired TXT create/delete
  alongside each data change (skip TXT creates that already exist; on delete, send back the exact
  stored TXT value so providers that match by value accept it).
- **Plan** (`plan/plan.go`): a `planTable` keyed by `(dnsName,setIdentifier)` holds `current`
  (from registry) and `candidates` (from sources), grouped by record type; outcomes: no current ->
  create; no candidates -> delete; both -> per type: update when targets/TTL/providerSpecific
  changed, carrying the owner label over (`inheritOwner`); creates on a name that has *any* record
  owned by someone else are **skipped and counted** (`registryOwnerMismatchPerSync` gauge); a
  CNAME candidate conflicts with other types (RFC 1034 3.6.2) and the `PerResource` resolver picks
  deterministically (lowest by compare, preferring the resource that already owns the name);
  policies then prune: `sync` (all), `upsert-only` (no deletes), `create-only`.
- Documented failure modes worth copying into kudurru's docs: shared `owner=default` across
  clusters + `sync` policy deletes other clusters' records; lost TXT records = lost ownership ->
  records silently drift unmanaged; key rotation requires a stop-the-world rewrite.
- Alternatives external-dns offers: `dynamodb` registry (metadata in a table, not in DNS), `crd`
  registry (`DNSRecord` CRs in-cluster), `noop`, `aws-sd`.

### 2.5 Split-horizon design for kudurru

Setting: kudurru is **authoritative inside** for `example.com` (private RFC 1918 answers); the
**public** `example.com` zone lives at Cloudflare and must receive a *subset* of records
(public-facing hosts, ACME challenges, MX/SPF), some proxied.

Recommended model:

1. **One zone, one source of truth** in kudurru; outward sync is a *policy* on records, not a
   second zone. Each record gets labels (free-form `key=value`, like external-dns `labels` and
   Cloudflare tags); an **export policy** on the `CloudflareSink` selects by label selector + name
   glob + type list, e.g. `selector: {publish: cloudflare}`, `types: [A, AAAA, CNAME, TXT, MX,
   SRV, CAA]`, `exclude: ["*.internal.example.com"]`.
2. **Public value override**: a record can carry per-sink overrides - `sinks.cloudflare.targets:
   [203.0.113.10]`, `sinks.cloudflare.proxied: true`, `sinks.cloudflare.ttl: 1` - so the internal
   answer (10.0.0.10) and the published one (public IP or a CNAME to a tunnel hostname) differ.
   This is the UniFi-style "one object, two views" and avoids maintaining two zones by hand.
   Technitium's Split Horizon app uses 1:1 NAT rewriting (`externalToInternalTranslation`) as a
   convenience; offer that as a sink-level `translate: {"203.0.113.0/24": "10.0.0.0/24"}` option.
3. **Proxied flag semantics** (Cloudflare): only A/AAAA/CNAME are proxiable; proxied records
   must use `ttl: 1` (auto); `proxied` changes the public answer to Cloudflare edge IPs - never
   mirror proxied public answers back inward. Default `proxied: false`; allow per-record
   override and a sink default, exactly like external-dns.
4. **Ownership**: kudurru keeps an **internal ledger** (its own store) of what it wrote to each
   sink: `(sink, zone, name, type) -> {providerRecordIDs[], contentHash, lastApplied, generation}`.
   Additionally, for safety against ledger loss and for coexistence with other tools, mark
   Cloudflare records with **comment** `kudurru:owner=<clusterID>` (works on Free; 100 chars) and,
   where the plan allows, **tag** `kudurru-owner:<clusterID>` (paid plans only). On startup or
   ledger loss, re-adopt records whose comment/tag matches. Do **not** depend on TXT ownership
   records at Cloudflare (they are visible publicly and double record count), but *accept* the
   external-dns TXT format on read so a migration from external-dns -> kudurru can adopt
   `heritage=external-dns,external-dns/owner=<id>` records when configured with
   `adoptFrom: {externalDNSOwner: "<id>"}`.
5. **Reconcile loop** (idempotent, level-triggered):
   - trigger: zone change event (debounced 2-5 s) OR periodic full resync (default 10 min) OR
     manual "sync now";
   - desired = policy(select(zone records)) with overrides applied -> canonical `Endpoint` set;
   - current = Cloudflare list (one `GET /dns_records?per_page=5000` per zone, cached between
     runs; prefer `comment.startswith=kudurru:` filter to shrink reads) merged with ledger;
   - diff with external-dns-style table (per name+type, CNAME exclusivity, owner mismatch ->
     skip + condition);
   - apply via **batch** chunks of 200 (safe on Free) with deletes first, 1 s between chunks; on
     chunk failure retry per record; map 429/5xx to "requeue with backoff" honoring `retry-after`;
     budget: stay under ~1,000 calls/5 min across *all* Cloudflare sinks sharing an account
     (token bucket in the sink);
   - write status: `conditions[Ready, Synced]`, `observedGeneration`, `lastSyncTime`,
     `recordsManaged`, `drift` (records that differ and are not owned), `rateLimitRemaining`.
   - policies: `sync` (default), `upsert-only`, `create-only`, plus `deletionPolicy:
     Retain|Delete` for when the sink itself is removed.
6. **Conflicts**: a name at Cloudflare owned by nobody (no comment/tag/TXT) is **never modified**
   unless `adoptUnowned: true`; a name owned by another kudurru/external-dns owner is reported as
   a conflict condition and skipped (external-dns behaviour).
7. **Secrets**: token comes from a secret reference (k8s `Secret` key or Docker file/env);
   validate at config time with `GET /user/tokens/verify` + `GET /zones?name=example.com` and
   surface the result in status (`Authenticated`, `ZoneResolved` conditions). Recommend users
   create the token with *Zone Resources: Include -> Specific zone* and optionally client IP
   filtering; store nothing but the reference in the spec.

Minimal Docker YAML sketch:

```yaml
integrations:
  - name: cf-public
    type: cloudflare
    credentials: { apiTokenFile: /run/secrets/cf_token }   # or apiTokenEnv: CF_API_TOKEN
    zones:
      - zone: example.com
        zoneId: 023e105f4ecef8ad9ca31a8372d0c353   # optional; resolved via Zone:Read otherwise
        select: { labels: { publish: cloudflare }, types: [A, AAAA, CNAME, TXT, MX, SRV] }
        defaults: { proxied: false, ttl: 1, comment: "kudurru:owner=home" }
        policy: sync            # sync | upsert-only | create-only
        adoptFrom: { externalDNSOwner: "k8s-home" }
    resync: 10m
```

---

## 3. Target integration (2): UniFi Network

### 3.1 The two APIs

| | Official **UniFi Network API** ("Integration API") | Legacy controller API (undocumented) |
|---|---|---|
| Docs | `developer.ui.com/network/<version>/` (OpenAPI 3.1.0 `openapi.json`, Postman, `llms.txt`, Ansible); versions published from **v9.1.120** through **v10.6.106** (list on `developer.ui.com/llms.txt`) | reverse-engineered (unpoller, go-unifi, aiounifi, artofwifi) |
| Base URL (from spec `servers`) | local: `https://{consoleIP}/proxy/network/integration` -> paths `/v1/...`; cloud: `https://api.ui.com/v1/connector/consoles/{consoleId}/proxy/network/integration` | UniFi OS: `https://console/proxy/network/api/s/<site>/...` and `/proxy/network/v2/api/site/<site>/...`; classic controller: `https://host:8443/api/s/<site>/...` |
| Auth | `X-API-Key: <key>` (stateless; key created in Network app *Settings -> Control Plane -> Integrations* (path varies by version; UNVERIFIED exact menu), shown once, inherits creator's admin rights). Cloud path uses a Site Manager key. | `POST /api/auth/login` (UniFi OS) or `/api/login` with `{username,password}`; cookie + `X-CSRF-Token` (from `x-csrf-token` response header) on writes. unpoller v6 also sends `X-API-Key` on legacy paths and reports it works on UniFi OS consoles. |
| Pagination | `?offset=&limit=` (unpoller uses 200) and `?filter=`; envelope `{offset,limit,count,totalCount,data[]}` | `{meta:{rc:"ok"},data:[...]}` |
| Errors | `{code:"api.authentication.missing-credentials", message, requestId, requestPath, statusCode, statusName, timestamp}`; 429 with `Retry-After` observed by unpoller (`RateLimitError`) | HTTP codes + `meta.msg` |
| Stability | versioned, documented | changes between releases (e.g. `/stat/sta` fields), 227+ endpoints |

### 3.2 Official API surface relevant to kudurru (parsed from `openapi.json` v10.6.106, 44 paths)

```
GET  /v1/info                                   -> {applicationVersion}
GET  /v1/sites                                  -> page of {id (uuid), internalReference ("default" - legacy site name), name}
GET  /v1/sites/{siteId}/clients[?offset&limit&filter]   -> page of Client overview
GET  /v1/sites/{siteId}/clients/{clientId}      -> Client details
POST /v1/sites/{siteId}/clients/{clientId}/actions   body {action: AUTHORIZE_GUEST_ACCESS|UNAUTHORIZE_GUEST_ACCESS}
GET/POST /v1/sites/{siteId}/networks ; GET/PUT/DELETE /networks/{id} ; GET /networks/{id}/references
GET/POST /v1/sites/{siteId}/dns/policies ; GET/PUT/DELETE /dns/policies/{dnsPolicyId}
GET/POST /v1/sites/{siteId}/devices ; GET/DELETE /devices/{id} ; POST /devices/{id}/actions ; GET /devices/{id}/statistics/latest
GET /v1/sites/{siteId}/device-tags ; /wans ; /vpn/servers ; /vpn/site-to-site-tunnels ; /radius/profiles
GET/POST firewall/policies, firewall/zones, acl-rules, traffic-matching-lists, wifi/broadcasts, hotspot/vouchers, switching/*
```

Client object (`Client overview` / `Client details`, discriminator `type`):

| field | type | notes |
|---|---|---|
| `id` | uuid | stable client id |
| `name` | string | **user alias if set, else hostname/OUI name (UNVERIFIED which fallback)** |
| `type` | `WIRED` \| `WIRELESS` \| `VPN` \| `TELEPORT` | |
| `access` | `{type: DEFAULT \| GUEST}` | guest/authorised state |
| `connectedAt` | date-time | |
| `ipAddress` | string | current IP |
| `macAddress` | string | **only on WIRED/WIRELESS** subtypes |
| `uplinkDeviceId` | uuid | AP/switch the client hangs off |

**Not in the official client object**: hostname (DHCP option 12), fixed-IP reservation,
`local_dns_record`, network/VLAN id, note, user group. Those still require the legacy API.

DNS policy object (`DNS policy`, discriminator `type`): common `id, enabled, domain (<=127 chars),
metadata`; types and extra fields:

| `type` | fields |
|---|---|
| `A_RECORD` | `ipv4Address`, `ttlSeconds` 0..86400 (0 = default) |
| `AAAA_RECORD` | `ipv6Address`, `ttlSeconds` |
| `CNAME_RECORD` | `targetDomain` (<=127), `ttlSeconds` 0..604800 |
| `MX_RECORD` | `mailServerDomain`, `priority` 0..65535 |
| `SRV_RECORD` | `service` (`_ldap`), `protocol` (`_tcp`), `serverDomain`, `port`, `priority`, `weight` |
| `TXT_RECORD` | `text` |
| `FORWARD_DOMAIN` | `ipAddress` of upstream resolver (conditional forwarding) |

(`libdns/unifi` implements libdns on top of exactly these; `unpoller` models them as
`DNSPolicy{domain,enabled,id,type}`.)

Network object (`Create or update Network`): `name`, `vlanId` 1..4009, `enabled`, `management`
(`GATEWAY|SWITCH|UNMANAGED`), `dhcpGuarding`, gateway IPv4 `{hostIpAddress, prefixLength 8..30,
dhcpConfiguration{ipAddressRange, leaseTimeSeconds, domainName, dnsServerIpAddressesOverride,
gatewayIpAddressOverride, ntpServerIpAddresses, ...}}`, IPv6 `{slaacEnabled, dhcpConfiguration}`.
WiFi broadcasts carry `DNS assistance configuration {mode: AUTO|MANUAL, servers[]}` and an mDNS
proxy policy.

### 3.3 Legacy API surface (what kudurru still needs)

From `unpoller/unifi/types.go` + `paultyng/go-unifi`:

| Path (prefix `/proxy/network` on UniFi OS) | Data | Fields of interest |
|---|---|---|
| `GET /api/s/<site>/stat/sta` | **active clients** | `mac, ip, hostname, name (alias), fixed_ip, use_fixedip, network, network_id, vlan, usergroup_id, oui, is_wired, is_guest, blocked, note, first_seen, last_seen, ap_mac, sw_mac, sw_port, essid, dev_vendor/dev_family/dev_cat (fingerprint)` |
| `GET /api/s/<site>/stat/alluser?within=<hours>` (POST `{type:"all:",conn:"all",within}`) | known/historical clients | same subset + `duration` |
| `GET /api/s/<site>/rest/user[/<id>]`, `POST`, `PUT` | **client "user" records** (what the UI calls client settings) | `name, hostname, mac, fixed_ip, use_fixedip, network_id, local_dns_record, local_dns_record_enabled, usergroup_id, blocked, note, virtual_network_override_*` (go-unifi `User`) |
| `POST /api/s/<site>/cmd/stamgr` `{cmd: block-sta|unblock-sta|kick-sta|forget-sta, mac}` | client actions | |
| `GET /api/s/<site>/rest/networkconf` | networks | `name, vlan, vlan_enabled, ip_subnet, domain_name, purpose, dhcpd_enabled, dhcpd_start/stop, dhcpd_leasetime, dhcpd_dns_enabled, dhcpd_dns_1..4, dhcpd_ip_1.., mdns_enabled, networkgroup` |
| `GET /proxy/network/v2/api/site/<site>/active-leases` | **DHCP leases** | `ip, mac, hostname, display_name, use_fixedip, lease_expiration_time, network_id, status, client_type, device_type` (unpoller `DHCPLease`) |
| `GET/POST/PUT/DELETE /proxy/network/v2/api/site/<site>/static-dns` | **static DNS records** (UI "Local DNS") | per Pulumi `unifi.dns.Record` (fork of terraform-provider-unifi): `type (A,AAAA,CNAME,MX,NS,PTR,SOA,SRV,TXT)`, `value`, `name`/`key`, `ttl` (0 = auto), `enabled`, `port`, `priority`, `weight`; community script `unifi-bulk-dns` confirms POST + `X-API-Key` works here |
| `GET /api/s/<site>/stat/device` | APs/switches/gateways (`mac, ip, name, model`) | PTR/A for infra |
| `GET /api/stat/sites` | sites (`name` = internal ref, `desc`) | |

The `local_dns_record` field on `/rest/user` is the per-client "Local DNS record" the UniFi UI
shows (hostname override that the gateway's dnsmasq serves) - that is the closest thing to a
UniFi-side "DNS name for this client".

### 3.4 Go libraries

| Library | License | Scope | Verdict |
|---|---|---|---|
| `github.com/unpoller/unifi/v6` v6.3.1 | MIT | read-only; `Config{URL, User, Pass, APIKey, ...}`; `Login()` handles `/api/auth/login` vs `/api/login`, CSRF; `GetSites/GetClients/GetUsers/GetNetworks/GetDevices/GetActiveDHCPLeases(WithAssociations)`; Integration/v1 `GetIntegrationSites/Networks/DeviceStats/WANs/GetDNSPolicies`, generic `getIntegrationList[T]` pager with drift detection; cloud connector client with `RateLimitError{RetryAfter}`; gzip quirk handling; `--discover` endpoint prober | **use for inbound inventory** (MIT, maintained, handles both APIs); contribute writers if needed |
| `github.com/paultyng/go-unifi` | MPL-2.0 | archived; typed models incl. `User.LocalDNSRecord`, `Network`, CRUD on `/rest/*`, `stamgr` | do not depend; read for field names |
| `github.com/filipowm/go-unifi` v1.11.2 | MPL-2.0 (fork) | maintained fork (also `ubiquiti-community/go-unifi`, `akerl/go-unifi`) with write support | option for **outbound writes** (fixed IPs, static DNS); MPL-2.0 is file-level copyleft - fine to import as a dependency from Apache-2.0 code, but do not vendor-modify without releasing changes |
| `github.com/libdns/unifi` | MIT | libdns over official DNS-policy API (`ApiKey, SiteId, BaseUrl=https://gw/proxy/network/integration/v1`) | reference for a "UniFi as sink" option |
| `Kane610/aiounifi` (Python) | MIT | added Network API v1 + API-key auth (PR #1096) | n/a (Python) |

### 3.5 What kudurru pulls from UniFi and what it does with it

Inbound (`ClientSource`), polled (default 60 s; official API has no push/websocket):

1. **Sites** -> map `internalReference` (legacy name) <-> `id` (uuid) once.
2. **Networks** (legacy `rest/networkconf` for `ip_subnet`, `domain_name`, `vlan`) -> kudurru
   *network* objects: name, VLAN, CIDR, DHCP domain. Used for (a) PTR zone auto-creation
   (`0.168.192.in-addr.arpa`), (b) client-group membership by subnet, (c) per-network default
   search domain.
3. **Clients** (official `/clients` for live state; legacy `stat/sta` + `rest/user` + `active-
   leases` for alias/hostname/fixed-IP/VLAN) -> kudurru *client inventory* entries
   `{mac, ips[], name (alias), hostname, network, vlan, fixedIP, groupHints[], lastSeen, source:
   unifi/<site>}`.
4. **Name synthesis** -> A/AAAA + PTR in a configurable *managed* zone: `nameTemplate:
   "{{ .Name | slug }}.{{ .Network.Domain | default "lan" }}"`, preferring alias > local_dns_record
   > DHCP hostname > OUI-vendor+last-4-of-MAC; collisions resolved deterministically (append
   `-2`), stable per MAC. Records carry `labels: {source: unifi, mac: ...}` and are owned by the
   integration (never hand-edited; show as read-only "synthesized" in the UI). TTL short (60-300
   s) since leases move.
5. **Client groups / policies** -> `groupRules`: by UniFi network/VLAN name, by `usergroup_id`,
   by a UniFi *client note/tag* convention (e.g. note contains `kudurru:group=kids`), by
   `is_guest`, by device fingerprint category. Groups feed the blocking engine (blocky-style
   `clientGroupsBlock`) and the dashboard's "client name" column - this is the Pi-hole
   "known clients" experience without manual entry.
6. **Optional outbound** (`RecordSink`/"Push"): (a) write `dhcpd_dns_1..4`/
   `dnsServerIpAddressesOverride` on selected networks so DHCP hands out kudurru (requires write
   scope; off by default; status shows drift if someone changes it back); (b) mirror selected
   kudurru records into UniFi **DNS policies** (`A_RECORD`/`CNAME_RECORD`) or legacy
   `static-dns` so the gateway resolver answers if kudurru is down; (c) block/unblock client via
   `stamgr` as an *action* hook (e.g. from a dashboard button) - gated by RBAC.

Config sketch:

```yaml
integrations:
  - name: unifi-home
    type: unifi
    endpoint: https://192.168.1.1
    tlsInsecureSkipVerify: false          # pin fingerprint instead when self-signed
    credentials:
      apiKeyFile: /run/secrets/unifi_key   # official Integration API
      # legacy: usernameFile/passwordFile (local-only admin, read-only role)
    site: default
    poll: 60s
    inventory:
      zone: lan.example.com               # managed zone for synthesized names
      reverseZones: auto                  # create in-addr.arpa/ip6.arpa from networks
      nameTemplate: "{{ .Alias | default .Hostname | slug }}"
      ttl: 120
      includeNetworks: ["LAN", "IoT"]
    groups:
      byNetwork: { IoT: iot, Guest: guest }
      byNote: "kudurru:group="
    push:
      dhcpDNSServers: false
      mirrorRecords: { enabled: false, types: [A, CNAME] }
```

---

## 4. Does the model generalize? Survey of likely future integrations

| Integration | Direction | Transport / auth | What kudurru would exchange | Fit |
|---|---|---|---|---|
| **Home Assistant** | both | REST `Authorization: Bearer <long-lived token>`; webhook triggers `POST /api/webhook/<id>` (no auth); MQTT discovery `homeassistant/<component>/<object_id>/config` (device-based `homeassistant/device/<id>/config` with `dev/o/cmps`), `unique_id` required | publish kudurru as a *device* with entities (queries/s, blocking on/off switch, per-group block switch, "last blocked" sensor); HA `dhcp` discovery lists `hostname` glob + OUI; HA device registry can be an **inbound ClientSource** (names for MACs) | `Hook` (events -> MQTT/webhook) + `ClientSource` (HA device registry, read via `/api/states` or MQTT) |
| **MQTT** generic | out | broker URL, user/pass or mTLS | events (`kudurru/events/query_blocked`), stats, availability topic | `Hook` sink |
| **Tailscale** | in (+out) | `https://api.tailscale.com/api/v2/tailnet/{tailnet}/devices` with API access token (`tskey-api-...`, Bearer) or OAuth client; DNS config endpoints under `/dns/{nameservers,preferences,searchpaths}`; split DNS via `dns/split...`/policy file - **UNVERIFIED this session (docs not fetchable; from memory + community notes)** | pull devices `{nodeId, name (MagicDNS), hostname, addresses[100.x, fd7a:...], tags, os}` -> A/AAAA in `ts.example.com` + PTR for `100.64.0.0/10`; optionally push kudurru as a split-DNS nameserver for internal domains | `ClientSource` + optional `Push` |
| **Proxmox VE** | in | `https://host:8006/api2/json`, header `Authorization: PVEAPIToken=USER@REALM!TOKENID=UUID` (no CSRF needed for tokens); verified paths from the official `apidoc.js`: `GET /cluster/resources` (`?type=vm`), `GET /nodes/{node}/qemu/{vmid}/agent/network-get-interfaces` (QEMU guest agent), `GET /nodes/{node}/lxc/{vmid}/interfaces`, `GET /nodes/{node}/lxc/{vmid}/config` | VM/CT names + IPs -> A/AAAA/PTR in `pve.example.com`; tags -> groups | `ClientSource` |
| **Pi-hole-compatible API** | serve | Pi-hole v6 FTL API: `POST /api/auth {password}` -> `{session:{sid,csrf,validity}}`, `sid` header/cookie, `/api/stats/summary`, `/api/history`, `/api/queries`, `/api/dns/blocking`, `/api/domains/{allow\|deny}/{exact\|regex}`, `/api/config/dns/hosts` (see `pihole.md` §4-6) | let existing dashboards/apps (PADD, HA pi_hole integration, mobile apps) talk to kudurru | a *compat facade* module, not an integration - implement as an optional HTTP API surface mapped onto kudurru's native API |
| **Prometheus** | serve | `/metrics` text exposition; ServiceMonitor CRD in k8s | metrics | built-in, not an integration |
| **Grafana** | serve/out | data source via Prometheus; dashboards JSON shipped as `includes` | | built-in dashboards JSON |
| **Webhooks (generic)** | out | HTTPS POST with HMAC signature header, retries with backoff, event filter | events: `record.changed`, `zone.transferred`, `client.seen`, `query.blocked` (sampled), `integration.error`, `cluster.leader_changed` | `Hook` sink |
| **external-dns** (k8s) | in | kudurru serves the webhook contract (§1.6) on `localhost:8888`-style listener or cluster service; external-dns runs with `--provider=webhook --webhook-provider-url=http://kudurru:8888` | Ingress/Service hostnames -> kudurru records, with external-dns TXT ownership stored *in kudurru* as TXT records (works unchanged) or mapped to kudurru labels | `RecordSource` (inbound records) |
| **Cloudflare / Route53 / etc. via libdns** | out | provider-specific | public zone sync | `RecordSink` (Cloudflare first-party; others via libdns adapters) |
| **UniFi** | in (+out) | §3 | inventory, groups, optional DNS/DHCP push | `ClientSource` (+`RecordSink`) |

Pattern that emerges - four verbs cover everything:

1. **RecordSink** (push kudurru records outward: Cloudflare, UniFi DNS policies, libdns providers).
2. **RecordSource** (pull/accept records inward: external-dns webhook, RFC 2136 clients, future
   Route53-import) - *mostly already covered by the core API*, so keep the interface small.
3. **ClientSource** (inventory: UniFi, Tailscale, Proxmox, HA device registry, DHCP leases from
   kudurru's own DHCP role) -> names/MACs/IPs/groups -> synthesized records + client groups.
4. **Hook** (event sink: webhook, MQTT, HA, Slack; and *actions* exposed back to the UI such as
   "block this client at the gateway").

Everything else (Pi-hole facade, Prometheus, Grafana) is an API/metrics surface, not an
integration, and should not be forced through the plugin model.

---

## 5. Recommended integration architecture for kudurru

### 5.1 Tiers

| Tier | Who writes it | Runs where | Contract | Trust |
|---|---|---|---|---|
| **A. In-tree Go integrations** (cloudflare, unifi, tailscale, proxmox, homeassistant/mqtt, webhook, externaldns-source) | kudurru repo | in the kudurru process, on **one elected node** per integration instance | small Go interfaces (§5.2), registered at `init()` like CoreDNS/Caddy/Gravity | full trust; reviewed in-repo |
| **B. Out-of-process HTTP integrations** | third parties / users | sidecar container, separate deployment, or any host | **kudurru webhook contract** (§5.5) = external-dns webhook routes + two extra routes for inventory/events; JSON; `Content-Type: application/kudurru.integration+json;version=1` | process isolated; credentials stay with the sidecar |
| **C. (Later, optional) go-plugin binaries** | third parties needing in-pipeline hooks (request controller / post-processor) | subprocess, gRPC, AutoMTLS | `.proto` versions of the DNS-pipeline hooks | process isolated; only if a real demand for *query-path* plugins appears - not in v1 |

Rationale: Tier A gives the UniFi/Cloudflare features users asked for with the least ceremony;
Tier B gives the "simple integration model so integrations can be added later" requirement
without rebuilding kudurru (and is already a known contract in the k8s DNS world); Tier C is the
only way to let third parties into the *query path* safely, but CoreDNS/Caddy both prove that
compile-time Go is fine for that, so defer it.

### 5.2 Go interface set (Tier A)

```go
package integration

// Manifest is static metadata, rendered generically by the UI/CLI (`kudurru integrations list`).
type Manifest struct {
    Type        string            // "cloudflare", "unifi" ... (id, lower-kebab)
    DisplayName string
    Version     string            // integration schema version, SemVer
    Capabilities []Capability     // RecordSink | ClientSource | Hook | Push | Actions
    IOClass     string            // "cloud_polling" | "local_polling" | "local_push" (HA iot_class)
    ConfigSchema json.RawMessage  // JSON Schema for Spec; also emitted into the CRD's openAPIV3Schema
    SecretKeys  []SecretKey       // which spec fields are secret refs, with "verify" hints
    Docs        string
}

type Integration interface {
    Manifest() Manifest
    // Caddy-style lifecycle: New() -> strict-unmarshal spec -> Provision -> Validate -> Start/Stop.
    Provision(ctx context.Context, host Host, spec json.RawMessage, secrets SecretResolver) error
    Validate(ctx context.Context) error          // cheap, no network
    Check(ctx context.Context) (Status, error)   // network: auth + reachability (token verify, /v1/info)
    Start(ctx context.Context) error             // runs until ctx is cancelled (leader-scoped)
    Stop(ctx context.Context) error
}

// Outbound record sync. Endpoint and Changes are *wire-compatible* with external-dns.
type RecordSink interface {
    DomainFilter() DomainFilter                                 // zones/suffixes this sink serves
    Records(ctx context.Context, zone string) ([]Endpoint, error)
    AdjustEndpoints(ctx context.Context, eps []Endpoint) ([]Endpoint, error) // canonicalise, fill provider-specific defaults (proxied, ttl=1)
    ApplyChanges(ctx context.Context, zone string, ch Changes) (ApplyResult, error) // batch-aware; ApplyResult carries provider IDs for the ledger
}

// Inbound inventory.
type ClientSource interface {
    ListClients(ctx context.Context) ([]Client, error)      // {ID, MACs, IPs, Name, Hostname, Network{Name,VLAN,CIDR,Domain}, FixedIP, Tags, LastSeen, Attributes}
    ListNetworks(ctx context.Context) ([]Network, error)
    Watch(ctx context.Context, ch chan<- InventoryEvent) error // optional; polling fallback provided by the host
}

// Event sink + actions.
type Hook interface {
    Topics() []string                                   // subscribed event topics (glob)
    Handle(ctx context.Context, ev Event) error         // at-least-once; host retries with backoff and dead-letters
}
type ActionProvider interface {
    Actions() []ActionSpec                              // e.g. {Name:"block-client", Params: schema, RBAC: "integrations:unifi:act"}
    Invoke(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, error)
}

// Host services handed to integrations (Technitium IDnsServer / Gravity Instance analogue).
type Host interface {
    Zones() ZoneReader                 // read records with labels; subscribe to change events
    Inventory() InventoryWriter        // upsert synthesized clients/records owned by this integration
    Ledger(integration string) Ledger  // ownership ledger (provider IDs, hashes, generations)
    Events() EventBus
    Logger() *slog.Logger
    Metrics() prometheus.Registerer    // namespaced kudurru_integration_<type>_*
    Status() StatusWriter              // conditions, counters, lastSync
    Resolve(ctx, q dns.Question) (*dns.Msg, error) // re-enter the resolver (Technitium DirectQuery)
    Clock() clock.Clock
}

type Registry struct{ ... }
func Register(m Manifest, ctor func() Integration)     // init()-time; panics on dup like Caddy
```

Design notes:

- `Endpoint`/`Changes` are external-dns's JSON shapes (`dnsName, targets, recordType,
  setIdentifier, recordTTL, labels, providerSpecific`) so Tier A and Tier B share one plan engine
  and one wire format. The plan engine is a port *in spirit* of `plan.Calculate` (Apache-2.0, so a
  direct dependency on `sigs.k8s.io/external-dns/plan` + `endpoint` is also acceptable; it is a
  heavy module - vendor only those two packages or re-implement the ~400 lines).
- Every integration *instance* gets: a stable `name`, its `spec`, resolved secrets, and a
  per-instance namespace in the store (`/kudurru/integrations/<name>/{ledger,state,status}`).
- **Leader-scoped execution**: integrations are singletons per cluster; the host runs each
  instance on the node that holds the lease `integration/<name>` (k8s Lease or embedded-etcd
  election). Status is written to the shared store so any node's API can serve it.
- Polling cadence, backoff, jitter, rate-limit buckets and retry classification (soft vs hard
  error, copied from external-dns's `SoftError`) live in the **host**, not in each integration.
- Secrets: spec never contains secret values; it contains `SecretRef{name, key}` (k8s) or
  `{file | env}` (Docker). `SecretResolver` reads them at Provision and on rotation (file watch /
  Secret informer) and re-provisions. Status shows `secretRef` fingerprint, never the value.

### 5.3 Declaring integrations: Docker (YAML) vs Kubernetes (CRD)

Same JSON Schema, two carriers. The YAML form is literally the CRD `spec` minus metadata; the
operator's CRD `openAPIV3Schema` is generated from the manifest's `ConfigSchema`, so there is one
validator.

Docker / single-instance (`kudurru.yaml`):

```yaml
integrations:
  - name: cf-public
    type: cloudflare
    enabled: true
    credentials:
      apiToken: { file: /run/secrets/cf_token }     # or { env: CF_API_TOKEN }
    spec:                                            # type-specific, validated by ConfigSchema
      zones: [...]
  - name: unifi-home
    type: unifi
    credentials: { apiKey: { file: /run/secrets/unifi_key } }
    spec: { endpoint: https://192.168.1.1, site: default, poll: 60s, inventory: {...} }
```

Kubernetes (operator):

```yaml
apiVersion: kudurru.io/v1alpha1
kind: Integration
metadata: { name: cf-public, namespace: dns }
spec:
  type: cloudflare
  clusterRef: { name: home }            # which kudurru cluster (CR) runs it
  credentials:
    apiToken: { secretRef: { name: cloudflare-token, key: token } }
  cloudflare:                            # one-of block keyed by type, like external-secrets `spec.provider.<name>` / Caddy inline_key
    zones:
      - zone: example.com
        select: { labels: { publish: cloudflare } }
        defaults: { proxied: false, ttl: 1 }
        policy: sync
  resync: 10m
  suspend: false
status:
  observedGeneration: 3
  conditions:
    - { type: Ready, status: "True", reason: Synced, lastTransitionTime: ... }
    - { type: Authenticated, status: "True", reason: TokenVerified }
    - { type: Synced, status: "False", reason: RateLimited, message: "429 retry-after 37s" }
  lastSyncTime: ...
  records: { managed: 42, drift: 1, conflicts: 0 }
  provider: { rateLimitRemaining: 1150, apiVersion: "v4", sdk: "cloudflare-go v7.12.0" }
```

Why one CRD (`Integration` with a typed one-of block) rather than one CRD per integration:
the manifest already carries the per-type JSON Schema, CEL `x-kubernetes-validations` can enforce
"exactly one of {cloudflare, unifi, ...} matches `spec.type`", RBAC is simpler, and the UI lists
one kind. (external-secrets uses the same `provider.<name>` one-of; Crossplane uses per-provider
`ProviderConfig` kinds - heavier.) Keep the door open for `ClusterIntegration` (cluster-scoped,
namespace selectors) later.

Related k8s objects the operator should also offer so records can be declared declaratively:
`Zone` (authoritative zone + policy), `Record`/`RecordSet` (with `labels` and per-sink overrides),
and accept external-dns `DNSEndpoint` CRs via the external-dns source, so users keep their
existing tooling.

### 5.4 Reconcile loop (shared by all RecordSinks)

```
loop(integration, zone):
  desired  := host.Zones().Select(zone, spec.select) |> applyOverrides(spec.defaults, record.sinks[name])
            |> sink.AdjustEndpoints
  current  := sink.Records(zone) (cached; refreshed every resync or after apply)
  ledger   := host.Ledger(name).Load(zone)
  plan     := Plan{Current: current, Desired: desired, Owner: clusterID, Policy: spec.policy,
                   OwnershipMarkers: [ledger, commentMarker("kudurru:owner="), txtHeritage("external-dns", spec.adoptFrom)]}
  changes  := plan.Calculate()           // owner-mismatch -> Conflict condition + skip; unowned -> skip unless adoptUnowned
  if dryRun: record changes in status.preview; return
  res      := sink.ApplyChanges(zone, changes)   // batched; soft errors -> requeue(backoff, Retry-After)
  ledger.Record(res.ProviderIDs, hashes, generation)
  status.Update(Synced, counts, lastSyncTime)
triggers: zone change events (debounced 2s..5s), resync ticker (10m default), manual, secret rotation, spec change (re-provision)
```

Properties: idempotent (re-running with no changes produces zero API calls beyond the cached
read), level-triggered (never trusts an event alone), bounded (per-run API budget; aborts and
requeues when `Ratelimit` remaining < reserve), observable (`kudurru_integration_sync_duration_seconds`,
`..._changes_total{op}`, `..._errors_total{class}`, `..._rate_limit_remaining`), and safe-by-default
(no deletes of unowned records; `policy: upsert-only` is one line away).

ClientSource loop is simpler: poll -> normalise -> diff against previous snapshot -> upsert/expire
synthesized records and group memberships (expire after `ttl * 3` missed polls) -> emit
`client.seen/left` events for Hooks.

### 5.5 Out-of-process contract (Tier B)

Serve/consume **exactly** the external-dns webhook routes so existing providers work as kudurru
sinks and kudurru works as an external-dns provider:

```
GET  /                 -> {"filters":[".example.com"]}                     (negotiate; Accept: application/external.dns.webhook+json;version=1)
GET  /records          -> [Endpoint]
POST /adjustendpoints  -> [Endpoint]
POST /records          <- Changes{create,updateOld,updateNew,delete}      (204)
GET  /healthz, /metrics (separate listener)
```

kudurru extensions (negotiated by an additional media type
`application/kudurru.integration+json;version=1` on `GET /`, which may return
`{"filters":[...],"capabilities":["records","clients","hooks","actions"],"manifest":{...}}`):

```
GET  /clients          -> [Client]            (ClientSource)
GET  /networks         -> [Network]
POST /events           <- Event                (Hook; respond 2xx; 5xx = retry; HMAC header X-Kudurru-Signature)
GET  /actions          -> [ActionSpec]; POST /actions/{name} <- params
```

Rules copied from external-dns: 2xx success (204 for apply), only 5xx retried, 4xx permanent,
3xx forbidden, body cap 32 MiB, context cancellation propagated, localhost sidecar recommended but
any URL allowed with mTLS or bearer token configured on the `Integration` (`spec.webhook: {url,
auth: {bearerSecretRef|mtls}, timeouts}`). The kudurru CLI gets `kudurru integrations webhook
conformance <url>` (like running an in-tree provider with `--webhook-server`) so authors can test.

### 5.6 Permissions, RBAC, secrets, UI

- RBAC verbs: `integrations:read`, `integrations:write` (create/edit spec), `integrations:secrets`
  (bind secrets), `integrations:act` (invoke actions, e.g. UniFi block); Technitium's lesson:
  installing/enabling code-bearing integrations (Tier C) is admin-only.
- Secrets never leave the node that resolved them; the API returns `secretRef` + fingerprint;
  `kudurru integrations check <name>` runs `Check()` and prints conditions.
- UI: generic form from `ConfigSchema` (like HA config flows), status conditions, last sync,
  "preview changes" (dry-run plan table), per-integration log tail and metrics, and the
  inventory view (clients discovered, which group, synthesized names).
- Docs: each in-tree integration ships `README.md` with Name / Description / Syntax / Examples
  sections (CoreDNS plugin README convention) and a table of required remote permissions
  (Cloudflare: Zone:Read + DNS:Edit scoped to zones; UniFi: API key from a read-only admin unless
  push is enabled).

### 5.7 Package layout (proposal)

```
ee/integration/            registry, manifest, lifecycle, leader-scoped runner, plan engine, ledger, status, secret resolver
ee/integration/webhook/    Tier-B client (consume) + server (serve external-dns contract)
ee/integration/cloudflare/ sink: cloudflare-go v7, batch, rate budget, comment/tag ownership, adopt external-dns TXT
ee/integration/unifi/      clientsource: unpoller/unifi (official v1 + legacy), name synthesis, groups; optional push via fork of go-unifi or raw HTTP
ee/integration/libdns/     generic sink adapter over libdns providers (opt-in build tags per provider)
ee/integration/{tailscale,proxmox,homeassistant,mqtt,webhookhook}/
api/v1alpha1/                    Integration CRD types (+ Zone/Record), generated from manifests
cmd/kudurru/integrations.go      `kudurru integrations list|check|sync|preview|webhook conformance`
```

---

## 6. What kudurru should borrow

1. **external-dns webhook contract as the lingua franca** (routes, media type, `Endpoint`/`Changes`
   JSON, 204/5xx semantics). Be an external-dns provider natively and reuse it for third-party
   sinks - zero new learning for the k8s crowd.
2. **external-dns plan + ownership rules**: per-(name,type) table, CNAME exclusivity, owner
   mismatch -> skip + metric, `sync/upsert-only/create-only` policies, soft-error classification
   (429/5xx retry next loop), cached provider reads, per-owner IDs that must differ across clusters.
3. **Ownership markers that survive tool changes**: Cloudflare `comment`/`tags` for kudurru's own
   mark, plus *reading* `heritage=external-dns,external-dns/owner=` TXT records to adopt.
4. **Cloudflare batch endpoint** with 200-op chunks, deletes-first ordering, per-record fallback,
   and a global 1,200/5 min token bucket shared across sinks of one account; `ttl: 1` + proxied
   rules; scoped tokens verified via `/user/tokens/verify`.
5. **UniFi official Integration API** for anything that has it (sites, clients, networks, DNS
   policies) with `X-API-Key`, `offset/limit/filter` paging and the `{count,totalCount,data}`
   envelope; use the legacy `/rest/user`, `/stat/sta`, `/v2/.../active-leases` only for alias,
   hostname, fixed IP, VLAN and leases, via **unpoller/unifi v6 (MIT)**.
6. **Caddy's module lifecycle** (`New -> strict JSON unmarshal -> Provision -> Validate -> Start ->
   Cleanup`, namespaced registry, panics on duplicate id) and **CoreDNS's fixed stage order** for
   any in-process query-path hooks.
7. **Gravity's role runtime**: constructor registry, `Start(ctx, config)` with restart-on-config-
   change, event bus with string topics, `ErrRoleNotConfigured` idling, and its external-dns
   UID-per-target trick to make deletes exact.
8. **Technitium's hook taxonomy** (request-controller, blocking, authoritative, record-type
   handler, post-processor, query-log sink, query-log *reader*) as the names of kudurru's
   internal pipeline stages - even if only in-tree code attaches to them in v1.
9. **Manifest-driven UI** (Grafana `plugin.json` / HA `manifest.json`): id, version,
   capabilities, `iot_class`-like polling/push flag, JSON Schema config, secret keys, docs link;
   one generic settings form and one generic status card.
10. **external-secrets `spec.provider.<name>` one-of CRD shape with `secretRef{name,key}` and
    `status.conditions[Ready]`** for the `Integration` CRD; same schema reused as the Docker YAML.
11. **libdns** as the inner provider layer for "any other DNS provider" sinks (MIT, ~100 providers).

## 7. What kudurru should avoid

1. **Runtime-loaded in-process code from a store** (Technitium apps / Pi-hole-style scripts):
   privilege escalation by design, forces cluster-wide binary sync, and breaks the Apache-2.0
   story if the store hosts GPL apps. Compile-time Go in-tree + HTTP out-of-process instead.
2. **Shipping go-plugin as the primary extension API in v1**: proto per interface, broker, local-
   only, MPL dependency surface; keep it as a *possible* Tier C for query-path hooks only.
3. **TXT ownership records as the *only* ownership source** at Cloudflare: public, doubles record
   count, lost on manual cleanup, and a shared owner id across clusters + `sync` policy deletes
   other clusters' records (documented external-dns footgun). Use a ledger + comment/tag marker,
   and never delete unowned records.
4. **Hand-maintained second "public" zone** for split-horizon: use labels + per-sink overrides on
   one zone; otherwise drift is guaranteed.
5. **Depending on `paultyng/go-unifi`** (archived) or on the undocumented UniFi v2/legacy API
   for anything the official API covers; isolate legacy calls behind a feature flag with version
   detection via `GET /v1/info` and tolerate 404s per endpoint (unpoller's `--discover` approach).
6. **Treating UniFi `name` as a DNS label without normalisation**: aliases contain spaces/emoji;
   slugify, dedupe, and keep a stable MAC->name mapping; do not re-point names when IPs flap
   within a lease.
7. **Pinning cloudflare-go loosely**: v7 minors ship breaking changes in other packages; pin an
   exact version, keep the sink's use of the SDK thin (records + zones + batch), and wrap errors so
   SDK type changes do not leak.
8. **Secrets in spec/status/logs**: no token values in YAML, CRs, status, or `kudurru integrations
   list` output; refs + fingerprints only; `file:`/`env:`/`secretRef` carriers only.
9. **Event-only sync**: every sink must have a periodic full resync and a dry-run/preview path;
   never rely on a change event alone (external-dns, Gravity and Caddy all re-derive from state).
10. **Blocking the DNS data path on integrations**: integrations run on the leader, off the query
    path; a wedged Cloudflare API must never slow resolution or zone transfers.

## 8. Open questions

- UniFi: does `Client.name` in the official API equal the user alias when set, and what is the
  fallback (hostname vs OUI vendor)? Needs a live console to confirm; also which Network app
  version first exposed `dns/policies` (spec history goes back to v9.1.120 but per-endpoint
  introduction is not recorded in the index).
- UniFi legacy `static-dns` JSON field names (`key` vs `name`, `record_type` vs `type`) are
  inferred from Pulumi/terraform-provider resources and a community CSV tool - verify against a
  console before relying on them for push.
- Cloudflare tags are paid-plan only; decide whether the ownership marker is comment-only (works
  everywhere, 100 chars on Free) or comment + tag when available.
- Whether to vendor `sigs.k8s.io/external-dns/{plan,endpoint}` (Apache-2.0, heavy module graph)
  or re-implement the plan table (~400 lines) - leaning re-implement with a conformance test
  against external-dns's `plan_test.go` cases.
- Tailscale API details (OAuth scopes, split-DNS endpoint) were not verifiable this session.
- Leader election primitive for integrations when running single-instance in Docker (trivial)
  vs k8s (Lease) vs embedded-etcd cluster (election key) - belongs to the clustering design.

## 9. Source index

- external-dns: `docs/tutorials/webhook-provider.md`, `api/webhook.yaml`, `provider/provider.go`,
  `provider/webhook/{webhook.go,api/httpapi.go}`, `endpoint/{endpoint.go,labels.go}`,
  `registry/txt/registry.go`, `registry/mapper/mapper.go`, `docs/registry/{txt,registry}.md`,
  `plan/{plan.go,conflict.go,policy.go}`, `provider/cloudflare/{cloudflare.go,cloudflare_batch.go}`,
  `docs/tutorials/{cloudflare,pihole,crd}.md`, `apis/v1alpha1/dnsendpoint.go`.
- CoreDNS: `plugin.cfg`, `plugin.md`, `plugin/plugin.go`, `core/dnsserver/{register.go,
  zdirectives.go,directives.go}`, `core/plugin/zplugin.go`; https://coredns.io/explugins/.
- Caddy: `modules.go`, `caddy.go`, `context.go`; https://github.com/caddyserver/xcaddy.
- go-plugin: `README.md`, `plugin.go`, `client.go`, `server.go`, `discover.go`, `examples/grpc/README.md`.
- Technitium: `DnsServerCore.ApplicationCommon/*.cs` (+ `technitium.md` §11).
- Gravity: `pkg/roles/role.go`, `pkg/instance/{instance.go,role_instance_hook.go,types/events.go}`,
  `pkg/externaldns/{server.go,config.go}`, `cmd/external-dns` (+ `gravity.md`).
- cloudflare-go: `go.mod`, `LICENSE`, `dns/record.go`, README (raw GitHub); Go proxy version lists;
  Cloudflare changelog "go-sdk v7.0.0" (2026-04-30).
- Cloudflare docs: `/dns/manage-dns-records/how-to/batch-record-changes/`,
  `/fundamentals/api/reference/limits/`, `/fundamentals/api/get-started/create-token/`,
  `/api/resources/dns/subresources/records/methods/{list,create,batch}/`,
  `/dns/manage-dns-records/reference/record-attributes/`.
- UniFi: `https://developer.ui.com/llms.txt`, `https://developer.ui.com/network/v10.6.106/
  {openapi.json,llms.txt}`; unpoller `types.go, integration*.go, clients.go, users.go, networks.go,
  dhcp_leases.go, dns_policies.go, remote.go, unifi.go`; go-unifi `unifi/{user.generated.go,
  user.go,network.generated.go,unifi.go}`; libdns/unifi README; Pulumi `unifi.dns.Record` docs;
  `denisvinciguerra/unifi-bulk-dns` README.
- Others: libdns `libdns.go`; Grafana plugin.json reference; Home Assistant integration manifest
  docs; external-secrets SecretStore API page; Proxmox `pve-docs/api-viewer/apidoc.js`;
  Pi-hole v6 API details from `pihole.md`.
