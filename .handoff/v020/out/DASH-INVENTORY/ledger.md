### DASH-INVENTORY Dashboard inventory view and client-group record views — November 13, 2026 (D397, D403)

The dashboard now has an inventory view, and its record editor can set
client-group record views.

**Filtering > Inventory** (`/inventory`) is a separate list page in the
navigation, next to Clients, and the Clients page header links to it. It is
not a tab of Clients. The ui.md rule is that every inspected list gets its
own page with a URL. The two lists also come from different sources (the
query log against the inventory snapshot), use different permissions
(`clients.read` against `inventory.read`) and have different columns. The
page reads `GET /inventory` (D397) and shows every device: devices that
inventory integrations report and the answering node's DHCP leases, including
devices that never sent a query. Each row shows:

- names, hostname, device ID and tags;
- MAC addresses;
- every address with its state in words, explained in a tooltip (Assigned,
  Expired, Ambiguous, Superseded with the owning device and source, or
  Withdrawn);
- observation and expiry times;
- Live or Stale. Only Stale is a badge.

The filters (source, kind, staleness, text) are sent to the server and kept
in the URL. Keyset pages load with "Load more devices". A device links to the
Integration that reported it and, for a lease, to its DHCP scope. It also
links to the client page of each address that the query log knows (GET
/clients, 24 h).

For the Integration link, `GET /integrations` summaries now carry
`inventorySource`. It is computed with the same `host.SourceID` (cluster,
namespace, name, UID) that the host stamps on every observation, on both the
SQLite and the Kubernetes administration paths. A session without
`integrations.read` sees the raw source ID instead.

On a SQLite cluster member the page states that DHCP lease devices and lease
MACs belong to the answering node only. `GET /inventory` itself now answers
`nodeLocalLeases: true` on such a node (SQLite driver with more than one
peer, or the primary or follower role, decided as `GET /cluster` does), so
operator and auditor sessions, which have `inventory.read` but not
`cluster.read`, get the caveat on a cluster primary too. GET /cluster and the
follower status remain fallbacks. On other nodes a
short line names the node whose leases are shown. Without `inventory.read`
(the viewer role) the page sends no request and says which permission is
needed. The two 503 answers each get their own message: "not mounted" and
"serving inventory unavailable" (startup, retried every 30 s). Any other
error uses the usual error banner. The page only reads, so the follower and
static-mode gating does not hide it, and tests show it lists on both. The
follower tables did not change (`inventory.read` is a read). `follower.test.ts`
still checks them against the regenerated OpenAPI document. The `mac`
matcher help on the group pages and in MatchersField now names DHCP lease
MACs and their node-local effect.

**Record views (D403).** The record editor has a "Served to" picker:

- every client (the default data);
- published only (`public`);
- a ClientGroup. For a record set given as `ns/name`, only groups in that
  namespace are offered. For a bare name, groups of every namespace are
  offered, and choosing one from another namespace stores the new record in
  `that-namespace/api-<zone>`. When the record set is retyped afterwards,
  the choice keeps naming the same group (`ns/name` becomes `name` once the
  record set names that namespace, and the reverse), and a group of another
  namespace than the record set's is refused before saving instead of
  storing the view name where it would name a different group.

When a group is chosen, the editor says what zones.md says: views are not
served in a DNSSEC-signed zone (it warns when the zone object has
`spec.dnssec.enabled` or the records hold RRSIG/NSEC/NSEC3/DNSKEY). AXFR,
IXFR, NOTIFY, export, RFC 2136 UPDATE and integration sinks carry only the
default data. The dnsdist edge bypasses both packet caches
(`zone-client-view`). A zone holds at most 16 views, and a view replaces only
default data from its own namespace. The editor refuses NS, DNAME, SOA, the
DNSSEC types and an apex CNAME in a view before saving. An existing record's
view is fixed, because the view is part of the record's identity.

Record lists show a "view <group>" badge (`ns/name` outside the default
namespace) linked to the group, or a "public" badge. Rows are keyed by view
and namespace. Delete removes one view only, and the confirmation names the
view.

On the SQLite runtime the record list is the served zone: its rows name no
record set, and a `public` endpoint or a group view the build refuses (a
signed zone, more than 16 views, default data of another namespace) is stored
but never listed. The zone page detects such a list (every row has an empty
`recordSet`, which a desired row never has, and a served zone always lists
its SOA) and says above it what it shows and leaves out; the editor warns
before a public save there ("A public record is stored but not listed
here", naming the Tools download and the `DELETE ...?view=public` call that
still reach it), the group-view note says a refused view is not listed, and
the success toast says a public record is not listed.

Three server changes were needed for this:

- `POST /zones/{zone}/records` matched an endpoint by name, type and set
  identifier only. Writing a view therefore replaced the default endpoint of
  the same name and type in the record set, and the other way round. The view
  is now part of the identity, on the store path and on the SQLite command
  path.
- `DELETE /zones/{zone}/records/{name}/{type}` takes `?view=` (a ClientGroup
  name, `public`, or `-` for the default data only). Without it, every view
  is removed, as before. The forwarded follower input omits an empty view.
