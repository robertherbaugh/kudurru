# M3 work package: cli (the `kudurru` client verbs)

Branch `worktree-wf_287eb68b-e67-4`, based on `3e52e5a`. Covers
docs/architecture.md §3.5 (CLI), §8.3 (CLI login), §8.7 (`kudurru admin` over
the unix socket) and the client side of the §10 API contract.

Packages: `internal/apiclient` (new, hand-written client), `internal/cli` (new:
contexts, keyring, kinds and tables, manifests, admin socket client),
`cmd/kudurru` (every file except `serve.go`, `edge.go` and `vip.go`).
`internal/config` is untouched. New dependency: `github.com/zalando/go-keyring`
v0.2.8 (MIT; pulls in `godbus/dbus/v5` BSD-2 and `danieljoos/wincred` MIT, all
pure Go). `golang.org/x/term` moved from indirect to direct. No NOTICE entry is
needed: nothing was adapted from another project.

The previous attempt's work-in-progress was not on the branch (it was still at
the initial commit), so the package was written from scratch.

## What was built

### `internal/apiclient` (the transport)

A small client over the contract, kept in `client.go` with typed helpers in
side files so that a generated client (the api agent's
`internal/apiclient/openapi`) can sit underneath without the CLI changing:

- `New(server, Options{Token, InsecureSkipVerify, CAFile, Timeout, UserAgent, HTTPClient})`.
  A bare host means https. A path in the URL is kept as a prefix, for servers
  behind a path-prefix proxy. Every client has a cookie jar for the password
  login.
- `Do(ctx, method, path, query, in, out)` plus `Get`, `Post` and `Delete`.
  Paths are relative to `/api/v1` unless they start with `/api/`. `in` may be
  any value (marshalled), `[]byte` or an `io.Reader`. `out` may be a value, an
  `io.Writer` or a `*[]byte`. `Raw(ctx, Request)` returns the response as is,
  for text/dns, merge patches, If-Match and SSE.
- Auth: `Authorization: Bearer <token>` when a token is set. With the session
  cookie, `X-Kudurru-CSRF` is sent on every mutating request; `Me` and `Login`
  pick up the `csrf` value.
