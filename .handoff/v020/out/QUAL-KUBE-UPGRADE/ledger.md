### G22-WP3/WP4 Kubernetes upgrade from 0.1.0 and process-level outage, Lease handoff and restart — November 11, 2026 (D373)

A Kubernetes installation written by 0.1.0 now has upgrade evidence at three
levels, and the Kubernetes failure scenarios have a process-level test with
built binaries.

CRDs and stored objects (envtest, in-package). `TestEnvtestCRDUpgradeFromV01`
(internal/store/kube/upgrade_v01_test.go) installs the nine CRDs of the
v0.1.0 tag (the seven `kudurru.io` bases of the validator fixtures plus the
AuditLog and DHCPLease CRDs copied verbatim into
internal/store/kube/testdata/v0.1.0 with a provenance note) and writes every
0.1.0 sample the way the 0.1.0 driver wrote it (server-side apply as `api`,
status through the subresource), objects in the `default` namespace, a
kubectl-written placeholder stub zone, a placeholder secondary zone and a
free-form rewrite that 0.1.0 accepted and this release's schema refuses. A
field of this release is shown to be pruned by the 0.1.0 schema, which
proves the old CRDs are really installed. The CRDs are then replaced in
place with this checkout's and the five new ones are created: all 14 are
Established and NamesAccepted with the new schema and
`status.storedVersions == [v1alpha1]`. This release's driver lists and reads
every object with its UID, resourceVersion, generation, spec and status;
`UpdateStatus` works on every object, including the refused ones; an
unchanged-spec write of every API-managed object works (ratcheting); a
changed spec of a refused object is refused naming the rule until it is
corrected; writes with fields new in this release keep them. zonecache
writes local.db from these objects and a replica started with the API server
stopped serves all of them from that file, ready and authoritative. The test
waits for the published snapshot (State FromSnapshot and the expected zones
view), not for the cache state, because zonecache turns FromSnapshot before
it rebuilds the views.

Helm chart. `TestChartUpgradeFromV01` (deploy/upgrade_test.go) renders the
chart with five value sets (default two-replica kubernetes mode, standalone,
edge, hostNetwork, DHCP) and compares them with committed reductions of what
the v0.1.0 chart rendered (deploy/testdata/v0.1.0/chart, regenerated from
the tag with `-update`): every 0.1.0 object is still rendered under its name,
and workload selectors, the StatefulSet's serviceName, podManagementPolicy
and volumeClaimTemplates, Service clusterIPs, Secret types, data keys and
resource policy, and binding roleRefs are unchanged.

Built binaries, upgrade. `TestKubeUpgradeFromV01` (test/e2e/upgrade_kube_test.go)
runs the 0.1.0 binary built from the tag with the kube driver on an envtest
API server that has the 0.1.0 CRDs. Through its API it stores a zone, a record
set, records and two placeholder zones, mints break-glass and personal
tokens and ships HMAC-sealed AuditLog batches. It stops; the CRDs are
upgraded; `kudurru iam migrate status|plan|begin --offline` runs to
`complete` with `RemoteAllowed: true`; `kudurru audit keys init
--legacy-master-file --legacy-before` writes the ring, published as the
`kudurru-audit-keys` Secret; this release (licensed fixture binary, because
`audit verify` needs an audit license) starts on the same data directory with
`store.kube.expectedClusterID` pinned. The 12 answers (six names, UDP and TCP)
are unchanged, the bound tokens authenticate as the same principal, the
placeholder zones are still stored, a new record is served and `kudurru audit
verify` returns `verified: true` over the trail including the 0.1.0 batches.
The migration leaves exactly one token unbound: the first break-glass token
0.1.0 minted, because 0.1.0 created the `break-glass` service account in the
same request, so token and account share a creation second, which the
migration's strict issuance floor refuses to bind. The test requires 2 bound
and 1 unbound and a 401 for that token; docs/deploy/iam-upgrade.md now says so.

Built binaries, failure. `TestKubeProcessesColdStartLeaseLossAndRejoin`
(test/e2e/kube_cluster_test.go) runs two licensed built binaries with the
kube driver on one envtest API server, each through its own TCP relay, with
the cluster ID pinned. Partitioning the Lease holder hands
`kudurru-controller` to the other (15.5 to 18.5 s in the runs); while still cut
off, the former holder stops reporting itself as leader on /healthz and
never does again during the partition, while the new holder does; a change made
meanwhile reaches only the connected node while the partitioned one keeps its
last good answers, and after the heal both converge (about 0.5 s) under one
leader that the old holder does not reclaim. Both restart on their
directories and then on empty ones without losing the change. With the API
server cut off, a node restarted on its directory answers from local.db over
UDP and TCP about 5.3 s after start, is ready, reports the snapshot age and no
leader and keeps its API unavailable; a foreign pin, a snapshot without
identity and a first boot answer SERVFAIL with 503 and the reason in
/healthz. An unpinned node during an outage exits before it answers (no
admitted offline settings cache), so under Kubernetes it crash-loops for the
outage. That departs from G22-WP4 item 2, which asked for SERVFAIL and
unready: it is existing product behavior, not changed here, nothing wrong is
served, the test pins it, and docs/deploy/runbooks.md says so. The
integrator should record it as a follow-up (keep an unpinned node up and
unready). The first 0.1.0 break-glass token staying unbound is likewise a
user-visible upgrade behavior that needs the owner's explicit acceptance in
D373 clause 6; the docs now say "almost always", because the two 0.1.0 calls
can straddle a second boundary.

