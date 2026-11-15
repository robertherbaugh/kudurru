### OBS-1 query-log sink fan-out host — November 10, 2026 (D371)

The collector had one queue feeding one sink, so no exporter could be added
without tying it to the SQLite commit. `querylog.sinks` now lists up to eight
additional destinations beside the primary `querylog.sink`, and D371 records
the delivery contract the V2-OBS row and README contract 6 ask for.

The one collector still applies the exclusions and the privacy mode once,
stamps the ID and publishes to the live tail. It then offers the same immutable
event, without waiting, to one bounded queue per additional sink
(`internal/querylog/sinkhost.go`). Each sink has its own goroutine, batch size,
flush interval, per-attempt timeout and attempt budget. Overflow is dropped for
that sink only and counted in
`kudurru_querylog_dropped_total{stage="sink",sink="<name>"}`. A batch is written
again only after an error the sink marks as not applied (`querylog.Retryable`,
optionally with the wait the destination asked for); every other failure and an
exhausted budget drop the batch, and nothing is replayed or moved to another
sink. `kudurru_sink_sent_total`, `kudurru_sink_errors_total`,
`kudurru_sink_queue_length` and `kudurru_sink_last_success_timestamp_seconds`
(the names section 11.1 reserved) carry one series per configured sink name and
disappear with the sink. Loss is logged at the first event and at each tenfold
total. The privacy transform runs before every sink queue, an excluded query
enters none, `nothing` offers no per-query event, and `querylog.enabled: false`
stops every sink.

Sinks follow the captured-route model of D266. Preparing a configuration
builds the sink objects and takes one reference per sink for the prepared
route, but opens nothing: a sink is opened from its own goroutine by its first
event, and the open is tried again with capped backoff until it succeeds, so an
unreachable exporter cannot delay startup, a reload or a publication. The host
opens a sink once; a sink type that holds a connection re-establishes it inside
its own writes. On the complete SQLite runtime the bundle's release hook
returns the references (`internal/servingbuild`), and a candidate that is
prepared and then refused returns them itself; on Kubernetes the reload
subscriber's runtime holds them. An unchanged entry keeps its queue, goroutine
and connection through every publication; a new entry starts with new queries
and is never backfilled; a sink without references stops accepting behind the
events the collector had already queued (`Async.AfterQueued`), drains for at
most five seconds, counts the rest and closes.

Requests on the reload subscriber of the Kubernetes driver do not hold their
route: a query reads the collector's adopted route and is then queued. When a
reload changes a sink's entry between those two steps, the query is offered to
a sink that has already retired. It is not handed to the entry's new sink (a
route's privacy mode and its sinks are one captured decision); it is counted
as dropped for the name, with the reason `sink replaced`, while a route still
references a sink of that name. When the entry was removed, no sink was owed
the query, nothing is counted and the removed series stay gone. The query is
in the local database and the live tail in both cases. A request that holds a
complete publication never reaches a retired sink.

Shutdown drains the collector, then closes `querylog.db` beside the sink
drains under the same five-second bound, and a sink that ignores cancellation
is never closed under its own write. `querylogRuntime.close` reports the
collector and the database only, so `query log flush incomplete` means local
loss. A sink that did not drain is named in its own warning (`additional
query-log sink did not drain`, with the events still queued and its drop
total); `SinkHost.Close` returns a `SinkDrainError` that lists those sinks. A
destination that is stalled or unreachable at shutdown can hold the process for
the rest of the five seconds.

The first type is `slog` (log lines next to the database). The settings schema
accepts exactly the types the runtime can build
(`TestQuerylogSinkTypesHaveFactories`). A later sink type adds a
`querylog.Sink` implementation, its factory in `querylogSinkFactories`, and its
option and settings object in `internal/config`; the host passes the settings
object named after the type to `Sink.Open` unchanged. D371 also settles the
open choice in contract 6: ClickHouse and Loki ship as direct HTTP batch sinks
on this host, and dnstap to DNS-collector stays the documented path for other
destinations and for wire-level consumers.

