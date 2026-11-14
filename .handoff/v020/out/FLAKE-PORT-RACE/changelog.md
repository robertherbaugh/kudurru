# FLAKE-PORT-RACE: bullets for `CHANGELOG.md` `[0.2.0]`

This package changes test helpers only. No product code, API, CLI, setting or
listener behavior changes, so nothing here is user-facing. The bullet below is
for contributors; drop it if the changelog stays product-only.

### Fixed

- Tests: mounted, end-to-end and DNS listener tests no longer fail intermittently with
  `bind: address already in use`. Test listener ports used to be picked by
  binding port 0 and closing the socket again, so another test or process
  could take the number before the server bound it. They now come from
  `internal/testutil/testport`, which allocates outside the kernel's ephemeral
  port range and holds each port with a cross-process file lock until the test
  ends, also for tests that point `TMPDIR` at a private directory.