- Errors: every non-2xx response becomes `*apiclient.Error` with `StatusCode`,
  `Key`, `Message`, `Hint`, `Missing{resource,verb,scope}`, `RetryAfter` and
  `TookMS`. The 403 `missing` block is read from the error object, from a hint
  object, or from a hint that is a JSON string. Bodies that are not the
  envelope (a proxy's HTML) keep their first line. `Error()` reads, for
  example, `forbidden: not allowed (missing zones.write on zone:home)` or
  `managed_by: ... (managed by file:objects.d/zones.yaml)`. Helpers:
  `IsKey` and `IsStatus`.
- Pagination: `Pages` and `ListAll` follow `continue`, and stop if the server
  repeats a token.
- SSE: `Subscribe(ctx, path, query, SubscribeOptions{LastEventID, Reconnect, RetryDelay, MaxDelay}, fn)`.
  It parses the WHATWG event-stream format: multi-line data, comments, `retry:`
  and CRLF. On reconnect it resends `Last-Event-ID` and backs off
  exponentially. Transient statuses (429, 502, 503, 504) are retried. A 401,
  403 or 404 on the first connection is returned at once. `ParseSSE` is
  exported.
- Typed helpers: `Me`, `Status`, `Login`, `Logout`, `CreateToken`,
  `ListObjects`, `GetObject`, `PutObject` (If-Match), `DeleteObject` and
  `Apply` (sends a JSON array and accepts a bare array, `{items}` or
  `{results}` back), plus `ObjectPath` and `Seg`.

### `internal/cli` (client-side state and formatting)

- **Contexts file**: `$KUDURRU_CLI_CONFIG`, else
  `$XDG_CONFIG_HOME/kudurru/config.yaml` (`~/.config/kudurru/config.yaml`).
  It holds `apiVersion/kind: Config`, `currentContext` and
  `contexts[]{name, server, token?, tokenStore, namespace, insecureSkipVerify, caFile, user}`.
  Unknown fields are rejected on load. Saves are atomic, mode 0600 in a 0700
  directory.
- **Resolve** applies precedence in this order: flags (`--context`,
  `--server`, `--token`, `-n`), then the environment (`KUDURRU_CONTEXT`,
  `KUDURRU_SERVER`, `KUDURRU_TOKEN`), then the current context. A context's
  stored token and its TLS settings are only used when the server is that
  context's own server (compared after normalisation), so `--server`
  pointing elsewhere never leaks it.
- **Credentials**: tokens go to the OS keyring (service `kudurru`, account =
  context name) through `SystemKeyring`, a go-keyring wrapper with a 3 s
  timeout so a locked Secret Service cannot hang the CLI. When the keyring
  refuses, the token stays in the 0600 file (`tokenStore: file`) and the CLI
  prints a warning. `KUDURRU_KEYRING=file` skips the keyring entirely
  (containers, CI).
- **Kinds**: `dnszones` (zone, zones, dnszone, dz; cluster-scoped),
  `dnsrecordsets` (rrset, rrsets, dnsrecordset, recordset), `blocklists`
  (list, lists, blocklist, bl), `clientgroups` (cg, clientgroup) and
  `policies` (pol, policy). CamelCase kinds and `resource.group` spellings are
  accepted. The print columns follow the CRD printcolumns, plus MANAGED-BY
  for zones (from the `kudurru.io/managed-by` annotation) and AGE everywhere;
  `-o wide` adds more. `Table` renders kubectl-style tables.
- **Manifests**: files, directories (`-R` recurses, `.yaml/.yml/.json` in
  lexical order) and `-` for stdin. Accepts multi-document YAML, JSON objects,
  JSON arrays and `List`/`*List` documents. Documents that are empty or only
  comments are skipped. Errors name the file and the document index.
- **Admin socket client** (`AdminClient`): protocol `auto` (default) speaks
  HTTP/1.1 over the unix socket (`POST /admin/v1/reset-password`,
  `POST /admin/v1/create-token`, `GET /admin/v1/list-users`, API-style
  errors). That is what the auth agent's `internal/auth/adminsock` serves in
  its worktree. When the peer does not answer HTTP, it falls back to the
  task's NDJSON protocol (`{op,args}\n` → `{ok,result,error}\n`). `--protocol`
  forces either one.

### Commands (`cmd/kudurru`)

Global flags on the root: `--cli-config`, `--context`, `--server`, `--token`,
`-n/--namespace`, `-o/--output table|wide|json|yaml|name`,
`--insecure-skip-tls-verify`, `--ca-file` and `--request-timeout`. `--config`
still names the server's kudurru.yaml. Help is grouped into Server, Objects,
DNS and lists, Observe, and Setup and administration.

| Verb | API |
|---|---|
| `login --server URL (--token kdr_… \| --token-stdin \| -u user [-p pw \| --password-stdin \| prompt]) [--name ctx]` | `GET /auth/me`; password: `POST /auth/login`, `GET /auth/me` (csrf), `POST /auth/tokens` {name: kudurru-cli@host, expiresIn?}, `POST /auth/logout` |
| `logout [ctx] [--remove]` | (local) |
| `config get-contexts \| use-context \| current-context \| delete-context \| set-context` | (local) |
| `get <kind> [name…] [-n ns] [-A] [-l sel] [-o …]` | `GET /objects/{kind}` (all pages), `GET /objects/{kind}[/{ns}]/{name}` |
| `apply -f file\|dir\|- [-R]` | `POST /apply` (JSON array; namespaced docs without a namespace get `-n`/context/`default`) |
| `delete <kind> <name…> \| -f … [--ignore-not-found]` | `DELETE /objects/{kind}[/{ns}]/{name}` |
| `describe <kind> <name>` | `GET /objects/…` + `GET /info/messages` filtered to the object |
| `zones list \| get \| import <zone> -f file [--origin] \| export <zone> [--out] \| notify <zone>` | `/zones…` (import as `text/dns`, export with `Accept: text/dns`) |
| `records list <zone> \| add <zone> <name> <type> <targets…> [--ttl --record-set --comment --set-identifier] \| rm <zone> <name> <type>` | `GET\|POST /zones/{zone}/records`, `DELETE /zones/{zone}/records/{name}/{type}` |
| `lists list [-A] \| refresh <name…> \| search <domain> \| allow\|deny <domain…> [--regex --list]` | `/blocklists`, `/blocklists/{ns}/{name}/refresh`, `/blocklists/_search`, `/domains/{allow\|deny}/{exact\|regex}` |
| `blocking status \| enable \| disable [5m] [--until RFC3339]` | `GET\|POST /dns/blocking` |
| `cache flush [name]` | `POST /cache/flush?name=` |
| `query <name> [type] [--client ip]` | `POST /dns/query`; dig-style output with flags, the kudurru verdict, sections, query time and node |
| `stats summary [--window]`, `stats top --dim … [--top N --blocked --window]` | `/stats/summary`, `/stats/top` (upstreams too) |
| `logs [--limit N] [-f] [--client --domain --status --status-class --qtype --upstream --list --zone --since --until]` | `GET /queries` following `cursor_next`, printed oldest first; `-f` streams `/queries/stream` from the newest paged id with `Last-Event-ID` resume and client-side filtering; `-o json` prints ndjson |
| `backup --out f [--safe --exclude-managed]`, `export [kind…]`, `restore -f f` | `GET /objects/*` all namespaces, cleaned for re-apply; restore uses `POST /apply` |
| `import bind <zone> -f file` | same as `zones import` |
| `settings get [key…] [--scope]`, `settings set key=value… key-` | `GET /settings`, `GET /node/config`; `PATCH` (merge patch) by key scope; hostOnly and unknown keys refused locally with the env var name |
| `nodes`, `audit [--since --kind --name --actor --limit]` | `/nodes`, `/audit` (cursor_next) |
| `api [METHOD] <path> [-d inline\|@file\|@-] [-H …] [--content-type]` | raw |
| `completion bash\|zsh\|fish\|powershell` | dynamic completion of kinds, object names, zones and contexts |
| `admin reset-password [user] [--password(-stdin) --create] \| create-token [--user --description --expires-in] \| list-users` | admin socket |
| `doctor [--skip-api --skip-ports]` | adds dataDir writability and mode, port bindability, API reachability (`/version`), token validity (`/auth/me`), diagnoses (`/info/messages`); exits non-zero when a check fails |

`serve`, `health`, `version` and `config show|schema` are unchanged. `doctor`
moved to `doctor.go` and keeps its existing lines. `main` runs
`newRoot().Execute()` as before; `logs --follow` installs its own SIGINT and
SIGTERM context so Ctrl-C ends a stream with status 0. Every other command
keeps the default signal behaviour, so Ctrl-C at the password prompt still
kills the process.

`backup --safe` redacts URL userinfo passwords and the query parameters
token, key, apikey, api_key, secret, password, pass, auth, access_token and
sig. `--exclude-managed` drops objects owned by `file:` or `integration:`.
The backup file is written 0600. `restore` warns when a document still holds
`REDACTED`.

`doctor` probes the local listener when no context or `--server` is set. A
plain-HTTP 400 "…HTTPS server" answer switches it to https without
verification, which covers the api agent's `listeners.api.tls.auto:
selfsigned`.

## Serve wiring

None. The CLI does not touch `serve.go`. For the integrator:

1. **The edge and vip commands.** The root is built in `newRootWith(o *cliOpts)`
   in `cmd/kudurru/main.go`. When the edge/vip agent's `edge.go` and `vip.go`
   land, register their constructors in the "server" group next to `serve`:

   ```go
   add("server", newServeCmd(&configPath), newHealthCmd(), newDoctorCmd(&configPath, o), newVersionCmd(),
       newEdgeCmd(&configPath), newVIPCmd(&configPath)) // names as the edge agent defines them
   ```

   The root has persistent flags `-n` and `-o`; the supervisors must not
   define flags with those shorthands.
2. **The admin socket.** `kudurru admin` needs the auth agent's
   `adminsock.Server` listening on `/run/kudurru/kudurru.sock` (the default of
   both sides). No CLI change is needed.
3. **The API.** Every verb only needs `internal/api` mounted on
   `listeners.api.address`.

## Measurements

Taken on the shared 8-CPU machine (five other agents running), Go 1.27.2,
linux/amd64.

| What | Value |
|---|---|
| Stripped CGO_ENABLED=0 binary | 48,677,024 bytes, up from 47,808,672 at 3e52e5a (+868 KB, +1.8 %); 95 % of the 51,234,000 D75 budget (2.5 MiB headroom) |
| `kudurru version` wall time, 50 runs | 94 ms per run, the same as the 3e52e5a binary (the CLI adds no init work) |
| A refused API call (`get zones --server http://127.0.0.1:1`) | 78 ms per process, mostly process start |
| Top-level commands | 30 (from 5) |
| Tests | `cmd/kudurru` 1.7 s, `internal/apiclient` 1.4 s, `internal/cli` 1.0 s under `-race` |
| Golden files | 39 under `cmd/kudurru/testdata/golden` (`go test ./cmd/kudurru -update` rewrites them) |

Cross-builds: `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/kudurru`
passes, and `GOOS=windows go vet ./internal/cli ./internal/apiclient` passes.
go-keyring uses `/usr/bin/security` on macOS and wincred on Windows, with no
cgo on either.

## Tests

- `cmd/kudurru` runs against `fakeapi_test.go`, an httptest server for the
  contract subset. It has bearer and cookie+CSRF auth, `continue` and
  `cursor` pagination (small pages force several round trips), managed_by
  conflicts, the SSE stream with scripted reconnects, and response shapes
  copied from the api agent's `internal/api` types: DiagnosisMessage,
  SettingsBody, audit.Event, ListSearchHit, the query result, nodes and
  ApplyResult with errorKey and hint.
  - Golden tables for every kind, plain and wide, `-A`, `-l` and `-o name`.
    Also `describe`, zones, records, lists, query (forwarded and blocked),
    stats summary and top (upstreams too), nodes, audit, settings, export
    and admin list-users.
  - Login with a token, with `--token-stdin`, and with a password (the
    test checks the CSRF header on the token mint and that the session is
    logged out). Also the keyring fallback to the 0600 file and six failure
    cases.
  - Context switching (`use-context`, `--context`, `KUDURRU_CONTEXT`,
    `delete-context` dropping the keyring entry). Env and flag precedence,
    including that a foreign `KUDURRU_SERVER` gets no token.
  - Multi-document apply from a file and from stdin, a directory with
    `-R`, per-document errors, and malformed manifests.
  - `logs`: three pages, filters, and ndjson. `logs --follow`: duplicate
    suppression, `Last-Event-ID` 5 then 7 across a dropped stream, the
    event shape of `querylog.Event`, client-side `--domain` filtering, and a
    401 on the stream.
  - The admin socket: a fake of `adminsock` with strict JSON decoding (the
    exact request bodies are asserted) and an NDJSON server reached through
    `auto` and `--protocol ndjson`.
  - Completion scripts for four shells, and dynamic completion of kinds,
    object names, zones and contexts.
  - `doctor` against the fake: diagnoses, a rejected token, an unreachable
    explicit server, a local TLS listener, a world-writable dataDir and a
    world-readable state.db.
  - `version` and `config schema` unchanged.
- `internal/apiclient`: URL building, error decoding (eight cases), auth
  headers and CSRF only on writes, token secret field names, pagination
  (including a repeated token), the three apply result shapes, If-Match,
  body kinds, the SSE parser, reconnect with `Last-Event-ID` through a 503,
  and the error paths.
- `internal/cli`: config round trip and modes, Resolve precedence (ten
  cases), the credentials fallback, `SystemKeyring` against go-keyring's
  mock, kind aliases, manifest parsing, the table writer and formats, and
  the admin client over both protocols.

## The client surface a generated client must satisfy

The CLI calls only these, so a generated transport can replace the internals
of `apiclient.Client` while keeping:

```go
func New(server string, opts Options) (*Client, error)
func (c *Client) Server() string
func (c *Client) SetToken(string); func (c *Client) Token() string; func (c *Client) SetCSRF(string)
func (c *Client) URL(path string, q url.Values) string
func (c *Client) Raw(ctx, Request{Method, Path, Query, Body, ContentType, Accept, Header, NoTimeout}) (*http.Response, error)
func (c *Client) Do(ctx, method, path string, q url.Values, in, out any) error   // + Get, Post, Delete
func CheckResponse(*http.Response) error                                           // → *Error{StatusCode, Key, Message, Hint, Missing, RetryAfter, TookMS}
func IsKey(error, string) bool; func IsStatus(error, int) bool
func (c *Client) Pages(ctx, path, q, pageSize, func(ListPage) error) error         // ListPage{Items []json.RawMessage, Continue}
func (c *Client) ListAll(ctx, path, q, pageSize) ([]json.RawMessage, error)
func (c *Client) Subscribe(ctx, path, q, SubscribeOptions, func(Event) error) error // Event{ID, Event, Data}
func (c *Client) Me(ctx) (*Me, error); Status; Login(ctx, user, pw) (*Me, error); Logout; CreateToken(ctx, CreateTokenRequest) (*Token, error)
func (c *Client) ListObjects(ctx, kind, ListOptions) ([]json.RawMessage, error); GetObject; PutObject(…, resourceVersion, obj); DeleteObject
func (c *Client) Apply(ctx, docs []json.RawMessage) ([]ApplyResult, error)
func ObjectPath(kind, ns, name string) string; func Seg(string) string
```

Objects travel as `json.RawMessage` on purpose. The CLI decodes them into
`api/v1alpha1` types only to print them, so a generated client with its own
model types need not match the CRD Go types.

## What the contract left ambiguous

The CLI accepts every reading below. Where the api agent's code (read-only,
its worktree) already settles it, that is noted.