- The Playwright run found that on the complete SQLite runtime the record
  list (the served model) never listed view RRsets, so a committed view was
  not visible. Served rows now include `Zone.ViewRRsets`, with `kudurru.view`
  set to the group name and `namespace` set to the group's namespace, so
  `kids/lan` and `default/lan` of one name and type are two rows.
- `DELETE /zones/{zone}/records/{name}/{type}` also takes `?namespace=`
  (only record sets of that namespace; omitted when empty in a forwarded
  input), on the store and the SQLite command path. The dashboard sends it
  for a served view row, so deleting `kids/lan` keeps `default/lan`.
- `GET /inventory` answers `nodeLocalLeases`.

`docs/deploy/client-identity.md` and `docs/architecture/zones.md` describe
all of this.

Evidence under `.context/v020-final-20261110/evidence/DASH-INVENTORY/`:

Review fixes (after the "accept-with-fixes" review, `review-*.log`):

- `fix-api01-without-namespace.log` and `fix-api02-field-without-effect.log`
  (retained, failed on purpose): without `ZoneRecord.Namespace` the test does
  not build; with the field but without setting it and without the delete
  filter, `TestZoneRecordDeleteViewInOneNamespace` fails on both subtests
  ("the delete of kids/lan touched default/lan: []") and
  `TestCapturedZoneRecordsListServedViews` lists two identical
  `/lan nas.example.com` rows.
- `fix-api03.log`: `TestZoneRecordDeleteViewInOneNamespace` (store and
  command), `TestCapturedZoneRecordsListServedViews` (now with a `kids`
  record set holding a `lan` view of the same name and type),
  `TestZoneRecordWritesKeepViewsApart`, `TestZoneRecords`,
  `TestCapturedZone*` and `TestZoneCommandEndpoint*` pass.
  `fix-api04-race.log`: those plus `TestInventory*`, `TestOpenAPIDrift` and
  the forwarding tests pass with `-race -count=2` (56.5 s).
  `fix-api05-full-pkg.log`: `go test ./internal/api/...` passes (129.1 s).
  `TestInventorySaysWhenLeasesAreNodeLocal` (memory, SQLite standalone,
  primary, follower; auditor and operator) is new.
- `fix-mount01-race.log` (race, `./cmd/kudurru`):
  `TestRunServeSQLiteInventoryAPIAndLeaseMACs` (now also asserts
  `nodeLocalLeases` false on a standalone node),
  `TestRunServeKubeUniFiSourcesTwoReplicasAndPartition` and
  `TestCLIReferenceDrift` pass (108.3 s).
- `fix-lint01.log`: golangci-lint v2.15.0, 0 issues on `./internal/api/...`.
  `fix-docsgen01.log`: `hack/docsgen` passes. `fix-bench01.log`: 14 bench
  guard tests pass. `fix-gen01.log`, `fix-gen02-docs.log`: generators.
- `fix-tsc-eslint01.log`, `fix-tsc-eslint02.log`: tsc and eslint
  (`--max-warnings 0`) over `src` and `e2e`, exit 0.
- `fix-vitest03-without-fix.log` (retained, failed on purpose): with the
  product files reverted, 5 of the new tests fail.
  `fix-vitest04-caveat-without-fix.log` (retained): the auditor-on-primary
  caveat test fails without the fix (the mock viewer matrix grants
  `cluster.read`, unlike permissions.yaml, so the test answers GET /cluster
  with 403). `fix-vitest02.log`/`fix-vitest05.log`: RecordViews,
  InventoryPage and ZoneDetailPage, 43 passed. New vitest cases: the public
  "stored but not listed" warning (and its absence on a desired list), the
  retyped record set keeping or refusing the group, the helpers
  (`remapView`, `viewNamespaceError`, `listsServed`), a served list with
  `default/lan` and `kids/lan` rows (distinct links, delete sends
  `view=lan&namespace=kids` and no record set), and the auditor caveat.
- `fix-vitest06-full.log` (retained): full suite at a load average of about
  16 while lint and Go tests shared the machine: 1128 passed, 4 failed (three
  20 s timeouts in DomainsPage, InventoryPage paging and PolicyPages, and the
  App sign-in heading). `fix-vitest07-rerun-four.log`: those four files
  pass alone (115 tests). `fix-vitest08-full.log`: the full suite at a load average of about 10, 81 files and 1132 tests passed (258.7 s), so the `PolicyPages.test.tsx` timeout is load, not a defect.
- `fix-e2e01.log`: rebuilt bundle and licensetest binary, then
  `e2e/inventory.spec.ts --repeat-each=2`, 4 passed (33.4 s). The view test
  now also checks the served-list banner, the public warning in the editor,
  that the served view row carries `recordSet: ""` and `namespace:
  "default"`, and that the confirmation names "every record set of namespace
  default" (the real server accepts `?namespace=`).

Original implementation evidence:

