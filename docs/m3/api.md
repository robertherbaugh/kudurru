# M3 work package: api

The REST API server under `/api/v1` and the external-dns webhook
(docs/architecture.md §10, §3.4 `internal/api`, §5.6, §9.6, §8.8, D31, D70).

## What was built

### `internal/api` (new)

- **Server.** `api.New(api.Options{...})` returns a `*Server` that is an
  `http.Handler`; `Run(ctx)` serves `listeners.api.address`, `Serve(ctx, ln)`
  serves an existing listener. huma v2 (MIT) operations on the Go 1.22+
  `net/http` mux (`humago.NewWithPrefix`), OpenAPI 3.1 at
  `/api/v1/openapi.json`, Scalar at `/api/v1/docs`. Only `Store` and
  `Validator` are required; every other dependency switches its routes to
  501 (`not_implemented`) or 503 (`unavailable`) when nil, so partial wirings
  and tests work.
- **Envelope.** Success bodies are the resource, or `{items, continue,
  took_ms}`; errors are `{error:{key,message,hint}, took_ms}`. huma's own
  errors (request validation, body limits, negotiation) are converted to the
  same envelope; huma's 422 for schema violations becomes 400 `invalid` with
  the field path, 422 stays for semantic errors. Keys: `invalid`,
  `unauthenticated`, `forbidden` (hint `{"missing":{resource,verb,scope}}`,
  repeated as `error.missing`), `csrf`, `not_found`, `conflict`, `managed_by`
  (hint = owner, e.g. `file:objects.d/zones.yaml`; `error.objects` lists
  `{kind, namespace, name, managedBy}` when several objects are refused),
  `default_zone`, `zone_not_empty`, `precondition_failed`
  (412), `unprocessable`, `too_large`, `rate_limited`, `read_only` (503,
  ha-static), `host_only`, `env_locked`, `not_implemented`, `unavailable`,
  `internal`. Kubernetes-shaped object bodies stay pure objects (re-appliable
  verbatim); their duration is in `Server-Timing`, every other body carries
  `took_ms`.
