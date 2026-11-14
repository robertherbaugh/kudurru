# FLAKE-OIDC-WRITER: bullets for `CHANGELOG.md` `[0.2.0]`

This package changes test fixtures only. No product code, API, CLI, setting or
document changes, so nothing here is user-facing. The one bullet below is for
contributors who run or extend the `cmd/kudurru` tests; drop it if the changelog
stays product-only.

### Fixed

- Tests: the mounted serving fixtures in `cmd/kudurru` no longer report a writer
  failure that never happened when a test ends (`browser writer ended context
  canceled`, seen intermittently in hosted CI). The fixtures ran the always-on
  runtime and the elected writer under two children of `t.Context()`, which Go
  cancels in no fixed order before any cleanup runs, so the runtime could stop a
  moment before the writer's own context ended. The always-on runtime of a
  fixture now stops only in its cleanup, after the writer, which is the order the
  server itself uses at shutdown. A fixture that runs always-on work for an
  elected writer takes its context from `servingFixtureAlwaysOn`.