- `api03-views.log` (race): `TestZoneRecordWritesKeepViewsApart` (store and
  SQLite command subtests; 1.89 s), `TestZoneRecords`, and the
  `TestZoneCommandEndpoint*` tests pass. `api04-mutation-without-view-identity.log`
  (retained, failed on purpose): without the view in `sameEndpoint`, both
  subtests fail (200 instead of 201 when the lan view is written).
  `api01`/`api02` are retained failures of the first test fixture: the
  command harness reads from another store.
- `api05-served-views.log` (race): `TestCapturedZoneRecordsListServedViews`
  and the three `TestCapturedZoneReads*` tests pass.
  `api06-mutation-without-served-views.log` (retained, failed on purpose):
  without the `ViewRRsets` block only the default row is listed.
- `mount01-race.log` (race, `./cmd/kudurru`):
  `TestRunServeSQLiteInventoryAPIAndLeaseMACs` (54.4 s, now asserts that the
  Integration summary's `inventorySource` equals the UniFi devices' source),
  `TestRunServeKubeUniFiSourcesTwoReplicasAndPartition` (68.1 s, both replicas
  name the same `integration-` source) and
  `TestIntegrationAdminActualRouterReadCheckAndRefusals` (8.9 s) pass.
- `race01-api.log`: `go test -race ./internal/api/` passes (451.8 s).
  `drift01-lint02.log`: `TestOpenAPIDrift`, `hack/docsgen` (TestReferenceDrift),
  `TestCLIReferenceDrift` and the 14 bench guard tests pass after the
  generated commit, and golangci-lint v2.15.0 reports 0 issues on
  `./internal/api/...`. `lint01.log`: 0 issues on `./internal/api/...` and
  `./cmd/kudurru/...` before the served-views fix.
- vitest: `vitest06-full.log` covers 81 files, with 1126 passed and 1 failed.
  The failure was `policyFields.test.tsx`, which asserted the old MAC help
  text on purpose. It was updated, and `vitest07-matchers-follower.log` shows
  it passing together with `follower.test.ts` (17 tests). `vitest08-full.log`
  (tsc, eslint over src and e2e, all files) shows 1126 passed and 1 failed: a
  20 s timeout in `PolicyPages.test.tsx` (policy wizard) at a load average of
  about 14, while the `internal/api` race run shared the machine. The same
  file passes alone (`vitest09-policy-alone.log`, 14 tests), and the test
  passed in `vitest06`. New tests:
  - `InventoryPage.test.tsx` (14): every state in words, the links, filters
    and URL, pages, viewer restricted with no request, auditor raw source,
    both 503s, the cluster caveat, the single node, follower and static,
    the Clients link, and the helpers;
  - `RecordViews.test.tsx` (9): options, namespace move, public, refused
    types, signed warning, fixed view on edit, helpers, the list badge with
    single-view delete keeping the default data, and adding beside the
    default data.
- Playwright against a real `kudurru serve` (licensetest binary, embedded
  bundle):
  - `e2e02.log`: `e2e/inventory.spec.ts` and `e2e/zones.spec.ts`, 5 passed
    (26.8 s). The inventory test (9.7 s) uses its own node and a webhook
    inventory peer (`e2e/support/inventorypeer.ts`, "clients" capability).
    The node polls it, the two devices that never queried are listed with
    MAC, Assigned, tags, Live and the Integration link, and the text and kind
    filters reach the server and the URL. The view test (10.4 s) adds a lan
    view beside a default A record through the editor, sees it listed once
    served, and deletes it alone.
  - `e2e04-pages.log`: `pages.spec.ts` (now listing `/inventory`),
    `navigation.spec.ts` and `inventory.spec.ts`, 83 passed (2.3 min).
  - Retained failures: `e2e01.log` (the view row never appeared, which led to
    the served-views fix) and `e2e03-pages-and-checks.log` (`pages.spec.ts`
    requires every navigation entry to be listed).

What remains open:

- The Playwright spec does not create a DHCP lease. The harness has no DHCP
  socket seam for a real binary, so lease devices, the Superseded and
  Ambiguous states and the cluster caveat are proven against the mock only;
  `nodeLocalLeases` is proven by the API unit test (faked store Info) and as
  false on the mounted standalone node, not on a mounted cluster.
- Stale devices are also mock-only: the webhook inventory lifetime is 3
  minutes from `lastSeen`.
- The view spec checks the listing and the editor, not per-group DNS answers.
  Those are covered by INT-GROUP-VIEWS.
- On the SQLite runtime, a public endpoint, or a group view the build
  refuses, is still not listed, so the dashboard cannot edit or delete it;
  it now says so, and the Tools download and the records API still reach
  it. Listing desired public endpoints on the served path was not done: that
  path deliberately lists only the served image.
- Served rows still carry no record set. A default-data row's Delete removes
  that name and type from every record set of the zone; a view row's Delete
  is limited to its group's namespace.
- The served-list detection (every row has an empty `recordSet`) relies on
  that API contract rather than an explicit flag.
- The record list on SQLite still does not poll after a save. A new record
  appears on the next read once it is served, as before this package.
- `kudurru records add/rm` have no `--view` flag. The API supports it.
- A client in several groups and the 16-view budget are not shown in the UI.
