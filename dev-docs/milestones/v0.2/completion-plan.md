# v0.2 continuing completion plan

Baseline: PR #54 at `6fe025e`, 2026-10-31. This working plan keeps every
original F subrequirement and G gate visible while implementation continues.
The [audit](followup-audit.md) supplies the exact acceptance requirement;
[gaps.md](gaps.md) carries detailed execution evidence. Historical audit
statuses are not current completion claims. “Implemented; final gates open”
means the named behavior exists; G21–G24 still apply. No requirement is
removed by this plan. F64c retains its original explicit v1.0 subtarget.

All GitHub workflows on the baseline pass, including
[CI](https://github.com/robertherbaugh/kudurru/actions/runs/41182647688) and
[security](https://github.com/robertherbaugh/kudurru/actions/runs/41182647671).
The user has excluded the understood Cloudflare docs check from this work.
Later pushes require their own hosted results.

## Execution and dependencies

The parent integrates reviewed signed-off commits on the existing PR branch,
maintains design/contracts/evidence, and runs composed checks. Three isolated
agents initially own F01, F47 and F04; the parent owns F64a. Freed agents take
the next independent package, retaining original proofs, ownership, current
transactions and final credential/license checks. No refused mutation replay.

1. Close F01, F47, F04 and F64a; reconcile shared config and startup seams.
2. Run independent packages F72, F29b, F22a, F06 and F70, then the remaining
   bounded DNS, operational, IAM and observability items in the table below.
3. Complete HA production coordination and forwarding before cluster CLI,
   static membership, driver-wide mutation and multi-process qualification.
   Complete integration-host wiring before provider administration and identity
   acquisition acceptance. Complete durable TSIG/security authority before
   encrypted-listener admission and AD acceptor/ownership integration.
4. Complete recursive DNS caching/anchor/cookie packages with external RFC
   interoperability and bounded-work evidence; retain all transport/cache and
   client-policy isolation requirements.
5. Complete AD discovery, principal/record permissions, aging, migration and
   rollback, followed by real Windows/DC/member/DHCP acceptance. A pending
   request asks for disposable Windows and two-VM keepalived lab hostnames and
   roles. Lab scope is not inferred from silence or existing production access.
6. Finish all public contracts, upgrade/failure, performance/interoperability
   and deployment/release gates on the final artifacts. Publishing a release,
   changing release branches or deploying to production is not part of this work.

External decisions F03, F23 and F50 need explicit documented dispositions;
where implementation can satisfy the original requirement, do that work.
Fixed-class performance and Windows/two-VM acceptance require their named
runners. Unit tests cannot substitute for those results.

## Complete subrequirement inventory

| ID | Requirement | Workstream | Current work state |
|---|---|---|---|
| F01 | Cold-start cluster-ID check when kube has never reached the API | Store and HA | Active: cluster identity agent |
| F02a | SQLite remaining §11.4 audit columns, including authz path | Store and HA | Queued: missing |
| F02b | SQLite secret redaction in before/after | Store and HA | Queued: partial |
| F02c | SQLite cluster epoch / primary fencing setters | Store and HA | Implemented; final gates open |
| F02d | `InternalKinds` once internal kinds exist | Store and HA | Implemented; final gates open |
| F03 | `WatchOptions.ResourceVersion`: replay-as-Added vs true resume | Store and HA | External acceptance/decision open |
| F04 | Shared sqlite/kube table rejecting the same inputs | Store and HA | Integrated d4075f5; scoped real-driver races pass |
| F05 | Cache parsed endpoints per record-set object | Zones and AD | Queued: partial |
| F06a | Separate NOTIFY TSIG key field in ZonePolicy | Zones and AD | Queued: missing |
| F06b | Per-zone `notifySource` | Zones and AD | Queued: missing |
| F07a | Encrypted store-backed internal TSIGKey | Zones and AD | Queued: missing |
| F07b | TSIG on DoH/DoQ/DoH3 | Zones and AD | Queued: missing |
| F08 | `doctor` self-NOTIFY / egress-address round trip | Zones and AD | Queued: missing |
| F09a | Per-integration elector, rather than one global Runner lease | Zones and AD | Implemented; final gates open |
| F09b | Production integration-host wiring and failover | Zones and AD | Queued: partial |
| F10a | Reload inline objects without restart | Zones and AD | Queued: missing |
| F10b | `Policy/default` as explicit fallback | Zones and AD | Implemented; final gates open |
| F11a | Server-side Kerberos/GSS-TSIG/TKEY feasibility implementation | Zones and AD | Queued: partial |
| F11b | Real Windows DC/member/DHCP; SPN provisioning/rotation; edge/VIP routing | Zones and AD | External acceptance/decision open |
| F12a | Atomic RFC 2136 storage for each advertised driver | Zones and AD | Queued: partial |
| F12b | Principal/record ownership and AD permission translation | Zones and AD | Queued: missing |
| F12c | Imported-record adoption for legitimate AD registrants | Zones and AD | Queued: partial |
| F12d | Durable content/security/aging replication and fenced failover | Zones and AD | Queued: partial |
| F13a | AD discovery/import metadata and blocker reports | Zones and AD | Queued: missing |
| F13b | Shadow, final write handoff, reconciliation, lossless rollback | Zones and AD | Queued: missing |
| F14a | Windows DHCP, enabled name protection and shared credentials | Zones and AD | External acceptance/decision open |
| F14b | Aging/scavenging with static-record protection | Zones and AD | Queued: missing |
| F14c | AD administration/diagnostics/backup and restore | Zones and AD | Queued: partial |
| F15 | Windows acceptance with every DC DNS service stopped | Zones and AD | External acceptance/decision open |
| F16 | Current-hour query stats beyond 100k distinct names | DNS and resolver | Queued: missing |
| F17 | `$important` / `$badfilter` | DNS and resolver | Implemented; final gates open |
| F18a | `control.allowedNetworks` / `control.blockedQTypes` config | DNS and resolver | Queued: missing |
| F18b | Reverse-PTR hostname cache | DNS and resolver | Queued: missing |
| F18c | UniFi hostname source | DNS and resolver | Queued: partial |
| F19a | Durable replicated blocking toggle on complete SQLite path | Store and HA | Implemented; final gates open |
| F19b | Kube runtime annotation publisher/watcher or equivalent cluster toggle | Store and HA | Queued: missing |
| F20a | AdGuard black-box conformance implementation | DNS and resolver | Implemented; final gates open |
| F20b | Real AdGuard/container qualification and CI evidence | DNS and resolver | Integrated; final gates open |
| F20c | Move benchgate to `hack/` | DNS and resolver | Integrated; final gates open |
| F20d | Lazy-DFA regex engine | DNS and resolver | Queued: missing |
| F21a | `upstreams.importFile` dnsmasq import | DNS and resolver | Queued: missing |
| F21b | Response-cache inspect REST | DNS and resolver | Queued: missing |
| F21c | Cache/strategy benchmark limits | DNS and resolver | Queued: partial |
| F22a | QUIC max connections / TCP queries / idle timeout config keys | DNS and resolver | Implemented; actual transport/config/mounted races pass |
| F22b | DoH `Alt-Svc` | DNS and resolver | Queued: missing |
| F22c | RFC 8467 response padding | DNS and resolver | Queued: missing |
| F22d | DoH path client IDs | DNS and resolver | Implemented; final gates open |
| F23 | Separate public blocked-services/safe-search data repo (D120) | DNS and resolver | External acceptance/decision open |
| F24a | Rate-limit slip metric | DNS and resolver | Integrated; final gates open |
| F24b | Per-ClientGroup rate-limit exemptions | DNS and resolver | Queued: missing |
| F25a | Multi-socket DoT | DNS and resolver | Queued: missing |
| F25b | Multi-socket DoQ/DoH3 with connection-ID-aware routing | DNS and resolver | Queued: missing |
| F26a | Privacy reload preserves daily pseudonym identity | DNS and resolver | Queued: partial |
| F26b | Cache growth retains initial shard count | DNS and resolver | Queued: partial |
| F26c | Blocking refresh/degraded settings hot reload | DNS and resolver | Queued: partial |
| F26d | Querylog enabled/sink hot reload | DNS and resolver | Queued: missing |
| F26e | `auth.*` hot reload | DNS and resolver | Queued: partial |
| F27 | RFC 8198 aggressive NSEC/NSEC3 cached synthesis | DNS and resolver | Queued: missing |
| F28 | RFC 5011 automatic trust-anchor updates | DNS and resolver | Queued: missing |
| F29a | Expiring negative trust anchors for forwarded validation | DNS and resolver | Implemented; final gates open |
| F29b | Negative trust anchors for recursive groups | DNS and resolver | Queued: missing |
| F30 | DNSSEC validation of forwarded answers | DNS and resolver | Implemented; final gates open |
| F31 | DNS cookies to authoritative servers | DNS and resolver | Queued: missing |
| F32 | Persist node-scope API changes or remove write half | Operations and UI | Queued: partial |
| F33 | Remote `GET /nodes/{name}/health` | Operations and UI | Integrated; final gates open |
| F34 | `/auth/me` exposes zone-scoped permissions | IAM and access | Implemented; final gates open |
| F35a | Read external-dns `DNSEndpoint` CRs directly | Zones and AD | Queued: missing |
| F35b | `kudurru externaldns-webhook` sidecar subcommand | Zones and AD | Queued: missing |
| F35c | external-dns webhook authentication | Zones and AD | Integrated; final gates open |
| F36 | Serve object lists from live snapshot instead of store | Operations and UI | Implemented; final gates open |
| F37 | Adopt generated OpenAPI Go client underneath CLI client | Operations and UI | Queued: missing |
| F38 | Dedicated SQLite authz path/request/client audit columns | Observability and release | Queued: missing |
| F39a | Local edge resolver-cache invalidation | Edge and VIP | Implemented; final gates open |
| F39b | Remote edge blocking-generation pull / resolver invalidation | Edge and VIP | Queued: missing |
| F40a | Host-only `edge.*` / `vip.*` settings keys | Edge and VIP | Queued: missing |
| F40b | Keepalived provider events to audit/SSE in serve | Edge and VIP | Queued: missing |
| F41a | ServiceProvider current Owner | Edge and VIP | Queued: missing |
| F41b | Chart provider annotations | Edge and VIP | Queued: partial |
| F42 | Per-VRID `--hmac-key-file` in chart | Edge and VIP | Queued: missing |
| F43a | dnsdist console backend diff-apply | Edge and VIP | Queued: missing |
| F43b | `reloadAllCertificates()` without restart | Edge and VIP | Queued: missing |
| F43c | Console spike (a) gates both changes | Edge and VIP | External acceptance/decision open |
| F44a | Persist grant revocation across restart/regrant | IAM and access | Implemented; final gates open |
| F44b | Cluster-wide production use on every advertised driver/route | IAM and access | Queued: partial |
| F45a | Stored OIDC identities with independent UID | IAM and access | Implemented; final gates open |
| F45b | External per-user disable / log out everywhere administration | IAM and access | Queued: partial |
| F46 | Share login lockout counters across replicas | IAM and access | Queued: missing |
| F47 | Bound offline Kubernetes SA tokens or periodically TokenReview | IAM and access | Active: service-account agent |
| F48a | `autoProvision: never` and `syncGroups` with user store | IAM and access | Implemented; final gates open |
| F48b | RP-initiated logout | IAM and access | Queued: missing |
| F49 | Optional external-dns shared-secret header / tokenFile | IAM and access | Integrated; final gates open |
| F50 | Separate Kubernetes vs Kudurru SA binding subject or document overlap | IAM and access | External acceptance/decision open |
| F51 | Optional HTTPS-only catalogue and BlockList downloads | Operations and UI | Queued: missing |
| F52 | Dig answer reports matched exclusion, avoiding second request | Operations and UI | Queued: missing |
| F53 | Query-time exact ClientGroup scope for allow/deny lists (D143) | Operations and UI | Queued: partial |
| F54a | Regex “overrides block list” explanation | Operations and UI | Queued: missing |
| F54b | Regex allow/deny cache invalidation | Operations and UI | Queued: partial |
| F55 | Per-digest index for paginated URL entries | Operations and UI | Queued: missing |
| F56 | Self-host Inter/JetBrains Mono in Kudurru theme | Operations and UI | Queued: missing |
| F57 | Nested Shape help for list/map elements/keys | Operations and UI | Integrated; final gates open |
| F58a | Astryx record-delete AlertDialog | Operations and UI | Implemented; final gates open |
| F58b | `/queries/export` API and query-log link | Operations and UI | Queued: missing |
| F59 | `--context` chooses kubeconfig/direct CRD verbs | Operations and UI | Queued: missing |
| F60a | Offline `import adguard` / `import blocky` | Store and HA | Integrated; final gates open |
| F60b | `cluster status` / `promote` / `join-token` | Store and HA | Queued: missing |
| F60c | `integrations …` CLI | Store and HA | Queued: missing |
| F60d | Zone option writes via PUT/PATCH | Store and HA | Queued: partial |
| F61a | CLI browser loopback + PKCE login | IAM and access | Queued: missing |
| F61b | CLI device-code login | IAM and access | Queued: missing |
| F62a | CLI conformance against actual API router | Operations and UI | Queued: partial |
| F62b | §10 documents ApplyResult errorKey/hint, wrapped settings, SSE event | Operations and UI | Queued: partial |
| F63 | Fixed-class benchmark job, including UDP sockets and Persist200k | Observability and release | External acceptance/decision open |
| F64a | `kudurru_cluster_fenced` metric/dashboard/alert | Store and HA | Implemented and scoped tests pass; final gates open |
| F64b | `/healthz` snapshotAge / leader | Store and HA | Implemented; composed kube outage check pending F01 integration |
| F64c | DNSSEC signing-key expiry metric | Store and HA | Original v1.0 subtarget; not a v0.2 closure |
| F65 | `cluster.role: static` / Compose read-only HA membership | Store and HA | Queued: missing |
| F66 | Separate querylog.db / local.db path keys and volumes | Observability and release | Queued: missing |
| F67 | E2E-2 static SQLite cluster, peer list bodies, correct leader | Store and HA | Queued: partial |
| F68a | AdGuard Home black-box e2e | Zones and AD | Integrated; final gates open |
| F68b | Knot catalog consumer interoperability | Zones and AD | Queued: missing |
| F68c | Two-VM keepalived failover acceptance | Zones and AD | External acceptance/decision open |
| F69 | Central reads for retired node via scope=node&node=other | Observability and release | Queued: missing |
| F70 | Prune unreferenced central dictionaries | Observability and release | Queued: missing |
| F71 | Summarize facts of permanently stopped sources' last partial hour | Observability and release | Queued: missing |
| F72a | Diagnosis page: missing AuditLog CRD and object-count recommendation | Observability and release | Active: diagnostics agent |
| F72b | Same AuditLog checks in `kudurru doctor` | Observability and release | Active: diagnostics agent, after F72a |
| F73 | AuditLog verification-key lifecycle independent of session rotations | Observability and release | Queued: missing |
| F74 | Central availability/shipping lag/audit-spool Grafana panels | Observability and release | Integrated; final gates open |
| F75a | `GET /audit/verify` using existing verifiers | Observability and release | Implemented; final gates open |
| F75b | `kudurru audit verify` | Observability and release | Integrated; final gates open |

## Original review gates

| IDs | Current state and remaining closure |
|---|---|
| G01–G03 | Implemented and validated existing mutations; preserve regressions. |
| G04 | Baseline GitHub CI/security green; repeat on integrated final head. |
| G05–G15 | All 11 feature packages remain open until their F rows and original additional package acceptance pass. |
| G16–G19 | AD discovery/security/DHCP/aging/cutover and Windows acceptance remain open. |
| G20 | Inventory complete: 75 original rows, 130 subrequirements, retained above. |
| G21 | Per-feature production wiring, API/CLI/UI/schema/permissions/docs and original-proof behavior. |
| G22 | Supported-driver upgrade, mixed-version rejection, cold start, restart, promotion, partition and rejoin. |
| G23 | Actual BIND/Knot/AdGuard/dnsdist/Windows results plus fixed-class bounds; no skipped comparator credited. |
| G24 | Applicable release checklist, Compose/Kubernetes, two-VM VIP, licensing and artifact checks; no release publication implied. |
| G25 | Keep this plan, audit closures, gap ledger, changelog and PR consistent with exact commits/results. |