Evidence under `.context/v020-final-20261110/evidence/OBS-FANOUT/`:

- Mounted, complete SQLite runtime:
  `TestRunServeQuerylogSinksStalledFailingAndReload` (`race02`, 13.84 s;
  `root-race01`, 13.79 s; `root-race02`, 13.86 s; three more consecutive
  passes in `root-race03`; after the review fixes `fix-root-race02`, 13.97 s
  and 14.94 s, and `fix-root-race05`, 13.83 s and 13.84 s). The settings command adds
  a sink that never returns from its first write; 160 real UDP queries are then
  answered (87 to 108 ms in the retained runs), all 162 rows are read back
  through `GET /queries`, the mounted SSE tail delivers every event with
  consecutive IDs, and the metrics listener reports exactly 60 overflowed
  events for that sink, a queue length of 100 and no collector-queue drops.
  Replacing the stalled sink gives the new sink exactly the three queries
  answered after it was added, with the next three IDs. A further settings
  change with the entry untouched keeps the same sink instance (built and
  opened once) and delivers the name hidden under `hide_domains`. A sink that
  keeps answering "try again" is written twice per batch, then the batch is
  dropped and counted (4 errors, 2 drops). An unknown type, two log-line sinks
  and a reserved name are refused with a 4xx and leave the settings unchanged.
  Removing the last sink removes its series. After shutdown the reopened
  `querylog.db` holds all 169 rows.
- Mounted, Kubernetes driver on envtest:
  `TestRunServeKubeQuerylogSinksSettingsFileReload` (`root-race01`, 7.74 s;
  `root-race02`, 8.18 s; three more consecutive passes in `root-race03`;
  `fix-root-race02`, 8.22 s and 8.48 s; `fix-root-race05`, 7.94 s and 7.52 s). The
  mounted settings file is rewritten by rename; the watcher, the
  shared-settings refresh and the reload subscriber add a sink, replace it with
  a stalled one and remove it. Real UDP queries keep answering, rows keep
  committing, the overflow counter reads exactly 50, sink IDs are consecutive
  with no replay, and the removed sink closes once.
- Reload subscriber against the real collector, tail and SQLite writer:
  `TestQuerylogRuntimeSinksLegacyReload` (`race01`, 3.34 s; `fix-root-race02`
  and `fix-root-race05`, 3.34 to 3.35 s in all four runs): a stalled and a healthy sink at once,
  300-event burst, exact overflow of 200, unchanged sink neither rebuilt nor
  reopened, and a 300 ms shutdown bound that still closes the database and
  never closes the stalled sink under its write. Since the review fixes it
  also requires that `close` returns no error for that shutdown and that
  exactly one `additional query-log sink did not drain` warning names the
  stalled sink with 100 events queued and 200 dropped.
- Host and collector seam (`internal/querylog`, `light-race03` on the committed tree): 12 new tests
  in `sinkhost_test.go`, among them
  `TestSinkFanoutBlockedSinkNeverStallsPrimaryOrTail`,
  `TestSinkRetriesAreBoundedAndOnlyForRetryableFailures`,
  `TestSinkReloadAddsAndRemovesWithoutReplay`,
  `TestSinkOpenFailureNeverBlocksAndRecovers`, `TestSinkHostCloseIsBounded`,
  `TestSinkReceivesOnlyTransformedEvents` and
  `TestAfterQueuedOrdersBehindAcceptedEvents`. `internal/querylog` and
  `internal/config/...` under race: exit 0, 409 passes, no skips.
  `internal/config` adds `TestQuerylogSinks` and `TestQuerylogSinksValidation`
  and pins `querylog.sinks` as a hot key in `TestRestartRequired`. The review
  fixes add two tests there (14 in all), see below.
