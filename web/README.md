# web

The kudurru dashboard (decision D69 in docs/architecture.md):

- **Toolchain:** Bun (package manager, test runner, `bun build`; Vite under Bun as fallback).
- **UI:** React 19 + TypeScript with [Astryx](https://github.com/facebook/astryx), Meta's MIT-licensed design system (`@astryxdesign/core` plus a theme package, consumed as prebuilt CSS, pinned exactly while it is in Beta).
- **Data:** TanStack Query; TanStack Table + Virtual for the query log; generated TypeScript client from the huma OpenAPI document.
- **Charts:** uPlot for the live 24 h series, ECharts for pies and top lists, until `@astryxdesign/charts` is published as stable.
- **Delivery:** single SPA embedded into the binary with `go:embed`, strict CSP, no CDN, path-prefix aware.

Not started; milestone M3.