1. **403 hint.** "missing:{resource,verb,scope} in hint" could be a hint
   object, a JSON string, or a sibling of the message. `internal/api` puts
   `missing` beside `message` in the error object. All three forms are
   decoded.
2. **The `POST /auth/tokens` secret field.** It is read from `token`,
   `secret` or `value`. internal/api uses `token`.
3. **`POST /auth/login` response.** The contract does not say whether csrf
   is in the body. The CLI reads it if present and otherwise calls
   `/auth/me`.
4. **`POST /apply` response.** It could be an array, `{items}` or
   `{results}`. internal/api uses `List{items}` and adds `errorKey`, `hint`
   and `resourceVersion` per document; the contract should list those.
5. **`GET /zones` items.** The apex could be `zoneName` or `zone`, and
   `ready` a bool or a condition string. internal/api uses `zoneName` and a
   bool.
6. **Record names in `POST /zones/{zone}/records` and the DELETE path.**
   Relative or absolute? internal/api accepts both; the CLI passes them
   through.
7. **`/cache/flush {?name}`.** Query or body? The CLI sends the query
   parameter. internal/api accepts both.
8. **`POST /dns/query` record format.** Presentation strings or objects?
   internal/api uses strings plus a `flags` object. The CLI renders both.
9. **`GET /info/messages`.** The contract gives no shape. internal/api
   uses `{severity, source, reason, object: "Kind/[ns/]name", message, hint, node}`;
   `describe` matches on `object`.
