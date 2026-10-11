# M3 work package: ui (the dashboard SPA)

Branch `worktree-wf_287eb68b-e67-3`, on top of 3e52e5a. Everything lives in
`web/`; the shared files touched are `Makefile` (web-* targets, `fmt` and
`clean` cover web), `.github/workflows/ci.yml` (a `web` job), `.gitignore`,
`go.mod` (`ignore ./web/node_modules`), `NOTICE` and this report.

## What was built

### `web` (Go package)

- `web.FS` (`fs.FS`, `//go:embed all:dist`, rooted at the bundle) and
  `web.Built()` (false while `dist/index.html` is the committed placeholder).
- `web.Handler(web.Options{BasePath, FS, ContentSecurityPolicy})`: serves
  the bundle with `Cache-Control: public, max-age=31536000, immutable` for the
  content-hashed `assets/`, `no-cache` plus the SPA fallback (index.html) for
  every extension-less path, 404 for missing files and for anything under
  `api/`, gzip (cached per file) when accepted, ETags, `GET`/`HEAD` only, and
  `Content-Security-Policy` (`web.DefaultCSP`), `X-Content-Type-Options`,
  `Referrer-Policy`, `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy`.
  The `<base href>` of index.html is rewritten (HTML-escaped) to the base path,
  which is how one bundle serves under any prefix.
- Tests: `TestFSServesIndex` (the task's assertion: FS has index.html and
  the handler serves it), `TestIndexAssetsPresent` (fails when index.html
  references assets that are not embedded, i.e. a built index.html committed
  over the placeholder), a routing table, gzip/ETag, base-path escaping and
  placeholder detection. `go test -race ./web` passes with the placeholder and
  with a real bundle.

### `web/` (the SPA, D69 stack)

Bun 1.4.3 + Vite 8 (rolldown) + React 19.3 + TypeScript 6 strict
(`noUncheckedIndexedAccess`, `verbatimModuleSyntax`) + Astryx 0.6.8
(`@astryxdesign/core`, themes neutral, stone and the fun y2k; prebuilt CSS,
built themes only, pinned exactly) + TanStack Query 5 / Table 8 / Virtual 3 +
uPlot 1.6.32 + ECharts 6.1 (tree-shaken, dynamically imported) + lucide icons.
No CDN, no inline script, relative asset URLs.

Pages (all in the side navigation, each a lazily loaded chunk; `src/routes.tsx`):