- Mutations, each restored afterwards: retiring a released sink at once
  instead of behind the collector queue fails the reload test (`mutation01`:
  the removed sink misses queued events); a blocking offer fails the collector
  test (`mutation02`) and the mounted test at "SQLite rows to keep committing
  behind a stalled sink" while the 160 UDP queries are still answered
  (`mutation04`). `mutation03-canceled` is a run that was withdrawn while it
  waited for the machine lock; nothing ran.
- Root selection with envtest (`root-race01`): exit 0, 48 passes, no skips, in
  118.2 s. It includes the existing query-log reload, failed-prepare,
  disabled-restart, export and storage-restart mounts, the reload subscriber
  tests, `TestRunServeSQLiteCompletePrimaryUpdateRestart`, two Kubernetes
  mounts, `TestForwardedShapes` and `TestCLIReferenceDrift`. After the lint
  fixes, the sink, reload and CLI-reference selection (`root-race02`): exit 0,
  12 passes, no skips, in 58.4 s; the three sink tests three times in a row
  (`root-race03`): exit 0 in 78.6 s.
- Library races (`lib-race01`): exit 0 for `internal/querylog/...` (sqlitelog
  219.3 s), `internal/config/...`, `internal/servingbuild` (159.1 s),
  `internal/serving`, `internal/store/validator`, `hack/docsgen` and `deploy`.
  API settings, schema, query and OpenAPI-drift selection (`api-race01`):
  exit 0.
- Lint: `lint01` (retained) reported seven gosec findings, all in the new
  test files (six integer conversions and a marshaled login struct); they
  were rewritten or annotated, and `lint02` reports zero issues for
  `internal/querylog`, `internal/config`, `internal/servingbuild` and
  `cmd/kudurru`. `go vet` (`vet01`, `vet02`) and `gofmt` are clean.
- Generated: `make generate` (`gen01`) changed the three copies of the
  `Kudurru` CRD; `make docs-gen` (`gen02`) changed the settings and CRD
  references. `TestOpenAPIDrift`, `TestReferenceDrift`,
  `TestAggregatedRolesDrift` and
  `TestSettingsAdmissionContainsOnlyPublicClusterKeys` (`drift01`) and
  `TestCLIReferenceDrift` (`root-race02`) pass on the committed tree; no API
  route or DTO changed, so OpenAPI and both generated clients are unchanged.
  `make docs-site` with `--panicOnWarning` builds (`docs-site01`,
  `docs-site02`). `make size` with `CGO_ENABLED=0` (`size01`): 81,219,744
  bytes, 81% of the budget; no dependency was added.

Review fixes (commits `d60a3d4e`, `164eaf2c`, `7071dac5`, `e1c8d88f` and
`7c4e1779` on `wip/v020/obs`, after the generated commit `907a3000`; logs
`fix-*`). The first three commits were made by a fixer that was interrupted
before it had finished the mounted checks; a second fixer verified them,
reworked one test that did not prove its claim, and ran the rest.
`fix-root-race04` and `fix-before03` ran on the sources of `e1c8d88f` and
`7c4e1779` just before they were committed. No generated artifact changed, so
there is no second generated commit.

- A query caught by the replacement of its sink (`d60a3d4e`, `e1c8d88f`).
  Host seam: `TestSinkEventInFlightAtReplacementIsCounted` (the exact case:
  the retired instance writes one event, the new one two, one is counted, and
  after the name is removed a late event counts nothing and revives no series)
  and `TestSinkReplacementAccountsForEveryAcceptedEvent` (four enqueuers
  through `Async.Enqueue` across 300 replacements: accepted = written +
  dropped; `fix-after01`, three runs, 3, 12 and 26 events counted as dropped).
  Real runtime through the reload subscriber:
  `TestQuerylogRuntimeQueryInFlightAtSinkChangeIsCounted` holds the route a
  query read, changes the entry, waits for the replaced sink to close and
  queues through the held route: three rows in `querylog.db`, the old sink
  wrote one query, the new one one, `dropped{stage="sink",sink="export"}` is 1
  and `kudurru_sink_sent_total` is 2; after the entry is removed a late query
  is the fourth row and no series returns (`fix-root-race04`, three passes of
  2.06 to 2.08 s; `fix-root-race05`, 2.06 s and 2.07 s).
  `TestQuerylogRuntimeChangedSinkEntryCountsQueriesInFlight` keeps four
  enqueuers running across 200 changes of the entry and requires accepted =
  written + dropped, no query written twice and 201 builds (`fix-root-race04`:
  233,278, 279,675 and 276,941 queries accepted, 1, 4 and 0 of them counted as
  dropped; `fix-root-race05`: 297,748 and 281,170 accepted, 1 and 7 counted
  as dropped).