- **Middleware (§8.8).** Recover → RequestID (kept from trusted proxies) →
  RealIP (`listeners.api.trustedProxies`, right-most untrusted
  X-Forwarded-For) → SecurityHeaders (CSP `default-src 'self'`,
  X-Frame-Options DENY, nosniff, Referrer-Policy, HSTS when TLS terminates
  here or a trusted proxy says https) → AccessLog + metrics → Go 1.25
  `http.CrossOriginProtection` → RateLimit (`Options.RateLimit`, applied to
  `/api/v1/auth/login` only) → Authenticate (`Options.Authenticate`) → actor
  (`sqlite.WithActor` so the sqlite driver's audit rows name the principal)
  → per-operation Authorize (CSRF check for cookie sessions on unsafe
  methods, then the route's declared permission; 401/403) → handler →
  audit. Metrics: `kudurru_api_requests_total{method,route,code}`,
  `kudurru_api_request_duration_seconds{route}`; `route` is the operation
  pattern (bounded), `other` for the SPA and unknown paths.
- **Permissions (D31).** Every operation is registered through one helper
  that takes a `perm` declaration: `public`, `authenticated`, a fixed
  `{resource, verb}` with cluster or `zone:{zone}` scope, or by-kind for
  `/objects/{kind}`. Scopable routes (zone listing, record sets, objects of
  zone-scoped kinds) authorize per object at `zone:<name>` and filter lists,
  so a zone-admin binding works; non-zone kinds still refuse with 403 rather
  than an empty list. The declaration also lands in the OpenAPI document as
  `x-kudurru-permission`. Denied attempts are audited from the authz layer.
- **Write path (D70).** `PUT/POST /objects`, `/apply`, the convenience routes
  and the UI lists all go through one `writeObject`: 1.5 MiB cap (413),
  apiVersion/kind/name/namespace checks against the URL, status and
  server-owned metadata dropped, ownership (`managed_by` 409 for another
  manager, `bootstrap` objects taken over), `If-Match` (412),
  `Validator.ValidateUnstructured` (strict unknown fields, defaults), typed
  conversion, `Validator.Validate` (defaults of non-pointer structs,
  transition rules), an "unchanged" short cut identical to the objects
  loader's, then `Store.Apply` with manager `api` and the stored
  resourceVersion as the optimistic precondition. ha-static answers 503
  `read_only` from `objects.Loader.CheckAPIWrite`.
- **Routes.** 64 paths, 84 operations; everything in the contract except the
  v0.2 IAM write API:
  - objects: `GET /objects/{kind}` (namespace, equality `labelSelector`,
    `limit`, opaque `continue`), `GET|PUT|DELETE /objects/{kind}/{name}`
    (cluster-scoped) and `/objects/{kind}/{namespace}/{name}`, `POST
    /objects/{kind}` (create, 409 if it exists), `POST /apply` (multi-doc
    YAML, a JSON object/array or `kind: List`; per-document `{kind,
    namespace, name, result, error, errorKey, hint, resourceVersion}`),
    `POST /{kind}:batchDelete` for all five kinds (the mux cannot match a
    wildcard followed by a literal in one segment, so each kind has a
    literal route). A DNSZone that record sets still reference is not
    deleted alone (409 `zone_not_empty`, `error.objects` lists them), and
    the root zone `.` not at all (409 `default_zone`; D148).
  - zones: `GET /zones` (`deleteRefusal`: `default_zone` or `managed_by`
    when a delete is refused whatever the caller's permissions), `GET
    /zones/{zone}`, `DELETE /zones/{zone}` (the zone and every record set
    whose zoneRef names it, any namespace; `zones.delete` on the zone plus
    `records.delete` when record sets exist; `?dryRun=true` runs every
    check and answers the same `{zone, zoneName, type, dryRun, recordSets,
    recordSetCount, endpointCount, flushed}` without deleting; one sqlite
    transaction, on the kube driver record sets first and the zone last;
    409 `default_zone` for `.`, 409 `managed_by` with `error.objects`;
    D148), `GET|POST
    /zones/{zone}/records` (`?recordSet=` default `api-<zone>`,
    `?namespace=`; names relative to the zone, absolute, or `@` are stored
    absolute without the trailing dot), `DELETE
    /zones/{zone}/records/{name}/{type}` (`?recordSet=`,
    `?setIdentifier=`; an emptied record set is deleted), `POST
    /zones/{zone}/import` (text/dns; `?origin=` creates the zone, which
    needs cluster `zones.write`; endpoints are chunked 1,000 per record set
    `import-<zone>-N` labelled `kudurru.io/import`, and a re-import replaces
    the previous chunks), `GET /zones/{zone}/export` (text/dns, built with
    `zone.Build(ServedRecordSets)` like the replicas), `POST
    /zones/{zone}/notify` (`zones.transfer`).
  - lists and policy: CRUD on `/blocklists`, `/clientgroups`, `/policies`
    (`/{kind}/{namespace}/{name}`), `POST
    /blocklists/{namespace}/{name}/refresh`, `GET
    /blocklists/_search?domain=` (compiled snapshot `Contains` /
    `ContainsAllow`, inline entries as the fallback), `POST
    /domains/{allow|deny}/{exact|regex}` (`ui-allow` / `ui-deny`, inline
    source `ui`, created on demand, deduplicated, RE2-checked; the list is
    added to `Policy <ns>/default` when that policy is API- or
    bootstrap-managed so the decision takes effect at once).
  - runtime: `GET|POST /dns/blocking` (`blocking`, `timer` seconds,
    `until`; node-local Toggle, `scope:"node"`), `POST /cache/flush`
    (`name` flushes the common types with and without DO/CD), `POST
    /dns/query` (pipeline in-process, synthetic client 127.0.0.1 or
    `client`, `ClientID "api-dig"`).
  - stats and log: `/stats/summary|timeseries|top|upstreams|clients/timeseries|recent_blocked`,
    `GET /clients`, `GET /queries`, `/queries/suggestions`,
    `/queries/stream` (SSE), `POST /queries/{id}/actions`, `DELETE
    /querylog`, `GET /querylog/status`. Bodies are the
    `sqlitelog` read-model structs plus `took_ms`; every one carries
    `scope:{scope:"node", nodes:[{name}], partial:false}`.
  - settings: `GET|PATCH /settings` (cluster keys), `GET|PATCH
    /node/config` (node keys), `GET /settings/_schema` (the metadata table
    with effective values).
  - cluster: `GET /nodes`, `/nodes/{name}/health`, `/cluster`, `/edge`
    (stub until the supervisors expose state), `/info/messages` (objects
    loader diagnoses, policy index diagnoses, list resolver, zonecache
    Degraded and snapshot state, ha-static, unready components).
  - auth: `/auth/status`, `/auth/login`, `/auth/oidc/{idp}/start|callback`,
    `/auth/logout`, `/auth/password`, `/auth/tokens[/{id}]` and the IAM
    read-only lists `GET
    /iam/users|serviceaccounts|rolebindings|identityproviders` are
    documented here and forwarded untouched to `Options.AuthHandler` (the
    auth stack's mux; 501 without it). Their schemas are the mirrors in
    `internal/api/authtypes.go`; `TestForwardedShapes` (cmd/kudurru)
    compares them with the stack's types and decodes the real answers
    strictly. `GET /auth/me` is served here (principal, the live roles of
    `Options.Roles`, CSRF from the session, permissions evaluated per
    resource/verb at cluster scope, `mustChangePassword`).
  - audit: `GET /audit` (kind, name, actor, since, limit, cursor or
    continue, order=asc|desc; oldest first by default, `cursor_next`),
    `GET /audit/export` (NDJSON, `audit.export`).
  - meta: `GET /version`, `/openapi.json` (+ yaml and 3.0 variants huma
    serves), `/docs`.
- **SSE.** `GET /queries/stream` subscribes to the querylog Tail, filters
  in-process (client IP/CIDR/name glob, domain glob, status list or class,
  qtype, upstream, list, zone), writes `id:`/`event: query`/`data:` with the
  GET /queries row shape, replays `Tail.Recent(0, Last-Event-ID)` first on
  reconnect (`Last-Event-ID` header or `?last_event_id=`), drains queued
  events before each flush, and sends `: ping` every 15 s.
- **UI mount.** `/` serves `Options.UIHandler` (e.g. `web.Handler`) or a
  built-in SPA handler over `Options.UI` (any `fs.FS`): files by path,
  `index.html` with `<base href>` rewritten to the path prefix for client
  routes, 404 for missing files with an extension, a placeholder page when
  no bundle is given. `listeners.api.pathPrefix` mounts everything below a
  prefix (`/kudurru` redirects to `/kudurru/`, paths outside it are 404,
  `Location` headers carry the prefix).
- **TLS.** `listeners.api.tls.certFile/keyFile` (re-read when the file
  changes, checked every 10 s), or `tls.auto: selfsigned` (the default):
  ECDSA P-256, five years, generated once into `<node.dataDir>/keys/api.crt`
  and `api.key` (0600) and reused while valid; `tls.auto: off` for plain
  HTTP behind a TLS-terminating proxy.
- **Settings.** `flatSettings` renders the effective config by schema path
  (the JSON form of the config structs); PATCH applies a JSON merge patch
  over one scope: unknown keys 400 `invalid`, keys of the other scope 400
  `invalid` naming the right route, hostOnly keys 400 `host_only`, env-set
  keys 409 `env_locked`, RFC 7386 merging inside map-valued keys, then the
  result is decoded with koanf and `Config.Validate`d like the file, handed
  to `Options.Settings` (persisted), and served from memory. The response
  lists `changed`, `restartRequired` and `persisted`.
- **Generated artifacts.** `make openapi` (`go run
  ./internal/api/openapigen`) writes `api/openapi.json` (3.1) and
  `api/openapi-3.0.json` (huma's downgrade, for generators);
  `TestOpenAPIDrift` fails when either is stale. `OpenAPIDocument(v30)`
  builds a dependency-free server so the document does not depend on the
  build, node or config.
- **Test double.** `internal/api/apitest`: `Authenticate` ("Bearer
  test:<user>[:<groups>]" or the `kudurru_test_session` cookie with a
  session carrying `CSRFToken`), `CSRF`, `Authorizer(bindings...)`,
  `Grant`, `GrantZone`, `StandardBindings()`.

### `internal/apiclient/openapi` (new, generated)

`make client-go` runs oapi-codegen v2.8.0 (Apache-2.0) with
`internal/apiclient/openapi/oapi-codegen.yaml` over `api/openapi-3.0.json`
into `zz_generated.client.go` (package `openapi`, models + client, enum
constants prefixed with their type). Main types: `Client` (one method per
operationId: `ListZones`, `PutObject`, `Apply`, `SetBlocking`,
`ListQueries`, ...), `ClientWithResponses` (`XxxWithResponse` returning
`XxxResponse` with `JSON200`/`JSON201` bodies and `JSON4xx`/`JSON5xx` as the
`Error` envelope), `NewClientWithResponses(serverURL+"/api/v1",
WithHTTPClient, WithRequestEditorFn)`. It lives below `internal/apiclient`
because the CLI agent's hand-written transport already occupies
`internal/apiclient` with a `Client` type; both coexist until the CLI
switches (follow-up). `TestGeneratedClient` drives the server through it.

### `ee/integration/externaldns` (new)

`externaldns.New(Options{Store, Validator, Cache, Namespace, ZoneSelector,
Address, Registerer, Logger})` is an `http.Handler` with `Run(ctx)`/`Serve`:
`GET /` answers the domain filter (apexes of the primary zones matching
`zoneSelector`) with `Content-Type:
application/external.dns.webhook+json;version=1` exactly (the external-dns
client compares it byte for byte), `GET /records` lists the endpoints of the
record sets in `Namespace` managed by `integration:external-dns`, `POST
/records` applies `plan.Changes` (both the v0.15+ `create/updateOld/...` and
the older capitalised keys) and answers 204, `POST /adjustendpoints` echoes.
Changes are grouped per DNS name into one `DNSRecordSet` named
`edns-<name>` (sanitised, with a hash suffix when lossy or too long, label
`externaldns.kudurru.io/name`) referencing the longest matching zone;
ownership TXT records are stored verbatim (quotes included); emptied record
sets are deleted. Writes are validated (`Validator.Validate`) and applied
with the stored resourceVersion; a conflict re-plans up to three times. A
same-named record set of another manager is never taken over (422). Status
mapping: bad JSON / missing names / names outside every zone 400, validator
refusals and ownership conflicts 422, bodies over 32 MiB 413, store failures
500 (external-dns retries only 5xx). Metrics
`kudurru_externaldns_requests_total{route,code}` and
`kudurru_externaldns_changes_total{action}`. `OptionsFrom(cfg.ExternalDNS.Webhook)`.

### `internal/audit` (new)

`Event` (§11.4 fields), `Recorder`, `Reader`, `Query`; recorders `SQLite`
(over `*sqlite.Store`'s `RecordAudit`/`ReadAudit`, the same hash-chained
table the driver writes for store mutations), `Log` (the `"log":"audit"`
stdout line), `Memory`, `Multi`; `Diff` caps before/after at 4 KiB. The API
records denied attempts and non-store mutations (blocking toggle, cache
flush, NOTIFY, list refresh, settings changes, query-log purge, audit
export).

### `internal/rbac` (additive)

`routes.go`: `Route`, `LintRoutes(perms, routes)` (no declaration, unknown
resource or verb, duplicates, public routes naming a resource),
`Permissions.Has`, `Permissions.ResourceNames`. permissions.yaml unchanged:
every route maps onto an existing resource/verb.

### `internal/config` (additive)

`listeners.api` became `APIListener`: `address`, `tls.{auto,certFile,keyFile}`,
`pathPrefix`, `trustedProxies`; new `externalDNS.webhook.{enabled,listen,namespace,zoneSelector}`.
All hostOnly and restartRequired, in the metadata table, validated; the
drift tests stay green.

## Wiring for serve (integrator)

After the pipeline, notifier and runner are built in `runServe`
(`cmd/kudurru/serve.go`). The querylog block must hoist `tail` (today a
local inside `if cfg.Querylog.Enabled`) and keep `qlWriter`.

```go
import (
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/web"
)

var qlTail *querylog.Tail // set to `tail` inside the querylog block
var qlReader *sqlitelog.Reader
if qlWriter != nil {
	qlReader = qlWriter.Reader()
}

// Audit: the sqlite driver's hash-chained table (Docker) plus the stdout line.
recorders := audit.Multi{audit.Log{Logger: logger}}
if ss, ok := st.(*sqlite.Store); ok {
	recorders = audit.Multi{audit.SQLite{Store: ss}, audit.Log{Logger: logger}}
}

apiSrv, err := api.New(api.Options{
	Store: st, Validator: v, Cache: zc, Policy: polSrc, Blocking: bs,
	Querylog: qlReader, Tail: qlTail, // Purger: when sqlitelog exports one
	Upstreams: upSet, ResponseCache: rc, Chain: chain,
	Config: cfg, // Settings: a SettingsWriter once the Kudurru singleton exists
	Objects: loader, Health: checker, Membership: membership,
	Lists: listResolver, Notifier: api.NotifyTrigger(nt.Trigger),
	// Refresher: api.RefreshFunc(...) once listfetcher has an on-demand refresh
	Audit: recorders,
	// From the auth package (M3 auth): authenticator middleware, authorizer,
	// CSRF check, login limiter, the /auth/* handler and bearer roles.
	Authenticate: authn.Middleware, Authorizer: authz, CSRF: authn.CheckCSRF,
	RateLimit: authn.LoginLimiter, AuthHandler: authn.Handler(), Roles: authn.Roles,
	UIHandler: web.Handler(web.Options{BasePath: cfg.Listeners.API.PathPrefix}),
	Logger: logger, Registerer: reg, Version: info, NodeName: cfg.Node.Name,
})
if err != nil {
	return err
}

// runner table (always-on, every replica):
{"api", false, apiSrv.Run},

if cfg.ExternalDNS.Webhook.Enabled {
	eo := externaldns.OptionsFrom(cfg.ExternalDNS.Webhook)
	eo.Store, eo.Validator, eo.Cache, eo.Registerer, eo.Logger = st, v, zc, reg, logger
	edns, err := externaldns.New(eo)
	if err != nil {
		return err
	}
	add("externaldns", false, edns.Run) // every replica (§5.6)
}
```

Notes:
- `Options.Authenticate` must put an `auth.Principal` in the context (and an
  `auth.Session` for cookie sessions, whose `CSRF` and `RolesSnapshot`
  `/auth/me` reports) and may answer 401 itself; anonymous requests pass and
  are refused by Authorize on non-public routes. `Options.Authorizer` is the
  `rbac.Chain` (bindings, SAR); a nil authorizer denies everything that is
  not public. `apitest` has doubles of all three.
- The API calls the Authorizer with `rbac.Principal{Name, Groups,
  ServiceAccount}`; ServiceAccount is `<ns>/<name>` for Kubernetes service
  accounts (`system:serviceaccount:<ns>:<name>`) and kudurru service
  accounts.
- `listeners.api.tls.auto` defaults to `selfsigned`: the first start writes
  `<dataDir>/keys/api.{crt,key}`.
- `make openapi` after any route change; `make client-go` regenerates the
  client (needs network for `go run ...oapi-codegen@v2.8.0`).

## Decisions and deviations

- Own-token routes (`/auth/tokens*`) are `authenticated` rather than
  `tokens.own`: every role holds `tokens.own`, including zone-admin, whose
  zone-scoped binding cannot satisfy a cluster-scope check; the auth handler
  scopes tokens to the caller and a token never exceeds its owner.
- `/dns/query` needs `stats.read`; `/info/messages` and `GET /node/config`
  need `settings.read`; `PATCH /node/config` needs `node.config`; `/nodes`,
  `/cluster` need `cluster.read`; `/edge` `edge.read`; `/clients`
  `clients.read`; `/stats/recent_blocked` `querylog.read` (it shows client
  addresses); `POST /queries/{id}/actions` `blocklists.write`.
- Object reads go to the store (read-your-writes after a PUT), not the zone
  cache; the zone cache serves `_search` and the external-dns reads.
- `/apply` answers 200 with per-document results even when some fail; the
  whole request fails only when the body does not parse.
- The OpenAPI document is committed in two versions because oapi-codegen
  reads 3.0 only.
- huma's global error constructor is replaced once per process (only
  `internal/api` uses huma).

## Measurements

| What | Value |
|---|---|
| `BenchmarkListZones1000` (GET /zones, 1,000 zones + 1,000 record sets, memory store, full middleware, admin) | 3.9-9.3 ms/op, 3.0 MB/op, 9,146 allocs/op (shared 8-CPU machine, noisy) |
| same as zone-admin (per-zone authorization) | 3.5-8.2 ms/op, 2.7 MB/op, 11,136 allocs/op |
| OpenAPI document | 64 paths, 84 operations, 87 schemas; 349 KB (3.1), 349 KB (3.0) |
| Generated client | 26,001 lines (`zz_generated.client.go`) |
| Binary size, `api.New` and `externaldns.New` referenced from cmd/kudurru (stripped, CGO_ENABLED=0) | 47,816,864 → 47,947,936 bytes (+131 KB); the integrator's `make size` is authoritative |
| Code | ~9,900 lines in internal/api, ee/integration/externaldns, internal/audit (tests included) |

## Surprises

- The previous attempt's WIP was not on the branch (the branch had been
  reset to the initial commit); it was recovered from unreachable commits
  (3dcec90, d66d3a6) and fast-forwarded.
- huma fills embedded parameter structs only when the embedded type is
  exported (`WindowParams`, `QueryParams`); unexported ones were silently
  ignored.
- huma reads the body of any operation whose `RequestBody` is declared, even
  without a `Body` field; the forwarded auth routes capture `RawBody` and
  hand the auth handler a fresh reader.
- Endpoint `dnsName`s are absolute with or without the trailing dot in
  `internal/zone`; a relative `www` would land outside the zone, so the
  records route canonicalises names.
- oapi-codegen generates colliding enum constants (`Client`) unless
  `always-prefix-enum-values` is set.
- huma's Scalar page loads from unpkg.com with its own CSP, against §10's
  "no CDN" (follow-up).
- The CLI agent created `internal/apiclient` by hand in parallel; the
  generated client therefore lives in `internal/apiclient/openapi`.

## Follow-ups

Added to docs/followups.md under "API": on-demand list refresh, query-log
purge, settings persistence and live reload, scatter-gather, self-hosted
Scalar, IAM reader, cluster-scope-only permissions in `/auth/me`,
external-dns `DNSEndpoint` reads and sidecar subcommand, list routes from
the zone cache, CLI switch to the generated client, audit columns. Closed or
narrowed: the API side of `WithActor`, the 1.5 MiB cap, `CheckAPIWrite`,
validate-unstructured-then-typed, `POST /dns/blocking`, REST exposure of
`Set.Status()` and cache flush.

### Query-log and stats scope (#9)

`GET /queries`, `/queries/stream` and `/stats/*` honor
`scope=cluster` and `scope=node&node=<name>`. Cluster reads contact the known
members over the authenticated TLS internal listener, with a two-second
timeout for remote reads. Local reads use the request's deadline. Responses
list every requested node and set `partial: true`
when any node fails. A selected node is served by that node; unknown nodes are
404, and an unavailable selected node or a cluster with no readable nodes is
503. A coordinator without a local query log can still read its peers.

Cluster query pages sort by timestamp, then node name and row ID. Their opaque
cursors retain the per-node positions, original membership and an upper time
bound, so later inserts do not displace a traversal. Keep the filters when
following a cursor; the page size may change. Historical cursor maps are limited
to 64 nodes, including departed nodes retained as partial results; cluster query
pagination returns 503 when initial membership exceeds this limit. Row identity
is `(node, id)`.
When a page is partial, restarting the search after recovery is necessary to
obtain the missing node's complete history. Query-log storage and retention
remain per node; aggregation cannot recover a replaced pod's lost emptyDir.

Additive rollups are summed in common UTC buckets. Summary distinct counts
are exact unions up to 10,000 hashes and use bounded HyperLogLog sketches above
that threshold (`approximate: true`). Latency percentiles come from merged
histograms. Top lists and client series merge up to four times the requested
number of candidates per node and are marked approximate; top lists expose
`cutoff_rank`. Domains-on-lists uses the largest reported compiled count,
since the list configuration is replicated rather than additive. Upstream
server health includes its node.

Live tails multiplex privacy-transformed peer streams. Cluster SSE IDs are
opaque per-node resume maps; node SSE IDs remain decimal. `event: scope`
reports the initial contributing nodes and updates when a peer's availability
changes. The dashboard displays these updates. Failed peers are retried with
jittered exponential backoff up to 30 seconds, resuming from their last received
ID; the backoff resets after a successful connection. Membership changes
end the connection so EventSource reconnects against the current pods. Replay is bounded
by each node's in-memory ring, as for a single-node stream. Subscribers and
writes are bounded so slow dashboard clients cannot block DNS ingestion.
