### QUAL-UPGRADE-OPEN the open findings of the SQLite and Kubernetes upgrade packages — November 11, 2026 (D373)

This section follows "G22-WP1/WP2 SQLite upgrade: the refused-shape classes
and a stable built-binary test" (out/QUAL-SQLITE-UPGRADE-2/ledger.md) and the
Kubernetes upgrade section (out/QUAL-KUBE-UPGRADE/ledger.md), and closes
what their reviews and verifications left open.

A record set can no longer stop its zone through the primary runtime. The
compiler already withheld whatever the zone builder refuses, and the builder
refuses whatever `BuildCommittedPrimary` refuses. One more check the runtime
runs before admission could still be reached: `zonestate.ReplaceStatic`
refused the whole projection when a record set sat at a name that holds a
record a dynamic update (RFC 2136) added, because no production caller
authorizes a takeover. The zone then took no change at all while the dynamic
record existed (driver pending `retry`, every later record set stored and
never served). The compiler now checks each endpoint against the committed
dynamic records before the builder takes it (`zonestate.DynamicOwnership`,
the reservation rule of `ReplaceStatic` applied one record at a time),
leaves out only the colliding endpoints, reports a record set with nothing
served `duplicated` with the reason, and commits the rest in one pass; the
dynamic record keeps answering. A first cut left out the whole record set
of a colliding endpoint and recompiled the zone once per conflict found;
review showed that it withdrew the records a served multi-endpoint set had
been serving and that identical colliding sets cost quadratic time (5.79 s
for 400), so it was replaced. `TestCompilerWithholdsEveryRefusalOfThePrimaryRuntime` is the
table of every check the primary runtime runs on a compiled zone
(zonestate normalization and `ReplaceStatic`, the `BuildCommittedPrimary`
prebuild), with a record set shape that would reach each one, compiled with
no head, with a head and next to a dynamic record, plus a sweep over every
record type the DNS library knows. Zone-wide capacity bounds are listed in
the table without a case and stay open. Of several CNAME or DNAME targets
at one name, the kept one is served with its own TTL; for CNAME that
differs from 0.1.0, which served the lower TTL of both, and a test now pins
it.

A list source that has not finished its first download on this node never
blocks publication. A source whose digest is published but whose body this
node lacks as the source is configured now (a lost or never filled cache,
a URL changed after the last download, a follower that has not pulled yet)
is compiled empty, and storing its body wakes the serving publisher
(`DiskCache.AwaitBody`, `OnAwaitedBody` wired to `Publisher.Wake`); before,
it held back every candidate. While it waits the list does not block on
that node, and the node logs this once per source (`listfetcher: list
source has a published digest but this node holds no body for it`) and
again when the body is stored, with the source URL redacted. A body held under another digest still holds
the candidate back, so a node never serves a body nobody published. The
fetcher writes a list's status as soon as its own downloads end, and a
download outlives the pass that started it, so a URL that never answers
holds back neither the status of other lists nor the first download of a
list added meanwhile: a BlockList change ends the waiting pass and the next
pass starts after Run's one-second floor. A `RefreshNow` kick does not end
a pass that waits on a hung download; it is read when the pass returns.

The mounted upgrade test failed in the composed pre-push run with
`temp.test. served = false after 30s`. Creating a primary zone takes about
six commits one after the other, and each is served only after a full
serving publication (1.4 to 2.4 s under the race detector on four CPUs), so
the zone was served 14 to 22 s after its write here and later under heavier
load. The wait is now bounded by progress: it fails when the zones the API
reports and readiness have not changed for 30 s, or after three minutes.
A first form also capped the number of observed changes at twenty; that
failed a run that was progressing (21 changes in 12.5 s, readiness flaps
once per publication) and was removed. A one-shot UDP probe then failed
once with an i/o timeout: a query in flight when a generation is published
is dropped by design (the output is bound to the publication it was
captured under). The one-shot probes now ask again up to three times when
no response arrives, as a stub resolver does, and assert the response as
before; the SOA probe of the zone step uses the same helper, so a dropped
query is no longer read as "not served". The test also moves a published
list's URL to one that never answers and requires a later record to be
answered within 60 s. During the partition the Kubernetes cluster test now
requires the cut-off former Lease holder's health read to succeed before it
compares the reported leader, so a failed read no longer passes as "not
leader".

Evidence under `.context/v020-final-20261110/evidence/QUAL-UPGRADE-OPEN/`
(final release binary `kudurru 0.2.0-qual-uo (2c1a0239)`, `CGO_ENABLED=0`,
stripped, 81,940,640 bytes, sha256 `fe1ab386…`; previous release
`kudurru 0.1.0 (89c59f89)`, sha256 `f049295c…`, built from the tag; the
commits after 2c1a0239 change only tests):

