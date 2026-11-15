### INT-6 ClientGroup-scoped record views (split horizon) — November 10, 2026 (D403)

An endpoint whose `kudurru.view` names a ClientGroup used to be withheld
from every client (`zone.Served`), so only the publish-only `public` half of
split horizon worked. It is now served to that group, on both drivers. The
ClientGroup is looked up in the record set's namespace, as Policy
`clientGroupRefs` are, and the resulting ID (`namespace/name`) is compared
with the `Client.GroupIDs` the identify stage already captured. A zone with
views is built as its default zone plus one complete variant per group (at
most 16). In a variant a view RRset replaces the default RRset of the same
name and type, a view CNAME replaces everything at its name, and any view
RRset replaces a default CNAME. Views never remove a default name, but a
view name and its empty non-terminals exist in the variant, so a default
wildcard does not synthesize answers at those names for the group (standard
wildcard semantics). Clients outside the group get the default zone,
including NXDOMAIN or NODATA with the default SOA for view-only names and
their empty non-terminals. A view replaces only default data its own
namespace claimed: a view RRset that would replace another namespace's
default RRset (same name and type, or a CNAME on either side) is refused as
`Duplicated`, as the same default endpoint would be. View identities are
keyed by the ClientGroup ID, so views of one name in two namespaces do not
conflict. The
authoritative stage selects the variant after the zone match, the query ACL,
the availability gate and the transfer branch (`Zone.ForGroups`). The
selection reads no store, does not allocate, and returns the zone itself when
the zone has no views. CNAME chains entering the zone from another zone use
the client's variant there too. A client in several groups with views of one
zone gets the view of the group whose ID sorts first.

How views interact with the rest of the system (all recorded in D403):

- DNSSEC: views are not served in a signed zone (`spec.dnssec.enabled`, or
  RRSIG/NSEC/NSEC3/DNSKEY in the default data, including imported
  signatures). The view endpoints are reported `Invalid` and the group gets
  the default data. Views cannot hold NS, DNAME, SOA or DNSSEC records or an
  apex CNAME. View data at or below a delegation, or below a DNAME, is
  refused as `Occluded`. On SQLite the compiler also checks the dynamic
  (RFC 2136) records of the committed head: a view endpoint below a
  delegation an UPDATE added is `Occluded`, and any DNSSEC record an UPDATE
  added makes the zone's view endpoints `Invalid`. Before this fix such a
  view was committed and reported served, and the committed build then
  refused the whole zone on every pass (review finding 1).
- Budget: at most 16 views per zone, shared in build order by every record
  set that feeds the zone, whatever its namespace (a known limit). Each view
  is a full copy of the zone's names and index, rebuilt with the zone.
- Reporting: on Kubernetes a refused view endpoint is reported on its record
  set like any other dropped endpoint. On SQLite the record set projection
  carries one outcome, so a refusal is visible only when nothing else in
  the record set is served (the existing rule for every dropped endpoint).