10. **`/blocklists/_search` hits.** internal/api uses
    `{list: "ns/name", mode, rule}`.
11. **`GET /settings` and `/node/config`.** These are wrapped as
    `{scope, node, settings:{…}}` in internal/api; the PATCH body is the bare
    tree and the answer adds `changed` and `restartRequired`. The CLI sends
    `application/merge-patch+json`.
12. **The `/queries` status filter.** Comma-separated or repeated?
    internal/api uses comma-separated.
13. **The SSE event name and payload.** internal/api documents
    `event: query` with rows shaped like `/queries` items. The CLI prints
    any event that decodes to a row with a domain, and also reads the
    `querylog.Event` shape.
14. **Audit pagination.** The CLI reads both `cursor_next` and `continue`,
    and both the `audit.Event` objects (actor, resource) and flat fields.
15. **Default namespace.** When none is given, the CLI uses
    `-n` > context namespace > `default`, matching `bootstrap.DefaultNamespace`.
    The contract does not say what the server assumes for a namespaced
    document without one, so `apply` fills it in client-side.
16. **The admin socket protocol.** The task specified NDJSON. The auth
    agent's adminsock serves HTTP/1.1 at `/admin/v1/*` with
    `DisallowUnknownFields`, so the CLI sends exactly
    `{username,password,create}` and `{user,description,expiresIn}`.
    `create-token` has no `--name` or `--service-account`: a token without
    `--user` belongs to the break-glass service account.