- Shutdown reporting (`164eaf2c`): `TestSinkHostCloseIsBounded` now requires a
  `SinkDrainError` that names both blocked sinks with two events queued each
  and still wraps the deadline error; the runtime assertions are in
  `TestQuerylogRuntimeSinksLegacyReload` above.
- The shipped sink type through its production factory (`7071dac5`):
  `TestQuerylogRuntimeSlogSinkWritesQueryLines` configures one `slog` sink with
  the real factories and a JSON logger under `hide_domains`: exactly one
  info-level `query` line per query with `sink=log-lines`, the name hidden, no
  query name anywhere in the log, both rows in `querylog.db`, a clean close and
  no drain warning (`fix-root-race02`, 1.07 s twice; `fix-root-race05`,
  1.06 s and 1.07 s).
- A refused candidate (`7071dac5`):
  `TestCandidateReturnsItsQueryLogSinkReferences` in `internal/servingbuild`
  prepares a logging route with a sink reference and refuses the candidate
  after the semantic owners are complete (a dependency that changed after its
  capture) and inside semantic preparation (a catalog resource that does not
  match the settings); the host forgets the sink both times. An accepted
  candidate holds the sink until the logging component's release hook runs, and
  the sink is never opened (`fix-light-race02`, three passes of 0.76 to
  0.81 s; `fix-lib-race02`, two passes of 0.86 s).
- Final tree (`7c4e1779`, clean worktree, `fix-batch04-steps`): the five
  runtime tests and the two mounted tests of `querylog.sinks`, twice, under
  race (`fix-root-race05`): exit 0, 14 passes, no skips, 63.1 s. The existing
  query-log runtime, reload and mounted tests and `TestCLIReferenceDrift`
  (`fix-root-race06`): exit 0, 8 passes, 31.8 s. `internal/querylog` twice
  under race, the servingbuild test twice and `TestReferenceDrift`
  (`fix-lib-race02`): exit 0. `go vet` (`fix-vet02`) and scoped lint over
  `internal/querylog`, `internal/servingbuild` and `cmd/kudurru`
  (`fix-lint02`): exit 0 and zero issues. `gofmt` is clean. The same
  selections on `7071dac5`, before the test rework: `fix-root-race02`
  (exit 0, 12 passes, 74.8 s), `fix-root-race03` (exit 0, 8 passes, 32.1 s),
  `fix-vet01`, `fix-lint01` (zero issues) and `fix-docs01` (`make docs-site`
  with `--panicOnWarning` builds 164 pages; `TestReferenceDrift` passes).
  Whole packages under race on the fixed sources (`fix-lib-race01`):
  `internal/querylog` 1.4 s, `internal/querylog/sqlitelog` 217.2 s,
  `internal/servingbuild` 160.6 s, exit 0. `fix-stages01` is the first fixer's
  vet and race run of `internal/querylog` for each of its first two commits,
  and `fix-light-race03` the whole package three times under race again.

Retained runs of the review fixes that failed or did not complete (the
before-fix runs and mutations were expected to fail):

- `fix-before01`: the two host tests against `sinkhost.go` as committed at
  `907a3000`: `4 events were recorded locally; sink x wrote 3 and counted 0
  dropped, want 3 and 1`, and one of 30,361 accepted events unaccounted for.