- Transfers and exports: AXFR, IXFR, the IXFR journal, NOTIFY serials and the
  BIND export read the default data only (`Zone.RRsets`; on SQLite the
  committed aggregate's `records`). A secondary never receives group data,
  and there is no per-TSIG-key view transfer.
- RFC 2136 UPDATE: prerequisites are evaluated against the default data and
  added records are default records, on both drivers (Kubernetes builds
  `dynamicupdate.State` from `built.RRsets`; SQLite from
  `Aggregate.Records`). On SQLite a static view endpoint still reserves its
  name and type against UPDATE, as disabled and publish-only endpoints
  already did. On Kubernetes view records are not reserved, as before.
- Content hash and serial: the hash covers the views (zones without views
  keep their hash), so a view change advances the serial. On SQLite the
  committed aggregate holds views in a separate `views` list (validated,
  sorted, charged against the record and wire limits, and folded into the
  head content hash only when present). `ReplaceSource` drops a removed
  source's view records.
- Answer cache: view answers are authoritative and final and never reach the
  cache stage. The e2e test shows that a lan-only name in an overlay zone is
  answered locally for lan while other clients keep getting the cached
  upstream answer.
- Edge (dnsdist) packet caches: they key on the question, never on the
  client's group. Any group view, whether in a desired record set or still
  served by a committed model in the same publication, makes the edge-cache
  state report `zone-client-view`. That turns off both
  `AuthoritativeAvailable` and `Available`, so the edge bypasses its
  authoritative and resolver pools (`bypass-zone-client-view`). The view
  check now runs before any authoritative-only reason, such as
  `zone-expiry`, can mask it. While a ClientGroup with client matchers
  exists the policy index is already client dependent and kudurru reports
  `client-scope` first; the live edge test shows both reasons.
- Sinks: Cloudflare plan selection skips group-view endpoints (it used to
  select them, which would have published internal data). UniFi push already
  skipped every viewed endpoint.
- Admission: the CRD schema restricts `view` to `public` or a DNS-1123
  subdomain name (pattern and 253-byte bound), and the builder checks the
  same rule for objects stored before the schema change.

Evidence under `.context/v020-final-20261110/evidence/INT-GROUP-VIEWS/`:

- `e2e01.log`: `CGO_ENABLED=1 KUDURRU_E2E=1 go test -count=1 -v -run 'TestClientGroupViewsSplitHorizon|TestModifierTargetsAndClientCacheIsolation' ./test/e2e/`:
  2 passed, 0 skipped. TestClientGroupViewsSplitHorizon took 7.64 s
  (SQLite binary: two groups over UDP and TCP, view-only NXDOMAIN with SOA,
  overlay/cache isolation, `public` never served, AXFR from a lan client
  carries no group data). TestModifierTargetsAndClientCacheIsolation took
  9.51 s. The package took 93.5 s, including the binary build.
- `race01-kube-mounted.log`: `go test -race -count=1 -run 'TestRunServeKubeClientGroupViews$' ./cmd/kudurru`:
  1 passed in 11.27 s (envtest and runServe: two groups over UDP and TCP,
  view-only NXDOMAIN, `public` never served, the apiserver refuses an
  invalid view name, a view change reaches the served zone).
- `race02-packages.log`: `go test -race -count=1` passed for
  `internal/zone` (10.8 s), `internal/zonestate` (76.3 s),
  `internal/stages/authoritative` (3.1 s), `internal/primaryzone` (246.5 s),
  `ee/integration/plan` (1.6 s) and `internal/servingbuild` (395.7 s).
  `race03-dnsdist.log`: `ee/edge/dnsdist` passed (44.7 s).
- New tests: `TestClientGroupViewsAnswerPerGroup`,
  `TestClientGroupViewsStayOutOfTransfersAndExport`,
  `TestClientGroupViewRefusals`, `TestClientGroupViewInactiveEndpoints`,
  `TestForGroupsAllocation` (0 allocations with and without views),
  `TestCommittedPrimaryViews` and `TestClientGroupViewsThroughRecordCache`
  (`internal/zone`); `TestClientGroupViewsPerClientAnswers` and
  `TestClientGroupViewsTransferDefaultData` (authoritative stage, UDP and
  TCP, chains, overlay fallback). With the two `ForGroups` lines disabled the
  stage test fails 16 assertions. `TestCompilerCommitsClientGroupViews`
  (SQLite compiler, codec, build, UPDATE state, serial),
  `TestCodecClientGroupViews` (zonestate), and
  `TestAuthoritativeCacheRefusesServedClientViews` plus extended
  `TestAuthoritativeCacheOriginalStaticAndUnavailableSources` (servingbuild:
  both pools refused). The extended dnsdist mode tests and
  `TestSelectOriginalPublicAndExpiry` (Cloudflare selection skips group
  views) also cover the change.
- Existing tests changed on purpose because they asserted the old withheld
  behavior. `TestContentHashIsCanonical` now requires a view record to change
  the hash. `TestLookupDoesNotMutateZone` recomputes the hash including views.
  `TestBINDRoundTrip` compares the export with the default-data hash, because
  the export carries no views.
- Performance (`bench01-baseline.log` at 841c5e99, `bench02-after.log` at
  37e162f1): BenchmarkServe B/op and allocs/op are unchanged:
  authoritative 464 B/5, cname-chain 480 B/5, nxdomain 448 B/4, not-local
  256 B/1. BenchmarkLookup100k is unchanged: hit 16 B/1, cname 32 B/1,
  wildcard 96 B/3, nodata 0/0, nxdomain 0/0. Wall time is within the noise
  of the shared machine (authoritative 2.9 to 5.9 µs before and 3.7 to
  5.5 µs after). TestLookupCNAMEAnswerAllocation, TestTracedChainAllocations
  and TestRequestKeepsItsSizeClass pass before and after. BenchmarkBuild100k
  at the base (`bench05-build-base.log`): 271.36 MB/op, 6,244,421 to
  6,244,426 allocs/op, 318.5 B/record. The first version, with the view in
  the shared identity key (`bench04-build-after.log`), measured
  275.56 MB/op. Commit 85c45854 moved view identities to their own map, and
  `bench06-build-after.log` measured 271.36 MB/op, 6,244,423 allocs/op and
  318.5 B/record, back at the base. No pinned benchmark file was edited, and
  `bench-guards01.log` shows the 14 bench guard tests passing.
- Retained failed runs: `bench03-build-base.log` did not compile, because
  `internal/zonestate` at HEAD referenced `zone.MaxViews` while
  `internal/zone` was checked out at the base. It was rerun as bench05.
  `lint01.log` reported one gofmt issue in `internal/zonestate/views_test.go`,
  fixed before commit aab4d80e. `unit01-neighbors.log` shows
  `internal/primaryruntime` failing only because `KUBEBUILDER_ASSETS` was
  unset; `unit02-primaryruntime.log` passed with it set.
- `drift01.log`: `internal/store/validator` and `hack/docsgen`
  (TestReferenceDrift) pass after the generated commit 9ee8dde8.
  `lint02.log`: golangci-lint v2.15.0 reports 0 issues over every touched
  package, including `./cmd/kudurru/...` and `./api/v1alpha1/...`.
  `race04-cmd-neighbors.log`: `go test -race -count=1 -run
  'TestRunServeSQLiteCompletePrimaryUpdateRestart|TestRunServeKubeDynamicUpdateAtomicityAndOwnership|TestRunServeKubeClientGroupViews|TestForwardedShapes|TestCLIReferenceDrift'
  ./cmd/kudurru` passed (ok, 41.6 s; run without -v, so there are no
  per-test durations).

Review fixes (commits a14a18fd, 8154ec2e, 55f56160; evidence `fix*.log`):

- Dynamic data wedge: `TestCompilerRefusesViewsHiddenByDynamicData`
  (`internal/primaryzone`, four cases: dynamic delegation above and at the
  view name, dynamic DNSKEY, unrelated dynamic A). `fix00-compiler-dynamic-before.log`
  is the retained failing run without the compiler change (3 of 4 cases
  fail: the view record set is `served`); `fix01-compiler-dynamic.log`
  passes. The committed build of the zone now succeeds and the unrelated
  record set commits.
- Ownership: `TestClientGroupViewsCannotOverrideAnotherNamespace` and
  `TestClientGroupViewsSameNameInTwoNamespaces` (`internal/zone`).
  `fix03-zone-ownership-before.log` is the retained failing run at the
  previous code (no Duplicated for three cross-namespace overrides; the
  same view name in two namespaces refused); `fix04-zone-ownership-after.log`
  passes the whole package.
- Recorded limits pinned by tests that pass before and after:
  `TestClientGroupViewNamesSuppressDefaultWildcard` and
  `TestClientGroupViewBudgetIsPerZone`.
- Export and IXFR: `TestClientGroupViewsSplitHorizon` now logs in to the
  mounted API, reads `GET /api/v1/zones/split/export` before and after a
  view-only change, and sends IXFR from the old serial. The serial
  advances; the IXFR answer is the unchanged default data with the new
  serial in AXFR form (the stage keeps no IXFR journal for this zone, as
  documented in `transfer.go`), with no group data. Retained failed runs:
  `fix05-e2e-export-ixfr.log` and `fix05b` (the wait treated the
  fail-closed no-answer window during the commit as fatal),
  `fix05c` (IXFR query with an empty SOA in authority: FORMERR),
  `fix05d` (the assertion expected an SOA-only delta). `fix05e` and
  `fix18-e2e-final.log`: `-count=2` of TestClientGroupViewsSplitHorizon
  and TestModifierTargetsAndClientCacheIsolation, 4/4 passed (43.6 s
  package).
- Live edge: `TestRunServeRemoteEdgeClientGroupViews` (`cmd/kudurru`, F39
  live suite, pinned `powerdns/dnsdist-21:2.1.3@sha256:c70ae736...`, image
  already present): the real root behind the real supervisor and dnsdist.
  After an authoritative cache hit on the default answer, ClientGroups lan
  (127.0.0.2) and kids (127.0.0.3) and an outside client (127.0.0.4) each
  get their own answer for `probe.private.example` and `nas` three times
  in alternating order, with no cache hit, both caches detached and the
  same daemon PID (mode `bypass-client-scope`). After the ClientGroups are
  deleted the zone still serves views and both caches stay detached as
  `bypass-zone-client-view`; removing the views requalifies both caches.
  `fix06-live-edge-views.log` is a retained failure (`root ready` timed out
  after 25 s while two other heavy runs shared the machine; the existing
  `TestRunServeRemoteEdgeAuthoritativeCacheACL` passed alone right after,
  `fix10`). `fix11` and `fix12` are retained failures of the first test
  version, which expected `bypass-zone-client-view` while ClientGroups
  existed; `fix12` logged the actual `bypass-client-scope` with the correct
  per-group answer. `fix13` passed (98.7 s). `fix14-live-f39-suite.log`:
  `hack/test-dnsdist-live.sh f39` (now including the new test), 3/3
  passed: RegexAndLateFill 81.6 s, AuthoritativeCacheACL 54.1 s,
  ClientGroupViews 66.7 s.
- `fix07-race-packages.log`: `go test -race -count=1` passed for
  `internal/zone` (10.4 s), `internal/zonestate` (81.2 s),
  `internal/primaryzone` (181.3 s), `internal/stages/authoritative`
  (2.5 s), `ee/integration/plan` (1.6 s), `internal/primaryruntime`
  (116.7 s). `fix09-kube-mounted.log`: `TestRunServeKubeClientGroupViews`
  passed with -race (21.6 s). `fix19-bench-build.log`: BenchmarkBuild100k
  271.36 MB/op, 6,244,420 to 6,244,426 allocs/op, 318.5 B/record (equal
  to the base); TestForGroupsAllocation and
  TestLookupCNAMEAnswerAllocation pass. `fix08-bench-guards.log`: 14 bench
  guard tests pass. `fix17-lint.log`: golangci-lint 0 issues over the
  touched packages (`fix15` and `fix16` are retained gosec findings on the
  new e2e login, fixed by using the existing login helper).
  `fix20-drift.log`: `internal/store/validator` and `hack/docsgen` pass; no
  generated file changed, so there is no new generated commit.

What remains open: views cannot be
transferred to a secondary, and there is no per-TSIG-key view selection.
Views are not served in signed zones. A client in several groups gets only
the first group's view; views are not merged. On Kubernetes, static view
records do not reserve their names against UPDATE, while on SQLite they do
(a pre-existing difference, unchanged here). There is no dashboard surface
for choosing a view, and the API and UI do not show which group sees which
answer. The 16-view budget is shared first come across namespaces; there
is no per-namespace budget. On SQLite a refused view endpoint is not
reported when the rest of its record set is served. The live edge test runs
Do53 over UDP only. The full `./cmd/kudurru` suite was not run.