- Built binary, `-race`, pinned to four CPUs (`taskset -c 4-7`), 20
  consecutive iterations (`e2e01-upgrade-x20-4cpu.log`, 1,077 s, zero
  skips, load average 2.6 to 5.6): `TestUpgradeFromV01DataDirectory`
  (20 of 20, 22.0 to 40.2 s each), `TestUpgradeFromV01RefusesLegacyObjects`,
  `TestBuiltBinaryRefusesNewerDatabases` and
  `TestBuiltBinaryRefusesNewerAuditSpool` (20 of 20 each); 340 subtests
  passed.
- Mounted, `-race`, pinned to four CPUs (`taskset -c 0-3`), 20 consecutive
  iterations at head `ecd3a38e` (`mounted12-x20-4cpu.log`, 2,475 s):
  `TestRunServeSQLiteUpgradeWithLegacyPlaceholders` 20 of 20 (110.8 to
  134.8 s each); temp.test. served 15.5 to 22.0 s after its write; a record
  written while a list URL is silent answered after 5.4 to 14.9 s, one
  written after a list URL moved to a silent one after 7.6 to 12.5 s;
  longest stretch of readiness away from 200 while a URL is silent 2.0 to
  4.8 s.
- Retained failures of the mounted test: `mounted10-awaited-before-overlay.log`
  failed `temp.test. served = false after 21 changes (12.518s)`, which is
  the change-count bound described above, not the overlay;
  `mounted11-x20-4cpu.log` (head `2c1a0239`, before the probe helper): 12
  of 13 passed, one failed `pair.api.test. A of a refused endpoint: <nil>
  … i/o timeout` (the dropped probe query; stopped after 13 runs to rerun
  with the fix); `mounted05`/`mounted06` failed an earlier form of the
  moved-URL step that was rewritten.
- Negative controls through `-overlay`, worktree untouched: with the four
  list files of 68a709e2 (`mounted08-lists-before-overlay.log`) the mounted
  test fails `the reachable list URL was not published within 1m0s while
  another URL is silent`; with only the preparation, cache and serving
  runtime of 28fcd110 (`mounted10`) the moved-URL step still passes
  (after-move answered after 16.4 s), because the fetcher's next pass
  already rewrites the moved list's status on a busy leader. The
  awaited-body path was then proven at unit level only (see the review fixes below for the mounted and runtime tests):
  `TestPublishedDigestWithoutLocalBodyServesEmptyUntilTheBodyArrives`
  (servingbuild) fails with the old `prepare.go` (`unit04`).
  `TestReconcilePublishesEachSourceWhenItsDownloadEnds` (`unit05`) and
  `TestRunDownloadsAListAddedWhileAURLIsSilent` (`unit07`) fail with the old
  `fetcher.go`. `TestCompilerWithholdsEveryRefusalOfThePrimaryRuntime`
  fails on the four dynamic-ownership rows with the compiler and projection
  of 68a709e2 (`unit02`), passes after (`unit03`);
  `TestCompilerDynamicCollisionRequiresExplicitTakeoverAndExpiryKeepsReservation`
  fails before (`unit01`); the mounted
  `TestPrimaryRuntimeRecordSetAtDynamicNameKeepsZoneCommitting` (real
  SQLite store, signed UPDATE) fails before with driver pending `retry`
  (`mounted03`) and passes after (`mounted02`; 5 of 5 under `-race` in `mounted13`);
  `TestBuildKeepsOneCNAMEWithItsOwnTTL` fails with the zone.go before the
  DNAME change (`unit06`, TTL 60 instead of 600).