17. **`stats top -n`.** It collides with the global `-n/--namespace`, so
    the CLI uses `--top N` (the server caps n at 100).

## Surprises

- The branch named in the resume note had no WIP commits; it pointed at the
  initial commit. Nothing could be recovered, so the package started from
  scratch.
- sigs.k8s.io/yaml (YAML 1.1 through go-yaml v2) reads an unquoted
  namespace `n`, `y`, `on` or `off` as a boolean. A manifest with
  `namespace: n` fails to decode with a type error. That is the standard
  Kubernetes behaviour, so it is left alone; the tests use other names.
- The global `-n` and `-o` shorthands rule them out for every subcommand,
  hence `stats top --top` and `logs --limit`.
- In the full `make test` run (48 packages ok), only
  `internal/controller` failed: `TestMeasureRecordSetChanges/resync_of_1000_converged_record_sets`
  saw 948 status writes. That is the timing-sensitive measurement already
  listed in docs/followups.md. It passes when run alone (23 s), and the
  controller package depends on none of the packages this work touched.
- `logs --follow` resumes from the newest paged id. Without that, rows
  logged between the page fetch and the stream connect would be lost or
  duplicated. The client also drops ids at or below the last printed id.

## Follow-ups

Added to docs/followups.md:

- `--context` selecting a kubeconfig, so CRD verbs go straight to the
  apiserver (§3.5). Today a context is always a kudurru API endpoint.
- The remaining §3.5 verbs: `import pihole|adguard|blocky|technitium`,
  `lameduck`, `health --vrrp`, `cluster status|promote|join-token`,
  `integrations …`, and `zones` scoped writes (`PUT/PATCH /zones/{zone}`).
- Browser (loopback + PKCE) and device-code `login` (§8.3, v0.2).
- Swapping `internal/apiclient`'s transport for the generated
  `internal/apiclient/openapi` client once it is stable, behind the surface
  above.
- A conformance test that runs the CLI against the real `internal/api`
  router (apitest) instead of the fake. It needs both packages on one
  branch, so it belongs to the integrator.
