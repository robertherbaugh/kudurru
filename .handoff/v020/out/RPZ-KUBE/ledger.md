### RPZ-1 RPZ feeds on the Kubernetes driver — November 10, 2026 (D390)

On Kubernetes the RPZ manager was never created and the blocking stage had no
selector, so an RPZ BlockList source compiled to an empty list and was silently
unenforced. Every Kubernetes replica now runs the same `rpz.Manager` as SQLite,
with accepted copies in `local.db`. It acquires through a new per-feed
`CaptureZoneKeys` hook on the stored TSIG startup source and its hot replacement
(D314/D365), as `secondary.Options` already does: unsigned feeds are admitted
while stored keys are pending, and a ring replacement wakes the manager. The
manager is an every-node runnable, not leader-only.

`policy.ListInfo` now keeps the accepted BlockList it was compiled from. The
Kubernetes selector (`kubeRPZ`, `cmd/kudurru/kube_rpz.go`) takes the BlockLists
of each recursive request's own policy decision and selects them from one
immutable binding of the configuration snapshot, the node's feed snapshot and
the published TSIG ring. The binding is rebuilt only when one of the three
changes. Every input is an in-memory atomic read, so the DNS path does no store
read, needs no leader and does no network work. A feed that is missing, unbound,
bound to a rotated credential or expired stays selected as an unavailable
sentinel and answers SERVFAIL with EDE 14 "RPZ <origin> unavailable"; a stale or
unsynced configuration snapshot does the same, which, unlike primary zones,
also stops forwarding for every client whose policy selects an RPZ list. A
restart reuses an unexpired persisted copy without the primary; without the
apiserver only unsigned feeds are reused, because the stored TSIG keys are read
from the apiserver with no local fallback: a signed feed then fails closed for
all recursion under its policy until the pod restarts with the apiserver
reachable (a running pod keeps its ring through an outage). SQLite behavior is unchanged
apart from the `rpz` runnable now being registered outside the complete-runtime
branch (same name, same every-node scope).

Evidence under `.context/v020-final-20261110/evidence/RPZ-KUBE/` (all with
`-race`, KUBEBUILDER_ASSETS 1.37.0, run through the heavy-test lock):

- `race11.log`: `TestRunServeKubeRPZFeeds` PASS in 49.0 s and
  `TestKubeRPZBindingReuseAndFailClosed` PASS in 0.01 s. The envtest test runs two
  replicas against an in-process RPZ primary signed with a stored TSIG key and
  an in-process upstream. It checks, on both nodes and over UDP and TCP: the
  QNAME trigger (NXDOMAIN), the RESPONSE-IP trigger (NODATA for an answer of
  192.0.2.66), the CLIENT-IP trigger (NXDOMAIN only for source 127.0.0.2), and
  rpz-tcp-only (TC over UDP, an answer over TCP). It also checks that transfers
  were signed and that the follower runs its own `rpz` runnable. Then: a feed
  serial update is applied on both nodes; adding a second list whose primary is
  unreachable makes every query under the policy SERVFAIL with EDE
  "RPZ missing.test. unavailable" until the list is deselected; node A's query
  log has `blocked_rpz` rows for list `default/feed` and a `servfail_upstream`
  row for `default/missing`; after A stops, B takes the controller lease, keeps
  enforcing and applies another feed update; with the apiserver gate refusing
  every new request for 4 s (8 refusals observed) the full matrix still holds;
  with the primary unavailable, B restarts and enforces the unexpired copy at
  once over UDP and TCP; after expiry every query under the policy SERVFAILs
  with EDE "RPZ policy.test. unavailable" and a rule match is no longer
  answered; a fresh transfer recovers; B's query log holds the outcomes.
- Review fixes (`fix-*.log`, all `-race`, through the heavy-test lock):
  `fix-07.log`: `TestRunServeKubeRPZFeeds` and
  `TestKubeRPZBindingReuseAndFailClosed` with `-count=2`, 4 PASS (55.3 s and
  51.5 s; 11 and 9 apiserver refusals at the gate). The test now also restarts
  node B with the gate closed: once `kudurru_store_snapshot_serving 1` shows the
  fresh `local.db` copy, every query under the policy answers SERVFAIL with EDE
  "RPZ policy.test. unavailable" over UDP and TCP and stays so for 2 s, although
  the signed copy is unexpired, because the stored TSIG keys cannot be read; a
  restart with the apiserver reachable binds the feed again. The first restart
  check now waits up to 5 s instead of probing once.
  `TestKubeRPZBindingReuseAndFailClosed` now pins that a stale configuration
  snapshot leaves every selected feed unavailable; `fix-08-negative-control.log`
  shows it FAILS ("a stale configuration snapshot left the feed available") when
  the binding ignores staleness. `fix-10-unit.log`: `./internal/zonecache` and
  `./internal/rpz` PASS. `fix-11-neighbors.log`: `TestRunServeKubeOfflineSnapshot`
  and `TestRunServeKubeStoredTSIGManagementHotRotation` PASS,
  `TestRunServeKubeStoredTSIGStartupTransportsRestartAndOffline` FAILED on a
  listener port collision (`bind: address already in use`, in its own port
  choice, a file this package does not touch) and PASSED on rerun
  (`fix-12-neighbor-rerun.log`, 13.9 s). `fix-09-lint.log`: golangci-lint on
  `./cmd/kudurru/... ./internal/zonecache/...`: 0 issues.
  Retained failed runs: `fix-01.log`, `fix-02-debug.log`, `fix-03-debug.log`,
  `fix-04.log` failed the new offline phase when it probed once: the first answers
  after readiness were `servfail_unsynced` (no EDE) and then, after the snapshot
  served, forwarded. `fix-05-debug.log` and `fix-06-debug.log` (temporary trace,
  removed) show the sequence: ready after 0.87 s, then for about 100 ms the RPZ
  list was not yet in the policy and even `blocked.rpzlab.net` resolved NOERROR,
  then every answer was SERVFAIL with the EDE; `fix-06-debug.log` also failed
  the query-log check because the trace loop pushed older rows past the
  1,000-row read, so that check now runs before the offline restart.
