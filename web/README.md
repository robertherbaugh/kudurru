# web

The kudurru dashboard (docs/architecture.md §10, decision D69): a single-page
app embedded into the binary with `go:embed` and served by `web.Handler`.

- **Toolchain:** Bun (package manager, script runner) with Vite 8 (rolldown)
  for the dev server and production build, Vitest + Testing Library for tests.
- **UI:** React 19 + TypeScript (strict) with [Astryx](https://github.com/facebook/astryx)
  (`@astryxdesign/core` and the neutral/stone/y2k themes, MIT, consumed as
  prebuilt CSS and JS, pinned exactly while Astryx is in Beta).
- **Data:** TanStack Query; TanStack Table + Virtual for the query log;
  `EventSource` for the live tail.
- **Charts:** uPlot for the time series, ECharts (tree-shaken, loaded on
  demand) for the donuts, until `@astryxdesign/charts` is stable.
- **Delivery:** no CDN, no inline scripts (strict CSP from `web.DefaultCSP`),
  relative asset URLs and a `<base href>` rewritten by the Go handler, so one
  bundle serves under any path prefix.

## Commands

```sh
make web-install   # bun install --frozen-lockfile
make web-dev       # Vite dev server with the MSW mock API (no kudurru needed)
make web-test      # vitest run
make web-lint      # eslint (zero warnings) + tsc --noEmit
make web-build     # production bundle into web/dist + licence file + size budget
```

Inside `web/`: `bun run dev` proxies `/api` to `KUDURRU_API`
(default `http://127.0.0.1:8080`) for work against a real server;
`bun run dev:mock` answers `/api/v1` in the browser from `src/mocks`.
Open `http://localhost:5173/?mockUser=admin` to skip the login form in
mock mode (users `admin`/`admin` and `viewer`/`viewer`).

`make build` never needs Bun. A placeholder `web/dist/index.html` is
committed so `go build ./...` works on a plain checkout; `make web-build`
overwrites it and writes the hashed bundle into `web/dist/assets/` (ignored
by git). Do not commit the built `index.html`: `TestIndexAssetsPresent` in
this package fails when index.html references assets that are not embedded.

## Layout

| Path | What |
|---|---|
| `embed.go`, `handler.go` | `web.FS` (the bundle) and `web.Handler` (SPA fallback, caching, gzip, CSP, `<base href>` rewrite) |
| `src/api/` | the typed client: `types.ts` (wire types), `client.ts` (fetch, errors, CSRF), `endpoints.ts` (one function per route), `context.tsx` (session, permissions, node scope), `hooks.ts` |
| `src/pages/` | one lazily loaded module per route (`src/routes.tsx` is the table) |
| `src/components/` | shell, stat tiles, query-log table, record editor, generated object forms, settings rows, blocking control |
| `src/charts/` | uPlot stacked bars and the ECharts donut, both reading colours from CSS |
| `src/lib/` | router, base path, RR field model, settings metadata, formatting, ownership |
| `src/mocks/` | MSW handlers and deterministic fixtures (3 zones, 50 records, 6 lists, 4 groups, 3 policies, 24 h of series, 500 query rows, 2 nodes) |
| `scripts/` | `budget.ts` (initial-load gzip budget, default 600 KiB) and `licenses.ts` (dist/third-party-licenses.txt) |

## Generated API types

`src/api/types.ts` is hand-written against the API contract today. When
`api/openapi.json` lands, `bun run gen:api` writes `src/api/generated.ts`;
then each type in `types.ts` becomes an alias such as
`export type DNSZone = components['schemas']['DNSZone']`. Nothing else imports
schema names directly, so the swap touches one file.

## Licensing

Pi-hole is EUPL-1.2: none of its markup, styles or JavaScript is used. The
information architecture is re-derived from the twelve principles in §10.
Third-party notices are in the repository NOTICE and, for binary
distributions, in the generated `third-party-licenses.txt` served next to
the bundle.