Evidence under `.context/v020-final-20261110/evidence/QUAL-KUBE-UPGRADE/`:

- `store01.log` to `store03.log`: retained failures while the test was
  written (the 0.1.0 schema refuses `rateLimitExempt` by apply, the CRD
  scheme was missing, the unchanged-spec write of the secondary zone added
  the Go type's empty policy and so changed the spec). `store04.log`:
  `TestEnvtestCRDUpgradeFromV01` PASS 10.87 s. `store05-race-package.json`:
  `go test -race ./internal/store/kube` full package, 204 passes, 3 skips (the
  opt-in `TestMeasureStore`, `TestMeasureFirstRequests`,
  `TestMeasureElectorProductionTimings`), 94.6 s. `store06-race.log`: the
  upgrade test under `-race` at the final head, PASS 11.36 s.
- `chart01-generate.log`: `TestChartUpgradeFromV01` with `-update`, five
  subtests PASS. `chart02-negative-selector.log`: negative control, a changed
  `app.kubernetes.io/component` selector fails all five subtests naming the
  field. `chart03.log`, `chart04-race.log`: PASS (0.58 s under `-race`).
- `e2e01-kube-cluster.log`: retained failure; the test expected an unpinned
  node in an outage to answer SERVFAIL and report unready, while it exits.
  `e2e02-kube-both.log`: cluster test PASS 102.02 s; upgrade test failed once
  waiting for `blocklists.kudurru.io` to report Established with stored
  versions `[v1alpha1]` within 30 s (not reproduced in the eight later runs; the
  wait now logs the last conditions it saw). `e2e03-kube-upgrade.log`: empty,
  the run was cut off by the machine restart. `e2e04-kube-upgrade.log`:
  retained failure, the first break-glass token got 401 after the migration.
  `e2e05-kube-both-race.log`: cluster PASS 100.78 s; upgrade failed with 1
  bound and 1 unbound token after only moving the tokens a second after the
  admin user, which showed the unbound token is the break-glass one.
  `e2e06-kube-upgrade-race.log`: upgrade PASS 37.35 s.
- `e2e07-kube-both-race-count3.log` (test binary at 0f3e5069, `-race
  -count=3`, `KUDURRU_REQUIRE_ENVTEST=1 KUDURRU_REQUIRE_PREVIOUS_RELEASE=1`,
  previous binary sha256 f049295c... identical to the harness's own tag
  build): `TestKubeProcessesColdStartLeaseLossAndRejoin` PASS three
  consecutive times (108.65, 98.16, 96.48 s), `TestKubeUpgradeFromV01` PASS
  three times (31.29, 28.99, 31.28 s), zero skips, 407.7 s.
- `e2e08-kube-upgrade-legacy-zones.log` (the placeholder zones of 42634d30):
  `TestKubeUpgradeFromV01` PASS twice (40.96, 30.99 s).
- Review of the first cut: `review-store01.log` FAIL and `review-store02.log`
  (`-count=4`) one FAIL of four, both `zones from local.db map[]`: the test
  read the snapshot right after the cache state turned FromSnapshot, before
  the views were rebuilt. `review-chart01.log` and `review-e2e01.log` (both
  e2e tests `-race -count=2`, 4/4 PASS, 279 s) PASS.
- After the fixes (d1007ee4, 4f761d2b, 2fad1dad):
  `fix-store01-race-count10.log`: `TestEnvtestCRDUpgradeFromV01` under
  `-race -count=10` with `KUDURRU_REQUIRE_ENVTEST=1`, 10/10 PASS, 109.3 s,
  load average about 1.8. `fix-e2e01-kube-cluster-race-count2.log`:
  `TestKubeProcessesColdStartLeaseLossAndRejoin` with the fencing check,
  `-race -count=2`, PASS twice (110.15, 101.87 s; Lease acquired 17.8 and
  18.5 s after the partition), 222.7 s. `fix-lint01.log`: golangci-lint
  v2.15.0 over ./test/e2e/... ./internal/store/kube/...: 0 issues.
- `lint01.log`: golangci-lint v2.15.0 over ./test/e2e/... ./internal/store/kube/...
  ./deploy/...: 0 issues.

Still open: the optional `UPGRADE_FROM=0.1.0` phase of hack/kind-e2e.sh was
not added (kind is not installed here and it needs ghcr.io pulls of the
published 0.1.0 images and charts), so no container-level `helm upgrade`
under dig load has run; the chart check compares renders only. The child
servers of the process tests are CGO-disabled and not race-instrumented. The
upgraded node in the process test is the `licensetest` build, not the
unlicensed release binary, because `audit verify` needs an audit license.
The upgrade covers one replica; a rolling two-replica upgrade from 0.1.0 is
not tested (D373 states the upgrade is offline whenever IAM Secrets or
AuditLog batches exist, which every installation that used the API has).
Hosted runs of these tests have not happened yet.
