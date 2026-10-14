# Final v0.1 feature review

Reviewed on 2026-10-14 from `700f2a1`, against the shipping claims in the
CHANGELOG, architecture tour, administrator guide and release checklist.
The branch was periodically rebased onto `origin/main`, most recently at
`a26e223`. That includes Edge PRs #50/#51, audit integrity and event details
(#55), cluster live tail and scope selection (#56), the collapsible nav
(#57), and portal downloads (#58). This review's fixes are integrated with
those changes; the separately owned Edge implementation is unchanged here.

## Shipping scope

| Area | v0.1 behavior | Review result |
|---|---|---|
| DNS | UDP/TCP, DoT, DoH, DoQ, DoH3; authenticated PROXYv2; ingress limits | Existing transport and forgery tests pass. Real-client acceptance remains a deployment gate below. |
| Client identity and policy | IP/CIDR groups, merged policies, schedules, safe search and blocked services | Listener identity is address-based. Client ID, MAC and hostname matchers have no production listener/inventory source. Release claims now say so. |
| Blocking and upstreams | Lists, allow/deny entries, CNAME blocking, block page and requests, upstream groups and conditional forwarding | Existing stage, policy, listfetcher and upstream suites cover these paths. |
| Cache | TTL, prefetch, serve-stale, persistence and flushing | Existing cache and pipeline tests cover serving and restart behavior. |
| Authoritative DNS | Primary and forward zones, records, AXFR/IXFR out, TSIG and NOTIFY out | Duplicate apexes previously prevented all zone views from rebuilding. The fix isolates the conflict and preserves unrelated zones. |
| Configuration and store | SQLite or Kubernetes, CRDs, settings reload, file objects, bootstrap and saved snapshots | An actual Kubernetes outage startup regression was reproduced and fixed, including deferred identity initialization. |
| Query log and statistics | Local logs, node/cluster reads, cluster live tail, rollups, privacy, retention, optional central PostgreSQL | Clients inventory previously ignored scope. It now uses the existing scoped reader and reports partial results; the dashboard sends the selected scope. |
| Dashboard and CLI | Object management, client/group pages, query log, charts, settings, nodes and tools | Client scope and chart labels are corrected. The nav footer shows the serving node's version and effective plan. Existing CLI coverage remains applicable. |
| Audit | Local, central and Kubernetes trails, redacted details/export and retained-trail integrity verification | Concurrent pruning could falsely report tampering during paginated local scans. Verification now uses a consistent read snapshot; key-retention instructions cover sealed batches. |
| Access and licensing | Local users, tokens, sessions, OIDC, Kubernetes RBAC, offline license verification | Identity store outages no longer prevent DNS startup. Existing credentials remain required; no offline IAM snapshot or authentication bypass is added. |
| Packaging | Helm/CRD charts, kustomize, compose, signed release artifacts and images | Local Helm render/schema checks pass; the baseline main commit's CI and kind workflows passed. Final-branch validation is recorded below. |
| Edge and VIPs | dnsdist/keepalived supervisors, discovery and VIP availability | Separately owned PRs #50/#51 are merged and included after rebase. Deployment identity and real failover acceptance remain required. |

Explicit deferrals remain: follower/raft stores, SQLite cluster membership,
DNSSEC, secondary/stub zones and RFC 2136, remote node health, cluster-wide
blocking toggles, remote Edge cache invalidation, IAM write pages and
transport client IDs. Cluster statistics and the cluster live tail are
already implemented and are no longer described as deferred.

## Confirmed client-address failure in Talos RC.4

The deployment in namespace `kudurru` uses Services `kudurru-dns-a` and
`kudurru-dns-b` with DNS VIPs `10.200.200.25` and `10.200.200.26`.
Both have `externalTrafficPolicy: Local`; their selectors and endpoints
point directly to kudurru's `dns` pods on port 53. A fresh read-only check
after the latest rebase still found three RC.4 DNS pods and no edge pods;
`kudurru-dns` also selects DNS pods.
This deployed path does not include dnsdist. When Edge is enabled, the
chart routes its published DNS Service to Edge, and extra VIP Services
must follow that configuration.

An authenticated cluster API read returned 100 recent query rows, all
with client `10.200.200.1`. The one-hour top-client response had that
address plus loopback probes. This establishes that the recorded data
already contains the gateway, before any dashboard rendering.

A 20-second capture on worker `10.30.50.91`'s external DMZ interface
`ens19` contained 39 packets to VIP `.25`, all with source
`10.200.200.1`. The simultaneous worker `.92` capture saw no VIP traffic.
The capture proves the address is already lost before Kubernetes receives
that traffic. It does not distinguish gateway DNS forwarding, interception
or source NAT, and it does not establish the behavior of idle VIP `.26`.

The network remedy is to preserve device source addresses on the route to
the VIPs, with a return route to the device networks. Changing API trusted
proxies or accepting an arbitrary DNS/HTTP client claim cannot restore an
address already removed upstream. For deployments intended to use Edge,
the extra published Services must also select the edge pods.

No production network, Service or workload was changed during this review.
The investigation used existing configuration, authenticated API reads and
packet captures. The [client-address runbook](../deploy/runbooks.md#clients-show-only-the-gateway)
contains the checks and remediation sequence.

## Product fixes

- `/clients` now honors local, selected-peer and cluster scopes, including
  the central database. It returns source/node/partial status without
  removing the existing list fields. Clients, client details, group member
  lists and the group wizard use the selected scope.
- The per-client chart recognizes the real API's explicit `other` column.
  It retains the individual device columns and labels the aggregate Other
  clients.
- The nav footer shows the running version below Nodes, including release
  candidate suffixes, alongside the effective license plan. It uses the
  serving node's metadata even when a peer is selected and hides the row
  when the nav is collapsed.
- A kube-backed node with a saved snapshot can open its DNS listeners
  while the apiserver is unavailable. Availability failures defer bootstrap
  and IAM initialization; unrelated errors retain their normal failure
  behavior. Deferred initialization retries after recovery.
  Completed credential bootstrap is not replayed during retries, preserving
  revocations; an unavailable configured OIDC provider keeps its ID reserved
  so stored providers cannot replace its trust configuration.
- AuditLog probe authorization failures warn and retain the retrying shipper
  rather than aborting DNS startup during an RBAC upgrade. Reads and writes
  still require authorization, and locally spooled events ship after access
  recovers without restarting the node.
- Duplicate canonical zone apexes no longer stop unrelated zone updates.
  A conflicting apex retains its previously served data; on cold start a
  deterministic metadata-name winner serves until the conflict is removed.
  Resolve the duplicate objects rather than relying on this fallback.
- SQLite and spool audit verification now hold one read snapshot across
  bounded pages, so retention and capped writes cannot create false broken
  chains between pages. PostgreSQL already uses repeatable read; Kubernetes
  verifies one consistent list response.
- The encryption-key rotation runbook now keeps the previous key while
  retained AuditLog batches still need its seal key, rather than discarding
  it after the session lifetime alone. Verification currently accepts only
  current/previous keys; further rotations must respect that limit.
- Release notes and follow-ups now describe the implemented seven-stage
  pipeline, IP/CIDR identity, cluster reads and live tail accurately.

## Validation

The whole-repository race run below passed after rebasing onto `a26e223`.
The subsequent audit snapshot fix has separate affected-package race and
process acceptance checks. A merged scope-label change required updating
one client-page test assertion; all other web tests passed in the full run,
and that client's five tests passed after the assertion update.

| Check | Result |
|---|---|
| Whole-repository Go race tests | 83 packages passed after the latest rebase, with Kubernetes 1.37 envtest and required PostgreSQL 17 integration. After the snapshot fix, affected audit/startup/clients/drift and built-bundle race tests also passed. |
| Go vet and golangci-lint | Passed on the final integrated source, zero issues. |
| Generated contracts and references | OpenAPI, Go/TypeScript clients and CLI/reference documents regenerated; drift tests passed. CRD/deepcopy generation also reproduced without drift. |
| Helm and deployment assets | Strict chart lint, render matrix, kubeconform and kustomize checks passed against the rebased Edge chart. |
| Web unit tests | 583 tests across 43 files validated: 582 passed in the full run; one old scope-label assertion was updated and all five Clients tests then passed. Nav metadata, scope, license and formatting coverage passed. |
| Web lint, typecheck and production bundle | Final nav source passed. Initial compressed load: 294.8 KiB against a 600 KiB budget; ordinary release binary with the real bundle: 62,419,104 bytes against 100,000,000 bytes. |
| Browser acceptance | All 133 real-binary Chromium tests passed against the latest merged bundle. Separate desktop, collapsed-rail, hover-flyout and mobile-drawer checks passed with no page shift or metadata overflow. Mock captures display v0.1.0-rc.1 / Personal. |
| Serve acceptance and BIND interoperability | Passed again on the final integrated source with actual processes: both central-database E2Es, three Kubernetes audit replicas and restart, internal peer isolation, SIGTERM drain, saved state, configuration/port failures, readiness, AXFR/NOTIFY/TSIG and conditional forwarding. |
| OIDC | Real Dex 2.46 login and mock-provider security cases passed; final stack regressions preserve revoked credentials and configured-provider trust during recovery. |
| Docs | Guide lint: 17 files, zero errors. Final Hugo build and internal HTML link checks passed (72 HTML documents). |
| Portal | The seven branded-email tests passed previously. The newly merged Downloads page and portal navigation passed 12 focused UI tests. The installed Bun cannot parse the portal's v2 lockfile; these UI checks used shared installed web dependencies with matching direct UI/test versions, without changing the lockfile. No portal deployment is certified. |

The two audit acceptance fixtures previously ran without the required
entitlement and failed with HTTP 402 once exercised. They now use the
existing test-only signing key and a synthetic license; ordinary release
binary coverage and production licensing behavior remain intact.

The first parallel rebased web run had three failures (two explicit test
timeouts); the complete suite passed with two workers. A Go embed check
overlapped bundle regeneration and was rerun successfully once the bundle
was stable. The original non-race query-log capacity probes exceeded their
timing budget under shared VM load; this review does not establish a product
performance regression. No production traffic-path change or real VIP
failover is certified by these local checks.

## Remaining GA gates

1. Correct the gateway path and verify two distinct device addresses in
   the cluster query log, Clients inventory and per-client chart for both
   VIPs over UDP and TCP. Check exact-IP policy isolation and selected-node
   views. Old gateway rollups are historical data, so use a fresh window.
2. Verify the separately owned Edge deployment, source identity through
   the authenticated PROXYv2 hop, supervisor health and the keepalived
   failover checks in the release checklist. PRs #50/#51 are merged in the
   reviewed source; the live deployment still runs RC.4 without Edge.
3. Complete the licensing/publication checklist, including the standby
   public signing key requested in open question 22. Only
   `kudurru-2026-1.pub` was present at the reviewed baseline; production
   private key custody and contact monitoring were not audited here.
4. Finish the release repository settings. The active `protect releases`
   ruleset requires review and CI, but still allows squash/rebase; its
   required-check list omits `promotion`, and no active branch ruleset
   refusing `v*` names was returned. These are owner-controlled settings;
   this review made no GitHub settings changes.

The review does not certify GA until the deployment and owner-controlled
gates pass. See [the release checklist](../release-checklist.md).
