### RPZ-2 RPZ feed diagnostics — November 10, 2026 (D391)

Per-feed RPZ state used to stay inside `rpz.Manager`: only a rule count and a
ready flag reached policy reads, so an operator could not see why a feed
answered SERVFAIL. Each node now reports its own feeds, on SQLite and on
Kubernetes. `GET /api/v1/blocklists/_rpz?namespace=` and
`GET /api/v1/blocklists/{namespace}/{name}/rpz` (`blocklists.read`) return one
item per enabled RPZ source with the answering `node`. Each item has a derived
`state`: `pending` (no copy and no failure yet), `serving`, `expired` (only once
the copy's absolute expiry has passed at the manager's observation time) or
`unavailable` (configuration, credential or transfer failure, or an unexpired
copy the node cannot use: discarded because the configuration or credential
changed during acceptance, or confirmed after the node's clock). Every state but
`serving` answers SERVFAIL for clients whose policy selects the list, `pending`
included. It also carries
a `reason` and `message`, the serial, primary, last confirmation, absolute
expiry, next attempt, the last refresh error (bounded to 1 KiB) and any
configuration error. Then come the compiler totals, the compiled rules by
trigger (QNAME, CLIENT-IP, RESPONSE-IP) and by action (NXDOMAIN, NODATA,
PASSTHRU, DROP, TCP-only, local data), and every ignored reason with its
complete count and up to five example owners. The compiler now counts ignored
owners per reason even past its 32-sample diagnostic cap. The serial and counts
stay visible after the copy expires. Without a manager the routes answer 501.
If the node's configuration snapshot has no such list they answer 404, and
before any snapshot exists they answer 503.

`kudurru lists rpz [name] [-A] [-o wide|json]` renders the same items (`-A`
with a name is refused).
`/info/messages`, and so `kudurru doctor`, adds a diagnosis with source `rpz`
for each feed that is not serving cleanly: `TransferPending` info (deliberately not an error: every new feed starts
there),
`RPZTransfer` warning while a copy still serves or error without one, and
`RPZExpired` and `ConfigurationUnavailable` errors. It also adds an
`RPZRulesIgnored` info for each feed with ignored owners. `/metrics` exports
scrape-time gauges labeled `list` and `source`: `kudurru_rpz_feed_available`,
`_expires_timestamp_seconds`, `_confirmed_timestamp_seconds`, `_serial`,
`_rules` and `_ignored_rules` (with `reason`). All of these reads use the
manager at request time and never touch the DNS path. A new operator guide,
`docs/deploy/rpz.md`, covers the supported triggers and actions, what is ignored
and counted, precedence, expiry and how to check a feed. The architecture page
(`docs/architecture/block-lists.md`) now shows this flow.

Evidence under `.context/v020-final-20261110/evidence/RPZ-DIAG/` (`-race`,
KUBEBUILDER_ASSETS 1.37.0; `./cmd/kudurru` and the full `./internal/api` run
through the heavy-test lock):

- `unit01.log`, `unit02.log`: `./internal/rpz` PASS (2.4 s, 2.8 s), including
  `TestCompileRuleAndIgnoredAccounting`, `TestIgnoredAccountingBeyondDiagnosticCap`
  and `TestStatusesReportAcceptedCopyAndListLookup`.
- `api01.log` (retained failure): the first `TestRPZFeedRoutesOverRealTransfer`
  draft failed on its counts assertion (the log prints the compiler counts). It
  was fixed before api02 by the interrupted earlier implementer; whether the
  test or the code changed was not recorded.
  `api02.log`/`api03.log`: `TestRPZFeedRoutesOverRealTransfer` PASS (1.5 s) and
  `TestRPZFeedRoutesWithoutManager` PASS. The first test runs a real TCP AXFR
  primary, a real store and zone cache, and the real manager. It checks pending,
  serving with every field, unavailable from a missing TSIG key
  (`ConfigurationUnavailable`), unavailable from an unreachable primary
  (`RPZTransfer`), serving with a failed refresh, expired with the last serial
  and counts, the 404 for an unknown list, and the `/info/messages` entries.
- `pkg01.log` (retained failure): a full `./internal/api` run before
  regeneration, where only `TestOpenAPIDrift` failed, as expected.
  `api04-full.log` (retained): the full `-race` run hit the default 10-minute
  `go test` timeout while it shared the CPUs with other lanes; no test failed.
  `api05-full.log`: full `./internal/api` `-race` PASS in 299.2 s.
- `cli01-update.log`, `cli02.log` (retained): the golden and CLI reference were
  stale before the goldens and `make docs-gen` were updated.
- `mount01-sqlite.log`: `TestRunServeSQLiteRPZFeedDiagnostics` PASS (13.4 s).
  `mount02-kube.log` is empty: that run was cut off by the machine restart.
- `mount03.log`: `TestConvenienceGolden` PASS (0.1 s; goldens `lists-rpz`,
  `lists-rpz-all-wide`, `lists-rpz-name`), `TestRunServeSQLiteRPZFeedDiagnostics`
  PASS (10.3 s) and `TestRunServeKubeRPZFeeds` PASS (57.9 s). The SQLite test
  mounts `runServe` with a real unsigned TCP primary. Through the real CLI and
  `/metrics` it reads the serving feed (serial, expiry = confirmation + SOA
  expire, counts, one NSDNAME owner ignored as `resolver-ns-provenance`). It then
  stops the primary and reads the expired feed with its last error while DNS
  answers SERVFAIL. The Kubernetes test (two envtest replicas, signed feed)
  checks through the CLI that node A reports the missing feed as `unavailable`
  and the served feed with serial 2 and its trigger/action counts. It checks
  the gauges on node A, and that node B, restarted offline, reports its copy
  as `expired` with serial 3 and the last error.
- `gen01.log`: `make openapi`, `make client-go`, `bun run gen:api`,
  `make docs-gen`. `drift01.log`/`drift02.log`: `TestOpenAPIDrift`,
  `TestCLIReferenceDrift`, `TestReferenceDrift` and `TestAggregatedRolesDrift`
  PASS after the generated commit. `lint01.log`: golangci-lint v2.15.0 on
  `./internal/rpz/... ./internal/api/... ./cmd/kudurru/...`, 0 issues.
  `bench01.log`: bench guards OK.
- Review fixes (after an independent review, verdict accept-with-fixes):
  `fix-api01-prefix.log` (retained failure): the extended
  `TestRPZFeedRoutesOverRealTransfer` failed before the fix because the pending
  diagnosis did not mention SERVFAIL. `fix-api02.log`: `TestRPZFeed*` `-race`
  PASS (2.5 s) after the fix; the test now requires every feed diagnosis on both
  `/info/messages` paths, the plain one and the captured read SQLite mounts
  (`InfoMessageReads`), checks a clock stepped back before the last
  confirmation (`unavailable`/`RPZTransfer`, never `expired` with a future
  `expiresAt`), 401 without a token on both routes, and the new
  `TestRPZFeedRouteBeforeConfiguration` checks the 503. Retained mutant runs
  that show the tests bite: `fix-api03-mutant-expired.log` (old `expired`
  derivation restored: FAIL with state `expired` and `expiresAt` in the future)
  and `fix-api04-mutant-captured.log` (RPZ diagnoses removed from
  `infoOperationalMessages`: FAIL). `fix-api05.log`: `TestRPZFeed`,
  `TestInfoMessages`, `TestCapturedInfoMessages`, `TestPermission`, `TestRoute`
  `-race -count=2` PASS; `fix-unit01.log`: `./internal/rpz` `-race -count=2`
  PASS (5.2 s). `fix-mount01.log` (retained failure): the first mounted draft
  ran `kudurru doctor` with a missing `--config` file, which doctor rejects;
  the `/info/messages` assertions before it had passed. `fix-mount02.log`:
  `TestRunServeSQLiteRPZFeedDiagnostics` `-race` PASS (11.0 s); it now reads
  `/info/messages` through the CLI and runs `kudurru doctor --skip-ports`
  while the feed serves (`RPZRulesIgnored` info, no `RPZExpired`) and after
  expiry (`RPZExpired` error, `diagnoses: FAIL`).
  `fix-mount03-mutant-captured.log` (retained mutant): with the
  `infoOperationalMessages` call removed the mounted test FAILs.
  `fix-mount04.log`: `-race -count=2` of `TestConvenienceGolden` (now also
  refusing `lists rpz <name> -A`), `TestRunServeSQLiteRPZFeedDiagnostics`
  (10.4 s, 11.0 s), `TestRunServeKubeRPZFeeds` (62.1 s, 56.2 s),
  `TestCLIReferenceDrift` and `TestDoctor*` PASS (`TestDoctorDatabase` skips
  without PostgreSQL), no race report. `fix-drift01.log`: `TestOpenAPIDrift`,
  `TestReferenceDrift`, `TestAggregatedRolesDrift` PASS; the fixes change no
  generated artifact. `fix-lint01.log`: golangci-lint v2.15.0 on the three
  packages, 0 issues. `fix-bench01.log`: bench guards OK.

What remains open: the dashboard shows no RPZ feed state yet. The list wizard
and list detail page cannot create or show RPZ sources, and there is no
dashboard unit test or Playwright spec. A later dashboard lane will build these
on the routes above. Feed state is per node, and there is no cluster-wide fan-out
read, so an operator compares nodes by asking each one. On Kubernetes the dig
explanation and policy list reads still report no RPZ fields (carried over from
RPZ-1). The full `./cmd/kudurru` suite (hack/go-test.py) was not run, and the full
`./internal/api` suite was not rerun after the review fixes. The Kubernetes
mounted test still does not read `/info/messages`; Kubernetes nodes use the
plain path, which the route test covers.
