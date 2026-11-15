### ID-INVENTORY Inventory API with MAC fill from DHCP leases — November 10, 2026 (D397)

The client inventory now has a read API, and the built-in DHCP server supplies
MAC addresses to `ClientGroup` `mac` matchers on both store drivers.

Every DHCP lease store snapshot, which is published only after a committed
transaction, now also holds the identity of each bound lease: address, MAC,
client hostname and expiry. It includes leases without a usable name and
scopes without `registerDNS`. On Kubernetes the working copies are filled from
`DHCPLease` objects, so every replica holds them. The identify stage asks
`dhcpdns.Resolver.LeaseMAC` for the live lease of the query's source address.
When the captured inventory supplied no MAC for the address, the lease MAC
fills it. Inventory MACs win. Two scopes that lease one address to different
MACs fill nothing. The fill clears the inventory source, so a source-bound
private MAC grant never matches a lease MAC. The read uses the request time,
touches only committed memory, and adds no store, SQL or leader dependency to
the DNS path.

A UniFi poll in which two clients claim one address with the same
observation time now passes the contested address to the inventory as a tied
claim on every claimant (also an expired one). Before, the UniFi translation
dropped the address from both clients, so the inventory listed them with no
address and no sign of the collision, and an older claim of another source
could still win the address. Now the inventory's tie rule identifies nobody at
the address, `Devices` lists it as `ambiguous` on each claimant, and no older
claim can take it. Forward and reverse records, names and group contributions
are still built only from the addresses a client owns outright.

The lease snapshot is local to the process. On Kubernetes every replica
watches all leases, so the lease MAC applies to queries answered by any
replica. On a SQLite cluster only the node whose DHCP service holds the lease
fills the MAC. The same client querying another node falls through to its
CIDR, hostname, client ID or default policy there, and can get a different
policy. `docs/deploy/client-identity.md` and `dhcp-dns.md` state this and
recommend an inventory integration or `ip`/`cidr` matchers for cluster-wide
effect.

`GET /api/v1/inventory` lists every inventory device, including devices that
never sent a DNS query: integration-source devices (UniFi, webhook) and the
answering node's bound leases (source `dhcp/<scope>`). Each device carries
MACs, names, tags, observation and expiry, `stale`, and a state for every
address: assigned, expired, ambiguous, superseded (with the owning source and
ID) or withdrawn. `inventory.Snapshot.Devices` computes these states from the
same claim map that `Lookup` uses. Leases follow the identify rule, so an
inventory MAC supersedes a lease MAC. The API has filters (`source`, `kind`,
`stale`, `q`) and keyset pages by (source, id). On the complete SQLite runtime
it reads the inventory owner of the request's original serving cut. On
Kubernetes it reads the shared producer holder that the identify stage reads,
so it shows whatever the F09B Kubernetes sources feed into that holder. The
route needs the new permission `inventory.read` (auditor, operator, admin; not
viewer, the same split as `dhcpleases.read`). Kubernetes identities map it to
the virtual resource `inventories`. `kudurru inventory` walks the pages. GET
/clients is unchanged and stays the query-log view.

Evidence under `.context/v020-final-20261110/evidence/ID-INVENTORY/`:

- `mount08-both.log` (no race detector): `TestRunServeSQLiteInventoryAPIAndLeaseMACs`
  passes in 30.8 s and `TestRunServeKubeLeaseMACSelectsGroupAndInventory`
  passes in 8.6 s. `race05-mounted.log` (race detector): they pass in 53.9 s
  and 17.7 s.
- The SQLite test runs the real serve command with the UniFi TLS double, the
  node's DHCP service bound to loopback sockets (`dhcp.Service.ListenWith`
  through the `listenDHCPService` seam), the query log, and the authenticated
  PROXYv2 UDP edge path. The DHCP server cannot lease loopback addresses, so
  leased clients use TEST-NET-1 addresses. The test shows:
  - UniFi devices are listed before any of them sends a query (generation > 0,
    official device `assigned`).
  - A dnsmasq lease import through `POST /node/dhcp/v4/scopes/lan/import`
    makes a `mac` group block `leased.test` over UDP for both leased
    addresses. It does not block an unleased address or a UniFi device.
  - The leases are listed with their MAC and hostname.
  - A lease that expires stops matching and is no longer listed as live.
  - `GET /clients` lists the addresses that queried, not the UniFi devices
    that never did.
  - In the UniFi conflict phase, both colliding devices list 127.0.0.2 as
    `ambiguous`, and the official device keeps its address.
  - After the controller fails, the official device turns `stale` with its
    address `expired` at the original expiry.