- `fix-before02`: the runtime tests of `7071dac5` against the runtime and host
  as committed at `907a3000`, with the `sink` attribute removed from the slog
  factory. `TestQuerylogRuntimeSinksLegacyReload` fails twice on `close
  reported an incomplete local flush although only an exporter did not drain:
  context deadline exceeded`; `TestQuerylogRuntimeSlogSinkWritesQueryLines`
  fails twice on the missing attribute. The first form of
  `TestQuerylogRuntimeChangedSinkEntryCountsQueriesInFlight` passed both
  times with `accepted 12000, written 12000, counted as dropped 0`, the same
  numbers as on the fixed tree (`fix-root-race02`): its primary sink was the
  SQLite writer, which under the race detector holds the collector for most of
  a second per batch, so the queue stayed full and no query was ever caught
  in flight. That is why `e1c8d88f` adds the exact test and gives the load
  test a primary sink the collector can keep up with.
- `fix-before03`: the two reworked runtime tests with one line of the fix
  removed through an overlay (`offer` does not call `missed`). The exact test
  fails in all three runs (`dropped{stage=sink,sink=export} = 0, want 1`).
  The load test fails in one run of three (`the collector accepted 305732
  queries; sink export wrote 305729 and counted 0 dropped: 3 are unaccounted
  for`, after its 30 s wait) and passes twice with no query caught: it is an
  invariant check whose trigger is a matter of timing, and the exact test is
  the proof.
- `fix-mutation01`, `fix-mutation02`: `semanticOwners.release` without the
  sink release fails both refusal cases and the accepted case of the
  servingbuild test; a logging component without its release hook fails the
  accepted case. `fix-mutation03`: `missed` without its check that a route
  still references the name fails `TestSinkEventInFlightAtReplacementIsCounted`
  (`a late event revived series of the removed sink`).
- `fix-flake04`, `fix-flake05` and `fix-flake-probe04` are the reproductions
  of review finding 2 described below; their failures are the result.
- `fix-root-race01` is the first fixer's run of the sink tests, cut off in its
  third test when that session was interrupted; `fix-batch01-steps` belongs to
  it. `fix-flake-build01` was withdrawn while it waited for the machine lock;
  nothing ran.

`TestRunServeSQLiteCompletePrimaryUpdateRestart` (review finding 2). The
reviewer saw this existing test fail 3 times in 25 runs on the lane tree
(`review-root-race02`, `review-root-race03`: `mounted built-in query lost
committed authority`, `SERVFAIL`, `servfail_unsynced`,
`secondary:unavailable:example.`) and never in 32 runs of the base and lane
sources back to back. The cause is in code this package does not touch.
The failure reproduces on the base sources when the test reaches its built-in
queries a little later than usual:

- Mechanism, read from the code. The committed-primary status writer runs
  250 ms (`DefaultZoneCoalesce`) after a publication and writes the zone's
  status (`servingbuild.PrimaryStatus.Reconcile`). That write advances the
  `DNSZone` resourceVersion, which is part of the lifecycle source token
  (`primaryruntime.imageCurrentSource`), so the publication that carries the
  write serves the zone as committed-unavailable until the primary driver has
  aligned its receipt; the comment at `primary_status.go:103` states it ("DNS
  admission still fails closed"). The HTTP adapter waits up to two seconds for
  an eligible publication (`servinghttp.Adapter.capture`), so a built-in
  `POST /dns/query` that arrives while the status write is being published is
  answered from that publication after a wait: the reviewer's `SERVFAIL` with
  `TookMS:492.123`. The test sends its built-in queries about 35 ms after its
  last wait, normally about 200 ms before the status write.
- Observation (`fix-flake-probe01`, five runs of a race test binary built from
  the `33ee04bc` sources through an overlay, with an overlay-only probe that
  queries the DNS listener every millisecond and reads the `DNSZone` through
  the API): the status write became visible 40 to 254 ms after the test's last
  wait in every run. After it the listener gave the probe no answer within
  200 ms for 0.6 to 0.8 s in every run, and answered `SERVFAIL` for 0.44 s and
  0.55 s in two of them. The lane tree shows the same in three runs
  (`fix-flake-probe02`, built from the sources that became `7c4e1779`), once
  with the built-in query sent 4 ms after a `SERVFAIL` phase of 0.48 s had
  ended. All eight runs pass.
- Reproduction on base (`fix-flake04`): the same base binary, probe off, with
  one `time.Sleep(450 ms)` between the test's last wait and its built-in
  queries fails 3 of 12 runs with the reviewer's message (`mounted built-in
  query lost committed authority`, `SERVFAIL`, `servfail_unsynced`,
  `secondary:unavailable:example.`, `TookMS` 371, 214 and 222). The lane head
  with the same sleep (`fix-flake05`) fails 2 of 8 runs (`TookMS` 142 and 194).
  With the probe on (`fix-flake-probe04`, base, four runs) one run fails; its
  timeline has the status write 383 ms before the query and the `SERVFAIL`
  answer 188 ms after the query was sent.
- Controls: without the sleep the base binary passes 10 of 10 (`fix-flake01`).
  With a sleep of 1000 ms base and head pass 12 of 12 each (`fix-flake02`,
  `fix-flake03`): by then the driver's alignment is committed, the request
  waits for that publication instead and is answered correctly
  (`fix-flake-probe03`: two built-in queries sent during a `SERVFAIL` phase
  were answered `NOERROR` 443 ms and 550 ms later).

The test therefore fails when its thread reaches the built-in queries roughly
0.2 to 0.8 s late and the status write's publication is still the unaligned
one, which a busy machine provides now and then. This package does not touch
the status writer, the primary runtime, the authoritative stage or the test;
with no sink configured its code on this path is an empty sink set. The
frozen binaries were built by `fix-flake-build02` to `fix-flake-build04` and
removed afterwards; the probe and the sleep exist only in the overlay, whose
sources are kept in `fix-probe-source`.

What remains open: no dnstap, ClickHouse or Loki sink exists yet; this package
is the host they mount on, and `slog` is the only type. Events carry no DNS
wire data, so raw capture and its audited override remain for the dnstap
package. Sink state is exposed as metrics and log warnings only: there is no
status API route, CLI command or dashboard page for sinks, and the hand-kept
dashboard schema mock does not list `querylog.sinks` yet. On the complete
SQLite runtime a removed sink closes when the last publication that named it
is released, so a long-lived request holding that publication (an open
live-tail stream) delays the close and keeps the sink's series visible; it
receives no new queries meanwhile. A blocking exporter that ignores
cancellation keeps one goroutine until it returns. The host opens a sink once
and never reopens it after a failed write, so a sink that holds a connection
(dnstap) must reconnect inside its own write; a factory error on an admitted
document fails candidate preparation on SQLite and marks the node unready on
Kubernetes, so each type's settings must be validated completely in
`internal/config`; the per-attempt timeout reaches only sinks that implement
`BatchSink` and honor the context. On Kubernetes a query caught by a change of
its sink's entry is counted, not delivered, so changing a sink on a busy node
can fire `KudurruQuerylogDrops` once. The settings validator admits one `slog`
sink, so the mounted proofs exercise one additional sink at a time; two sinks
at once (one stalled, one healthy) are proven at the reload subscriber and
collector seams. `TestRunServeSQLiteCompletePrimaryUpdateRestart` stays timing-dependent on
the base sources, as shown above: this lane changed neither the test nor the
status writer. Whether the test should wait for the status write to settle
before its built-in queries, and whether a primary zone that answers `SERVFAIL`
for about half a second after a status write (race-detector builds, mounted
test) is acceptable, are decisions for the integrator. The full `./cmd/kudurru` suite, `test/e2e`,
the Grafana dashboard (no `kudurru_sink_*` panels yet; the existing drop panel
already groups by sink and `KudurruQuerylogDrops` fires on any drop) and the
paired performance gate were not run or changed in this lane, and no hosted CI
ran on the lane branch.