- Review fixes (heads `36d37907` and, for the wiring test, the commit after it; logs `fix*.log`):
  `TestCompilerKeepsServedEndpointsOfARecordSetThatGainsADynamicName` (a
  served set {www, extra} edited to add an endpoint at the dynamic name,
  next to 50 identical colliding sets) and the extended dynamic-ownership
  row of `TestCompilerWithholdsEveryRefusalOfThePrimaryRuntime` (a dynamic
  DNAME above a static name, the reservation of an expired endpoint, a
  multi-endpoint set; every colliding set asserted `duplicated` with the
  dynamic-owner reason) fail before the fix (`fix01`: www and extra
  withdrawn, the multi-endpoint set reported duplicated) and pass after
  (`fix02`). The reviewer's probe as an overlay (`fix03`): www=1 extra=1,
  and 50/100/200/400 identical colliding sets compile in 4/6/11/35 ms
  (5.79 s for 400 before). The mounted
  `TestPrimaryRuntimeRecordSetAtDynamicNameKeepsZoneCommitting` now also
  writes a set with one endpoint at the dynamic name and one at
  kept.example.; it passes 3 of 3 under `-race` (`fix05`) and fails with the
  compiler and projection of `15021a0f` as an overlay (`fix13`, kept.example.
  never answered). New mounted
  `TestRunServePublishedListDigestWithoutLocalBodyServesEmptyUntilTheBodyArrives`
  (runServe on a data directory whose BlockList status names a published
  digest, an empty lists directory and a silent URL): ready and answering
  1.2 to 2.4 s after start and blocking 0.26 to 0.32 s after the URL
  answers, 5 of 5 under `-race` on four CPUs (`fix08`, `fix16`); with the
  `prepare.go` of 68a709e2 it fails `not ready and answering 25s after
  start ... (readiness 503, 1 list requests)` (`fix10`, and `fix12` with the
  wiring line removed too). With only the wiring line
  (`o.Lists.OnAwaitedBody(s.publisher.Wake)`) removed it still passes
  (`fix11`): on a single leader the fetcher's status write after the
  download prepares again. The wiring itself is proven by
  `TestServingRuntimeWakesWhenAnAwaitedListBodyIsStored` (cmd/kudurru: the
  real `newServingRuntime` with SQLite and the started primary; a BlockList
  whose status names a published digest the node's cache lacks is
  published with the list empty, then the body is stored with no store
  change, as a follower pulling by digest does, and the list must block
  within 10 s): 3 of 3 under `-race`, blocking 183 to 205 ms after the
  body is stored (`fix17`); with the wiring line removed through `-overlay`
  it fails `the stored body was not served within 10s: the generation was
  not prepared again` (`fix18`). `TestDiskCacheReportsAnAwaitedBodyOnce` fails
  before the log (`fix06`) and passes after (`fix07`). At the final head,
  `-race -count=2` on four CPUs (`fix16`, 247.5 s):
  `TestRunServeSQLiteUpgradeWithLegacyPlaceholders` 2 of 2 (119.9 s,
  118.1 s), the dynamic-collision and lost-body tests 2 of 2 each. Race
  runs of primaryzone, zonestate, primaryruntime (`fix04`) and listfetcher,
  servingbuild (`fix09`) pass; scoped golangci-lint over primaryzone,
  zonestate, listfetcher and cmd/kudurru 0 issues (`fix15`, after `fix14`
  found a gosec G101 on a test URL, fixed in `36d37907`). The built-binary
  e2e series was not rerun after the compiler change.
- Package race runs: `internal/primaryzone`, `internal/zonestate`,
  `internal/primaryruntime` (`race02`; `race01` failed only because
  `KUBEBUILDER_ASSETS` was not exported), `internal/listfetcher`,
  `internal/zone`, `internal/primaryzone` (`race05`), `internal/servingbuild`
  (257 s) and `internal/zonestate` (`race06`), `internal/listfetcher`
  `-count=3` (`race04`, `race07`). `TestKubeProcessesColdStartLeaseLossAndRejoin` with the fencing
  change passes under `-race` (`e2e02-kube-cluster-fencing.log`, 107.7 s,
  head `15021a0f`). `mounted13`: the dynamic-collision mounted test passes
  5 of 5 under `-race`. Scoped golangci-lint v2.15.0
  over listfetcher, primaryzone, zonestate, zone, servingbuild, cmd/kudurru
  and test/e2e found one errcheck issue in the new listfetcher test
  (`lint02.log`), fixed in `15021a0f` and re-linted with 0 issues over listfetcher and cmd/kudurru (`lint04`). No
  generated artifact changed.

Open: zone-wide capacity bounds (a million records, 64 MiB of records, a
128 MiB aggregate, 1,024 chunks) still refuse a zone as a whole, which then
keeps its last commit. A record set with nothing served at a dynamically
owned name is reported `Duplicated` only in the zone's source diagnostics,
and a left-out endpoint of a set that is otherwise served only by its
absence; the set's own conditions are computed without the dynamic records
and may still say ready. The moved-URL step of the mounted test does not
distinguish the old preparation from the new one on its own (see the
negative controls). The `OnAwaitedBody` wiring in `serving_runtime.go` is
proven on the real serving runtime with a body stored without a status
change, but not through a real follower: no mounted or e2e test runs a
follower that pulls the body over the internal API, and the mounted
lost-body test on runServe passes without the wiring on a single leader. A
`RefreshNow` during a hung download still waits for the pass. Unchanged from the earlier
sections and still open under G21: every commit makes readiness 503 until
its generation is published, an API write that overlaps a publication is
answered 503 without effect, and a query in flight at a publication is
dropped (a resolver asks again). The tests wait for a quiet node and ask a
dropped query again; neither is a product fix. The Kubernetes items the
earlier section left for the owner (the first 0.1.0 break-glass token,
the unpinned node that exits in an outage, the kind phase) are unchanged.