| Page | Contents |
|---|---|
| Login | local form (hidden when `localLogin` is disabled), one button per `/auth/status` provider (BFF start URL with `next=`), default-credentials nag, `?error=` from a failed OIDC callback |
| Dashboard | four stat tiles linking into the pre-filtered log (Total / Blocked / % blocked / Domains on lists, active/total clients in the footer); 24 h stacked bars forwarded/cached/authoritative/other/blocked (uPlot) with click → log ±5 min and a keyboard-reachable data table offering the same drill-down; per-client stacked chart with "Other clients"; query-type and upstream donuts (ECharts) with HTML legends that drill down; top permitted domains, top blocked domains, top clients |
| Sidebar (every page) | status dot, queries/s, blocking switch with countdown, one dot per node |
| Query log | filters in the URL (domain with wildcards and suggestions, client, status class, type, upstream, list, since/until), keyset pagination over `cursor` (Load more plus auto-fetch near the end), virtualised table (TanStack Table column model, TanStack Virtual, one `<tbody>` per item so expanded rows measure as a unit), live tail over SSE with Last-Event-ID resume, per-row Allow/Deny with toasts, expandable "why" row (status/rcode, matched list and rule, CNAME chain, upstream or zone, cache/stale/prefetch, DNSSEC, EDE text, reply type/time/answer, node, client and group, protocol/ECS, trace id); records_total/filtered and earliest memory/disk timestamps |
| Long-term | presets 7 d/30 d/90 d/1 y or a custom range; stacked series at `step=auto`, totals summed from it, six top lists for the range with `approximate` badges; drill-downs carry since/until |
| Lists / Groups / Policies | one generic page (`ObjectsPage`): search, enabled switch and comment on every row, managed-by badge with the owner, multi-select + `:batchDelete` with a confirmation, create/edit dialog generated from field descriptors (`lib/objectSchemas.ts`, mirrors api/v1alpha1), read-only with the reason when another manager owns the object. Lists add health (OK / unchanged / unavailable, cached / unavailable / invalid), entry counts, last update, Refresh, "Add domains" (allow/deny × exact/regex into ui-allow/ui-deny, shows `processed{success,errors}`) and "Which lists contain a domain?". Groups and the generated forms have "Add many" (paste IPs, CIDRs or MACs) |
| Clients | inventory with groups, last seen, 24 h counts linking into the log, multi-select "Add to group" (PUT of the ClientGroup with name and comment) |
| Zones | type, serial, records, Ready, transfer/NOTIFY status, managed-by; "New zone" (primary/forward) |
| Zone | records flattened across record sets; typed per-RR editor (A, AAAA, CNAME, MX, SRV, TXT, CAA, HTTPS, SVCB, NS, PTR, TLSA, SSHFP, NAPTR, raw) with per-field validation, several targets per record, comment, disabled, record set; BIND import (file or paste, per-line skips shown), export preview and download, Send NOTIFY |
| Settings | Basic (the common keys plus appearance) / Expert (every key of `/settings/_schema`, grouped and searchable); each row bound by `data-configkey`, typed editor, JSON merge patch to `/settings` or `/node/config`; host-only, env-locked and not-permitted keys read-only with the reason (env var name included) |
| Nodes | version, roles, ready, q/s, leader, VIP master, snapshot age, store driver, per-node health checks on demand, cluster mode/leader/read-only, edge backends and VIP owners |
| Tools | built-in dig (`POST /dns/query`, answer/authority/additional, list/rule/upstream/zone/CNAME chain/EDE), cache flush (one name or all), object import through `POST /apply` with per-document results, backup/export links (objects as JSON, zones as BIND, audit), doctor report assembled from version, nodes, health, query-log status and diagnosis |
| Diagnosis | `/info/messages` sorted by severity with links to the object; badge in the top bar and the navigation |
| Access & audit | the principal, roles and permission matrix; own API tokens (secret shown once, revoke); read-only IAM (users, service accounts, role bindings, identity providers); audit log with filters and export |

Cross-cutting:

- **Node scope** (§10 principle 9): the top-bar selector ("All nodes (N)" or
  one node, persisted per browser) appears on every stats/log page and every
  stats/log request sends `scope=cluster` or `scope=node&node=<name>`; the
  `scope` block of responses is shown under each page title.
- **Colour code** (principle 4): `q-permitted / q-cached / q-blocked /
  q-error` classes and `--q-*` custom properties mapped onto Astryx tokens;
  charts resolve the CSS colours at draw time and redraw on theme, mode,
  contrast or OS scheme changes. Auto/light/dark modes, a high-contrast
  overlay, neutral (initial chunk), stone and y2k themes (separate chunks).
- **Path prefix**: `<base href>` rewritten by the handler; the router,
  API base, downloads and OIDC links derive from it; API URLs are never
  treated as client routes.
- **Accessibility**: AppShell skip link and landmarks, labelled controls,
  combobox pattern for suggestions, keyboard row navigation in the log
  (Enter/Space expand, arrows move), data tables behind every chart, polite
  live regions for counts and countdowns, reduced-motion respected.
- **Permissions**: `GET /auth/me` permissions drive disabled buttons with a
  tooltip naming the missing `resource.verb` (verbs of permissions.yaml:
  `blocking.toggle`, `cache.flush`, `node.config`, ...); 403s show the
  missing permission from the error hint; any 401 outside `/auth/*` returns
  to the login page.

### API client (`web/src/api`)

`types.ts` holds every wire type (objects field for field from api/v1alpha1,
stats and log from internal/querylog/sqlitelog); `client.ts` is the fetch
wrapper (same-origin cookies, `X-Kudurru-CSRF` on mutating requests,
`ApiError{status,key,message,hint,retryAfter}` with `missing` and `owner`
accessors); `endpoints.ts` has one function per route; `context.tsx` the
session, permission check and node scope. When the api package commits
`api/openapi.json`, `bun run gen:api` writes `src/api/generated.ts` and the
types in `types.ts` become aliases of `components['schemas'][...]`; nothing
else names a schema, so the swap is one file.

### Mocks (`web/src/mocks`)