- `race12-negative-control.log`: the same test with only the selector wiring
  removed FAILS at "node A enforces the QNAME trigger" (52.2 s), so the test
  fails without the change.
- `race13-neighbors.log`: `TestRunServeSQLiteStoredTSIGBackgroundTCP` (100.6 s),
  `TestRunServeSQLiteStoredTSIGBackgroundTLS`,
  `TestRunServeKubeStoredTSIGManagementHotRotation` and
  `TestRunServeKubeDynamicUpdateAtomicityAndOwnership`: 4 PASS, 134.3 s.
- `race14-kube-family.log`: `-run 'TestRunServeKube|TestRunServeOffline|TestRunServeDirectDNSEndpoint|TestKubeRPZ|TestRunServeSQLite.*RPZ|TestRunServeSQLiteComplete'`:
  20 PASS, 0 FAIL, 0 skip, 249.3 s (including the offline-snapshot,
  shared-login, blocking two-replica and DNSEndpoint partition tests).
- `unit01.log`, `unit02.log`: `./internal/rpz`, `./internal/policy`,
  `./internal/stages/blocking` PASS, including the new
  `TestAcquisitionZoneScopedCapture`.
- `lint01.log`: golangci-lint v2.15.0 on `./cmd/kudurru/... ./internal/rpz/... ./internal/policy/...`: 0 issues.

Retained failed and diagnostic runs: `race01.log` and `race02.log` failed
because the test's in-memory upstream was replaced when the shared-settings
refresh reloaded cluster settings from a missing node file (`race03.log` shows
the query answered by public DNS); the test now writes the node settings file
like the other Kubernetes mount tests. `race04.log` and `race05.log` failed at
"restart did not reuse the unexpired copy" with NOERROR. `race06-debug.log` to
`race10-debug.log` (temporary instrumentation, since removed) show the cause: for
about 250 ms after a node became ready, the policy index still reported
`blockListRefs "feed" is not compiled yet`. With the default
`blocking.allowDegraded: true` a node is ready and filters only with the lists
already loaded. This is the documented load window for every list, RPZ-only
lists included, and is not RPZ-specific (the SQLite path was not measured). The test sets `allowDegraded: false`,
which keeps the node not ready until the list is in the policy. `race08-debug.log`
also failed once on a UDP/TCP listener port collision; the test now probes a
port free for both.

Still open: a just-started Kubernetes node does not apply its RPZ feeds for the
short window before the RPZ-only list is compiled (about 250 ms after an online
restart, about 100 ms after an offline one); the feed is skipped, not failed
closed. With the default `blocking.allowDegraded: true` the node is ready in that
window; `allowDegraded: false` keeps it not ready on a normal start, but on the
offline restart the node was already ready while the list was missing, and the
listeners answer regardless of readiness. SQLite uses the same selection input.
Treating a referenced RPZ list that is known but not yet compiled as required
coverage on both drivers is a follow-up. A signed feed restarted without the
apiserver fails closed instead of reusing its persisted copy; reusing it would
need a design decision (for example binding by the stored credential
fingerprint). Reuse of an unsigned feed on an offline restart follows from the same
restore path but no mounted test runs an unsigned feed. The stale-snapshot case
is pinned by the binding unit test, not by a mounted run. DROP, PASSTHRU and local-data actions are not exercised on the
Kubernetes mount; they use the shared blocking stage and its adapter tests. On Kubernetes the local dnsdist edge cache
is not flushed when a feed changes (SQLite does not flush on feed changes either),
so an edge-cached answer can outlive a feed update by its TTL. Per-feed status
(`rpz.Manager.Statuses`) is still not exposed through the API, CLI or dashboard
(RPZ-2), and the Kubernetes dig explanation and policy list reads do not report
RPZ fields. No BIND interop run was made for the Kubernetes path; the in-process
primary uses the same `secondary.TransferClient` that the SQLite BIND interop
tests cover.