- The Kubernetes test (envtest) creates a `DHCPLease` object, which makes the
  `mac` group block over UDP (the block answer is REFUSED, because cluster
  settings come from the settings ConfigMap and the environment) for the
  leased address only. `GET /inventory` lists the lease on the replica.
  Deleting the object ends both the match and the listing.
- `race04-pkgs.log` (race detector, after the generated commit): `internal/api`
  (577 s, including `TestOpenAPIDrift`), `ee/rbac` (including
  `TestAggregatedRolesDrift`), `internal/rbac`, `internal/inventory` and
  `internal/servingbuild` (548 s) all pass. `race01-pkgs.log`:
  `internal/inventory/...`, `internal/dhcpdns` and `internal/stages/identify`
  pass under race. `internal/dhcp` passes under race in the focused runs.
- `race03-root.log` (race detector, root selection): 24 passes, including
  `TestDHCP*` (16), `TestNodeDHCPLeavesTheKubeStoreToDHCPKube`,
  `TestInventoryCommand`, `TestCLIReferenceDrift`, `TestConvenienceGolden`,
  `TestForwardedShapes` and the Kubernetes inventory test. Two failures are
  retained:
  - The SQLite inventory test failed because its first login got a 503 before
    a serving generation existed. The test now retries 503 for up to 30 s
    (commit "retry the mounted login").
  - `TestRunServeUniFiHostnameHeldPublicationDeadline` failed with
    `assignment_remaining` < 0. It failed the same way in `race05`. It passes
    without the race detector in this tree (`mount09-unifi-held.log`, 17.6 s,
    held policy retained in 366 ms). The same failure appears in other lanes'
    evidence before this package (`INT-UNIFI-PUSH/race07.log`,
    `INTEGRATE-1/oidc-repro-base-INVALID-ran-whole-suite.log`). The test has no
    DHCP service, so `LeaseMAC` returns false at once. I conclude that the
    failure comes from race-detector load, not from this package, but I did
    not run the base commit under race to confirm it.
- `drift01.log`: `go test ./deploy/ ./hack/docsgen/` pass after regeneration.
  `lint01.log`: golangci-lint v2.15.0 reports 0 issues on every touched
  package.
- New unit tests:
  - `TestLeaseMACFillsOnlyWithoutInventoryMACs`. A mutation check confirms it
    fails if the fill keeps the inventory source.
  - `TestDevicesExplainStalenessAndAddressOwnership`.
  - `TestResolverLeaseMACsAndListing`.
  - `TestLeaseBoundIdentityWithoutDNSRegistration`.
  - `TestServiceListenWithCallerSockets`.
  - `TestInventoryListsDevicesLeasesAndConflicts`: ambiguous inventory and
    DHCP collisions, an inventory MAC superseding a lease, stale flags,
    filters, pages, and 400/403/503 answers.
  - `TestInventoryCommand`, with goldens.
- `race01-pkgs.log` (retained, failed): `TestMeWithBearerRoles` counts one
  permission entry per resource. Adding `inventory` raised the count from 26
  to 27, and the expected count was updated. `TestOpenAPIDrift` failed before
  regeneration. `race02-api.log` (retained): only `TestOpenAPIDrift` failed,
  before regeneration.
- Retained failed mounted runs. Each one showed a test-fixture issue:
  - `mount01`/`mount02`: the DHCP config was rejected, first for an empty
    management value, then because a loopback subnet is not usable for DHCP.
  - `mount03`: UniFi device IDs have the form `<site>/<MAC>`. On Kubernetes,
    cluster settings ignore the in-memory upstream, so allowed and blocked
    answers were both NXDOMAIN.
  - `mount04`: the import was refused because the addresses were outside the
    pool.
  - `mount05`/`mount06`: the query log excludes special-use `.test` names by
    default, and the UniFi source withholds colliding addresses instead of
    passing an ambiguous claim to the inventory.
  - `mount07`: a device whose own claim had expired in a restored serving
    image was reported as `withdrawn`. Fixed in `inventory.Snapshot.Devices`
    (commit "report a device's own expired claim as expired"); the mounted
    test now checks for `expired`.