MSW 3 handlers for the whole contract (error envelope, 401/403 with
`missing`, 409 managed_by, 412 on If-Match, keyset cursors, SSE tail,
`processed{success,errors}`, merge-patch settings with `host_only`) over
deterministic fixtures (fixed seed and clock): 3 zones with 50 records,
6 lists including the starter list, 4 groups, 3 policies, 24 h of 5-minute
series, 500 query rows, 2 nodes, diagnosis messages, audit entries, the real
settings schema. The permission matrix follows permissions.yaml (admin = all
verbs, viewer = the viewer role plus querylog.read). `bun run dev:mock`
serves them in the browser; `?mockUser=admin` skips the form.

## Pages, wizards and dialogs

The information architecture of the dashboard (UX round 2, 2026-10-11).
Every new screen follows it; `web/src/components/Wizard.tsx`,
`web/src/components/FormDialog.tsx` and `web/src/components/fields/` are the
building blocks.

- **Dedicated page** for every object a user configures or inspects. It has
  a URL, a current-state header (enabled, health or ready, managedBy,
  counts), sections, and actions. List pages are tables whose rows link to
  the detail page. Routes: `/policies/:ns/:name`, `/lists/:ns/:name`,
  `/groups/:ns/:name`, `/clients/:key`, `/zones/:zone`, `/upstreams/:group`.
- **Wizard on its own route** for creating anything with more than about
  four fields or several concepts: `/policies/new`, `/lists/new`,
  `/groups/new`, `/zones/new`, `/upstreams/new`. Editing an existing object
  happens on its detail page, in inline sections with their own Save, or at
  `/…/:name/edit` reusing the wizard steps with every step reachable. Wizard
  step state may live in `?step=`. The word `new` is therefore not a usable
  object name on those routes (the zone and upstream wizards refuse it).
- **Dialog (FormDialog)** only for short, single-purpose actions:
  confirmations, adding one source or entry, adding domains to allow/deny,
  cache flush, the blocking timer, password change, token creation,
  appearance, the record editor (a small typed form), zone file import and
  the block page preview. Nothing that scrolls more than one screen.
