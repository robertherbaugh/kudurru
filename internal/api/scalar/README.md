# Scalar API reference bundle (MIT)

`standalone.js.gz` is `dist/browser/standalone.js` of the npm package
`@scalar/api-reference` **1.44.20** (the version huma v2.39.1 pins), gzipped
with `gzip -9 -n`. kudurru serves it at `/api/v1/docs/scalar-1.44.20.js` so
the API reference at `/api/v1/docs` loads nothing from a CDN and works under
`listeners.api.pathPrefix` and the strict CSP (dev-docs/design-record.md §8.8).

- Licence: MIT, `LICENSE` in this directory (Copyright (c) 2023-present
  Scalar), from https://github.com/scalar/scalar/blob/main/LICENSE; listed in
  the repository NOTICE.
- Source: https://registry.npmjs.org/@scalar/api-reference/-/api-reference-1.44.20.tgz
  (sha256 `4741fe017d13456fa014556f5f8010a1024b926867743f68c78b88223bda2535`).
- `standalone.js` sha256 `f349c815d31be09d11e386726da989e3af50c2f1885910764b51f5b0fae9e28e`,
  SRI `sha384-tMz7GAo6dMy55x9tLFtH+sHtogji6Scmb+feBR31TAHmvSPRUTboK9H3M5NFaP4R`
  (identical to the integrity huma renders for the unpkg URL;
  `TestScalarBundleIntegrity` checks it).

To update: download the tarball of the version the huma release in go.mod
pins (`grep -o 'api-reference@[0-9.]*' $(go env GOMODCACHE)/github.com/danielgtaylor/huma/v2@*/api.go`),
replace `standalone.js.gz`, and change `scalarVersion` and `scalarIntegrity`
in `internal/api/docs.go`.