- Review fixes (`fix-*.log`):
  - `fix-01-unifi-collision-before.log` (retained, failed on purpose): the
    new `TestSamePollCollisionIsListedAmbiguous` fails without the UniFi
    change, because an older claim of another source identified the contested
    address. `fix-02-unifi-unit-race.log`: `ee/integration/unifi` and
    `internal/inventory` pass under race with it. `fix-04-ee-integration.log`:
    every `ee/integration/...` package passes. `fix-05-lint.log`:
    golangci-lint v2.15.0 reports 0 issues on `ee/integration/unifi`.
  - `fix-06-mounted-norace.log` (no race detector):
    `TestRunServeSQLiteInventoryAPIAndLeaseMACs` (40.2 s, now with the short
    lease at 20 s and asserted first),
    `TestIntegrationUniFiHostnameConflictAndOmissionCommit` (12.4 s),
    `TestRunServeUniFiHostnamePolicyLifecycle` (69.6 s) and
    `TestRunServeUniFiHostnameHeldPublicationDeadline` (23.6 s) pass.
  - `fix-07-mounted-race.log` (race detector): the SQLite inventory test
    (52.5 s), the Kubernetes lease test (10.3 s), `TestInventoryCommand` and
    `TestIntegrationUniFiHostnameConflictAndOmissionCommit` (41.0 s) pass.
    Retained failures, at a load average of about 16 on 8 cores:
    `TestRunServeUniFiHostnamePolicyLifecycle` (successor identity did not
    converge, after its conflict phase was accepted) and
    `TestRunServeUniFiHostnameHeldPublicationDeadline`
    (`assignment_remaining=-1.148s`). `fix-03-mounted-race.log` (retained):
    the same `assignment_remaining` failure in the lifecycle test, before the
    hostname test's conflict predicate was updated; the other selected tests
    passed.
  - To settle the earlier unconfirmed attribution: `fix-08-base-0d969081-unifi-race.log`
    runs the two UniFi hostname tests under race on the pre-package base
    (pass, 105.9 s and 43.3 s), and `fix-09-head-unifi-race.log` runs them
    at the new head with `-count=2` (all four pass: 117.9 s, 32.1 s, 105.6 s,
    69.8 s). Both pass when run alone at head. They fail only in longer
    selections on a loaded machine. That supports the load attribution, but a
    single base run does not prove it.
  - The hostname lifecycle test used to recognize the accepted conflict phase
    as "one address left in the durable source". The tied claims are now in
    the durable source, so it requires three addresses with two clients
    claiming 127.0.0.2. Its DNS assertions are unchanged: in that phase
    127.0.0.2 still identifies nobody.
  - `fix-10-bench-guards.log`: the bench guards pass (14 tests).
- `dash-typecheck01.log`: the dashboard typecheck could not run in this
  worktree because `tsc` is not installed (no `node_modules`). Only
  `generated.ts` changed there.

What remains open: the dashboard inventory view, its unit tests and the
Playwright spec belong to the later dashboard lane, and the GroupDetailPage MAC
tooltip still says only an inventory integration supplies MACs. The mounted
runs prove the lease-MAC precedence over an inventory MAC only in unit and API
tests: the UniFi double reports loopback addresses, which the DHCP server cannot
lease. The Kubernetes inventory devices come only from what feeds the shared
holder (the F09B lane). On a multi-node SQLite cluster, DHCP leases are per
node for both the listing and the MAC fill (documented, not changed). The
block page and unblock requests (`internal/blockpage.PolicyFinder`) still
identify the client by address only: they get no inventory or lease MAC,
hostname or client ID, so for a MAC- or name-selected client the policy shown
and the groups recorded on an unblock request can differ from what the DNS
answer applied. This gap predates this package for integration MACs; it is
documented in `docs/deploy/client-identity.md` and needs a follow-up that
gives `PolicyFinder` the identify stage's capture. No new proof was added for
CIDR ties or identity refresh (README D86). No query-path benchmark was rerun
for the per-query lease lookup, which loads one atomic snapshot per lease
scope when inventory gives no MAC.