- **Typed controls.** Never a free-text box for:
  - a value from a closed set: `SelectField`/`EnumSelect` (an Astryx
    Selector, with the chosen option's help under it) or, for a wizard's
    first choice, `ChoiceCards` (a RadioList);
  - a duration: `DurationField` (presets from the settings schema's
    `presets`, "Custom…" for anything else); TTLs and SOA timers in seconds:
    `SecondsField`;
  - a reference to another object: `ObjectRefPicker` or
    `UpstreamGroupSelect`;
  - a boolean: `Switch` / `SwitchField`, or `CheckboxField` in a form
    that is saved as a whole;
  - a number with bounds: `NumberField` (Astryx NumberInput, the range in
    its help and an inline range error); a size: `BytesField` (number and
    unit);
  - a date and time: Astryx `DateTimeInput`; a time: `TimeInput`;
  - a list: `StringListField` (rows with add/remove, "Paste many", a
    per-row check); lists of objects and maps: the row editors of
    `ShapeField`.

  Free text stays for names, domains, URLs, addresses and CIDRs, each with
  format validation and an example (`fields/formats.ts`: ip, ipv4, ipv6,
  cidr, hostport, url, domain, upstream, email), in `TextField` /
  `TextAreaField`.
- **Astryx controls only, help under the control.** Every control is an
  Astryx component through the wrappers of `components/fields/`; no raw
  `<select>`, `<input>` or `<textarea>` (an ESLint rule rejects them, and
  `description` on the Astryx inputs, outside `components/fields/`).
  A field reads label, control, helper text, then the status message:
  Astryx's `description` renders between label and control, so the
  wrappers take `help` instead, render it as supporting `Text` after the
  control and link it with `aria-describedby` (`fields/FieldHelp.tsx`;
  Switch and CheckboxInput keep their own description, which sits beside
  the control). Labels stay on one line so a row of fields lines up on its
  controls.
- **Settings are rendered from the schema** (contract C1): GET
  `/settings/_schema` gives each key its control (enum, min/max, presets,
  format, ref, items/keys/values/fields shapes), its section (`group`, in
  the order of `config.Groups`) and its label (the path is shown small
  underneath). Each row is 60% explanation (label, path, help, badges)
  and 40% value (the control, then the default or Save, Reset and Edit as
  JSON), one column under 900px; every row has help, the server's or, when
  it is missing or a few words, a sentence about the value's type from the
  shape. Raw JSON is only the "Edit as JSON" escape hatch of list and
  map keys. A key that differs from its default shows "changed" and offers
  "Reset to default" (`null` in the merge patch). A 400 names the failing
  parts of a value inline (`error.fields`). Keys without metadata (servers
  before C1) fall back to a control from the bare type.
- **Read-only objects** (managedBy file, bootstrap or Kubernetes) show the
  same page with the controls disabled and the reason (`readOnlyReason`).

## Wiring for serve (integrator)

`cmd/kudurru/serve.go` has no API listener yet; the api package adds the
`/api/v1` handler on `cfg.Listeners.API.Address`. Mount the dashboard on the
same mux, after the API so `/api/` never reaches the SPA (the handler also
404s `api/*` on its own):

```go
import "github.com/robertherbaugh/kudurru/web"

mux := http.NewServeMux()
mux.Handle("/api/", apiHandler)                       // api package (huma), incl. /api/v1/docs
mux.Handle("/", web.Handler(web.Options{}))           // dashboard at the root
if !web.Built() {
	logger.Warn("dashboard bundle not built into this binary; serving a placeholder page (make web-build)")
}
// in the runner list, next to "ops" and "metrics":
// {"api", false, func(ctx context.Context) error { return serveHTTP(ctx, cfg.Listeners.API.Address, mux, logger, "api") }},
```

Behind a reverse proxy that keeps a prefix (e.g. `/kudurru/`), mount both
under it and strip it for the SPA:

```go
prefix := "/kudurru" // no config key yet, see follow-ups
mux.Handle(prefix+"/api/", http.StripPrefix(prefix, apiHandler))
mux.Handle(prefix+"/", http.StripPrefix(prefix, web.Handler(web.Options{BasePath: prefix})))
```

The handler sets its own CSP (`web.DefaultCSP`: `script-src 'self'`,
`connect-src 'self'`, `style-src 'self' 'unsafe-inline'` because Astryx
inserts its popover position rules and React/uPlot set geometry through
style attributes; no script source is relaxed). If the API middleware also
sets a CSP, it must not apply to the dashboard routes or must be identical.
`/api/v1/docs` (Scalar) needs its own CSP from the api package.

## API routes and shapes the contract did not fully specify

The client and mocks assume the following; the api package should match
them or tell the integrator so `types.ts` can follow:

1. `GET /settings/_schema` → `{items:[{path,type,scope,default,help,hostOnly?,envLocked?,restartRequired?,writeOnly?}]}` in lowerCamel (`internal/config.Key` has no JSON tags). `GET /settings` and `GET /node/config` return nested objects keyed by the dotted path segments; PATCH bodies are `application/merge-patch+json`; a host-only key is rejected with 400 key `host_only`.
2. `GET /auth/me` → `permissions` uses the verbs of permissions.yaml (`read`, `write`, `delete`, `toggle`, `flush`, `refresh`, `purge`, `config`, `transfer`, `own`, ...), `*` accepted as a wildcard resource or verb. `POST /auth/login` returns the same body as `/auth/me` (the UI needs the CSRF token without a second call). `GET /auth/status` `localLogin` may be a boolean or `enabled|admins-only|disabled`. `GET /auth/oidc/{idp}/start?next=<route>` returns to `next`; a failed callback redirects to the SPA with `?error=<message>`.
3. `GET /queries/stream`: events named `query`, `id` = the row id, `data` = one QueryRow JSON (same shape as `/queries`), same filters and scope parameters as `/queries`, cookie auth (`withCredentials`). Resume is `Last-Event-ID` (EventSource reconnects) and also `?lastEventId=` on a fresh connection, because EventSource cannot set headers.
4. `GET /queries/suggestions?field=domain|client|upstream|list|zone&prefix=` → `{field, values:[...]}`; `POST /queries/{id}/actions` → `{processed:{success:[{item}],errors:[{item,error}]}}`.
5. `GET /stats/top` and `GET /stats/timeseries` accept `from`/`to` (RFC 3339) as well as `window` (long-term page); `GET /stats/upstreams?window=` (in the §10 surface, not the task contract) → `UpstreamsResult` of sqlitelog for the upstream donut.
6. `GET /zones` rows carry an optional `transfer` object `{zoneTransfer, notify, lastNotify?{at,targets,ok,error?}, lastTransfer?{at,peer,kind:AXFR|IXFR,ok}}` for the transfer/NOTIFY status; `GET /zones/{zone}` returns the same row shape.
7. `GET /zones/{zone}/records` rows carry `managedBy` (the record set's manager) and `namespace`; `DELETE /zones/{zone}/records/{name}/{type}` accepts `?recordSet=` to pick one of several record sets; `POST /zones/{zone}/notify` → `{targets:[...]}`; `POST /zones/{zone}/import` → `{imported, recordSet?, skipped?:[{line,error}]}`.
8. `POST /{kind}:batchDelete` body `{names, namespace?}` (the UI sends one call per namespace); returns `processed`.
9. `GET /blocklists/_search?domain=` → `{items:[{list,namespace,rule,mode}]}`; `POST /blocklists/{ns}/{name}/refresh` → 202/204.
10. `GET /clients?window=24h` → `{items:[{ip,name?,mac?,groups:[...],lastSeen?,queries24h,blocked24h}]}` (§10 lists the route, not the shape).
11. `GET /dns/blocking` → `{blocking, timer?, until?}` (both when disabled with a deadline); `POST /dns/blocking` with `{blocking:false, timer}` or `{blocking:false, until}` or `{blocking:true}` returns the new state. `POST /cache/flush` → `{flushed}`.
12. `POST /dns/query` → `{rcode,status,status_class,answer,authority,additional:[{name,type,ttl,class?,data}],upstream?,list?,rule?,zone?,cname_chain?,ede?{code,text?},took_ms,node?}`.
13. `GET /nodes` rows `{name,version,roles,ready,leader,vipMaster?,qps,snapshotAge?,address?,storeDriver?,startedAt?}`; `GET /nodes/{name}/health` → `{name,checks:[{name,ok,message?}]}`; `GET /cluster` → `{mode,leader,nodes,epoch?,readOnly?}`; `GET /edge` → `{backends:[{name,address,up,qps?}],vip?:[{address,owner?}]}`; `GET /version` → `{version,commit,date,go?}`.
14. `GET /info/messages` items `{id,severity:error|warning|info,reason,message,object?{kind,namespace?,name},hint?,node?,since?}`.
15. `POST /apply` accepts `application/yaml` or `application/json` bodies and returns `{items:[ApplyResult]}`; `GET /objects/{kind}` is linked as a JSON download.
16. `POST /auth/tokens` body `{name, expiresIn?}` → token row plus `token` (the `kdr_` secret); rows `{id,name,createdAt,expiresAt?,lastUsedAt?,lastUsedIP?}`. `GET /audit` → `{items,cursor_next?}` rows `{id,ts,actor,action,kind,namespace?,name,outcome,authzPath?}`; `GET /audit/export` is a download. IAM rows: users `{name,displayName?,local,roles?,disabled?}`, service accounts `{name,namespace?,tokens?}`, role bindings `{name,role,subjects:[{kind,name}]}`, identity providers `{name,issuer,clientID?}`.
17. Comment and enabled on every object (principle 6): v1alpha1 has no such fields, so the UI writes the annotations `kudurru.io/comment` and `kudurru.io/enabled: "false"`. The server and controllers do not honour `kudurru.io/enabled` yet (follow-up). Ownership comes from `kudurru.io/managed-by` (`api`, `bootstrap`, `file:<path>`, `kubernetes`/`kubectl`, `integration:<name>`, `system`).

## Decisions and deviations

- **Own 150-line router instead of a router library**: a dozen flat routes,
  search params as the single source of filter state, prefix aware and plugged
  into Astryx's `LinkProvider`; saves ~60 KB.
- **Native `<select>` for enums** (status, type, theme, record type,
  settings enums) next to Astryx inputs: fully keyboard/AT accessible and
  trivial to test; Astryx `Selector` is a popover listbox.
- **Charts keep an accessible twin**: every uPlot chart has a data table
  with drill-down links and every donut an HTML legend; canvas is skipped
  when unavailable (tests) without losing the data.
- **Doctor in the browser** composes existing endpoints; a `GET /doctor`
  would let it match `kudurru doctor` (follow-up).
- **Licence file**: `scripts/licenses.ts` writes `dist/third-party-licenses.txt`
  (all 28 production packages, licence texts and the Apache ECharts NOTICE;
  fails on copyleft licences); it is embedded and linked from Tools.
  Astryx 0.6.8 ships no LICENSE file in its npm tarball, so the script
  writes the MIT text with Meta's copyright for it.
- React is 19.3 (architecture.md names 19.2; 19.3 was current when the
  lockfile was created).

## Measurements

| What | Value |
|---|---|
| Initial load (index.html + referenced JS/CSS, gzip -9) | 235.1 KiB (budget 600 KiB; enforced by `scripts/budget.ts` in `bun run build`) |
| Largest initial pieces (gzip) | react 66.1 KiB, app entry 44.5 KiB, shared Astryx chunk 42.6 KiB, CSS 37.0 KiB, theme 16.6 KiB, tanstack-query 10.2 KiB |
| Whole bundle | 1,780 KiB raw, 530 KiB gzip (ECharts chunk 182 KiB gzip, loaded with the first donut) |
| Dashboard / query-log chunks (gzip) | 27.4 KiB / 24.7 KiB |
| Binary size impact (embed is uncompressed) | +1,822,720 bytes (`go test -c` of ./web with the bundle vs the placeholder); kudurru today 47,808,672 bytes stripped, so about 49.6 MB of the 51,234,000-byte D75 budget once wired |
| Tests | vitest: 9 files, 183 tests, ~10 s on the shared machine; Go: `go test -race ./web` 7 tests; full module `CGO_ENABLED=1 go test -race ./...` green (48 packages) |
| Lint | `eslint --max-warnings 0` (typescript-eslint type-checked + react-hooks) clean, `tsc --noEmit` clean, `golangci-lint run ./web/...` 0 issues, gofmt/vet clean |
| CSP check | production bundle served with `web.DefaultCSP` in headless Chrome: no CSP violations logged |
| Source | ~10,100 lines of TypeScript outside tests (incl. ~1,800 lines of mocks/fixtures) |

## Surprises

- The previous attempt's WIP commits were not on the branch (it had been
  reset to the initial commit); they were found as dangling commits and the
  branch fast-forwarded to 5740c25 before continuing.
- MSW 3 renamed `onUnhandledRequest` to `onUnhandledFrame`, and `sse()`
  throws at construction where `EventSource` is missing (jsdom), so the SSE
  handler is registered only in browsers.
- jsdom does no layout: TanStack Virtual renders zero rows unless the test
  setup gives the scroller an `offsetHeight`.
- Astryx buttons disable with `aria-disabled` (not `disabled`) and render
  field errors twice (visual and live region); tests query accordingly.
- Rolldown names a shared chunk after its first module (`format-*.js` holds
  shared Astryx code); vendor groups pin `react` and `tanstack-query`.
- `make web-build` overwrites the tracked placeholder `web/dist/index.html`
  by design (the task wants it committed); `TestIndexAssetsPresent` catches
  an accidental commit of the built file.
- Headless Chrome's `--screenshot` returns before the MSW service worker
  starts; screenshots were taken with a small CDP driver instead.

## Follow-ups

Added to docs/followups.md under "Dashboard (web/)":

- api package: match the shapes above (especially the settings schema JSON
  names, the SSE event name and `?lastEventId=`, `transfer` on zone rows,
  `managedBy` on record rows, `/clients`); then generate `src/api/generated.ts`
  from `api/openapi.json` and alias the types.
- Honour `kudurru.io/enabled: "false"` (and show `kudurru.io/comment`) in the
  controller, policy compiler and list resolver, or add `comment`/`enabled`
  to the v1alpha1 specs (decision needed).
- A config key for the URL prefix (e.g. `listeners.api.pathPrefix`) feeding
  `web.Options.BasePath` and the API mount.
- `GET /doctor` so the Tools page shows the same checks as `kudurru doctor`.
- Embed pre-compressed assets (gzip at build time, decompress only for the
  rare client without gzip) to cut the binary impact from 1.8 MB to ~0.55 MB.
- Playwright end-to-end run against a real `kudurru serve` (skipped here; the
  CDP screenshots covered layout, dark mode and mobile by hand).
- Replace the `window.confirm` on record delete with an Astryx AlertDialog;
  `GET /queries/export` link in the query log once the route exists; cluster
  join wizard, Integrations page and IAM writes are v0.2 (§10 principle 13).
- Adopt `@astryxdesign/charts` once stable and drop uPlot/ECharts.
