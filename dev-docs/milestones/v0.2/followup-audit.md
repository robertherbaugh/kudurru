# v0.2 follow-up evidence audit

## Owner decision: independent-VM VIP acceptance deferred, November 9, 2026

No disposable Linux VM pair is available. The owner explicitly moved F68c's
independent two-VM keepalived acceptance to follow-up, alongside the AD lab work
(D358). It remains unqualified, with its original lameduck, drain, node-loss and
restart requirements retained. It is outside the current completion scope; no
VM assignment or provisioning question remains pending. Existing container
keepalived evidence and kind/MetalLB acceptance cover their actual environments.
Kind or minikube on one host does not establish independent-host VRRP/L2 failure
behavior. Available container/Kubernetes checks and other non-AD work continue.

## Owner decision: AD remains preview, November 9, 2026

The owner confirmed that no AD lab is available and explicitly deferred the
remaining AD feature work and real Windows acceptance to a future agent.
AD1–AD12, the AD-specific portions of F11–F15, and G16–G19 remain **preview**:
they are unfinished and unqualified, not passed, and are outside the current
non-AD completion scope. Their original requirements and evidence remain below
for the future handoff. No Windows lab provisioning is pending in this work.
Shared RFC 2136, TSIG, supported-driver, HA and general release requirements
remain in scope independently of AD. Linux/Samba evidence cannot establish
Windows compatibility or justify production AD DNS replacement. See D353.


Audited 2026-10-31 at `b42fa24` for PR #54. This is the substantive G20
inventory against [followups.md](../../followups.md) and the original
[G05–G24 review](gaps.md). It does not change either document's requirements,
retarget work, certify a release, or close a whole feature package.

F01–F75 identify the **75 rows whose Target cell includes v0.2**, in their
original section/order at this commit. The original section headings and
unique item descriptions below preserve the source identity; suffixes split
bundled requirements. Keep these IDs even if the source ledger is reordered
or its completed rows are later deleted. Cross-references are to the G IDs in
the original review. All entries also inherit G21's public-contract gate.

Statuses:

- **Implemented**: concrete source and relevant test evidence were inspected.
  This describes the named subrequirement, not every deployment or package.
- **Partial**: a foundation exists; named behavior, wiring, or coverage remains.
- **Missing**: the inspected implementation seam does not supply the requirement.
- **External**: hardware, operator acceptance, or an explicit owner decision is
  required. Existing local code is described separately where appropriate.

All test paths below are **static evidence, not claims that tests ran** in
this audit. No Go, browser, container, interoperability, or benchmark suite
was run for this documentation change. Scoped `git log -- <path>` supplied
implementation commits where identifiable. Previously reported commands in
README or milestone prose are not used as fresh execution evidence.
Negative findings identify the current seam and the acceptance still needed;
they are scoped inspection findings, not proofs about every possible program.
Parent-owned remote node-health, offline AdGuard/Blocky CLI, hosted CI fixes,
and desired-zone publication-wait diagnosis were active separately and are
not credited as landed at this baseline.

## Store and drivers

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F01 | Cold-start cluster-ID check when kube has never reached the API | Missing | `cmd/kudurru/serve.go` accepts saved data when `Store.Info().ClusterID` is empty; `internal/store/kube/kube.go` discovers the ID from `kube-system` online. `cmd/kudurru/serve_offline_test.go` exercises age/outage, not an independent identity source. | Supply an independently trusted expected ID and test wrong-cluster offline startup and upgrade compatibility; G11, G22. |
| F02a | SQLite remaining §11.4 audit columns, including authz path | Missing | `internal/store/sqlite/audit.go: AuditEntry/sqlInsertAudit` still has `Reason`, without dedicated authz/request/client columns; `internal/store/sqlite/schema.go` migration 2 is replication authority, not the proposed audit migration. | Add a new migration without reusing migration 2; preserve chain verification and existing data; G13, G22. Also F38. |
| F02b | SQLite secret redaction in before/after | Partial | `internal/audit/redact.go` redacts exported documents; `internal/store/sqlite/audit.go: insertAudit` caps raw documents, without calling that redactor. `internal/audit/sanitize_test.go` is not at-rest redaction proof. | Decide/store safe diffs before persistence, including private kinds, without changing existing hash verification; tests inspect stored audit bytes and export; G12, G13. |
| F02c | SQLite cluster epoch / primary fencing setters | Implemented | `internal/store/sqlite/authority.go`, `authority_transition.go`, `command.go`; `authority_transition_test.go`, `command_checks_test.go`; `5079fa7`, `acb124c`. Persisted authority and elected-writer command checks supersede bare setters. | Retain production promotion/partition and mixed-version qualification; this is not consensus; G06, G11, G22. |
| F02d | `InternalKinds` once internal kinds exist | Implemented | `internal/store/sqlite/sqlite.go: Options.InternalKinds`; `internal/store/kube/kube.go`; `internal/store/kube/runtime_kinds_test.go`; private types in `api/internalkinds/v1alpha1/`. | Re-run safe snapshot/private-state migration acceptance on final supported drivers; G12, G22. |
| F03 | `WatchOptions.ResourceVersion`: replay-as-Added vs true resume | External | `internal/store/kube/watch.go` explicitly accepts RV but replays current informer state as Added, followed by bookmark/live events; `internal/store/kube/crud_test.go` covers watch behavior. | Owner must settle whether documented level-triggered replay satisfies the requirement. True event-history resume is not implemented; do not silently relabel it complete; G21, G22. |
| F04 | Shared sqlite/kube table rejecting the same inputs | Missing | `internal/store/conformance/conformance.go` lists CRUD/watch/status/ownership cases; `internal/store/sqlite/conformance_test.go` and `internal/store/kube/conformance_test.go` invoke it. `internal/store/validator/*_test.go` supplies validator coverage separately. | Add shared named invalid fixtures asserting structured rejection and no mutation on both production drivers, using scoped envtest; G06, G21, G22. |

## Zone, controller, objects

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F05 | Cache parsed endpoints per record-set object | Partial | `internal/controller/cache.go` caches whole zone views/builds by zoneVersion; `zoneview.go` rebuilds affected zones from record sets. `internal/controller/measure_test.go` supplies performance fixtures. `6b9bbe0` adds expiry-aware rebuilds. | Per-object parsed RR reuse with detached copies and identity/TTL/expiry invalidation is not present; measure changed-record reconciliation separately from cached whole-zone reuse; G05, G23. |
| F06a | Separate NOTIFY TSIG key field in ZonePolicy | Missing | `api/v1alpha1/dnszone_types.go: ZonePolicy` has transfer keys and NOTIFY targets; `internal/primaryruntime/network/notify.go: notifyPolicy` derives keys from transfer policy. `notify_test.go` covers signed delivery. | Add explicit key selection, ownership/admission and rotation semantics, including fallback compatibility; G05, G06. |
| F06b | Per-zone `notifySource` | Missing | Same public ZonePolicy and `internal/zone/notify.go` / `internal/primaryruntime/network/notify.go`; no per-zone source-address setting. | Validate/render the source and prove actual egress selection over supported IPv4/IPv6 transports; G05, G23. |
| F07a | Encrypted store-backed internal TSIGKey | Missing | `internal/dnsserver/tsig.go` explicitly implements the seam using config-backed StaticKeyring; `internal/config/tsig_test.go`, `internal/dnsserver/tsig_test.go` test file/config keys. | Durable encrypted kind, schema, rotation/current-key exclusion, backup and publication wiring; G05, G06, G12, G22. |
| F07b | TSIG on DoH/DoQ/DoH3 | Missing | `internal/dnsserver/tsig.go` explicitly treats signatures on these listeners as unsigned; native UDP/TCP/DoT verification exists in `dns.go`. | Verify signed request and response handling on each listener; preserve original proof bytes and deadlines; G05, G06, G23. |
| F08 | `doctor` self-NOTIFY / egress-address round trip | Missing | `cmd/kudurru/doctor.go` and `internal/api/doctor.go` do not perform this diagnostic; working NOTIFY tests are in `internal/primaryruntime/network/notify_test.go`. | Bounded authenticated self-round-trip, address reporting and actionable timeout/routing errors; G05, G15, G23. |
| F09a | Per-integration elector, rather than one global Runner lease | Implemented | `ee/integration/host/lifecycle.go` obtains `Elector(electionKey(id))` per instance and joins old callbacks; `lifecycle_test.go`, `source_sqlite_test.go`; `7d653e7`, `0a0e5a5`. | This closes the host mechanism only; retain elected original token through final effects; G09. |
| F09b | Production integration-host wiring and failover | Partial | The host exists above; `cmd/kudurru/serve.go` mounts external-dns separately and its OIDC provider host is a different subsystem. No production integration host assembly was found there. | Mount provider execution with durable ledger/allocations and prove independent-instance lease loss/replacement; G09, G22. |
| F10a | Reload inline objects without restart | Missing | `internal/objects/loader.go` stores `Options.Inline`; its watcher reloads `Dir` and reuses that original slice. `watch_test.go` covers directory changes. | Re-read the inline source as one validated input; retain last-good owned objects on parse failure; G15, G22. |
| F10b | `Policy/default` as explicit fallback | Implemented | `internal/policy/index.go` separates `fallback` from sorted active policies and selects it only after active selection; `internal/policy/index_test.go`. `internal/bootstrap/bootstrap.go` seeds the default. | Preserve the explicit fallback contract when changing defaults/namespaces; no need for another priority workaround; G07, G15. |

## Active Directory DNS replacement (D222)

The [AD outline](../../active-directory-dns.md), AD1–AD12, remains the scope.
A read-only import, shared-key TSIG, a Samba-only probe, and permanent DC
forwarding cannot close these rows. Windows DNS must be stopped on **every
DC** for final qualification. Static/imported ownership, DHCP name protection,
security/aging replication and rollback retaining new writes remain required.

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F11a | Server-side Kerberos/GSS-TSIG/TKEY feasibility implementation | Partial | `dev-docs/spikes/ad-dns/README.md`, isolated `dev-docs/spikes/ad-dns/` module and `.github/workflows/ad-dns.yml`; `f1085d5`, `71807c2`, `310edc8`. Probe verifies identities and signs refusals; it does not persist UPDATEs. | Production acceptor and reviewed permissive dependency/cgo-free build evidence; G17, G23. |
| F11b | Real Windows DC/member/DHCP; SPN provisioning/rotation; edge/VIP routing | External | AD outline §§5, 9, 10 expressly records no Windows lab; isolated probe has independent MIT/nsupdate coverage only. | Actual Windows registrations, signed replies, encryption/key versions, rotation, KDC outage and routed context recovery with tested OS/topology; G17, G19, G23, G24. |
| F12a | Atomic RFC 2136 storage for each advertised driver | Partial | `internal/primaryruntime/sqliteadapter/`, `internal/store/sqlite/command.go`, `internal/zonestate/`; SQLite runtime mounted in `cmd/kudurru/serve.go`; `cmd/kudurru/serve_notify_mount_test.go`. Kubernetes head/chunk primitives are not equivalent to mounted full lifecycle acceptance. | Concurrent file/API/UPDATE writers, failed prerequisites/persistence and recovery on each supported mode; G06, G17, G22. |
| F12b | Principal/record ownership and AD permission translation | Missing | `internal/zonestate/` has update ownership primitives; `api/v1alpha1/dnszone_types.go` has shared-key policies. No production Kerberos principal/AD ACL translation is supplied by the refusing probe. | Stable principal identity, value-level shared DC records, protected names, reviewed ACL replacement/translation and negative cross-principal tests; G17/AD5–AD7. |
| F12c | Imported-record adoption for legitimate AD registrants | Partial | Generic current migration adoption exists in `internal/api/migration_current_adopt.go` and `internal/api/migration_current_adopt_test.go`, `d702f96`; this preserves API-managed ownership, UID/spec/records and adds migration-run annotations plus a journal; it does not grant AD record ownership. | Reviewed metadata/grace adoption that cannot let first writer steal protected/static/imported records; G16, G17. |
| F12d | Durable content/security/aging replication and fenced failover | Partial | Complete owned replication exists in `internal/store/replication/`, `internal/serving/`; `internal/servingbuild/acceptance_test.go` exercises actual images. AD aging/security payload and per-mode guarantees remain absent. | Acknowledged-update loss guarantee, ownership/aging backup/restore, stale writer refusal and Windows failover acceptance; G11, G18, G22. |
| F13a | AD discovery/import metadata and blocker reports | Missing | Existing migrations are Pi-hole/Technitium in `internal/importers/` and `cmd/kudurru/import.go`; BIND records in `cmd/kudurru/zones.go`. AD outline §§3–4 requires data those imports do not acquire. | Inventory directory partitions, real zone boundaries/locator/reverse data, ACL/aging omissions, unsupported views and signing blockers, repeatable dry run; G16/AD1–AD2. |
| F13b | Shadow, final write handoff, reconciliation, lossless rollback | Missing | Generic migration command/journal and adopted object settings are foundations (`internal/api/migration_command.go`, `migration_current_import.go`, `f4e3416`), not an AD migration coordinator. | Quiesce/reconcile concurrent source writes, publish complete snapshots, explicit write ownership, fence/export post-cutover updates before rollback; G16, G19/AD12. |
| F14a | Windows DHCP, enabled name protection and shared credentials | External | `internal/dhcp/` implements Kudurru DHCPv4; this is not evidence for Windows DHCP DNS registration. AD outline §6 requires option 81 and DHCID/prerequisite paths. | Real Windows DHCP renewal/release/address change/failover with negotiated forward/reverse responsibilities and name protection enabled; G18/AD8, G23. |
| F14b | Aging/scavenging with static-record protection | Missing | `internal/zonestate/` updates and record expiry do not implement AD no-refresh/refresh state. TTL expiry is not scavenging. AD outline §§6–7 defines the missing model. | Same-data refresh timestamps, static/imported protection, preview/execution parity, audit/recovery and metadata convergence; G18/AD9–AD10. |
| F14c | AD administration/diagnostics/backup and restore | Partial | General records/zone diagnostics and store backup exist; AD-specific owner/aging/registration/permission inspection is not furnished by `internal/api/` or the probe. | Distinguish SPN/clock/authz/prerequisite/storage failures; preserve security/lifecycle state in restore; G18/AD11. |
| F15 | Windows acceptance with every DC DNS service stopped | External | `dev-docs/active-directory-dns.md` §9 supplies the matrix, not execution results. | Real domain/DC/member lifecycle, health, DHCP, security, aging, registration bursts, outage, recovery, rollback; publish versions and guarantees; G17–G19, G22–G24. |

## Dataplane

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F16 | Current-hour query stats beyond 100k distinct names | Missing | `internal/querylog/sqlitelog/stats.go` and its distinct-query helpers retain fact/distinct paths; `internal/querylog/sqlitelog/measure_test.go` supplies measurement coverage. | Shorter per-domain rollups or bounded sketch with explicit exact/approximate output; rerun 1M-row workload and boundary correctness; G13, G23. |
| F17 | `$important` / `$badfilter` | Implemented | `internal/blocking/modifier.go`, `context_policy.go`; `modifier_test.go`, `modifier_acceptance_test.go`, `testdata/conformance/modifier-context.txtar`; `292fdb9`. Includes precedence/cancellation and selected-list context. | Run final conformance and scale/edge-policy isolation qualification; parser slow-rule classification alone is not lack of compiler support; G07, G23. |
| F18a | `control.allowedNetworks` / `control.blockedQTypes` config | Missing | `internal/stages/control/control.go` has options, but `cmd/kudurru/serve.go` passes `control.Options{}`; no corresponding keys in `internal/config/schema.go`. | Typed validation, actual capture/publication wiring and listener acceptance, preserving request-cut semantics; G07, G21. |
| F18b | Reverse-PTR hostname cache | Missing | `internal/policy/index.go: HostnameResolver`; serve wires `leaseDNS` in `cmd/kudurru/serve.go`. No reverse-PTR acquisition/cache source supplies it. | Bounded asynchronous acquisition with expiry/ambiguity and no query-path live lookup; G10. |
| F18c | UniFi hostname source | Partial | `ee/integration/unifi/inventory.go`, `acquisition_test.go`; `internal/servingbuild/inventory_test.go` tests durable captured inventory. DHCP `leaseDNS` wiring exists; integration-host production acquisition remains F09b. | Mount source fill and prove hostname/nameGlob decisions, stale removal, collisions and native/edge equivalence; G09, G10, G22. |
| F19a | Durable replicated blocking toggle on complete SQLite path | Implemented | `cmd/kudurru/serving_blocking_commands.go`, `serving_blocking_commands_test.go`, `serve_blocking_mount_test.go`; `10b749c`. Canonical `Kudurru.status.blocking` and absolute expiry replace a bare local timer. | Keep final expiry/authority/publication and follower-install acceptance; do not add a second legacy write; G07, G11, G22. |
| F19b | Kube runtime annotation publisher/watcher or equivalent cluster toggle | Missing | `internal/stages/blocking/toggle.go` supplies node-local Apply/Publisher seams; `cmd/kudurru/serve.go` wires local callbacks. No production `kudurru.io/blocking-disabled-until` publisher/watch was found in `internal/store/kube/`. | All kube replicas observe a durable authorized change and the same absolute expiry across restart; do not replay refused complete commands; G07, G11, G22. |
| F20a | AdGuard black-box conformance implementation | Implemented | `internal/blocking/blackbox_test.go: TestAdGuardBlackBox`, `292fdb9`: manually authored cases drive an external official binary; explicitly skips unless configured. | Harness existence does not establish execution or full corpus parity; F20b; G07, G23. |
| F20b | Real AdGuard/container qualification and CI evidence | External | No AdGuard binary configuration was found in `.github/workflows/ci.yml`; harness requires an external executable. It is not a container runner as originally requested. | Pin official program/image and record unskipped corpus run/version/results in a permitted runner; no copyleft source/data copying; G23, G24. |
| F20c | Move benchgate to `hack/` | Missing | `internal/blocking/benchgate/` remains; `Makefile: bench` invokes it there. | Move without changing gate rules/output and update callers; run its focused tests; G15, G23. |
| F20d | Lazy-DFA regex engine | Missing | `internal/blocking/matcher/regex.go` uses Go regexp/RE2 with a required-literal gram index and bounded candidate scan, not lazy DFA. `internal/blocking/matcher/matcher_test.go` tests that matcher. | Implement/qualify lazy-DFA semantics and memory/work bounds, or obtain an explicit recorded scope decision; indexed RE2 is not silent satisfaction; G07, G23. |
| F21a | `upstreams.importFile` dnsmasq import | Missing | `internal/config/schema.go`, `internal/upstream/config.go` do not expose it; existing import commands target zones/live migrations. | Parse supported dnsmasq routing into canonical groups with loss/unsupported reports and tests; G15. |
| F21b | Response-cache inspect REST | Missing | `internal/api/runtime.go` registers cache flush; `internal/api/stats.go` exposes upstream status. Neither supplies cache-entry inspection. | Bounded permissioned inspection, privacy and captured-owner contract; G07, G15, G21. |
| F21c | Cache/strategy benchmark limits | Partial | `internal/cache/bench_test.go`, `internal/upstream/bench_test.go` exist; `bench/limits.txt` gates blocking/zone lookup, not these benchmarks. | Add calibrated limits/baselines on chosen fixed class and run them; G23. |
| F22a | QUIC max connections / TCP queries / idle timeout config keys | Missing | `internal/dnsserver/listener.go: Limits` and `transports_test.go` implement/test code-level defaults and options; `internal/config/schema.go` lacks corresponding operator keys. | Validated configuration to actual listener limits, including zero/default cases; G07, G15, G21. |
| F22b | DoH `Alt-Svc` | Missing | `internal/dnsserver/doh.go`, `doh3.go` do not emit the header. | Advertise only configured reachable HTTP/3 endpoints; test HTTPS/proxy/path behavior; G07, G23. |
| F22c | RFC 8467 response padding | Missing | Padding tolerance in `internal/dnsserver/transports_test.go` is request parsing coverage; no response-padding policy in `internal/dnsserver/` / `internal/stages/`. | Correct transport/size policy with truncation/privacy/interoperability tests; G07, G23. |
| F22d | DoH path client IDs | Implemented | `internal/dnsserver/doh.go`, `doh3.go`, `internal/config/schema.go` HTTP `clientIDs`; `internal/clientid/` and transport tests; `cd76bba`. | Retain policy-selector/non-authentication distinction and edge propagation acceptance; G10, G23. |
| F23 | Separate public blocked-services/safe-search data repo (D120) | Complete | Owner-authorized public [kudurru-data](https://github.com/robertherbaugh/kudurru-data) at `13d8d42b504246867dea420a8ad0e04ff5043776`; Apache-2.0, 150 services, seven safe-search engines. | Published Git tree and pinned HTTPS bytes match the validated bundle; 104 manifest entries and consumer race tests pass. D333 and docs/deploy/blocked-services.md pin provenance/update behavior; G07/G24 retain their other requirements. |
| F24a | Rate-limit slip metric | Missing | `cmd/kudurru/serve.go: limiter.SetHook` counts only VerdictDrop; `internal/stages/ingress/ratelimit.go` and `ratelimit_test.go` already implement slip. | Counter once per slip, bounded labels, distinguish allow/drop, focused UDP acceptance; G07, G13. |
| F24b | Per-ClientGroup rate-limit exemptions | Missing | `api/v1alpha1/clientgroup_types.go` lacks exemption; ingress uses global prefix exemptions (`internal/stages/ingress/ingress.go`). | Define precedence with early ingress policy identification; preserve abuse bounds and captured groups; G07, G10. |
| F25a | Multi-socket DoT | Missing | `internal/dnsserver/dns.go: socketCount` explicitly returns one for DoT; `socket_test.go` covers listener socket behavior. | TLS listener reuse-port behavior, resource bounds and measured gain; G07, G23. |
| F25b | Multi-socket DoQ/DoH3 with connection-ID-aware routing | Missing | `internal/dnsserver/doq.go`, `doh3.go` bind individual QUIC listeners. | Connection affinity/migration, cross-socket handshake and bounded routing design plus throughput evidence; G07, G23. |
| F26a | Privacy reload preserves daily pseudonym identity | Partial | `cmd/kudurru/reload.go` creates `PrivacyFrom(next)`; `internal/querylog/privacy.go` draws an in-memory key when absent. Existing privacy/reload tests do not establish same-key reload continuity. | Explicit key lifecycle with tests for unchanged transform vs privacy changes/day rollover/restart; do not disclose keys; G13, G22. |
| F26b | Cache growth retains initial shard count | Partial | `internal/cache/cache.go: Reconfigure` intentionally retains shard count; `reconfigure_test.go` tests resize. | Decide acceptable performance bound or rebuild safely; benchmark large growth and in-flight captured owners; G07, G23. |
| F26c | Blocking refresh/degraded settings hot reload | Partial | `internal/config/schema.go` still flags refresh/degraded restart; fetcher constructed once in `cmd/kudurru/serve.go`. Complete serving owns compiled snapshots separately. | Distinguish capture-time behavior from fetch scheduling; update scheduler safely and demonstrate real refreshed publication without restart; G07, G22. |
| F26d | Querylog enabled/sink hot reload | Missing | `internal/config/schema.go` marks restart; `cmd/kudurru/reload.go` updates privacy/retention, not sink/async lifecycle. | Bounded drain/replacement with no DNS blocking, record duplication or lost local tail guarantees; G13, G22. |
| F26e | `auth.*` hot reload | Partial | Durable captured IAM/provider state and current commands exist (`internal/auth/providerhost/`, `internal/servingauth/`), while session/local-login keys still have restart metadata in `internal/config/schema.go`. | Enumerate per-key actual supported changes and safely mount remaining host/session controls; prove revoke/restart/follower behavior and final original deadlines; G12, G22. |

## Recursive resolver (D170)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F27 | RFC 8198 aggressive NSEC/NSEC3 cached synthesis | Missing | `internal/recursor/denial.go` verifies denial for an answer; `cache.go` caches query answers/delegations. That is not a reusable validated-range synthesis cache. | Answer new covered names/types without upstream traffic, with TTL/signature expiry, wildcard/delegation/opt-out bounds and no bogus reuse; G07, G23. |
| F28 | RFC 5011 automatic trust-anchor updates | Missing | `internal/recursor/roots.go` embeds DS anchors; `resolver.go` accepts configured replacements. No hold-down/add/revoke state machine or persisted RFC 5011 state. | Durable state transitions, restart/time rollback, revoked/unsupported keys and safe operator recovery; G07, G22, G23. |
| F29a | Expiring negative trust anchors for forwarded validation | Implemented | `internal/config/dnssec.go`, `internal/dnssec/options.go`; `internal/stages/dnssec/dnssec_test.go: TestNTAExpiryAndCachePersistenceRetainTrustScope`; `9d7bd79`. | Preserve absolute expiry/cache scope; this evidence does not close the recursive D170 requirement; G07. |
| F29b | Negative trust anchors for recursive groups | Missing | `internal/recursor/resolver.go: Config` has trust anchors, not NTAs; `internal/stages/dnssec/dnssec.go` preserves independent native recursive provenance. | Wire bounded expiring recursive exceptions; prove isolation from forwarded/default validation, expiry on cache hits, reload and durable restore; G07, G22. |
| F30 | DNSSEC validation of forwarded answers | Implemented | `internal/dnssec/validate.go`, `internal/stages/dnssec/dnssec.go`; `dnssec_test.go` covers AD/CD/DO, negative proof, bogus refresh and cache epochs; `9d7bd79`. | Final native/edge interoperability, tampered replies and cache isolation qualification; G07, G23. |
| F31 | DNS cookies to authoritative servers | Missing | `internal/recursor/transport.go` does query exchange; no EDNS COOKIE generation/server-cookie cache/BADCOOKIE recovery there. Stripping forwarded client cookies (`internal/upstream/set.go`) is separate. | RFC 7873/9018 cookie generation/retry, per-server rotation and spoof/error bounds; G07, G23. |

## API (dev-docs/milestones/m3/api.md)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F32 | Persist node-scope API changes or remove write half | Partial | `cmd/kudurru/serving_node.go`, `internal/api/node_command.go`; `serving_node_test.go`, `serve_users_mount_test.go`; `4ce376b`. Current raw-proof command admits local log leaves, then adopts memory/logger state; receipt explicitly promises no durable local transaction. | Choose a durable overlay/reset/host precedence policy or explicitly remove writes; prove restart readback and atomic failure without nesting a legacy write; G15, G22. |
| F33 | Remote `GET /nodes/{name}/health` | Missing | `internal/api/nodes.go: nodeHealth` still returns 501 for known remote peers; `nodes_test.go` documents local/unknown cases. | Parent's active remote-health work owns implementation; require bounded internal auth, unknown/timeout/partial behavior and permission tests; G13, G21. |
| F34 | `/auth/me` exposes zone-scoped permissions | Implemented | `cmd/kudurru/serving_http_sessions.go: sessionInfo` fills `ScopedPermissions` by normalized role scope; legacy `internal/auth/stack/routes.go` does too. `cmd/kudurru/serving_http_scopes_test.go`, `internal/auth/stack/security_test.go`; `b8579a8`. | Preserve token-scope intersection; add/retain explicit mounted `/auth/me` scoped-response acceptance, not just internal authorization tests; G12. |
| F35a | Read external-dns `DNSEndpoint` CRs directly | Missing | `ee/integration/externaldns/externaldns.go` is an HTTP webhook server; no direct endpoint reconciler in `cmd/kudurru/serve.go`. | Watch/cache/reconcile DNSEndpoint with permissions, ownership, deletion and fencing; G09, G21. |
| F35b | `kudurru externaldns-webhook` sidecar subcommand | Missing | `cmd/kudurru/cli.go` / `zones.go` register no such command; serve embeds the webhook when configured. | Dedicated bounded lifecycle/config/listener command with actual router/CLI contract coverage; G09, G15. |
| F35c | external-dns webhook authentication | Missing | `ee/integration/externaldns/externaldns.go` warns on unauthenticated non-loopback listening. | Shared-header feature is F49; its addition does not itself grant current write admission; G09, G21. |
| F36 | Serve object lists from live snapshot instead of store | Implemented | `internal/api/objects.go` chooses captured owner; `object_reads.go`, `object_reads_test.go`; `92bf51b`. Original complete image supplies objects and generation-bound cursors. | Preserve exact original publication, no store/successor fallback, consistent permission scopes and supported kube wiring; G15, G21, G22. |
| F37 | Adopt generated OpenAPI Go client underneath CLI client | Missing | `internal/apiclient/client.go` explicitly remains handwritten; generated `internal/apiclient/openapi/` exists separately. | Migrate transport without losing cookie/CSRF, SSE, pagination, errors, prefixes or cancellation; conformance against actual server; G15. |
| F38 | Dedicated SQLite authz path/request/client audit columns | Missing | Same physical schema gap as F02a; `internal/api/audit_attribution_test.go` verifies attribution but does not add storage columns. | One scoped storage migration should close both rows, preserving reason compatibility and old hash chains; G13, G22. |

## Edge and VIP

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F39a | Local edge resolver-cache invalidation | Implemented | `ee/edge/dnsdist/flush.go`, `flush_test.go`; `cmd/kudurru/serve.go` wires toggle and compiled-list callbacks. | Preserve existing local behavior and response-policy invalidation; G14. |
| F39b | Remote edge blocking-generation pull / resolver invalidation | Missing | `ee/edge/dnsdist/supervisor.go: pollZones` invalidates authoritative cache on serial changes; no equivalent resolver blocking-generation path. | Authenticated original generation, flush on change/expiry/reconnect and retry bounds across Deployment/DaemonSet; G07, G14, G23. |
| F40a | Host-only `edge.*` / `vip.*` settings keys | Missing | `internal/config/schema.go` lacks these namespaces; existing supervisors have CLI flags (`cmd/kudurru/edge.go`, `vip.go`). | Typed host settings and precedence/render validation for existing deployment modes; G14, G15. |
| F40b | Keepalived provider events to audit/SSE in serve | Missing | `ee/edge/vip/keepalived.go` provides a seam; `cmd/kudurru/vip.go` runs a supervisor. `cmd/kudurru/serve.go` does not mount `vip.NewKeepalived` event forwarding. | Emit authorized, bounded `cluster.vip.changed` / `vip.master_changed` with correct node/VRID transitions, restart/no duplicate behavior; G13, G14. |
| F41a | ServiceProvider current Owner | Missing | `ee/edge/vip/service.go: Owner` unconditionally returns unknown; `Events` nil; `provider_test.go` covers stub behavior. | Supported Service/announcer observation and stale/unknown handling for MetalLB/kube-vip; G14, G24. |
| F41b | Chart provider annotations | Partial | `ee/edge/vip/service.go: Annotations` exists; `deploy/helm/kudurru/templates/service.yaml` independently renders provider address/pool/sharing annotations. | Existing announcements are not missing; reconcile behavior with provider helper and validate rendered supported topologies; G14, G24. |
| F42 | Per-VRID `--hmac-key-file` in chart | Missing | `deploy/helm/kudurru/templates/edge.yaml` still passes one `/etc/kudurru/secrets/vrrp/vrrp-hmac`; multi-VIP keepalived code exists separately. | VRID-indexed secret mounts/flags, missing-key refusal, rotation and rendered chart/Compose acceptance; G14, G24. |
| F43a | dnsdist console backend diff-apply | Missing | `ee/edge/dnsdist/supervisor.go` drains/restarts on membership changes; `supervisor_test.go` verifies that lifecycle. | Owner's real console/YAML spike first; then newServer/setDown/rmServer update/failure ordering, health and rollback; G14, G23. |
| F43b | `reloadAllCertificates()` without restart | Missing | Same supervisor watches TLS files and restarts after config checking. | Real supported dnsdist console acceptance plus key/cert replacement, errors, readiness and active-session behavior; G14, G23. |
| F43c | Console spike (a) gates both changes | External | Original D17 and `dev-docs/release-checklist.md` retain external-program acceptance; no new console execution was done here. | Record exact dnsdist version, YAML directives and safe command/failure results before closing F43a/b; G23, G24. |

## Auth

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F44a | Persist grant revocation across restart/regrant | Implemented | `internal/auth/security/state.go: InvalidateGrant`, `objects.go`; `state_test.go: TestBindingNarrowRegrantAndRestartCannotReviveSession`; `internal/servingauth/current_test.go: TestPersistedRevocationRestartAndActualReplicaPromotion`; `15acbd8`. | Keep current command and original proof deadline checks; G12, G22. |
| F44b | Cluster-wide production use on every advertised driver/route | Partial | Complete SQLite auth images carry durable grant floors; legacy BindingSource lives in `internal/rbac/engine.go` and kube auth assembly differs. | Final public IAM writer, supported kube startup/migration and multi-replica revocation acceptance; do not infer from isolated SQLite test; G12, G21, G22. |
| F45a | Stored OIDC identities with independent UID | Implemented | `internal/auth/security/external.go`, `internal/servingauth/security.go`; `external_test.go`, `provider_test.go` include independent identity, staged disable, restart/promotion and token UID cases; `4aca7c2`. | Preserve provider/issuer/subject identity and frozen-grant intersection; stale row saying no store is incorrect; G12. |
| F45b | External per-user disable / log out everywhere administration | Partial | Persisted external identity has Disabled and account floors; `cmd/kudurru/serving_http_users.go` administers local users. No complete external-user administration surface established by the model alone. | Current authorized external-account admin operations with UID guards, atomic token/session revocation, API/CLI/UI and restart/promotion coverage; G12, G21, G22. |
| F46 | Share login lockout counters across replicas | Missing | `internal/auth/users/limiter.go` is an in-memory map; `cmd/kudurru/serving_http_oidc.go` also uses local limiters. | Explicit shared rate policy and bounded store-backed counters if retained in scope; test alternating replicas/restart/clock changes without logging identities/secrets unnecessarily; G12, G22. |
| F47 | Bound offline Kubernetes SA tokens or periodically TokenReview | Missing | `internal/auth/k8s/k8s.go: verifyJWKS` verifies claims/expiry and caches by token; successful JWKS verification does not check deleted pod/SA or bound `exp - iat`. | Pick explicit maximum lifetime or periodic review/outage policy, then deletion, malformed/future iat, overlong token, cache and outage tests; G12, G22. |
| F48a | `autoProvision: never` and `syncGroups` with user store | Implemented | `internal/servingauth/provider_test.go: TestExternalNeverAlwaysGrantFloorsAndDefaultSemantics`; `internal/auth/security/external.go` retains initial groups and invalidates on synchronized removal; `external_test.go: TestExternalSyncFloorsPreventRemovalRegrantAndUsernameRevival`; `4aca7c2`. | Complete-image route uses this model; legacy `internal/auth/stack/stack.go` still rejects never, so qualify advertised kube modes separately (F44b); G12. |
| F48b | RP-initiated logout | Missing | `cmd/kudurru/serving_http_sessions.go` / `internal/auth/session/handlers.go` perform local logout. No provider end-session redirect/ID-token-hint flow established in `internal/auth/browseroidc/`. | Trusted discovery endpoint, safe post-logout redirect/state, provider errors and local revocation/cookie clearing; G12, G23. |

## Security (dev-docs/milestones/m5/security.md)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F49 | Optional external-dns shared-secret header / tokenFile | Missing | `ee/integration/externaldns/externaldns.go` has no auth; `internal/config/schema.go` lacks `externalDNS.webhook.tokenFile`. Duplicate security-specific requirement of F35c. | Opt-in secret-file validation before handling; absent/wrong token refusal; bounded rotation semantics, no secret output, preserve disabled mode; G09, G21. |
| F50 | Separate Kubernetes vs Kudurru SA binding subject or document overlap | External | `internal/rbac/subjects.go: SubjectMatches` intentionally matches namespace/name for both providers; no provider discriminator. | Owner chooses schema separation or explicit security documentation in §8.5; tests cover same-name identities and scoped token behavior for chosen contract; G12, G21. |

## Security (dev-docs/milestones/m5/security-v0.1-round.md)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F51 | Optional HTTPS-only catalogue and BlockList downloads | Missing | `internal/config/schema.go` exposes URLs without httpsOnly; fetch/catalogue paths accept HTTP (`internal/listfetcher/`, `internal/listdata/catalogue/`). | Policy covers initial URL and redirects, custom CA compatibility, opt-in defaults and explicit diagnostics; test both download paths; G07, G21. |

## Dashboard (dashboard/)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F52 | Dig answer reports matched exclusion, avoiding second request | Missing | `dashboard/src/components/domains/DomainCheck.tsx` still calls dig then domains.list and locally matches exceptions; `DomainsPage.test.tsx` tests that explanation. | Report actual decision/exclusion in original DNS/HTTP diagnostic owner; UI consumes it without cross-generation second fetch; G07, G15, G21. |
| F53 | Query-time exact ClientGroup scope for allow/deny lists (D143) | Partial | `internal/api/domain_command.go`, `internal/domainlist/` attach lists through policies; contextual modifier/group identity exists in `internal/blocking/context_policy.go`, not proof of this UI intent. | Shared-policy/multiple-group acceptance where only selected group changes; schema/current command and captured query semantics; G07, G10, G15. |
| F54a | Regex “overrides block list” explanation | Missing | `internal/api/domains.go: DomainEntry` explicitly says matching information is empty for regex; `DomainsPage.test.tsx` covers regex entries, not this explanation. | Bounded meaningful regex impact/explanation, or explicit approved UI limitation; G07, G15. |
| F54b | Regex allow/deny cache invalidation | Partial | Named flushes in `internal/api/domain_command.go` and `domains.go: flushNames` do not enumerate regex matches. Complete serving uses generation-owned caches; `cmd/kudurru/serving_domain_commands_test.go` covers publication. | Prove cached queries matching a newly added/removed regex change only on fresh publications, old held requests remain old, including remote edge caches F39b; G07, G14, G22. |
| F55 | Per-digest index for paginated URL entries | Missing | `internal/api/source_reads.go` opens the exact captured digest and parses it per request; `sourceentries.go` caps concurrent body scans at four, without a persistent digest index. `source_reads_test.go` validates original-body/cursor behavior. | Benchmark multi-million lines; bounded per-digest index preserving line/exclusion/parser identity and original-generation pagination, eviction and body unavailability behavior; G15, G23. |
| F56 | Self-host Inter/JetBrains Mono in Kudurru theme | Missing | `dashboard/src/styles.css`, `dashboard/src/themes/kudurru/kudurruTheme.ts`; `branding/tokens.json` names fonts but no font binaries in dashboard public/src. | Licensed local assets, provenance/CSP, fallback/weight coverage, bundle budget and theme browser acceptance; G15, G24. |
| F57 | Nested Shape help for list/map elements/keys | Missing | `internal/config/shape.go: Shape` has no Help; Field/Option have Help. API shape projection in `internal/api/settings.go` and recursive form `dashboard/src/components/fields/ShapeField.tsx` are the implementation seams. | Preserve field/option help; expose nested help, populate real schema examples and show accessible list item/map key/value help recursively with focused Go/UI coverage; G15, G21. Assigned to this audit worktree after its documentation commit; not landed at baseline. |
| F58a | Astryx record-delete AlertDialog | Implemented | `dashboard/src/pages/ZoneDetailPage.tsx` imports/renders AlertDialog with Delete record action; `ZoneDetailPage.test.tsx` has delete coverage. | Final browser acceptance remains G23; do not implement a second confirmation; G15. |
| F58b | `/queries/export` API and query-log link | Missing | `internal/api/querylog.go` has no export route; `dashboard/src/pages/QueryLogPage.tsx` has no export action. | Permission/privacy/scope/bounds and streaming cancellation contract first; then accessible link/download and real API acceptance; G13, G15, G21. |

## CLI

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F59 | `--context` chooses kubeconfig/direct CRD verbs | Missing | `internal/cli/config.go`, `cmd/kudurru/cli.go` / `login.go` contexts select Kudurru HTTP endpoints. | Explicit transport selection, namespace/RBAC/ownership/impersonation semantics and kubeconfig error cases; never silently reinterpret stored API contexts; G12, G15, G22. |
| F60a | Offline `import adguard` / `import blocky` | Missing | `cmd/kudurru/zones.go: newImportCmd` registers BIND, live Pi-hole/Technitium and migration administration, not these commands. | Active parent-owned importer agent: dry run/loss reports, supported subset, bounded input/secret handling and real route semantics. Do not duplicate it; G15, G21. |
| F60b | `cluster status` / `promote` / `join-token` | Missing | No cluster command registration in `cmd/kudurru/cli.go`; `internal/cluster/` and SQLite authority supply primitives. | Separate bounded commands with one-time enrollment, authorized/fenced promotion, wrong-cluster/replay/expiry and partition/rejoin acceptance; G11, G15, G22. |
| F60c | `integrations …` CLI | Missing | Provider host/planner exists in `ee/integration/`; no dedicated command family in `cmd/kudurru/cli.go`. | API/current admission plus status/preview/actions CLI, exact plan ownership and license/error paths; G09, G15. |
| F60d | Zone option writes via PUT/PATCH | Partial | `cmd/kudurru/zones.go` has list/get/import/export/notify/delete; generic object apply can write DNSZone. `internal/api/zone_command.go`, `cmd/kudurru/serving_zone_commands.go` admit current zone mutations. | Dedicated zone option CLI and documented route contract with managed-owner/permission/no-op/current proof tests; G05, G15, G21. |
| F61a | CLI browser loopback + PKCE login | Missing | `cmd/kudurru/login.go` supports token or username/password; browser dashboard OIDC code in `internal/auth/browseroidc/` is separate. | Loopback listener/state/PKCE, provider/client selection, cancellation and no credential leakage; G12, G15, G23. |
| F61b | CLI device-code login | Missing | Same CLI; `CLIClientID` metadata does not implement RFC 8628 polling. | Provider device endpoint, bounded polling/backoff/expiry, approval/denial and stored CLI credential semantics; G12, G15, G23. |
| F62a | CLI conformance against actual API router | Partial | `cmd/kudurru/fakeapi_test.go` still drives command golden tests. Real route/mount tests exist (`serve_contract_test.go`, `serve_users_mount_test.go`, `internal/api/*_test.go`) but do not drive full CLI against that router. | Small shared CLI/real-server harness and representative object/settings/error/pagination/SSE commands; G15, G21. |
| F62b | §10 documents ApplyResult errorKey/hint, wrapped settings, SSE event | Partial | `dev-docs/design-record.md` §10 describes envelope/SSE broadly; concrete response types in `internal/api/objects.go`, `settings.go`, `events.go`; generated client presence is not prose contract reconciliation. | Parent-owned design/API documentation pass must match all three exact wire contracts; assert serialization through real-router tests; G15, G21. |

## Build, CI, docs

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F63 | Fixed-class benchmark job, including UDP sockets and Persist200k | External | `.github/workflows/ci.yml` selects explicit Ubicloud runner classes but has no calibrated `make bench` job. `Makefile` benchmarks blocking/zone; socket/persist benchmark sources remain separate. | Owner chooses stable class/noise policy; add all required workloads, measured baselines/artifacts and regression gate; static runner labels alone do not close it; G23, G24. |

## Deployment (binary behavior the chart and compose files need)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F64a | `kudurru_cluster_fenced` metric/dashboard/alert | Missing | `deploy/prometheus/rules.yaml` explicitly lists it as deferred; no metric exporter in `internal/metrics/` or serving authority. | Gauge from actual persisted fencing/publication authority, promote/restart tests, alert/panel and generated deployment sync; G11, G13, G24. |
| F64b | `/healthz` snapshotAge / leader | Missing | `internal/health/health.go` supports an info map, but `cmd/kudurru/serve.go` does not populate these fields. Remote/internal status is a different contract. | Exact schema/staleness/no-snapshot semantics with leadership/restore tests and human-probe docs; G11, G13, G21. |
| F64c | DNSSEC signing-key expiry metric | External | Original bundled row explicitly targets this subpart to **v1.0**; signing is still v1.0 in `dev-docs/design-record.md` §13.3. | Preserve original v1.0 scope; do not pull signing into v0.2 or count its intentional absence as a new v0.2 implementation gap; G24 boundary. |
| F65 | `cluster.role: static` / Compose read-only HA membership | Missing | `cmd/kudurru/serve.go` initializes `cluster.Self` for SQLite; `internal/cluster/membership.go: Static` exists but `internal/config/schema.go` has no cluster.role/static keys. | Supported member config, stable identity, authenticated list bodies and explicit read-only/follower role using current authority; duplicate foundation F67; G11, G22, G24. |
| F66 | Separate querylog.db / local.db path keys and volumes | Missing | `internal/config/schema.go` exposes node.dataDir; `cmd/kudurru/serve.go` constructs files there. `deploy/helm/kudurru/templates/workload.yaml` uses shared data mount. | Independent validated paths, chart/Compose mounts, permissions/migration/restart and no cross-volume atomicity promise; G13, G15, G22, G24. |

## End to end and fuzzing (dev-docs/milestones/m5/e2e.md)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F67 | E2E-2 static SQLite cluster, peer list bodies, correct leader | Partial | `internal/cluster/membership.go: Static`, replication/promotion primitives and `internal/servingbuild/acceptance_test.go` exist; mounted serve still starts Self (F65). `test/e2e/cluster_test.go` is not production static HA qualification. | Wire actual two-node startup/enrollment and membership, internal body acquisition, leader loss/rejoin and continuing valid local DNS; G11, G22, G24. |
| F68a | AdGuard Home black-box e2e | Partial | F20a harness exists and skips when unconfigured; no unskipped run here. | One shared acceptance closes F20b and this row; pin external program/version and record actual result; G07, G23. |
| F68b | Knot catalog consumer interoperability | Missing | No Knot catalog harness in `test/e2e/` or `hack/`; Secondary BIND tests do not supply Knot catalog evidence. | Reconcile exact v0.1 consumer promise with v1.0 catalog production/consumption scope; retain requirement pending explicit decision, then actual Knot version/fixture/result; G05, G23. |
| F68c | Two-VM keepalived failover acceptance | External | `dev-docs/release-checklist.md` §3; supervisor unit tests in `ee/edge/keepalived/` do not run two VMs. | Per deployed topology: lameduck retains VIP, drain and node loss move it within bounds, DNS continuity, restore; G14, G23, G24. |

## Central database and cluster-wide audit (D155–D161)

| ID | Original item / subrequirement | Status | Concrete evidence | Required to close; review |
|---|---|---|---|---|
| F69 | Central reads for retired node via scope=node&node=other | Missing | `internal/api/scoped_querylog.go` chooses central only for cluster scope; `queryPeers` rejects unknown/retired nodes. `internal/querylog/sqlitelog/central.go` reads combined nodes. | Node predicate for every relevant fact/rollup/dictionary query, pagination/uniques and retired-source tests without live peer fallback; G13, G21. |
| F70 | Prune unreferenced central dictionaries | Missing | `internal/central/pgquerylog/retention.go: Jobs` handles partitions/raw/rollups, not dictionaries; `dicts.go` inserts them. | Reference-safe transaction/pruning order with concurrent shipping, retained rollups and restart acceptance; G13, G22, G23. |
| F71 | Summarize facts of permanently stopped sources' last partial hour | Missing | `internal/querylog/sqlitelog/central.go` uses shipped rollup watermarks; `internal/central/pgquerylog/retention.go` has no stopped-source summarizer. | Idempotent source/hour backfill after inactivity, late shipping/rejoin reconciliation, no double counting and exact boundary tests; G13, G22. |
| F72a | Diagnosis page: missing AuditLog CRD and object-count recommendation | Missing | `internal/api/nodes.go` diagnosis messages and `internal/api/doctor.go` do not include AuditLog CRD availability/count; `cmd/kudurru/auditlog.go` supplies existing startup/log behavior. | Permission-aware bounded discovery/count, readable missing/forbidden/unavailable/over-50k status in actual Diagnosis page; G13, G15, G21. |
| F72b | Same AuditLog checks in `kudurru doctor` | Missing | `cmd/kudurru/doctor.go` checks central DB and current diagnostics but not AuditLog CRD/count. | Reuse bounded public diagnostic result, meaningful exit/warning status, no unprivileged metadata leakage; G13, G15. |
| F73 | AuditLog verification-key lifecycle independent of session rotations | Missing | `cmd/kudurru/auditlog.go: auditSealKeys` derives only current/previous encryption keys; `internal/audit/auditlog/verify.go` verifies with supplied ring. `auditlog_test.go` covers those keys, not durable retained-key lifecycle. | Independent persisted key ring/retention/compromise policy and multi-rotation retained-batch verification, backup/restore, bounded error reporting; G12, G13, G22. |
| F74 | Central availability/shipping lag/audit-spool Grafana panels | Missing | `deploy/grafana/kudurru-overview.json` lacks the three metrics; alerts already exist in `deploy/prometheus/rules.yaml`; metric definitions in `internal/central/metrics.go` and `internal/audit/spool/`. | Panels using actual labels/stream dimensions, meaningful no-central-data behavior, valid JSON and synchronized chart copy; G13, G24. |
| F75a | `GET /audit/verify` using existing verifiers | Implemented | `internal/api/authroutes.go` registers route; `auditverify.go`, `auditverify_test.go`, `audit_test.go`; `3df864c`. Permission/license gate, integrity/unavailable distinction, bounded shared scan and cache exist; underlying readers implement Verifier. | Retain final driver/live mounted acceptance; no second verification endpoint required; G13, G21. |
| F75b | `kudurru audit verify` | Missing | `cmd/kudurru/observe.go: newAuditCmd` lists events but registers no verification subcommand at baseline; `cmd/kudurru/auditlog.go` assembles server storage. | Use F75a route; fail on tampered/unavailable, human/JSON output, permission/license/cancel and real-router CLI tests; G13, G15, G21. |

## Integration update after the audited baseline

These changes are integrated on PR #54 after the baseline above. Exact
commands and execution results are recorded in [the gap ledger](gaps.md).
Baseline statuses remain visible to preserve the original review; this table
records their subsequent closure without counting duplicate rows twice.

| Rows | Implementation and validation | Remaining scope |
|---|---|---|
| F33 | `49f9e8b`: authenticated remote member health, two-second total deadline, 1 MiB response limit, strict identity and redacted errors. Actual TLS peer/public-route races pass. | Older peers report unsupported; final multi-node deployment qualification remains. |
| F60a | `48b11dd`, `32f9e81`, `f01d4db`, `54fcf48`, `01f35ed`, `39b13dc`: shared reviewed offline plan machinery and separate AdGuard Home/Blocky adapters. Explicit forward mode replaces recursive destination collisions, and structured Blocky entries respect the schema character bound. Previews never fetch source URLs or files; supported subsets and omitted semantics have explicit loss reports. | Conversion is intentionally partial; final combined CLI/current-API acceptance and generated contracts are recorded below in the ledger. Other F60 commands remain open. |
| F75b | `a097836`: audit verification CLI reuses the existing permissioned/licensed route. Real-router tests cover actual SQLite tampering, unavailable verification, human/JSON output, denied access, cancellation and request deadlines. | Existing 30-second server cache remains; verification cannot prove deleted history never existed. |
| F24a | `661174c`: cumulative slip counter, distinct from drops and retained across reconfiguration. Native UDP/TCP proxy-attribution/TC race tests pass. | F24b group exemptions remain separate and missing. |
| F74 | `60a3af1`: central availability, query-shipping-lag and audit-drop panels, synchronized Helm data and existing dashboard/metric checks pass. | Missing central series are not treated as outages; final deployment qualification remains. No central-retention feature is implied. |
| F20b, F68a | `f5d5316`: mandatory CI invocation of the first-party corpus against checksum-pinned official AdGuard Home **v0.107.79**. The actual race-enabled comparator passes. | Retain corpus/scale limits and final hosted-run qualification; the separate executable replaces the proposed container mechanism without copying copyleft source/data. |
| F35c, F49 | `5fc97d7`: optional host-only file-backed webhook bearer authentication, bounded startup read, constant-time pre-dispatch check and restart-required rotation. Config, actual listener, runServe invalid-startup and preserved write-gate races pass. | Default disabled; upstream external-dns v0.23.0 needs a trusted proxy to inject Authorization. Transport authentication does not grant current mutation authority or implement direct Kubernetes watches. |
| F57 | `d37f4ad`: recursive schema help with field override/fallback, populated real nested examples and accessible UI rendering. Metadata/serialization checks and 59 focused UI tests pass. | Generated contracts and final composed checks follow below; validation/write semantics remain unchanged. |
| F20c | `a2bb3bf`: move the unchanged regression gate to `hack/benchgate`, update callers and documentation. Gate race tests and baseline-against-itself checks pass. | Historical measurements retain their original package path. This is not a fresh performance qualification; F20d remains missing. |
| G04, G21–G23 | `7a28a44` corrects the obsolete mounted adoption gate assertion. `f5624e7` stops obsolete-source publication waits without acknowledging stale data. Four deterministic regressions fail before the fix; publication races, full primaryruntime/driver/servingbuild races and 10/10 repeated migration browser cases pass afterward. | Existing assertion/deadline strength is preserved. Final hosted CI and all wider AD/release gates remain separate. |

## Original review crosswalk

These are coverage relationships, not package completion claims. Requirements
in the original review without a v0.2 follow-up row are still required.

| Original review | Relevant F inventory / remaining independent gate |
|---|---|
| G05 / V2-ZONE | F05–F08, F60d, F68b; BIND/Knot, multi-replica, expiry/restart and diagnostics qualification remains separate. |
| G06 / V2-UPDATE | F02c, F04, F06–F07, F12; actual supported-driver lifecycle/atomicity/forwarding acceptance cannot be inferred from primitives. |
| G07 / V2-DNS | F10b, F17–F31, F52–F54; completed modifiers/forwarded validation are separate from missing recursive RFCs, transport settings and edge qualification. |
| G08 / V2-RPZ | No standalone v0.2-targeted follow-up row. Original feed/action/expiry/diagnostic/UI acceptance remains in G08; this audit neither drops it nor counts text blocking tests as RPZ acceptance. |
| G09 / V2-INTEGRATIONS | F09, F18c, F35, F49, F60c; existing host/planner code does not establish production mount or optional UniFi writes/Tier-B/external-zone acceptance. |
| G10 / V2-IDENTITY | F18b/c, F22d, F24b, F53; complete provider fill, inventory UI and edge equivalence still need final evidence. |
| G11 / V2-HA | F01, F02c, F12d, F19, F60b, F64–F67; primitives are distinct from production membership, join wizard/CLI, forwarding and partition/rejoin. |
| G12 / V2-IAM | F02b/d, F07a, F34, F44–F50, F59, F61, F73; local/external durable authority is separate from remaining admin surfaces, kube impersonation and CLI OIDC. |
| G13 / V2-OBS | F02a/b, F16, F24a, F26, F33, F38, F40b, F58b, F64, F66, F69–F75; original dnstap/OTel/ClickHouse/Loki nonblocking exporter work is additional scope. |
| G14 / V2-EDGE | F39–F43, F54b, F68c; original dynblock push and real dnsdist/VIP acceptance remain additional requirements. |
| G15 / V2-OPS | F08, F10a, F21, F32, F35–F37, F52–F62, F66, F72, F75b; preserve captured reads/current commands and final credential/license deadlines while extending surfaces. |
| G16 / AD1–AD2 | F12c, F13; full discovery/import and handoff, not generic provenance annotation. |
| G17 / AD3–AD7 | F11, F12, F15; production authority/security/ownership and actual Windows evidence. |
| G18 / AD8–AD11 | F12d, F14, F15; DHCP/name protection, aging/security persistence and administration. |
| G19 / AD12 | F13b, F15; final cutover, lossless rollback and tested supported Windows matrix. |
| G20 / inventory | All 75 targeted source rows mapped to 130 explicit subrequirements here. Original identities remain here when completed source rows are removed; no requirement is retargeted. |
| G21 / production contracts | Applies to every row; model/helper/test existence does not mount a feature. API/CLI/UI/schema/permissions/docs must agree. |
| G22 / upgrade/failure | All supported-driver migrations, durable restarts, fences and multi-process partition/rejoin remain final-artifact gates. |
| G23 / interoperability/performance | F05, F11, F15–F31, F39, F43, F55, F61, F63, F68; tests with optional external-program skips require unskipped results. |
| G24 / deployment/release | F15, F41–F43, F56, F63–F68, F74; applicable release-checklist, license/artifact and two-VM evidence remain open. |

## F64a authority-fencing metric closure

The SQLite metric registry now exports `kudurru_cluster_fenced` by sampling the
bound store's atomic authority and failed-persistence fence. Real store tests
cover unenrolled/follower/promoted states, a higher-term fence, successor-image
installation, failed durable fencing/retry/restart, divergence, failed resolution
rollback and explicit resolution. The registered scrape remains 1 while fenced;
healthy read-only followers report 0. No DNS or writable-readiness claim follows.
The immediate alert and Grafana panel are synchronized into Helm assets.

Focused SQLite races pass in 9.296 seconds; dashboard generation, registered
metric references and rule checks pass; scoped SQLite/deployment lint reports
zero issues. This closes F64a's instrument and shipped assets. F64b, F64c and
final multi-process/deployment acceptance retain their separate requirements.

## F64b human health details

`/healthz` now returns `leader` from the local controller elector and
`snapshotAge` from the snapshot currently being served. Unknown/expired self
leadership and live/no-snapshot states produce empty strings. No membership
lookup, network request or disk read is added to any probe. Detail observation
does not change readiness or imply write authority.

Real runServe HTTP and local state-transition races pass in 2.160 seconds;
health and authenticated peer-health races pass in 1.040/3.338 seconds. Tests
cover stale/fresh saved ages, live recovery, remote observation, old self terms
and absence of detail acquisition on startup/live/ready probes. Composed
Kubernetes startup/identity/health races now pass in 38.391 seconds after F01
integration. Fresh and stale admitted snapshots report their actual age;
first boot and rejected identities report no age; API outage reports no leader.
The fixture observes admission/refusal within its original startup budget,
because unsynced and admitted-stale states both return SERVFAIL/503.

## F04 shared rejection conformance closure

`d4075f5` adds 14 named typed-store rejection cases run on each production
driver, plus two separately labeled strict decoded-input unknown-field cases.
Literal expected structured causes are independent of the validator under test.
Each case checks objects/ownership, driver-specific durable state and an ordered
watch barrier. The real Kubernetes checks include managedFields; SQLite includes
resource version, change feed and audit counts.

The tests found and fixed raw apiserver Invalid errors, typed nil-target cause
differences, and ownership mutations before schema/transition/size rejection in
forced apply. RV-guarded real dry-run PATCH and pre/post-admission size checks
now precede takeover. Normalization retains nullable/default/list semantics.
Kubernetes remains a multi-request operation after successful preflight;
subsequent backend/concurrency failures are not claimed atomic.

Scoped production-driver/validator races pass: SQLite 12.448 seconds, actual
Kubernetes envtest 34.102 seconds, validator 1.337 seconds. They include successful
Force, patch-only RBAC, kubectl ownership and deterministic RV races. Final
committed table-only races pass in 4.859/20.092 seconds without envtest skips;
scoped lint reports zero issues. This closes F04's named conformance requirement,
not deployed admission-webhook qualification or all supported-driver gates.

## F22a configured native listener limits

`listeners.limits.maxTCPQueries`, `quicMaxConns`, `idleTimeout` and
`quicIdleTimeout` now map from typed validated host configuration into the actual
registry limits. They are node-scoped, host-only and restart-required. Count
zero remains unlimited; duration zero selects the existing 10s/30s defaults.
Negative/overflow/invalid values are rejected. These settings apply to native
listeners; separate dnsdist settings remain separate work.

Configuration and actual transport races pass in 1.238/2.598 seconds, including
unlimited/bounded TCP and DoT connections, DoQ/DoH3 connection refusal and idle
closure on TCP, DoT, DoH, DoQ and DoH3. The initial new QUIC tests assumed peer
closure observations matched server lifetime and always carried an HTTP/3
application code. Pinned quic-go clamps remote idle advertisement to five seconds;
RFC 9000 section 10.2.3 uses a transport application error for early rejection.
The final tests observe actual server-side DoQ idle expiration and accept only
the exact corresponding remote refusal codes, with no production timeout change.
A new mounted fixture originally asked the health name for A (which is not its
local TXT response); after correcting the fixture, real runServe TCP-count
acceptance passes in 7.133 seconds. Existing assertions were not weakened.
Scoped config/DNS/CLI lint reports zero issues; generated configuration and CLI
references are synchronized. F22b/c remain independent missing requirements.

## F26a daily pseudonym reload continuity

Query-log privacy replacements now share one private process-local key state
when the anonymizing transform is unchanged. Exclusion changes take effect
without changing that day's pseudonym. Old held filters and the replacement
share UTC-midnight rotation. A mode or anonymization-method change starts a new
lifecycle, including reuse of a previously prepared filter; new collectors and
process restarts do not inherit prior keys. Keys remain unexported and unpersisted.

Final lifecycle/concurrency races pass (`internal/querylog`, 1.032 seconds),
including exclusion application, ingestion/lookup equality, midnight, detached
caller objects, mode changes and a fresh collector. The production reload
subscription test passes under races (`cmd/kudurru`, 1.146 seconds). Scoped
querylog/CLI lint reports zero issues. Other F26 reload requirements remain open.

## F01 independent offline identity closure

`1bf8586` adds host-only restart-required `store.kube.expectedClusterID` and
shared saved-state admission. Both saved and independently supplied identities
must be nonempty and equal; a known online mismatch also refuses. All ordinary
and auxiliary Kubernetes requests inherit the initial identity guard. Matching
verification is cached for the process lifetime, so endpoint retargeting requires
restart. Empty pins preserve online compatibility but disable offline objects
and settings. Legacy identity-free saved state requires verified live refresh.
No saved data is relabeled, and auxiliary requests do not refresh snapshot age.

Combined config/settings/localdb/zonecache/store races pass with real envtest
assets (7.932/4.231/2.046/23.589/76.313 seconds). Agent acceptance includes actual
cold start and live recovery; composed health acceptance is recorded below.
D230 and the generated/operator configuration document the compatibility policy.

## F47 bounded projected credential closure

`101b614`, `9e7b177` and `41a7694` require integral original issuance/expiry,
valid optional not-before and at most ten minutes lifetime on both Kubernetes
verification paths. Positive cache entries last at most two minutes and never
past original expiry; clock rollback refuses. Private time evidence is rechecked
after delegated SAR and before cached object authorization. Complete TokenReview
acquisition, including client-go throttling, has a ten-second maximum with
shorter caller/client deadlines and cancellation preserved. No new Kubernetes
credential route is added to SQLite current commands.

Use a rotating audience kudurru projection with expirationSeconds 600; opaque
legacy and hour-long tokens are intentionally refused. Offline deletion remains
possible until original expiry, not immediate revocation. Real signatures/JWKS,
authoritative deletion/outage HTTP fixtures and actual REST cache-flush refusal
exercise these boundaries. The previously skipped existing envtest was rerun
with available assets and passed; three final envtest/projected repetitions pass
in 13.225 seconds. Combined auth/k8s/core/stack races pass in
11.264/1.047/10.941 seconds. D231 and deployment guidance state the limits.

## F72 AuditLog Diagnosis and doctor closure

`e77d025` adds a lazy, guarded, metadata-only namespace probe independent of
the startup reader/shipper decision. It reports missing/denied/unavailable or
incomplete scans, exact complete counts up to 50,000, and an explicit lower
bound at 50,001. Limits are one active scan, 500 objects/page, 101 pages,
2 MiB/page, 64 MiB/scan and three seconds. Pagination retains one RV; repeated
continuations, malformed/expired/oversized replies never yield a healthy count.
No discovery, redirects, full-object fallback or remote error text is exposed.

The generic Diagnosis renderer consumes the existing response shape. Doctor
reuses it and treats informational successful scans as OK. Original captured
source failure remains terminal; audit permission/license precede acquisition
and are rechecked afterward. Late API availability does not activate shipping
without restart, and does not affect DNS readiness.

Agent probe races with real Kubernetes metadata endpoints pass in 12.714s;
actual router/API and CLI/startup races pass in 3.837s/18.397s. Integrated tests
then use actual signed projected JWTs through both mounted JWKS and TokenReview
paths: expiry before acquisition makes zero calls; expiry during acquisition
withholds results. A production callback test proves wrong cluster identity
prevents the metadata request. Composed auth/API/CLI races pass in
6.126s/2.840s/5.719s. These supplement the actual signed-license deadline and
permission-revocation tests. The UI response shape and renderer are unchanged;
this package does not claim a new browser-suite pass.

## F22b native HTTP/3 discovery closure

Native DoH advertises a running DoH3 alternative only when bound address/port,
certificate/key files, path and client-ID policy match. TLS is required;
forwarding headers and a differing public HTTPS port suppress advertising.
The header preserves the authority/path and expires after 60 seconds. A stopped
listener is no longer advertised. Reverse proxies own public discovery and must
strip/replace backend headers; native readiness cannot prove firewall/NAT reachability.

Actual HTTP/1.1 and HTTP/2 acceptance follows the advertised endpoint with a
verifying HTTP/3 client and preserves the client-ID selector. Tests cover missing,
different-port/path/policy/certificate alternatives, plaintext/proxy requests,
IPv6 authority parsing and stopped listeners. Focused transport races pass in
1.299s; the complete native DNS transport race suite passes in 30.461s. D240
and query-path deployment guidance describe the conservative matching contract.
F22c response padding is recorded in the following closure.

## F22c encrypted response padding closure

Capture EDNS version-zero PADDING negotiation before pipeline adaptation and
apply zero-filled padding to a detached response at the native transport write
boundary. DoT/DoH/DoQ/DoH3 target 468-byte DNS-message blocks, counting final
TSIG bytes but excluding transport length prefixes. The peer's payload limit
and 65,535-byte ceiling constrain the result; a partial block fills the remaining
budget when necessary, and an option that cannot fit is omitted without changing
records. Padding is last; other options, AD/DO flags, original proof and cached
answers remain intact. Plaintext and nonoffering peers get no generated padding.

Actual wire tests cover all six transports, no-EDNS/no-offer negotiation, small
payloads, maximum message size, detached reusable responses, both supported
TSIG MAC sizes, a chained signed DoT transfer and BADSIG refusal before pipeline
execution. The verifier mutates its byte input while stripping TSIG; the transfer
fixture now verifies a copy and keeps exact received bytes for record assertions.
No production signature or refusal behavior was changed to satisfy the test.
Focused padding races pass in 1.255s, full transport races in 30.498s, and scoped
lint reports zero issues. D242 records the policy; external interoperability and
fixed-class performance remain under their original final gates.

## F56 integration: local brand fonts

`13c0c9e` closes the missing font implementation with pinned OFL assets and
provenance, relative hashed URLs under the unchanged CSP, normal/italic variable
weights and system fallbacks. All four Latin faces are included in the budget
at their actual transfer size, producing **480.5 KiB** against 600 KiB. Other
scripts retain system glyph fallback; other dashboard themes keep their own
font stacks. Package licenses are embedded in `third-party-licenses.txt`.

Seven real browser scenarios pass in 20.1s with a newly built CGO-disabled test
binary, including root and nested prefix, external-network denial, actual
heading font painting, all four UI weights/styles, notice delivery, failed-font
fallback and both color-scheme contrast checks. Nine existing theme tests,
TypeScript, scoped ESLint, generated theme check and Go handler races pass.
D243 records the delivery policy. Final full browser/artifact gates remain open.


## F06 independent NOTIFY key and source

`c80c6f2` and `46d7641` integrate separate backend and dashboard controls. The
explicit key is independent of transfer authorization; empty retains the old
first-transfer-key/unsigned behavior. The per-zone literal source overrides
host `notify.source`; empty preserves global/system selection. Missing keys,
rotation/revocation, unavailable/wrong-family source and current policy changes
refuse without alternate identity/source or obsolete intent acknowledgment.
Full normalized transfer policy and original elected writer/publication/final
checks remain intact. The primary-zone wizard/settings editor supports creation,
editing, clearing, validation/help and exact If-Match conflict refusal.

Regenerated manifests, Helm CRDs and embedded schemas retain the new public
fields. OpenAPI/Go/TypeScript generation runs without a wire-file change because
object schemas are generic; generated CRD/operator references change. Actual
IPv4/IPv6 UDP, distinct-key signing and current-command races pass. Combined
unoverlaid parent races: zone 1.334s, notifier 2.012s, validator 1.594s,
primaryruntime/network 43.225s, API 3.441s, cmd 37.878s, servingbuild 8.042s. This
includes F29 publication/reload checks and existing mounted NOTIFY regressions.
Integrated dashboard unit tests pass 86/86 in 7.11s with typecheck.

## F29b recursive exceptions and actual installed schema

`ddc5f2d` uses a separate bounded recursive NTA list with absolute expiry and
shared immutable matching. Active exception identity scopes native caches and
raw answers; cache restore/removal cannot renew the exception. Native provenance
marks only excepted RRsets, so expired exceptional signatures remain data while
outside alias proofs retain their own deadline and can refuse the answer.
Agent final seven-package races and ten repeats of real-socket NTA cases pass;
only the pre-existing optional live-Internet test skips. Parent regeneration
removes the temporary agent schema overlay, and the complete-publication/reload
races above pass using ordinary embedded artifacts.

Actual Kubernetes envtest contract races pass in 5.877s: the installed CRDs preserve
NOTIFY choices and the original recursive exception timestamp; direct API
updates reject invalid keys/sources/exception shapes without changing stored
resource versions. The first new fixture marshaled a whole zero-valued upstream
config, correctly rejected by existing enums; it now submits only its intended
partial settings, preserving all admission assertions. D238/D241 record the
policies. External DNS interoperability and complete release gates remain open.

## Prioritized bounded implementation queue

“Owner” is a proposed workstream, not an assignment or authorization to change
production. Each item should be its own reviewable commit, with synchronized
gap/design/changelog/generated artifacts and combined checks. Completed
remote health (F33), offline import subsets (F60a), audit verification CLI
(F75b), slip metrics (F24a), panels (F74), AdGuard CI wiring and benchgate
relocation (F20b/F20c), nested settings help (F57) and webhook authentication
(F35c/F49) are recorded above and removed from this queue.

| Priority / IDs | Current owner / dependencies | Concrete next change and exact acceptance |
|---|---|---|
| P1 / F09b | Integration host agent; SQLite atomic sources, then Kubernetes equivalents | Mount providers with original elected writer/credential/license proof, atomic ownership/inventory and receipt-qualified publication; qualify failover and finish Delete/Kubernetes dependencies. |
| P2 / F18a | Global-control agent; immutable serving configuration | Wire global network/type controls through captured publications and legacy reload without live fallback. |
| P2 / F70 | Central DB; reference/pruning transaction design | Prune bounded dictionaries against retained facts/rollups and concurrent shipping; prove no dangling references/data loss and stable retry/restart. |

AD F11–F15, core HA wiring, RFC 8198/5011, console diff-apply and external
qualification are critical remaining scope, but are not misrepresented as
small independent chores. Break them into reviewed design/implementation/
acceptance packages; Windows and two-VM evidence require appropriate labs.
The known Cloudflare documentation check does not block any queue item here.

## Audit validation and totals

At the audited baseline, **130 subrequirements** classify as **18 Implemented,
25 Partial, 76 Missing, 11 External**, covering **all 75 original targeted
rows**. These counts intentionally precede the separately attributed
integration update. Duplicate source requirements remain visible: F02a/F38,
F20/F68a, F35c/F49 and F65/F67 must share their eventual implementation and
acceptance evidence rather than be counted as independent feature delivery.
F64c retains its original explicit v1.0 subtarget inside a bundled v0.2 row.

Validation at the `b42fa24` baseline: enumerate Target cells containing v0.2, compare
F01–F75 coverage and suffix uniqueness, count status cells, verify referenced
concrete repository paths, and run `git diff --check`. Product tests were not
run. All remaining G21–G24 acceptance is retained; subsequent implementation and test results are attributed separately above.


F06 browser composition in `94869e5` passes with the newly rebuilt integrated
binary (11.3s): the real zone editor persists and clears both choices while
preserving transfer policy and metadata, with one PUT per action. The initial
fixture captured an ETag before first controller completion and correctly got
412. The final fixture observes Ready/current generation before opening the
editor. Existing temporary record-read 503s during policy publication are
recognized only on this test's exact record route; eventual 200, visible recovered
page and persisted values remain required. No mutation is replayed. Final
combined Go lint reports zero issues; browser typecheck and ESLint pass.


## F05/F31 implementation closure after the baseline audit

`eb20ac7` supplies bounded per-record parsing reuse with detached records and
full source/zone identity, TTL/expiry invalidation. `668203c` supplies native
recursive authority cookies, exact echoed-cookie checks, actual address
binding, bounded rotation/state and original-budget BADCOOKIE/TCP recovery.
The integrated nine-package race run and combined lint pass; actual UDP/TCP
and Kubernetes fixtures ran. Shared-host changed-record benchmarks and limits
are recorded in [gaps.md](gaps.md#f05f31-composed-parsing-reuse-and-authority-cookies).
These two items leave the implementation queue. Final performance,
interoperability and release gates remain independent.

`09c0eed` corrects hosted DoH3 timeout observation at the actual server, without
changing the configured bound or test deadline. Ten focused race repetitions
and the full transport race suite pass. The hosted dashboard failure remains
recorded even though 25 focused tests and all 880 dashboard tests pass locally.

### F18a implementation and generated-schema acceptance

`50773f9` closes the global-control implementation row: bounded validated
network/type lists, captured immutable rules, legacy atomic hot reload, local
edge invalidation and synthetic dependency enforcement. Ordinary generation
and composed mounted/runtime races pass on the integrated F09/F18 source;
actual installed Kubernetes CRDs preserve both settings and reject malformed
shapes. Combined scoped lint reports zero issues. The [gap ledger](gaps.md)
records exact results. F18b/c hostname sources and F39b remote edge propagation
remain separate open requirements; G21–G24 remain applicable.

`d1502a6` fixes a real F68c Docker-lab defect where cached health SERVFAILs
could outlive backend removal and fault a healthy VIP. Exact-name cache bypass
preserves transfer refusal, PROXY authentication, backend checks and all VRRP
timers. The complete container alternative now passes 12/12 checks; it does not
constitute the original independent two-VM qualification. See the gap ledger.


### F62/F70 implementation and container acceptance

F62a/b leave the implementation queue with `6938fbb`/`c439532`: actual-router
CLI tests and exact apply/settings/SSE prose pass scoped races and lint.
F70 leaves the queue with `eef61bd`/`b1a29e8`/`a7304d8`: schema-4 references,
locked dictionary resolution and bounded durable sweeps pass actual PostgreSQL
17.11 migration/concurrency/restart/deadline races and lint. Scale and final
artifact gates remain independent.

The completed F68c container alternative (`6eac332`) passes 12/12 checks with
real dnsdist/keepalived and four UDP/TCP streams, including whole-node recovery.
It uses app source `b8fd96f`, and does not certify the later final artifact,
independent VMs, Windows or replicated SQLite HA. Exact versions, transitions,
observed packet failures and limits remain in [gaps.md](gaps.md).


### F02/F38, F51 and F66 implementation closures

`aa3784c` closes F02a/F02b/F38 with schema-6 authenticated attribution and shared
pre-storage document redaction while preserving historical source chains.
`a15480e` closes F51's opt-in transport policy, including HTTP initial/redirect
refusal and existing trust/SSRF behavior. `b3f6986`/`5048c61` close F66 with
independent paths and additive volumes that retain original data durability.
Actual mounted composition, PostgreSQL acceptance, download, migration, chart,
Compose and volume checks pass as detailed in [gaps.md](gaps.md). Their explicit
historical-data, filesystem, physical-deployment and final-artifact limits remain.
F73 key lifecycle and the hosted dashboard failure are still active work.

### F60d and F50 follow-up closures

F60d is implemented in `c9f14dc`: dedicated reviewed zone-option get/set uses
the existing object PUT and `If-Match`, with complete-spec replacement and
metadata/status preservation. Actual-router tests cover ownership, permissions,
invalid/stale reviews and no replay. Actual SQLite/current-serving tests prove
successful persistence and original credential expiry at final commit for both
changed and unchanged options; composed races pass in 6.350s. CLI reference
generation and scoped lint pass. This does not add a second zone mutation seam.

F50 takes the original review's documented-overlap option in `feefdbc` (D254). The v0.2
contract preserves exact namespace/name matching for both authenticated
service-account providers, documents creation/recreation and distinct account
lifecycles, and explains narrowing token scopes plus independent SAR grants.
Actual auth-to-binding-engine tests cover both kinds, other names/namespaces,
zone scope, absent service-account identity and narrow/malformed scopes. Auth
and RBAC races pass in 1.052s/1.023s; scoped lint reports zero issues. This is
authorization-contract evidence, not new Kubernetes identity or driver-wide
acceptance; F44b/F47 and other identity gates retain their own evidence.

### F42 per-VRID secrets, F52 query explanations and dashboard transport

F42 is implemented by `49a9cf8`, `376d63d`, `e760dd7` (D253). Explicit Secret
references must cover every configured VRID exactly once, with no shared fallback
or Helm content reads. A bounded init stages private root0600 files because
real keepalived 2.4.3 refuses group-readable HMAC files. The normal container
retains SETUID/SETGID for its existing script user; the init drops all capabilities.
Real baseline preflight fails without these script capabilities and passes with
them. Seven disposable cases cover per-VRID/shared/rotated references, missing/
empty/oversized refusal and staging all 255 files (0.531s, retained 30s bound).
Parent composed deployment/CLI/keepalived races pass in 4.774s/1.196s/16.969s.
Ordinary deploy generation and local pinned Trivy misconfiguration scan pass.
Agent lint reports zero issues. Actual kubelet/CSI projection, enforced cgroup
limits and seamless mixed-key rotation are not claimed; coordinated restart
remains required. Owned lab resources were removed.

F52 is implemented in `8879459` (D255). Actual removed rules remain bounded
diagnostic witnesses in the immutable compiled lists. HTTP dig uses the original
question/client/policy selection, requires list-read permission for evidence,
and keeps it outside response caches. The UI uses the same answer without a
second list read or browser regex evaluation; another rule can still block.
Compile/query bounds and partial evidence are explicit. Stale/unselected rules
are omitted; response-address and alias-target exclusions are outside scope.
Full blocking/parser/matcher/stage races pass (1.672s/1.144s/3.280s/1.325s);
API/capture/schema races pass in 4.876s. Actual runServe SQLite/API acceptance
passes in 3.350s, including a disabled rule and a still-blocking second source.
All 28 domain UI tests pass in 39.67s, plus typecheck and scoped frontend/Go
lint. OpenAPI and both clients are regenerated. Fixed-class performance and
remote edge invalidation retain their separate gates.

Dashboard fixture fix `69e23c8` owns a test-only Undici 8.12.3 dispatcher and
closes it before MSW teardown. A controlled cancellation/native-fetch regression
exposes eight unintended real TCP connections with the old fixture and zero
with the fix. The original hosted component failure's native cause was not
captured, so this is a concrete transport defect fix, not a claim that a passing
rerun alone identifies every intermittent failure. The unchanged authenticator
and session tests plus regression pass 1,100 executions; full exact hosted
Node24.22.0 acceptance passes 881/881 (148.55s). Parent `e90dd7a` binds the probe
explicitly to loopback; 26 composed cases pass in 5.78s. No authentication
assertion, timeout, retry, production fetch path or MSW unhandled-request rule
was weakened. Fresh hosted confirmation remains required.

Prior-head f7ccf4a CI exposed TestBINDSecondary/NOTIFY failure in job124555248768.
That actual interoperability regression is assigned for investigation and is
not closed by successful local chart or protocol tests.

### F26d captured query-log sinks and disabled restart

`08ee759` and `9a62eb4` implement D266. Complete DNS publications include the
logging owner, retaining enabled/sink/privacy through emission. One bounded
queue/tail and at most one SQLite writer plus one slog sink survive reloads.
Sink preparation precedes publication; failure retains the previous bundle.
Accepted batches drain once to their original destination without replay.
Local history, purge, retention and configured shipping survive disabled/slog
modes, including restart with an existing database. Missing files stay lazy.
Shutdown joins the consumer before closing its writer; a timed-out external
handler cannot cause a replacement writer or close under pending I/O.

Agent mounted races pass in 48.457s, collector/SQLite in 1.154s/67.021s and full
servingbuild in 161.917s. Actual two-start UDP/TCP/history/purge regressions pass
twice in 85.887s. Parent mounted races pass in 38.366s, composed servingbuild/API
in 6.240s/9.873s, complete servingdns in 2.147s and combined CLI/restart races
in 33.815s. Scoped lint reports zero issues. Settings UI passes 22 cases;
generated schemas, help and references are updated. The agent full API run
hit its 180-second limit in source-scan coverage; no full API pass is claimed.

### F37 generated CLI transport

`a106708` plus the parent-generated artifact and Makefile hook implement D270.
The reproducible generator covers all 156 merged OpenAPI operations with direct
calls to their matching generated methods. The CLI retains raw body and
query/header ownership, cookie/CSRF, pagination, SSE, structured errors and
original cancellation. Final method/origin/escaped-segment checks prevent path
rewrites. Unknown routes retain one raw fallback; matched failures never retry
or fall back. Custom HTTP clients and low-level editors remain trusted extensions.

Complete agent generator/client races pass in 2.983s/1.633s and selected actual
router/SQLite CLI races cover 72 tests/subtests in 21.925s; scoped lint reports
zero issues. Parent regenerated for the merged routes; generator/client races
pass in 2.784s/1.580s and combined actual-router CLI/restart races in 33.815s.
These are scoped acceptance results, not a full repository qualification.

Hosted security 41213309941 and kind/MetalLB 41213309889 pass on `8ea7367`.
CI 41213309865 has no failed jobs at this checkpoint, with Go shard 1/2 still
running. GitHub docs 41213309876 exposed stale endpoint references for F54/F71;
this batch regenerates those references. The separate Cloudflare build remains
excluded. Subsequent heads require their own hosted checks.

### F16 exact current-hour domain statistics

`8b98f41` implements D269 with a transaction-maintained retained-fact
hour/node/domain index, plus original source centrally. Whole cells aggregate
directly; ragged interval edges use indexed raw rows. Counts remain exact after
privacy, rollback, purge and retention. SQLite schema 3 pins its WAL snapshot
with pending totals; central schema/compatibility 6 seeds under write locks and
uses an empty FK guard to refuse old connected partition DROP/TRUNCATE paths.
Older readers/janitors must upgrade. Index size scales with retained distinct
hour/node/source/domain entries; pathological boundary intervals still read
matching facts. Source-recovery provenance gates remain unchanged.

Agent final regression passes 61 SQLite, 10 central and 111 pgquerylog cases;
scoped races cover snapshot, upgrade/destructive-operation, privacy, retention
and source recovery. Final scoped lint reports zero issues. Parent composed real
PostgreSQL migrations/pgquerylog, mounted recovery/stats/export API and SQLite
races pass in 1.735s/41.926s/4.217s/173.457s. Its owned fixture and volume were
removed after the command passed.

The exact 1M-row/252k-name SQLite workload passes: raw unique aggregation
1.063s versus index 433.950ms; raw top 1.011s versus index 346.301ms. Full Summary
521.945ms, TopDomains 429.856ms, ingest 63.246s and index 9,809,920 bytes. Central
1M/252k acceptance also passes: Summary 217.787ms, TopDomains 379.051ms, ingest
312.606s and index/indexes 74,096,640 bytes. The central timing predates final
upgrade/snapshot refinements; final regression validates those refinements.
These are shared-sandbox measurements, not fixed-class qualification.

### F43c console capability under YAML

D275 and `hack/labs/dnsdist-console/run.py` qualify the external dnsdist 2.1.3
gate. Ten actual Docker checks pass against the immutable local image
`sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e`:
YAML startup/DNS, failed-command refusal, new UDP/DoT routing, backend add/down/
remove, certificate replacement with active-session continuity, invalid-cert
retention, unchanged PID and persisted-YAML restart convergence. The runner
uses the external client and private configuration, never console keys in argv.
A terminal return acknowledgment is needed; Lua print reaches the daemon log.

An established DoT connection selected to the removed/down backend returned
SERVFAIL, while new connections reached the replacement. This is recorded,
not hidden by reconnect/replay. Certificate-only reload preserved the separately
established connection. Minimal fixture members are forced up; production
health validators, PROXYv2, other encrypted protocols, bounded supervisor
failure convergence and readiness remain F43a/b implementation acceptance.
The capability gate is satisfied without claiming that implementation complete.

Hosted CI 41213309865 on 8ea7367 was superseded/canceled with Go shard 1/2
unfinished; its aggregate ci-ok failed because of that cancellation. The newer
8bdb218 CI remains running. No full old-head CI success is claimed.

### F03 exact Kubernetes watch resume

D277 replaces the previously ignored Kubernetes ResourceVersion with a direct
server watch for explicit nonzero cursors. Actual retained modifications and
deletions arrive in order without replaying current objects as Added. The
original guarded client and namespace are retained; private kinds watch only
labeled backing Secrets and filter decoded namespaces. Initial empty-cursor
informer behavior stays intact. The 256-event channel applies backpressure;
Stop, cancellation and driver Close terminate remote requests and blocked sends.

Compaction or malformed events close the stream with a terminal Err accessor;
opening errors return directly. There is no automatic relist or event/mutation
retry. Cursor 0 is refused for exact resume. SQLite's coalesced feed/compaction
relist remains its separately documented contract, not an exact-history claim.

Final selected real envtest/private-kind/initial-watch and resume races pass in
19.006s. They include replay of an already-deleted object's modification and
deletion, exact resource versions, namespace/private-kind filtering, driver Close,
cancellation under full result-buffer pressure, interrupted watch opening,
malformed events and explicit expired-stream classification. Scoped kube lint
reports zero issues. The earlier compile attempt used the wrong test-only
subject type; it was corrected before the successful final run.

### F40a typed host supervisor settings

`52ec29d` implements D274: 46 edge and 24 VIP keys hydrate the existing explicit
render/run commands. Defaults precede file, environment and explicitly changed
flags. False/zero/empty values, aliases and whole-collection replacement are
preserved; only the exact explicitly overridden key skips lower environment
parsing or file reads. VIP instances and preempt delays have typed shapes and
narrow JSON environment support. Configured zero priority retains provenance
through node discovery. VRIDs remain operator-owned, secrets stay references,
and no supervisor starts automatically from serve.

Every key is node-scoped/host-only/restart-required. Actual authenticated API
coverage refuses value changes and null resets on all 70 leaves through both
settings/node-config routes (280 cases), plus whole-section resets. Existing
source/PROXYv2 credential, TLS, frontend, renderer and legacy/HMAC refusals remain.
Agent full config races pass in 9.586s, selected CLI races in 3.646s (38 cases),
actual chart/Compose checks in 1.294s (32 cases), and scoped lint with zero issues.
Parent selected config/CLI races pass in 1.928s/3.552s. Generated reference drift
passes in 0.028s; the settings UI passes all 22 cases with the new mock schema.
An initial parent combined test invocation incorrectly passed the CLI-only
update flag to other packages; those packages refused the flag before testing.
Correct independent commands above pass; no timeout or assertion was weakened.

Hosted security 41214453550 and GitHub docs 41214453575 pass on 8bdb218.
CI 41214453524 has only Go shard 1/2 still running at this checkpoint, with other
jobs passed. Newer local commits are not yet qualified by that hosted run.

### F53 exact ClientGroup allow/deny scope

`926b2a3` implements D273. The baseline reproduction leaked both allow and deny
rules to adult traffic sharing a Policy with kids, across exact/wildcard/regex
entries. Private copied winning membership now filters list references before
immutable Selection construction, including fallback policies. Optional bounded
BlockList `clientGroupRefs` uses namespace-local groups. Reserved UI group names
retain their original scope before migration; convenience/current approval
writes materialize it. No request diagnostic GroupIDs, lower-tier memberships,
newer publication or query-path store read can widen scope.

Agent repeated full policy/domainlist/unblock/validator races pass in
1.618s/4.182s/26.177s/18.466s; repeated mounted/current-command races pass in
60.127s, API in 5.269s and scoped lint reports zero issues. Parent composed
mounted UDP/TCP, raw-cache, held-publication and current-command races pass in
29.461s. A separate actual Kubernetes API-server test passes in 18.112s,
proving deployed CRD rejection of contradictory reserved scope, duplicates,
invalid names and over-limit sets, and exact accepted readback without pruning.
It bypasses the store validator, so an embedded-only schema cannot mask drift.

Deepcopy and all three deployed/embedded BlockList CRDs are regenerated;
reference/schema drift passes in 0.029s/0.010s. Dashboard types include scope,
and saving list settings preserves it (all 20 list-detail cases pass, plus
typecheck and scoped lint). Original credentials, current-command final fences,
request Received/Deadline and compiled content ownership remain unchanged.
F54b remote-edge invalidation and other topology gates are still separate.

### Further F21b/F41a/F43b implementation evidence

`6400220` adds bounded exact-key raw cache metadata over REST with original
HTTP/resolver/cache scope, no DNS I/O or stored answer/subnet enumeration.
Mounted DNS/API races and actual SQLite held-cut/ABA tests pass. `1c934e7` and
`9990ede` add host-selected provider ownership observation and fix Node-incarnation
binding; real envtest source expiry/replacement and permission tests pass.
`e60001f` adds bounded dnsdist console reconciliation and immutable certificate
reload; actual pinned generated-supervisor acceptance passes all enabled TLS
frontends, held DoT, invalid material and uncertain mutation recovery. Fully
restart-free named additions remain open until their separate targeted-health
protocol acceptance passes. These updates do not certify physical ARP/BGP or
Windows/two-VM acceptance.

Hosted CI 41214453524 on 8bdb218 failed only its first Go shard because the command
fixture reported the independently canceled serving root as an unexpected writer
error. `0e8c3be` fixes that fixture after deterministic reproduction; 20 repeated
root-shutdown/final-credential races pass in 82.997s. Production authority and
deadlines were not changed. Security, GitHub docs and kind/MetalLB passed that
head. Newer commits still need their own hosted runs. Exact commands and limits
remain in the gap ledger; the complete original inventory is retained.

### F18b/F32/F40b/F43a integration evidence

`83c1096` supplies opt-in bounded asynchronous reverse-PTR fallback through the
original identification seam; actual UDP, host-only API and mounted SQLite
policy/expiry races pass. `ffcf20e`/`bd47907` persist supported SQLite node log
preferences through the original fenced transaction and refuse unsupported node
writes with read-only API/UI capability; repeated restart, real envtest and all
23 Settings UI cases pass. `380458f` connects the private bounded keepalived
journal to original-authorized audit/SSE, with real FIFO/router/token-expiry
acceptance. `88ca477` qualifies same-PID named backend additions through captured
node targeted health, with actual pinned YAML/Lua runtime acceptance and default
races. Legacy/structural restart fallback and physical-lab limits remain explicit.

`439ddc3` documents two narrow Semgrep false positives; all 593 pinned rules pass
locally. `e581fc8` corrects the hosted edge golden-fixture directory collision;
actual selected golden/supervisor checks pass. Fresh hosted evidence remains
required on the updated PR head. Exact outcomes, unsuccessful intermediate checks
and remaining scope are retained in gaps.md and the complete inventory.


### F65/F67 static deployment implementation, November 8

D300 mounts bounded host-selected static SQLite twins, with local independent
leadership and authenticated status/list bodies. API/UI object and credential
mutations, public unblock submissions and the external-dns mutation webhook are
refused. Actual two-runServe peer-loss/restart and single-dispatch local-control
acceptance passes. Seven affected packages pass1055 race entries with4 external
service skips; corrected full config passes340, vet and dashboard checks pass.
Earlier failures and exact bounds remain in gaps.md and
`.context/ha-static-20261108/`. Separate-process acceptance and configured lint
remain pending; enrollment/promotion and partition/rejoin are separate open
requirements. Shared internal credentials are not individual member proof.


### F65/F67 separate-process static acceptance, November 8

`TestStaticClusterProcessesReadOnlyPeerLossRestart` now starts two actual
server processes using the existing synthetic ten-node license fixture, with
separate SQLite files and original configured peer identities. UDP/TCP DNS,
authenticated status/digest reads, local leaders, distinct database identities,
read-only object/settings refusals and one node-local blocking command pass.
After A exits cleanly, B continues DNS and lists A without claimed readiness;
A restarts from its original database/config with identity and session intact.
Mutations are single attempts without replay or deadline extension.

The frozen-source race-enabled E2E harness passes1 top-level test, zero failures
or skips, child0/wrapper0, wall56.499293604s, minimum17,192,493,056B. Actual server
binaries use the existing CGO-disabled fixture build, without race instrumentation.
E2E package vet exits0. All frozen source identities match; exact receipts are in
`.context/ha-static-process-20261108/`. Existing TestTwoNodes and audit assertions
remain; only their shared licensed fixture name is generalized. Configured lint,
enrolled replication/promotion, partition/rejoin and final release gates remain
open. This evidence does not grant individual member authority from shared
internal credentials.


### F60b cluster status command, November 8

`kudurru cluster status` reads the existing authenticated `/cluster` route once
and renders the server's driver, leader, member count and read-only mode. Wide
output adds cluster ID/epoch; JSON/YAML preserve the original response fields.
Static twins report local leadership; status grants no quorum or promotion
eligibility. Invalid arguments/format/transport and canceled input do not dispatch.

Actual-router race9 entries pass with no skips/failures, child0/wrapper0,
wall23.441864966s, minimum17,223,516,160B. Generated CLI reference and command
package vet pass. The first race run's success fixtures used viewer without
cluster.read and were correctly refused (6 pass/3 failed entries); those results
remain. Success fixtures now use the existing administrator identity; permissions
and refusal assertions are unchanged. Exact frozen-source receipts are under
`.context/cluster-status-20261108/`. Configured lint remains unavailable.
Enrollment, join-token, promotion, forwarding and final HA acceptance remain open.


### F19b shared Kubernetes blocking runtime (D301), November 8

The actual Kubernetes server assembly now mounts the existing runtime annotation
contract on the release-namespace `kudurru-runtime` ConfigMap. Each command keeps
its original five-second-or-earlier context and rechecks authorization before
reading and before one UID/resourceVersion update or absent-state create.
Client retries are disabled. No-op commands still use the captured CAS; conflicts
and lost replies never acquire a new source or repeat the mutation. Confirmed
commits apply locally; unknown completion is explicit and cannot earn local
success credit. Independent later reads may observe the actual durable result.

All replicas use serialized bounded one-second read reconciliation and the same
absolute deadline. Repeated observations, including unrelated resourceVersion
changes, cannot rearm a locally consumed timer. Startup is enabled until first
observation; outages retain confirmed DNS state with its original expiry and
refuse unavailable runtime API reads. Changes retain the existing cache observer.
The API records the action once through the existing audit seam. Existing RBAC
already covers the runtime ConfigMap; no schema or privilege expansion.

Final production has driver6, API4, toggle1 and root failure-path2 actual race
passes. The same combined command retains its startup fixture failure. Final
actual two-runServe acceptance separately passes1 with real API-server state,
DNS on both replicas, one disable, restart without expiry renewal, refused
outage mutation, actual expiry and one enable from the other replica. Final
mounted child0/wrapper0, wall61.709555064s, minimum16,656,818,176B; affected
package vet exits0 in11.622499991s. No skips or race diagnostics in those passes.

Earlier mounted failures remain: implicit policy response, upstream edits absent
from the real settings source, and fast list startup preceding the initial DNS
assertion. Fixtures now select explicit policy/source and blocking list startup;
no mutation replay or original deadline extension was added. One earlier mounted
source also passed before the final unchanged-deadline observation guard.
The harmless controller-runtime missing-logger diagnostic remains in the log.
Exact source freezes, raw outcomes and failed sources are retained under
`.context/kube-blocking-20261108/`. Protected dashboard bytes remain unchanged.

This is eventual shared state, not synchronous acknowledgment by every node,
cluster-wide transactional IAM admission or same-transaction Kubernetes audit.
Configured lint remains unavailable and final driver/release gates remain open.


### F35a direct DNSEndpoint source (D302/D303), November 8

The optional host-only `externalDNS.crd` source now runs inside the original
elected controller term on Kubernetes. It lists/watches source CRs read-only
and derives same-namespace DNSRecordSets with exact source UID ownership,
deterministic names, native validation and each zone's recordSetSelector.
The longest selected primary-zone suffix wins. Invalid current source data
withdraws only its children; foreign owners remain intact and one conflict
does not starve unrelated sources. Original finite term contexts, source/zone
UID and resourceVersion checks, atomic creation, expected-version updates and
original UID/version deletes bound each operation. Lost replies and refusals
are not retried within that operation. Later reconciliation observes anew.

Actual source updates exposed Kubernetes' distinction between POST Update
ownership and SSA Apply ownership for the same manager. The store now permits
that transition only for exclusively self-owned fields under the exact original
resourceVersion. Foreign non-status field owners and intervening edits refuse.
Optional original UID/version deletion preconditions now cover memory, SQLite
and Kubernetes public/private objects; Force cannot bypass them. A stale SQLite
CommandTx delete poisons the entire command. Unguarded semantics remain intact.

Final production has 23 focused race passes with no failures/skips, including
real source permission/watch/update/refusal, cancellation, lost reply, foreign
ownership, replacement and actual runServe DNS/restart/deletion. Store conformance
and same-owner migration previously passed 56 entries on the unchanged final
store implementation. Legacy kube ownership/finalizer/rejection, external-dns
webhook/relay and generated-reference regressions pass 136 race entries with
no failures/skips. Vet passes. Helm renders the source through config.node,
keeps store.driver=kube and grants source get/list/watch only. No RBAC expansion.

Exact receipts and failed source archives are under
`.context/externaldns-crd-20261108/`. Final focused race child0/wrapper0,
wall71.913893334s, minimum15,052,296,192B; legacy regression child0/wrapper0,
wall27.051235979s, minimum15,104,389,120B; vet child0 in30.250234413s.
Earlier import collision, CRD fixture annotation failures (including one
accidental unchanged repeat), and actual same-manager SSA conflicts remain
preserved. Failed envtest children were identified and reaped. Helm succeeded;
the first optional Python YAML verification lacked its module, then exact
rendered blocks were verified without installing dependencies.

Source, zone and child remain separate Kubernetes API objects: this is an
eventual controller, not an atomic cross-object transaction or proof of
partition-safe distributed fencing. The minimal specification-only test CRD is
not an upstream CRD replacement. Disabling the adapter leaves existing data;
normal Kubernetes owner-reference garbage collection follows source deletion.
One mounted server restart is not multi-node lease-loss/partition acceptance.
Those cases, configured lint (unavailable executable), and final G09/G21/G22
and release gates remain open. Protected dashboard bytes are unchanged.


### F46 shared Kubernetes password admission (D304), November 8

The actual Kubernetes server now mounts one shared lockout owner for local
password login. One private release-namespace Secret atomically reserves both
normalized IP-prefix and username capacity before hashing, retaining the driver's
cluster-identity transport, original finite context and UID/resourceVersion CAS.
Five IP failures within five minutes lock for five minutes; ten username failures
within fifteen minutes lock for fifteen minutes. IPv6 uses the existing /64 key.
A successful password removes its own IP reservation and resets only older
completed username failures; other pending work and newer failures survive.
Durable sequence numbers distinguish even identical wall-time samples.

Unfinished reservations become failures at their original expiry. The document
has a 512KiB/4096-key maximum and only reclaims expired entries. HMAC keys under
its persisted random salt omit raw usernames/addresses; Secret access remains
sensitive. Clock rollback refuses rather than resetting counters. Every operation
retains an original bound of at most30s; unknown writes, conflicts, malformed state
and outages refuse without retries or local fallback. Committed-but-canceled
operations retain their effect and cannot return admission. Privileged Secret
removal resets counters. Existing Secret RBAC suffices; no new config or CRD.

Actual two-runServe acceptance proves a successful password, five failed logins
alternating replicas, both replicas refusing a correct password while locked,
restart persistence, unavailable shared reads returning503/no write or cookie,
and recovery preserving the lock. Real kube tests additionally verify original
CAS interposition, exactly one committed lost-reply reservation, no replay,
canceled input and a preexisting foreign Secret left unchanged. Pure tests cover
simultaneous reservations, later-failure preservation, successful username reset,
idle expiry, malformed/capacity refusal and original context/ticket consumption.

Final production has7 targeted race entries with zero failures/skips,
child0/wrapper0, wall35.457727048s, minimum14,743,908,352B. Earlier expanded pure9
and actual kube1 passes remain; full unchanged users/stack regression passes75
entries, zero failures/skips, wall14.226176477s. Final affected-package vet exits0
in4.811018429s, minimum15,063,453,696B. Exact freezes, logs and receipts are in
`.context/login-lockouts-20261108/`.

Earlier failures remain: the first mounted correct-password login returned503
because JSON omitted monotonic metadata carried by the original context deadline;
reservation expiry now normalizes to UTC before durable identity comparison,
without extending the deadline. That run also had a test variable-name compile
error. A later negative fixture tried to change an immutable Kubernetes Secret
type and was rejected; it now creates a separate preexisting foreign Secret.
No failed run is relabeled green. Protected dashboard bytes remain untouched.

This is the Kubernetes password mount only. Complete SQLite login/factor counters
still need same-command persistence and replication composition; OIDC initiation
limits are distinct. Shared counters do not grant credentials, TOTP replay
protection or cross-object IAM transactions. Configured lint remains unavailable;
F46's remaining driver/route work and final G12/G22/release gates stay open.


### F46 SQLite original-command password admission (D305), November 8

The mounted login now stages bounded shared IP-prefix/username reservations,
one password/TOTP verification, result and successful enrollment inside its
original currentAuthCommand. Only an exact package-issued, command-bound
pre-mutation credential refusal can commit failed counters. Final errors, clock
rollback, expiration and poisoned mutations roll back all effects. The earliest
original deadline/30s bound is retained; no second verification or new lease.

An optional versioned runtime row carries the complete 512KiB/4096-key document.
Prefix bounds precede allocation; unknown versions/malformed state refuse without
repair. Replica validation uses stored observation time. Legacy absence initializes
on first admission; privileged removal resets the operational limit. Safe object
exports omit it. Original policy/HMAC keys/concurrency semantics are unchanged.

Focused race-02 passes11 entries with no failures/skips (30.250527820s). Full
security/servingauth/runtimeowners/loginlimit default-race passes324 entries with
no failures/skips (77.315912087s, minimum14,762,717,184B). Affected vet exits0
(6.013013003s), and exact11-file gofmt check has empty output. Configured lint
is unavailable and vet is not its substitute.

Actual HTTP adapters alternate failures against one canonical Store, reject
locked correct passwords after adapter recreation, and carry exact counter bytes
through a separate physical follower's pre-captured install, Close/Open and
read-only authority refusal. Success, final guard failure, clock rollback and
ignored mutation poison check exact effects. This is not production forwarding.

The full55-top-level HTTP selection completed with58 pass entries and32 failed
entries, zero skips, numeric1 (268.668862522s, minimum14,658,461,696B). Failures
are browser-start503/expected302, including fixtures used by external-account
and RoleBinding tests. Source review identifies original-source currency after
an own pending/session commit as a candidate cause; investigation is ongoing.
No full HTTP/release green claim. All receipts and exact freezes are retained
under `.context/sqlite-login-lockouts-20261108/`.

Earlier race-01 failed because a success fixture used the password-change floor
itself; the corrected test uses a later time and asserts actual target entry.
Earlier final-error/clock apparent passes did not reach their intended target
and remain invalid-target evidence. No production acceptance check was weakened.
Self-service password/factor routes, OIDC admission, enrolled-replica forwarding,
configured lint and final F46/G12/G22/release gates remain open.


### Browser response after its own commit (D306), November 8

Full HTTP regression for D305 exposed 32 failed entries at browser-start 503,
with 58 passing entries. A new diagnostic proved that BeginBrowser had actually
committed its pending row, retiring the original source revision while all
captured dependencies remained current. Its postcommit Browser.Current check
therefore rejected its own successful command. The diagnostic's numeric1 and
all earlier failures remain preserved, rather than being treated as unrelated.

The private response helper now retains the exact original RuntimeOwner,
request lifetime and writer episode. It invokes unchanged original-source,
provider, HTTP and transaction gates. Only actual successful command completion
and final original-writer checks return a response-current predicate. That
predicate excludes the retired source revision but still rejects cancellation,
writer loss, term/incarnation changes and captured dependency replacement.
It grants no authentication, provider exchange or mutation. Start/callback still
observe their exact pending row/enrollment in a complete publication and check
original cookie policy and expiry before sealing. Final emission also checks the exact acknowledgment publication, so later revocation refuses without resealing. Local logout must commit
before the optional provider continuation; no provider is recaptured or retried.

Source-03 complete default-race HTTP selection passes 108 entries across 58 top-level tests,
zero failures/skips, child 0/wrapper 0, wall 371.588192269s,
minimum 14,489,436,160B. Includes actual TLS login/logout, frozen grants,
external administration, provider replacement during exchange, cookie-policy
replacement, invalid proofs, concurrency, ordinary password/factor/session
routes and the new exact completion targets. New negative targets stage a real
enrollment before callback/final/poison refusal and verify complete rollback;
original cancellation, writer stop, equal-key replacement and request release
invalidate a successful completion. Focused prior source-02 race 10 also passes.
Final source-04 adds a memory-only check of the exact publication used to seal
the response. A later canonical revocation invalidates that acknowledgment
even while the original dependencies remain installed; it does not reseal or
replay. All affected browser/external-account/frozen-grant/completion races
pass 52 entries across 23 top-level tests, zero failures/skips,
wall 146.833130272s, minimum 14,440,554,496B.
Final root vet exits 0, wall 2.005811144s. Receipts/freezes/original sources
are retained under `.context/oidc-own-commit-20261108/`. Configured lint remains
unavailable; full release and remaining F46 routes/forwarding gates stay open.


### F46 shared SQLite factor proof failures (D307), November 8

Authenticated TOTP setup, confirmation and disable now use the same bounded
shared IP-prefix/username counters as local login. Cookie, exact current account,
CSRF and existing local factor pacing remain admission prerequisites. Counters,
proof verification and factor effects share the original finite command; the
private security callback never receives a caller-claimed verification result.

Only command-bound password refusals or challenge/candidate-code refusals issued
before any authentication mutation can commit failures. Later conflicts,
enrollment/factor errors, original context expiry, clock rollback and poisoned
commands roll back counters and authentication state together. Valid proofs are
not verified a second time at final commit. The original five-IP/ten-username
policy and 512KiB/4096-key bounds remain unchanged. The one-second factor throttle
remains an additional local control, not a newly claimed distributed pace.

Real HTTP tests alternate failed setup/disable requests across separate adapters,
recreate one adapter and verify that setup, confirmation and login all refuse the
shared lock. Every failure changes only the versioned counter row; every other
payload field and object revision remain exact. Real challenge/code tests combine
setup, malformed challenge, disable and incorrect numeric code failures, preserve
factor refusal ownership across restart, and confirm exactly three password checks
across five attempts; challenge and candidate-code refusals occur before hashing.
Locked confirmation does no hash.
Valid activation and final-error/clock/poison cases assert actual target entry,
exact rollback or one committed factor/counter effect.

Focused default-race passes 24 entries, no failures/skips, wall
57.896847543s, minimum 14,382,419,968B. Full security/loginlimit
regression passes 110 entries, no test failures/skips, wall
81.331390614s, minimum 14,547,116,032B. The sqliteadapter package
has no direct test files; its actual transaction behavior is exercised by the
security and mounted HTTP tests. Affected vet exits 0 in
2.807594720s. Exact source freezes, original five-file archive and
receipts are in `.context/sqlite-factor-lockouts-20261108/`.

Three existing HTTP assertions now distinguish the intended operational counter
commit from unchanged authentication state, comparing the entire remaining
image and object revision; original declarations/messages and negative bodies
remain. Independent failure requests wait for publication of the actual previous
source before capturing their next request; no refused mutation is replayed.
Pure factor APIs retain their original no-op behavior and regression tests.
Self-service password changes, OIDC budgets, enrolled-node forwarding, configured
lint and final F46/G12/G22/release gates remain open. Kubernetes factor authority
is not implied by this complete-SQLite mount. No push or release.


### F46 SQLite self-service password admission (D308), November 8

Self-service password changes now share login and TOTP-management failure
counters. The original cookie/API-token and exact current account are verified
first; the current password and factor are admitted and verified once before any
password mutation. Invalid proofs commit only counters. All later failures abort
the entire command, including its counter result, password, factor consumption,
revocation floors, token deletions and replacement enrollment.

A private same-command enrollment helper uses the actual password decision just
verified before this operation's own password change. No caller can construct a
claimed decision to issue a session; token callers receive no cookie enrollment.
Exact local ownership, accepted token-provider aliases, must-change behavior and
original revocation-floor time remain. New-password hashing happens outside locks
before shared admission; only current-password/factor verification is admitted.
The opaque preparation is one-use, redacted and bound to the original request's
30-second-or-earlier deadline, retained through publication and response sealing.

Real HTTP adapters alternate invalid current passwords and invalid numeric TOTP
proofs. Each failure changes only the intended counter row, leaving the complete
remaining image exact. Recreating the adapter cannot bypass the lock. Actual TOTP
password change consumes its proof once. Private command tests reach staged
password/enrollment before callback/final/clock/poison/cancellation refusals and
assert exact rollback. Success revokes the old cookie and seals the replacement;
consumed preparation cannot be reused.

Final full security race: 105 test passes, no failures/skips, wall
103.992015973s, minimum 14,422,274,048B. Final focused HTTP race:
18 passes, no failures/skips, wall 107.030013562s, minimum
14,223,118,336B. Affected vet exits 0 in 4.011298767s;
formatting is clean. Earlier source01 focused race passed 19 entries. An overly
broad combined test selector was interrupted (child -15/wrapper 241); that raw
attempt is preserved and not credited as qualification. Source02 was then tested
with separate full-security and focused-HTTP commands.

Evidence, source freezes, archives and raw receipts are retained in
`.context/sqlite-password-change-lockouts-20261108/`. The issue.go preimage was
read from its exact committed blob immediately after that helper edit, as recorded
in the archive note; it is not claimed as a prior working-byte archive.
Configured lint remains unavailable. OIDC budgets, enrolled-replica forwarding,
Kubernetes factor semantics and final F46/G12/G22/release gates remain open.
No push or release.


### F46 shared SQLite browser request budgets (D309), November 8

Browser starts, callbacks and CLI preparations now charge one bounded replicated
request-budget document before the first complete HTTP lease capture and before
credential interpretation. This non-granting command modifies only operational
counters. It does not authorize a provider, interpret a cookie, acquire an
identity, or turn the D306 response predicate into a continuation. Only confirmed
charge completion reaches the existing adapter's initial authentication capture.

The original 30-second-or-earlier request context and exact elected writer remain
through dispatch, provider I/O and response. Nested writer binding cannot adopt a
successor, even with the same term/incarnation; provider handles narrow their
original source currency with the retained writer check. Final command/context/
clock/poison failures roll back charges. A charge that committed before later
cancellation remains charged and cannot dispatch or be refunded/retried.

Policies retain thresholds of five starts, ten callbacks and five CLI preparations
per normalized IP prefix. Each lane resets after a one-minute idle gap; reaching
its threshold starts a separate one-minute lock. One optional
`runtime/browser-admission/v1/state` document retains salted hashes, fixed-version
policy and source time, bounded by 512KiB/4096 live keys across lanes. Unknown,
empty, malformed or oversized state refuses; live history is never evicted.
Legacy absence initializes, and privileged removal explicitly resets this
operational history. Complete follower install/restart retains exact bytes;
followers cannot charge locally. Safe object exports omit this runtime state.

The existing local per-attempt latch and concurrency caps remain additional
controls; no distributed exchange-uniqueness claim is made. A second admitted
callback's counter commit can retire the first callback's original source during
provider I/O. That first request returns 503 without a second exchange or session.
The concurrent-callback test now proves the intervening commit changed only the
budget, retains the one-exchange assertion and requires zero sessions. Its original
302 expectation and source01 failure (27 pass/two failed entries) are archived.
Independent actual TLS single-callback success remains positive.

Full requestlimit/runtime-owner default-race: 45 passes, no failures/skips,
wall 6.613112600s, minimum 14,272,958,464B. The broad source02
browser/CLI/logout/writer run retained 98 passes, one failure and one skip, wall
319.369973289s. The failing CLI final-clock fixture lacked the original
primary runtime in its private clock-only clone; the fixture now carries that
same actual primary. Its original final-clock refusal assertion is unchanged.
The skipped installed-Chrome case was subsequently executed explicitly.

Final source03 focused default-race passes 32 entries, no failures/skips,
wall 71.328128825s, minimum 14,047,240,192B. It includes the actual
installed Chrome/Playwright TLS browser, corrected final-clock refusal, real
browser success/concurrent source refusal, shared adapters/follower restart,
missing-writer refusal, original cancellation/equal-fence replacement and
transaction rollback/capacity/privacy targets. Affected vet exits 0 in
2.206111614s. Formatting is clean; configured golangci-lint remains
unavailable. All raw outcomes, frozen sources, original fixtures and browser
input hashes are retained under `.context/sqlite-browser-admission-20261108/`.

This is the complete SQLite HTTP mount. Kubernetes browser admission, enrolled-
replica forwarding and final F46/G12/G22/release gates remain open. Earlier F20
performance failures and unrelated runtime/external acceptance gates are unchanged.
No push or release.


### F46 Kubernetes browser request admission (D310), November 8

The actual Kubernetes server charges starts and callbacks before selecting an
OIDC provider, including unknown-provider requests. It uses the same D309
five-start/ten-callback prefix policies, one-minute lock, salted address keys and
512KiB/4096-key bounds. The separate release-namespace Secret
`kudurru-browser-admission` must have the exact private type, UID, resource version,
shape and no owner references or deletion timestamp. Confirmed absence may create;
privileged deletion resets this operational history.

One original request context, bounded by thirty seconds or an earlier deadline,
spans the direct read, exactly one versioned create/update and provider dispatch.
There is no write retry, cache or local-counter fallback. Conflicts, malformed
state, API outages and uncertain write replies refuse dispatch. A committed charge
survives later cancellation, clock failure or a lost response without refund.
This adds request admission, not a cross-object authentication transaction or a
shared TOTP replay authority. Enrolled SQLite forwarding remains separate.

Default-race evidence: backend/document and actual API-server tests pass 22
entries; two actual runServe replica/restart/outage tests pass; the full Stack
suite passes 46 entries. All have zero failures/skips and exact frozen input
matches. Wall times are 18.480313138s, 49.101403396s and 21.055286751s;
minimum free bytes are 14,020,022,272, 13,847,126,016 and 14,116,102,144.
Actual Kubernetes tests exercise alternating clients, restart, original-version
conflicts, retained lost-reply writes, canceled requests and foreign Secrets.
The mounted route checks both lanes, unknown providers, no cookies, Retry-After
and admission-store outages. Affected vet exits 0 in 4.010559710s. Existing
formatting receipt is clean; configured golangci-lint remains unavailable.
Receipts and input identities: `.context/kube-browser-admission-20261108/`.

F46 enrolled-node forwarding and final driver/release qualification remain open.
The historical F20 performance failures and other external acceptance gates are
unchanged. No push, deployment or release.


### F25b CID-routed QUIC socket groups (D311), November 8

Linux DoQ and HTTP3 now honor `listeners.sockets` with immutable 1–256-socket
reuse-port groups and a destination-CID BPF selector. Each server CID has one
shard byte and 120 random bits. Shared token protection supports Retry across
sockets; application connection/worker caps remain shared. Handshake limits are
per library transport. An exclusive first bind is retained through reuse-port
setup; bind/filter failure refuses startup and any accept failure closes the
whole group. Other platforms retain one socket.

Actual IPv4/IPv6 kernel routing, every shard's TLS/QUIC v1/v2 with and without
Retry, and real DoQ/HTTP3 exchanges across three successive migrations are
covered, with single-socket controls. Partial binds, simultaneous startups,
limits and cleanup are covered. Upstream quic-go v0.63.0 failed the third
migration even on one socket. Its path manager discarded the active probing
CID's ownership while clearing candidate paths. The pinned MIT module retains
all 500 original files and hashes; only `path_manager.go` changes, retaining the
active probing ID and retiring it on the next switch. A new twelve-switch test
uses two probing CID slots; original upstream assertions remain unchanged.
Docker dependency resolution includes the local replacement. No module-cache
edit or upstream acceptance is claimed.

Final frozen-source default-race results: upstream path/CID tests 39 passes,
zero skips (11.421829848s); complete dnsserver/config 706 passes, four existing
spike/measurement skips (56.692364702s); actual runServe signed-transport and
SQLite encryption/restart test one pass (50.283144008s). No failures or race
diagnostics. Minimum free bytes respectively 13,898,244,096, 13,652,758,528 and
13,433,724,928. Affected vet exits 0 (19.833713570s). Configuration reference
generation exits 0; docs drift tests pass four entries. Mock help matches the
schema exactly. Configured golangci-lint remains unavailable.

One fresh local 1s/case benchmark on two 2.90GHz cores records DoQ one/four
sockets at 55,708/48,841ns (12.3268% lower) and HTTP3 at 70,311/79,129ns
(12.5414% higher). Allocations are 138 and 262 respectively for both counts.
The adverse HTTP3 result is retained. This is not a fixed-class throughput or
release-performance qualification. Earlier migration failures, the refused
module-cache overlay, and the diagnostic candidate that retired all probing
IDs immediately remain preserved separately; that candidate is not final code.

Evidence and exact inputs: `.context/quic-multisocket-20261108/`. Frozen final
Go source matches all race/vet/benchmark receipts; later changes are documentation
and mock help only. User dashboard/dist bytes remain untouched. F25b behavior is
implemented; configured lint, fixed-class and final integrated release gates
remain open. No push, deployment or release.


### Integrated configured lint restored, November 8

Built golangci-lint v2.15.0 (the AD workflow's pinned version) with Go 1.27.3
into a private persistent tool directory. The complete repository scan uses the
unchanged `.golangci.yml`, concurrency 2 and all packages, with issue truncation
disabled. Final exit 0, zero issues, 54.091007994s, minimum free 11,519,586,304B.
Original complete inventory had 39 findings; its numeric failure and the earlier
36-entry truncated report remain preserved. A subsequent two-finding failure
also remains preserved.

Corrections cover test error handling, identifiers, public documentation and the
current Kubernetes FieldsV1 accessor. The QUIC whole-group test now explicitly
requires a socket failure error. Actual compiled-program instruction/state limits,
range-index invariants, deterministic test randomness and disposable credentials
justify narrow line-specific analyzer annotations. The private uncancelable
matcher path intentionally passes nil to its own helper; context-aware diagnostics
retain their original context. No analyzer is disabled in repository configuration.
A Go scanner comparison proves identical noncomment production tokens in all
five touched production files. No matcher kernel, capacity, proof, default,
benchmark, baseline or algorithm changed.

Default-race verification passes the three QUIC socket-failure entries and 13
changed follower-cancellation, fingerprint-oracle and actual API-server ownership
entries, with zero failures/skips. Wall times 6.213863577s and 41.267271066s;
minimum free 11,901,157,376B and 11,388,620,800B. Final production-token check and
whitespace check pass. All commands verify frozen source identities; user
`dashboard/dist/index.html` bytes remain unchanged. The first archive parser
attempt failed before source edits, and a first Go token-check invocation rejected
its file arguments before running; both diagnostic failures are retained.

Evidence: `.context/lint-restoration-20261108/`. This supersedes unavailable-lint
status for the current root source. Historical F20 relative/missing-scope/CPU
failures, incomplete F28 activation, HA/AD implementation and external/final
release gates remain open. No push, deployment or release.


### Composed storage, serving and central qualification, November 8

On integrated commit `19d6c7297e2d85a9c17c5229fa1a43af81a98127`, full
SQLite/trust-anchor default-race suites pass 964 entries across six packages;
serving/build/primary-runtime/runtime-owner suites pass 477 across eight packages.
Both have zero failures or skips. Wall times are 290.880184649s and
266.968804250s, with minimum free bytes 11,312,537,600 and 10,999,070,720.

Full central/audit races use the actual pinned PostgreSQL 17 image and pass
244 entries with zero failures. Their one measurement-test skip is subsequently
run explicitly: `TestDomainHourMillionCentral` ingests the original one million
rows with 252,000 distinct names and passes under default race detection, with
zero failures/skips. The ordinary and scale commands take 110.196661470s and
420.164267912s, with minimum free bytes 10,956,136,448 and 9,743,323,136.
Two package-level no-test-files markers are separate from the measurement skip.
Only the owned PostgreSQL container and its volumes are removed afterward;
the existing isolated Docker daemon and other retained fixtures are preserved.

`make size` builds an actual stripped, static CGO-disabled linux/amd64 ELF of
80,384,160 bytes, below the unchanged 100,000,000-byte ceiling. Version smoke
reports 0.2.0, commit 19d6c729, Go 1.27.3. ELF metadata confirms no dynamic
interpreter. This private artifact includes the unchanged user dashboard bundle;
it is not a clean published release artifact.

All commands match the same 4,530 tracked-file source freeze, including the
protected user dashboard bytes. Numeric outcomes are persisted before enrichment.
Evidence: `.context/integrated-qualification-20261108/`. These composed passes do
not complete F20 performance, F28 activation, enrolled HA, AD implementation,
Windows/fixed-class acceptance, hosted CI or the remaining release gates.
No push, deployment, release or waived gate.


### Complete local benchmark scope and indexed-literal refinement, November 8

On 859bacf6, all ten original fresh native processes (five blocking, five zone)
exit 0 with unchanged workloads, baseline, limits and 20%/noise-floor rules.
GOMAXPROCS=6 and the reported 2.90GHz CPU model match the committed baseline;
this sandbox is still not a named fixed-class runner. The original Go benchgate
actually runs and exits 1: 96 ok, 12 improved, four regressions, 300 new metrics,
zero missing metrics and zero absolute-limit violations. There are 99 case names,
439 metric vectors and 2,195 values, exactly five per vector. The prior missing
zone scope is now measured, without rewriting any historical failure.

The four failed medians are exact-list miss p50 163ns versus 109ns; regex-1000
miss p99 18,434ns versus 12,661ns; suffix-list miss average 175.9ns versus
140.8ns; and zone CNAME p99 1,545ns versus 1,128ns. Unchanged-list compilation
is 72,818,058ns with eight allocations; steady 3M RSS is 92.58MiB, below the
original 150MiB ceiling. This suffix/exact RSS fixture does not qualify regex,
complete serving or simultaneously held old/new snapshots. All native outcomes,
full vectors and the numeric gate failure are retained in
`.context/current-benchmark-gate-20261108/`.

An actual CPU profile of the unchanged original regex-1000 miss workload attributes
34.58% cumulative sampled CPU to indexed batch exclusion. A coincidental selected
gram sent a name through all indexed literals. The new refinement uses that same
index to require at least one complete original literal before scanning its lane.
It keeps existing sparse dispatch, rule ordering/tags, Visit callbacks, Unicode
bypass, programs, cache limits, proof budgets and retained accounting unchanged.
The existing gram tests and benchmark sources remain byte-identical.

Default-race checks pass 93 focused entries with no skips and 656 full
blocking/parser/matcher/list-fetcher entries with two actual skips (memory helper
and unconfigured external comparator). A separate actual pinned AdGuard v0.107.79
race passes all 46 entries without skips; documented deviations are unchanged.
Independent automaton-oracle fuzzing passes 255 baseline seeds and 197,295
coordinator executions in 20s requested time; internal input skips are not
reported as valid-language comparisons. Vet and configured scoped lint exit 0.
All commands match frozen source. Receipts: `.context/indexed-literal-20261108/`.

The same original 3s CPU-profile workload before/after records 898.7/600.9ns
average and 18,226/1,455ns p99, both zero B/op and allocations. This diagnostic
supports the refinement but is not a replacement five-process gate. All four
pre-fix failures and earlier F20 failures remain evidence. A prepared CRC
optimization remains unmerged because unchanged compilation passed the actual
gate. Final-source five-process and fixed-class qualification remain open.
No push, deployment, release or baseline change.


### Benchmark evidence validation, November 8

`make bench` and `bench-baseline` now require exactly BENCH_COUNT measurements
for every current metric. The gate also offers explicit processor-suffix checks;
strict CPU mode requires actual CPU headers. Parsing refuses non-finite/negative
values, malformed result rows, duplicate units, invalid iterations and mixed CPU
headers. Thresholds and absolute maxima cannot be NaN, infinity or negative.
No baseline, corpus, limit, noise floor, ignore rule or measurement command changes.

Default-race tests pass 25 entries without failures/skips; vet and configured
scoped lint pass. Tests cover partial/extra samples, missing individual metrics,
mixed processors, missing/mismatched CPUs, native failure and actual regression.
The final tool checks all retained original five-process data with samples=5,
procs=6 and strict CPU enabled: exit 1 and byte-identical full findings to the
original gate, preserving all four regressions. The Makefile dry run retains all
ten original native commands. Evidence: `.context/benchgate-integrity-20261108/`.

These integrity checks do not certify fresh-process provenance or a named runner
class. F21c/F63 calibration, remaining measured regressions and release gates
remain open. No baseline refresh, push, deployment or release.


### CNAME answer allocation, November 8

The original zone CNAME profile shows answer assembly and slice growth in the
lookup path. A first alias now reserves two RR slots, so the ordinary alias plus
one address answer allocates once instead of twice. Additional hops and larger
RRsets retain normal bounded growth. Query-type behavior, ownership callbacks,
hop budgets, wildcard copies, zone cuts and answer ordering are unchanged.
A new allocation assertion also exercises an actual ownership refusal before
following the target; all prior lookup and benchmark fixtures remain unchanged.

Full zone/zone-cache/primary-zone/authoritative-stage default races pass 659
entries, zero failures/skips, 30.851530642s and minimum free 10,667,352,064B.
Actual Kubernetes assets are enabled for the cache suite. Affected vet and
configured scoped lint exit 0. Evidence: `.context/query-tail-20261108/`.
The original 3s profiled CNAME sample records two allocations before and one
after; its after sample is 885.2ns average and 1,774ns p99. Those diagnostic
samples do not clear the original five-process 1,545ns/1,128ns median regression.
The independent pure-list miss profile points mainly to filter memory access;
no speculative Bloom-filter or nil-receiver rewrite is applied. Final-source
five-process qualification remains required. No baseline, limit or corpus edits.

### Integrated five-process benchmark after literal and CNAME fixes

All ten original native commands completed with exit 0, all groups reaped,
no source mismatches or resource stops. Five fresh blocking processes and five
fresh zone processes supply 99 benchmark cases, 439 metrics and 2,195 values.
The actual Go benchgate exits **1** with unchanged baseline, limits, 20% threshold,
five-sample count, six-processor suffix and strict CPU checks. Both CPU headers
report Intel Xeon at 2.90 GHz; this is not a named fixed-runner qualification.

Findings: 97 ok, 1 regression, 14 improved, 300 new; no missing metrics or
absolute-limit violations. The remaining relative failure is exact-list miss
p50: 158 ns versus 109 ns (+44.9541%). Its five values are 151, 158, 166, 165,
and 95 ns; the low fifth sample does not replace any prior sample.

Regex-1000 miss p99 is 1,593 ns, down from the earlier 18,434 ns median.
CNAME lookup p99 is 1,291 ns, with exactly one allocation in all five processes;
the earlier CNAME failure remains retained. Suffix miss Evaluate is 167.4 ns,
just within the original relative threshold. Unchanged 1M compilation is
72,382,832 ns and eight allocations. Steady 3M suffix/exact RSS is 92.54 MiB;
this does not measure whole-serving or retained old+new regex RSS.

Native wall time totals 668.472929887 seconds; sampled minimum free space is
10,866,843,648 bytes. Every raw per-process receipt preceded enrichment. Full
vectors and all gate findings remain beside this report. Prior full gate FAIL
on 859bacf6, isolated F20 failures, calibration gaps and external acceptance
remain unchanged. No benchmark, corpus, threshold, baseline or limit was edited.

Evidence: `.context/final-benchmark-gate-20261108/`.

### Release benchmark collection and baseline integrity

A manual workflow now accepts the owner's explicit runner label and collects
five fresh processes each for original blocking, zone, cache, upstream-strategy,
UDP/DoT/QUIC and Persist200k suites. Separate reports keep blocking helper ignore
rules from hiding the added suites. The collector preserves raw numeric failures,
resource stops and source/fixture identities, refuses output replacement, and
requires matching class/toolchain/commands/fixtures plus intact report hashes
for comparison. It never rewrites the committed baseline or declares release
qualification. Named-class execution, calibration of new absolute limits and
final hosted evidence remain open; the 40-command collector has not been run.

The actual Go gate now rejects failed/empty baselines and applies explicit sample
and processor controls to both inputs. Socket q/s is higher-is-better; packet
loss retains its original lower-is-better direction. Four collector control tests
pass, including an actual nonzero Python child; gate default races pass 37 entries
with zero skips, plus vet and installed scoped lint. Both workflow files parse
with the existing YAML v3 dependency. A missing Python YAML module and the first
helper's YAML 1.1 interpretation of `on` are retained as validation-tool failures;
no workflow input was changed to conceal them. Rechecking the retained full
benchmark output produces byte-identical findings and exit 1. No threshold,
noise floor, ignore rule, absolute limit, benchmark or corpus changed.

Evidence: `.context/release-benchmark-tools-20261108/`; commands and limitations
are documented in `bench/README.md`. F21c/F63 and final performance gates remain open.


### Replication admission and final command expiry, November 8

Inbound peer authorization now retains the original finite request context and
concurrency slot; canceled or expired successful authorization cannot dispatch.
Coordinator pulls, status and promotion include queueing in that original bound;
expired waiters invoke no dependency callbacks and grant no eligibility.
Forwarded command envelopes now register expiry after domain final checks at the
actual SQLite commit/no-op boundary, including retained dedup outcomes. Failed
check registration refuses and rolls back the whole command.

The archived original command/SQLite adapter reproduces expired status-200
mutation and dedup outcomes plus missing final-check capacity refusal (numeric 1,
four failed entries). Final default races pass 160 entries / 83 top-level tests
across cluster, internalapi, replica and follower, with no failures/skips; the
actual Kubernetes Lease handoff uses installed 1.37.0 API-server/etcd binaries.
Vet and configured scoped lint pass. Earlier 159-pass/one-envtest-skip and a
lint error in new test cleanup remain retained; the final run includes that test
correction. This closes these admission defects, not enrollment, operator
commit authorization, forwarding mounts or HA partition/rejoin acceptance.

Evidence: `.context/replica-admission-20261108/` and
`.context/replica-command-expiry-20261108/`. F60b/G11/G22 remain open.


### Expanded release workload measurements, November 8

The new collector ran once on committed 63a52742 in the isolated clean checkout:
all 40 original native commands and seven original Go shape/absolute checks exit
0, with five fresh processes per suite, no retries/replacements, resource stops
or remaining process groups. All 4,537 source identities match. The eight suites
produce 136 case names, 592 metric keys and 2,960 values; every vector has five
measurements. Native wall times total 1,112.418948899 seconds and sampled minimum
free space is 10,477,436,928 bytes. Raw numeric outcomes precede enrichment.

A separate actual Go comparison against unchanged baseline/limits exits **1**:
95 ok, 14 improved, three regressions, 300 new, no missing original metrics or
absolute-limit violations. Suffix miss latency 307.4 vs 255.1 ns/op, p50 221 vs
170 ns, and Evaluate 172.5 vs 140.8 ns/op fail the original relative arithmetic.
Earlier exact-miss and all older failures remain recorded; no sample was replaced.
Unchanged-1M median is 71,770,967 ns/8 allocations, regex-1000 p99 1,578 ns,
steady 3M suffix/exact RSS 92.97 MiB and separate peak 140.8 MiB.

UDP one/six-socket medians are 199,535/270,035 queries/s; persistent DoT
236,547/211,708, with zero loss/failure values in all reported samples. DoQ
one/four sockets are 20,487/22,092 ns/op and HTTP3 31,360/29,628; additional
sockets are not uniformly faster. Original Persist200k save/restore and all
cache/upstream vectors and allocations are retained without inventing new limits.

The explicit class is local-sandbox-diagnostic (reported 2.90 GHz, GOMAXPROCS 6).
The recording result does not override the failed original relative gate or
establish a named fixed class. F21c/F63 class selection, actual calibration,
new absolute limits, hosted and final release qualification remain open.
Evidence: `.context/release-expanded-measurements-20261108/result.md`, every
native receipt/log, all-vectors.csv and original-baseline-gate.json/log.


### November 8 — SQLite TSIG management composition

Permission-checked metadata/create/replace/overlap-removal/revoke routes now mount
through the actual SQLite command facade and complete serving owner. Generated
OpenAPI, Go and TypeScript clients include these routes. Provider lifetime,
original writer, same-transaction source/credential checks, redacted audit and
exact committed-publication observation remain mandatory. Architecture and
operator documentation distinguish durable receipt from serving acknowledgment.

Ordinary composition passed; new final-clock and physical deferred-FK commit
error tests passed (18 entries). The broad core race exposed two contract
updates (resource count and virtual TSIG SAR mapping), now corrected. The
subsequent mounted selection passed 64 entries but FAILED four entries: two
pending-startup subcases plus their parent, and native UPDATE. Startup capture
failure currently drops the expected empty unsigned SERVFAIL. The native UPDATE
advanced the durable head while its peer received EOF: the original data
generation output guard rejects the command's own commit. No wire acknowledgment
is credited and the mutation was not replayed. These response-path corrections
are the next production task; F07a remains incomplete.

All three earlier external API skips were executed against real PostgreSQL and
Kubernetes envtest: default race passed 17 entries with no skips. Full configured
lint passes after two test-only switch cleanups; affected-package vet and
dashboard typecheck pass. Original failures and every source freeze remain in
`.context/tsig-management-integration-20261108/`. Production Kubernetes authority,
remaining original final-authority cases and release gates remain open.


### November 8 — native UPDATE acknowledgment and startup refusal

D313 separates the exact empty response returned by the actual root UPDATE
adapter from ordinary DNS data. The private completion retains original writer,
verified immutable keys/dependency installations, request/signature deadlines and
pre-command mode/retirement read attempt. Binding after the pipeline rejects
changed response codes, replaced messages, answer data and transfers. Ordinary
queries retain their complete generation check; a separate UPDATE invalidates a
held query. No command retry, replacement capture or new Store lookup is added.
Unavailable startup captures emit only empty unsigned SERVFAIL through the
original transport deadline, with partial lease cleanup and no baseline signer.

The unchanged original startup/native UPDATE-NOTIFY-transfer tests now pass.
Full DNS-server and primaryruntime race qualification passed 581 entries with
four existing measurement/helper skips, no failures (133.607475977 seconds).
The broad mounted race passed 36 entries, including actual background TCP/TLS,
management and encrypted restart, native transfer and anchor barriers. It failed
the deliberate writer-fencing test and its parent because the optional fixture
classified the intended driver exit as unexpected. That outcome is preserved.
The fixture now accepts only the explicitly expected loss of its original term
with the actual unavailable error. The corrected output/primary selection passes
14 entries, no failures/skips (55.705224212 seconds), including signed TCP/UDP
acknowledgments and final data/key/writer/cleanup/deadline refusals. Production
bytes are unchanged between these race selections. Full configured root lint
passes (83.564644963 seconds, zero issues); affected-package vet passes.

Earlier new-fixture missing-preparation-bound failures and the stopped original
run remain in `.context/dns-response-admission-20261108/`; no original deadlines
or production guards were relaxed. These scoped checks do not complete the
remaining F07a/Kubernetes, managed activation, HA, performance or release gates.


### November 8 — Kubernetes immutable stored TSIG startup mount

D314 adds a single asynchronous preparation under the original startup deadline:
actual authenticated cluster identity, complete bounded private source, then full
decrypt/merge with the captured encryption and file inputs. Unknown and failed
sources cannot authorize file-only signing. Successful keys are immutable until
restart; no query-time Store lookup, late credential adoption, hot administration
or cross-resource transaction authority is introduced. Unrelated unsigned
saved-state DNS and unsigned secondary acquisition remain independent.

All six native/encrypted transports select the actual StaticKeyring before
verification without claiming a complete serving lease. Secondary jobs preserve
stored credential epochs and original acquired expiry. NOTIFY selects immutable
keys before network work; only its initial round waits for preparation. An actual
envtest/runServe test verifies online/restart signatures, signed SOA/transfer and
NOTIFY, API-outage and wrong-owner refusal, and unchanged persisted acquisition
metadata when no complete source is available. It passed default race with no
skip (38.275064642 seconds). Focused default race passed 18 entries with no skips;
full DNS-server/secondary/notifier/tsigkeys races passed 650 entries with exactly
four existing measurement/helper skips, no failures (55.162494342 seconds).

Original Kubernetes offline and SQLite TSIG compatibility races passed 31 entries,
no failures/skips (203.367148342 seconds), including native UPDATE acknowledgments,
background TCP/TLS, encryption restart, pending-source refusal and original expiry.
Full configured root lint passed with zero issues (87.161939136 seconds); affected
vet passed (3.611785218 seconds). Production bytes stayed unchanged across these
checks. Source01's optional-fixture failures (signed-only HTTP helper applied to
unsigned queries; mixed-case key assertion and missing secondary primaries) remain
preserved, with source02 corrections limited to the new tests. The mounted test
alone expanded in source03. Receipts/freezes live in
`.context/kube-tsig-startup-20261108/`.

This is startup-only source activation. Kubernetes management/current mutation
authority, remaining F07 acceptance, managed-root activation, HA, AD, all retained
performance failures and external/final release gates remain open. No push or
release publication follows from these checks.


## November 8: retained managed edge cache policy (D315)

The supervisor now exclusively owns its configuration-directory inode from
before preparation through worker and child shutdown. First managed startup
persists one private versioned bypass identity, syncing the file and directory
ancestry before child startup. Restart retains that identity; disabling bypass,
malformed or indirect storage refuses without repair. Failed preparation does
not erase the policy. Actual installed evidence still requires the loaded child,
both console detachments and the original source/child epoch.

Evidence: `.context/edge-managed-durability-20261108/`. The first focused command
failed compilation on a new integer-type mismatch, before any tests executed;
that receipt is retained. After the correction, focused default race passes 21
entries with the opt-in live case skipped. Actual pinned dnsdist YAML and Lua
acceptance then passes all three entries without skips, including full supervisor
restart on the same directory, disabled-bypass refusal and uncached postrestart
queries. Full edge default race passes 189 entries with two explicitly opt-in
live skips; the managed-cache live case was exercised separately.

This closes a restart prerequisite, not F28 activation. The authenticated
backend-to-edge exchange, original activation admission, elected/follower mounts,
complete retirement/interoperability and final release gates remain open. Storage
must persist for the deployment; filesystem replacement/rollback, independent
processes and orphan daemons outside the supported PID-1 container lifecycle
are not fenced by an advisory directory lock.

Configured scoped lint passes with zero issues after six specific gosec findings
were annotated: one public file-format tag and five operator/test-owned paths.
No behavior changed after race/live qualification; the first lint failure is
retained. Source hashes and raw outcomes are in the evidence directory.


## November 8: authenticated managed edge observations (D316)

The optional host-only `edge.managedObservationAddress` listener exposes the
actual retained deployment, fresh supervisor incarnation, acknowledged child/
source epochs and detached backend membership through the existing authenticated
internal TLS guard. One fresh challenge, expected node/deployment, original
2-second total deadline, 128-member and 128 KiB limits bound acquisition.
Substituted/malformed/redirected/expired observations refuse without replacement.
The listener and zone polling share one captured credential source. Listener
failure cancels and joins the supervisor and other listener.

Evidence: `.context/edge-managed-observation-20261108/`. Focused default race
passes 26 entries without skips. Actual pinned dnsdist YAML/Lua acceptance passes
three entries without skips, exercising the authenticated exchange after loaded
policy/source/child acknowledgment and full supervisor restart. Full internal
API, edge and configuration default race passes 596 entries, with only the two
opt-in console/managed-live cases skipped; managed-live ran separately. Root
bind-failure coverage is lifecycle evidence with a script child, not dnsdist
cache qualification. The CLI and configuration references are regenerated,
including the prior D315 help text omitted from its commit.

These observations grant no backend activation or mutation authority. Explicit
exhaustive topology, original local activation admission, updater/follower
mounts and the complete F28 retirement/interoperability requirements remain open.
All previous release, performance and external acceptance failures remain open.

Root edge/supervisor configuration and generated-reference default race passes
46 entries without failures/skips. The first full configured lint command fails
one missing exported-error-group comment; its receipt is retained. Only the
missing comment and explanatory node-name documentation changed afterward.

The final full configured repository lint passes with zero issues in 58.4938 s
on source04. All code behavior and tests match the qualified source; the sole
post-race Go edit is the exported-error-group documentation comment.


### Original complete edge fleet collector

The private root collector now validates the entire explicit nonempty fleet before
network acquisition, detaches requirements, verifies exact local backend tuples
on every authenticated response and issues only one complete result. Partial
failure cannot retry earlier peers or proceed to later peers. Original caller
and runtime cancellation, issued pointer identity and finite lifetime remain
mandatory. Focused default race passes 17 entries without failures/skips; evidence
is `.context/anchor-edge-fleet-20261108/`. The collector is unmounted and supplies
no activation grant. The explicit topology/lifecycle contract, input adoption,
terminal writer/follower behavior and full F28 acceptance remain open.

Scoped configured lint passes after renaming one test variable that shadowed a
builtin; the first lint failure is retained, and production bytes did not change.
Source review also confirms two independent automatic retry layers, RunWriter
and cluster.Runner.superviseLeader. A future terminal anchor outcome must survive
both without stopping unrelated writer work. This remains a mounting constraint,
not a claim that the current unmounted worker is active.


### F28 terminal writer state across both retry layers

A runtime-owned terminal record now survives host replacement and cluster
Runner restart within the original elected term/incarnation. The failed anchor
child stays joined until cancellation, leaving unrelated writer services alive.
Completed-receipt cancellation remains terminal and preserves original error
identity. A canceled incomplete socket joins without inventing a receipt; only
a genuinely new term/incarnation clears the terminal record. This adapter is
still unmounted; managed activation and full F28 acceptance remain open.

Evidence: `.context/anchor-writer-terminal-20261108/`. Initial focused race
failed one new test because its server accepted only one exchange. That fixture
now accepts and holds both original canceled callers. The correction also adds
a real signed receipt with final-pin cancellation test. Focused race passes 10
entries; composed Anchor/RootAnchor/ServingRuntime race passes 92, no skips or
failures. Vet exits 0. First scoped lint failed one unused test parameter;
renaming only that parameter to underscore yields configured lint exit 0 with
zero issues (23.6383 s). Production source matches the successful race/vet cut.
Prior failures and all performance/external gates remain retained.


### F28 direct standalone runtime activation (D317)

Explicit host-only `upstreams.recursion.managedTopology: direct` now mounts the
actual elected updater and managed complete builder on standalone SQLite. It
requires no PROXYv2 ingress and an exhaustive operator declaration that clients
reach the node without a caching proxy. The private admission belongs to one
root owner/runtime, survives successful preparation cancellation and is retained
by managed publication. Disabled topology, unsupported stores and enrolled roles
cannot activate; remote edge and enrolled HA acceptance remain open.

The first bounded anchor step completes before unrelated elected startup writers
start. All outcomes release those services; terminal anchor work remains parked
with its term/incarnation record. No original source/capture/receipt checks were
relaxed. UI-owned upstream replacement now preserves the original host topology
alongside the import path, with original environment precedence.

Evidence `.context/anchor-direct-activation-20261108/` preserves all commands and
source01..09. Focused ordinary9 passes. First mounted ordinary fails a new DoQ
fixture nonzero ID; exact ID-zero correction passes actual three-phase runServe:
signed-wire bootstrap, all six transports, retained restart without rebootstrap,
and disabled admission returning empty unsigned SERVFAIL/auth503. First composed
race (96 pass/1 fail) exposes genuine primary-startup generation interposition;
startup sequencing fixes it. A focused race then passes8. The next composed race
passes96 but fails one new shutdown expectation: primary's existing writer check
may return ErrUnavailable when cancellation races a generation wake. Only that
new fixture is narrowed to accept the exact joined cancellation outcome with
same original live token/term/incarnation; early worker exit still fails. Final
direct race passes5/no skips, including deterministic held-reply startup ordering,
terminal caller replacement, explicit follower refusal and all6 runServe paths.
The preceding composed commands remain failures, not relabeled full-suite green.

Full servingbuild race passes218/no skips. The same command's config suite
fails2 (lost host topology under UI section ownership, missing enum help); fixes
and new source-precedence cases yield full config race350/no skips. Affected vet
exits0/22.2384s; reference generation and all4 drift tests pass. Full configured
lint initially fails one context-parameter order in a new helper; signature/call
reordering only yields lint0/24.2393s/zero issues. Production runtime bytes match
the qualified race cut; final config loader behavior is source08, final test-only
signature source09. Complete signed native-recursive/root-retirement integration,
edge activation, enrolled follower lifecycle and RFC interoperability are still
required. All HA/AD, fixed-class/performance and external/final gates stay open.


### F28 native recursive DNS through the complete runtime

The direct standalone runtime now has actual native-recursion acceptance over
UDP, TCP, DoT, DoH, DoQ and DoH3. A real non-loopback private root serves signed
ED25519 DNSKEY records and priming NS/glue. Every response must retain AD, the
original DS identity and a verifiable signature. Both actual runServe lifetimes
pass: restart reacquires native data over UDP, performs no second bootstrap TCP
fetch and leaves the accepted owner version unchanged. The outer raw cache is
disabled so it cannot substitute for the native resolver path.

Evidence: `.context/anchor-native-mount-20261108/`. The first ordinary command
fails readiness; a new fixture startup-error report identifies its omitted
required upstream strategy in the second failed command. Supplying that fixture
field yields ordinary exit0/one pass (20.8447 s); combined direct authoritative
and native-recursive default-race exits0/two passes/no skips (64.9035 s). No
production behavior changed in this tranche. Earlier failures remain preserved.
Configured command-package lint exits0/zero issues (15.0280 s). All-anchor-loss
retirement, follower supervision/activation, edge topology and external RFC
interoperability remain open; this is not full F28 or release qualification.


### F28 direct follower supervisor and actual revocation output (D318)

Direct SQLite activation now admits enrolled primary/follower roles. The complete
runtime mounts a deny-only follower supervisor; initial absent/dormant data waits
without acquiring trust or initializing canonical state. Every captured child
retains its exact installed incarnation. Terminal results survive later installs
within the same member/authority term, including their local incarnation changes.
Role/term changes cancel and join old acquisition before successor work; failures
park without ending unrelated runtime services. Edge topology and full HA
enrollment/forwarding/promotion orchestration remain separately incomplete.

Evidence `.context/anchor-follower-supervisor-20261108/` retains source01..05
and every numeric outcome. The first focused ordinary command passes3/fails2:
one failure exposes supervisor rearm on InstallReplica's incarnation increment;
the other compares raw producer/receiver documents across canonicalization.
The supervisor now retains terminal results by member/term while preserving the
individual poller's stricter checks. The fixture verifies installed generation
and exact receiver bytes before/after. Corrected ordinary passes5/no skips; the
final composed anchor/root/runtime default-race passes106/no skips (128.5986 s).
Promotion now requires actual peer EOF before runtime shutdown. Original failed
commands remain failures.

A separate actual runServe test bootstraps a primary from a real signed response,
then installs its complete image on an enrolled follower with target capture
before source read. The mounted follower serves all six transports while its
independent acquisition is held; authenticated REVOKE then retires output to
empty unsigned SERVFAIL and readiness/auth503. Actual close/open/runServe
restart preserves denial, exact canonical image and the two total root fetches.
Ordinary1 passes (29.8483 s); default-race1 passes/no skips (32.2512 s). This
proves local follower denial, not canonical follower progress or accelerated
RFC hold-down. The outer raw cache is disabled in this fixture; previous cache
retirement evidence remains separately recorded. All-anchor-loss canonical
writer/restart composition, edge activation, RFC interoperability and remaining
HA/AD/performance/external release requirements remain open.

Final static follow-up: docs generator exits0 and full config/reference ordinary
checks pass354/no skips. The first full lint exits1: unchecked joined-child result
and two lostcancel diagnostics. An explicit child object now owns cancellation
and completion; canceled old-epoch results are consumed without permitting retry.
Final affected default-race on source06 passes6/no skips (43.2701 s), including
all mounted follower transport/restart and peer-EOF cases. The broader106 result
remains tied to its prior source03; it is not silently relabeled as source06.

Second full lint fails one S1005 discarded-value diagnostic. Source07 records
non-cancellation errors from joined children instead; final full configured
lint exits0/zero issues (29.2496 s). This last change adds completion logging
only; source06's six race results retain their exact source fingerprint. Same-term
install after a successful normal observation still needs a bounded fresh-poll
continuation; terminal old operations must never be rearmed. That follow-up
remains explicit rather than claiming complete automatic follower liveness.


### F28 fresh follower acquisition after completed normal input

A successful authenticated normal observation now permits a later fresh capture
of a different installed incarnation within the same member/authority term. Its
original operation has already completed with the exact final authority check.
The successor operation captures its own source/local carrier/deadline before
wire acquisition; unchanged owner/settings retain the existing cooldown.
Terminal errors still set `stopped` before any future capture, and the supervisor
retains terminal outcomes across same-term installs. A held receipt cannot adopt
a successor, even after an earlier normal observation succeeded. Denial and
incomplete transport do not create this normal-completion permission.

Evidence `.context/anchor-follower-continuation-20261108/`: production source01
passes all27 follower default-race entries/no skips (255.8060 s including cold
compilation after the explicit standard Go build-cache reclamation). The actual
SQLite tests retain canonical follower bytes through signed normal input and
new primary refresh/install. The held-response test is strengthened separately
to exercise successful normal input, one admitted new source, then another
install while that next receipt is held. Final results are recorded below.
The prior stricter per-operation epoch test and malformed-terminal-install
regression remain unchanged. This closes the D318 successful-normal continuation
gap; canonical writer all-loss/restart, edge activation, recovery, enrolled HA
orchestration and external/final release requirements remain open.

The final strengthened pair passes2/no skips under default race (24.8420 s),
including a previously successful normal observation before the held successor
receipt is invalidated by a second actual install. Production code is identical
to the 27-pass follower suite; only that new test was strengthened.

Final scoped configured command-package lint exits0/zero issues (182.5157 s,
including cold normal-mode dependencies). No earlier failures are erased.


### F28 canonical all-anchor loss and physical restart

The actual elector-carried root caller now has a complete-runtime regression
for signed bootstrap followed by signed self-revocation of the sole anchor.
A real net.Pipe write inside the original Output.Emit scope holds retirement;
after the authority observes the second acquisition, denial cannot complete
until that write drains. Both local denial and paired canonical update commit,
the old transfer continuation refuses, and accepted history retains quarantine
and the permanent revocation tombstone.

The same SQLite database is physically closed and reopened under managed,
static/default and disabled-topology configuration. Each complete runtime
refuses protected publication with prepare_failed, preserving the exact accepted
version and encoded pure state without another root acquisition. The two root
operations are explicitly invoked under the actual elector; this does not
accelerate or qualify the one-hour scheduler minimum. Restart runs the complete
read runtime, not a newly elected writer. The held output is one scoped pipe
write; prior mounted six-transport evidence remains separate.

Evidence: `.context/anchor-canonical-loss-20261108/`. Initial ordinary passes1
with no skips (39.6666 s). The strengthened source02 synchronizes on the actual
second authority request and passes1 under default race with no skips (28.4512 s).
Initial scoped lint fails QF1003; source03 changes only the restart branch to a
tagged switch. Edge activation, explicit recovery, enrolled HA orchestration,
RFC interoperability and final release requirements remain open.

Final scoped configured command-package lint exits0/zero issues (27.6478 s)
on source03. The prior race remains tied to source02; only the tagged-switch
style correction separates them.


### F28 original installed edge fleet activation (D319)

SQLite host topology now supports `edges` with 1–32 explicit unique node,
observation address, retained deployment identity and exact backend DNS/internal
membership requirements. Internal credentials must exist. The complete root
constructor captures one local image/token before contacting any peer, validates
the whole fleet, then acquires one authenticated installed observation from each.
The original Store publication barrier adopts only the exact issued observation
once into runtime life; any deadline, peer/membership or local authority/image
refusal fails startup without recapture. All original canonical/local-denial,
writer, complete publication, native/raw cache and final-output checks remain.

Runtime lifetime adoption relies on the previously qualified D315 persistent
immutable bypass across edge children; diagnostic readiness never grants it.
Host declarations cannot discover undeclared proxies. Replacing/rolling back edge
deployment storage is excluded as before. An initial empty follower requires its
first complete image before edge startup; direct followers can still wait.
Full enrolled HA bootstrap/orchestration remains open.

Evidence `.context/anchor-edge-activation-20261108/`: focused ordinary44 passes,
no skips (36.6714 s). Actual pinned dnsdist2.1.3 image89c7f196 plus a real runServe
backend initially fails only the final query before post-restart health recovery;
that original failure is retained. The corrected bounded readiness wait passes1,
no skips (23.8418 s). This actual YAML daemon test starts installed bypass before
the backend is ready, bootstraps once, queries both authoritative/resolver pools
over UDP/TCP, restarts the edge with retained deployment/new run identity, then
joins the backend and observes no cached data. Backend restart authenticates the
fleet again without another root bootstrap or accepted-version progress; both
actual pools have zero cache capacity/entries. Previous Lua installed-cache
qualification remains separate; this is not all external edge transport coverage.

Composed source03 default race passes133 entries/no skips (167.4716 s), including
the enabled pinned daemon, all managed-root/runtime cases and selected config
cases. Source review then found the host fleet needed the same explicit retention
as topology across UI-owned upstream section loading. Source04 preserves that
host list and tests actual file/UI/environment precedence. Final config/static
outcomes are recorded separately below; source03's race evidence is not relabeled.

Safe operator recovery, initial edge follower installation/orchestration, complete
RFC interoperability/hold-down and remaining release gates are still open.

Final configuration source05 passes363 default-race entries/no skips
(31.6501 s). The first full config race retained361 passes/two failures: the new
fixture incorrectly expected environment-list support, which existing policy
intentionally refuses, and the generic enum fixture lacked the required fleet.
Corrected fixtures retain that refusal, test scalar environment topology
precedence, and supply a complete fleet for the positive enum case. The actual
file/UI list preservation now passes without weakening collection validation.
The reference generator also exits0 (1.4053 s).

Final generated-reference tests pass4/no skips (0.8041 s), and full repository
configured golangci-lint exits0/zero issues on source06 (115.1813 s). No rules
or thresholds changed. The final runtime/activation production files are identical
to source03; source04/05 changes are the separately qualified host loader and
config fixtures. Minimum free space during lint was32,947,277,824 bytes.


### F28 recovery provenance and persistence primitives (D320)

Explicit quarantined-owner recovery authenticates new DS-selected DNSKEY trust,
retains initial policy/seed identity and exact permanent tombstones, and records
at most 32 immutable version-2 recovery events. Ordinary refresh preserves the
ledger. A separate opaque proof binds both complete state digests and the original
wall/monotonic lifetime through captured SQLite commit. No operator command,
authorization/audit mount, migration or automatic reseed is exposed here.

Pure race73 and pure/accepted/persistence race190 passed without skips. The wider
trustanchors/SQLite default-race run reached 975 passing test entries but FAILED
at its five-minute package deadline; it is not full SQLite qualification. New
recovery tests prove actual original-refusal rollback, committed recovery, physical
writer/follower restart, ordinary continuation and mounted rejection of an
independently well-formed replica rewriting prior recovery provenance. Their first
run failed because a writer-only fixture read was used on a follower; the corrected
read-only installed-image fixture passes both tests under race. Evidence remains
in .context/anchor-recovery-20261108; all earlier failures are retained.

A separate qualification compiled the exact pre-change pure codec from be4446fc
and fed it actual generated format-1 and recovered format-2 documents: race1
passed, preserving format-1 bytes and rejecting format 2. This checks the codec,
not an entire old daemon or mixed-version fleet. The first lint invocation could
not start a missing executable; the installed configured linter then reported
one test variable shadowing builtin copy, corrected with a local rename.
Offline operator admission/audit, strict existing-schema opening, operational
mixed-version acceptance and complete F28/release gates remain open.

Final configured trustanchors/SQLite scoped lint passes (zero issues, 2.006s).


### F28 offline original-command recovery (D321)

`anchors status|recover` now uses a strict existing current-schema SQLite open:
no database/directory creation, schema migration, metadata repair or authority
initialization. Original finite context, existing WAL mode, complete metadata and
exclusive process ownership are required. Real SQLite tests cover missing/empty/
indirect/old/new-schema files, missing identity/audit metadata, non-WAL state and
actual process-lock exclusion; 17 race entries pass without skips.

The OS-authorized offline command requires explicit stopped-process acknowledgment,
exact original accepted-envelope digest, reason, bounded replacement DS records
and numeric root endpoints. It captures original owner/writer/source before TCP
acquisition, uses actual preparse receipt sampling and returns the independent SQL
disposition. Trust and the OS UID/reason/version audit commit in one transaction.
An actual SQLite trigger rejecting the audit proves rollback of both; stale input,
unauthenticated input and cancellation preserve the original quarantine. The first
command build failed on a duplicate fixture variable declaration and is retained;
corrected ordinary7 and final default-race8 pass without skips.

The final race includes actual `runServe` after offline recovery, complete native
recursive DNSKEY authentication and AD output over UDP/TCP/DoT/DoH/DoQ/DoH3, then
a second physical restart of the same database. Original configured seed/policy
identity stays fixed; the explicit replacement key verifies the actual wire. The
original recovery version and tombstones remain, with one recovery TCP acquisition
and no restart rebootstrap/progress. Wall65.7071s, package25.987s, minimum32,864,342,016B;
evidence .context/anchor-recovery-command-20261108. This is not full enrolled HA,
external RFC interoperability, fleet mixed-version or final release acceptance.

Final configured command/SQLite/trustanchors lint passes (zero issues, 29.051s).
The first two lint runs retained test-only G115/G304 diagnostics; explicit clock
range termination and test-path annotations fixed them. One broad race invocation
was accidentally started before lint had exited; root terminated only that newly
owned group (child -15, wrapper241), retained its137 passes as unqualified and
verified no remaining Go/compiler/link/test processes before the sequential run.
The separate earlier five-minute package timeout remains failed evidence.

The final sequential default-race suite on source06 passes all 1,026 test entries
across trustanchors, accepted, persistence, resolverstate, retirement, SQLite and
runtimeowners, with zero failures/skips. Go0, wall280.2494s, SQLite268.006s, minimum
32,446,283,776B, log1,071,360B, no source mismatches or resource stop. This supersedes
neither the retained five-minute timeout nor the interrupted overlap attempt;
those outcomes remain explicit.

Final CLI/default-race30 passes without failures/skips (65.9093s), including
all offline recovery cases, regenerated CLI-reference drift, shell completion
and existing convenience-command regressions. Source06 remains unchanged.


### F60b/G11 promotion authorization prerequisite, November 8

D322 changes coordinator operator authorization from an early verdict to a
retained original proof check plus dependency release. Missing callbacks refuse.
The coordinator keeps that original authorization and the complete serving pin
through SQLite `PromoteChecked`, whose final memory/clock check runs after all
staged authority/feed writes and before commit or idempotent success. A refusal
rolls back, preserves memory authority and generation notifications, and survives
physical reopen. Cancellation cannot be hidden by a callback returning nil.
The low-level store CAS remains available to already-authorized internal callers;
this change does not mount enrollment, forwarding or operator promotion routes.

Focused default-race qualification passes 21 entries without failures or skips,
including actual SQLite rollback/reopen, exact retry, terminal SQL1811 failure,
original-context/dependency lifetime and queued admission. The full SQLite, follower, replica, serving, servingauth and servingbuild
default-race run passes 1,192 entries without failures or skips (296.055 seconds).
All frozen source identities match; configured scoped lint passes with zero
issues (8.415 seconds). Evidence:
`.context/promotion-authorization-20261108/`.

### F20 retained exact-miss performance diagnosis, November 8

No matcher, parser, workload or gate changed. The original five-process gate on
6cace80f still fails exact-1M miss p50 (158 ns median versus 109 ns). One isolated
original Evaluate CPU profile reports 88.96 ns/op, zero allocations, and attributes
most samples to normalization/filter memory loads; nil regex calls are a minor
cost. One full original latency-sibling CPU profile reproduces 169 ns exact-miss
p50 with a 35 ns timer p50. Profiling is diagnostic and cannot replace unprofiled
samples or named-runner evidence. The profiles do not justify a matcher semantic
or filter-capacity change. Original failures and fixed-class/final gates remain
open. Evidence: `.context/exact-miss-review-20261108/`.


### F60b/G11 independent member identity prerequisite, November 8

D323 adds `internal/cluster/memberidentity`. Exclusive durable 0600 creation and
strict loading retain a private node-local Ed25519 CA bound to the original
member UUID. Public enrollment data is detached; no private key enters images.
Standard TLS 1.3 mutual verification uses exact independent member CAs before
application bytes. Short-lived leaf renewal/restart keeps the enrolled CA;
a different CA cannot impersonate it by claiming the same UUID. Route checks
match the actual verified connection to the exact member and recheck original
certificate expiry. No insecure certificate-verification fallback is installed.

Initial default-race17 passes without failures/skips, including actual two-peer
TLS, wrong-client/wrong-server refusal before HTTP, same-UUID replacement,
physical key reopen, concurrent create, bad-file refusal, and held original leaf
expiry. Expanded final default-race21 passes without failures/skips (4.612 s),
adding another enrolled member claiming the wrong UUID, anonymous-client and
TLS1.2 refusals. Configured scoped lint passes with zero issues (2.007 s). Its
first run flagged intentional private-key serialization; narrowly justified
annotations identify the protected file and in-memory comparison. That failure
is retained; final source differs from race02 only in those comments. This is an identity
prerequisite only: durable membership, join tokens, root wiring, current
revocation/output checks, forwarding and complete HA remain open. Evidence:
`.context/member-identity-20261108/`.


### F60b/G11 captured member replication client, November 8

D324 adds explicit `NewMemberClient` with the node-local D323 identity and each
original resolved peer's exact CA. Local UUID/key mismatch, missing target trust
and custom transports refuse. One captured exchange owns a TLS transport with
connection reuse disabled, so a changed enrollment at the same address cannot
send through an old connection. Certificate/request lifetime is checked before
authority observation and final image/command-result admission. A TLS rejection
cannot create network-loss promotion eligibility; a POST alert or original
cancellation after complete body read preserves unknown outcome without replay.

Focused default-race4 passes against actual SQLite/TLS image, 304 and command
paths, same-address changed trust, same-UUID impostor refusing before HTTP,
explicit dedup retry, credential refusal, and actual committed command with
cancellation at completed body EOF. Final composed replica/memberidentity/
follower default-race112 passes without failures/skips (28.271 s), including
actual remote-TLS refusal classification. Configured scoped lint passes with
zero issues (6.220 s); source03 differs from qualified source02 only in the
constructor documentation. This client does not
supply durable enrollment, current membership/output exclusion or root startup;
those remain open. Legacy shared-credential transport tests are retained.
Evidence: `.context/replica-member-client-20261108/`.

### F60b/G11 bounded enrollment owner, November 8

D325 adds immutable complete member/invitation state and bounded canonical wire
formats. Owner limits are 64 active members, 256 historical identities, 64
invitations and 512 KiB. Hash-only stored invitations bind original cluster,
issuer term, exact target and an absolute lifetime of at most ten minutes.
Purpose-separated possession signatures also bind original timestamps. Accept
consumes the invitation and adds the member together in one successor; revoked
UUIDs and signing keys cannot return under replacement certificates. Revocation
cancels older target invitations; explicit expiry pruning never removes members.
Opaque admission retains original context, actual time, issuer and before/after
digests for a terminal final check. It is not a durable commit receipt.

Default-race54 passes without failures/skips (59.722 s), including actual 256-member
history exhaustion, new-certificate/same-key revocation, valid-but-rewritten
history refusal, one-time consumption, strict wire decoding, real expiry and
zero-value admission refusal. Final configured scoped lint passes with zero
issues (3.649 s); earlier missing-comment and formatting failures are retained.
Final source04 differs from race-qualified source02 only in comments/whitespace. These pure
primitives do not supply durable owner migration, join-token routes, operator
policy, follower install exclusion or root HA mounting. All earlier performance
failures and external acceptance requirements remain open.
Evidence: `.context/enrollment-owner-20261108/`.

### F60b/G11 complete enrollment storage, November 8

D326 introduces supported schema9 and required member-enrollment-v1 image
negotiation. Migration inserts only an exact non-granting marker; original startup
converts that marker to Dormant. Missing, malformed, orphaned or oversized
current-schema membership never repairs. Schema8 anchor retirement provenance
and bytes remain unchanged, with the loader explicitly supporting schema9.
The original schema8-only migration regression still targets actual schema8;
current-store fixtures assert the actual supported marker without emulation.

One-use original writer/operator captures retain exact owner CAS and finite
lifetime. The SQLite adapter atomically stores invitation consumption and member
addition, registers the postimage fence, and retains original possession/expiry
through commit. Generic command owner mutations and actual install/conflict
replacement preserve history; legacy bulk writes refuse the enrollment prefix.
Secret invitation responses remain staged until the outer command commits.

Expanded focused default-race26 passes without failures/skips (15.626 s). Cases
include actual SQL1811 rollback, original cancellation/authorization, postimage
ABA, real expiry after staging, raw erasure refusal, physical primary/follower
restart, higher-image resurrection and explicit conflict-resolution refusal,
quarantine rollback, complete current-schema corruption refusal, schema8-to9
row preservation and actual downgrade refusal. Earlier unsupported-schema
loader and missing-token fixture failures remain preserved. The first broad run
had 1003 passes and 16 failed entries: random UUID ordering in a new history fixture,
new mandatory-row accounting, future-schema and old-schema fixture framing.
Those are corrected while preserving original negative targets and validation
error priority; 34 regression race entries pass. Final composed source05 default-race suite
passes 1464 entries without failures/test skips (357.534 s), including serving,
authentication publication and local storage. The accepted framing package has
no standalone tests and reports a package-only skip; actual adapter/image cases
exercise it. Full configured root lint passes with zero issues (130.222 s).
A later admission-only correction puts opaque handle terminal/consumption state
in private shared cells, so copied values cannot reopen attempts; final source06
affected default-race passes 69 entries without failures/skips (42.676 s).
Final source06 mounted startup/recovery/cluster default-race passes 26 entries
without failures/skips (120.999 s); final full configured lint exits 0 with zero
issues (99.199 s). Root enrollment routes, operator/output
barriers, current peer exclusion and HA orchestration remain open; this is not
production enrollment completion. Evidence: `.context/enrollment-storage-20261108/`.

### F60b/G11 atomic initial Primary enrollment, November 8

D327 stages first membership and Standalone-to-Primary authority in one actual
SQLite Command from an original Store-issued complete cut, an existing Dormant
owner and the local private identity. The caller retains original operator
authorization and dependency pins; final admission checks the original lifetime
and exact planned authority while old memory authority remains installed.
Audit can be appended in the same transaction. A successful commit advances one
generation and fences the old writer; rollback changes none of these effects.
Uncertain completion blocks the old writer and wakes dependents without claiming
a rollback or a committed candidate. The actual restart reads durable authority.

Focused source01 default-race passes 21 entries without failures/skips (13.626 s).
Composed enrollment, SQLite, replica and serving/auth/build race passes 1259
entries without failures or test skips (374.423 s). The accepted framing package
has no standalone tests; actual adapter/image cases exercise it. Final source02 explicitly distinguishes unknown completion from confirmed
rollback; all 21 focused race entries pass again (11.219 s), and configured
SQLite/enrollment lint passes with zero issues (8.018 s).
Root operator routes, publication/output acknowledgment, peer exclusion, join
transport and full HA assembly remain open.
Evidence: `.context/enrollment-primary-20261108/`.

### F63/G23 fixed benchmark runner decision, November 8

The owner explicitly selected Ubicloud 8-core Ubuntu 24.04. The measurement
workflow now pins `ubicloud-standard-8-ubuntu-2404`, with the original
`GOMAXPROCS=6`, five fresh samples per suite, original corpora and durations.
The runner-choice decision is resolved. Eight host cores do not silently change
the original six-processor workload or authorize a baseline/threshold reset.

Actual hosted baseline calibration, additional absolute limits and candidate
comparison remain open. The workflow currently records evidence; a successful
recording alone does not qualify the release. The original local gate failure
(exact1M miss p50 median158ns versus109ns) and every earlier failed result remain
retained. Local sandbox results cannot close this named-runner gate. No workflow
execution, push, baseline change or performance completion is claimed here.

### F60b/G11 original peer request and output boundary, November 8

D328 adds actual TLS certificate admission and Store-issued original peer
requests. One bounded read cut requires active local/remote members and exact
Primary authority before body I/O. A request permits one image or one actual
command; copied handles share consumption. Original owner/writer checks, the
membership postimage fence, original cancellation and the final certificate check
protect actual command completion. The last peer check follows domain checks.

Only a successful actual completion permits one output scope. That scope compares
the exact resulting authority/generation and original owner and holds wmu through
bounded emission and explicit flush. Original request/peer-certificate/local-CA
expiration supplies the earlier transport deadline. Canceled lock waiters return
without acquiring replacement authority; an active emitter retains exclusion
until it actually returns. Unknown commit has a distinct error and no output.

Initial source01 focused race 36 passed. Source02 expanded actual HTTP coverage
reported 44 passes/4 failed entries: the HTTP2 fixture waited for END_STREAM while
deliberately preventing handler return. Source03 checks protected body bytes or
304 headers before releasing the handler, then separately checks clean stream
completion. The original failed receipt remains. Source03 focused race 52 passes
without failures/skips (22.442 s). Configured lint initially found two test-only
style/check issues; both are corrected in source04 and lint passes with zero
issues (2.808 s). Composed member/enrollment/replica default-race passes
221 entries (55.893 s); command/authentication regressions pass 209 (31.252 s),
without failures or skips. Final original-writer review also rejects a foreign or
explicitly zero carried WriterToken at capture and command entry, while retaining
the exact matching token. Source05 focused race passes 55 entries (22.842 s);
source06 final carried-token regressions pass 29 entries (18.243 s), without
failures/skips. Final source06 configured scoped lint passes with zero issues (10.020 s).
All 2,960 source06 identities match final bookkeeping.

The existing replica server/root routes, original domain output permission pins,
join transport, operator routes and complete HA assembly remain open. This is
not production HA completion. Evidence: `.context/member-request-20261108/`.

### F60b/G11 enrolled replication HTTP mount, November 8

D329 adds NewMemberServer with original Store-issued peer admission before body
acquisition. Mandatory actual read/write deadlines, the earlier request/peer
lifetime and one concurrency slot cover acquisition through explicit response
flush. Exact image/304 responses use the original complete cut and authority.
SQLiteMemberAtomic retains original peer/writer checks around domain authorization,
mutation and durable dedup. The synchronous command callback must keep domain
and dependency pins through commit and emit. WithCommandOutput requires the
actual command completion kind; image receipts cannot acknowledge commands.

Late admission, command-expiry, write or flush failure aborts the actual HTTP
stream. There is no delayed buffered success after the original output scope.
Actual member clients retain unknown completion after an acknowledgment is lost
and preserve one domain effect across an explicit same-command dedup retry.
Early generic refusals expose no authority metadata. The original bound also
covers queueing/body acquisition, and an active flush retains its concurrency slot.

Source01 focused race retained 17 passes/3 failed entries: two image comparisons
included independently sampled image creation timestamps. Source02 compares exact
generation and complete payload while preserving the actual live response;
focused race passes 36 entries without failures/skips (23.041 s). Source03 full
member/enrollment/replica default-race passes 266 entries (68.118 s), no failed or
skipped tests. Accepted framing has a package-only no-test skip. Final command-kind
review adds explicit image-receipt refusal; source04 affected member/command race
passes 146 entries (46.481 s), no failures/skips. Initial lint found context argument
ordering and a test switch style issue; source05 corrects those without changing
admission semantics. Final configured scoped lint exits0 with zero issues (4.612 s).
All 2,962 source05 identities match. Evidence: `.context/member-server-20261108/`.

These are actual HTTP/1 and HTTP/2 library mounts and actual SQLite transactions.
Production listener/domain assembly, operator activation/join-token/promotion,
join transport, forwarding and complete HA/restart/rejoin acceptance remain open.
The test domain is deliberately a transaction fixture, not production RBAC credit.
The legacy error-only-authorizer server is preserved without enrolled-authority
credit. No hosted benchmark, push, deployment or release occurred.

### F60b/G11 authenticated refusal and promotion eligibility, November 8

D330 fixes the member client's classification of an actual authenticated primary
that aborts HTTP before response headers. TLSHandshakeDone records contact only
after the original independently enrolled peer CA verifies the actual connection.
That contact persists within the original exchange; it neither refreshes trust
nor supplies a term, accepted image or command success. ExchangeError preserves
Reached=true on later transport failure, clearing the coordinator's missed-pull
window. POST still returns an unknown outcome and is never automatically replayed.
Failed connections without verified contact remain network-loss evidence.

The new tests first reproduced both missing-contact failures on unchanged client
source (numeric1, two failed tests, 4.209 s). Fixed full replica default-race passes
82 entries without failures/skips (22.242 s), including three repeated actual
TLS/HTTP aborts with no promotion window, one-dispatch unknown writes and a real
failed TCP connection. The actual enrolled server/client image/dedup/committed
revocation integration passes three entries (6.214 s). Final configured scoped
lint exits0 with zero issues (4.611 s). All 2,962 source03 identities match;
original failing receipts remain under `.context/member-contact-20261108/`.
Root operator/domain/listener/join assembly and full HA acceptance remain open.

### Shared upstream ownership and retained API failures, November 8

Node-only managed topology, installed edge identity and import paths no longer
enter shared upstream ownership or exports. Original node/default values survive
malformed stale UI-owned sections, while invalid original host fields still
refuse. The historical audit fixture removes its exact fresh enrollment sentinel;
production schema migration is unchanged. A captured stack also establishes an
unbounded wait in the API flush-cancellation test; its mock now waits for the
actual write-deadline interrupt and bounds observations.

Focused race16 and full settings/config/API race1,037 pass with no failures.
The full run's one Kubernetes-assets skip is separately executed and passes on
the installed control plane; PostgreSQL history runs in the full suite. The first
failed/interrupted run and lint tool/annotation failures remain in gaps.md and
`.context/settings-scope-20261108/`. These results do not establish final hosted
CI or enrolled HA assembly.

### Actual Ubicloud recording and original reference preparation, November 8

Run 42514599389 on exact 9cc2ae04 completed all 40 measurements and seven
shape/absolute checks on the selected Ubicloud class: 136 cases, 592 metrics,
five values each, actual AMD EPYC 9454P and GOMAXPROCS6. All hashes and exits
verify. Recording succeeds; release_qualified remains false. The old Intel
baseline and exact-miss regression remain retained. A separate explicitly
triggered workflow now pins the original baseline source 0cc9565e for new
reference measurements on Ubicloud; eight control tests and YAML validation
pass, but no reference execution is claimed. Same-class comparisons, additional
absolute calibration and final-source gates remain open; see gaps.md and
bench/README.md.

Current-source actual Knot race passes nine entries without skips. The preserved
old hosted command shard exposed an observation assertion followed by a fixture
cleanup deadlock. Cleanup now releases held providers first and the test captures
its original published inventory before the challenged Sync. Ten diagnostic
race iterations and all 42 final integration race entries pass; scoped lint is
clean. The original timeout/assertion remains retained, not labeled a flake.

Hosted immutable benchmark reference run 42517712186 completes all twenty native
measurements but fails the old CNAME ceiling (2,090ns >2,000ns). It ran on EPYC
9224; the candidate ran on EPYC9454P under the same Ubicloud label. Neither the
failed reference nor that CPU mismatch is waived. A paired original/candidate
measurement in one VM is the next concrete step; no replacement sample or new
baseline is approved by these results. Fresh security on 86a3ff91 is green;
full hosted CI and the remaining implementation/external gates stay open.


### F63/G23 paired runner control, November 8

The selected Ubicloud pool supplied different CPUs to the candidate and original
reference, and the reference independently failed its CNAME ceiling. The new
paired workflow places both existing collectors sequentially in one VM: original
0cc9565e remains immutable, candidate source is exact, all sixty native process
receipts and original gates remain mandatory. A failed reference is ineligible
for comparison and keeps the overall result failed; the independent candidate
recording does not clear it. Eligible original scopes use the unchanged strict
Go comparison. No baseline, threshold, corpus or sample replacement is introduced.
Twelve control tests and workflow YAML validation pass. Hosted paired execution
and new absolute calibration remain pending; this is not release qualification.


### F08 Kubernetes doctor egress diagnostic, November 8

The missing Kubernetes self-NOTIFY mount first fails in the actual API-server
fixture (native exit1, 26.444s); that original result is retained. D331 now mounts
the diagnostic with the same complete immutable TSIG startup inventory used by
all six listeners. It checks the original verified packet/key identity and
finite deadline, reports only the observed UDP source, and grants no writer,
leader or zone-readiness authority. Unknown/wrong-owner sources cannot fall back
to file keys. Other NOTIFY traffic retains its original receiver.

Actual Kubernetes stored/file key round trips and restart, offline/wrong-owner
startup, unsigned/wrong-key/TCP/malformed refusals, plus all original SQLite
Doctor tests pass under default race: 35 entries, zero failures/skips, 39.065s.
All frozen source identities match. Scoped command lint exits0 with zero issues
in 27.649s. Evidence is `.context/doctor-kube-20261108/`. This closes the named
Kubernetes diagnostic gap; TSIG hot management, HA assembly and remaining final
release gates remain open.


### F21c original cache/strategy calibration, November 8

The selected-runner historical reference supplies five fresh values for all20
cache and60 strategy metrics. Separate committed ceilings use each original
median plus max(20%, original noise floor); reports, process receipts, hashes and
all vectors are retained in bench/calibration/ubicloud8-20261108. All thirteen
control tests pass. Collectors now pin and enforce those files alongside unchanged
blocking/zone limits. No baseline, old threshold, ignore or workload is changed.

Actual Go absolute checks of the retained reference cache and strategy reports
exit0. The full reference's old CNAME failure remains1 and cannot establish a
paired baseline. Retained candidate42514599389 fails both new scopes: five cache
limits (parallel hit168.6ns, Set288B/op, memory551258352B/op,468.7bytes/entry,
349.8overhead-bytes/entry) and three strategy limits (fast/strict126398ns,
slow-first/rtt_weighted259747ns and177us p50). These are honest absolute failures,
not a CPU-matched relative comparison; current-source optimization/qualification
remains open. No benchmark was rerun to select or replace samples. Evidence:
.context/cache-strategy-calibration-20261108/.


### F21c cache provenance allocation, November 8

Original cache calibration exposed160 bytes of empty inline provenance per
entry. Cache entries now omit only exactly-zero metadata; complete nonzero
scope/security/upstream metadata shares one allocation with its entry. Restored
scope changes replace immutable metadata so retained dump snapshots cannot mix
old keys with a new scope. Original lifetimes, original fields, version2 wire
format, admission and workloads are unchanged. Both insertion forms have an
actual one-allocation guard; returned values do not alias owners. A deterministic
blocked actual dump plus concurrent restore rebind preserves all32 original rows.

Full cache/persistence/cache-stage/upstream-stage default races pass102 entries
without failures/skips (7.413s), followed by configured scoped lint exit0/zero
issues (2.812s). All source01 identities match. One unchanged original-cache
1s diagnostic on this sandbox exits0 (26.842s): Set128B/1alloc, memory308.7bytes
and189.8overhead-bytes per entry; all Get/Growth lookup cases0alloc. Parallel
hit163.6ns remains above the new156.48ns ceiling; no local timing or one-process
result is fixed-runner or five-process qualification. Original hosted cache and
strategy failures remain open pending actual new-source measurement. Evidence:
.context/cache-metadata-20261108/.


### F18c/F26d/F28 held DNS across canonical commits, November 8

D332 distinguishes retained recursive/policy responses from data-generation
currency. Actual query-log reload and UniFi held queries originally failed in
all three local repetitions (six failures, native exit1, 123.201s). The root
component now retains the typed original runtime dependencies and actual anchor
input for ordinary forwarded/policy responses. Authoritative/local/mixed output,
API dig and native resolver mutations retain full generation checks; the original
ordinary-query-after-UPDATE refusal test is unchanged and passes. Empty unsynced
SERVFAIL can report unavailable local data without answer/delegation/proof rows.
Raw-cache work consumes but skips an old-generation mutation, never an anchor
retirement refusal. Original query lifetimes, mode/retirement holds and all
expected policy/address values remain unchanged.

Earlier drafts and failures remain: the first fixed run still failed UniFi;
source04 broader races had87 passes, one expired-record output failure and one
explicit live-edge skip. Source05 had12 passes and a restoration observation
failure. A diagnostic-only richer assertion run passed and did not establish
that failure's cause. The final fixture checks the exact successful TCP
allocation/restoration observation rather than discarding a converged UDP result
and reacquiring another cut during continuous polling; it still asserts every
original exact address and bound. No held request is retried or renewed.

Final source06 mounted default races pass44 entries without failures/skips
(267.998s), including both original held cases, full UniFi lifecycle, native
six-transport bootstrap/revocation/restart, actual pinned dnsdist edge restart,
root cache/output retirement and original UPDATE refusal. Configured command
and SQLite lint exits0 with zero issues (32.255s); frozen source identities match.
Earlier root39 and broader87 passing entries remain scoped evidence, not a full
repository green claim. Evidence: .context/held-fixtures-20261108/.


### G21 publication-context monitor attribution, November 8

Hosted CI42518981841's SQLite shard failed the final-publication context monitor.
A diagnostic-only overlay reproduced one failure in ten runs and captured the
actual stack: modernc.org/sqlite.interruptOnDone.func1 called Context.Done from
an unrelated asynchronous SQL watcher during the monitor's wall-clock flag.
It was not a call from AnchorPublication.Apply. The original failing log and
instrumented native exit1 remain retained.

The test monitor now attributes calls to the synchronous final callback stack,
including all four Context methods. A deterministic control proves that an
independent watcher is ignored and all four synchronous calls are counted.
Production SQLite and final callback behavior are unchanged. All62 actual
publication race entries pass without failures/skips (19.432s); configured
command/SQLite lint exits0 with zero issues (32.255s). Source06 hashes match.
Evidence: .context/anchor-publication-context-20261108/ and
.context/held-fixtures-20261108/lint06.json. Final hosted gates remain open.


### F63/G23 actual paired Ubicloud comparison, November 8

Run42518988512 measured immutable original0cc9565e and candidate92d5c9ee on one
AMD EPYC9454P VM, Go1.27.3, original GOMAXPROCS6. All20 original and40 candidate
native processes and their3/7 shape/absolute gates exited0. Independently checked
source/plan identities, native argv/counts/bounds/reaping, report hashes and all
811 five-value vectors preserve45/219 original and136/592 candidate cases/keys.

Actual strict Go comparisons exit1/1/0 for blocking-zone/cache/upstream. The
blocking failure is exact1M miss p50:160 versus90ns (+77.78%). Cache has eight
relative failures: hit528.5 versus420.5ns, parallel193.5 versus128.5ns, Set288
versus112B and604 versus456.9ns, memory551203648 versus375285520B,468.7 versus
292.7bytes/entry,349.8 versus173.8overhead bytes/entry, and1117414235 versus
798471632ns. All60 original strategy metrics pass. No original comparison key
is missing. New workloads remain unqualified; aggregate/hosted exit1 and
release_qualified=false are retained.

This run predates b8877d85's cache metadata fix and the new cache/strategy limit
files. It does not qualify current source. The previous CNAME reference failure,
heterogeneous-CPU observation and all earlier regressions remain preserved;
no sample, baseline, ignore, original ceiling or corpus was replaced. Final
current-source performance and remaining implementation/external gates stay open.
Full artifacts, independent review and all vectors:
.context/hosted-release-20261108/paired-artifact/ and paired-reviewed-*.


### November 8 continued publication, CI and cache work

F23 is complete: the owner-approved public kudurru-data13d8d42b is verified
against the prepared tree and pinned HTTPS document (D333). D334 corrects
last-good ingress health output and operational observer shutdown;21 focused
race entries and configured lint pass. The separate full browser secondary
workflow passes with the original DNS convergence bound and expected address.

Paired Ubicloud42522360297 on91cb21bb passes blocking-zone/upstream and cache
memory, but retains two cache latency regressions (strict comparison0/1/0).
All811 five-value vectors and complete sources/receipts are verified. Subsequent
cache return-copy/clock work passes102 affected race entries and scoped lint;
its one-process local diagnostic is not release evidence. The gap ledger holds
exact results and prior failures. Final hosted checks, new workload calibration,
HA/driver/AD implementation and external acceptance remain open.


### F63/G23 socket and persistence calibration, November 8

D335 establishes the initial bounds from the first complete hosted recording,
run42514599389 at immutable9cc2ae04, selected Ubicloud eight-core Ubuntu24.04,
EPYC9454P, Go1.27.3 and original GOMAXPROCS6. The four unchanged workloads retain
14 cases/58 metrics/290 values, five fresh native processes per suite, exact
reports/commands/receipts/workload identities and all vectors in
bench/calibration/ubicloud8-sockets-persist-20261108/.

Each upper bound uses median+max(20%, existing unit noise floor); throughput and
persisted-entry lower bounds use80% of the initial median. Every exact metric is
required, and every UDP loss/DoT failure sample must be zero. These new bounds
preserve all original baseline files, comparisons, thresholds, floors and
ignores. The collector pins seven limit files; the historical reference retains
its three original scopes. A failed candidate never replaces calibration.

Gate default races pass51 entries without failures/skips (2.013s);14 Python
controls pass. An initial nine-significant-digit serialization rounded one
persistence boundary and failed its control; that draft and failure are retained.
Seventeen significant digits preserve the exact formula. Configured gate lint
exits0 with zero issues (2.010s). The actual corrected Go gate passes all four
scopes of newer retained run42522360297 without additional benchmark sampling.
Evidence: .context/socket-persist-calibration-20261108/.

This closes initial socket/persistence calibration, not final release gates.
The original paired cache hit/Set failures, prior failed evidence, heterogeneous
CPU observations and current-source qualification remain open. No sample was
retried or replaced; release_qualified remains false.


### F60b/G11 bounded member dependency/output composition, November 8

D336 adds providercoordinator.WithMemberCommandOutput as a separate original
member-command path. It requires an already carried elected writer and a finite
request of at most30 seconds, then retains publication/provider/host exclusions
through actual MemberCommand and one Store-admitted output scope. Output uses
the earliest request/peer/resource/host bound. Actual committed completion is
reported independently of output errors or cancellation. Root must still verify
raw user proof/current permission and retain its original lifetime through
transport writes and explicit flush; membership supplies no domain permission.
The ordinary command and postcommit-publication paths are unchanged.

The first focused run retained one pass/five failed entries because the new
fixture used nonexistent dnszones.list permission names. Correcting the fixture
to the real stats.read permission preserved raw credential verification. Focused
default races then pass14 entries (18.043s); composed serving/provider/coordinator/
enrollment/replica default races pass350 entries without failures/skips (79.954s).
Cases include actual enrollment/mutual TLS, all dependency replacement exclusions,
changed/no-op completion versus transport failure, forged credential refusal,
copied-request replay refusal, missing writer/unbounded input, and a real blocked
socket timing out at the earlier host deadline. Configured scoped lint exits0
with zero issues. Source02 identities remain matched. Evidence:
.context/member-output-coordinator-20261108/.

This completes the bounded helper, not production enrolled HA. Root listener and
domain adaptation, raw forwarded credential transport, operator activation/join/
promotion, forwarding and full partition/rejoin acceptance remain open. The
existing MemberServer callback retains its own WithCommandOutput responsibility;
its emit must not be nested inside this helper's already admitted raw emitter.


### G04 stub browser convergence, November 8

Hosted CI42530377433 onb2d197d8 passed153 browser cases and failed the stub case.
Its retained trace places the failure at the initial primary DNS observation,
before any stub creation: six empty address arrays followed by a three-second
DNS timeout. The earlier corrected secondary case passed in this hosted run.
The initial serving gap's cause is not established by that trace.

The three independent stub DNS convergence observations now use bounded toPass
assertions, which handle rejected queries as well as wrong answers. The original
ten-second observation bounds, exact192.0.2.81,90-second whole-test limit and
single post-expiry SERVFAIL observation are unchanged. No mutation or held
request is replayed. The full real-browser two-server create/routing/edit/expiry
scenario passes locally in27.266s including setup (18.4s test); file ESLint passes.
The exact f916537e binary builds CGO-disabled with unchanged generated dashboard
bytes. Evidence: .context/hosted-release-b2d197d8-20261108/. Hosted final-source
qualification remains open; the earlier failed run is preserved.


### F60b/G11 original peer cancellation during output, November 8

A final review added a separate-command-context regression against f916537e.
It failed: after MemberCommand returned, the output context retained the command
parent but did not propagate cancellation from the independently captured peer
request. The actual Store guard refused eventual completion, but that alone
could not interrupt an active emitter. The numeric1/one failed test is retained.

The coordinator now joins original peer cancellation through the entire emission
callback and reaps its cancellation callback before releasing dependency pins.
The actual command's committed bit remains independent. Final affected
coordinator/serving/provider default races pass140 entries without failures/skips
(45.499s); configured coordinator lint exits0/zero issues (6.615s). Source04 hashes
match. Earlier composed350 evidence remains scoped to source02. Evidence:
.context/member-output-coordinator-20261108/cancel-before03.json and race04.json.
Production enrolled listener/domain/forwarding assembly remains open.


### F60b/G11 scoped member HTTP composition, November 8

D337 connects the bounded coordinator to the enrolled HTTP transport without a
nested writer/output lock. A private Store-issued context binds the exact active
command-output scope. The scoped executor checks it before and after write/flush;
plain, foreign, image-only, canceled, extended-deadline and escaped contexts
refuse. Executor modes are mutually exclusive. StageSQLite prepares domain and
dedup together in the supplied active transaction; its result is only a proposal
until the actual enclosing MemberCommand succeeds. Ended transactions refuse.
The coordinator preserves the Store context while joining both original peer
and command cancellation, retaining the earlier dependency/transport deadline.

Initial focused default races pass 27 entries. Full enrollment-persistence,
replica and coordinator default races pass 253 entries without failures/skips
(71.339s). Direct coordinator-to-HTTP/1 and HTTP/2 tests then pass 3 entries,
observing protected small response bytes before handler return. Their domain
uses an actual current raw credential but a fixture mutation; they do not claim
production forwarded credential encoding or root domain-policy coverage.

Lint found one equivalent-struct construction style issue; it was corrected to
an explicit type conversion. Final source03 targeted default races pass all 30
entries without failures/skips (23.247s), including rollback versus proposal,
current domain/dedup completion, cancellation, scope identity and both actual
HTTP protocols. Configured scoped lint exits 0 with zero issues (9.624s). Exact
source identities and all earlier results are preserved under
.context/member-scoped-http-20261108/.

This closes the library coordinator/transport composition. Production listener
startup, raw forwarded credentials, operator activation/join/promotion, typed
root domains and complete partition/rejoin/restart acceptance remain open.


### Fixed-runner paired recording b2d197d8, November 8

Ubicloud 8-core Ubuntu 24.04 run 42530394856 completed on the same AMD EPYC
9454P VM for reference 0cc9565e and candidate b2d197d8, with Go 1.27.3,
GOMAXPROCS=6 and five fresh processes per suite. All 20 reference and 40
candidate native commands exit 0; all 811 five-value metric vectors (4,055
values), exact source manifests, command arguments, raw receipts and reaping
records verify. The original reference fails upstream slow-first/rtt_weighted
limits: 268,025 ns/op exceeds 230,468.4 and p50 186 microseconds exceeds 130.8.
The candidate fails cache hit-parallel at 177.1 ns/op against 156.5, plus six
upstream fast/strict, fast/random and slow-first/rtt_weighted ns/op/p50 limits.
Candidate blocking/zone, UDP, DoT, QUIC and persistence gates pass.

The collector preserves exit 1 for both sources and the paired result, with
comparison_eligible=false and release_qualified=false. It runs no relative
comparison because the historical reference failed its original gates. This
recording uses a different CPU model than the earlier Ryzen recording; no
cross-model baseline substitution, threshold adjustment, replacement sample or
retry was made. The earlier cache regressions and all failed recordings remain
retained. Evidence: .context/hosted-release-b2d197d8-20261108/paired-artifact/,
paired-reviewed-summary.json and paired-reviewed-vectors.csv. The release
performance gate remains open.


### F60b/G11 cancellation during active member output, November 8

A real net.Pipe regression reproduced four failures: command or original
output-scope cancellation did not interrupt a blocked Write or Flush. The
original finite deadline alone allowed output to remain active after either
parent was canceled. D337 now installs that original deadline first, then joins
both cancellation parents to one deadline-shortening callback. Both callbacks
finish before the Store output guard and dependency pins release. Explicit
cancellation checks also refuse a flush after a buffered write is canceled.
Actual commit state remains independent of transport failure; there is no retry.

Full affected replication, enrollment-persistence and coordinator default races
pass 261 entries without failures/skips (90.019s). The final pre-flush checks
then pass 64 targeted race entries (42.726s), including actual HTTP/1 and HTTP/2
composition, blocked socket interruption and callback joining. Configured lint
passes with zero issues (7.421s). Earlier errcheck and staticcheck cleanup
findings are retained. Final source04 transport races pass all eight entries
without failures/skips (7.019s). Evidence and frozen identities: .context/member-emit-cancellation-20261108/.
Production HA listener/operator/domain assembly and complete acceptance remain
open; these transport checks do not close those items.


### Cache entry padding and retained diagnostics, November 8

The restored-entry flag now sits with the other entry flags, removing unused
alignment padding without changing locks, metadata, admission, timestamps or
serialization. The unchanged 1-million-entry memory benchmark on the local
Xeon reports 308.8 bytes/entry before and 292.7 after (one fresh process each,
GOMAXPROCS=6, one iteration). This is a local diagnostic, not fixed-runner
qualification or evidence that the parallel-hit latency gate passes. Final
cache/persistence/pipeline default races pass 86 entries without failures/skips
(8.820s), and configured lint passes with zero issues (4.412s).

Two earlier local experiments were fully archived and reverted: separating the
publication lock's cache lines and using an immutable read topology with shard
exclusion. Neither improved the original parallel-hit diagnostic: the retained
three-sample medians are 213.4 ns before, 250.7 ns for padding and 242.1 ns for
the topology experiment. All samples have zero allocations. No concurrency
change from those experiments remains. A mistyped pipeline package path caused
one qualification command failure; the actual missing package then passed.
Evidence: .context/cache-publication-layout-20261108/. All hosted failures and
original release gates remain unchanged.


### F60b/G11 versioned raw credential wire, November 8

D338 adds command version 2 with bounded original API-token, session-cookie or
compact-JWT bytes and an optional JWT provider selector. Version 1 keeps its old
wire form and refuses the new field. Password/TOTP and Kubernetes credentials
are unsupported. Shape validation grants no permission. The current domain
verifier must authenticate the received bytes, compare claimed identity, and
retain verified credential bounds through commit and emission. Fingerprints bind
the raw proof; dedup stores only the fingerprint and outcome. Ordinary/Go and
structured command/proof diagnostics redact secret values; explicit JSON remains
private transport data and must not be logged or audited.

Focused default races pass 13 entries, including actual enrolled HTTP/1 and
HTTP/2 raw API-token verification with coordinator/Store output, invalid raw
proof, forged subject, missing proof and incompatible-version refusals. Extended
tests confirm raw credentials never enter the persisted complete image and a
revoked token reaches the current verifier even for an existing dedup outcome.
A shadowed test-helper name caused one coordinator compile failure; the other
two packages passed 233 entries. After the narrow rename, the full coordinator
suite passes 41 entries without failures/skips (49.512s). Configured two-package
lint passes with zero issues (9.225s); all frozen identities match. Evidence:
.context/member-credential-wire-20261108/.

The HTTP domain mutation is a fixture, and the actual current proof tested here
is an API token. This closes the raw credential wire prerequisite, not production
root domains, public forwarding ingress/final response, credential-expiry output
composition, HA listener/operator activation or complete restart/rejoin acceptance.


### G21 retained hosted failures and writer fixture fence, November 8

Hosted CI 42530377433 on b2d197d8 completed with failure. Dashboard passed
153 cases and failed initial primary convergence in the stub fixture; the
previously recorded local fix remains awaiting hosted confirmation. Go shard 2
passed. Shard 1 failed the signed stored-TSIG restart transport exchange and the
explicit writer-fencing UPDATE output case. The latter had already committed
and published its UPDATE. All other completed CI jobs passed; ci-ok failed.

ObserveAuthority sets writeBlocked before persisting/publishing its successor
term. The primary driver correctly refuses during that interval, while the test
fixture previously recognized only a term change. The fixture now recognizes
blocked/nonwritable authority or term/incarnation retirement of its exact
original token, only for the explicitly expected writer-loss case and
ErrUnavailable. Other early driver errors still fail. No production admission
was relaxed. The TSIG helper now reports phase/protocol and bounded HTTP error,
status/version and context diagnostics, without logging credentials or signed
wire bytes. Its original four-second bound and single signed exchange remain.

Before changes, the exact two failures passed 12 local default-race entries
(88.193s including build). This does not reproduce or erase the hosted failures.
The changed fixtures and related output cases pass 14 race entries with no
failures/skips (72.779s including build); frozen inputs match. The TSIG cause
remains unconfirmed and requires hosted diagnostic evidence. Evidence:
.context/hosted-go-failures-20261108/ and
.context/hosted-release-b2d197d8-20261108/final-ci.json.

Configured command-package lint passed with zero issues (60.919s). A bounded
ten-lifecycle investigation then passed all ten fresh signed restart tests
(137.891s including build): 40 real restart phases, six transports per phase,
with original four-second exchange bounds and no replay. The hosted TSIG
failure remains unreproduced; these successes do not establish its cause.
All frozen source identities and the protected dashboard artifact match.


### F60b/G11 verified credential lifetime through output, November 8

D339 adds ReauthorizeCurrentForOutput, sharing the existing raw-proof verifier
and final commit check. Its opaque CredentialLifetime retains only original
verified timing and the trusted clock. Under the separately admitted original
Store output scope it shortens the finite context to the verified token/JWT or
cookie absolute/idle deadline, then checks time and cancellation around actual
emission. It supplies no permission, writer authority or completion receipt.
Password/TOTP forwarding remains unsupported; no transaction context is retained
after commit. Actual output refusal leaves durable completion independent.

Tests use real API tokens, sealed cookies and signed JWTs across eight local/
external credential variants. They cover earlier operation bounds, expiry
before/during output, clock regression, cancellation, nil/unverified evidence
and preserved committed rows. Enrolled HTTP/1 and HTTP/2 API-token composition
uses the derived Store-scoped context. An actual net.Pipe write stops at the
verified token expiry; pre-output expiry enters no transport callback.

The first focused command passed ten serving-auth entries but failed to compile
the coordinator fixture because tokens.Parse returns a Boolean. That failure
is retained; the corrected coordinator selection passes 14 entries. Full
serving-auth/coordinator/replica default races pass 339 entries without failures
or skips (94.383s). Configured three-package lint passes with zero issues
(14.232s). All frozen sources match. Evidence:
.context/member-credential-output-20261108/.

Production root domains, public forwarding ingress/final response, HA listener
activation/join/promotion and complete restart/partition/rejoin acceptance remain
open. Cookie/JWT verification here is concrete but its member HTTP wire
composition remains separate follow-up work. No release qualification is claimed.


### F60b/G11 cookie/JWT member HTTP verification, November 8

D340 defines version-2 credential references without inventing an API-token ID
for cookies or JWTs: API tokens retain their actual ID; cookies/JWTs use a
purpose-separated SHA-256 digest of the exact typed proof and original provider
selector. Reference equality grants nothing and follows successful current raw
verification, alongside verified principal/provider/issuance comparisons. The
version-1 protocol and mandatory current authorization/dedup rules are unchanged.

The enrolled HTTP/1 and HTTP/2 fixture now sends actual API tokens, sealed
registered cookies and signed JWTs through the original command, current
transaction verifier and Store-scoped bounded output. Every transport/proof
combination refuses invalid raw proof, forged subjects/references, missing proof
and incompatible versions without state changes. Original dedup replays reach
the current verifier and refuse after token deletion, session revocation or JWT
grant removal. Complete persisted images contain none of the original raw
credentials. The mutation remains an explicitly identified fixture domain.

Focused default races pass 38 entries. Full coordinator/replica races pass 163
entries without failures/skips (80.201s), with frozen identities unchanged.
Evidence: .context/member-cookie-jwt-http-20261108/. Root production domains,
public forwarding/final response, enrolled listener/operator startup and full
HA restart/partition/rejoin acceptance remain open.

Configured two-package lint passes with zero issues (9.025s); source identities
remain matched. No qualification beyond the recorded scope is inferred.


### F60b/G11 first typed root member command, November 8

D341 adds the actual root scoped executor for version-2 blocking.toggle. Unknown
operations, generic object/status dispatch and legacy credential envelopes
refuse. An exact Kudurru/default resource-version precondition protects new
commands, including no-ops. The payload requires explicit blocking intent and
permits only an absolute whole-second deadline; relative timers cannot restart
at the receiver. Current effective configuration is validated before the same
transaction stages canonical blocking status and the command's dedup outcome.

The handler retains the original running elected writer, current provider/host
pins and actual raw blocking.toggle permission through commit and one Store-
admitted response. Verified subject/provider/issuance/reference comparisons
precede effects and dedup reads. Current authorization also supplies actual
audit actor/path fields; ReauthorizeCurrentForOutput now returns that decision
alongside its detached lifetime. Original request, envelope and credential
bounds survive through emission. The response says committed with publication
pending, and the publisher is woken outside the protected scope. It never
claims that a successful command has already reached the serving publication.

The actual root executor passes 15 race entries through enrolled HTTP/1 and
HTTP/2, checking canonical status, absolute intent, authorized exact dedup
replay, stale versions, unsupported operations/relative timers/missing intent,
forged references, invalid proof, revoked-token replay and durable audit
attribution with no raw credential in image or audit. The initial compile
collision with the existing replica test helper, missing original writer in
enrollment setup (three failures before target entry), and incorrect raw JSON
byte equality after dedup canonicalization (three failures after the positive
root mutation) are all retained. The final test compares the complete structured
receipt and exact resulting generation; it does not recapture or renew intent.

Composed root/member/lifetime races pass 81 entries (54.328s). That selection's
names did not include the older ServingBlocking/ServingCurrentCommand suites;
a separate exact selection passes all 51 entries (56.526s). No failures/skips
occur in either final selection. Configured command/auth/coordinator lint passes
with zero issues (55.323s). All frozen inputs match. Evidence:
.context/root-member-blocking-20261108/.

The test explicitly sets up enrollment and mounts the real root executor on the
enrolled HTTP server. Production listener/operator activation, public forwarding
and final public responses, other typed root domains and full HA acceptance
remain open. Existing public blocking commands and their publication contract
remain separate. This is not completion of F60b/G11 or release qualification.


### F60b/G11 retained enrolled listener startup, November 9

D342 mounts existing enrolled SQLite history on the production internal socket.
The exact local member SNI selects TLS 1.3 mutual authentication; legacy TLS,
status/list credentials and routes stay separate. Every handshake reads bounded
complete enrollment, while each actual request still captures the original
Store owner, peer certificate and writer before body acquisition and retains
those checks through command/image output. HTTP Host cannot select a route set.
The root blocking executor is mounted directly. Startup requires the original
private mode-0600 member-identity.json and active exact local CA; missing,
permissive or same-UUID replacement identities refuse without repair. Explicit
dormant enrollment leaves member routes disabled, including installed read-only
images. First activation requires restart; no operator activation is implied.

The initial focused races pass 19 entries. The final exact startup/root-command
selection passes 23 entries with no failures/skips (56.912s), including actual
runServe twice, unchanged private identity/enrollment, HTTP/2 image reads and
HTTP/1+HTTP/2 root blocking commands. Complete internalapi/memberidentity/replica
races pass 184 entries without failures/skips (32.662s). Source identities match.
The subsequent readiness diagnostic adds the publisher's bounded lifecycle
reason without private object errors or altered admission/deadline behavior.

The broader selector inadvertently included unrelated CLI TLS/browser suites.
It retained 111 pass entries, one installed-Chrome skip, a static-twin node-b
readiness failure and the command package's four-minute timeout. One exact
current-source diagnostic reproduces the static failure. The committed parent
4bc1d7f0 in an isolated checkout also reproduces the same failure under the same
bounds, proving it predates these listener changes. The added readiness reason
reports preparing. A bounded race CPU profile retains another failure and shows
substantial complete-image validation/preparation and race-instrumentation cost;
it does not establish a safe correction or justify relaxing validators, tests
or release performance limits. All failed runs and the isolated parent remain.
Evidence: .context/member-listener-startup-20261108/.

Operator activation/join/promotion, outgoing replica acquisition/public command
forwarding, remaining typed root domains, the pre-existing static startup
failure and complete HA/release acceptance remain open. This is scoped listener
qualification, not F60b/G11 completion or release qualification.


### G21 hosted predecessor qualification, November 9

Hosted CI 42535008378 on d9cc74ed completed successfully: both Go shards,
all six command-package batches, browser suite and ci-ok pass. Website, AD DNS,
license-server, Compose, docs, security, dev-image, edge and kind workflows also
completed successfully on that same source. The earlier stub convergence,
stored-TSIG exchange and UPDATE writer-fixture failures remain preserved; this
run does not establish the previously unreproduced TSIG cause. It also does not
erase the later local GOMAXPROCS=2 static-twin readiness failure reproduced on
parent 4bc1d7f0. The three subsequent credential/root-command commits and retained
listener work require their own hosted run. No final-source or release completion
is inferred. Private logs and terminal receipts:
.context/hosted-release-d9cc74ed-20261109/.

Retained-listener configured command/internalapi/SQLite lint passes with zero
issues (45.898s). The first lint result's three test-only gosec findings remain
recorded; local annotations explain the intentionally insecure temporary key
mode and byte-only fixture reads. Assertions, production behavior and original
bounds are unchanged. Final source04 identities and protected dashboard bytes
match. No new claim closes the reproduced static startup or release gates.


### G21/F65 provider acquisition scheduling, November 9

D343 corrects a lost-notification window after successful reconciliation and
removes periodic full-image validation after success. Subscriptions precede
reads and survive host release; original host deadlines and bounded error
retries remain. Complete validators, Store barriers and command admission are
unchanged. The original implementation fails the new idle-poll and held-release
regression assertions. The final minimal source passes all 73 coordinator race
entries (65.843s package / 70.152s wall) and configured lint with zero issues
(8.829s). The new tests' initial cleanup failures remain in the receipts; cleanup
now joins under an independent bounded context.

The unchanged static-twin lifecycle test passes once with GOMAXPROCS=8
(50.507s package) and once with GOMAXPROCS=4 (52.540s, matching ordinary CI's
four cores). Its original per-node 20-second readiness bounds are unchanged.
The earlier GOMAXPROCS=2 failure remains open. Successful race tests at other
processor counts are not a performance-gate waiver. Experimental planning
caches, schema memoization and canonical-JSON refinements did not resolve that
failure and were archived and removed before final qualification. No such
optimization is included. Exact frozen source and all results are retained in
.context/static-startup-20261109/.

Hosted CI 42539324120 on predecessor 5afee4cb is FAIL. The first-image follower
and pending-TSIG follower abort at member listener setup with invalid complete
enrollment; a separate signed HTTP/3 restart exchange reports H3_INTERNAL_ERROR.
Other jobs, including the second Go shard, pass. These are retained failures to
correct, not evidence of final-source or release completion.


### G21 pending follower listener correction, November 9

D344 fixes the member-listener abort reproduced in hosted run42539324120.
Only the exact migration sentinel with no installed generation/history on an
Unenrolled/Follower receiver returns a distinct pending result. Root leaves
member routes disabled; missing/corrupt/extra/previously installed histories
remain errors. Original mounted tests and readiness bounds are unchanged.

The new SQLite pending/corruption tests and existing first-image follower,
pending-TSIG, four-phase six-transport encrypted restart, live member revocation
and enrolled-listener restart cases pass 21 race entries with no skips/failures.
Packages take 3.320s and 42.545s; compilation plus execution takes 142.110s.
Configured command/SQLite lint passes with zero issues (66.158s); frozen source
identities match. Evidence: .context/pending-member-listener-20261109/.
The prior hosted HTTP/3 refusal remains recorded and did not reproduce in this
composed run; no cause or final hosted qualification is inferred.


### F21c/F63 retained cache diagnostics, November 9

Two source experiments were archived and fully withdrawn. Deferring provenance
copying passed 82 cache/stage race entries after correcting a new test's
serialized-versus-monotonic clock comparison (initial 81 pass/one failure
retained), but the unchanged original BenchmarkGet parallel-hit diagnostic was
262.2 ns versus 223.4 ns before. Wider shard separation measured 288.0 ns. Both
are single-process local observations with zero allocations, not fixed-class
gates; no optimization is retained. A bounded original parallel-hit CPU profile
shows shared reader-count atomics, shard locking and Prometheus counter updates
as substantial costs. It does not establish a safe concurrency replacement.
Evidence: .context/cache-lookup-metadata-20261109/ and
.context/cache-shard-isolation-20261109/. Original benchmark bodies, thresholds,
baselines and the prior hosted cache/upstream failures remain unchanged.


### F60b/G11 reviewed first-primary API and CLI, November 9

D345 mounts complete enrollment review and first-primary activation on the
authenticated nonstatic SQLite router and generated client. The operator must
supply exact separately reviewed cluster/member/source/owner identities, local
name/address and audit reason. Original publication capture precedes body input;
a finite 30-second request, current raw proof, cluster.join and cookie CSRF
protect the same transaction as membership, Primary authority and audit.
Private identity creation is exclusive; an existing exact valid identity is
retained, and missing/corrupt/permissive keys are never repaired.

The dedicated CLI sends one request without redirects, replayable bodies,
implicit review refresh or retry. The public receipt reports only actual commit
and required restart. Unknown completion retains its distinct error and cannot
be inferred as rollback. Join-token, join, promotion, outgoing acquisition,
public forwarding, other typed domains and complete HA acceptance remain open.

Initial focused races pass all 15 entries without failures/skips (77.782s wall),
including actual-router CLI success, stale review, source/token revocation during
body acquisition, cancellation, original deadline, identity refusal/retention
and cookie CSRF. Generated OpenAPI/client dispatch is current. Additional audit
rollback, one-shot client and composed storage/listener qualification follow; no
release gate is closed by this scoped work. Evidence:
.context/cluster-activation-20261109/.

Composed default races pass all 73 entries across root commands, HTTP source
ownership, enrollment persistence, API and client packages (132.353s wall),
with no failures/skips and all frozen identities matched. This includes actual
audit-trigger rollback after identity creation; current permission ordering;
redirect/unknown-outcome refusal; original storage commit, unknown completion,
physical restart and follower-install acceptance; and retained root listener
restart with HTTP/1 and HTTP/2 member trust. OpenAPI and CLI reference drift
checks pass. Source review corrected capability admission to follow declared
permission checks; no earlier refused request was replayed.

Final source keeps identity filesystem I/O outside Command: original published
permission and exact dormant-source review precede file acquisition, and the
transaction independently repeats current proof/source/owner admission. The
corrected activation/CLI selection passes all 14 race entries (95.245s wall).
The source-token zero/released/canceled checks separately pass both race entries.
Final configured five-package lint passes with zero issues (46.505s), and all
frozen identities including the protected dashboard artifact match. No original
assertions, deadlines, benchmark thresholds or workload inputs changed.

Hosted CI 42545168959 on predecessor 73d4b057 reports an actual QUIC data race
in TestQUICSocketDNSAcrossMigration/doh3/sockets=1: ConnectionState reads the
send-connection pointer/capabilities while switchToNewPath replaces it. The full
failed shard log is retained under .context/hosted-release-73d4b057-20261109/.
That source is not CI-qualified; the race is the next corrective work. The
paired release measurement 42545184734 is still running, with no gate claim.


### G21 QUIC migration state race, November 9

Hosted 73d4b057 shard 2 fails the unchanged HTTP/3 socket-migration test with
concurrent ConnectionState and switchToNewPath sender accesses. A new real
DoQ/HTTP3 concurrent state/address test reproduces this on original vendored
bytes (exit 1, three failed leaves plus parent, one passing leaf). A separate
upstream sender test reproduces the GSO-failure flag race (exit 1, one failure).
Both failures and the complete hosted trace are retained.

D346 synchronizes publication and public address snapshots with the existing
connection-state mutex, and makes the send queue's GSO-refusal flag atomic.
Packet fallback, path/CID retirement, socket counts and original deadlines are
unchanged. Original tests and the upstream source manifest remain byte-identical.
The new and original application QUIC tests pass 26 race entries; upstream
sender/path/GSO tests pass 25 race entries. Neither selection skips tests.

The full DNS-server default-race suite passes 381 entries without failures
(55.534s wall), with four actual opt-in measurement/helper skips: TestSpikeMeasure,
TestSpikeDoQGoroutines, TestSpikeDoQHelper and TestMeasureAXFR100k. These do not
qualify the separate performance measurements. Upstream module vet passes
(6.416s). Frozen source identities match throughout. Evidence:
.context/quic-state-race-20261109/. Final lint/hosted qualification remains
separate; no earlier HTTP/3 signed-exchange cause or release success is inferred.

Final configured DNS-server lint passes with zero issues (14.434s wall). All
source03 identities match, including the original socket fixture, upstream
manifest and protected dashboard artifact. The reproduced preimage failures
remain retained; no hosted final-source or performance completion is inferred.


### Fixed-runner paired measurement, November 9: failed

Ubicloud run 42545184734 completed on 73d4b057 with five fresh processes per
scope and GOMAXPROCS=6. Both sides report AMD EPYC 9454P and Go 1.27.3.
Independent artifact review matches all 406 reference and 4,715 candidate
source identities to their commits, every report digest and all numerical
receipts. All 60 measurement commands exited 0 and reaped their process groups;
1,095 reference and 2,960 candidate metric values each form five-value vectors.

The historical reference passes all three absolute gates, so THIS paired
comparison is eligible. The candidate fails cache and upstream absolute gates:
parallel cache hit is 173.6 ns against 156.48 ns, and six upstream latency
measurements exceed their unchanged limits. Blocking/zone, UDP, DoT, QUIC and
persistence absolute gates pass. Paired relative comparisons fail with seven
blocking/zone regressions, one cache regression and ten upstream regressions.
There are no missing comparison keys. All thresholds and workloads are unchanged.

This result does not inherit the previous run's reference-ineligibility refusal.
Both that earlier failure and these measured failures remain preserved. Release
performance is NOT qualified; no rerun or baseline relaxation closes it. Evidence:
.context/hosted-release-73d4b057-20261109/paired-reviewed-summary.json and
paired-reviewed-vectors.csv, with the complete original artifact alongside.


### F60b/G11 reviewed invitation issuance, November 9

D347 mounts `cluster join-token` and POST /cluster/join-token on the complete
enrolled Primary runtime. Exact separately reviewed identities, target intent,
absolute expiry, current cluster.join and cookie CSRF protect one command. A
Store-issued request is captured before body acquisition; real SQL completion
issues the only output scope while publisher/resource/host/writer pins remain
held. Secret output is deadline-bound and explicitly flushed before releasing
those pins. Only its hash and attributed audit persist. Unknown completion and
committed-but-unavailable output remain distinct, with no redirect or replay.

Initial composed races pass 40 entries across Store, coordinator, root router,
API and client. The final generated-contract/router/client selection passes
242 entries with zero failures/skips, including actual HTTP/1 and HTTP/2, CLI,
source interposition during body acquisition, invalid/reviewed expiry, cookie
CSRF, actual audit-trigger rollback, output-flush failure after commit, and an
incomplete real HTTP body bounded by its original 150ms request. The Store
selection includes actual SQLite 1811 rollback and 787 unknown completion,
no-op/write receipt separation, cancellation and one-use copied handles.

OpenAPI and generated dispatch now contain 188 operations; CLI reference
generation passes. Evidence: .context/operator-command-output-20261109/. Broader
Store/coordinator regression and configured lint follow. Joining, promotion,
outgoing acquisition/forwarding, driver/AD work and release gates remain open.

Final full SQLite default-race qualification passes 709 entries with zero
failures/skips (551.572s wall). Coordinator regression separately passes all
74 entries. The preceding combined command's 240-second SQLite package timeout
is retained: 573 SQLite passes plus 74 coordinator passes, with no assertion
failure before timeout. The successful rerun used a 10-minute package bound;
no fixture deadlines or workloads changed. Configured five-package lint passes
with zero issues (77.781s); frozen source and protected dashboard identities match.

Hosted predecessor CI 42545168959 completed with both Go shards failing.
Besides the reproduced QUIC race corrected by D346, shard 1 retains recovered-
anchor startup failure with an adjacent ops-port bind collision, an anchor
publication capture reporting generation pending, and a phase-1 signed HTTP/3
restart exchange reporting H3_INTERNAL_ERROR. Full logs and terminal status are
preserved under .context/hosted-release-73d4b057-20261109/. These require separate
correction/qualification; this commit does not claim full CI or release success.


### G23 cache reader admission correction, November 9

D348 removes the shared publication-reader counter from ordinary cache Get.
Per-shard locking validates exact immutable addressing; every complete mutation
suspends lookup admission, and fallback resamples time after the full publication.
Resizing drains old readers and releases retired maps. New concurrency tests
cover delayed readers across resize/flush, expiry while held, and a deliberately
partial two-shard flush. Original tests, workloads, limits and baselines remain.

All 92 cache/persistence/stage race entries pass, followed by all 18 mounted
DNS/flush/growth/HTTP-inspection/anchor entries; neither run fails or skips.
One original BenchmarkGet process per source, GOMAXPROCS=6 and 1s per case,
measures hit 844.5 to 762.8 ns, miss 174.6 to 154.8 ns and parallel hit 237.7 to
164.8 ns (0.693310896x). Every case retains zero bytes/allocations per operation.
This local diagnostic supports the correction but cannot close the failed fixed-
runner gate. Configured lint follows. Evidence:
.context/cache-reader-admission-20261109/. All prior measured failures remain.

Final configured cache/stage lint passes with zero issues (5.217s). All frozen
identities match; original benchmark/fixture/limit files and the protected
dashboard artifact remain unchanged. Fixed-runner qualification remains open.


### Startup validation correction, November 9

D349 retains complete Store validation with its same-transaction token for
provider reconciliation, reuses the primary driver's unchanged planning roster
across scheduling-only hints, and bounds successful public CRD fingerprints to
256 entries per immutable Validator. Keys include kind, encoded document digest
and validation modes. No document or decoded secret is retained. Failed checks
never enter the cache. Strict decode, source metadata, complete-image integrity,
private owners/custom validators and final source/writer/dependency/publication
checks remain; ordinary write admission/defaulting/status methods are unchanged.

The original recovered-anchor, direct-anchor bootstrap and stored-TSIG restart
race selection passes all three tests without failures/skips (96.625s wall).
Both recovered-trust restarts retain their original 10-second readiness deadlines
and all six transports. The actual static twins peer-loss/restart and enrollment-
refusal selection also passes both tests at GOMAXPROCS=2 (53.134s wall), correcting
the reproduced node-b readiness failure without fixture changes. Earlier failed
Store-only and driver-only attempts remain recorded; neither alone fixed startup.

Full affected-package default-race qualification passes 1,346 entries: SQLite
712, Validator 333, provider coordinator 74, primary driver 9 and serving builder
218. There are zero failures/skips (438.587s wall). Configured lint on all five
packages passes with zero issues (50.707s). Every frozen source identity matches,
including the protected dashboard artifact. Evidence:
.context/serving-validated-cut-20261109/; retained profiles and temporary traces
are under .context/hosted-root-refusals-20261109/. Instrumentation was removed
before qualification; original fixtures, deadlines, workloads and gates remain.

Hosted b176922a run 42551432186 exposed a stale generated API endpoint reference
for enrollment review, activation and join-token, now regenerated from the
existing contract. Both API and CLI reference-drift tests pass (25.056s wall).
The failed hosted log is retained. Final hosted checks and the failed fixed-runner performance
comparison remain separate; no complete release qualification is claimed.


### Follower acquisition queue correction, November 9

D350 corrects an unbounded mutex wait after the follower pull had already
established its finite lifetime. Cancellation-aware admission retains the same
serialization through acquisition and installation, with no detached waiter or
late source capture. A queued refusal does not rewrite the active pull's status.
The new actual SQLite/held-source regression reproduces both canceled and expired
waiters outwaiting their original lifetime on the preimage. With the correction,
all 108 follower/replica default-race entries pass without failures/skips (31.059s
wall); configured two-package lint passes with zero issues (7.216s). Source
identities and the protected dashboard artifact match. Evidence:
.context/follower-pull-admission-20261109/. The failed preimage remains preserved.
Production enrolled acquisition, join/promotion/forwarding and full HA acceptance
remain open.

A separate ordinary blocking-evaluation frame extraction was withdrawn after one
unchanged 14-leaf before/after diagnostic showed mixed results and worse target
misses, despite a smaller frame. Exact original bytes were restored and verified;
no production optimization or gate credit was retained. Evidence:
.context/blocking-evaluate-frame-20261109/. Paired Ubicloud run 42554034011 is
measuring 9963dec2 after the qualified cache correction; it has no outcome yet.


### Retained enrolled acquisition and publication, November 9

D351 mounts the existing replica Coordinator for retained enrolled SQLite nodes.
Startup loads the exact private identity and installs prepared listener/worker
before Run. Every exchange constructs pure validators under its own original
context. A Store-issued scope holds membership/authority exclusion across one
bounded mutual-TLS exchange without a SQL transaction; cancellation-aware queue
admission, original CA expirations, actual peer verification and one-attempt term
observation prevent replacement or renewed authority. Higher terms fence and
retire the old acquisition. The original Puller capture still admits actual
installation, 304 acceptance and successful-contact bookkeeping. Shutdown joins
the worker. Initial remote join, operator promotion, forwarding and complete
partition/rejoin acceptance remain open.

The actual mounted listener/command/acquisition race selection passes 20 entries,
including scheduled successor acquisition/publication, physical database restart
with the retained private CA, 304, remote revocation and joined shutdown. Full
enrollment/member-identity/replica/follower races pass 313 entries (105.209s wall);
SQLite authority/enrollment/fencing/promotion/replica races pass 111 (39.885s).
No selected test fails or skips; the accepted-owner package has no test files.
Configured four-package lint passes with zero issues (71.148s). The final lint
changes are comments and a test-local variable rename only; race source09 and
lint source14 manifests retain that distinction. The protected dashboard artifact
and all original test/assertion/deadline/benchmark files remain unchanged.

Earlier preparation failures are retained: missing optional host label, a runtime
setup guard incompatible with the original command fixture, a compile-only return
arity error, and four lint naming/comment findings. None is reported as a pass.
Evidence: .context/member-acquisition-20261109/.

### Paired Ubicloud performance acceptance on 9963dec2, November 9

Run 42554034011 passes all three strict same-VM relative comparisons and all seven
candidate absolute gates on the owner-selected ubicloud-standard-8-ubuntu-2404
class, Go 1.27.3, GOMAXPROCS=6, five fresh processes per workload. The historical
reference's three absolute gates also pass, making the comparison eligible.
Independent review verifies all source blobs, report digests, numeric outcomes,
full group reaping and all five-value vectors: reference 45 cases/219 keys/1,095
values; candidate 136 cases/592 keys/2,960 values. All 60 native commands and 13
native gate/comparison commands exit zero, with no missing or failed gate keys.

Both recordings used AMD Ryzen 9 7950X3D. The previous failed paired run used
EPYC 9454P, so cross-run gains cannot be attributed entirely to the cache change.
The earlier 18 relative and seven absolute failures remain preserved. This is
measured performance acceptance for 9963dec2; the recorder deliberately retains
release_qualified=false. Later source changes, final hosted checks, remaining
features and external Windows/two-VM acceptance are not closed by it. Evidence:
.context/hosted-release-9963dec2-20261109/paired-artifact/ and the independently
verified paired-reviewed-summary.json / paired-reviewed-vectors.csv.


### Promotion queue lifetime, November 9

D352 corrects the next original-lifetime gap exposed by the actual enrolled
acquisition mount: promotion could wait past its deadline behind a retained
network exchange. The new actual-TLS/SQLite preimage test fails at the queue
boundary; the correction reuses cancellation-aware writer-lock admission without
changing authorization, installed-generation CAS, eligibility or final checks.
Focused acquisition/promotion races pass seven entries, related SQLite/replica
authority races pass 104, and the final deadline/cancel regression passes three.
There are no failures/skips in those corrected runs. Scoped lint passes with zero
issues (6.617s). The first lint's two unchecked cleanup-channel results were
corrected with explicit cancellation assertions and requalified. Original failed
preimage and lint evidence remain under .context/promotion-queue-20261109/.
The operator promotion API/CLI and full HA acceptance remain open.


### Reviewed operator promotion API and CLI, November 9

D354 mounts operator promotion through the retained-member Coordinator. The
original HTTP proof/source and at most 30-second lifetime precede body reads.
`cluster promotion -o json` supplies the complete reviewed generation/member;
`cluster promote --review FILE --confirm-node NAME --request-id ID --reason TEXT`
sends one explicit decision without redirects, source refresh or automatic replay.
Current cluster.promote and cookie CSRF are mandatory. Coordinator queue and real
publication pin precede all lower dependency exclusions and actual installed-image
CAS. Final original credential/time/source checks retain those exclusions through
SQL completion. Canonical attributed audit and promotion commit together; unknown
completion stays distinct from success/rollback. The committed receipt makes no
publication or peer-convergence claim. Generated API/client/reference contracts
now include 190 operations.

The final mounted default-race selection passes four entries: two root cases and
two public API refusal cases. Actual TLS primary loss and production monotonic
eligibility lead to one authenticated CLI promotion, new-term whole publication,
physical restart and retained private CA/audit. Viewer, wrong-node, stale-source,
slow-body and independently revoked held-source requests refuse. Cookie CSRF is
checked before body acquisition. The first image remains explicit fixture
administration; initial network-join acceptance is separate.

Full replica/servingauth/apiclient races pass 99/213/318 entries. Focused SQLite
and public schema races pass 18/5 entries, including actual SQL 1811 audit rollback
and 787 commit failure, original final-proof refusal, duplicate and restart.
Configured scoped lint passes with zero issues (8.820s); final changes after the
race manifest are an interface comment and unused test-parameter rename only.
Generated client and CLI/API references are refreshed. Evidence is retained under
.context/promotion-mount-20261109/ with source and numeric receipts.

The broader local API run is explicitly incomplete: 434 passing entries, one
actual missing-PostgreSQL skip, then its four-minute package timeout in
TestQueryExportPartialAndBounds. The existing large migration case passed before
the timeout. This run exits 1; it is not full API acceptance. Earlier import-name,
missing fixture Hasher, expected-installed fixture CAS and lint findings remain
preserved. Hosted CI 42553837447 on 9963dec2 has since completed successfully
(all 22 jobs); later commits still require their own hosted results.
Initial remote join, remaining typed forwarding, Kubernetes barriers and complete
HA/final release gates remain open. AD preview deferral remains D353.


### Initial remote join scope — November 9, 2026 (D355)

Mounted offline `cluster join` into a nonexistent private directory, original
invitation-CA TLS on a distinct SNI, original shared-credential guard and signed
member possession. Primary source/enrollment/output capture precedes proposal
body acquisition; one-time acceptance and attributed audit commit atomically.
The receiver captures the actual never-installed authority/anchor cut before
contact, then installs the original primary term, complete image and local anchor
carrier in one transaction. Existing data, independent source changes, expired
or canceled attempts and repeated invitations refuse. Output distinguishes
primary acceptance from local installation; failures preserve identity and never
trigger replay, trust discovery or implicit repair.

Evidence under `.context/initial-join-20261109/`: `join04` ordinary exit 0,
22 actual pass entries; `lower-race08` exit 0, 373 race entries across member
identity/internal transport/enrollment/persistence/replication; `root-race10`
exit 0, 37 race entries across affected SQLite and root command paths. There are
no test failures/skips in those passing selections (accepted-owner has no test
files). Actual CLI/TLS first image, physical reopen/private-CA retention,
subsequent enrolled acquisition, wrong shared key, original pre-body source
interposition, one-time acceptance, cancellation, bounded lock queue and actual
SQLite 1811/787 boundaries are covered. Primary activation remains explicit
fixture administration; these are not independent VM or full partition/rejoin
acceptance. Preserve `compile02`'s TLS handshake-callback failure and
`boundaries06`'s test-wrapper compile failure; both are corrected in later checks.

Incomplete-join reconciliation, remaining public/typed forwarding, complete HA
and final-source hosted/performance/driver/deployment qualification remain open.
AD remains owner-deferred preview under D353. No release is published or implied.

Hosted security on prior source `0a1a44cb` found two Semgrep missing-unlock
warnings in `Cache.lockLookup`. Both returns intentionally transfer the locked
shard to its sole caller, `Get`, whose hit/miss/expired/stale branches all unlock.
Narrow annotated exceptions document that ownership; no cache behavior or rule
configuration is changed. The final configured scan result is recorded below.

Final qualification for this join scope: `final-race14` exit 0, 14 actual race
entries, including exclusive original-deadline database creation and existing-file
refusal; `lint17` exit 0, zero issues; `generate18` exit 0, updated CLI reference.
Pinned Semgrep 1.181.0 (`sast19`) exit 0, zero findings across 2,539 scanned files,
including every changed production path and the cache helper. The same five
configured policy sets and nine existing exclusions were used with a read-only
source mount. Its 18 partial-parser warnings in unchanged paths remain in the
raw JSON; none concerns a changed production path. Earlier `lint12` unchecked
test closes and `lint15` fixture-file G304 findings remain preserved; test closes
and narrow test-only provenance annotations correct them. The pre-existing
`dashboard/dist/index.html` bytes remain untouched and excluded from staging.


### Incomplete-join reconciliation — November 9, 2026 (D356)

Initial join now commits exclusive immutable node-local intent before proposal
dispatch, binding the exact invitation fingerprint, member CA and pristine
migration carrier/authority. No bearer secret enters that metadata or public
images. Existing intent refuses another proposal capture; unconfirmed intent SQL
completion fences local admission and dispatch is refused.

The explicit offline `cluster join-reconcile` command opens current-schema
existing data and the retained private identity. Its new finite observation is
captured before one current-member TLS image GET; it never sends another POST.
The opaque historical input preserves original expiry and cannot create a
proposal. Even after invitation expiry, exact local intent, currently valid CAs,
original primary term and complete consumed-ticket/target provenance are required.
The first image, authority and local carrier install atomically. Missing intent,
changed identity, pruned provenance, an unaccepted member and an already installed
receiver refuse. Observed membership is not a new primary commit or serving
publication. Older incomplete receivers without intent are not silently repaired.

Focused default-race `race04` passes all 54 actual entries with no failures/skips
in 107.025 seconds. This includes actual CLI/TLS reconciliation after a real
expired invitation and first-image validation failure, physical restart/private
CA retention, no acceptance replay, missing/changed intent, same-UUID replacement
CA, cancellation, original lock deadline, shared-cell consumption and actual
SQLite 1811/787 intent/install boundaries. `ordinary02` preserves one incorrect
new test assumption about structural proposal signing; source03 tests expiry at
actual Accept instead. Its 48 other entries passed. Evidence is retained under
`.context/join-reconciliation-20261109/`; broader qualification follows below.

Remaining typed/public forwarding, Kubernetes authority/management and ownership,
full supported-driver/partition/rejoin/independent-VM acceptance, and final-source
hosted/performance/deployment gates remain open. AD remains owner-deferred preview
under D353. No release publication or completion claim follows from this scope.

The broader `lower-race05` command reached its original 420-second outer bound
and was terminated (child -15; wrapper 241) after 1,009 actual pass entries, zero
test failures/skips. Enrollment, persistence and replica packages completed
successfully; the accepted-owner package has no tests. SQLite was interrupted in
TestWatchSlowConsumerCatchesUp, so this is **not** a full SQLite pass. The existing
10,000-object test completed; no test deadline or assertion was changed. All
source hashes matched and the owned process group is empty. Full final-source
hosted coverage remains required.

Final scoped qualification: `lint08` exits 0 with zero issues (15.434 seconds);
`generate09` completes `make docs-gen` (41.486 seconds). Pinned Semgrep 1.181.0
`sast10` exits 0 with zero findings across all seven changed production files,
using the unchanged five policy sets and nine exclusions, a read-only source
mount and 126 applicable rules (6.821 seconds). Full hosted SAST remains a
separate final-source gate. `lint06` retains its two test-only findings; source07
only renames the copied request variable and annotates the intentional generic
JSON secrecy assertion. Production bytes match race-passing source03. The
protected dashboard artifact remains unchanged and unstaged.

Predecessor hosted CI 42559340879 on 0a1a44cb completed with one failing test:
TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled received an HTTP/2
INTERNAL_ERROR during its DoH exchange. Other test shard passed; ci-ok correctly
failed. Its raw log is retained for a separate investigation. This recovery scope
does not explain or close that failure, or qualify the overall release.


### Managed-anchor hosted DoH investigation — November 9, 2026

The retained predecessor CI failure (42559340879 on 0a1a44cb) remains unexplained.
Phase/protocol diagnostics preserve all original lifecycle assertions and bounds.
One bounded default-race invocation repeated the test ten times: ten passes, no
failures/skips, exit 0, 156.527 seconds; source identities matched. This local
nonreproduction does not close the hosted HTTP/2 INTERNAL_ERROR or establish a
production fix. Evidence: `.context/anchor-doh-ci-20261109/`. Final-source hosted
acceptance remains required.


### F60b/G11 original follower blocking forwarding — November 9, 2026 (D357)

The enrolled SQLite follower now captures its original complete HTTP source,
private identity and finite attempt before reading a blocking command body.
One 30-second lifetime begins before authentication; strict body decoding resolves
relative timers once against the pre-body observation. Actual publication,
provider, host, key, catalog and transferred-owner exclusions precede Store
membership/source admission and the one member-mTLS POST. Queueing cannot adopt
a successor generation, term or credential. The primary independently verifies
the raw proof, checks the original settings version and commits the blocking
effect, attributed audit and dedup result atomically.

The authenticated HTTP 202 receipt confirms primary commit with publication
pending. It does not install a follower image or claim local serving convergence.
Headers/body are explicitly flushed under the original source and credential,
certificate, provider and host bounds. Dashboard refetch preserves observed local
state; CLI text/JSON distinguishes the pending receipt. OpenAPI and both generated
clients include the response. Other mutation domains remain closed.

Evidence: `.context/member-forward-20261109/`. Final compatibility `compat-race20`
passes 254 actual test entries, zero failures/test skips, exit 0, 133.547 seconds;
the generated OpenAPI Go package has no test files. It includes actual HTTP1/2
follower forwarding, original held-body source refusal, later explicit Coordinator
installation/publication, join, promotion, listener/restart and current-command
compatibility. Focused lower `race02` passes 12 entries; root/CLI `race09` passes
six. The composed fixture uses independent per-node auth-key owners; its later
pull is explicit so receipt-before-install can be observed. This is not automated
convergence or partition/rejoin acceptance.

Scoped Go lint `lint15` exits 0/zero issues; dashboard BlockingControl has ten
passing tests, typecheck and scoped ESLint pass. Pinned Semgrep 1.181.0 `sast21`
scans all 18 changed production files with the original five policy sets/nine
exclusions: exit 0, zero findings/errors, 9.226 seconds. All frozen source hashes
match; the original dashboard dist modification remains untouched and unstaged.

Earlier lint findings, root test build errors and `race07` failure/interruption
remain retained. That fixture mistakenly shared the same exclusive auth-key owner
between primary and follower; the preserved owned-child stack shows the resulting
wait. Independent receiver options correct only fixture ownership. No production
lock, assertion, deadline or admission was weakened. `eslint18` records an invalid
Bun invocation (exit 1); corrected cwd invocation `eslint19` passes. These results
do not explain the earlier hosted managed-anchor HTTP/2 failure.

Remaining typed forwarding, partition/rejoin, Kubernetes authority/management and
ownership, supported-driver acceptance, assigned two-VM Linux acceptance and
final-source hosted/performance/release checks remain open. AD/Windows stays
owner-deferred preview under D353. No release publication or completion claim.


### F60b/G11 separate-process partition catch-up and restart — November 9, 2026

`TestEnrolledClusterProcessesPartitionCatchupAndRestart` runs two real server
processes with independent data directories and private identities. Public
activation and invitation APIs precede the real offline join CLI; there is no
fixture SQL, injected member transport, direct image installation or manual pull.
A loopback TCP relay controls connectivity while actual member TLS terminates in
the servers. The original primary term is unchanged throughout this case.

The actual follower forwards blocking with HTTP 202, then its scheduled worker
installs and publishes the committed state. During partition, UDP/TCP DNS retains
its valid original authoritative answers and a forwarded write refuses. The
primary independently creates and deletes records; reconnection automatically
installs both effects. The refused command is not replayed. Physical follower and
primary restarts preserve exact private-identity hashes, current primary ownership,
blocking state and authoritative data, including absence of the deleted record.

Evidence: `.context/enrolled-process-20261109/process02.json`: exit 0, one test
pass, zero failures/skips, 100.499 seconds wall including builds; test 63.65 seconds.
The harness runs with default race instrumentation; child servers use the existing
CGO-disabled synthetic-license acceptance build, not a production signing key or
race-instrumented child binary. All 4,781 source identities match. Scoped E2E lint
`lint04` exits 0/zero issues in 16.666 seconds. Original dashboard bytes remain
untouched and unstaged. This closes the unchanged-term process scenario only;
promotion/conflict reconnection, other mutation domains and supported-driver/final
release gates remain open. VM and AD deferrals retain their separate decisions.


### F60b/G11 authenticated peer observation and promoted rejoin — November 9, 2026 (D359)

Writable enrolled SQLite primaries now perform one bounded metadata-only peer
exchange per scheduled Coordinator turn. Actual current roster trust and member
TLS authenticate both sides; receiver TryLock refuses cyclic peer-lock waits.
Higher/conflicting terms use the existing durable authority transition. The
original attempt installs no image, chooses no conflict branch and supplies no
promotion eligibility. After fencing, only a later independent follower pull
captures and installs the successor. Output failure cannot erase a durable fence.

Evidence in `.context/member-authority-20261109/`: `lower-race02` exits 0 with
39 test pass entries and zero failures/skips (31.864 seconds wall); `process03`
exits 0 with both separate-process scenarios passing and zero failures/skips
(164.731 seconds wall). The promoted-rejoin scenario uses public activation,
invitation, offline join, scheduled loss eligibility and reviewed promotion,
then restarts the old primary. Only production scheduled peer observations and
pulls fence/install/publish the successor. Private identity hashes and actual
UDP/TCP authoritative data remain intact. Default race instruments the harness;
CGO-disabled synthetic-license child server binaries are not race-instrumented.
All frozen source identities match; subsequent source changes only clarify two
comments. Scoped lint `lint05` exits 0 with zero issues in 36.479 seconds;
pinned Semgrep 1.181.0 `sast06` scans all nine changed production files with zero
findings/errors, exit 0 in 6.616 seconds. Both final-source freezes match.
Broader compatibility races and final hosted checks remain separate requirements.
The older hosted CI 42563990848 was still pending when the owner requested an
immediate push; the push canceled it. Its prior/final status is retained, with no
pass inferred. The new remote commits require their own hosted qualification.

Three-process conflicting promotions/branch recovery, other forwarded mutation
domains, Kubernetes authority/ownership, supported-driver and final release
qualification remain open. AD/Windows and independent-VM VIP acceptance remain
owner-deferred under D353/D358. Prior hosted HTTP/2 failure remains unexplained.
No release publication or overall completion is claimed.


### D359 final local compatibility checkpoint — November 9, 2026

Added actual-TLS revocation-after-handshake refusal in both directions and an
equal-epoch conflict case that preserves the installed branch, refuses writers
and retains exact conflict authority across physical restart. The conflict test
covers metadata semantics, not public three-process conflicting promotions or
operator branch recovery.

On the unchanged production source committed as `57128ac7`, with these two tests:

- Full enrollment-persistence and replica default-race suites: `compat-race08`,
  exit 0, 269 test pass entries, zero failures/skips, 86.161 seconds wall.
- Mounted command-package join, reconciliation, promotion, retained acquisition,
  forwarding and member-listener default-race selection: `root-race09`, exit 0,
  52 test pass entries, zero failures/skips, 141.295 seconds wall.
- Final changed-test package lint: `lint10`, exit 0, zero issues, 6.416 seconds.

All source freezes match. Prior focused/process/lint/SAST evidence remains in
`.context/member-authority-20261109/`; no timing or qualification is inferred for
unrun scopes. The protected pre-existing dashboard build bytes remain unchanged.

Hosted kind runs 42567930338 (`50b5c2ea`) and 42568087573 (`57128ac7`) both fail
before cluster creation: the pinned kind tool download receives HTTP 504, curl
exit 22. Raw failed logs are retained. Both official download paths subsequently
respond HTTP 200 to read-only sandbox HEAD requests; that is not a successful
hosted install or Kubernetes qualification. No acceptance assertion or tool
version was changed. Final-source hosted CI/security/performance and the other
implementation/acceptance gaps remain open; no release was published.


### F60b/G11 conflicting promotions and offline branch recovery — November 9, 2026 (D360/D361)

`TestEnrolledProcessConflictResolution` runs three real server processes with
independent data directories and identities, each reached only through its own
connectivity relay. Public activation, invitations and offline joins precede
loss of the primary. B and C are each promoted while isolated from the lost
primary and from each other, then accept conflicting record writes. After the
partition heals, only production scheduled peer exchanges record durable
equal-epoch conflict on both: writes return 503 and protected HTTP closes, while
UDP/TCP authoritative DNS keeps answering each member's last-good branch,
including after a physical restart of C.

That last property initially failed (D360). `process06` shows both conflicted
members answering SERVFAIL for every name; `process09`–`process12` diagnostics
(temporary logging, since removed) located `prepare_failed: replication: writer
fenced` in the root anchor owner's inert token check, which always ran as a
writable publication. It now passes the token's own DNS-only disposition. The
in-process regression `TestRootAnchorPreparationPublishesFencedDNSOnly` fails
without the fix (`dnsonly-race03-unfixed`, publisher never reaches `dns_only`)
and passes with it. Protected HTTP stays closed on DNS-only bundles.

Branch recovery is host-local and offline (D361), because a conflicted member
has no authenticated HTTP surface; the first online-route design was withdrawn
after `process03`/`process04` showed `generation_pending` on the conflicted node.
`cluster resolution --offline` refuses a running member's locked database and
reports the exact recorded conflict; `cluster resolve --offline` refuses another
member's review, a wrong confirmation, a missing reason and a repeat after commit.
The accepted decision commits quarantine, epoch e+2, the kept generation and a
`cluster.resolve` audit row atomically. C and the restarted original primary then
fence, quarantine their own rows and install B's branch through scheduled exchanges
and pulls alone; C's divergent record is absent everywhere and is never replayed.
C forwards a blocking command to B with HTTP 202. Physical restarts retain the
resolved authority and data; all three private identity hashes are unchanged.

Evidence under `.context/conflict-resolution-20261109/`:

- Store `ResolveAudited` focused race `store-race01`: three tests (atomic audit,
  injected audit 1811, final-proof refusal, invalid attribution, duplicate without
  second audit, restart; actual 787 unknown COMMIT; stale conflict operand) pass.
- Full SQLite default race `sqlite-race01`: exit 0, 748 test pass entries, zero
  failures/skips, 6m22s wall.
- `./internal/cluster/...` default race `cluster-race01`: exit 0, 433 passes; one
  actual skip, `TestLeaseHandoff` (no local envtest assets); the accepted-owner
  package has no tests.
- Affected root selection `root-race01` (`Anchor|Doctor|Primary|Cluster|Join|
  Promot|Member|Enroll|Fenc|Replica|Follower|CLIReference`): exit 0, 274 passes;
  one actual skip, `TestDoctorDatabase` (no PostgreSQL DSN). It includes the
  managed-anchor transport test and the doctor writer case below.
- All three enrolled-process scenarios `process15`: exit 0, three passes
  (30.9 s, 49.8 s, 68.2 s), 2m41s wall including builds. The harness is
  race-instrumented; CGO-disabled synthetic-license child servers are not.
- Scoped lint `lint02`: zero issues. Pinned Semgrep 1.181.0 `sast01` with the
  unchanged five policy sets and nine exclusions over the five changed production
  files, read-only source mount: exit 0, zero findings/errors, 7.3 s.
- `source01` freezes all 15 changed paths; every later check matches it. The
  protected pre-existing dashboard build modification remains untouched and unstaged.

Hosted status for predecessor `ffd18d41`: security rerun (attempt 2) and kind
rerun (attempt 2) both pass; attempt-1 504 download logs are retained under
`.context/hosted-ffd18d41-20261109/`. CI 42568583609 failed: lint (golangci-lint
download 504) and `go test 1/2` with two test failures. (1)
`TestDoctorNotifyCannotAdoptNewPublicationKeysOrWriter/writer`: the fixture's
elector reported the deliberately fenced driver's `ErrUnavailable` before its term
was canceled. The exact-token writer-loss tolerance already used by the output
fixture now also applies when this case declares the loss; no assertion changed.
(2) `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled` failed again,
now `H3_INTERNAL_ERROR` on DoH3 in phase 1 (earlier: HTTP/2 INTERNAL_ERROR on DoH,
CI 42559340879). It passes locally in `root-race01`. An unconfirmed hypothesis is
a post-start canonical commit retiring an in-flight authoritative answer, which
HTTP transports abort. It remains **unexplained and open**.

Remaining: other forwarded mutation domains and enrolled IAM/login admission,
Kubernetes TSIG/RFC 2136/integration/authority/lease work, F28 and G21–G24
deployment qualification, and final-source hosted CI/security/fixed-runner
performance. AD/Windows and independent-VM VIP acceptance remain owner-deferred
under D353/D358. No release is published or claimed.


### F60b/G11 typed follower forwarding of objects and zone commands — November 9, 2026 (D362)

An enrolled SQLite follower now forwards the public canonical object surface
(`POST`/`PUT`/`DELETE` under `/objects/` for zones, record sets, block lists,
client groups and policies) and the four zone convenience commands the
dashboard record editor uses (record upsert/delete, import, zone delete). Each
request starts its 30-second bound before body read and captures the original
source; the follower normalizes, admits the credential early under the existing
forward pins and sends one version-2 `objects.write`/`objects.delete`/
`zones.command`. The primary repeats normalization and runs the public API's own
command body (or route function and plan) inside the member transaction, with
exact old/new-scope or sealed-plan reauthorization of the raw credential. The
effect, its audit row and the dedup record commit together. Clean pre-mutation
client refusals (`400/404/409/412/413`) are retained and relayed exactly; a plan
whose Apply began and did not finish always rolls back; an exchange lost after
send answers `503 completion_unknown`. Nothing is retried or redirected, and the
receipt does not claim follower-local convergence.

Evidence under `.context/object-forward-20261109/`:

- Real two-process `TestEnrolledClusterProcessesPartitionCatchupAndRestart`
  (`process02`): pass in 70.8 s with forwarded create (201), stale If-Match
  relayed as 412, update, delete, zone-editor upsert, relayed missing-zone 404 and
  record delete, each converging in UDP/TCP DNS on both nodes; a write sent during
  partition returns 503 and is never applied or replayed after reconnection.
- Root member tests over actual member mTLS (`root-race06`):
  `TestRootMemberObjectCommandActualHTTP` (commit, exactly one attributed audit
  row, dedup replay, retained 412/409 refusals, forged subject, payload/operation
  mismatch, unsupported kind, invalid raw proof, revoked credential, raw token
  absent from the durable image) and `TestRootMemberZoneCommandActualHTTP`
  (upsert, retained 404, mismatched/unknown inputs, delete).
- API seam unit tests: repeated normalization/key refusals, refusal classification
  and invalid primary output.
- Full `internal/api/...` default race `api-race02`: exit 0, 646 passes, three
  environmental skips (two PostgreSQL, one envtest).

- Root affected default-race selection `root-race07` (member, forward, object, zone,
  cluster, blocking, promotion, join, enrollment, fencing, replica, follower, CLI
  reference, doctor, anchor): exit 0, 419 passes, one skip (`TestDoctorDatabase`, no
  PostgreSQL). E2E `process03`: both enrolled two-process scenarios, the
  three-process conflict scenario and the static-twin process scenario pass.

A first root test run exposed a harness omission (no ZoneCommand on its API server);
the domain correctly refused with `zone command forwarding unavailable`. Settings,
IAM/login admission, integrations and other domains are still primary-only.


### F46/F60b follower local login through shared primary admission — November 9, 2026 (D363)

Local password login on an enrolled SQLite follower now forwards one version-1
`auth.login` member command. The follower first applies its node-local request
budget, factor pacing and local-login setting, then generates the session ID and
CSRF secret and forwards only the CSRF hash, the normalized username, password,
factor code and client key, plus a random nonce. The primary runs
`security.IssueForwardedLocalWithAdmission`: the same shared IP/username counters,
password and factor verification, account/grant and local-login policy as its own
route, enrolling exactly the prepared session. Lockouts and credential refusals
are retained results whose counters commit; other failures roll back. The follower
explicitly pulls, then seals the cookie with its own secret once its complete view
contains the session. Neither the password, the CSRF secret nor a cookie is
persisted, and the dedup fingerprint includes the unstored nonce.

Evidence under `.context/object-forward-20261109/`: the real two-process partition
scenario (`login-process01`) passes in 62.8 s. A fresh client gets the primary's
exact 401 for a wrong password through the follower, then logs in there, and the
returned session authenticates `/auth/me` on both the follower and the primary.
`TestRootMemberLoginCommandActualHTTP` passes over actual member mTLS: committed
prepared session, password absent from the durable image, retained 401 refusal
with counters, and refused subject/session/version/operation mismatches. Full
`internal/auth/...` default race `auth-race01`: exit 0, 833 passes, four envtest
skips. Affected root default-race selection `root-race08` (login, session, password,
factor, OIDC and the forwarding/cluster set): exit 0, 410 passes, one opt-in
skip (installed-Chrome browser test). Scoped lint passes with two annotated,
intentional G117 password marshals (one production member payload, one fixture).

Settings, other IAM administration, OIDC/browser/device logins on followers and
remaining typed domains still require the primary.


### F35a multi-node lease loss and partition acceptance — November 9, 2026

`TestRunServeDirectDNSEndpointLeaseLossAndPartition` runs two mounted `runServe`
replicas against one envtest API server, each through its own proxy that can
partition it, including already established watch connections, without stopping
the process. Replica A acquires the actual `kudurru-controller` Lease and both
replicas serve the generated record. Partitioning A from the API server lets the
Lease expire; B acquires it with no test intervention and alone reconciles a
changed DNSEndpoint, while partitioned A keeps answering from its last valid data.
After the partition heals, A converges to B's result and never reclaims the Lease;
exactly one generated child exists throughout. Deleting the source withdraws the
record on both replicas and leaves no children.

Evidence under `.context/externaldns-failover-20261109/`: `race03` passes (26.1 s);
`race04` repeats both DNSEndpoint mounted tests three times with default race,
six passes, no failures/skips. Scoped lint passes. `race01`/`race02` are retained:
they failed because the harness's `offlineServeConfig` sets the process-global
KUBECONFIG, so building B's configuration before starting A routed both replicas
through B's proxy and A kept renewing. Each replica now starts before the next
configuration is built; no product code or assertion changed.

This closes the multi-node lease-loss/partition gap recorded for F35a. Source,
zone and child remain separate API objects (eventual controller, not a
cross-object transaction); final G09/G21/G22 gates remain.


### F44b durable Kubernetes grant revocation floors — November 9, 2026 (D364)

On the Kubernetes driver, sealed sessions carry the grants resolved at sign-in.
The binding source refused such a frozen grant after a binding change removed it,
but only through an in-memory revocation time on each replica: a restarted
replica, or one that never observed the removal, honored the earlier session
again once the binding was re-granted. Local `User` subjects were already covered
durably by `sessionsValidAfter`; group-based and OIDC frozen grants were not.

Each replica now persists the revocations it observes into the namespace Secret
`kudurru-grant-floors` (`kudurru.io/grant-floors`), as a commutative per-grant
maximum under resourceVersion CAS with bounded retry, and merges every replica's
floors into its binding source every five seconds and on each local revocation.
With durable floors configured, frozen grants are refused until the first merge;
live bindings and built-in authority still apply. Floors never decrease, and
malformed content refuses instead of resetting. A removal while no replica runs
remains unobservable and is documented.

Evidence under `.context/kube-grant-floors-20261109/`:
`TestBindingSourceDurableFloorsSurviveRestartAndRegrant` (fail-closed before
load, persisted floor, fresh-memory replica after regrant refuses the old session
and admits a later login, older merges cannot lower, legacy behavior unchanged);
envtest `TestGrantFloorsMergeAcrossReplicasAndRestart` (empty read, 40 concurrent
merges from two stores, monotonic floors, restart, malformed refusal). Default
race with envtest assets across `ee/rbac`, `internal/auth/stack` and
`internal/store/kube` (`race01`): exit 0, 271 passes, three opt-in measurement
skips. Mounted root Kubernetes selection with envtest (`root-race01`: Kube,
Kubernetes, DNSEndpoint, offline, RoleBinding, stack, cold start): exit 0, 100
passes, no skips. Scoped lint passes after a whitespace-only gofmt alignment of the
new files. The Kubernetes RoleBinding write API (create/replace/delete through
Kudurru) remains SQLite-only; on Kubernetes, RoleBindings are managed as CRs.


### F07a Kubernetes stored TSIG management and hot reload — November 9, 2026 (D365)

Kubernetes nodes previously loaded stored TSIG keys once at startup and answered
the `/tsigkeys` management routes as unavailable. After a successful startup
preparation, each node now rereads the complete stored source every ten seconds
and after its own commands. A changed source that prepares with the original
sealer and file baseline replaces the signing/verification ring in place and
wakes secondary managers; a changed source that does not prepare withdraws
stored and file signatures (fail closed, as at startup) until a later source
prepares; a failed read keeps the current ring.

Create, replace, reseal and revoke run on Kubernetes through `kubeTSIGLive`. Each
requires the node's published complete source cut to still be current, then
writes one object with its exact resourceVersion. Because the API server assigns
UIDs, create provisions a carrier and re-seals it bound to its assigned UID; a
failed bind deletes exactly that carrier once (`ErrProvisional`). A carrier that
cannot be opened is removable only by exact UID and resourceVersion. Receipts
report publication on the issuing node; other nodes adopt within ten seconds.

Evidence under `.context/kube-tsig-management-20261109/`: envtest
`TestRunServeKubeStoredTSIGManagementHotRotation` (`race04`, 18.2 s) creates a key
through the API, verifies signed TCP answers, refuses a stale binding with 409,
replaces it (the new key signs and the old key is refused), keeps it across a
restart, revokes it (refused), then adopts a key another writer created directly
without a restart. `TestApplyStoreTwoPhaseCreateCompensationAndUnusableRevoke`
covers bind failure compensation, a stale-source refusal and exact unusable-carrier
removal. Full `internal/tsigkeys` and `internal/dnsserver` default race
(`lib-race01`): exit 0, 643 passes, four opt-in measurement skips. Scoped lint
passes. `race01`/`race02` are retained: the first probe assumed BADKEY, while an
unknown key on this path answers NOTAUTH with TSIG BADSIG; both are refusals.
Root TSIG/secondary/NOTIFY/doctor/OpenAPI/CLI-reference selection with envtest
(`root-race01`): exit 0, 134 passes, one skip (`TestDoctorDatabase`, no
PostgreSQL). The route description changed, so OpenAPI, both generated clients
and the CLI reference were regenerated.


### F12a Kubernetes RFC 2136: open design decision — November 9, 2026

V2-UPDATE requires recovery "on both drivers". SQLite mounts the atomic UPDATE
handler and durable primary history; on Kubernetes the authoritative stage has no
UPDATE handler and answers NOTIMP. The Kubernetes zone-state backend
(`internal/store/kube/zonestate.go`) exists, but the primary lifecycle runtime and
its transactional adapter are SQLite-only, and Kubernetes serving builds zones from
DNSZone/DNSRecordSet objects rather than from committed zone heads.

The blocking question is atomicity. An RFC 2136 message evaluates prerequisites
over the whole zone and then applies every change or none. Kubernetes has no
multi-object transaction, and zone data spans independent API objects. Candidate
designs, none implemented in this pass:

1. One UPDATE-owned record object per zone, written by single-object
   resourceVersion CAS. Prerequisites are evaluated against a captured complete
   zone cut and the write is refused unless every other record object of the zone
   still has its captured resourceVersion. A concurrent API/file writer to another
   object between the check and the CAS is the residual race that needs a decision.
2. Mount the primary lifecycle runtime on the Kubernetes zone-state backend and
   serve committed heads, matching SQLite semantics; this changes the Kubernetes
   serving model and is substantially larger.
3. Explicitly not advertise RFC 2136 on Kubernetes in v0.2 (CRD/docs refusal of
   `policy.update` other than `deny` on that driver) and defer it.

On November 10 the owner asked for the standards-conforming option that meets the
goal; option 1 was implemented as D366 (recorded below).


### F12a RFC 2136 on Kubernetes — November 10, 2026 (D366)

Following the owner's direction to use the standards-conforming option that meets
the goal, Kubernetes now answers RFC 2136 UPDATE. RFC 2136 section 3.7 requires
atomicity with respect to concurrent UPDATE transactions; Kubernetes offers
single-object CAS but no multi-object transactions. Each zone therefore has one
UPDATE-owned DNSRecordSet, `rfc2136-<zone>` (manager `dns-update`), and every UPDATE
is one exact-resourceVersion write of that object.

Each UPDATE verifies TSIG, takes a per-zone node lock, reads the zone and every
record set referencing it from the store, builds the served zone with the serving
selection/builder, and runs the shared `dynamicupdate.Plan` with grants from
`currentguard.CompileUpdatePolicy` (now shared with SQLite) and reservations for
SOA, apex NS and every RRset another source owns. The UPDATE-owned part of the plan
is written by create-only, versioned update or versioned delete. A lost cross-node
CAS re-plans from fresh data up to eight times, then answers SERVFAIL without a
write. Prerequisite and policy refusals write nothing. The controller's status
serial advances and drives NOTIFY; API, file and integration sources keep their
own objects and cannot be changed by UPDATE.

Evidence under `.context/kube-update-20261110/`: envtest
`TestRunServeKubeDynamicUpdateAtomicityAndOwnership` passes four times (`race05`,
`race06` x3): signed add with a signed response and the `dns-update`-owned object,
serial advance, YXDOMAIN prerequisite failure without a write, refused ungranted
type, refused unsigned UPDATE, an API-owned record unchanged, 12 of 12 concurrent
UPDATEs on one node committed, 16 of 16 concurrent UPDATEs split across two nodes
acknowledged and present on both, restart durability, a granted name-wide delete,
and deletion of the emptied UPDATE-owned object. `TestKubeUpdateEndpointsRoundTrip`
covers A/AAAA/TXT (including quotes)/MX/SRV/CNAME round trips and reservation/SOA
exclusion. Default race with envtest assets across `internal/primaryruntime`,
`internal/dynamicupdate`, `internal/zone` and `internal/store/kube` (`lib-race01`):
exit 0, 876 passes, three opt-in measurement skips. Root selection (Update, Kube,
Kubernetes, Primary, TSIG, Notify; `root-race01`): exit 0, 173 passes, no skips.
Scoped lint passes. Retained earlier runs: `race01` (test used an invalid `ANY` RR
literal), `race02`/`race03` (a test assumption about name-wide deletes; the first
concurrency run committed 6 of 12 before the per-zone lock was added).


### v0.2 completion pass: what was merged at 68a709e2, and its verification state — November 11, 2026

This pass started on November 10 from `33ee04bc` (PR #54, hosted CI green on
that head). Eight work lanes (int, ha, f28, obs, qual, core, flake, docs)
implemented packages from read-only maps; each package was implemented, then
reviewed adversarially, then fixed, and, when the fixer committed anything,
verified by an independent agent. Usage credits ran out three times and the
machine restarted once during the pass, so several packages stopped between
those steps. The sections below record each package as it was merged, followed
by a paragraph that states its review and verification state at `68a709e2`.

**What was merged, and how.** A first batch (DOCS-ARCH, QUAL-CI, HA-FIX,
F28-FENCED, OBS-FANOUT, DOCS-OPS) was cherry-picked, regenerated
(`de0d249a`) and pushed on November 11; the integrator then widened one sink
shutdown bound (`bb6b73aa`). Later on November 11 **the owner had every lane
merged into PR 54 before verification was complete**, and then asked for what
is left to be validated. All eight lane heads were cherry-picked (qual
`2fad1dad`, core `02089768`, ha `17246f3a`, int `0fac45e0`, obs `d4e63712`, f28
`90740ec6`, flake `796c04e3`, docs `a10afdf1`), the contracts were regenerated
(`3fe4b160`), and the integrator fixed one composition defect: the new
integration release route had no follower class (`68a709e2`, which also makes
the fixture teardown test check its errors). `git log 33ee04bc..68a709e2` holds
208 commits.

**Verification state by package** (review findings / whether the fixes were
verified; details in each section):

| Package | Decision | Review | Fix | Independent verification of the fix |
|---|---|---|---|---|
| DOCS-ARCH | none | accept with fixes, 8 (4 minor, 4 nit) | fixed | never run |
| QUAL-CI | D377 | accept with fixes, 9 (2 major) | partial | 8 resolved, 0 unresolved, 2 new problems open |
| HA-FIX | D374–D376 | accept with fixes, 7 (2 major) | fixed | 2 resolved, nothing open |
| F28-FENCED | D369 | accept with fixes, 4 (2 major) | fixed | 2 resolved, nothing open |
| OBS-FANOUT | D371 | accept with fixes, 6 (5 minor, 1 nit) | fixed | post-verified: 4 resolved, 1 minor open, 2 nits open |
| DOCS-OPS | none | accept with fixes, 8 (3 major) | partial | 3 resolved, 3 new problems open |
| QUAL-SQLITE-UPGRADE | D373 | accept with fixes, 11 (6 blocker or major) | fixed | 4 of 6 resolved; 2 unresolved and 4 new, carried into QUAL-SQLITE-UPGRADE-2 |
| QUAL-SQLITE-UPGRADE-2 | D373 | **reject**, 5 (3 major) | partial | 4 resolved, 1 major unresolved, 2 new open |
| QUAL-KUBE-UPGRADE | D373 | accept with fixes, 6 | fixed | 4 resolved, 1 nit open |
| CORE-APPLY-MULTI | D378 (and the D375 correction) | accept with fixes, 8 (1 major) | partial | 6 resolved, 2 unresolved (1 major), 2 new open |
| HA-REFUSAL | D368 | accept with fixes, 8 (5 minor, 3 nit) | partial | **never run** |
| HA-AUTH-SETTINGS | D368 | accept with fixes, 5 (3 major) | fixed | 4 resolved, nothing open |
| INT-DEL-A | D367 | accept with fixes, 6 (2 major) | fixed | 2 resolved, 1 nit (changelog wording) |
| INT-DEL-B | D367 | accept with fixes, 7 (2 major) | fixed | 2 resolved, 5 new problems taken up by INT-DEL-C |
| INT-DEL-C | D367 | accept with fixes, 2 | fixed | 1 resolved, 2 new open (1 minor, 1 cosmetic) |
| OBS-HTTP-SINKS | D371 | accept with fixes, 5 minor | fixed | **never run** |
| OBS-DNSTAP-EMIT | D372 | accept with fixes, 4 minor | fixed | 4 resolved, 1 minor open |
| F28-HISTORY | D369 | accept with fixes, 3 (2 minor, 1 nit) | fixed, including a production change | not run in its lane; re-examined by F28-HISTORY-RECHECK |
| F28-GUARDS | D370 | accept with fixes, 8 (1 major) | partial | the major finding is unresolved |
| F28-HISTORY-RECHECK | D369 | **not reviewed** (the implementer stopped before returning a result) | none | none |
| FLAKE-OIDC-WRITER | none | **not reviewed** (the implementer stopped before returning a result) | none | none |
| DOCS-VERSION | none | accept with fixes, 6 | partial | 0 resolved, 1 major unresolved, 4 new open |

Packages that were planned and never started because the credits ran out:
CORE-RETAINED-SERVFAIL (narrow D377), CORE-STATUS-WRITE-SERVFAIL (the
SERVFAIL window after a primary zone's status write), F28-EF1-LIVE (the live
dnsdist follower test), F28-PROCESS, FLAKE-PORT-RACE,
FLAKE-PRIMARY-UNAVAILABLE and FLAKE-AUTH-COOKIE-AND-SUPERVISOR. Their maps are
under `.context/v020-final-20261110/`.

**Hosted CI.** Both pushed heads of this pass failed:

- `de0d249a`: CI run 42954844334 failed in `go test 1/2` while 20 other jobs,
  including e2e, `go test 2/2` and lint, passed, and nine other workflows
  passed (security 42954844450 among them). Attempt 1 failed
  `TestRunServeQuerylogFailedPrepareKeepsOldCompleteRoute` (`listen tcp
  127.0.0.1:42457: bind: address already in use`, the harness's free-port race)
  and `TestCLIOIDCBrowserActualTLSProofRefusals/nonce` (`browser writer ended
  context canceled`). Attempt 2 failed
  `TestServingHTTPClusterActivationIdentityRefusalAndRollback/invalid-member`
  (`primaryruntime: required adapter/publication unavailable`). FLAKE-OIDC-WRITER
  below attributes the writer failure to the fixtures and shows that it
  predates the batch; the port race and the activation failure are classes
  seen on earlier heads and were not fixed in this pass.
- `68a709e2`: CI run 43045349810 failed two jobs. `go test 1/2` failed
  `TestServingBlockingWriterCanceledAfterCommitBeforeObservation` (subtests
  `helper` and `blocking`, `serving_runtime_test.go:140: providerresources:
  owner closed`); `helm` failed `TestChartUpgradeFromV01`
  (`deploy/upgrade_test.go:94: git archive v0.1.0 (fetch the tag): exit status
  128`, because the hosted checkout has no tags). Logs:
  `evidence/INTEGRATE-2/hosted-68a709e2-130608116935.log` and
  `hosted-68a709e2-130608117068.log`.

The local pre-push run of the merged tree (`evidence/INTEGRATE-2/`) passed
gofmt, build, vet and drift, and failed lint (two errcheck findings in
`serving_fixture_teardown_test.go`), the library races
(`TestActualSQLiteSourceWaitsWithOriginalCancellationAndWriterToken/lease_cancel`
in `ee/integration/host`, `TestFollowerOperationsClassifyEveryMutation` in
`internal/api`) and the `cmd/kudurru` selection
(`TestRunServeSQLiteLongApplyKeepsServingDNSAndReads`,
`TestRunServeSQLiteUpgradeWithLegacyPlaceholders`,
`TestRootMemberFollowerRefusesUnforwardedMutations`). `68a709e2` fixes the
lint findings and the two follower classification failures; `lease_cancel`
passed three of three on rerun; the long-apply and upgrade tests passed alone
(`rerun-two.log`), but the long-apply run logged an 80-second gap without a
DNS answer and 41 of 43 failed reads during a 120-document apply. Full lint,
the library races and the `cmd/kudurru` package were not rerun after
`68a709e2`.

**Excluded checks.** On November 11, 2026 the owner excluded both Cloudflare
Workers Builds checks (`kudurru-docs` and `kudurru-www`) from this work. The
`kudurru-www` check first appeared, and failed with zero duration, on
`de0d249a`.

No gate is closed by this pass. G04 is red on both pushed heads; G21–G25 stay
open; the paired benchmark has not been run on the final head.


### DOCS-ARCH architecture pages in step with the code — November 10, 2026

Map entries G21-16 and the architecture part of DOC-1. No decision number: no
design decision was needed. Base commit `33ee04bc`; lane branch `wip/v020/docs`.
Seven source commits up to `0d565018`, then three source commits and one
`chore(gen)` commit that resolve the four findings of the independent review
(verdict "accept-with-fixes").

The architecture tour (`docs/architecture/`) described several v0.2 features
as unfinished after they had landed. Each such statement was checked against
the code at `33ee04bc` and replaced with what the code does. Statements about
work that other lanes own (integration `Delete`, Tier-B hooks, the Kubernetes
integration host, settings and sign-out forwarding, managed trust anchors,
query-log sinks) were left as written.

What the pages now say:

- **Index (`README.md`).** A v0.2.0 tour of ten pipeline stages. The tour table
  lists `integrations.md` and `central-stopped-source-rollups.md`. "Built and
  not built" no longer claims that no follower exists, that
  `/nodes/{other}/health` answers 501, that the blocking toggle is per node, or
  that dynamic UPDATE is in progress. A new table, "What depends on the store
  driver", records the complete-SQLite and Kubernetes behavior of RPZ feeds,
  the integration host, RFC 2136, stored TSIG keys, the blocking toggle,
  settings writes, TOTP, session inventory, RoleBinding writes, local-user and
  external-account administration, and enrollment/promotion, with the refusal
  key where Kubernetes answers 503. The loop diagram shows the transfer
  managers, the SQLite serving writer and the DNSEndpoint source, and the text
  explains that the SQLite election follows durable write authority. The
  objects loader appears in both groups of that diagram: every replica runs it
  on Kubernetes, and on SQLite it is a leader-only loop, so an enrolled
  follower never applies its local `objects:` and `objectsDir` files (review
  finding 3).
- **RPZ (`block-lists.md`, `policies-and-blocking.md`, `query-path.md`).** The
  adapter is mounted on the complete SQLite runtime (manager, complete-candidate
  composition, per-request selector, `blocked_rpz`), with a new diagram. The
  pages state that Kubernetes neither transfers nor evaluates RPZ sources and
  that per-feed transfer status has no API, CLI, metric or dashboard surface.
  The status table lists `local_record`, `blocked_upstream` and `blocked_rpz`
  as produced; only `retried` and `in_progress` remain reserved.
- **Client identity (`policies-and-blocking.md`).** The MAC, hostname and
  client-ID tiers are no longer described as unfed; policy `rewrites` are
  served by the `rewrite` stage. `queryLog` on a policy is still read by
  nothing, and the page still says so.
- **Enrolled SQLite membership (`configuration-and-store.md`,
  `deployment.md`).** Activation, invitation, join, follower acquisition,
  reviewed promotion, peer authority observation, offline conflict resolution
  and typed follower forwarding (blocking toggle, object writes, four zone
  commands, local login) are described as mounted. The sentence "Settings,
  other IAM administration and remaining typed root domains are unfinished" is
  kept. Static twins are described as read-only through the API, with a
  diagram comparing them with an enrolled cluster. The deployment table has an
  enrolled-cluster row. The driver comparison table no longer gives the SQLite
  leader as "always this node": it is the node with durable write authority (a
  standalone node, each static twin, or the enrolled primary), and "Used by"
  lists enrolled primaries and followers (review finding 1). The file loader
  paragraph says where the loader runs on each store.
- **Access (`access.md`).** Kubernetes binding revocation floors are durable
  (D364). New section "Local sign-in on an enrolled follower (D363)" with a
  sequence diagram. External sessions: re-evaluated on every request from
  stored login evidence without contacting the identity provider (D212).
- **Zones and TSIG (`zones.md`, `query-path.md`,
  `configuration-and-store.md`).** Kubernetes stored-key management and the
  ten-second reload replace "fixed until restart" and "unavailable". A native
  UPDATE is not forwarded between enrolled SQLite members; Kubernetes serves
  UPDATE through its single-object path.
- **Other.** `integrations.md`: DNSEndpoint partition and lease loss is covered
  by `TestRunServeDirectDNSEndpointLeaseLossAndPartition`.
  `upstreams-and-cache.md`: unblock-request mutations are not "unconverted".
- **Hand-drawn diagrams (`diagrams/*.excalidraw.svg`, review finding 2).**
  Three diagrams carried labels that the rewritten pages contradict. Each label
  was changed twice, in the Excalidraw scene embedded in the SVG and in the
  drawn SVG text, so that a later re-export from the scene keeps the change.
  `overview`: the pipeline box says "seven of ten stages" and names
  postprocess, rewrite and dnssec as not drawn (the seven boxes were not
  redrawn as ten); the SQLite store box reads "Docker / systemd: state.db on
  each node (sqlite driver, leader = node with write authority)" instead of
  "the node is always leader"; the loader arrow reads "objects loader (kube:
  every replica, sqlite: the leader)" instead of "in every node".
  `config-flow`: "leader: the node with write authority" instead of "leader:
  always this node". `policy-decision`: the footnote of the MAC, hostname and
  client-ID tiers reads "only when a source supplies that identity" instead of
  "no transport fills these in yet". In `overview`, one label that an earlier
  hand edit had left 0.23 px off its scene position was moved back so that the
  scene and the drawn text agree exactly in all eight diagrams.

Rendering defects found and fixed while checking diagrams: at `33ee04bc`,
eleven sequence diagrams of the tour did not parse with the Mermaid 11.17.2
bundle the docs site ships. Ten had a semicolon in message or note text (a
semicolon ends a sequence-diagram statement); the eleventh had a closing fence
followed by text on the same line (`configuration-and-store.md`), so the code
block continued and swallowed the following paragraphs. Semicolons in
sequence-diagram text are now written `#59;`, the fence is closed, and the new
diagrams avoid semicolons.

Source strings and comments corrected with the pages:

- Dashboard `MatchersField.tsx` and `GroupDetailPage.tsx`: MAC, host name and
  name pattern matchers no longer say "inventory integrations arrive in v0.2"
  or "this version has none, so it never matches"; they name the sources each
  matcher needs. `policyFields.test.tsx` asserts the new text.
- `internal/config/schema.go`: the `cliClientID` help no longer says
  "(kudurru login, v0.2)"; the dashboard mock schema follows.
- Zone groups (review finding 4): the `zonegroup` scope help of
  `rbac.bindings` no longer says "reserved: zone groups arrive in v0.2". It
  says that zone groups are not implemented and that a binding with this scope
  grants nothing; the dashboard mock schema follows, and
  `TestReservedBindingScopeHelp` (`internal/config`) fails on the old text. The
  same promise had two more sites that the review's search for the exact
  phrase did not find: the `spec.scope.kind` field comment of RoleBinding
  ("zonegroup (v0.2)"), which is the CRD field description and a row of the
  generated CRD reference, and the scope constants of `internal/rbac`. Both now
  say reserved. The CRD copies and the reference page are regenerated in the
  `chore(gen)` commit. No behavior changes.
- Go comments (no behavior change): zone type constants and `zone.NewBuilder`,
  `internal/notifier` (inbound NOTIFY), `internal/store` (drivers and elector),
  the transfer counter, and the TOTP seed field of the internal kinds.

Code paths behind the main corrected statements:

| Statement | Code |
|---|---|
| RPZ mounted on SQLite only | `cmd/kudurru/serve.go` (`feeds` created only when `sql != nil`; `add("rpz", false, feeds.Run)` in the complete branch); `internal/servingbuild/dns.go` (`SetRPZCapture`); `internal/stages/blocking/blocking.go` (`s.opts.RPZ != nil`), `rpz.go` (`StatusBlockedRPZ`) |
| RPZ feed status has no surface | `internal/rpz/manager.go` `Statuses`/`StatusFor` have no caller outside the package |
| Shared edge cache and RPZ | `internal/servingbuild/edge_cache.go` (`cacheTimeline.at`), `internal/rpz/read.go` (`CacheUntil`) |
| `local_record`, `blocked_upstream` produced | `internal/responsepolicy/rewrites.go`, `internal/stages/blocking/rewrite.go`, `internal/dhcpdns/resolver.go`; `internal/responsepolicy/response.go` (`Guard`), `internal/stages/postprocess/postprocess.go` |
| Identity sources | `internal/stages/identify/identify.go`; `cmd/kudurru/serve.go` (`idStage.Hostnames = leaseDNS`, `mountReversePTR`); `internal/dnsserver/dns.go`, `doq.go`, `doh.go`, `handler.go` (client IDs); `internal/policy/index.go` `IdentifyAt` (tier order) |
| Edge does not render the client-ID TLV | `internal/dnsserver/proxyproto/header.go` (`TLVTypeClientID`) is referenced only by `proxyproto/policy.go` |
| SQLite election follows authority | `internal/store/sqlite/sqlite.go` (`Elector`), `authority.go` (`runDynamic`), `internal/store/replication/types.go` (`Authority.Writable`) |
| Peer health read over `:8082` | `internal/api/nodes.go` `nodeHealth` |
| Blocking toggle replicated | `cmd/kudurru/serving_blocking_commands.go`, `serving_member_forward.go`, `kube_blocking.go`, `internal/store/kube/blocking.go`; `internal/api/static.go` (`blockingScope`) |
| Static twins read-only | `internal/api/static.go` (`staticObjectMutation`, `staticWriteMW`) |
| Member commands a follower forwards | `cmd/kudurru/serving_member_command.go` (operation switch), `serving_member_objects.go`, `serving_member_zones.go`, `serving_member_login.go`, `serving_member_forward.go` |
| Production paths use `NewMemberClient` | `internal/cluster/replica/enrolled_source.go`, `enrolled_forward.go`, `join_client.go`, `member_authority.go` |
| Root assembly of the member server | `cmd/kudurru/serving_member_listener.go` `prepareMemberServices` |
| Partition, promotion, rejoin, conflict acceptance | `test/e2e/enrolled_cluster_test.go`, `test/e2e/enrolled_conflict_test.go` |
| Follower login | `cmd/kudurru/serving_member_login.go` (`forwardLogin`, `rootMemberLoginDomain`), `serving_http_sessions.go` (`sealEnrollmentWithin`) |
| OIDC start/callback/CLI prepare need the local writer | `cmd/kudurru/serving_http_request_limit.go` |
| External session re-evaluation | `internal/servingauth/external_cookie.go` `externalSessionPrincipal` |
| Kubernetes grant floors | `docs/architecture/access.md` "Authorization" (D364) |
| Expression policies: captured provider compiles, legacy adapter refuses | `internal/auth/oidccore/cel.go`, `internal/auth/providerregistry/plan.go`; `ee/oidc/fromconfig.go` |
| Kubernetes TSIG management and reload | `cmd/kudurru/tsig_kube_management.go` (`kubeTSIGLive`), `tsig_kube_startup.go`, `doctor_notify_kube.go` |
| UPDATE proof only on a writable node | `cmd/kudurru/primary_proof.go` `capture`; `primary_proof_test.go` |
| Kubernetes NOTIFY from the elected loop | `cmd/kudurru/serve.go` (`add("notifier", true, ...)` when there is no complete runtime) |
| Zone types served and unsupported | `internal/zoneview/zoneview.go`, `internal/zone/zone.go` `NewBuilder`, `internal/controller/zone.go` `zoneVerdict` |
| Per-driver refusals | `cmd/kudurru/serving_totp_legacy.go`, `serving_users_legacy.go`; `internal/auth/stack/routes.go`; `internal/api/rolebindings.go`, `integrations.go`, `cluster_enrollment.go`, `cluster_promotion.go`; `cmd/kudurru/reload.go` `settingsWriter` |
| Unblock decisions flush the name | `internal/api/unblock.go` (`flushName`); `cmd/kudurru/serving_routes.go` |
| DNSEndpoint lease loss | `cmd/kudurru/externaldns_crd_failover_test.go` |
| Objects loader: every replica on Kubernetes, leader-only on SQLite | `cmd/kudurru/serve.go` (`{"objects", sql != nil, ...}`: the second field is `leaderOnly`; the `objects` health check reports a follower ready without loading) |
| Ten stages, of which the overview draws seven | `cmd/kudurru/serve.go` (the `stages` slice passed to `pipeline.New`: ingress, identify, control, postprocess, authoritative, blocking, rewrite, cache, dnssec, upstream); `docs/architecture/query-path.md` (first diagram) |
| A zonegroup or clientgroup binding grants nothing | `internal/rbac/rbac.go` (`Scope.Covers` needs the same kind and name); every `rbac.Scope{Kind: ...}` a route builds is `ScopeCluster` or `ScopeZone` |

Evidence under `.context/v020-final-20261110/evidence/DOCS-ARCH/`:

- `mermaid01-baseline.log`: the Mermaid blocks of `docs/architecture/*.md` at
  `33ee04bc`, parsed with the vendored Mermaid 11.17.2 bundle
  (`docs-site/vendor/mermaid`) in jsdom by `mermaid-check.cjs` (retained next
  to the logs): 104 blocks, 11 failures (retained failed run; three in
  `access.md`, six in `configuration-and-store.md`, one each in
  `policies-and-blocking.md` and `zones.md`).
- `mermaid02.log`, `mermaid03.log`, `mermaid04-head.log`: the same check after
  the edits; the last one on lane head `0d565018`: 108 blocks, 0 failures.
- `links01.log` to `links03-head.log` (`anchors.py`): 118 relative links of the
  architecture pages, every target file and heading anchor present, 0 broken.
- `docs-site01.log` and `docs-site02.log` (head `0d565018`): `make docs-site`
  with the pinned Hugo 0.167.0 and `--panicOnWarning`, exit 0, 163 pages;
  `docs-links01.log` and `docs-site02.log`: `make docs-check-links` (htmltest
  0.17.0), 160 documents, passed. Neither target validates Mermaid syntax,
  which is why the parse check above was added.
- `build01.log`: `gofmt -l` on the touched Go files is empty, `go build ./...`
  and `go vet` on the touched packages exit 0.
- `race01.log`: `go test -race -count=1` on the touched Go packages:
  `internal/zone` 3.5 s, `internal/notifier` 2.2 s,
  `internal/stages/authoritative` 1.5 s, `internal/config` 11.1 s, all ok;
  `api/v1alpha1`, `api/internalkinds/v1alpha1` and `internal/store` have no
  test files. No skips.
- `dashboard01.log`: vitest on `policyFields.test.tsx` and
  `GroupClientPages.test.tsx`, 2 files, 24 tests passed, 20.5 s.
  `dashboard02-negative.log` (retained failed run, intended): the updated
  `policyFields.test.tsx` against `MatchersField.tsx` from `33ee04bc` fails 1
  test, so the assertion follows the help text.
- `gen01.log` on the seven source commits up to `0d565018`: `make docs-gen`
  and `make generate` exit 0 and leave the tree unchanged (the one
  `chore(gen)` commit of this package comes from the review fixes below);
  `TestReferenceDrift`, `TestCLIReferenceDrift`, `TestRoutePermissionLint`,
  `TestLintRoutesCatchesMissingDeclaration`, `TestOpenAPIDrift`,
  `TestOpenAPIServedAndDocs` and `TestAggregatedRolesDrift` pass;
  golangci-lint v2.15.0 on the touched packages reports 0 issues; dashboard
  `tsc --noEmit` and eslint on the changed files exit 0.
- `drift01-before-regen.log`: `TestReferenceDrift` already passed before
  regeneration, because the configuration reference lists the nested fields of
  `auth.identityProviders` without their help text.
- `review-*.log`: the independent reviewer's runs on `0d565018` (verdict
  "accept-with-fixes", four minor findings). `review-render01-head.log` is a
  render of the built site in headless Chrome 156: 108 Mermaid blocks drawn as
  SVG, 0 error graphics.

Evidence for the review fixes (lane head `51878320`, same directory; the
scripts `fix-stale-check.py`, `fix-exsvg.py` and `fix-heavy.sh` and the label
lists `fix-edits-*.json` are retained next to the logs):

- `fix-stale01-before.log` (retained failed run, expected): `fix-stale-check.py`
  on `0d565018`, 25 checks, 19 failed. It reads the driver table, the loop
  diagram of the index, the labels of the three diagrams (embedded scene and
  drawn text), the stage list of `query-path.md` against the overview's stage
  boxes, and the zone-group strings in the source and generated files. All four
  findings reproduce. `fix-stale02-head.log`: the same check on `51878320`, 25
  checks, 0 failed.
- `fix-diagrams01-before.log` (retained failed run): `fix-exsvg.py check` on
  the eight hand-drawn diagrams at `0d565018`. It rebuilds, from every text
  element of the embedded scene, the SVG group that Excalidraw exports and
  requires exactly that group in the file. Seven diagrams matched byte for
  byte; `overview` had one label 0.23 px off its scene position (left by the
  hand edit of October 16).
- `fix-diagrams02-apply.log`: the relabeling. The first attempt was refused
  three times, by design: the widths stored in the scenes are those of a
  browser whose web fonts had not loaded, so the tool now proves its
  measurement against either width before it trusts it, and "H" and "T" are
  not in the font subset embedded in `policy-decision`, so the footnote was
  reworded without "DHCP" and "PTR". The second attempt applied six labels;
  each new label is measured in headless Chrome with the embedded font and
  fits its container.
- `fix-diagrams03-head.log`: `fix-exsvg.py check` on `51878320`: eight
  diagrams, 234 labels, all drawn exactly as their scenes say; `xmllint` exit
  0; Chrome renders the three edited diagrams. `fix-render-*-before.png` and
  `fix-render-*-after.png` are those renders. They were inspected by eye:
  every new label is in the hand-drawn font, inside its box, without overlap.
- `fix-diagrams04-negative.log` (retained failed runs, intended): a copy with
  one drawn label changed and its scene left alone fails the check, and a
  label that needs "H" and "T" is refused without changing the file.
- `fix-race01-before.log` (retained failed run, expected):
  `TestReservedBindingScopeHelp` against the unchanged `schema.go` fails on
  "help promises the scope for v0.2". `fix-race02.log`: after the fix the test
  passes, and `go test -p 2 -race -count=1` passes for `internal/config`
  (13.2 s) and `internal/rbac` (1.0 s); `api/v1alpha1` has no test files.
  `fix-race03-head.log` on `51878320`: `internal/store/validator` (7.0 s) and
  `internal/config/crdschema` (1.1 s), which read the regenerated CRD, pass
  under the race detector. No skips.
- `fix-build01.log`: `gofmt -l` on the touched Go files is empty,
  `go build ./...` and `go vet` on the touched packages exit 0.
- `fix-gen01.log` (one run under the machine-wide lock): `make generate` and
  `make docs-gen` change exactly four files (three copies of the RoleBinding
  CRD and `docs-site/content/reference/crds/rolebinding.md`), which form the
  `chore(gen)` commit. `TestReferenceDrift`, `TestCLIReferenceDrift`,
  `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestLintRoutesCatchesMissingDeclaration`, `TestOpenAPIServedAndDocs` and
  `TestAggregatedRolesDrift` pass; `go test ./deploy/` passes (15.8 s);
  golangci-lint v2.15.0 on `api/v1alpha1`, `internal/rbac` and
  `internal/config` reports 0 issues; `make docs-site` exits 0 with 163 pages;
  `make docs-check-links` passes 160 documents. `fix-race03-head.log` repeats
  the reference, OpenAPI and aggregated-role drift tests on the committed head
  with a clean tree.
- `fix-mermaid02-head.log`: 108 blocks, 0 failures with the vendored Mermaid
  11.17.2. `fix-links02-head.log`: 119 relative links, 0 broken (one more than
  before: the driver table links to "The leader").
- `fix-render01-head.log`: the reviewer's render script on the site built in
  `fix-gen01.log`, headless Chrome 156: 108 Mermaid blocks drawn as SVG, 0
  error graphics, and the built site serves the three relabeled diagrams
  unchanged.
- `fix-dashboard01.log` (a copy outside the worktree, two workers):
  `SettingsPage.test.tsx` and `AccessPage.test.tsx`, which read the settings
  schema mock, 2 files, 29 tests passed, 13.3 s; `tsc --noEmit` exits 0.

What remains open:

- This was not a sentence-by-sentence audit of all 9,200 lines of the tour. It
  covered the lines named in G21-16 and DOC-1 and every statement found by
  searching the pages for "in progress", "unfinished", "remain open/separate/
  pending", "not yet", "not wired", "planned" and version markers. `dhcp.md`,
  `licensing.md`, `observability.md`, `central-stopped-source-rollups.md` and
  the managed trust-anchor parts of `upstreams-and-cache.md`, `query-path.md`
  and `deployment.md` were searched but not rewritten.
- Statements owned by other lanes were left in place and need those lanes'
  updates when their work lands: `integrations.md` (Kubernetes execution,
  `Delete` cleanup, remaining providers, dashboard controls),
  `configuration-and-store.md` ("Settings, other IAM administration and
  remaining typed root domains are unfinished"), managed trust anchors, and
  query-log sinks. The new store-driver table in `README.md` states today's
  Kubernetes position for RPZ feeds, the integration host, TOTP, sessions,
  RoleBinding writes and user administration; each row must change with the
  package that changes it. `zones.md` now says that a native UPDATE is not
  forwarded from an enrolled follower (UPD-1/G21-04).
- Strings and comments that still promise v0.2 for features that have not
  landed were not changed, as directed: the `impersonate` mode help and errors
  (`internal/config/schema.go` "impersonate is planned for v0.2",
  `internal/config/auth.go`, `internal/auth/stack/stack.go`, IAM-1), the edge
  error "--backends-from cluster needs follower or raft membership (v0.2)"
  (`internal/cluster/source.go`), record views (`internal/zone/zone.go`,
  INT-6), `ExternalSpec` (INT-4) and the IXFR journal comments. They will be
  false in a v0.2.0 binary unless the owning package lands or rewords them.
  The zone-group strings are no longer in this list: they say "reserved" now.
  If IAM-2 lands zone groups, it must replace that wording in
  `internal/config/schema.go`, the dashboard mock schema,
  `api/v1alpha1/iam_types.go` and `internal/rbac/rbac.go`, and
  `TestReservedBindingScopeHelp` must then stop covering `zonegroup`.
- Hand-drawn diagrams. The overview still draws seven stage boxes; it now says
  so and names the three it leaves out, but it was not redrawn with ten.
  `diagrams/query-pipeline.excalidraw.svg` also shows seven stages; no page
  embeds it (`query-path.md` uses a Mermaid chain), so it was left alone. The
  four other diagrams (`deploy-docker`, `deploy-kubernetes`,
  `deploy-compose-ha`, `upstreams`) were read label by label for statements
  the pages contradict and none was found ("node always leader" on the
  single-container panel and "its own leader" on a static twin are true), but
  they were not checked against the code beyond that. No diagram shows an
  enrolled SQLite cluster, integrations or DHCP. The edited scenes were not
  opened in Excalidraw itself (it is not installed here and nothing was
  downloaded): they are verified by decoding, by the byte-exact scene-to-SVG
  check and by rendering the SVG in Chrome. The stored text boxes of most
  labels remain narrower than the drawn text, as before; the six edited labels
  store their drawn width.
- The loop diagram of the index lists "settings reload" under "every node".
  `serve.go` adds the `settings` loop only when there is no complete SQLite
  runtime; what reloads settings on the complete runtime was not traced in
  this package, so the entry was left as it is.
- Mermaid is rendered in a browser only by hand: the reviewer and the fix pass
  rendered the built site in headless Chrome 156 (`/usr/bin/google-chrome`;
  the earlier statement that no Chromium is installed was wrong). The parse
  check, the render script, the stale-statement check and the diagram check are
  scripts in the evidence directory, not repository tests or CI steps.
- The tracker table and root `README.md` parts of DOC-1 belong to the
  integrator and were not touched. `docs/enterprise.md` still says that
  "UniFi inventory integrations are planned"; that page is outside this
  package.

**Review and verification state at 68a709e2.** The review accepted the package with eight findings (four minor: the driver table still called the SQLite leader "always this node", hand-drawn diagrams contradicting the new text, the loop diagram placing the objects loader on every node, and the `zonegroup` help still promising v0.2; four nits). The fixer reports all of them fixed in four commits and rebutted two contract notes (the reserved ledger files, and the generated-artifact note). No independent verification of the fix commits ran. Integrated in the first batch (`de0d249a`).


### G22-WP6/G23-R3/G23-R4/G24-SYNCTEST hosted qualification gaps and prebuilt-artifact switches — November 10, 2026 (D377)

Lane commits on `wip/v020/qual`, base `33ee04bc`. First pass: `3b0cb519`,
`a2726146`, `f788e7b6`, `b8ca0208`, `00c41d60`, `b162b1aa`. Review-fix pass on
top: `49aae9ee` (`fix(dns)`, the one product change), `06f921b9` (`test(e2e)`),
`52aee771` (`ci(e2e)`), `2ac0d76f` (`ci(helm)`), `bac6e7ab` (`test(kind)`) and
`0048c606` (`docs(architecture)`, line breaks of one paragraph only; the
`fix-*` logs of the final runs name `bac6e7ab`, `fix-unit03-wider.log` names
`0048c606`). No generated file, no new dependency, `go.mod` unchanged. Nothing
below ran on GitHub Actions: this sandbox cannot run workflows, so the hosted
proof is the first run of the ci workflow with these commits.

What now happens:

- **The hosted e2e job has every program its tests can skip without, and fails
  instead of skipping.** It gains the PostgreSQL 17 service and the envtest cache
  of the go job, runs the new `make interop-internal` after `make e2e`, and sets
  `KUDURRU_REQUIRE_BIND`, `KUDURRU_REQUIRE_POSTGRES` and `KUDURRU_REQUIRE_ENVTEST`.
  The first and the last are new (the `KUDURRU_REQUIRE_POSTGRES` rule of D158):
  `kudurrutest.Named` and `kudurrutest.BINDTool` are the one lookup for `named`
  and the dnssec tools in `test/interop`, `internal/rpz`,
  `internal/stages/authoritative` and `internal/zoneview`. Eight tests that ran in
  no hosted job now run there: `TestBINDRPZRuntimeTransfersRefreshRestart`,
  `TestBINDRPZFallbackAndAuthentication`, `TestBINDRPZPolicyReference`,
  `TestBINDSignedSecondaryThroughListeners`,
  `TestBINDStubThroughListenersAndDurableOfflineReferral`,
  `TestCentralAuditTwoNodes`, `TestCentralQuerylogTwoNodes` and
  `TestKubeAuditAcrossReplicas`. `TestKnotCatalogConsumer` is the only test of the
  job that still skips (opt-in, private Docker daemon; G23-R5).
- **The internal BIND step no longer depends on `make e2e` passing** (review
  fix). Its condition is
  `!cancelled() && (success() || steps.e2e.conclusion == 'failure')`: it runs
  when everything before it succeeded or when the end-to-end step itself
  failed, not after a failed setup step or a cancellation. One failing
  end-to-end test therefore no longer removes the five G23-R3 result lines from
  that run's log. The three later steps of the job (recursor against BIND, dex,
  AdGuard Home) keep their default condition, as before this package.
- **A census of the hosted log must read subtest lines.** With `named` hidden and
  no require variable, `TestBINDRPZRuntimeTransfersRefreshRestart`,
  `TestBINDSignedSecondaryThroughListeners` and
  `TestBINDStubThroughListenersAndDurableOfflineReferral` report `--- PASS` while
  every one of their subtests reports `--- SKIP`. With `KUDURRU_REQUIRE_BIND=1`
  the same run fails all five tests.
- **A node answers an upstream failure across a configuration commit** (review
  fix, the one product change; decision row in `out/QUAL-CI/decision.md`). Run
  with PostgreSQL for the first time, `TestCentralQuerylogTwoNodes` showed that
  a query whose answer is an upstream-failure SERVFAIL got no answer at all when
  it arrived right after a canonical commit (an API token created through the
  admin socket): the query ran on the retired data generation, and the output
  guard of D332 kept forwarded and policy-blocked replies, the empty unsynced
  SERVFAIL and ingress identity, but not the failure of the forwarding path
  (`write failed: serving: dependency changed`). The client waited for its own
  timeout. `retainedDNSResponse` now also keeps that refusal when it is empty:
  status `servfail_upstream` or `timeout_upstream`, response code SERVFAIL, AA
  clear, no answer or authority records, nothing but OPT in the additional
  section, origin unattributed or failure. Every other check of the retained
  path is unchanged. The first pass had moved the test to a fake resolver
  instead; the test forwards to the discard port again, as at `33ee04bc`, and
  additionally requires the SERVFAIL response code.
- **The three un-skipped end-to-end tests, as they now stand.** All three failed
  at `33ee04bc` when run with PostgreSQL and envtest:
  - `TestCentralAuditTwoNodes` looked for `blocking.toggle` under
    `GET /audit?kind=blocking`. That expectation was stale, not a product
    defect: on SQLite the blocking command is one write of `Kudurru/default`
    `status.blocking` audited in its own transaction as `kudurru.status`, and
    `Options.AuditBlockingCommand` (`internal/api/api.go:339`, set only for
    the Kubernetes driver at `cmd/kudurru/serve.go:1352`) records
    `blocking.toggle` only where the driver cannot audit in its transaction.
    The test reads the `kudurru.status` rows. Review fix: enabling is exercised
    again. node-a pauses for 30 seconds, node-b pauses without a timer and then
    enables blocking, and both nodes must show node-a's pause, node-b's pause
    and node-b's enable (`status.blocking.disabled` true to false) from the
    central database.
  - `TestCentralQuerylogTwoNodes`: see the product change above. Its original
    assertion is restored and strengthened.
  - `TestKubeAuditAcrossReplicas` never provisioned AuditLog trust. This guard
    was also stale: F73 makes that trust operator-provisioned ("explicit upgrade
    provisioning required", `completion-plan.md:206`; `docs/deploy/runbooks.md:291-294`;
    `hack/kind-e2e.sh` does the same), and every replica answered
    `audit keys: trusted verification material unavailable`
    (`internal/audit/auditkeys/ring.go:33`). The test runs
    `kudurru audit keys init` for the envtest cluster and creates the
    `kudurru-audit-keys` Secret. No assertion of this test changed.
- **The harnesses run on prebuilt artifacts and name them.**
  `KUDURRU_E2E_LICENSED_BINARY` names a prebuilt `-tags licensetest` binary for
  the six licensed tests, which previously rebuilt it in every test and ignored
  `KUDURRU_E2E_BINARY`; a binary whose build information lacks the tag is refused
  before any node starts. Without the variable the binary is built once per run.
  `kudurrutest.Describe` logs, for every binary, the `kudurru version` output, the
  SHA-256 of the file, and the build tags, cgo setting and commit recorded by the
  Go toolchain; `MainBuild` prints it per package and each licensed test logs it.
  `hack/kind-e2e.sh` honors `BUILD=0 IMAGE=<repository:tag>`; it and
  `hack/compose-e2e.sh` print `kudurru version` and the image ID of the image
  under test.
- **`hack/kind-e2e.sh` refuses an unusable image before it creates a cluster**
  (review fix). `IMAGE` has to be `repository:tag` in both modes, because the
  chart receives the two halves separately: a digest reference or a reference
  without a tag exits 2 before anything runs. With `BUILD=0` an image that the
  local daemon does not have exits 2 instead of being pulled by `docker run`.
  In both modes the script runs `license verify -` in the image with the test
  license key and exits 2 when the image does not accept it, which is what a
  build without `-tags licensetest` does. A run that stops before
  `kind create cluster` no longer prints cluster diagnostics (they would
  describe whatever cluster the current context names) or deletes a cluster.
- **`make test` hands the installed envtest binaries to every Kubernetes suite**
  (not in the map; found while adding the envtest step). The go job installed
  them into `bin/k8s`, but `internal/api`, `internal/auth/k8s`, `internal/cluster`,
  `internal/controller` and `internal/zonecache` read only `KUBEBUILDER_ASSETS`,
  which nothing exported. `TestLeaseHandoff`, `TestKubeColdStart`,
  `TestKubeDriver`, `TestDeleteZoneKube`, `TestServiceAccountTokens`,
  `TestSubjectAccessReview`, `TestStackEndToEnd` and `TestIdentityOnKubeStore`
  have therefore skipped in every hosted run. The recipe now exports the newest
  `bin/k8s/<version>-<os>-<arch>` (a value from the environment wins) and, with
  `KUDURRU_REQUIRE_ENVTEST=1` (set in the go job), stops before the run when
  there is no kube-apiserver.
- **The publish workflow installs envtest before `make test`** (not in the map).
  `internal/primaryruntime`, new in v0.2, fails without the binaries, so the test
  job in front of publishing would have failed on the first v0.2 tag.
- **G24-SYNCTEST closes no gap; its premise was false** (corrected after
  review). `hack/test_sync_version.py` already ran in every hosted go job and in
  the test job of `publish.yml`: `deploy.TestVersionSyncScript`
  (`deploy/release_test.go:340-346`) executes it and fails when a Python test
  fails, and the hosted go log of run 42736964083 has
  `ok github.com/robertherbaugh/kudurru/deploy`. The map's grep missed the Go
  caller, and commit `00c41d60` repeated the claim ("which no workflow ran").
  The step this package added to the helm job stays as a second, visible run of
  the same five tests (the go job has no `-v`, so the first run leaves no line of
  its own); commit `2ac0d76f` corrects its comment, and the G24-SYNCTEST entry of
  `map-g24-g25-release-docs.json` has a `correction` field. The tracker should
  say "already covered by `TestVersionSyncScript`; a visible duplicate step was
  added", not "now runs in CI".

Evidence under `.context/v020-final-20261110/evidence/QUAL-CI/` (BIND 9.18.50,
PostgreSQL 17.11 started natively on a private port, envtest 1.37.0, Go 1.27.3;
hosted uses BIND 9.18.39 and the `postgres:17-alpine` digest of the go job).
Logs named `fix-*` are from the review-fix pass, `review-*` from the reviewer,
the others from the first pass.

Review-fix pass:

- Upstream failure across a commit, before and after. `fix-e2e01-before.log`: a
  binary built with the product file of `b162b1aa` (through `go build -overlay`)
  fails both tests that need the change. `TestCentralQuerylogTwoNodes` in its
  restored form: `query node-b: ... i/o timeout` (9.93 s), the failure of
  `e2e01`. `TestUpstreamFailureAnsweredAcrossCommits`: `commit 1 of 5, query 16:
  no answer: ... i/o timeout` (5.50 s). `fix-e2e02-after.log`: the fixed binary,
  both tests with `TestCentralAuditTwoNodes` and `TestKubeAuditAcrossReplicas`,
  twice: eight passes, no skip, 110.1 s; the regression test logged 967 and 964
  queries answered SERVFAIL across five commits. `fix-e2e02b-precommit.log`:
  the three changed tests after their last edits, three passes, 31.4 s.
- The output guard. `fix-unit01-before.log`: with the product file of
  `b162b1aa` overlaid, `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation`
  fails in exactly the five new subtests that must be answered
  (`upstream_failure`, `upstream_timeout`, `failed_target`,
  `actual_upstream_failure_empty` and the same with EDNS); the other 25
  subtests pass, among them the 13 new ones that must stay refused (an
  upstream-failure status with a success code, with an authoritative, local,
  mixed or synthesized origin, and the real failure with an answer record, an
  authority record, an additional record or AA added, each with and without
  EDNS). `fix-unit02-after.log`: 13 top-level tests and 40 subtests pass, none
  fails or skips, 143.9 s: the guard test and its neighbors
  `TestRootUpdateOutputActualCompletionAndFinalRefusals`,
  `TestRootUpdateOutputDoesNotRelaxOrdinaryDataGeneration`,
  `TestRootAnchorOutputActualBlockedWriteDrainsBeforeFirstManagedCommit`,
  `TestRootAnchorOutputOriginalLifetimeAndTerminalFailure`,
  `TestAnchorCanonicalLossDrainsOutputAndSurvivesActualRestart`, the three
  `TestServingReload*`, both `TestRunServeUniFiHostname*` (the held queries of
  D332) and the two `TestPrimaryRuntime*` UPDATE tests. The "actual" subtests
  run the real upstream stage against a closed loopback port.
- The final head `bac6e7ab` on a clean tree, with the environment of the hosted
  e2e job (three require variables, DSN, `KUDURRU_NAMED`).
  `fix-e2e03-make-e2e-hosted-env.log`: `make e2e`, exit 0; `test/e2e` 313.6 s,
  `test/interop` 27.8 s; 24 top-level tests pass, none fails, one skips
  (`TestKnotCatalogConsumer`); 52 subtests pass, none skips.
  `TestCentralAuditTwoNodes` 15.89 s, `TestCentralQuerylogTwoNodes` 9.82 s,
  `TestKubeAuditAcrossReplicas` 24.07 s,
  `TestUpstreamFailureAnsweredAcrossCommits` 4.92 s (921 queries),
  `TestEnrolledClusterProcessesPromoteAndRejoin` 75.57 s (it passed here; see
  the retained `review-05`). `fix-bind01-interop-internal.log`:
  `make interop-internal`, exit 0: the five tests and their 25 subtests pass,
  no skip (`internal/rpz` 2.2 s, `internal/stages/authoritative` 1.4 s,
  `internal/zoneview` 1.2 s).
- `fix-build02-head.log` and `fix-e2e04-prebuilt-head.log`: the four changed or
  new tests on binaries built with the release flags from `bac6e7ab`
  (`make build`, 81,146,016 bytes, under the 100,000,000 budget; SHA-256 in the
  build log): four passes, no skip, 54.6 s; the licensed tests log
  `kudurru 0.2.0 (bac6e7ab, ...)`, the checksum, `tags licensetest` and the
  full commit. `fix-e2e05-regression-x5-4cpu.log`: the regression test five
  times under `taskset -c 0-3` with `GOMAXPROCS=4`: five passes, 22.7 s, 911 to
  948 queries each.
- `fix-lint01.log`: scoped golangci-lint v2.15.0 on `./cmd/kudurru/...` and
  `./test/e2e/...`, 0 issues. `fix-vet01.log`: gofmt, `go vet` of both
  packages, `CGO_ENABLED=0 go build ./...` and `bash -n hack/kind-e2e.sh` clean.
- `fix-unit03-wider.log`: at the final head `0048c606`, every test of the 23
  `cmd/kudurru` test files that expect a SERVFAIL or serve DNS with an
  unreachable upstream: 54 top-level tests and 85 subtests pass, none fails or
  skips, 395.1 s. Among them are the mounted `runServe` tests of direct anchors
  (bootstrap, follower revocation, recursive DNSKEY), stored TSIG on SQLite and
  Kubernetes, Kubernetes dynamic UPDATE, offline snapshot and recovery, the
  held UniFi queries, and the `TestServingBlockingRefresh*`,
  `TestServingClientExemption*`, `TestServingListScope*`,
  `TestServingMigration*`, `TestServingRuntime*` and `TestServingZoneNotify*`
  publication tests.
- `fix-kind01-stub.log`: `hack/kind-e2e.sh` at the final head with stubs for
  docker, kind, kubectl and helm. The docker stub executes the real release
  and `licensetest` binaries of that head for `docker run`, so the version line
  and the license check are real. Six cases: a digest reference and a reference
  without a tag exit 2 with no command run; `BUILD=0` with an image the daemon
  lacks exits 2 after one `docker image inspect`; `BUILD=0` with the release
  build exits 2 at the license check (`license key is signed with a key this
  build does not trust`), before `kind create cluster` and without cluster
  diagnostics; `BUILD=0` with the `licensetest` build behind
  `registry.example:5000` and `BUILD=1` both reach `kind create cluster` (the
  stub fails there on purpose). The same log lists how accepted references are
  split for the chart.
- `fix-actionlint02-head.log`: actionlint 1.7.12 on `ci.yml` and `publish.yml`
  at the final head, exit 0. `fix-actionlint01.log` has the control: a copy with
  the step id misspelled in the new condition is rejected
  (`property "e2x" is not defined`).
- `fix-deploy02-head.log` (and `fix-deploy01-sync-and-workflow.log` before the
  commits): the helm step (5 tests, OK) and, through Go,
  `TestVersionSyncScript`, `TestWorkflowActionsPinned` and
  `TestGoreleaserConfig` pass.

First pass (the two e2e logs marked superseded ran the first form of the two
central tests):

- `e2e07-make-e2e-hosted-env.log` (superseded by `fix-e2e03`): `make e2e` at
  `b162b1aa` on a clean tree with the environment of the hosted job. Exit 0;
  `test/e2e` 308.4 s, `test/interop` 27.2 s; 23 top-level tests pass, none
  fails, one skips (`TestKnotCatalogConsumer`); 52 subtests pass, none skips.
  All six licensed tests log the same harness-built binary.
- `bind02-interop-internal.log`: `make interop-internal` with
  `KUDURRU_REQUIRE_BIND=1`: the five tests pass (`internal/rpz` 2.2 s,
  `internal/stages/authoritative` 1.4 s, `internal/zoneview` 1.2 s).
  `bind01-baseline.log` is the same result before the change.
- `e2e09-three-on-4-cpus.log` (first form of the two central tests): the three
  un-skipped tests twice under `taskset -c 0-3` with `GOMAXPROCS=4`: six
  passes, 96.2 s.
- `e2e10-licensed-prebuilt-rerun.log` (first form of the two central tests):
  all six licensed tests and `TestCentralQuerylogTwoNodes` on binaries built
  with the release flags from `b162b1aa` (`make build`, 81,146,016 bytes;
  SHA-256 in `build02-prebuilt.log`). Each licensed test logs
  `kudurru 0.2.0 (b162b1aa, ...)`, the checksum, `tags licensetest` and the full
  commit. Seven passes, no skip, 246.6 s.
- Controls. `e2e03`: `KUDURRU_REQUIRE_ENVTEST=1` without binaries fails
  `TestKubeAuditAcrossReplicas`; `e2e04`: without the variable it skips as before.
  `e2e05`: `KUDURRU_E2E_LICENSED_BINARY` naming the untagged release build fails
  both selected tests at once, with the reason, before any node starts. `e2e06`:
  `KUDURRU_REQUIRE_POSTGRES=1` without a DSN fails both central tests.
  `bind05`: a `KUDURRU_NAMED` that does not exist fails the five BIND tests.
  `bind06-control-no-named-required.log` and `-optional.log`: `named` hidden by a
  private mount namespace; required: five failures; optional: the three parent
  passes over skipped subtests described above, and two skips.
- `unit01-kudurrutest.log`: seven new harness tests pass under race
  (`TestUnavailableSkipsUnlessRequired`, `TestNamedSkipsOrFailsWithoutBIND`,
  `TestNamedSearchOrder`, `TestBINDToolSkipsOrFailsWhenMissing`,
  `TestDescribeNamesTheArtifact`, `TestBuildLicensedPrebuilt`,
  `TestBuildPrebuilt`).
- envtest export. `envtest01-unset.json` and `envtest06-unset-api.json`: without
  the variable the eight tests skip. `envtest03`, `envtest04`, `envtest05`: with it
  they pass (`internal/auth/k8s` 15 passes, `internal/cluster` 21,
  `internal/zonecache` 32, `internal/controller` 38 with the opt-in
  `TestMeasureKube` skipped, `internal/api -run Kube` 5). Package times in the
  hosted go logs of run 42736964083 (`hosted-33ee04bc/go1.log`, `go2.log`:
  `internal/cluster` 2.3 s, `internal/auth/k8s` 4.6 s, `internal/zonecache` 12.6 s)
  match the local run without the variable (2.6, 4.5 and 13.4 s), not the run with
  it (7.0, 10.3 and 21.4 s), and the step environment in that log has no
  `KUBEBUILDER_ASSETS`. `maketest01-envtest-export.log`: the recipe in four
  configurations. `envtest02-primaryruntime-no-assets.log`: the failure the
  publish workflow would have hit.
- Workflows and scripts. `actionlint03.log`: actionlint 1.7.12 on `ci.yml` and
  `publish.yml`, exit 0 (`actionlint01-baseline.log` before the change);
  `yaml01-parse.log`: both files parse, with the job and step lists;
  `deploy01-workflow-tests.log`: `TestWorkflowActionsPinned`,
  `TestGoreleaserConfig` and `TestVersionSyncScript` pass; `sync02-ci-step.log`:
  the new helm step (5 tests, 0.1 s) and `make version-check` (PASS);
  `kind01-stub.log`: `hack/kind-e2e.sh` with logging stubs for docker, kind,
  kubectl and helm: `BUILD=0` builds nothing, both modes print the version and
  image ID before `kind create cluster`. `lint01.log`: scoped golangci-lint
  v2.15.0, 0 issues; `vet01.log`: gofmt, `go vet`, `go build ./...` and
  `bash -n` clean.
- Hosted baseline `hosted-33ee04bc/e2e.log` (run 42736964083): the four skips this
  package starts from.
- First-pass observations. `obs01-sqlite-blocking-audit.log`: on SQLite,
  `POST /api/v1/dns/blocking` is audited only as `kudurru.status` on
  `Kudurru/default`; six `GET /audit?kind=blocking` answers stay empty, while
  the API description of `kind` and the dashboard's audit kind filter list
  `blocking`, and Kubernetes records `blocking.toggle` (proposed follow-up row
  below). `obs02-servfail-dropped-across-commit.log`: one SQLite node at
  `33ee04bc`; the query sent right after `admin create-token` is dropped
  (`write failed err="serving: dependency changed"`), later ones get SERVFAIL
  at once, and a forwarded answer in the same position is delivered. This is
  the behavior that commit `49aae9ee` changes.

Retained failed runs:

- `e2e01-three.log` (tree at `33ee04bc` plus the switches): the three un-skipped
  tests fail, 176.3 s. It shows the missing `kind=blocking` event, the unanswered
  query of node-b and `audit keys: trusted verification material unavailable` on
  every replica. Two generated first-boot passwords of deleted throwaway nodes
  were redacted from this log; nothing else was changed.
- `fix-e2e01-before.log` and `fix-unit01-before.log`: the intended failures
  without the product change, described above. Three generated first-boot
  passwords of deleted throwaway nodes were redacted from the first (the script
  is `scripts/fix-redact.py`); nothing else was changed. The two tests now give
  their nodes a fixed admin password, so a later failure prints none.
- `review-05-make-e2e-hosted-env.log` (the reviewer's only full `make e2e` with
  the hosted environment, at `b162b1aa`): exit 2, 22 pass, 1 fail, 1 skip.
  `TestEnrolledClusterProcessesPromoteAndRejoin` failed after 21.1 s:
  `static process PUT /objects/dnsrecordsets/default/via-follower: status 503
  want 200, error completion_unknown`; both node logs are empty (the test sets
  the log level to error, and the primary logs nothing in this case). This
  package does not touch that test, `cmd` outside the output guard, or `ee`.
  The cause was not reproduced here; read from the code it is the limit that
  the HA-FIX ledger records as open ("the same guard can drop the
  acknowledgment of any single forwarded write that coincides with another
  primary commit"): the primary commits the forwarded command, and
  `MemberRequest.withOutput` (`internal/store/sqlite/member_request.go`) then
  requires the store authority, which includes the installed generation, to
  equal the one recorded at the command's completion. Any commit in between,
  for example the status write that follows an object change, yields
  `replication.ErrCAS`; the member server aborts the response
  (`internal/cluster/replica/member_server.go`, `panic(http.ErrAbortHandler)`),
  and the follower answers `completion_unknown`
  (`cmd/kudurru/serving_member_objects.go`, `objectForwardError`). The write
  was committed, nothing is replayed, and the test stops because it requires
  `200`. HA-FIX `process02-enrolled.log` is the same answer in the sibling
  test. Seen once in the nine runs of this test in the QUAL-CI logs (seven
  passes: `e2e07`, `e2e08`, `e2e10`, three in `review-12` and
  `fix-e2e03-make-e2e-hosted-env.log`; the ninth is the port failure below).
  **How to read the first hosted run:** this line in the hosted e2e log is
  that product limit, not a regression of this package, and since commit
  `52aee771` it no longer hides the BIND results of the same run; it does fail
  the step that carries G23-R4, so a hosted green needs either a rerun or the
  HA lane's answer first.
- `review-12-flake-promote-rejoin-x4.log`, run 3 of 4, and
  `e2e08-licensed-prebuilt.log`: a node could not bind a port that the harness
  had reserved earlier (`enrolled-b: kudurru exited before it was ready ... api:
  listen 127.0.0.1:34063: bind: address already in use` after 8.1 s;
  `conflict-c`, metrics port, after 11.3 s). `kudurrutest.FreePort` closes its
  listener and the node binds seconds later, up to about 100 s for a node that
  a test starts late; other lanes were running tests on this machine both
  times. Pre-existing and not changed here. **How to read it:** `exited before
  it was ready` with `bind: address already in use` in the node log is this
  harness race; a rerun is the remedy until the harness retries or allocates
  outside the ephemeral range.

Rows the integrator owns (this lane may not edit `dev-docs/followups.md`, the
decision log or the tracker; proposed text):

- Decision log: the row in `out/QUAL-CI/decision.md` (upstream-failure refusals
  across canonical commits). If it is rejected, drop commit `49aae9ee` together
  with `0048c606` (the rewrap of the paragraph it extends): the tree without
  them has the reviewed first-pass form of the query log test (fake resolver),
  and nothing else in this package depends on them.
- `dev-docs/followups.md`, section "API":
  `| Audit kind filter for blocking changes on SQLite: GET /audit?kind=blocking is always empty there, because the blocking command is audited inside its own transaction as kudurru.status on Kudurru/default and Options.AuditBlockingCommand is set only for the Kubernetes driver (which records blocking.toggle). The kind description of GET /audit (internal/api/authroutes.go:59) and the dashboard's audit kind filter (dashboard/src/pages/AuditPage.tsx:49) offer "blocking" on both drivers and do not offer "Kudurru". Decide: also record blocking.toggle on SQLite (a second row for one effect), or keep one row and make the description and the filter say which kind a driver uses | QUAL-CI, evidence obs01-sqlite-blocking-audit.log; TestCentralAuditTwoNodes reads the kudurru.status rows and passes either way | v0.2 (G21 contract) or the next patch release |`
- `dev-docs/followups.md`, section "Dataplane":
  `| DNSSEC validation failures across canonical commits: a query that ends in dnssec_bogus on a data generation that a commit just retired is still refused at output (no answer; the client times out), as upstream failures were before the QUAL-CI fix. Read from retainedDNSResponse, not exercised. Deciding it needs a mounted test with a signed zone and the anchor-retirement cases of D332 | QUAL-CI decision row | v0.2.x |`
- `dev-docs/followups.md`, section "Store and drivers" (the HA-FIX ledger
  already states the limit; this adds the test that meets it):
  `| A write forwarded through an enrolled follower answers 503 completion_unknown although the primary committed it, when another primary commit lands between the command's commit and its acknowledgment (sqlite MemberRequest.withOutput compares the store authority, which includes the installed generation; the member server then aborts the response). The status write that follows an object change can be that commit. TestEnrolledClusterProcessesPromoteAndRejoin met it once in a full make e2e (PUT of a record set through the follower, QUAL-CI review-05); TestEnrolledClusterProcessesPartitionCatchupAndRestart met it in HA-FIX process02 | QUAL-CI review-05-make-e2e-hosted-env.log; HA-FIX ledger "What remains open" | before a hosted e2e green is credited: HA lane to make the acknowledgment tolerate an unrelated later commit, or to say in the tracker that this explicit answer is accepted and how the test is read |`
- `dev-docs/followups.md`, section "Build, CI, docs":
  `| e2e harness port reservation: kudurrutest.FreePort closes its listener and the node binds the port seconds later (up to about 100 s for a node that starts late in a test). Another process can take the port in between: "kudurru exited before it was ready ... bind: address already in use". Seen twice on a shared machine (QUAL-CI e2e08: conflict-c metrics port; review-12 run 3 of 4: enrolled-b API port), not yet on a hosted runner. Fix candidates: retry a node start on EADDRINUSE with fresh ports, or allocate outside the ephemeral range | QUAL-CI review | v0.2.x |`
- Tracker text for G24-SYNCTEST: "already covered: `deploy.TestVersionSyncScript`
  runs `hack/test_sync_version.py` in every hosted go job and before
  publishing; QUAL-CI added a visible duplicate step to the helm job", not
  "now runs in CI".

What remains open: the hosted run itself (the acceptance of G23-R3, G23-R4
and G24-SYNCTEST is a hosted log line, the eight Kubernetes tests have never
run on a hosted runner, and the new step condition is checked by actionlint
only: that the BIND step runs after a failed `make e2e` step is unobserved);
the product change has a decision row without a number and is two commits
(`49aae9ee`, `0048c606`) that can be dropped together; `dnssec_bogus` refusals are still dropped in the
same window after a commit (read from the code, not changed, not exercised);
a forwarded write through an enrolled follower can answer `completion_unknown`
and fail `make e2e` (HA lane; `review-05`), and the harness port race can fail
it too (`review-12`, `e2e08`), so one hosted red on either signature is
expected now and then until they are fixed; the `kind=blocking` audit filter
on SQLite is a G21 contract question that the adapted test does not depend on;
`hack/kind-e2e.sh` was not run against Docker or kind in either mode (this
sandbox has no Docker daemon and no `kind`), so the second bullet of G22-WP6
and the new image checks rest on `bash -n`, the stub runs with real binaries
and, for `BUILD=1`, the next hosted run of the kind workflow, which will be the
first real execution of the license check; the new log lines of
`hack/compose-e2e.sh` were not run against Docker either; the other envtest
suites keep their own skip logic and rely on the `make test` check rather than
on a switch in each test; `TestKnotCatalogConsumer` still skips in hosted CI;
three tests fall back to a hard-coded `bin/k8s/1.37.0-linux-amd64`
(`internal/primaryruntime`, `internal/iammigrate`, `cmd/kudurru`
`TestRunServeKubeBootstrapAfterRecovery`), which `make test` no longer reaches
because it exports the variable, but a plain `go test` without it will fail or
skip once `make envtest` resolves `1.37.x` to a later patch (only 1.37.0 is
published today); the full `cmd/kudurru` package was not run here (the
integrator runs it through `hack/go-test.py`); the optional `make g22` target
was not added; the G22-WP8 final pass on the final head is not part of this
package.

**Review and verification state at 68a709e2.** The review accepted the lane's own code with nine findings, two major (the central query-log test had been rerouted around the dropped upstream-failure SERVFAIL; `make e2e` failed with `completion_unknown` through a follower). The fixer was partial: it made the product change recorded as D377 (`49aae9ee`), and the `completion_unknown` cause was fixed in the ha lane (D376); it rebutted three points (two stale test expectations and the redundant but kept helm step). The verifier resolved eight findings and left **two new problems open**: (1) `49aae9ee` also releases empty SERVFAILs the node produces itself (native-recursion capture refusal, failed `Stage.Bind` capture, indeterminate validation), so a recursive node now answers SERVFAIL with extended error 23 for a cache miss in the roughly 60 to 90 ms after each canonical commit, where the client used to get no answer and succeed on retry; (2) the commit, D377, `docs/architecture/query-path.md` and the changelog describe the class more narrowly than the code. The package that was to settle both, CORE-RETAINED-SERVFAIL, never started. Also open: the harness port race (minor finding, FLAKE-PORT-RACE never started; it failed hosted CI on `de0d249a`), and the claim that sixteen formerly skipped acceptance tests now run in CI is not credited: the e2e job passed on `de0d249a`, but nobody checked from that log that each test ran unskipped, and no hosted run of this pass is green.


### G21-01 Kubernetes forwarded auth and IAM routes answer the envelope — November 10, 2026 (D374)

On the Kubernetes driver the API forwards its `/auth` and `/iam` routes to the
legacy authentication stack after its own admission. Six registered routes had
no pattern on that mux and answered Go's plain-text `404 page not found`:
`GET /iam/rolebindings/{namespace}/{name}`, `GET /auth/cli/providers`,
`POST /auth/cli/{idp}/prepare`, `POST /auth/cli/{idp}/verify`,
`GET /auth/oidc/{idp}/logout` and `GET /auth/oidc/{idp}/logout/callback`. The
map found this by reading; a mounted envtest run confirmed all six and no
seventh.

The stack now serves the RoleBinding item read from the cache that backs the
list: the stored object with `apiVersion`, `kind`, UID and resourceVersion,
`404 not_found` for an absent object and `400 invalid` for a target that is not
a namespace label and object name (a config-file binding is not an object and
stays list-only). Public CLI login answers `503 cli_login_unavailable` and
provider logout continuation `503 provider_logout_unavailable`, each with a
hint, because both need the captured provider resources of the complete SQLite
runtime. A last fallback on the legacy mux answers `503 unavailable` with the
hint "requires the complete SQLite runtime" for any forwarded route without a
handler. `runServe` and the contract harness build the mux through one
function, `legacyAuthHandler`. `kudurru login` in its browser and device modes
keeps the `cli_login_unavailable` key through the safe-error filter and prints
a fixed client message that names `--token` and `--username`; no server text is
retained and no context is saved.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `race01-g2101-prefix`, `race02-g2101-prefix-inventory` (retained failures
  before the fix): envtest `TestRunServeKubeForwardedAuthRoutesAnswerEnvelope`
  fails; the inventory lists exactly the six routes with
  `status=404 content-type="text/plain; charset=utf-8"`.
- `race03-g2101-fixed`: the same test passes in 8.97 s on a mounted Kubernetes
  `runServe`. It reads the RoleBinding item (UID equal to the object created
  through the API server), checks six refused or absent targets, checks the five
  keyed refusals with hints, runs `kudurru login` and `kudurru login --device`
  through a verified-TLS front and matches the printed refusal, and requests all
  39 `auth` and `iam` operations of the served OpenAPI document with an
  administrator session, asserting that every answer of 400 or above is the JSON
  envelope with a key. `TestLegacyAuthHandlerFallbackAndRefusals` covers the
  fallback and its precedence.
- `race04-g2101-stack-apiclient`: default race of `internal/auth/stack`
  (10.4 s, includes the new `TestIAMRoleBindingItemRead`) and
  `internal/apiclient` (1.7 s, includes
  `TestCLILoginProvidersKeepsUnavailableKeyWithoutServerText`).
- `lint01-g2101`: scoped golangci-lint, 0 issues.
- `race11-root-regression` (final tree with regenerated artifacts): default race
  selection of 129 root tests around the three changes (Kubernetes mounts, the
  contract harness, CLI login, settings, source-entry, domain, migration,
  adoption, cutover, offline import, TSIG, unblock, zone and notify commands,
  member commands and forwards), 604.6 s: 127 passes, one skip
  (`TestCLIOIDCBrowserInstalledChromeActualTLS`, needs an installed browser) and
  one failure. `TestServingAdoptionActualHTTPFinalExpiryRollsBackAllZones`
  failed with a race-detector report between the test's own replacement of
  `opts.Now` (`serving_migration_adopt_test.go:375`) and the integration host's
  background refresh reading it (`integration_runtime.go:146`). Neither access
  is touched by this package, whose only edit in that file removes the gate
  wrapper from the test server. `race12-adoption-clock-race-head-x20` repeats
  the test 20 times at the same tree: 20 passes, 58.8 s. The race is a rare
  defect of that test and stays open. `TestForwardedShapes` now builds its mux
  through `legacyAuthHandler` and decodes the item read strictly into
  `api.RoleBindingDocument`.

What remains open: public CLI login, provider logout continuation, session
inventory, TOTP, external-account administration, API user commands and
RoleBinding writes stay SQLite-only on Kubernetes; each has a keyed refusal, and
the capability matrix and UI gating are G21-02/G21-13 work. The CLI recognizes
the refusal at the provider list, which both login modes read first; it does not
special-case the prepare and verify routes.

### G21-05 contract cleanups: vestigial 503 gate, declared 501, explained refusals — November 10, 2026

The complete SQLite runtime no longer wraps the API in `servingMutationRoutes`.
That gate answered `503 current_command_unavailable` for every unsafe request
under `/zones`, `/unblockrequests` and BlockList source entries that was not on
its list of converted commands. All nine registered unsafe routes of those
families are complete current commands, so only paths that are not routes still
hit it. Such a path now gets the router's own `404 not_found`
("no route METHOD path"). `TestCompleteCommandRouteFamiliesAreReviewed` keeps
the review at test time: it derives the unsafe routes of the three families from
the real API server and fails when one is added or removed without updating the
reviewed table. The map's acceptance text expected `405`; the router has always
answered `404 not_found` for any unmatched method and path
(`TestUnknownAPIPathIsEnvelope`), and this package did not change that contract.

`PATCH /settings` and `PATCH /node/config` declare the `501 read_only` answer
that a server without a node configuration writer (Kubernetes) already gave for
node-scope changes. `POST /settings/sections/{section}/ownership/{action}` and
`.../yaml` on canonical SQLite settings keep `503 unavailable` and now carry a
hint that names `kudurru settings set` and `kudurru settings get`. The
defensive stored-TSIG refusal says "not available on this node" instead of
blaming the storage driver, since both drivers mount management. The
dashboard's Authenticator panel shows "Authenticator management is unavailable
on this server" for `factor_management_unavailable`.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `race05-g2105-prefix` (retained failure before the fix): mounted SQLite
  `TestRunServeSQLiteUnregisteredMutationsAndOwnershipRefusals` shows ten
  unregistered unsafe paths answering `503 current_command_unavailable`, three
  ownership refusals without a hint and the three CLI texts without a hint.
- `race06-g2105-fixed`: 28 passes in 92.0 s: the mounted test (router `404` for
  the ten paths, a registered zone deletion still reaching its command, exact
  hints, and the exact CLI text of `kudurru settings adopt|release|yaml`),
  the route-family review test, and the source-entry, domain, migration and
  stored-TSIG command tests whose harnesses used the gate.
- `dashboard01-authenticators`: vitest for `Authenticators.test.tsx` and
  `Sessions.test.tsx`, 55 passes, with `tsc --noEmit` and scoped eslint clean.
- `gen01`, `drift01`: `make client-go` (with `make openapi`), `bun run gen:api`
  and `make docs-gen` change `api/openapi.json`, `api/openapi-3.0.json`,
  `internal/apiclient/openapi/zz_generated.client.go` and
  `dashboard/src/api/generated.ts` (the `501` response of the two PATCH
  operations) and nothing under `docs-site/content/reference`. Afterwards
  `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestLintRoutesCatchesMissingDeclaration`, `TestOpenAPIServedAndDocs`,
  `TestUnknownAPIPathIsEnvelope`, `TestGeneratedClient`, `TestCLIReferenceDrift`,
  `TestReferenceDrift` and `TestAggregatedRolesDrift` pass, and the dashboard
  typecheck passes with the regenerated types.
- `race10-api-stack-apiclient`: default race of `internal/api/...` (187.8 s),
  `internal/apiclient/...` and `internal/auth/stack` on the regenerated tree,
  exit 0.

`TestServingMutationRoutesDomainAliasesAndSourceCommand`, the unit test of the
removed gate, is deleted with it; the route-family test replaces its purpose.
Nothing else is open in this entry. The refusal inventory across every runtime
shape (G21-11) and role-aware refusals (G21-02) are separate packages.

### FWD-BOUND follower forwarding for apply, kind routes and batch deletion — November 10, 2026 (D375)

D362 forwards canonical object writes from an enrolled SQLite follower, and
`POST /apply`, the `POST`/`PUT`/`DELETE` routes of `/blocklists`,
`/clientgroups` and `/policies`, and `POST /{kind}:batchDelete` reach the same
forward. Capture refuses a request without a lifetime of at most 30 seconds,
and that lifetime was installed only for paths under `/objects/` and `/zones/`.
The other routes were therefore refused, with the misleading message "the
follower's source changed": kind routes with `503`, apply and batch deletion
with `200` and that error for every item. The tracker and CHANGELOG already
described `kudurru apply` as forwarded.

One predicate, `memberForwardRoute`, now names the 25 registered routes that a
follower forwards, compared segment by segment as the router does. A capture
refusal says whether the follower's source changed, the lifetime ended, the
request was canceled, or the route is not forwarded, and that the write was not
applied. Apply and batch deletion send one member command for each document or
name inside the single request lifetime and keep their per-item answers. An
item is reported with `unavailable`, not applied and never replayed when the
lifetime has ended, when the request was canceled, or when the follower
installed a newer image after the request began (its next pull, usually
carrying the request's earlier items). An item whose exchange with the primary
fails, including an unreachable primary, reports `completion_unknown`, as a
single forwarded write does. There is no document cap.

Review fixes (same day, commits `83b49fdf`, `6ddd7cef`, `8e3eeb87`, `d620b1ad`
and `8c7e4862`; the acknowledgment fix `29cdf81a` has its own entry below):

- Lifetime ended and canceled. The review showed that the documented
  `unavailable` refusal was unreachable through the router: `ObjectForward` and
  `ZoneForward` took a failed `servinghttp.SourceToken` (which refuses every
  ended context) to mean "not a follower" and fell through to the follower's
  local command, whose credential check answered `401 unauthenticated`,
  "current credential is invalid or expired". The forwards now read the mode of
  the request's original lease through the new `servinghttp.SourceMode`, which
  answers for an ended context while the handler holds its lease and returns no
  token. A request that began on a follower stays a forward and is answered with
  the capture refusal; nothing is sent and nothing runs locally.
- Route coverage test. `TestMemberForwardRouteCoversEveryForwardedRoute`
  compared the predicate with a list derived from the same predicate. It now
  requests every registered unsafe route on an API server whose
  `ObjectForward` and `ZoneForward` hooks record that they were reached, with
  the documents, name lists and zone inputs that forwarded routes take, and
  requires that the reached routes, the bounded routes and the reviewed table
  are the same 25.
- Operator page. `docs/deploy/sqlite-join.md` said both that an unreachable
  primary refuses a write with `503` and that it reports `completion_unknown`.
  The member client joins an unknown outcome to every failed write exchange, so
  the page now says `503 completion_unknown`, and `503 unavailable` for a
  primary that answers but cannot run the command (`8c7e4862` removes an
  ambiguous word from that sentence).
- Earlier bound kept (`d620b1ad`). The review listed one narrowed deadline:
  before this package a follower gave every `POST`, `PUT` and `DELETE` under
  `/objects/` and `/zones/` the 30-second lifetime, and the predicate then gave
  it only to the 25 forwarded routes. The other requests under the two
  prefixes ran with the ordinary five-minute route lifetime instead.
  `POST /zones/{zone}/notify` is the only registered one. No answer changed
  (see the probe below), but nothing justified taking the deadline away.
  `boundMemberObjectWrites` now bounds the forwarded routes and, as before,
  every other unsafe request under the two prefixes. `memberForwardRoute` is
  unchanged and still names exactly the forwarded routes, so the route test and
  the "this route is not forwarded from a follower" refusal keep their meaning.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `race07-fwdbound-prefix` (retained failure before the fix): on the follower
  fixture over actual member mTLS, `POST /apply` answers `200` with
  `errorKey: unavailable`, "the follower's source changed", for its first
  document.
- `race08-fwdbound-fixed-x5` (retained failure of a new test, not of the fix):
  `TestRootMemberForwardApplyKindRoutesAndBatchDelete` passes five of five and
  `TestMemberCaptureErrorNamesItsCause` five of five;
  `TestMemberForwardRouteCoversEveryForwardedRoute` fails five of five because
  the test compared a sorted list with an unsorted one. The two sets were equal;
  the test now sorts both.
- `race09-fwdbound-fixed-x3`: 15 passes in 23.4 s, three runs each of the route
  table test, the capture test, the route-family test and two follower tests
  (the two follower tests pass again in `race11-root-regression`).
  `TestRootMemberForwardApplyKindRoutesAndBatchDelete` applies three documents
  (zone, record set, block list) in one request, applies a refused document
  beside a committed one, creates, conflicts on and replaces a client group
  through the kind routes, creates and deletes a block list, batch-deletes a
  record set and a client group, and checks the primary's store, exactly one
  attributed audit row, no local install before the pull, and convergence of the
  follower after its pull. `TestRootMemberForwardApplyOriginalSourceCutRefusesItems`
  holds the body, lets the follower install a successor, and requires `200` with
  "the follower's source changed" for both documents and nothing on the primary.
- Real two-process runs with a relay-controlled partition. `process01-enrolled`
  and `process02-enrolled` are retained failures of the first test versions, and
  both taught something. In `process01` every new step before the partition
  passed in both scenarios (apply of two documents, `PUT` and `DELETE` of a block
  list, batch deletion, each converging in DNS or the list route on both nodes);
  the partitioned apply then reported its document with `completion_unknown`,
  not `unavailable`, because the member client classifies a failed POST exchange
  as an unknown outcome. The test now accepts either explicit key and proves by
  later DNS observation that nothing was applied or replayed. In `process02` the
  promote scenario passed (98.3 s) and the other failed before the partition:
  the second document of a two-document apply answered `completion_unknown`.
  The member server withholds its acknowledgment when the primary's authority
  advances again between a command's commit and its answer
  (`MemberRequest.CheckCommandOutput`), and the status controller's write for
  the block list created by the first document did that. It was the behavior
  of every forwarded write; a multi-item request provoked it against itself
  (fixed in the next entry). The test of that commit then applied one new
  document, waited for both nodes, and applied two documents of which the first
  was already stored (`unchanged`, `created`); the review-fix pass restored two
  new documents. `process03-enrolled-x2` (that earlier test shape): both
  scenarios pass twice (87.0, 103.5, 84.6 and 102.8 s; package 384.8 s). Each
  run covers through the follower:
  apply of one and of two documents, `PUT` (201) and `DELETE` (204) on
  `/blocklists/default/via-editor`, `POST /dnsrecordsets:batchDelete`, each
  observed on both nodes; and during the partition a kind-route `503`, an apply
  whose document is an explicit error and a batch deletion whose item is an
  explicit error, none applied or replayed after reconnection.
- `lint02-all`: `go vet` and scoped golangci-lint over `cmd/kudurru`,
  `internal/api`, `internal/auth/stack`, `internal/apiclient` and `test/e2e`,
  0 issues. `lint03-final` repeats both on the final tree: 0 issues.
- `final01-head`: at lane commit `55753c42` the tree is clean, `gofmt -l` is
  empty, `CGO_ENABLED=0 go build ./...` succeeds, `test/e2e` lints with 0
  issues after its last edit, and the drift tests pass again. The delivered
  head `0f580333` differs from it only by one corrected paragraph in
  `docs/deploy/sqlite-join.md`.
- Review-fix evidence (`fix-*` logs). `fix-race02-root-prefix-mutation`
  (retained failures before the fixes; `go test -overlay` restores the six
  changed sources to `0f580333` and removes `policies` from the predicate): on
  the follower fixture, after the body was held 31 s, `POST /apply` answers
  `200` with `errorKey: unauthenticated` for both documents, `PUT
  /clientgroups/default/late-c` and `POST /zones/late-zone/records` answer
  `401`, and `POST /clientgroups:batchDelete` reports `unauthenticated` for its
  name; a request canceled while its body is held answers `401` on a recorder;
  the route test reports "`POST /policies` reaches the follower forward but
  memberForwardRoute does not bound it" for the three policy routes.
  `fix-race05-root-fixed`: 28 root tests pass with `-race` in 124.6 s on the
  fixed tree, among them `TestRootMemberForwardAfterLifetimeEndedIsRefusedAsAForward`
  (33.3 s: `unavailable`, "30-second forwarding lifetime ended", "not applied"
  for the two apply items, the kind route `503`, the zone record command `503`
  and the batch item; the three new groups absent and the batch's group still
  present on the primary), `TestRootMemberForwardCanceledRequestIsRefusedAsAForward`
  (`503 unavailable`, "the request was canceled before this write was sent"),
  the rewritten route test, the two earlier FWD-BOUND follower tests, every
  `TestRootMember*` test, and the nine tests the reviewer selected.
  `fix-race06-root-fixed-x5`: the canceled, route and acknowledgment tests pass
  five times each (15 passes, 67.8 s). `fix-race03-light-packages`: default
  race of `internal/servinghttp` (19.5 s, includes
  `TestSourceModeAnswersForAnEndedRequestUntilRelease`).
- Earlier bound. `TestRootMemberFollowerRequestLifetimeBound` observes the
  remaining lifetime of a request where the API handler receives it, behind
  the production middleware chain of the follower and of its primary.
  `fix-race08-bound-prefix` (retained failure before the fix;
  `serving_member_objects.go` restored to `29cdf81a` through
  `go test -overlay`): on the follower, `POST /zones/example.test/notify` and
  four unregistered unsafe paths under `/zones/` and `/objects/` have
  4m59.99s instead of at most 30 s. `fix-race09-root-final` (fixed tree): 37
  root tests pass with `-race` in 179.1 s: the 28 of `fix-race05-root-fixed`,
  the new test (12 forwarded routes and 5 earlier-bounded paths at most 30 s
  on the follower; 9 other requests there, and all 26 on the primary, the
  ordinary lifetime), `TestCLIReferenceDrift`, and seven manual NOTIFY tests
  (five `TestServingZoneNotify*`, `TestRunServeSQLiteManualNotifyRestartAndRearm`
  and `TestZonesImportExportNotify`). `fix-race10-bound-fixed-x5`: the new test
  passes five times (13.1 s). `fix-race12-probe-unforwarded` (evidence only;
  `fix-probe-unforwarded_test.go.txt` supplied through `go test -overlay`): on
  the follower fixture `POST /zones/example.test/notify` answers
  `503 unavailable`, "current NOTIFY command unavailable", in 7 ms, and the
  four unregistered paths answer `404 not_found` in 1 ms. The bound therefore
  never decided an answer, with or without the fix.

What remains open for this entry: the lifetime-ended refusal is proven on the
in-process follower fixture over actual member mTLS, not in two real processes.
An apply through a follower is still cut by the follower's next pull (five
seconds by default): every later document then reports `unavailable`, "the
follower's source changed", and is not applied. That is the original-source
rule, the operator page says so, and the process test starts each apply right
after the follower converged so that it does not meet a pull. A request whose
context has ended on a primary or standalone node still gets the local
command's `401`, "current credential is invalid or expired"; that path was not
part of this package. The route test's probes cover the request shapes of
today's forwarded routes; a new route that needs a body of another shape before
it writes a canonical object reaches no hook there and still needs a review.
The earlier bound that `d620b1ad` keeps is the two-prefix rule as it was: a
future node-local route under `/objects/` or `/zones/` gets the 30-second
lifetime on a follower, as it would have before this package.
DHCPScope objects still answer "this object kind cannot be written through a
follower". Typed `primary_required` refusals, role reporting and the remaining
forwarded domains are FWD-REFUSAL, FWD-LOGOUT, FWD-SETTINGS, FWD-SELF,
FWD-NODELOCAL, FWD-UI and FWD-E2E.

### FWD-BOUND review fix: a forwarded command is acknowledged under its own commit — November 10, 2026 (D376)

The review confirmed that a multi-document apply through a follower was not
reliable: a later document could answer `completion_unknown` although the
primary had committed it, and the committed process test avoided the case. The
cause is on the primary and concerns every forwarded command, not only apply. A
member command committed, released the Store's writer lock, and then took the
lock again to admit its acknowledgment, which requires the exact authority and
generation the command produced (`MemberRequest.WithCommandOutput`). The
publication gate keeps other current commands out of that gap, but not writers
that take only the Store lock, such as status reconciliation. A commit in the
gap made the comparison fail (`ErrCAS`), the member server aborted the
response, and the follower mapped the lost exchange to `completion_unknown`.

`Store.MemberCommandWithOutput` now runs the same output admission in the
command's finalizer, after confirmed SQL completion and before the writer lock
is released, as a local command's output already does (D347).
`providercoordinator.WithMemberCommandOutput`, the only production caller (the
root's scoped member executor for `blocking.toggle`, `objects.write`,
`objects.delete`, `zones.command` and `auth.login`), uses it. No comparison is
relaxed: the exact authority and generation, the original owner, the peer
certificate, the deadline and the command receipt are all checked as before,
and completion is reported apart from the output's result. `MemberCommand`
followed by `WithCommandOutput`, and image reads, keep the two-step form and
keep refusing after any other commit. The process test applies two new
documents again.

Evidence under `.context/v020-final-20261110/evidence/HA-FIX/`:

- `fix-race01-ack-coordinator-prefix` (retained failure before the fix, three
  of three): `TestMemberCommandOutputAcknowledgesBeforeAnUngatedWriter` with the
  three sources restored to `0f580333` through `go test -overlay`. A writer
  queued during the command commits wherever the Store consults the original
  request before the output; the coordinator returns `committed=true` with
  "replication: authority changed". With the fix the test passes: the queued
  writer commits only after the acknowledgment returned.
- `fix-race02-root-prefix-mutation` (retained failure before the fix):
  `TestRootMemberForwardIsAcknowledgedBesideOtherPrimaryCommits` forwards 40
  client group writes through the follower fixture over actual member mTLS
  while one primary goroutine commits a section row every 300 microseconds.
  15 of 40 answered `503 completion_unknown`; all 40 were stored on the primary.
- `fix-race05-root-fixed`: the same test passes (40 of 40 answered `201`) in the
  28-test root selection, with every `TestRootMember*` test (blocking, object,
  zone and login commands, listener and revocation). `fix-race06-root-fixed-x5`:
  five more passes (200 forwards, none unacknowledged).
- `fix-race03-light-packages`: default race of
  `internal/cluster/enrollment/persistence` (140.9 s) and
  `internal/auth/providercoordinator` (92.6 s). New store tests:
  `TestMemberCommandWithOutputExcludesWritersThroughAcknowledgment` (a queued
  writer cannot complete during a 100 ms acknowledgment and commits right
  after it), `TestMemberCommandThenOutputStillRefusesAfterAnotherCommit` (the
  two-step form still answers `ErrCAS`) and
  `TestMemberCommandWithOutputSeparatesCompletionFromOutput` (no-op, output
  error, output cancellation, refused command, canceled command, image request,
  revoked peer, missing output). `fix-race04-replica`: default race of
  `internal/cluster/replica` (40.6 s). `fix-race07-sqlite`: default race of
  `internal/store/sqlite` (261.8 s).
- Two real processes. `fix-process01-enrolled-prefix-x2` (node binary built
  from the three sources at `0f580333`; `fix-overlay-binary-check` shows that
  the overlay reaches the fixture's nested build): the restored process test
  passes four of four scenario runs (429.5 s). The window is narrow in real
  processes, so the restored assertion alone does not fail before the fix; the
  earlier retained `process02-enrolled` did (one of four scenario runs).
- Two real processes under load (evidence only, not a committed test).
  `fix-process-stress-variant_test.go.txt` is the process test with one added
  block, supplied through `go test -overlay`: after the committed apply rounds
  it sends 150 applies of two new documents through the follower back to back,
  without waiting for convergence, and classifies the answers.
  `fix-process03-stress-prefix` (node binary built from the three sources at
  `0f580333`), two runs: 144 of 150 applies answered `created` for both
  documents and 6 answered `completion_unknown` for one of them, in each run
  (12 of 300). The unanswered document was the first of its request in 8 of
  the 12 and the second in 4, so the primary's follow-up writes for the
  previous request interpose as well as those for the same request. The
  variant checks the record sets (the second documents): all 4 that answered
  `completion_unknown` were served by the primary afterwards, so they had
  committed.
  `fix-process05-stress-fixed` (node binary of `29cdf81a`), two runs: 150 of
  150 applies answered `created` for both documents in each run (300 of 300),
  no `completion_unknown`, no other answer. `fix-process04-stress-fixed` is the
  same run cut off after its first line when the first fixer's session was
  interrupted; it holds no result.
  `fix-process02-enrolled-fixed-x3`: both scenarios pass three times on the
  fixed tree (102.2, 113.1, 94.8, 114.3, 94.6 and 112.9 s; package 643.7 s).
  Each run applies two new documents (a block list and a record set) through
  the follower four times, requires `created` for both every time, observes
  each on both nodes, and deletes the four record sets in one
  `POST /dnsrecordsets:batchDelete` that must report four successes.
- `fix-lint01`: `gofmt -l` empty, `go vet` clean and scoped golangci-lint
  v2.15.0 with 0 issues over `cmd/kudurru`, `internal/store/sqlite`,
  `internal/auth/providercoordinator`, `internal/servinghttp`,
  `internal/cluster/enrollment/persistence` and `test/e2e`.
  `fix-final01-head`: at `29cdf81a` the tree is clean, `gofmt -l` is empty,
  `CGO_ENABLED=0 go build ./...` succeeds and `TestOpenAPIDrift`,
  `TestRoutePermissionLint`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass.
- Final head. The second fixer (the first was interrupted before it could
  report) repeated the checks at `ba1351ac` with a clean tree, after its own
  three commits: `d620b1ad` and `8c7e4862` (see the entry above) and
  `ba1351ac`, which names `MemberCommandWithOutput` in two places of the
  architecture page and in one code comment that still described the two-step
  form, and separates a paragraph that `29cdf81a` had run into the next one.
  `fix-lint02-final`: `gofmt -l` empty for every Go file changed since
  `0f580333`, `go vet` clean and scoped golangci-lint v2.15.0 with 0 issues
  over the same six packages; `fix-lint03-replica` adds
  `internal/cluster/replica`, where `ba1351ac` changed the comment.
  `fix-race11-packages-final`: default race of `internal/servinghttp` (7.9 s),
  `internal/auth/providercoordinator` (37.9 s),
  `internal/cluster/enrollment/persistence` (60.2 s),
  `internal/cluster/replica` (19.8 s) and `internal/store/sqlite` (271.7 s).
  `fix-process06-enrolled-final-x2`: both two-process scenarios pass twice
  (98.4, 113.0, 95.3 and 114.7 s; package 428.1 s).
  `fix-final02-head`: `CGO_ENABLED=0 go build ./...` succeeds;
  `TestOpenAPIDrift`, `TestRoutePermissionLint`, `TestReferenceDrift`,
  `TestAggregatedRolesDrift` and `TestCLIReferenceDrift` pass; no generated
  file differs from `0f580333`. There is therefore no new generated commit: the
  seven fix commits (`83b49fdf`, `6ddd7cef`, `8e3eeb87`, `29cdf81a`,
  `d620b1ad`, `8c7e4862`, `ba1351ac`) sit on top of `0f580333` (`chore(gen)`).

What remains open: the acknowledgment still travels over the network, so a
forwarded command whose response is lost, cut by the 30-second bound or
refused by the remaining output checks (a fenced writer, a revoked peer, an
expired credential or certificate) answers `completion_unknown`, as designed.
The acknowledgment is written to the peer while the writer lock is held, as it
already was; it now also holds the lock from the commit on, so a slow peer
delays other primary writers by up to the request's remaining lifetime. Nothing
measures that delay. The process-level evidence of the window is statistical
and comes from an evidence-only variant (12 of 300 applies unanswered before
the fix, 0 of 300 after); the committed process test did not fail before the
fix in four scenario runs. The committed proofs are the coordinator test,
which is deterministic, and the mounted test of 40 forwards beside a committing
writer (15 of 40 unanswered before the fix). The review left the choice
between closing the window and documenting it to the integrator: this entry
closes it, and its decision row is drafted without a number.

**Review and verification state at 68a709e2.** The review accepted G21-01 and G21-05 as they were and FWD-BOUND with fixes: seven findings, two major (an ended follower request fell through to the local command and answered `401`; the process test did not apply two new documents through the follower). The fixer resolved all of them in seven commits, rebutted four contract notes, and added the third decision row (now D376) instead of reverting `29cdf81a`. The verifier confirmed both major findings resolved, with nothing left unresolved and no new problem. The integrator numbered the rows D374–D376 and replaced D375's reason with the correction written by CORE-APPLY-MULTI: running a cut apply again through a follower does not finish it. Still open from this package: the data race of `TestServingAdoptionActualHTTPFinalExpiryRollsBackAllZones` between the test's `opts.Now` replacement and the integration host's refresh (outside the package, not fixed).


### F28-HA-1/F28-HA-5 Managed root anchors on fenced and conflicted members, and member clock skew — November 10, 2026 (D369)

The map predicted from code reading that a node with `anchorMode: managed` stops
answering all DNS, and cannot restart, once it is fenced or conflicted. The
prediction reproduced on unmodified `33ee04bc`. `race01` and `race02` run the new
managed-mode copy of `TestRootAnchorPreparationPublishesFencedDNSOnly` beside the
static original: the static test passes, and the managed test fails after 16.5 s
with the publisher never reaching `dns_only` and both UDP and TCP answering
`SERVFAIL` for the node's own SOA. Root admission accepted only the writable and
follower roles; managed preparation, every output capture and startup admission all
used that one rule. The exposure is wider than an old primary: a follower observes
a successor's term (and becomes fenced) before it installs that term's image, so
every managed follower went through the same outage at each promotion.

Root admission for managed anchors is now split (D369). Serving admission accepts
every role that can hold this member's accepted owner: writable, follower, fenced
and conflict. A fenced or conflicted cut is prepared, checked and published only
with its token's own DNS-only disposition (D360), through the unchanged Store
registration path; `internal/store/sqlite/anchor_publication.go` already accepted
`PublicationOptions{DNSOnly: true}` with an anchor registration and needed no
change. Startup admission accepts the same roles with `managedTopology: direct`,
and with `edges` after the same complete fleet observation under the original
token. Updater admission is unchanged: only the elected writer of the original
term polls as a writer, and an unelected role never receives a writer token.

The decision the map left open is recorded as "yes": every role without an elected
updater keeps the deny-only supervisor (D318). A fenced or conflicted member can
stay in that role for days, and it is still answering with DNSSEC validation, so a
revoked root key must not stay trusted there. The supervisor runs one child per
member, term and role. A role change inside one term (fenced to follower by
install, or to conflict or divergence) joins the old child and admits one new
child; ordinary installs still cannot rearm a terminal result. A signed REVOKE
observed in the fenced or conflicted role commits the same durable local denial as
on a follower and retires the DNS-only output; no observation in these roles
changes accepted history, the canonical image or the authority.

F28-HA-5: the follower poller and the elected updater returned a terminal
`ErrClock` whenever the local wall clock was before the committed checkpoint's
wall time, which the supervisor and the writer adapter then kept for the whole
authority epoch or writer term. Both now use one rule, `anchorCheckpointWait`: a
clock that trails the checkpoint by at most `trustanchors.ClockTolerance` (five
minutes) waits until it reaches the checkpoint, acquires nothing meanwhile and logs
a time-synchronization warning; a larger gap stays a terminal `ErrClock`. The
poller then acquires once and resumes its cooldown; the updater returns to the
accepted `NextRefresh`. The wait creates no sample, receipt, checkpoint or state,
so the pure package's credit rules are untouched: a new writer session still gets
no credit for its first observation, and credit inside one session is still
checkpointed monotonic progress, withheld when wall and monotonic progress differ
by more than the tolerance. The comparison reads the wall clock through a private
field on the poller and updater that only tests set (the mounted poller takes it
from the test-only `servingRuntimeOptions.AnchorWall`); samples, receipts and
stored state always use the real session clock, and no protocol constant changed.

The wait must not create a terminal outcome of its own. An independent review of
the first form (lane head `9204e534`) found one. The poller pins every captured
operation to its installed incarnation, and only a completed normal observation
let a later capture move to a newer one. A child that had only waited so far had
completed nothing, so any ordinary replicated install during the wait, which can
last five minutes, ended it with `ErrFenced`, and the supervisor kept that result
for the whole member, term and role. That hit every freshly started child: a
process start, a term change, or the new fenced-to-follower role change. A poller
that has started no acquisition and whose last operation only deferred for its
clock was neither held nor refused (D318), so its next fresh capture may now adopt
a newer installed incarnation in the same member, term and role, as the
supervisor's readiness wait does. The other rules are unchanged and each has a
test: a refused or failed acquisition cannot move, a clock wait after a failed
acquisition changes nothing, no operation crosses a role change, and a first
capture that never waited must still match the cut its supervisor classified. The
elected updater has no installed incarnation to keep (every step captures a fresh
operation under its original writer term) and needed no change.

The same review found two proof gaps with correct production behavior behind
them, and both now have lane tests: the edges test ran with static anchors, and
no test drove a fenced member back to follower by install under the mounted
supervisor. The first form is commits `c662dfc0` and `9204e534`; the review fixes
are the two commits after them, ending at lane head `f9818a59`.

Evidence under `.context/v020-final-20261110/evidence/F28-FENCED/` (default race
detector, real SQLite, real TCP/UDP sockets, signed in-process root authority; no
forged anchor state or sample). The first block ran on the first form (lane head
`9204e534`); the review-fix block after it ran on the final tree:

- `race03`: `TestRootAnchorPreparationPublishesFencedDNSOnly` (static, 1.05 s) and
  `TestRootAnchorManagedPreparationPublishesFencedDNSOnly` (3.01 s) pass with the
  fix.
- `race08`, every first-round test in its final form plus the affected root selection
  (`Anchor|Doctor|Primary|Cluster|Join|Promot|Member|Enroll|Fenc|Replica|Follower|
  CLIReference|ManagedTopology|ManagedEdges|ServingRuntime` over `./cmd/kudurru`
  and `./internal/config`): exit 0, 360 pass entries (171 top-level tests), zero
  failures, one actual skip (`TestDoctorDatabase`, no PostgreSQL DSN), 405.9 s plus
  2.4 s. It contains:
  - `TestRootAnchorManagedPreparationPublishesFencedDNSOnly` (3.04 s) and
    `TestRootAnchorManagedPreparationPublishesConflictDNSOnly` (2.97 s): an actual
    managed/direct runtime bootstraps through the elected `RunWriter` mount, then
    loses its term. Status `dns_only`, authoritative SOA over UDP and TCP through
    the production output guard, `CaptureHTTP` refused, the published bundle keeps
    the managed root seal, exactly one supervision acquisition (two root reads in
    total, none more inside the cooldown), and the image, accepted owner version
    and authority are unchanged.
  - `TestAnchorDNSOnlySupervisorRevocationRetiresOutput/fenced` (1.80 s) and
    `/conflict` (1.83 s): the supervisor's own held signed REVOKE is released; a
    held output attempt refuses to emit, UDP and TCP answer an empty `SERVFAIL`,
    two root reads, canonical image and authority unchanged.
  - `TestAnchorFollowerSupervisorFencedRoleKeepsDenyOnlyPollingAcrossReopen`
    (1.56 s): a follower fenced by a higher term publishes DNS-only, a new child
    for the fenced role acquires independently, the REVOKE retires output, and a
    physical reopen of the fenced database passes startup admission with the
    denial retained and no acquisition.
  - `TestAnchorEdgeActivationAdmitsDNSOnlyCutOnlyWithCompleteFleet` (4.39 s, eight
    subtests): for a fenced and for a conflicted backend, a complete two-edge
    observation admits the DNS-only cut (protected HTTP stays closed); an
    unavailable edge, a missing backend membership and an authority change during
    the observation each refuse after contacting the whole fleet exactly once.
    This fixture leaves `anchorMode` at its static default with a dormant owner,
    so it proves the edges admission rule only and its `dns_only` publication is
    the static path from D360. The managed owner behind edges is proven by
    `TestAnchorEdgeActivationManagedDNSOnlyRestartNeedsCompleteFleet` below.
  - `TestRunServeDirectAnchorsFencedAndConflictRestart` (17.62 s): one actual
    `runServe` database is bootstrapped as writer, then fenced, then conflicted,
    with a physical close, reopen and restart for each role. Each start reaches
    operational listeners with serving not ready and auth status 503, answers the
    last-good record on UDP, TCP, DoT, DoH, DoQ and DoH3, makes exactly one root
    acquisition (three in total with the bootstrap) and leaves the authority, the
    image and the accepted owner version unchanged.
  - `TestAnchorCheckpointWaitBounds` (seven cases, including exactly five minutes
    and one nanosecond more), `TestAnchorFollowerPollerWaitsForCheckpointWithin
    ClockTolerance` (3.03 s: a checkpoint two seconds ahead gives two non-attempts
    with one root read, then exactly one poll, then the cooldown),
    `TestAnchorFollowerPollerCheckpointBeyondClockToleranceIsTerminal` (0.96 s) and
    `TestAnchorUpdaterCheckpointSkewWaitsWithinToleranceAndRefusesBeyond` (2.82 s:
    a successor writer session neither stops nor acquires, keeps the accepted
    owner version, and returns to the exact `NextRefresh`; five minutes plus one
    second stays terminal and is not recaptured).
  - Every pre-existing anchor, follower, supervisor, edge, cluster, join and
    promotion test in that selection, including `TestAnchorFollowerPollerRetains
    EpochDuringCooldown`, `TestAnchorFollowerSupervisorTerminalSurvivesGeneration
    Install`, `TestAnchorEdgeActivationRefusesChangedOriginal` and
    `TestCLIReferenceDrift`.
- `race09`: `TestRunServeDirectAnchorsFencedAndConflictRestart` (17.45 s) again and
  `TestRunServeDirectAnchorsFencedRecursiveValidation` (8.16 s): a fenced managed
  node restarted against a private signed root on a non-loopback interface returns
  AD-validated native recursion on all six transports from its last accepted
  owner, with exactly one supervision TCP acquisition and unchanged history.
- `pure01` (light run): `TestClockJumpCannotCreditCandidateAndBackwardTimeRefuses`,
  `TestRestartPreservesOnlyDurablyCheckpointedSessionProgress` and
  `TestFiveSeedAnchorsAndIndependentClockSessions` pass; the pure package is
  untouched.
- `lint01`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`: zero issues.
  `gofmt -l` is empty for the touched files and `go vet ./cmd/kudurru` is clean.
- `drift01` (light run on the lane head): `TestOpenAPIDrift`, `TestReferenceDrift`
  and `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in `race08`.
  The package changes no API route, DTO, setting, CRD, permission or CLI text, so
  there is no generated commit.

Review fixes, on lane head `f9818a59` with a clean worktree (`fix-lint02`,
`fix-vet03`, `fix-race07`; every `fix-*` log records the head, the count of
uncommitted paths and a digest of the `cmd/` diff against `9204e534`, which is
`b01da997d44800ee` for the final tree):

- `fix-race07`, the same affected root selection over `./cmd/kudurru`
  (`./internal/config` is untouched by the review fixes and was not rerun): exit 0,
  327 pass entries (169 top-level tests), zero failures, the same one skip
  (`TestDoctorDatabase`), 427.6 s. Against `race08` its test set drops only
  the nine `./internal/config` tests and adds the six tests below and
  `TestRunServeDirectAnchorsFencedRecursiveValidation` (`race09`). It contains:
  - `TestAnchorFollowerPollerClockWaitAdoptsOrdinaryInstall` (1.46 s): a poller
    built as `runAnchorFollowerEpoch` builds it (supervisor epoch preset) defers
    with its wall clock held two seconds before the checkpoint. Two ordinary
    primary writes are installed during the wait (same member, term and role,
    newer incarnation, unchanged checkpoint). Each following step still waits,
    acquires nothing and holds the newly installed cut; once the clock reaches the
    checkpoint there is exactly one poll (two root reads in total), then the
    cooldown, with image and authority unchanged.
  - `TestAnchorFollowerSupervisorClockWaitSurvivesOrdinaryInstall` (6.61 s): the
    same through the mounted supervisor, its child and the child's wake loop. The
    follower keeps complete publication while its child waits, an ordinary install
    lands, two further clock checks prove a capture after the install, and after
    the clock is released there is exactly one supervision poll and none more
    inside the cooldown.
  - `TestAnchorFollowerPollerClockWaitKeepsRefusalRules` (three subtests): after a
    wait, a role change is still a terminal `ErrFenced` and is not recaptured; a
    poller whose acquisition failed, then waited for a clock set back, still cannot
    move to a newer install; and a first capture that never waited still refuses a
    cut newer than the one its supervisor classified.
  - `TestAnchorUpdaterCheckpointWaitSurvivesOrdinaryWrites` (0.93 s): two ordinary
    writes during a newly elected writer's wait neither stop it nor make it due;
    it returns to the exact accepted `NextRefresh` with the owner version unchanged.
  - `TestAnchorEdgeActivationManagedDNSOnlyRestartNeedsCompleteFleet` (13.29 s,
    eight subtests): for a fenced and for a conflicted backend, an actual managed
    backend bootstraps through its elected writer behind a complete two-edge fleet
    (one root read, two fleet calls), loses its term durably, and its database is
    physically closed and reopened. With a complete observation the restart is
    admitted (two more fleet calls), publishes `dns_only` with the managed root
    seal and no static seal, emits through the production output guard, refuses
    `CaptureHTTP`, makes exactly one supervision acquisition (two root reads, none
    more inside the cooldown) and leaves the accepted owner version and the
    authority unchanged. An unavailable edge, a missing backend membership and an
    authority change during the observation each refuse startup after contacting
    the whole fleet exactly once, with no root acquisition.
  - `TestAnchorFollowerSupervisorPromotionFencedThenInstallResumes` (3.45 s): a
    second member database is promoted for real. The supervised follower observes
    that term (fenced under it: `dns_only`, protected HTTP closed, one new
    supervision poll, managed seal emitted), then installs the successor's image,
    which is a role change inside that one term: complete publication resumes
    with one new supervision poll (four root reads in total, none more inside the
    cooldown), and the image, accepted owner version and authority are unchanged.
    The accepted owner is the same on both sides of this promotion; a divergent
    owner is F28-HA-2.
- `fix-lint02`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`: zero issues.
  `fix-vet03`: `go vet ./cmd/kudurru` is clean; `gofmt -l` is empty for the touched
  files. `fix-vet02` vets the first review-fix commit alone through an overlay.
- `fix-drift01` (light run): `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in `fix-race07`.
  The review fixes change no API route, DTO, setting, CRD, permission or CLI text,
  so there is still no generated commit.

Retained failed runs and what they showed:

- `race01`, `race02` (unmodified production code): the predicted defect, as above.
- `race05` (mutation A: serving admission put back to writable-or-follower; 14
  failing entries): all six HA-1 tests fail. `runServe` never brings listeners up
  on the fenced database, edges topology refuses with "root anchor policy
  unavailable or changed" before contacting any edge (0 calls), fenced and
  conflicted nodes answer `SERVFAIL` on UDP and TCP with `prepare_failed`, and no
  supervision acquisition happens.
- `race06` (mutation B: D369 serving admission kept, supervision put back to the
  follower role only; 5 failing tests): DNS-only serving and the edges admission
  test pass, and every supervision assertion fails (root reads stay at one, the
  fenced follower never acquires, the revocation is never seen).
- `race07` (mutation C: any checkpoint ahead of the local clock is terminal again):
  the two within-tolerance tests and three bound cases fail with `ErrClock`; the
  beyond-tolerance test passes either way, as it should.
- `race10` (mutation A again, on the two tests added or extended after `race05`):
  `TestAnchorDNSOnlySupervisorRevocationRetiresOutput/fenced` and `/conflict` never
  acquire, and `TestRunServeDirectAnchorsFencedRecursiveValidation` never brings
  listeners up.
- `review-probe01`, `review-probe02` (the reviewer's probes on `9204e534`, source
  in `review-probe-source.log`): `TestReviewProbeClockWaitThenOrdinaryInstall`
  fails with a terminal `replication: writer fenced` after one ordinary install
  during the wait (the hole above), and
  `TestReviewProbeLaneEdgesDNSOnlyFixtureAnchorMode` shows the edges fixture with
  `anchorMode="static"`, a dormant owner and a static seal (the proof gap). The
  managed edges restart and promotion probes pass. `review-probe01` is the earlier
  run in which one probe failed on the reviewer's own helper.
- `fix-race01` (the new tests before the fix: the poller file overlaid with the
  `9204e534` rule, test seam kept; 24 pass entries, 2 failing tests): the step
  test fails with `an ordinary install during the checkpoint wait ended this poller
  for its epoch: err=replication: writer fenced reads=1`. The mounted test logs
  `root anchor follower stopped for this authority epoch err="replication: writer
  fenced"` and fails with no clock check after the install and one root read. The
  refusal-rule, updater, managed edges and promotion tests pass, as they should.
- `fix-race03` (mutation A again, on the edges and promotion tests; 19 failing
  entries, none passing): the managed and the static edges tests refuse with "root
  anchor policy unavailable or changed" before contacting any edge, and the
  promotion test never reaches `dns_only` in the fenced role.
- `fix-race04` (mutation B again; 15 pass entries, 4 failing): the static edges test
  passes in full, which is exactly why it could not stand for managed anchors. The
  two managed `complete` cases publish and emit, then fail on the supervision read
  that never comes; the promotion test fails in the fenced role.
- `fix-race05` (mutation R: the supervisor keys its child by member and term only,
  as before D369): only the new promotion test fails. The member returns to
  complete publication, its fenced-role child stops with `replication: writer
  fenced` at the role change, no new child is admitted and root reads stay at
  three, so it would have no revocation polling for the rest of the term.
  `TestAnchorFollowerSupervisorFencedRoleKeepsDenyOnlyPollingAcrossReopen` and
  `TestAnchorFollowerSupervisorTerminalSurvivesGenerationInstall` pass under it.
- `fix-race08` (mutation R over the whole anchor selection,
  `Anchor|Fenc|Follower|ManagedTopology|ManagedEdges|ServingRuntime|Promot|Replica`,
  on lane head `f9818a59`; 194 pass entries, 100 passing top-level tests,
  332.0 s): exactly one test fails, the new promotion test. No other test in that
  selection notices a supervisor that ignores the role, which is the gap the
  review named.
- `fix-lint01` (first lint of the fix): one staticcheck QF1001 finding (De Morgan)
  on the new condition. It was rewritten to the equivalent form with no other
  change. `fix-race02` (the new tests: 26 pass entries, 34.2 s) and `fix-race06`
  (the affected selection: 327 pass entries, 169 top-level tests, one skip,
  430.6 s) passed before that rewrite, on digest `38537d583d13d8ee`; `fix-race07`
  is the rerun after it. `fix-race01` and `fix-race03` to `fix-race05` also ran
  on that earlier digest.

`race04` is the first passing run of the new tests (26 pass entries, 39.6 s),
before the revocation test was extended to the conflict role and renamed and before
the recursion test existed. The first-round mutations were temporary edits of the
working tree, restored byte-for-byte before the commits. The review-round variants
(`fix-race01`, `fix-race03` to `fix-race05`, `fix-race08`) were compiled through
`go test -overlay` and never touched the worktree (`fix-batch01.sh`,
`fix-batch02.sh`; `fix-overlay-source.log` shows each replaced file as a diff);
lane head `f9818a59` contains no mutation.

What remains open. This package does not change how a fenced or conflicted member
gets back to follower: F28-HA-2 (package F28-HISTORY) still owns the strict
`anchorSuccessor` install rule, which refuses an authoritative branch whose owner
generation is not greater than the local one and also refuses an owner whose
`UpdatedAt` is earlier than the local one (a second place where member clock skew
matters). Until then such a member now serves DNS-only and supervises instead of
answering `SERVFAIL`, but it can stay fenced until the primary's generation passes
its own. Fencing and conflict in these tests come from `Store.ObserveAuthority` on
real databases, not from scheduled member-TLS exchanges between separate processes
(the promotion test promotes a second real member database, but still delivers its
term and its image by direct Store calls); the separate-process managed acceptance
is F28-HA-3/F28-HA-4. The edges tests use in-process authenticated edge peers; the
opt-in pinned dnsdist test was not rerun.
The clock wait is proven at the poller and updater step, at the pure rule's bounds
and, for the poller, under the mounted supervisor, always with a test wall-clock
reading (trailing or held), not with a skewed operating-system clock and not
between separate processes; the updater's wait has no mounted test. A gap beyond
five minutes is still visible only as a log line (`invalid clock continuity`); an
operator-visible status is F28-GATE-4. Two refusals that predate this package are
unchanged and can still end supervision for a member, term and role without any
clock skew: an install that lands inside a capture, between its image read and the
Store barrier check, refuses that capture (D318), and a child whose very first
capture already sees a newer cut than its supervisor classified is refused. Each
window lasts only as long as one capture. The first is read from the code and has
no dedicated test; the second is pinned by
`TestAnchorFollowerPollerClockWaitKeepsRefusalRules/unwaited-first-capture`. D369's
history clause (F28-HA-2) is decided in the row but not implemented by these
commits.
Each promotion now costs a follower up to two root DNSKEY TCP queries (one in the
fenced role, one after install) instead of one. The enrolled process scenarios in
`test/e2e` were not rerun: they use static anchors with topology disabled, where no
changed function is mounted.

**Review and verification state at 68a709e2.** The review accepted F28-HA-1 and asked for fixes to F28-HA-5: four findings, two major (the clock wait pinned the poller to its first capture's incarnation; the edges variant of the new test never involved a managed owner). The fixer resolved all of them in two commits and documented the widened admissions the reviewer listed as weakened guards. The verifier confirmed both major findings resolved, with nothing unresolved and no new problem. Integrated in the first batch.


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

**Review and verification state at 68a709e2.** The review accepted the package with six findings (five minor, one nit). The fixer resolved them in five commits; the lane ran no verification, and the post-verification pass (run `wf_e35d6a8c-451`, logs `pv2-*` under `evidence/OBS-FANOUT/`) found the fix commits sound: findings 1, 3, 4 and 5 are resolved and each new test fails on the pre-fix code. Open: (a) finding 2, `TestRunServeSQLiteCompletePrimaryUpdateRestart` can answer `SERVFAIL` (`servfail_unsynced`) for a committed primary zone in a window after a status write (1 of 8 with an injected 1000 ms pause); attributed to code that predates the package, and its package CORE-STATUS-WRITE-SERVFAIL never started; (b) two nits (an idle sink reported as undrained when the collector itself fails to drain; the `sink host stopped` path has no test). The 300 ms close bound the post-verifier called too tight was widened to 2 s by the integrator (`bb6b73aa`; twelve race runs on two cores passed, `evidence/INTEGRATE-1/sink-close-bound01.log`).


### DOCS-OPS operator documentation for shipped v0.2 features — November 10, 2026

Map entries G21-14 and G25-OPDOCS. No decision number: no design decision was
needed. Base commit `51878320` (lane branch `wip/v020/docs`, after DOCS-ARCH).
Ten source commits: `e4a7c586` (enrolled cluster guide), `fa515d4a` (dynamic
update guide and stored TSIG keys), `fafe2b4a` (Kubernetes role bindings and
grant floors), `05d20854` (deployment index), `418066be` (runbook for a lost
primary), `ba7d9fb9` (upgrade runbook pointer to the IAM migration),
`ca2b8965` (Access screenshots and the screenshot script), `4e2f1851` (guide
text), `12b3e022` (sign-in page) and `7c7c195b` (note about the Actions
column). A second review round added six: `4cd4505e` (every update policy
that stops a SQLite zone), `055630d0` (update answers for a key without a
grant), `d4af05c0` (CLI password login on a follower), `350efba0` (what a
follower passes on and how apply behaves), `4f4c27fb` (order of the v0.1
upgrade around the CRD step) and `5f10ed88` (the eleven errors of the guide
checker). No generated commit: the package changes no generator input, and
`make docs-gen` leaves the generated reference unchanged.

Operators now have pages for the v0.2 features that were only described in
the architecture tour or the CLI reference, and the stale lines named in
G21-14 are corrected. The procedures of the new SQLite pages were run against
real `kudurru serve` processes on loopback with a binary built from the lane
head. The first version of two pages did not state everything those runs
answered: the second review found three wrong or incomplete statements and
two behaviors that the first runs had recorded and the pages and this ledger
had left out. The section "Review fixes" below lists them, and after it the
pages state what the runs answer, with one stated dependency on the HA lane.
The Kubernetes statements were checked against the code and the existing
envtest tests, because no cluster is available here.

What the pages now say:

- **Enrolled SQLite cluster (`docs/deploy/sqlite-cluster.md`, new).** The
  whole life of a cluster: what to prepare (the same explicitly configured
  encryption key on every member, member names and internal addresses, a
  token-based CLI context for each member), activation of the first primary
  (`cluster enrollment`, `cluster activate`, restart), invitation and offline
  join, checking the members, what a follower passes on to the primary and
  what its two 503 answers mean, how `kudurru apply` and `kudurru restore`
  behave with several documents, promotion (`cluster promotion`, `cluster
  promote`, the meaning of each `reason`), the return of the old primary,
  recovery after two promotions, the limits of this version and a table of
  the answers of the cluster commands, with a small state diagram. The join
  page, `compose-ha.md`, `deploy/compose/ha/README.md`, the deployment index
  and the site's "Choose a deployment" table link to it. The join page keeps
  its sections: the HA and F28 lanes are editing it, so only a three-line
  pointer was added at its top. `docs/deploy/runbooks.md` gains "The primary
  of an enrolled SQLite cluster is lost", and its upgrade section now points
  a Kubernetes installation that comes from v0.1 to the offline identity
  migration of `iam-upgrade.md`, which nothing linked from there.
- **RFC 2136 (`docs/deploy/dynamic-updates.md`, new).** Key creation (file key
  or stored key through `kudurru api`), `spec.policy.update` and
  `spec.policy.updatePolicies`, the four things to check in a policy because
  the API accepts policies that take a SQLite zone out of service, an
  `nsupdate` example, what an update may not change, a table of answers (with
  `SERVFAIL` on SQLite for a key that no grant of the zone names), SQLite storage and backup of dynamic records,
  the enrolled cluster (updates go to the primary; a follower answers
  `SERVFAIL`), static twins, and on Kubernetes the `rfc2136-<zone>` record set
  (owner `dns-update`, namespace rule, eight attempts within ten seconds, do
  not hand-edit, selector labels, backup and restore).
- **Kubernetes role bindings (`docs/deploy/rolebindings.md`).** "Kubernetes
  durable accepted-authority work remains open" is replaced by a section on
  what Kubernetes does (D364): frozen roles of sessions and personal tokens,
  removal and regrant, the `kudurru-grant-floors` Secret, what to avoid, the
  fail-closed start, the unobserved removal while no replica runs, the limits
  and Helm's handling. `helm.md` and `kubernetes-service-account-auth.md`
  point to it.
- **Stored TSIG keys (`docs/deploy/tsig-keys.md`).** "once at startup" and
  "Full supported-driver rotation qualification remains separate" are gone;
  the page shows the `kudurru api` calls and where a zone names a key.
- **Administrator guide.** Chapter 9 no longer mentions "Read-only in this
  release" and gains "Use an authenticator" (set up, replace, remove, remove
  for another person) and "See and end your browser sessions"; chapter 10
  gains "Sign in with single sign-on" (browser and `--device`) and a table of
  the `cluster`, `iam`, `integrations`, `anchors` and `api` command groups;
  chapter 11 gains "Recover a lost authenticator" (host token, `kudurru api`);
  chapter 7 points to the dynamic update page. The dashboard pages for Access
  and Identity follow, and the sign-in steps of chapter 2 and of the first
  steps mention the Authenticator code field. The screenshots of the Access
  page (three) and of the sign-in page are retaken with the repository's
  script. `hack/guide-lint.py` reports no error: the new text had none, and
  `5f10ed88` rewords the eleven older places that it reported in chapters 7
  and 8 and on the Upstreams page (five `may`, one `etc.`, four long
  sentences, one long paragraph) without changing what they say.
- **Index (`docs/deploy/README.md`).** A row for the enrolled cluster and a
  task table for twelve more pages the index did not link.

Evidence under `.context/v020-final-20261110/evidence/DOCS-OPS/`. The runs use
`docsrun.py` (in that directory), which drives the built CLI, `dig`, `nsupdate`
and the HTTP API against real processes and redacts secrets:

- `build02.log`: `CGO_ENABLED=0 go build ./cmd/kudurru`, plain and with
  `-tags=licensetest`, both exit 0 (26 s under the lock). `build01.log` is a
  build that was canceled before it got the lock. Both binaries were removed
  after the runs.
- `run-cluster03.log` (license-test binary and the repository's test key,
  2 min 5 s): 46 checks pass, none fail. Three members: activation (committed,
  restart required, `member-identity.json` written, a repeat with the old
  review answers conflict), invitation (a second one with the old review and
  one with an expiry of 15 minutes answer conflict), offline join (refused
  without the encryption key before any change; `primary.committed` and
  `installed`), a token made on the primary accepted by the follower after
  5.0 s, a member started without the shared key ready but not receiving new
  data, promotion eligible 13.6 s after the primary stopped (`missedPulls: 3`,
  10.3 s unreachable), a wrong `--confirm-node` and a reused review refused,
  epoch 2, the third member following the new primary, and the old primary
  showing `mode: follower` and the new data after its restart.
  `run-cluster04-no-license.log`: the same 46 checks pass with the plain
  binary and no license key (see the open points).
- Retained failed runs: `run-cluster01.log` (25 pass, 13 fail) and
  `run-cluster02.log` (38 pass, 5 fail). The checks failed on assumptions of
  the run script, and each failure showed a behavior the page now states or
  the open points list: `kudurru apply` of a zone and
  its record set in one file answers 503 for the second document on a fresh
  node; a token is rejected by a follower until its next pull; a too-distant
  invitation expiry answers 409 conflict, not 400; the API of a just-promoted
  member answers `503 generation_pending` for about a second (0.7 s measured).
- `run-updates02.log` (plain binary, 34 s): 68 checks pass. Stored key through
  `kudurru api`, granted add over UDP and TCP, serial advance, exact-name TXT
  grant, `REFUSED` for an ungranted type, the apex, apex NS, a record set's
  name and type and a name with a record-set CNAME, `REFUSED` unsigned,
  `NOTAUTH(BADKEY)` and `NOTAUTH(BADSIG)` (both shown by nsupdate as "clocks
  are unsynchronized"), the four prerequisite answers, all-or-nothing, delete
  of a whole name, `records list`, `zones export`, absence from `kudurru
  backup`, survival of a record set change and a restart, `SERVFAIL` for a
  signed update to a zone with `update: deny`, and for `update:
  useNetworkACL` and a grant name outside the zone: zone `SERVFAIL`, node not
  ready, `READY False` (`AwaitingCommittedPrimary`), recovery after
  correction. Retained failed run `run-updates01.log` (34 pass, 9 fail): the
  same multi-document apply failure, after which the record-set checks ran
  without a record set.
- `run-totp01.log` (74 s): 23 pass. Setup (key, five-minute lifetime, SHA1,
  six digits, 30 s), confirmation signs the session out, sign-in refused
  without a code and accepted with one, CLI `--password-stdin --totp-stdin`,
  session list and revoke, `kudurru admin reset-password` keeps the
  authenticator, recovery with a host token through `kudurru api`.
- `run-follower-auth01.log`: 14 pass (authenticator setup and session revoke
  answer 503 on a follower, reads and sign-in through the follower work).
  `run-follower-update01.log`: 14 pass (a signed update sent to a follower
  answers `SERVFAIL` and is applied nowhere; the primary answers `NOERROR` and
  the follower serves the record after its pull).
  `run-invitations01.log`: 8 pass (64 invitations issued, the 65th refused with
  `503 unavailable`, also after expiry and after a restart).
  `run-static-update01.log`: a static twin with a file key accepts an update.
  `run-follower-alone01.log`: a follower restarted without its primary is
  ready, answers DNS and becomes eligible.
- `guide-lint01-baseline.log`: `python3 hack/guide-lint.py` reports 14 errors
  at the base. `final02-head.log`: 11 errors at the head of the first round;
  the three that are gone were in chapter 10 and the Access page, and none of
  the 11 is in text of this package. `fix-light01.log`: 0 errors in 67 files
  at the final head `5f10ed88`.
- `links01.log`: 19 changed or new files, 123 relative links and anchors, 0
  broken. `mermaid02.log`: the one new diagram parses with the vendored Mermaid
  bundle (`mermaid01.log` is the version with one more edge, removed because
  no test covers that transition).
- `site01.log`: Hugo build of the docs site with the Makefile's flags
  (`--panicOnWarning`, 165 pages, exit 0) and `htmltest` over the result (162
  documents, passed); the two new pages and their anchors are rendered.
- `final01.log` (under the lock, at `4e2f1851` plus the two sign-in pages, 9 s
  after a wait of 41 minutes for the lock): `make docs-gen` exit 0 with no
  change under `docs-site/content/reference` or `api`, `TestReferenceDrift`
  ok, `make version-check` PASS, `make docs-site` exit 0, `make
  docs-check-links` passed (162 documents). `final02-head.log` (final head
  `7c7c195b`, clean tree): 22 changed Markdown files with 146 relative links
  and anchors, 0 broken; `make version-check` PASS; Hugo build exit 0 and
  `htmltest` passed; the diagram parses; no change under the generated
  reference since the base. Of the five acceptance greps, `cluster activate`,
  `cluster promote`, `rfc2136-` and `kudurru-grant-floors` each appear in a
  `docs/deploy` page; `anchorMode` does not (see the open points).
- `shots01.log` to `shots03.log`: the repository's
  `dashboard/scripts/guide-screenshots.mjs` against the mock dev server with
  Chrome 156. `shots01` showed that the "token created" shot no longer
  contained the token (the two new panels pushed it below the visible area);
  the script now scrolls to the API tokens panel first, and `shots02` shows
  the token and the table. `shots03` retakes the sign-in page. The audit shot
  of the same group is not replaced. `probe-actions01.log`: at 1280 pixels the
  Actions columns of the Browser sessions and Local users tables end at 1467
  and 1607 pixels inside a 987 pixel scroll container and are reached by
  scrolling; the guide says so.
- `run-static-cluster01.log`: a static twin answers `503 unavailable` to
  `cluster enrollment` and `cluster promotion`.

Review fixes (second review round, verdict "accept with fixes"; six commits
on top of `7c7c195b`):

- **Update policies that stop a SQLite zone (`4cd4505e`).** The caution named
  two such policies. A grant name without the final dot and a `types` entry
  that is not a record type do the same. The field table now says that a grant
  name ends with a dot and what a `types` entry is, and the caution is a list
  of four checks.
- **Answers to a signed update (`055630d0`).** On SQLite a signed update from
  a key that the node has but that no grant of the zone names, also `allow`
  without a grant, is answered `SERVFAIL`, not `REFUSED`. The `REFUSED` row is
  limited to a key that a grant names, the `SERVFAIL` row has the SQLite
  case, and a paragraph says to compare the key name with the grants first.
  The cause, as read in the code (`internal/primaryruntime/update.go` and
  `sqliteadapter/adapter.go`): the planner answers `REFUSED`, the admission
  of that refusal runs an empty plan for the same key, which fails for a key
  without a current credential in the zone, and the runtime answers every
  failed admission as `SERVFAIL`. Kubernetes answers the planner's `REFUSED`
  (`cmd/kudurru/kube_update.go`).
- **Sign-in on a follower (`d4af05c0`).** `kudurru login --username` never
  works against a follower in this version: the sign-in is passed on, the
  API token that the CLI saves (`POST /auth/tokens`) is not. The browser
  sign-in works on a follower only while the primary is reachable.
- **Changes through a follower (`350efba0`).** Section 4 no longer explains a
  refusal as a race that "send it again" cures. It lists what a follower
  passes on and examples of what it does not, the meaning of `503 unavailable`
  and `503 completion_unknown`, and how a request with several documents
  behaves through a follower and on the primary.
- **Upgrade order (`4f4c27fb`).** The runbook said to migrate before the Helm
  steps, and the migration page requires the new CRDs, which are Helm step 1.
  The order is now: stop every process, step 1, migrate, step 2.
- **Guide checker (`5f10ed88`).** See above; the proof command of the
  acceptance now passes.

The review's suggestion for section 4 was to state that `kudurru apply` and
`kudurru restore` are not forwarded from a follower. That is the behavior of
this lane's base, and it is a defect that the HA lane had already fixed when
the review ran (`733eb797`, FWD-BOUND, and `29cdf81a` on `wip/v020/ha`): the
30-second request lifetime that the forward requires was installed only for
paths under `/objects/` and `/zones/`, so `POST /apply`, the `/blocklists`,
`/clientgroups` and `/policies` routes and batch deletion were refused with
"the follower's source changed". A page that said "not forwarded" would be
wrong in the release and would contradict the join page of that lane. The
page therefore describes the two lanes together, and the runs below were made
on both: the lane head, and the lane head with the HA lane's Go changes. Two
statements of section 4 hold only with the HA fix (`kudurru apply` through a
follower, and the dashboard's list editors through a follower). **Integrate
`350efba0` together with HA-FIX.** If HA-FIX were not part of the release,
those two statements would have to say "refused".

Evidence for the review fixes, in the same directory. `fix-probe.py` (and
`fix-probe-restore.py`) use the helpers of `docsrun.py`; every check is named
after the statement of the page that it stands for:

- `fix-build01.log` (under the lock, after a wait of 16 min 45 s; 18 s):
  `CGO_ENABLED=0 go build ./cmd/kudurru` twice, both exit 0. The first binary
  is the lane head. The second is the lane head with the 18 non-test Go files
  that `wip/v020/ha` (at `ba1351ac`) changed since the common base `33ee04bc`,
  supplied through `go build -overlay`; the two lanes change disjoint Go
  files (the log checks it). Nothing in the worktree was changed for it: the
  six paths that the log lists as modified are the documentation edits that
  were in progress. Both binaries and the overlay directory were removed
  after the runs.
- `fix-probe-policy02.log` (lane head, one SQLite node, two stored keys,
  1 min 57 s): 60 checks pass, none fail. Answers, four attempts one second
  apart for each case: type not granted `REFUSED`, name not granted `REFUSED`,
  key that no grant of the zone names `SERVFAIL`, `allow` without a grant
  `SERVFAIL`, `update: deny` `SERVFAIL`, unsigned `REFUSED`, a key the node
  does not have `NOTAUTH(BADKEY)`; no record stored, all zones still
  answering. A `tsigKeyRef` with the final dot and a `types` entry
  `TYPE65280` work. Six policy shapes (name without the final dot, `BOGUS`,
  `AXFR`, `OPT`, name outside the zone, `update: useNetworkACL`): the API
  accepts each one, all twelve DNS samples over six seconds are `SERVFAIL`
  with the node not ready, `zones list` shows `READY False` with the reason
  `AwaitingCommittedPrimary`, another zone of the node keeps answering, and
  the zone and readiness return after the correction. In this run the second
  of a series of identical granted updates, sent half a second after the
  first, was answered `SERVFAIL` in both series (2 of 16 updates); in
  `fix-probe-policy01.log` none of 16 was. Retained failed run
  `fix-probe-policy01.log` (55 pass, 1 fail): the probe sent `zones options
  set` with a resourceVersion read before the node wrote the zone's status
  and got `precondition_failed`, so the `OPT` case did not run; the probe now
  reads the options again.
- `fix-probe-follower-ha04.log` (lane head with the HA lane's Go changes, a
  primary and a follower, no license key, 4 min 10 s): 47 checks pass, none
  fail. `kudurru login --username` against the follower exits 1 at "mint a
  CLI token" with the primary up; the browser sign-in answers 200. Passed on:
  `records add`, `records rm`, `PUT` under `/objects/`, `kudurru delete`,
  `PUT /blocklists/...` (the route of the dashboard's editor), `blocking
  disable` and `enable`. `kudurru apply` of one document: 12 of 12 through the
  follower, 1.3 s apart. `kudurru apply` of 250 new record sets in one file:
  the first 122 created, the other 128 refused with `unavailable` (one "cluster
  authority changed", 127 "the follower's source changed"), exactly the 122 on
  the primary; twelve repeats through the follower created nothing more (each
  was cut inside the documents that were already applied); three runs on the
  primary applied 1, 0 and 1 more document and refused the rest with "current
  writable serving generation unavailable". Five new documents on the primary:
  five runs, one document each. Not passed on, each answered 503: `settings
  set`, `POST /auth/tokens`, `POST /iam/users`, `POST /tsigkeys`,
  authenticator setup. With the primary stopped: `PUT` under `/objects/`,
  `records add`, `kudurru apply` and the browser sign-in answer `503
  completion_unknown`, the token context still reads `cluster promotion`, the
  follower answers DNS, and nothing that was sent is on the primary after its
  restart.
- `fix-probe-follower-lanehead04.log` (lane head without the HA changes, 3 min
  8 s): 44 pass, 3 fail. The three are exactly the statements that need the
  HA fix: `PUT /blocklists/...` through the follower (refused, "the follower's
  source changed"), `kudurru apply` of one document through the follower (0 of
  12) and the first documents of the 250-document apply (0 of 250). Everything
  else answers as on the other binary, except that `kudurru apply` with the
  primary stopped says `unavailable` and not `completion_unknown`.
- Retained failed runs of the follower probe, each of which changed the page
  or the probe: `fix-probe-follower-ha01.log` (41 pass, 3 fail) and
  `fix-probe-follower-lanehead01.log` (39 pass, 5 fail) showed that the same
  file against the primary does not complete the apply (one document, then
  "current writable serving generation unavailable"), which the first draft
  of the section had promised, that refused documents carry two messages, and
  that the probe had chosen a setting that the node file locks.
  `fix-probe-follower-ha02.log` (45 pass, 2 fail) and
  `fix-probe-follower-lanehead02.log` (42 pass, 5 fail) showed that 40 repeats
  through the follower apply nothing more after a first request that applied
  222 of 250. `fix-probe-follower-ha03.log` (46 pass, 1 fail) and
  `fix-probe-follower-lanehead03.log` (44 pass, 3 fail) showed that a run on
  the primary can apply no document at all when it arrives while the node
  publishes the previous change, so the page says "one more document at
  most". In the four runs with the HA changes the first request through the
  follower applied 46, 222, 36 and 122 of the 250 documents.
- `fix-probe-multi-lanehead02.log` (56 s) and `fix-probe-multi-ha02.log`
  (54 s), one SQLite node that is not a cluster member: 13 checks pass on each
  binary. A file with 2 new record sets needs 2 runs of `kudurru apply`, with
  10 it needs 10 runs (15 and 12 when repeated without a pause, because a run
  that arrives while the node publishes applies nothing), with 10 new block
  lists 10 runs; a run applies one document and answers "current writable
  serving generation unavailable" for every later one, also for a later
  document that changes nothing. Retained failed runs
  `fix-probe-multi-lanehead01.log` (11 pass, 1 fail) and
  `fix-probe-multi-ha01.log` (13 pass, 1 fail): the checks expected unchanged
  documents after a changed one, and a restore of a node's own backup, to
  report `unchanged`; both are refused or changed, as stated.
- `fix-probe-restore-lanehead01.log` and `fix-probe-restore-ha01.log` (39 s
  each, 10 checks pass): `kudurru restore` of a backup with 9 documents needs
  4 runs on the node it came from (four of its documents count as changed)
  and 9 runs on a new node. In both logs the first DNS query to the new node after
  the last run timed out after two seconds and the next three were answered;
  this was not investigated.
- `fix-light01.log` (final head `5f10ed88`, clean tree): six commits with the
  trailer and subjects of 54 to 67 characters, no forbidden or generated path
  and no Go file touched, 24 changed Markdown files with 159 relative links
  and anchors and 0 broken, `hack/guide-lint.py` 0 errors, no British
  spelling in the changed files, four of the five acceptance greps found
  (`anchorMode` is not, see the open points).
- `fix-gates01.log` (under the lock, final head `5f10ed88`, 9 s after a wait
  of 5 min 52 s): `TestReferenceDrift` ok, `TestCLIReferenceDrift` ok, `make
  version-check` PASS, `python3 hack/guide-lint.py` 0 errors in 67 files,
  `make docs-site` exit 0 (165 pages), `make docs-check-links` passed (162
  documents), worktree clean afterwards.
- The reviewer's logs `review-r2-probe-*.log` show the same behaviors at the
  reviewed head and are the retained evidence of the findings.

Open, stated honestly:

- Not written here: the managed root anchor operator page and the stale
  sentence in `dnssec-validation.md` (G25-OPDOCS item 5; the F28 lane owns
  F28-RFC-4 and F28-GATE-2), `docs/enterprise.md` (item 6, outside the package
  scope), the version-pinned statements and migration comparisons (item 8,
  package DOCS-VERSION), upgrade notes from 0.1.0 beyond the pointer to the
  identity migration (item 10; the QUAL lane is deciding the upgrade
  contract), and the link from the root `README.md` (integrator-owned).
- The follower section of `sqlite-join.md` was left to the HA lane (FWD-DOCS).
  Section 4 of the new page names what a follower passes on in the terms of
  the commands and defers to that section for the routes. Its statements
  about `kudurru apply` and the dashboard's list editors through a follower
  need the HA lane's fix (`733eb797`, `29cdf81a`); see "Review fixes". Later
  HA packages that forward more (settings, identity administration) or change
  the refusals make three places of the page stale: the examples after
  "Every other change answers 503 on a follower", the answer table of
  section 4, and the sentence about `kudurru login --username` in the
  prerequisites.
- For the HA lane, from the runs of this package: the join page of that lane
  says to "run the apply again (committed documents report `unchanged`), or
  send an apply of many documents or a large restore to the primary". In
  these runs neither completes a larger file in one more step: 12 and 40
  repeats through the follower applied nothing more, and the primary applies
  one new or changed document for each request.
- Kubernetes statements (the `rfc2136-<zone>` record set, grant floors, stored
  key reload, `REFUSED` for an uncompilable policy and for a key that no
  grant names) rest on code reading and the existing envtest tests, not on a
  run in this package. The statement about `kudurru backup` and `restore` of
  the `rfc2136-<zone>` record set is from code only. On SQLite, a few answers
  that need a fault to appear were not produced and rest on the code:
  `completion_unknown`, `committed_output_unavailable`, `FORMERR`, `NOTZONE`
  and the DNAME rule. The conflict recovery commands (`cluster resolution`,
  `cluster resolve`) were not run here; the page links to the existing
  procedure, which `TestEnrolledProcessConflictResolution` covers. The CLI
  sign-in through an identity provider was not run (no provider here); its
  text follows the code and the existing dashboard page.
- Product behaviors found by the runs, documented as they are and not
  changed. The first three are for the integrator to route before the
  release; the pages can only describe them:
  - On SQLite one `POST /apply` request (`kudurru apply`, `kudurru restore`)
    applies its first new or changed document and answers `503 unavailable`,
    "current writable serving generation unavailable", for every later
    document, also for one that changes nothing. A file with N new or changed
    objects needs N runs; a backup of 9 objects needed 9 runs of `kudurru
    restore` on a new node. Seen on a node that is not a cluster member and on
    an enrolled primary, with and without the HA lane's changes. The first
    round recorded this as "the multi-document apply failure" for a zone and
    its record set and understated it.
  - With the HA lane's fix, an apply of several documents through a follower
    applies a first part (46, 222, 36 and 122 of 250 in four runs) and
    refuses the rest, and repeating the command through the follower applied
    nothing more in 12 and in 40 repeats.
  - At this lane's base, without the HA lane's fix, a follower refuses
    `POST /apply`, the `/blocklists`, `/clientgroups` and `/policies` routes
    and batch deletion every time with "the follower's source changed". The
    first round's `run-cluster03.log` (lines 193 to 199) had recorded the
    refused apply; the page and this ledger did not say so. Fixed by
    `733eb797` on `wip/v020/ha`.
  - `kudurru login --username` against a follower fails at the CLI token
    although the primary is up (`run-cluster03.log`, lines 117 to 120, also
    left out by the first round). The sign-in step of the command succeeds
    first, so the primary has enrolled a session when the command fails (from
    the order of the CLI's steps; the session list was not read).
  - Four update-policy shapes that the API accepts take a SQLite zone and the
    node's readiness down: an `update` value other than `deny` and `allow`, a
    grant name outside the zone, a grant name without the final dot, and a
    `types` entry that is not a data record type.
  - On SQLite a signed update is answered `SERVFAIL` instead of `REFUSED` for
    a zone that denies updates and for a key that no grant of the zone names.
  - On SQLite 2 of 32 granted updates of the two policy runs were answered
    `SERVFAIL`, each time the second of a series, half a second after the
    first.
  - TSIG error answers that nsupdate reports as a clock problem; `cluster
    status`, `nodes` and the license node count not seeing enrolled members,
    so the node limit is not applied to an enrolled cluster; the lifetime
    limit of 64 invitations with a misleading refusal; a member with the
    wrong encryption key reporting ready; and table action columns outside
    the visible area at a width of 1280 pixels.
- `hack/guide-lint.py` reports no error after `5f10ed88`. The linter is not a
  CI gate.

**Review and verification state at 68a709e2.** The review accepted the package with eight findings, three major. The fixer was partial (seven addressed, two rebutted). The verifier, running real nodes from the lane head with and without the ha lane's forwarding fix, resolved the three major findings and reported **three new problems, all open**: (1) the "Check four things" list in `docs/deploy/dynamic-updates.md` is incomplete: three more accepted policy shapes took a SQLite zone out of service in the same way; (2) after integration `docs/deploy/sqlite-cluster.md` section 4 and `docs/deploy/sqlite-join.md#commands-through-a-follower` contradict each other about repeating an apply through a follower (D375 now states that repeating does not finish a cut file); (3) the product defect that one SQLite `POST /apply` applies one document, which CORE-APPLY-MULTI has since fixed (D378), so the page's warning about it is no longer true. HA-REFUSAL also made two statements of `sqlite-cluster.md` stale (`503 unavailable` is now `503 primary_required`; `cluster status` shows `ROLE follower` with the primary as `LEADER`). DOCS-RECONCILE is to correct these; it has not run. Integrated in the first batch.


### G22-WP0..WP2 SQLite upgrade from 0.1.0 with the built binary — November 10, 2026 (D373)

A data directory written by the released 0.1.0 binary is upgraded by the
final binary and keeps serving what 0.1.0 served; a directory holding an
object this release refuses to write is not upgraded at all. The last step
of the `state.db` migration, inside its transaction, checks every stored
object as the complete serving image will (strict decoding, the current CRD
schema or the kind's own validation, and the SQLite DNSZone rule); one
failure rolls the transaction back, the process exits naming every object,
and `state.db` stays byte for byte as 0.1.0 wrote it. The same DNSZone rule
is write admission on SQLite: a zone of a type no release serves, a query,
transfer or notify policy that does not compile, a primary
`spec.policy.update` other than `deny`/`allow`, the zone name of another
DNSZone, or a new object name for the apex of a deleted zone whose
ZoneLifecycle the primary runtime still keeps is refused as `invalid` with
its field. What one part of an object cannot contribute is left out and
reported instead of failing the whole image, as 0.1.0 did: a record set
that names a forward zone; an endpoint the primary builder refuses (CNAME
next to other data, apex CNAME or apex NS) is withheld from the zone's
commit with its reason; a block list source whose fetch failed before any
body arrived (status error, no digest) compiles empty while the list
reports the failure. The serving publisher logs why a generation is not
published. Every database refuses a newer schema and leaves the file
unchanged; a refused `querylog.db` no longer has its header rewritten. The
review of the first cut found four ways a 0.1.0 directory or an ordinary
0.2 write still left a dead or unready node (dead-URL block list, refused
record endpoint, `update: useNetworkACL`, zone re-created under another
object name); all four are fixed and covered at the built-binary or mounted
level.

Evidence under `.context/v020-final-20261110/evidence/QUAL-SQLITE-UPGRADE/`
(lane head `f7e160a0`; final binary `kudurru 0.2.0-qualfix (f7e160a0)`,
`CGO_ENABLED=0`, stripped, 81,182,880 bytes, sha256 `83982914…`; previous
release `kudurru 0.1.0 (89c59f89)`, sha256 `f049295c…`, built from the tag):

- Built binary, `fix21-e2e-prevbinary.log` (`-race`, 79.2 s, zero skips):
  `TestUpgradeFromV01DataDirectory` passes with all 13 subtests (ready
  2.2 s after the upgrade; `SameAnswers` now also compares a record set
  with an apex CNAME, an apex NS and a CNAME next to an A record, and a
  block list whose URL 0.1.0 never fetched), `TestUpgradeFromV01RefusesLegacyObjects`,
  `TestBuiltBinaryRefusesNewerDatabases` (state.db, local.db, querylog.db;
  now SHA-256 of every database before and after the refused start) and
  `TestBuiltBinaryRefusesNewerAuditSpool` pass. The golden files were
  regenerated by the same tests with the released binary
  (`fix11-e2e-fixtures.log`, `fix14-e2e-legacy.log`).
- Mounted, `fix22-mounted-race.log` (`-race -count=2`, 125.7 s):
  `TestRunServeSQLiteUpgradeWithLegacyPlaceholders` (both subtests) and
  `TestServingFailureLogNamesTheFailureOnce` pass. The ordinary subtest now
  also writes a dead-URL block list and a record set of refused endpoints
  after the upgrade and asserts a later endpoint is served and readiness
  stays 200; deletes a zone, has a zone for its name under another object
  name refused naming the deleted object while DNS and readiness stay up,
  and re-creates it under its name; and has `update: useNetworkACL` refused.
  `fix08-mounted-race.log` is the same pair on the previous golden files.
- Negative control, `fix13-mounted-negative.log`: with the three source
  fixes reverted in the worktree, the mounted test on the regenerated
  golden file fails, "the upgraded server is not ready" after 30 s.
- Unit tests that fail before their fix (logs `fix01`, `fix03`, `fix04`,
  `fix05`, `fix15`): `TestPrepareCompleteServesAFailedFirstFetchEmpty`,
  `TestCompilerWithholdsEndpointsTheBuilderRefuses`, the subtest "the kept
  lifecycle of a deleted zone holds its name" of
  `TestApplyRefusesZonesTheImageCannotCarry`,
  `TestServingErrorsRefuseUpdateModesThePrimaryRuntimeCannotCompile` (parity
  with `currentguard.CompileUpdatePolicy` for every access mode) and the
  refused-open assertion of `TestMetaAndReopen` (querylog).
- Race: `fix06-race-touched.log` (primaryzone, all of primaryruntime
  including network, zone, zonestate, validator, store/sqlite 359 s,
  servingzones), `fix02-listfetcher-after.log` (listfetcher, servingbuild
  217 s), `fix23-race-light.log`, `fix24-race-sqlite.log`, `fix27-race-api.log`
  (`internal/api` 283 s, serving, objects, store/conformance): all ok.
- Scoped golangci-lint v2.15.0: 0 issues (`fix12-lint.log`, `fix25-lint.log`).
  vet, gofmt and the four drift tests: `fix26-drift-vet.log`. No generated
  artifact changed, so there is no chore(gen) commit.
- The reviewer's runs on the first cut (`review-*.log`) cover the earlier
  head: prebuilt-binary e2e twice, the negative control on the base binary,
  race of every touched package, `internal/api`, kube conformance and drift.

Retained failed runs: `fix11-e2e-fixtures.log` (the legacy 0.1.0 node lost
its ops port to another process, `bind: address already in use`; and the
first checksum comparison counted the empty `-wal` files a WAL open
creates), `fix14-e2e-legacy.log` (the comparison then found that a refused
start rewrote the header of `querylog.db`, fixed in `39ea32ae`),
`fix09-mounted-negative.log` (killed while waiting for the heavy lock,
never ran), and `review-race02-touched.log`
(`TestActualNotifyAncestorExpiryInvalidatesBoundChildWithoutNewGeneration`
in the untouched `internal/primaryruntime/network` hit its 3 s deadline
once under load; it passed in `review-race05-network-rerun.log` 4 of 4 and
in `fix06-race-touched.log`; rerun it in isolation if the integrated run
hits it).

Open: a block list source that the fetcher has never attempted (no status
entry) still holds back every candidate until the first attempt is
published; that is the intended first-fetch wait (the fetcher publishes the
failure within one attempt), not exercised with a fetcher that cannot write
the status. A zone name
stays bound to the object name of a deleted zone for as long as its Retired
lifecycle is kept (today: indefinitely); renaming a file-managed zone is
refused at the loader's next pass and needs the old object name or a later
release that lets a retired lifecycle release its apex. A primary zone with
a child zone can answer SERVFAIL, with readiness 503, for up to about a
minute on the first start after the upgrade (`review-lab06`); this is
documented in the runbook and the upgrade notes, not fixed. Update policies
whose grants the primary runtime refuses (an owner outside the zone, an
unknown type) are still not refused at write; by reading
`currentguard.compilePolicy` they keep the zone from being committed as the
update mode did (not run). The 0.1.0 shapes covered are those the
tests write; other combinations are not proven.

**Review and verification state at 68a709e2.** The review found eleven problems, six of them blocker or major. The fixer reported all ten actionable findings fixed in eight commits. The verifier, with the released 0.1.0 binary rebuilt from the tag, resolved four of the six blocker and major findings and left two **unresolved** (a record set that is invalid as a whole still left a zone unserved after an upgrade; the out files overclaimed) and **four new problems** (update grants the primary runtime refuses; a list URL that never answers takes a node down until the fetch ends; `TestUpgradeFromV01DataDirectory` failed 2 of 7 runs; a missing test shape). All six were handed to QUAL-SQLITE-UPGRADE-2, below. This section's changelog draft is replaced by that package's.


### G22-WP1/WP2 SQLite upgrade: the refused-shape classes and a stable built-binary test — November 11, 2026 (D373)

This section follows "G22-WP0..WP2 SQLite upgrade from 0.1.0 with the built
binary" (out/QUAL-SQLITE-UPGRADE/ledger.md) and corrects it where the
verification of that package found it wrong.

The primary zone compiler no longer refuses a zone because of one record
set. Whatever the zone builder refuses of a record set is left out of the
zone's commit and reported, and the rest of the zone is committed: a
record set that is invalid as a whole (an endpoint that parses in no zone,
or two endpoints with one name, type and set identifier) is projected
`withheld` with its first error, which used to return `malformed desired
source` and keep a zone without a committed head (the first start after
an upgrade from 0.1.0) from ever being served; endpoints outside the zone,
apex CNAME or NS, a CNAME next to other data, data below a DNAME and
duplicates of an earlier record set were already withheld or `duplicated`;
an endpoint the aggregate codec cannot hold is now refused before the
builder takes it. Review found one shape the builder took and the
committed primary (`BuildCommittedPrimary`, the runtime's prebuild before
admission) refused: two DNAME endpoints for one name with different set
identifiers, in one record set or two, which merged into a two-record
DNAME RRset and so kept the zone uncommitted after an upgrade and frozen on
a running node. The builder now keeps the first target and reports the
second, as it already did for CNAME, so it refuses everything the
committed primary refuses; a pairwise test runs every combination of a
small alphabet through the runtime's prebuild. A record set that is changed into one invalid as a whole
now leaves the zone, as on Kubernetes and in 0.1.0, instead of freezing
every later change of the zone. Update grants the primary runtime cannot
compile (an owner outside the zone or with an inner `*`, a type that does
not exist or that an update cannot carry, an empty or root key name, the
count and length bounds; also for `update: deny`, since the runtime
compiles every entry; for `allow` the workspace bound as if every key were
present) are refused on SQLite at write and at upgrade, by the same code
the runtime uses (`dynamicupdate.PreflightGrantSpecs`/`CompileGrantSpec`,
called by both `currentguard` and `zone.ServingErrors`). A block list
source for which no body was ever published (no status entry, or one
without a digest, with or without an error) compiles empty, so a URL that
accepts the connection and never answers no longer holds back every
serving generation for the length of its download attempts; a published
digest whose body this node lacks still waits. The committed-primary
status writer keeps the serial and content hash an earlier release stamped
while a zone has no committed head, so the first commit after an upgrade
can continue the 0.1.0 serial; before, the placeholder sometimes erased
them first and the zone got a new serial. The built-binary upgrade test
waits after each start until readiness has held and `state.db` has been
unchanged for a second, and the mounted test until readiness has held for
two seconds, because the node still commits zone lifecycles and DNSZone
status after readiness (recorded up to 0.45 s after readiness, `e2e03`);
each write is made once. That is a test wait, not a product change: the
503 it avoids is open under G21 (last paragraph).

Evidence under `.context/v020-final-20261110/evidence/QUAL-SQLITE-UPGRADE-2/`
(product head `f1a3ab56` for the built-binary runs, `57145be3`/`c90b0109`
for the rest; final binary `kudurru 0.2.0-qual2 (f1a3ab56)`,
`CGO_ENABLED=0`, stripped, 81,199,264 bytes, sha256 `b30e59b3…`, built
while only test files were uncommitted; previous release
`kudurru 0.1.0 (89c59f89)`, sha256 `f049295c…`, built from the tag,
`build01-v010.log`):

- Built binary, prebuilt, `-race` (`e2e07-all-x1.log`, 66.4 s, zero
  skips): `TestUpgradeFromV01DataDirectory` (13 subtests),
  `TestUpgradeFromV01RefusesLegacyObjects` (now 8 refused objects,
  including both grant zones), `TestBuiltBinaryRefusesNewerDatabases`
  (3 subtests) and `TestBuiltBinaryRefusesNewerAuditSpool` pass.
  `e2e06-upgrade-x10.log`: `TestUpgradeFromV01DataDirectory` passes 10 of
  10 (257 s; settled 1.1 to 2.4 s after readiness; load average 2.8 to
  4.0). `SameAnswers` now also compares the five record sets 0.1.0
  reported Invalid (`bad-a`, `twice`, `bogus`, `mail`, and `pair` with a
  CNAME and an A record in one record set) with what 0.1.0 answered.
- The golden `state.db` files were regenerated by those tests with the
  released binary (`e2e01-fixtures-probe.log`) and pass
  `TestOpenReleasedV01Golden` and `TestOpenReleasedV01LegacyGoldenIsRefused`
  (`unit03-golden.log`).
- Mounted, `-race` (`mounted01-race.log`, 109.6 s; `mounted03-race-timing.log`,
  124.3 s with step timings): `TestRunServeSQLiteUpgradeWithLegacyPlaceholders`
  (both subtests) and `TestServingFailureLogNamesTheFailureOnce` pass. The
  ordinary subtest now also writes the five invalid record sets after the
  upgrade, asserts a later record is served next to them, readiness stays
  200 and four of them are reported Ready=False reason Invalid on the
  record set; has update grants outside the zone (`deny`) and of an
  unknown type (`allow`) refused as `invalid` naming
  `spec.policy.updatePolicies[0]`; and adds a block list whose URL accepts
  the connection and never answers, then asserts the fetcher asked it, a
  later record is served, readiness is 200, `GET /zones` answers and the
  source has no digest.
- Negative control (`mounted02-negative-overlay.log`): with
  `primaryzone/compiler.go`, `listfetcher/prepare.go`, `zone/serving.go`
  and `currentguard/policy.go` replaced by their `addc912d` versions
  through `-overlay` (worktree untouched), the legacy subtest gets 6
  refused objects instead of 8 and the ordinary subtest fails with "the
  upgraded server is not ready" after 30 s.
- Unit tests that fail before their fix: `TestCompilerWithholdsEveryRefusalOfTheBuilder`
  (33 cases in source order: 25 record-set-level refusals, outside the
  zone, and the builder's apex, duplicate, CNAME, DNAME refusals, each
  with and without a committed head; 25 fail before, `unit01-compiler-before.log`),
  `TestPrimaryStatusPlaceholderKeepsTheSerialOfAnEarlierRelease`
  (`unit04`), `TestUndownloadedSourceDoesNotHoldBackTheCandidate` (`unit06`).
  `TestServingErrorsRefuseUpdateGrantsThePrimaryRuntimeCannotCompile`
  checks parity with `currentguard.CompileUpdatePolicy` for 20 grant shapes
  in both modes, with a keyring holding every named key.
- Race: `race01`/`race02` (zone, primaryzone, dynamicupdate,
  currentguard, listfetcher), `race03-sqlite-validator-servingzones.log`
  (`internal/store/sqlite` 359.6 s, store/validator and servingzones ok;
  servingbuild failed, see below), `race04-servingbuild.log`
  (`internal/servingbuild` 224.6 s, ok), `race05-light.log` (zone,
  primaryzone, dynamicupdate, all of `internal/primaryruntime` including
  network 79.8 s, listfetcher: ok), `root01-lists-blocking.log`
  (`cmd/kudurru -run 'Block|List|Download|Fetch|StaticTwins|Upgrade|Primary'`
  with envtest assets: 49 top-level passes, no failures or skips, 327.6 s).
- Scoped golangci-lint v2.15.0 over zone, primaryzone, dynamicupdate,
  currentguard, listfetcher, store/sqlite, servingbuild, cmd/kudurru and
  test/e2e: 0 issues (`lint02.log`). vet and gofmt clean; the four drift
  tests pass (`drift01.log`). No generated artifact changed, so there is no
  chore(gen) commit.

Retained failed runs: `e2e05-upgrade-x8.log` (one of eight runs failed
`SerialsContinue`: `file.test.` got a new serial after the upgrade; the
committed-primary status placeholder had erased the 0.1.0 serial before
the first commit read it, fixed in `f1a3ab56`); `race03` (servingbuild
`TestMissingBodyRetainsWholeBundleAndGatesHTTP` expected a never-fetched
URL to hold back the candidate, which D373 now serves empty; the test now
uses a published digest whose body is missing, `57145be3`); `lint01.log`
(an unused handler parameter in the new mounted test and a gosec G703
finding in `test/e2e/kube_cluster_test.go`, from the earlier
QUAL-KUBE-UPGRADE commit, both fixed in `c90b0109`);
`mounted02-negative-overlay.log` (the intended failure);
`unit01-compiler-before.log`, `unit04-…-before.log`, `unit06-…-before.log`
(intended failures); `mounted00-stopped-waiting-for-lock.log` and
`race03-stopped-before-start.log` are empty (stopped while waiting for the
heavy lock; never ran). `e2e02` and `e2e03` ran with a temporary probe that
lists the object writes after readiness (not committed).

Two existing assertions changed meaning, as the integrator directed:
`TestCompilerDynamicCollisionRequiresExplicitTakeoverAndExpiryKeepsReservation`
expected an invalid replacement to fail the whole projection and now
expects it withheld while the rest of the zone commits;
`TestPrepareCompletePinsPublishedDigestsWithoutPartialFallback` expected an
empty digest to hold back the candidate and now expects the source
compiled empty without its cached body.

Review fixes (head `1604dac7`; evidence `fix*.log`; load average 3 to 6):

- `TestCompilerCommitsEveryPairOfEndpoints` (new, `internal/primaryzone`):
  every pair over the names `example.`, `a.example.`, `x.a.example.`, the
  types A, TXT, CNAME, DNAME and NS, set identifiers "" and `s`, and two
  targets, in one record set or two, without a head and next to one,
  through `Compiler.Project`, `zonestate.Prepare`, the runtime's
  `Materializer.Prepare` with `BuildCommitted`, and a lookup of the rest of
  the zone. Before the fix 12 of 5,310 combinations stopped the zone, all
  `invalid DNAME RRset` (`fix06-pairs-before-trimmed.log`, the first full
  form 32 of 14,160 in `fix01`); after it none (`fix07`, 7.5 s; 66 s under
  `-race`, `fix12`). `TestCompilerWithholdsEveryRefusalOfTheBuilder` gains
  two DNAME cases (35 cases; both fail before, `fix05`), and
  `TestBuildRejectsConflicts` two plus `TestBuildKeepsOneDNAMEAcrossSetIdentifiers`
  in `internal/zone` (fail before, `fix03`; pass, `fix04`).
- Race: `internal/zone`, `internal/primaryzone`, `internal/listfetcher` ok
  (`fix12`); golden tests ok on the regenerated files (`fix17`).
- Built binary (`kudurru 0.2.0-qual2`, `CGO_ENABLED=0`; 0.1.0 rebuilt from
  the tag, sha256 `f049295c…`, `fix14`): the fixture gains a zone
  `dname.test.` with the DNAME shape; `fix16-e2e-write-fixtures.log`
  regenerated both golden files and passed both upgrade tests with the new
  `OneDNAMETargetKept` subtest (14 subtests). `fix15` is a retained failure:
  with the shape in `api.test.`, `SerialsContinue` correctly saw a new
  serial (the content changed), so the shape moved to its own zone.
  `fix23-e2e-upgrade-x3.log` (head `1604dac7`, load 5.5 at the start,
  165 s): `TestUpgradeFromV01DataDirectory` (14 subtests, 42 subtest passes),
  `TestUpgradeFromV01RefusesLegacyObjects` and
  `TestBuiltBinaryRefusesNewerDatabases` pass 3 of 3, settled 1.8 to 2.5 s
  after readiness; `TestBuiltBinaryRefusesNewerAuditSpool` skipped there
  (the script did not export `KUBEBUILDER_ASSETS`) and passes in
  `fix24-e2e-auditspool.log`.
- Mounted, `-race` (`fix20-mounted-race.log`, 139 s): both subtests pass;
  the silent-URL step now samples readiness from the list's write until a
  later record is answered and bounds the longest not-ready stretch (30 s)
  and the answer (60 s), far below the fetcher's 5-minute timeout. Observed:
  answered 16.8 s after the write, longest not-ready stretch 6.5 s, five
  stretches of 2 to 6.5 s, each `serving_generation_pending` then
  `preparing`, i.e. the publication of a commit. Negative control with only
  `internal/listfetcher/prepare.go` at `addc912d` through `-overlay`
  (`fix21-mounted-negative-listfetcher.log`, also `fix09` on the earlier
  test text): the earlier steps pass and the silent-URL step fails, the
  record set POST answered `503 generation_pending` for its whole 20 s.
  Retained failures: `fix10` and `fix13` (a first form that required
  readiness never to leave 200; it does at every publication, see below),
  `fix18`/`fix19` (readiness 503 at a point check after the regenerated
  fixture's own post-start commits, before the settle wait was added).
- Probe (not committed; text in `fix08-probe-test.go.txt`): from readiness
  of the upgraded golden directory, a settings PATCH every 20 ms and a UDP
  and TCP query every 10 ms for 4 s, three starts each (`fix08`, `fix11`).
  The PATCH right after login (about 2 s after readiness, the login itself
  being retried over 503s) was answered `503 unavailable` "current writable
  serving generation unavailable" in 2 of 3 starts in both runs, and a later
  PATCH `503 generation_pending` while the previous PATCH's generation was
  being published, in every start. TCP queries from the probe mostly got
  EOF (259 to 301 of about 350 per start, `fix11`) and UDP timed out 4 times;
  the cause of the TCP EOFs at that query rate was not established.
- Scoped golangci-lint v2.15.0 over zone, primaryzone, listfetcher,
  store/sqlite, cmd/kudurru and test/e2e: 0 issues (`fix22-lint.log`).
  No generated artifact changed.

Open: two items stay with the integrator. (1) G21, not met by this package:
every commit makes readiness 503 and the API answer `503 generation_pending`
until its generation is published (2 to 6.5 s under the race detector at
load 4 to 6), and a write whose command overlaps a publication is answered
`503 unavailable` without effect. After an upgrade the node's own lifecycle
and status commits do this for a moment after readiness first turns 200,
and any write does it to the writes that follow it. The tests wait for a
quiet node instead of repeating a request; the product behavior is
unchanged, because closing it is a change of the serving publisher's
current-authority rule (re-running an overlapped write would be a replay,
and holding readiness at the start would leave every later publication
as it is). (2) The verifier's TCP EOF was not tied to a cause: the probe
saw EOFs at a high query rate, which may be the listener's own limits.
Earlier open notes, still true: the cause of the verification failures is product behavior that stays: for about half a second after
readiness, longer under load, a node commits its own zone lifecycles and
DNSZone status; an API write whose transaction overlaps one is answered
503 `unavailable` without effect, and a query in flight at a generation
switch is dropped. An automation that writes the moment readiness turns
200 can see that 503 and must repeat the request; holding readiness until
those commits end is a serving-core change outside G22. A list source whose first download is still running is served empty with
no error reported until the attempts end. A source whose
published digest this node's cache lacks (a named source whose URL
changed, a lost cache) still holds back every candidate until its body
arrives, and a hanging URL also delays the status of other lists until
the fetcher's pass ends. The mounted test's ordinary subtest takes about
two minutes under the race detector. A zone name stays bound to the
object name of a deleted zone while its Retired lifecycle is kept, and a
primary zone with a child zone can answer SERVFAIL for up to about a
minute on the first start after the upgrade (both as recorded before).
The 0.1.0 shapes covered are those the tests write, the builder refusals
enumerated above and the pairwise alphabet; zone capacity limits (`zonestate.Limits`,
dynamic-update ownership conflicts) are not record-set refusals and are
not covered.

**Review and verification state at 68a709e2.** The reviewer **rejected** the package (five findings, three major). The fixer was partial: two findings fixed in the product (the DNAME and set-identifier blocker; the silent-URL step), two rebutted. The verifier resolved four of five and left **finding 2 (major) unresolved**: the `503` an API write can get just after readiness is absorbed by the test (`test/e2e/upgrade_test.go:113`) instead of being prevented or documented as a contract, and reported two new problems: `addParsed` now also resets a merged CNAME's TTL to the kept record's TTL (a 0.1.0 zone of that shape serves a different TTL and gets a new serial on upgrade, which no changelog line or test states), and the silent-URL negative control is tripped by the POST helper's 20-second retry window rather than the new bounds. In the composed pre-push run on the merged tree `TestRunServeSQLiteUpgradeWithLegacyPlaceholders` failed with `temp.test. served = false after 30s` after `served while a list URL is silent`, and passed when run alone; it is not shown stable. QUAL-UPGRADE-OPEN is to close these; it has not run.


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

**Review and verification state at 68a709e2.** The review accepted the test-only package with six findings. The fixer resolved four in three commits; the verifier confirmed all four resolved and reported one nit, open: the three-pass fencing loop in `test/e2e/kube_cluster_test.go` counts a failed `/healthz` read as "not leader". Recorded product behavior, not changed: an unpinned Kubernetes node started while the API server is unreachable exits and crash-loops, which departs from G22-WP4 item 2 (D373 records it as a follow-up). `TestChartUpgradeFromV01` from this package (`e63bdcf2`) fails in hosted CI on `68a709e2` because the hosted checkout has no `v0.1.0` tag (CI-RED is to fix it; not run).


### CORE-APPLY-MULTI one request applies every document on SQLite — November 11, 2026 (D378)

On the complete SQLite runtime a request that carries several items applied
only its first new or changed item. `POST /apply` and `POST
/{kind}:batchDelete` run one current command for each item, a current command
is admitted only while the request's original lease is the current
publication, and the first commit retires that lease. Every later item
answered `unavailable` ("current writable serving generation unavailable"): a
three-document `kudurru apply`, `kudurru apply -f a -f b -f c`, a restore into
a new node and the dashboard's multi-delete each needed one run for each
object. A standalone node and the primary of a cluster were affected alike.

One request now applies every item. Both routes declare an item sequence
(`api.Options.ObjectItems`). Root gives the request one command chain of the
provider coordinator and binds it to the elected writer episode of its first
item. Until one of the request's commands has completed, each item is admitted
on the original lease exactly as a single write is. The first completed
command anchors the chain: on its own commit receipt, which the Store now
issues in the command's finalizer under the writer lock
(`sqlite.CommandReceipt`), or on the coordinator's current mark when the
command changed nothing. Each later item is admitted under the publication
gate only while the coordinator's mark of the last command that changed
canonical state is still the chain's anchor, compared by identity, and while
the Store's authority is the lease's own authority advanced only in its
installed revision (`GenerationToken.AdvancedTo`,
`Publisher.CheckWriterAdvanced`). Every item still reads the raw credential,
grants, ownership, preconditions and limits again in its own transaction.

A foreign commit is a state-changing command of anyone else through the
coordinator: a public API mutation under any credential, including a sign-in,
a write forwarded from a follower, an operator-started integration operation, a
migration step or the unblock-request sweep. A command whose SQL outcome is
unknown counts as one. After a foreign commit every later item of the request
answers `unavailable` ("another change was committed on this node while the
request ran; this item was not applied; review current state before a new
decision"), is not applied and is not tried again; earlier items stay
committed. Writers outside the coordinator, which take only the Store lock or
the primary pin, are not detected and do not stop a request: the node's own
follow-up work (status reconciliation, the zone lifecycle and its heads, list
fetch status, integration reconciliation, the RoleBinding objects source, the
login-lane request charge and provider authority admission) and two writers
that act for another party, RFC 2136 updates under a TSIG key and commands of
the local admin socket. D378 accepts this: an item reads everything it is
admitted on again in its own transaction. Replacements of node-local dependency
installations between two items do not stop a request either. The request's
one lifetime bounds all items, and an item reached after it ended answers
`unavailable` with that cause instead of "current credential is invalid or
expired". The single-command check (`CheckWriter`, `CheckWriterLease`) is
unchanged for every other route and for the first item.

Every commit retires the publication that local DNS answers and every other
API request are bound to, until the node has published again, and the items
of one request commit back to back. Unpaced, one request kept the node without
a current publication for its whole duration (review probe: 27.5 s without a
correct local answer during one 300-document apply on a loaded machine, and
`503 generation_pending` for a second CLI). Once a request's items have run
for two seconds since the node last had a current publication for it, the next
item now first waits, inside the request's lifetime and writer episode and for
at most two seconds, until the publisher is current again
(`Publisher.WaitTerm`), and then commits nothing for 200 ms. The wait captures
no lease; the chain's compare still decides, so a command of another caller
committed in the pause stops every later item. A wait that reaches its bound
goes on without the publication and without the rest, so the pace makes a
request at most about twice as slow and cannot by itself run it out of its
lifetime. `kudurru apply` and `kudurru restore` now default
`--request-timeout` to the server's five minutes. The dashboard's multi-select
delete sends one batch delete per namespace one after another instead of all
at once, because of two concurrent multi-item requests one is refused as a
whole.

A follower is unchanged: it forwards each item as its own member command,
never uses the chain and stops when its installed source changes (the
forwarding lane's decision, D375, with two statements of its reasoning
corrected in `out/CORE-APPLY-MULTI/decision-D375.md`). The Kubernetes driver
has no such gate and was not affected; it now has the same test.
`docs/deploy/sqlite-cluster.md` section 4 and `docs/deploy/sqlite-join.md` no
longer contradict each other about a file that is cut through a follower:
repeating it there does not finish it, because the primary commits a
deduplication record for every forwarded command, also for an unchanged
document. `docs/deploy/api-contracts.md` and
`docs/architecture/configuration-and-store.md` ("Requests with several items",
with a diagram) describe the rule.

Commits on `wip/v020/core`: `71c3249c` (Store receipt), `3111477d` (advanced
authority compare), `2b351dea` (coordinator chain), `14608688` (the fix and
its root and mounted tests), `94ab1e7f` (Kubernetes and two-process tests),
`4ef8f537` and `da930f26` (documentation), `adb5df10` (forwarded-write tests
and the quiet wait of the mounted tests), and the generated commit `a3af56bf`. Review fixes: `9c38431c` (dashboard
batch deletes one after another), `663e6150` (the pace and its root and
mounted tests), `4e40af6e` (five-minute CLI default for apply and restore),
`af6347b3` (documentation), the generated commit `4e7e7b97` (CLI
reference) and `02089768` (a lint fix in a test, source only, committed after
the generated commit).

Evidence under `.context/v020-final-20261110/evidence/CORE-APPLY-MULTI/`:

- `race01-repro-base` (retained failure, the defect on the unmodified base):
  `repro-base.sh` returns every changed Go file to commit `b4987f32` with a
  build overlay and adds only the reproduction test.
  `TestRunServeSQLiteMultiDocumentApplyCLI` on a mounted `runServe` with the
  real CLI fails in 13.16 s: `dnsrecordset/default/x0 created`, then `x1` and
  `x2` refused with "current writable serving generation unavailable".
- `race02-first-fix` (retained failure of the first design): the mounted CLI
  apply passes, and `TestRunServeSQLiteMultiFileApplyAndRestoreIntoNewNode`
  fails: the restore into a new node creates `dnszone/multi` and refuses the
  other seven documents. That version still required the original lease's
  dependency installations for later items. `race04-diag-zone-first` (an
  overlay that prints the refusing check, not committed) shows the cause: the
  provider resource set of the retired lease is replaced by the node while
  the request runs ("provider not current"), which cut the restore at its
  fifth of eight documents in that run. Later items no longer compare those
  installations; each command pins the current ones.
- `race03-libraries`: default race of `internal/store/sqlite` (422.9 s),
  `internal/auth/providercoordinator` (70.8 s), `internal/serving` (6.3 s) and
  `internal/servinghttp` (13.4 s): all pass. They include
  `TestCommandReceiptStatesExactDisposition`,
  `TestCommandReceiptServesOneCommand`,
  `TestServingTokenAdvancedToComparesEveryOtherFieldExactly`,
  `TestHTTPWriterAdvancedKeepsOriginalAuthorityAcrossCommits`,
  `TestChainFollowsOwnReceiptsAndRefusesAForeignCommand`,
  `TestChainAnchorsOnItsFirstCompletedCommand`,
  `TestChainRefusesAnotherCoordinatorAndReentry` and
  `TestChainTreatsOutputAndMemberCommandsAsForeign`. In the same log
  `internal/api` fails only `TestOpenAPIDrift`; `drift01-base` shows that this
  drift and the `TestReferenceDrift` failure exist on the unmodified base.
  `race08-api`: default race of `internal/api` after regeneration, 267.0 s,
  pass.
- `race05-root` (125.4 s) and `race05-e2e` (275.2 s), first complete pass:
  the mounted CLI apply (39.0 s), the mounted multi-file apply and restore
  (56.9 s), the Kubernetes test (7.9 s), eight root item tests, and the
  two-process tests `TestEnrolledClusterProcessesPartitionCatchupAndRestart`
  (121.6 s) and `TestEnrolledClusterProcessesPromoteAndRejoin` (125.5 s).
- `race06-gen`, `race06-regression`, `race06-drift`, `race06-lint`:
  regeneration of the artifacts that were stale at the base (`make openapi`,
  `make client-go`, `bun run gen:api`, `make docs-gen`: five files), then the
  root regression selection in default race: 174 tests pass, none fails, none
  skips, 1230.2 s. It covers the current-command, object, zone, domain,
  source-entry, unblock, blocking, migration, adoption, cutover, member-command
  and member-forward tests, `TestServingHTTP*`, five existing mounted SQLite
  command tests, the real-router and generated-transport CLI tests,
  `TestCLIReferenceDrift`, `TestCompleteCommandRouteFamiliesAreReviewed` and
  the new tests. `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass after regeneration. The lint run reported two
  gosec findings in the new tests (a test-owned file read and an integer
  conversion), which are fixed.
- `race07-root` (268.5 s), `race07-e2e` (260.9 s) and `race07-lint`, the
  final verification of the content that is committed (`da930f26` plus the
  test changes of `adb5df10` and the regenerated files, all committed
  unchanged afterwards): the 13 new tests of the root package and
  `TestCLIReferenceDrift`, each twice, 28 passes and no skip: nine
  `TestServingItems*` tests, `TestRootMemberUnchangedForwardedWriteStillCommits`,
  the mounted CLI apply (35.0 s and 38.3 s), the mounted multi-file apply and
  restore (58.5 s and 67.0 s) and the Kubernetes test (7.3 s and 7.6 s). No
  run needed the repeat of a request that applied nothing. The two-process
  tests pass in 117.5 s and 123.9 s. Scoped golangci-lint v2.15.0 over the
  seven touched package trees: 0 issues.
- `final01-clean-tree`: on the clean committed tree `a3af56bf`: `gofmt -l`
  empty for every changed Go file, `go build ./...`, `CGO_ENABLED=0 go build
  ./cmd/kudurru`, `go vet` of the six library and test packages, and
  `TestOpenAPIDrift`, `TestReferenceDrift` and `TestAggregatedRolesDrift` pass.
  `go.mod` and `go.sum` are unchanged; there is no new dependency.
  `race09-duration-compare` also runs `TestCLIReferenceDrift` on that tree.
- `race09-duration-compare` and `race10-duration-compare`:
  `TestServingZoneCommandActualHTTPCompleteSQLite`, an existing test that does
  not use the new path, took 183.9 s in the regression run. The two logs run
  it from a binary of the unmodified base (build overlay) and a binary of the
  committed tree, alternating, on the loaded machine (load average 5 to 11 on
  8 cores). `race09` (retained failure): base 167 s and 178 s, tree 112 s, then
  the tree's second run fails after its last request waited the test's whole
  two-minute budget for publication; that log kept only the last lines.
  `race10`, three more rounds with full output: tree 176.6 s, 218.8 s and
  67.1 s, base 199.8 s, 218.5 s and 232.1 s, all six pass, and the longest
  wait at a single request is 80 s for the tree and 80 s for the base. The
  test's duration follows the machine's load on both trees and its budget is
  within reach on this machine without this change.
- Review fixes (`fix-*`). `fix-01-before` (retained failure): with the pace
  call disabled, `TestServingItemsLetTheNodePublishBetweenItems` fails (item
  1 began without a current publication) and
  `TestServingItemsPaceEndsWithTheRequestLifetime` fails (items applied after
  the lifetime). `fix-03-mounted-long` and `fix-08-race-items-mounted`
  (retained failures of an unbounded wait): under the race detector one
  apply of 300 and then of 120 documents did not answer within five minutes;
  `fix-04-diag-pace` (diagnostic overlay) shows single waits of 1.9 s to
  9.5 s for a publication under that load. `fix-06-norace-paced` and
  `fix-07-norace-unpaced` (no race detector, 150 documents): with a
  half-second budget 26.3 s and the longest time without a local answer
  3.3 s; unpaced 3.9 s, no correct local answer and no read during the whole
  request (the new mounted test fails). `fix-15-bound-before` (retained
  failure): with the wait's bound disabled `TestServingItemsPaceWaitIsBounded`
  fails after 120 s. `fix-09-race-bounded-wait`: with the bound, race, the
  mounted 120-document apply passes in 92.8 s (10 local answers, longest gap
  84.0 s, 3 reads ok and 43 refused) and the three pace tests pass.
  `fix-10-norace-bounded` (no race detector, twice): 120 documents in 4.0 s
  and 4.9 s, longest gap 4.0 s and 2.4 s. `fix-05b-dashboard`: dashboard
  typecheck and lint pass for the sequential batch deletes. `fix-11-docs-gen`:
  `make docs-gen` changes only `docs-site/content/reference/cli.md`.
  `fix-12-race-final` (race, on `4e7e7b97`): 19 tests pass, none fails or
  skips, 241.6 s: twelve `TestServingItems*` tests,
  `TestRootMemberUnchangedForwardedWriteStillCommits`, the three mounted
  SQLite tests (the long apply 115.9 s: 120 documents in 82.5 s, 14 local
  answers, longest gap 73.6 s), the Kubernetes test,
  `TestApplyAndRestoreDefaultToTheRequestLifetime` and
  `TestCLIReferenceDrift`. `fix-13-lint` found one revive issue in a new test,
  fixed in `02089768`; `fix-16-lint`, the rerun on `02089768`: 0 issues. `fix-14-drift` (on
  `02089768`): `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass, `gofmt -l` is empty for every changed Go
  file, `CGO_ENABLED=0 go build ./cmd/kudurru` passes, `go.mod` and `go.sum`
  are unchanged.
- `mermaid01`: the 29 Mermaid blocks of
  `docs/architecture/configuration-and-store.md`, including the new one, parse
  with the vendored bundle (the architecture lane's checker).

The acceptance tests, by criterion: (a) `TestRunServeSQLiteMultiDocumentApplyCLI`
(mounted `runServe`, real router, real CLI: three created in one request,
served over DNS, three `unchanged` on the second run); (b)
`TestRunServeSQLiteMultiFileApplyAndRestoreIntoNewNode` (`apply -f a -f b -f
c`, then `backup` and a `restore` of eight documents into a new node in one
run, served over DNS, eight `unchanged` on the second run); (c) the batch
delete in (a), `TestServingItemsBatchDeleteFollowsOwnCommits` and the
two-process test; (d) `TestServingItemsForeignCommandRefusesLaterItems`
(another administrator's write through the real router),
`TestServingItemsForwardedFollowerWriteRefusesLaterItems` (a member command
over the production member listener) and
`TestServingItemsFirstItemKeepsTheOriginalSource`, with
`TestServingItemsApplyFollowsOwnCommitsAcrossFollowUpWrites` and
`TestServingItemsReplacedDependencyKeepsLaterItems` for the writes that do not
count; each asserts that a refused item is not stored and that every item ran
its command once; (e) `TestServingItemsOriginalLifetimeBoundsEveryItem`; (f)
`TestServingItemsRevokedOrDowngradedCredentialStopsLaterItems`; (g) the
two-process test applies five new record sets in one request on the primary
while a follower pulls, repeats them for five `unchanged` results and deletes
them in one batch delete, next to the forwarding lane's existing two-document
applies and batch delete through the follower; (h)
`TestRunServeKubeMultiDocumentApplyAndBatchDelete` on a mounted Kubernetes
`runServe` with envtest.

What remains open. The root cause of the pause is outside this package: every
commit withholds local answers and refuses other requests until the next
publication (`cmd/kudurru/serving_update_output.go`), also for a run of single
writes on the unmodified base (review probe: 60 single applies left 62 of 75
queries unanswered); it belongs to the lane that owns the retained-response
guard. The pace only bounds it: on a node that needs more than two seconds to
publish (a large data set or a loaded machine) the waits end at their bound
and local answers stay withheld for most of the request (84 s of a 93 s
request under the race detector), and other callers on the node are answered
`generation_pending` while the items commit. A request is still held to the
publication it began on until its first item, as every single write is: when
the node's own work lands between the request's capture and that item, the
whole request is refused with "current writable serving generation
unavailable" before anything is applied, and it has to be sent again; of two
multi-item requests sent at once one usually ends this way (the dashboard now
sends its per-namespace requests one after another). Writers outside the
coordinator are not detected, including RFC 2136 updates and the admin socket,
which act for someone else (D378 accepts this), and no test puts one of them
between two items. The mounted CLI tests wait for a quiet node and repeat only
a request that applied nothing, up to five times, and now log each repeat as
`repeated=<n>` (0 in every fix run). Through a follower a file that does not
fit between two pulls is still cut, and repeating it there does not finish
it; chaining a follower on the primary's receipts needs receipts and
preconditions in the member protocol and was not attempted. Criterion (g) is
met by the forwarding lane's two-document applies and batch delete through
the follower in the two-process test plus this package's five-document
requests on the primary; no process-level test sends a file longer than one
pull interval through a follower and asserts the cut. The foreign-commit
refusal is proven in the root fixture only. The mark for a command whose SQL
outcome is unknown has no test, because no seam produces a failed commit.
`TestServingZoneCommandActualHTTPCompleteSQLite` failed once in six runs of
this lane's tree on the loaded machine (`race09`) by running out of its own
two-minute wait for publication; the base binary waits up to 80 s at a single
request under the same load, so the test is sensitive to load, and it is not
changed here. Watch it in the integrator's full run; if it fails there and not
on the base, bisect across `2b351dea`.

**Review and verification state at 68a709e2.** The review accepted the fix with eight findings, one major (a long request withheld local DNS answers and refused other callers for its whole duration). The fixer was partial: it bounded the major finding with the pace recorded in D378 and fixed four minor findings. The first verifier stopped when the credits ran out; the second resolved six findings and left **two unresolved**: finding 1 (major) as a residual under load (the 120-document gap drops to 2.3 s unloaded but stays about 63 s under race load) and finding 6 (minor; no follower cut test, no counted repeat), plus **two new problems**: `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` checks reads only when the apply runs longer than 10 s and then for at least one success, and the pauses make a long request more likely to be cut by other callers' commands (documented, not measured). On the merged tree the same test logged `apply of 120 documents took 1m29s: 11 correct DNS answers, longest time without one 1m20s, 2 reads ok, 41 reads failed` (`evidence/INTEGRATE-2/rerun-two.log`; in the composed run it failed with 0 reads ok). The rule that DNS keeps answering from last-good data is therefore not met during a long multi-item request; CORE-APPLY-OPEN is to fix it and has not run.


### FWD-REFUSAL, FWD-NODELOCAL and G21-02 Typed follower refusal, role reporting and node-local cache flush — November 10, 2026 (D368)

An enrolled SQLite follower now treats every API operation with an unsafe
method in one of three ways, kept by operation ID in one reviewed table
(`internal/api/follower.go`) and published in the OpenAPI document as
`x-kudurru-follower`. `forwarded` operations (blocking, the canonical object
routes including `POST /apply`, the kind routes and the five forwarded
`:batchDelete` routes, the four zone commands and local login: 26 operations)
go to the primary as typed member commands, as before. `local` operations (14:
cache flush, query-log purge and rollup verification, `POST /dns/query`, the
node DHCP routes, promotion, activation and join-token, and the two legacy
settings-ownership routes) run on the node. Everything else (53 operations,
and any operation that the table does not name) is refused by a route
middleware that `Server.declare` attaches where every route is registered:
after authentication and the permission check, before the body is read, it
answers `503 primary_required`, naming the primary from the authority that
the request's lease captured, with a hint and no `Retry-After`. A route added
later is therefore refused on a follower until someone classifies it, and the
inventory test fails for any registered mutation without a line. The writer
seams (`currentAuthCommandThen`, `currentAuthCommandOutput`,
`MigrationCommand`), the TSIG and integration mappers and the OIDC/CLI login
admission answer the same key when a follower's request reaches them, and a
DHCPScope object written through a follower is refused the same way.

`GET /cluster` and `kudurru cluster status` (new `ROLE` column) report `role`
(`standalone`, `primary`, `follower`), `primary`, `primaryRequired` and, on a
follower, the forwarded and local operation IDs. The API membership follows
the store's elector, so a follower names its primary as leader (and the list
refresher behind the router would answer its 409 naming the primary), and
`POST /blocklists/{ns}/{name}/refresh` on a follower answers
`503 primary_required` instead of a false `202`. `GET /auth/status` on a
follower carries `role` and `primary` and lists no OIDC provider. The settings
schema marks keys read-only exactly when their PATCH would be refused
(`PATCH /node/config` gets a hint to set log settings in the node's file or
environment). The block page replaces the unblock form with a message on a
follower, fenced or conflicted member, and a posted form answers `503` without
a store attempt. `POST /cache/flush` runs on a follower without a transaction:
it verifies the raw credential against the request's leased authentication
view (`AuthorizeOriginalTransition`), checks that the lease is still current,
and flushes only the node's own cache. A fenced or conflicted member now
answers `503 cluster_fenced` or `503 cluster_conflict` with a hint (the
conflict hint names `kudurru cluster resolution --offline` and
`kudurru cluster resolve --offline`) and no `Retry-After`;
`generation_pending` with `Retry-After: 1` remains for a publication being
prepared. The CLI adds hints for `primary_required`, `completion_unknown`,
`cluster_fenced` and `cluster_conflict`, and `kudurru login` reports a
follower's refusal as a known outcome.

Evidence under `.context/v020-final-20261110/evidence/HA-REFUSAL/`:

- `race01`-`race03-api-follower.log`: development runs of the new
  `internal/api` tests. race01 and race02 are retained failures (the test used
  a wrong schema path, then asserted on a fixture with no writable cluster
  key); race03 passed.
- `race04-api-mutation-expected-fail.log`: mutation check with the router
  middleware removed and `refreshBlockList` dropped from the table:
  `TestFollowerOperationsClassifyEveryMutation` and
  `TestFollowerRefusesEveryUnforwardedMutation` fail as intended (every
  primary operation reached its handler and answered 400/501).
- `race05-root-follower.log`, `race05b-light-packages.log`: first green runs.
- `gen01.log`: `make client-go` (190 operations), `bun run gen:api`, dashboard
  `typecheck` and `make docs-gen`, all exit 0.
- `race06-packages.log`: `go test -race -count=1` over `./internal/api/...`
  (206 s), `internal/servinghttp`, `internal/auth`, `internal/auth/stack`,
  `internal/blockpage`, `internal/cluster`, `internal/apiclient`,
  `hack/docsgen`: all ok.
- `race07-root-regression.log`: root regression selection of 45 test-name
  prefixes in `./cmd/kudurru` under `-race` (member forwarding, serving
  cache/node/settings/domain/unblock/zone/TSIG/blocking, CLI login, real
  router, runServe SQLite follower, TSIG follower, cache flush, local users,
  unblock requests, static twins, unregistered mutations, auth reload):
  ok in 890.7 s.
- `race08-root-new-x3.log`: the new and changed root tests
  `TestRootMemberFollowerRefusesUnforwardedMutations`,
  `TestRootMemberFollowerCommandSeamsNameThePrimary`,
  `TestRootMemberFollowerCacheFlushIsNodeLocal`,
  `TestServingCacheCommandFollowerAndMissingCapture`,
  `TestClusterStatusAndRefusalOnAFollower`, `TestClusterStatusRealRouter`,
  `TestMemberForwardRouteCoversEveryForwardedRoute`,
  `TestCLILoginTOTPSafeStageErrors`, `TestErrorHint`, `-race -count=3`: 27 of
  27 passed, 0 skipped, 39.0 s package time.
- `race09-root-mutation-expected-fail.log`: build overlay removing the router
  middleware, the command-seam refusal and the follower-local cache branch;
  all five follower root tests fail (58 route-level `want 503
  primary_required` failures, cache flush answering `unavailable`, list
  refresh losing the typed refusal), so the tests depend on the change.
- `lint01.log`: retained failure, one gosec G101 on a password literal in the
  new e2e helper; fixed. `lint02-final.log`: `gofmt -l` empty, `go vet` and
  scoped `golangci-lint` v2.15.0 over every touched package: 0 issues.
- `process01-enrolled.log` (before the lint fix) and
  `process02-enrolled-final.log` (final tree): `KUDURRU_E2E=1 go test -race`
  of `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (107.2 s),
  `TestEnrolledClusterProcessesPromoteAndRejoin` (125.0 s),
  `TestEnrolledProcessConflictResolution` (85.7 s) and
  `TestStaticClusterProcessesReadOnlyPeerLossRestart` (16.7 s): 4 of 4 passed
  both times, 344 s package time for the final run. The partition scenario
  now asserts the follower contract between real processes (role and primary
  on `/cluster` and `/auth/status`, one refused mutation per domain with key,
  primary name, hint and no `Retry-After`, no durable effect on the primary,
  list refresh not `202`, local cache flush); the conflict scenario asserts
  `cluster_conflict` with the offline commands and no `Retry-After`.
- `drift01.log`: `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestReferenceDrift`, `TestCLIReferenceDrift`, `TestAggregatedRolesDrift`
  after the `chore(gen)` commit.

Review fixes (accept-with-fixes, five minor findings). Provider login (OIDC
start and callback, CLI prepare) on a fenced or conflicted member now answers
`cluster_fenced` or `cluster_conflict` with a hint and no `Retry-After`, from
the same `servinghttp.AuthorityRefusal` that the lease adapter uses, instead
of `503 unavailable` with `Retry-After: 1`. Refusing a conflicted member
without the capture wait was implemented and then reverted (see `fix-09` to
`fix-11` below); both closed states keep the bounded two-second wait.
`kudurru login --username` reads
`GET /auth/status` and refuses on a follower before it sends the password, so
no orphan session is created on the primary, and the refusal text of a later
login stage no longer claims that nothing was changed. The two import previews
stay `primary` (decided, not changed): each plans an apply that only the
primary runs, and the decommission preview's revision is the primary's
resourceVersion that the decommission checks; the changelog now says that they
used to run on a follower. `TestCLILoginTOTPActualSQLiteLostResponseNeverRetries`
counted every request other than the login as a retry; it now requires exactly
one proof-free `GET /auth/status` before the login and still no request after
it.

- `fix-01-capture-wait-expected-fail.log`: with the conflict shortcut removed,
  its unit test
  `TestCaptureRefusalNamesFencedAndConflictedAuthority/conflict_refuses_without_the_capture_wait`
  failed after 30.0 s, as intended. The shortcut and that subtest were later
  reverted (below).
- `fix-02-expected-fail.log`: build overlay restoring the earlier admission
  and login code: `TestRootMemberClosedMemberProviderLoginNamesDurableState`
  (fenced and conflict: the three login routes answer `unavailable` with
  `Retry-After: 1`) and `TestCLILoginRefusesFollowerBeforeSendingProof`
  (the password was posted) fail, as intended.
- `fix-03-root-race.log`: retained failure. The new tests passed, but
  `TestCLILoginTOTPActualSQLiteLostResponseNeverRetries` failed twice because
  it counted the new status preflight as a retry; the test was made precise
  as described above.
- `fix-04-servinghttp-apiclient-race.log`: `internal/servinghttp` and
  `internal/apiclient`, `-race -count=2`: ok.
- `fix-05-root-race.log`: `./cmd/kudurru` `-race -count=2` over the CLI
  login, OIDC, device, follower, cache, request-budget and cluster-status
  selections plus the new tests: 102 top-level passes, 2 skips (the
  installed-Chrome qualification, which needs an explicit browser), 0
  failures, 402.6 s.
- `fix-06-lint.log`: `gofmt -l` empty; scoped `golangci-lint` v2.15.0 over
  `cmd/kudurru`, `internal/servinghttp`, `internal/apiclient`, `internal/api`:
  0 issues.
- `fix-07-api-follower.log`: `internal/api` follower tests,
  `TestRoutePermissionLint`, `TestOpenAPIDrift`, `-race`: ok.
- `fix-09-e2e-enrolled.log`: retained failure with the conflict shortcut.
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` passed (111.3 s);
  `TestEnrolledProcessConflictResolution` failed at its check that valid
  local DNS keeps answering: right after the API closed, both conflicted
  members answered SERVFAIL and readiness said "complete serving generation
  unavailable: preparing". `fix-11-e2e-conflict-new-x2.log` failed the same
  way twice more (34.8 s, 35.2 s); `fix-10-e2e-conflict-old-capture.log`,
  the same tree built with the earlier capture loop and admission through a
  `GOFLAGS=-overlay`, passed (71.6 s). The immediate refusal reaches the DNS
  check while the DNS-only publication is still being prepared; the two
  seconds per refused request had hidden that window. The shortcut was
  reverted rather than loosening the scenario's DNS assertion.
- `fix-08-drift.log`: `TestReferenceDrift`, `TestAggregatedRolesDrift`,
  `TestOpenAPIDrift`, `TestCLIReferenceDrift` on the final tree: ok. No
  generated artifact changed, so there is no new `chore(gen)` commit.

After the revert (commit `c033fc39`, on top of `929d66ce`, which added the
shortcut):

- `fix-12-servinghttp-race.log`: `internal/servinghttp` `-race -count=2`: ok
  (33.6 s).
- `fix-13-e2e-enrolled.log`: `KUDURRU_E2E=1 go test -race` of
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (108.2 s),
  `TestEnrolledClusterProcessesPromoteAndRejoin` (121.5 s) and
  `TestEnrolledProcessConflictResolution` (68.4 s): 3 of 3 passed, 313.9 s.
- `fix-14-root-follower.log`: `TestRootMemberClosedMemberProviderLoginNamesDurableState`
  and the `TestRootMemberFollower` tests, `-race`: ok.
- `fix-15-lint-servinghttp.log`: `gofmt -l` and scoped `golangci-lint` of
  `internal/servinghttp` on the final tree.

Still open. A conflicted member waits the bounded capture wait before each
refusal (finding 2 of the review, not fixed). Removing that wait first needs a
fix for the window seen in `fix-09` and `fix-11`, where a member that has
just recorded a conflict answers DNS with SERVFAIL while its DNS-only
publication is prepared; that window probably exists today too, hidden behind
the API's waits, and should be checked by the owner of the publication path. Sign-out with own-session revocation, settings and self-service
password and tokens are `primary` in this package; D368 names them as the
next forwarded domains of this lane, and allow/deny domain commands and
unblock decisions may follow. Until then a follower refuses sign-out with
`primary_required`, so a session created on a follower cannot be ended there.
`cluster_fenced` is covered by the lease-adapter test
(`TestCaptureRefusalNamesFencedAndConflictedAuthority/fenced`) and by the
mounted follower runtime over its real TLS listener
(`TestRootMemberClosedMemberProviderLoginNamesDurableState/fenced`), not by a
scenario between processes: no enrolled process test holds a former primary in
the fenced state before its first pull. The dashboard does not yet use `primaryRequired`,
`forwardedOperations`, `localOperations`, the auth-status role or the new
error keys (later dashboard lane). `docs/deploy/sqlite-cluster.md` (docs
lane) still describes `503 unavailable` and a follower naming itself as
leader. The full `./cmd/kudurru` package was not run (integrator runs it
through `hack/go-test.py`).

**Review and verification state at 68a709e2.** The review accepted the package with eight findings (five minor, three nit) and listed four weakened guards. The fixer was partial: it reports findings 1, 4 and 5 fixed and rebutted 2 (the shortcut for a fenced or conflicted member's capture wait broke an enrolled process test) and 3 (no process-level `cluster_fenced` assertion: needs a harness that keeps a former primary partitioned across a restart). **The fix commits were never verified**, and the four weakened guards were not re-examined; the three nits are open. Composition defect found at merge and fixed by the integrator: the Delete lifecycle's new `releaseIntegration` route had no follower class (`TestFollowerOperationsClassifyEveryMutation` and `TestRootMemberFollowerRefusesUnforwardedMutations` failed; `68a709e2` classifies it primary). HA-AUTH-SETTINGS later moved sign-out, own-session revocation and `PATCH /settings` from refused to forwarded; this section's statements about those routes are superseded there. HA-OPEN is to verify the fix; it has not run.


### FWD-LOGOUT and FWD-SETTINGS Forwarded sign-out, own-session revocation and cluster settings patch — November 10, 2026 (D368)

An enrolled SQLite follower now forwards sign-out (`POST /auth/logout`,
`POST /auth/logout/browser`), the revocation of one of the caller's own
sessions (`DELETE /auth/sessions/{id}`) and the cluster settings patch
(`PATCH /settings`) to its primary as two typed version-2 member commands,
`auth.logout` and `settings.patch`. Both follow the D362/D363 pattern. The
route is bound to 30 seconds before the body is read, the credential is
admitted against the follower's leased view, the source is captured once and
one command carrying the raw credential proof is sent. The primary verifies
that credential again inside the member transaction, where the effect, its
audit and the dedup record commit together. Clean refusals are retained
results, an unsent command answers `503 unavailable`, a failed exchange
`503 completion_unknown`, and nothing is retried.

For `auth.logout` the primary derives the session to end from the verified
cookie (new `servingauth.AuthenticateCurrentSessionWithClock`), never from an
ID that the follower names. `DELETE /auth/sessions/{id}` keeps the route's
owner check and retains its `404`. A token or JWT caller is a committed no-op.
A credential that the primary no longer accepts is a retained `401`, but a
command whose dedup record already exists is aborted instead, so a revoked
credential never recovers its own retained outcome. Because the command may
revoke its own credential, the result is emitted under the command scope only.
On the follower, a sign-out always clears the browser's cookies, also when the
command was not sent or its outcome is unknown, and the answer then says that
the session may stay valid until it expires. A successful sign-out pulls once,
so the follower refuses the session at once. A follower offers no provider
logout continuation.

For `settings.patch` the follower sends the merge patch, the If-Match and an
unstored 32-byte random nonce, with the `Kudurru/default` resourceVersion of its
own view as precondition. A stale view is a retained `409 conflict`. The
primary runs `api.Server.ForwardedSettingsCommand`, which repeats the route's
key, scope, lock and value checks, merges into the transaction's singleton,
checks branding against the transaction's pinned license (now and again at
commit) and stages one write whose object audit row names the original
caller. Refusals before the write (`400 invalid`, `400 host_only`,
`402 license`, `409`, `412`) are retained. A failed write rolls back. After a
committed patch the follower pulls once and answers when its published view no
longer carries the version that the patch was made against (bounded to five
seconds within the route's deadline; a failed pull or a late publication still
answers the committed result). Without that wait, found in review, the
caller's next patch through the same follower was a `409` and a forwarded
blocking switch a generic `503` for about five seconds, and the dashboard's
Settings page saves one key per request. The follower answers with the
primary's body under its own node name. The four
operations moved to the `forwarded` class, so `GET /cluster` lists them, the
OpenAPI document marks them `x-kudurru-follower: forwarded`, and a follower's
settings schema no longer marks cluster keys read-only. `PATCH /node/config`
stays `primary`.

Evidence under `.context/v020-final-20261110/evidence/HA-AUTH-SETTINGS/`:

- `race06.log`: `go test -race -count=1 -run '^(TestRootMember|TestMemberForward|TestServingHTTP.*Logout|TestServingHTTPDurableLoginTokensSessionsLogout|TestServingCanonicalSettings|TestServingBrandingHeld|TestServeLegacyNodeSettingsReadOnly|TestServingFollower|TestFollower)' ./cmd/kudurru`:
  37 passed, 0 skipped, 0 failed, 226.0 s. This includes the new
  `TestRootMemberLogoutCommandActualHTTP` (6.28 s: token no-op, retained
  404 for an unknown session ID, forged subject, unknown field and precondition aborted,
  revocation of a second own session with an exact dedup replay, sign-out of
  the cookie's own session, a revoked credential cannot recover its retained
  outcome, a new command with that credential is a retained 401, no raw cookie
  in the durable image), `TestRootMemberSettingsCommandActualHTTP` (3.97 s:
  commit with exactly one attributed audit row, dedup replay with no second
  write, stale precondition 409, retained 400 for unknown key, invalid value
  and empty patch, 400 host_only for a node key, 412 for an upstreams If-Match,
  402 for branding without entitlement, forged subject, missing precondition
  and short nonce aborted, the refused DSN and the raw token absent from the
  durable image, a revoked credential cannot recover the retained result) and
  `TestRootMemberForwardSettingsAndSignOutActualHTTP` (14.65 s: through the
  follower's mounted API and real lease: PATCH /settings, the relayed 400, the
  settings schema, a session revocation, the 404, sign-out without CSRF
  refused, browser sign-out clearing cookies and the follower refusing the
  session after its pull, and an API-token sign-out). It also includes the
  primary's own logout and settings tests, the follower refusal inventory
  (`TestRootMemberFollowerRefusesUnforwardedMutations`) and the route bound
  (`TestMemberForwardRouteCoversEveryForwardedRoute`,
  `TestRootMemberFollowerRequestLifetimeBound`).
- `light02.log`: `go test -race -count=1 ./internal/api ./internal/servingauth`
  passed (285.3 s and 98.3 s), including `TestSettingsForwardSeam` and
  `TestForwardedSettingsRefusalClassification`.
- `process01.log` (before the final lint rename) and `process02.log` (final
  tree): `KUDURRU_E2E=1 go test -run '^TestEnrolledClusterProcesses(PartitionCatchupAndRestart|PromoteAndRejoin)$' ./test/e2e`.
  Between two real processes, a settings change through the follower converges
  on both nodes, an invalid value is the primary's relayed 400, the session
  that the follower created is signed out through the follower (cookies
  cleared, a saved copy of the cookie refused by both nodes), and a settings
  patch while the follower is partitioned answers an explicit 503 and is never
  applied or replayed after reconnection. process01: both passed (116.4 s and
  122.7 s). process02: both passed again (110.9 s and 123.9 s; 245.8 s for the package).
- `lint03.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./internal/api/...`, `./internal/servingauth/...` and `./test/e2e/...`:
  0 issues. `drift01.log`/`drift02.log`: the drift tests pass after the
  generated commit.
- Review fixes (after an independent review of a7c85692):
  - `fix-race01-prefix.log`: the new
    `TestRootMemberForwardSettingsBackToBackActualHTTP` with the post-commit
    wait disabled fails as the reviewer measured: the second patch right
    after the first is `409 conflict` (3.49 s). This is the retained failed run
    that shows the test fails without the fix.
  - `fix-race02.log`: `go test -race -count=2 -run '^(TestRootMemberForwardSettingsBackToBackActualHTTP|TestRootMemberForwardSignOutFailuresActualHTTP|TestMemberLogoutFailureAnswers|TestRootMemberLogoutCommandActualHTTP|TestRootMemberSettingsCommandActualHTTP|TestRootMemberForwardSettingsAndSignOutActualHTTP|TestMemberForwardRouteCoversEveryForwardedRoute)$' ./cmd/kudurru`:
    7 tests passed twice each, 0 skipped, 70.2 s. The back-to-back test (6.0 s
    and 6.6 s) sends two patches and then a blocking switch through the
    follower with no retry: 200, 200, 202, and the follower serves each
    patch at once. `TestRootMemberLogoutCommandActualHTTP` now also revokes a
    live session of a second local account with the first account's cookie:
    a retained 404, and neither session ends.
    `TestRootMemberForwardSignOutFailuresActualHTTP` (6.7 s and 7.4 s), through
    the follower's mounted API: a sign-out of a session that the primary
    already revoked is the relayed 401 and clears the cookies; with the
    primary's writer stopped, `DELETE /auth/sessions/{id}` answers
    `503 completion_unknown` and keeps the caller's cookies, and a sign-out
    answers `503 completion_unknown`, clears the cookies and says that the
    session may stay valid; both saved cookies stay accepted by the follower
    and neither session is revoked on the primary.
    `TestMemberLogoutFailureAnswers` pins the unsent (`unavailable`),
    unknown-outcome and refusal answers, including the unsent branch, which
    no mounted fixture reaches.
  - `fix-race03-owner-mutation.log`: with the owner check removed from
    `rootMemberLogoutDomain.Execute` (existence check only), the
    foreign-account case fails (`200 {"revoked":true}`). The source was
    restored before any commit.
  - `fix-light01.log`: `go test -race ./internal/servingauth` passed
    (91.4 s) after `AuthenticateCurrentWithClock` stopped verifying each
    cookie twice.
  - `fix-process01.log`: `KUDURRU_E2E=1 go test -run '^TestEnrolledClusterProcesses(PartitionCatchupAndRestart|PromoteAndRejoin)$' ./test/e2e`:
    both passed (105.5 s and 120.8 s; 240.4 s for the package). The process
    test now sends two settings patches through the follower back to back,
    both `200`, and signs a second follower session out during the
    partition: `503` with key `unavailable` or `completion_unknown` and a
    message that the cookie was cleared, an empty cookie jar, and the saved
    cookie still accepted; after reconnection both nodes still accept it, so
    the sign-out was neither applied nor replayed.
  - `fix-lint01.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
    `./internal/servingauth/...` and `./test/e2e/...` at 17246f3a: 0 issues;
    `gofmt -l` empty. No route, DTO or schema changed, so nothing was
    regenerated.
- Retained failed runs: `race01.log` (an early harness that read the
  generation before publication: `serving: generation pending`, fixed in the
  test), `race03.log` (the mounted follower test answered
  `503 primary_required` for PATCH /settings before the classification moved
  to `forwarded`, which shows that the test fails without the change),
  `light01.log` (the follower schema test and the OpenAPI drift before the
  classification and the regeneration were finished) and `lint01.log` (a
  helper named `clear` shadowed the builtin; renamed in its own commit).

Open: self-service password, API tokens and TOTP, the allow/deny domain
commands and unblock decisions still answer `503 primary_required` on a
follower. The process test covers settings, sign-out and a partitioned
sign-out, not `DELETE /auth/sessions/{id}` (that route is covered through the follower's
mounted API in process). An unreachable primary makes a sign-out answer
`completion_unknown`, not `unavailable`: the member client treats every failed
POST as an unknown outcome, so `unavailable` appears only when the follower
never attempts the exchange. A forwarded blocking switch whose settings
precondition is stale still answers a generic `503` instead of the primary's
`409`; that is the existing FWD-BLOCKING relay, not changed here, and the
settings wait above removes the case that this package caused. Forwarded
settings on the rejoined old primary after a promotion is not asserted in the
process test. The dashboard does not yet consume the
new forwarded operation IDs. The full `./cmd/kudurru` package was not run
here; the integrator runs it through `hack/go-test.py`.

**Review and verification state at 68a709e2.** The review found no blocker and five findings (three major, two minor) plus six weakened guards. The fixer resolved all five in four commits; the second verifier confirmed the three major findings and the ledger finding resolved, nothing rebutted, nothing open (`evidence/HA-AUTH-SETTINGS/verify01.log`: the forwarded settings and sign-out selection, ok in 35.9 s). The implementer's own remaining items (OIDC, browser and device logins on a follower; node settings stay primary-only) stay open as listed above.


### F09b/F60c Integration `Delete` lifecycle, part 1 of 3: reserved finalizer, cleanup plan and ledger forget — November 10, 2026 (D367)

D367 records the whole `deletionPolicy: Delete` protocol (map items DEL-1 to
DEL-7, with the Kubernetes half DEL-8 as a later lane). This package lands DEL-1
and DEL-2, the parts the terminating job is built on. Running a `Delete` instance
is still refused by the SQLite root adapter (`integrationRuntime.current`), on
purpose: the refusal is removed only together with the cleanup job, so `Delete`
never behaves as a silent `Retain` (D239).

Store (DEL-1). The reserved finalizer `kudurru.io/integration-cleanup` and its
pure rule live in `internal/store/store.go`. SQLite admission applies the rule
inside `planApply`, after validation and defaulting, so `Store.Apply`,
`CommandTx.Apply` and restore arm an Integration whose policy is `Delete` in the
same write as its spec. No caller's document can set or strip the finalizer: a
same-manager apply without finalizers writes nothing, an edited document and a
`Force` takeover keep it, and an apply during termination neither finalizes nor
revives the object. Changing the policy to `Retain` disarms a live object and
releases a terminating one. `Retain` is the default, so a document that omits
`deletionPolicy` does the same; the audit row of a write that drops the
finalizer now says why (`disarmed: deletionPolicy is Retain; dropped
kudurru.io/integration-cleanup` on a live object, `released without cleanup:
...` on a terminating one, appended to a `Force` takeover's reason), so a
release is never mistaken for a finished cleanup. The Kubernetes driver has no
admission and never arms the finalizer itself (nothing would clear it while no
host runs there); it carries a finalizer the host armed through server-side
apply and through the `Force` takeover, which otherwise removes every field the
new manager omits. The new optional driver interface `store.FinalizerWriter`
changes one finalizer on the exact UID and resourceVersion the caller observed,
keeps manager, spec, status and generation, and removes a terminating object
when its last finalizer goes: `Store.UpdateFinalizer` and the transaction-bound
`CommandTx.UpdateFinalizer` on SQLite, a merge patch under the field manager
`kudurru-finalizer` on Kubernetes. That patch replaces the whole finalizer list,
so it carries the observed resourceVersion as the apiserver's optimistic lock: a
change between the driver's read and its write is a conflict that is not
retried, and a finalizer another actor added in between survives.

File reconciler. On SQLite an Integration is file-declared, so removing its
document is the delete request: an armed object now stays visible with a
deletion timestamp, re-adding the document with `Delete` does not finalize it,
and declaring `Retain` (or omitting the line) releases it, which the reconciler
logs as `objects: finalized` instead of an ordinary update. The delete is
requested and counted once: later passes leave an object that already has a
deletion timestamp alone, where they used to report, count and log a deletion on
every poll and file event. Admission acts on writes only, so a live `Delete`
Integration stored without the finalizer (a database written before admission
armed it, or a live `UpdateFinalizer` removal) used to stay unarmed behind its
unchanged document; the reconciler now applies that document
(`store.ReservedFinalizerUnarmed`), which arms the object on SQLite and changes
nothing else. On Kubernetes the same apply is a no-op at the apiserver, is
reported as unchanged and is made once per agreement, not on every pass.

Planner and ledger (DEL-2). `plan.Input.Cleanup` builds a deletes-only plan. It
refuses desired records and per-record policies, forces `sync` over the
destination's and the ledger's retained `upsert-only`/`create-only` policy, and
deletes a record set only when every row carries this cluster and UID marker (or
the ledger-proven legacy marker), every provider ID is in the confirmed ledger
baseline of that record set, and its name has no conflict. Every record set of
the owner that it leaves in place is listed in `Plan.Retained` with a reason
(conflict at the name, record set shared with rows of another owner, marker
without ledger evidence). The flag is part of the plan digest, and ordinary plans
keep the digests and operation IDs they had (pinned against values recorded
before the change). `ledger.Repository.Forget` removes one record's ledger row
and its operations by exact UID and resourceVersion, row first, through a new
guarded `Delete` callback. It refuses a pending row, a partially applied
operation, a stale identity, and a baseline that still lists provider content
unless the host supplies a fresh complete observation of absence. That last
case goes beyond the map, which allowed only a confirmed-absent baseline: a
record that someone removed at the provider is never confirmed by a delete
receipt, and its row would otherwise block cleanup forever. The SQLite root
adapter wires the callback (`integrationRuntime.forget`) as one fenced command
under the same lease, configuration and writer checks as every ledger write; it
can name only ledger and operation rows, so the shared request budget is out of
reach. The host gains `ownerLedgers`, `ownerOperations` and `strandedLedgers`,
which read one incarnation's rows across all destinations and name the rows of a
destination the spec no longer reaches.

Evidence under `.context/v020-final-20261110/evidence/INT-DEL-A/`:

- Store rule, unit (`race13`): `TestIntegrationCleanupArmedRule` (10 cases, each
  for both drivers and four caller documents) and
  `TestPlanFinalizerExactIdentityAndTermination` pass.
- SQLite, real `state.db` (`race02`, 5.8 s): `TestFinalizers` (unchanged),
  `TestFinalizersReservedIntegrationCleanup` with its six subtests
  (`ArmedInTheSameWrite`, `NoApplyStripsIt`, `RetainDisarmsAndReleases`,
  `MetadataOnlyRemoval`, `CommandTransaction`, `SurvivesReopenAndRestore`) and
  the shared `TestReservedFinalizerConformance` pass. Whole package under race
  (`race06`): exit 0 in 320.9 s.
- Kubernetes, envtest 1.37 (`race03`, repeated on the committed tree in `race15`):
  `TestReservedFinalizerConformance`, `TestReservedIntegrationFinalizerSurvivesApply`
  (non-Force server-side apply, a foreign `kubectl` field manager, Force takeover,
  applies during deletion, release by `Retain`, host disarm) and
  `TestUpdateFinalizerIsMetadataOnlyAndExact` pass, with the existing
  `TestDeleteHonoursFinalizers` and `TestForceIsWholeObjectTakeover`. Whole
  package under race (`race11`): exit 0, 85 passes, the three opt-in measurement
  skips, 73.7 s.
- Negative controls with the rule removed from each driver (`negative01`,
  `negative02`): the SQLite cases fail in all six subtests and the conformance
  case; on Kubernetes the conformance case fails at the Force takeover
  (`finalizers=[]`) and a document that lists the reserved name arms it.
- Mounted SQLite runtime (`race09`, `race10`):
  `TestSQLiteObjectSourceIntegrationCleanupFinalizer` (file loader: create armed,
  unchanged and edited documents, document removal leaves a terminating object,
  repeated pass, re-declared unchanged and edited, release by declared `Retain`,
  new incarnation) and `TestIntegrationRuntimeActualLedgerForgetIsFencedAndAtomic`
  (a real elected lease; the root deleter refuses a budget row, a desired object,
  a request without UID and `Force`; a stale or missing later request rolls back
  the ledger delete; a guard of another lease and a guard fenced by an
  Integration change forget nothing; the exact forget removes one ledger row and
  two operations with three audit rows; the budget row remains). The selection
  `^(TestIntegration|TestSQLiteObjectSource|TestCLIReferenceDrift)` under race:
  exit 0, 37 passes, no skips, 206.9 s, including the unchanged
  `TestIntegrationRuntimeRefusesDeleteAndUnlicensedExecution`.
- Planner, ledger and host under race (`race12`): exit 0, 109 passes, no skips;
  ledger, planner and the Cloudflare adapter again on the committed tree
  (`race16`): exit 0, 62 passes, no skips. New: `TestOrdinaryPlanDigestUnchangedByCleanupMode`,
  `TestCleanupDeletesOnlyMarkerAndLedgerProvenRows`,
  `TestCleanupOverridesRetainedPolicyOnlyInCleanupMode`,
  `TestCleanupConflictsMixedSetsAndAdoption`,
  `TestCleanupRefusesDesiredStatePendingRowsAndCapacity`,
  `TestForgetRemovesConfirmedAbsentRecordByExactIdentity`,
  `TestForgetRefusesPendingBaselineStaleAndForeignState` (four subtests),
  `TestForgetOrderSurvivesACarrierWithoutTransactions`,
  `TestSQLiteForgetIsFencedExactAndDurable` (real SQLite: stale term refused,
  reopen, budget row intact) and
  `TestOwnerLedgersAndOperationsSpanDestinationsRemovedFromSpec`.
- Real Cloudflare SDK against the local HTTP double (`race08`, `race16`):
  `TestSDKCleanupPlanDeletesOnlyLedgerProvenRowsOnUpsertOnlyZone` sends exactly
  one batch with the one proven delete on an `upsert-only` zone; a copied marker,
  a previous incarnation's row and a hand-made row stay byte-identical, and the
  adapter still refuses the same delete under `upsert-only`.
- Other touched or dependent packages under race: `internal/store/memory`,
  `internal/objects`, `internal/store/validator` (`race07`), `ee/integration/unifi`,
  `ee/integration/webhook` (`race08`), `api/...` (`race14`): exit 0.
- Drift (`drift01`, `drift02`): `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in `race10`. No
  generated artifact changed, so there is no generated commit.
- Scoped lint on the committed tree (`lint02`, HEAD `057eed2d`): zero issues over `api/v1alpha1`, `internal/store`,
  `internal/store/{conformance,sqlite,kube}`, `ee/integration/{plan,ledger,host,cloudflare}`
  and `cmd/kudurru`.

Review fixes (commits `a67e7346`, `20d51820`, `e449084f`, `1466f53b`, `88f45f86`
and `91e900da` on `wip/v020/int`; the last one changes one sentence of the
operator guide and was made after the runs below, which name `88f45f86`; logs
`fix-01` to `fix-17`):

- Kubernetes patch lock (`fix-01`, 17.5 s): the new envtest case
  `TestUpdateFinalizerActualConflictIsNotRetried` wraps the transport and changes
  the object between the driver's read and its first `kudurru-finalizer` patch,
  for the arming write and for the release. Both subtests get `ErrConflict` after
  exactly one patch, and the concurrently written finalizers survive.
- Plan digest (`fix-03`): `TestCleanupFlagAloneChangesDigestAndOperationIDs`
  builds both modes over observations that plan the same single delete, checks
  that the two plans are equal apart from the flag, the digest and the operation
  ID, and that the digests, operation IDs and durable intent names differ.
- Audit reasons (`fix-16`, on the committed tree; `fix-06` is the same selection
  with the first wording of the reason, see `fix-08a` below):
  `TestReservedFinalizerDropReason` (8 cases), `TestReservedFinalizerUnarmed`, and in
  `TestFinalizersReservedIntegrationCleanup` the extended
  `RetainDisarmsAndReleases` and the new subtest `OmittedPolicyDropIsAudited`
  (disarm, release with another finalizer left, release by a `Force` takeover
  with the policy omitted, and no reason on writes that keep the obligation).
- File reconciler over real SQLite (`fix-08`):
  `TestReconcileRequestsDeletionOfATerminatingObjectOnce` (three later passes and
  a restarted loader report nothing, the deleted metric stays 1, one `objects:
  deleted` line, no further audit row, and the object is gone after the cleanup
  owner's release), `TestReconcileArmsAnUnarmedDeleteIntegrationFromItsUnchangedDocument`
  (same process and after a restart: one update that changes only
  `metadata.finalizers`, then unchanged), `TestReconcileReleaseByDeclarationIsVisible`
  (policy omitted and explicit: `objects: finalized`, the audit reason, a new
  incarnation on the next pass) and
  `TestReconcileUnarmedDeleteIntegrationWithoutAdmissionIsAppliedOnce` (memory
  driver: one apply over three passes).
- File reconciler over the Kubernetes driver (envtest, in `fix-12` and `fix-16`):
  `TestFileDeclaredDeleteIntegrationIsNotRewrittenWithoutAdmission` reports the
  unarmed `Delete` Integration as unchanged on three passes of a restarted
  loader, with an unchanged resourceVersion and exactly one apply, and makes no
  apply once a host has armed the object.
- Mounted SQLite object sources (`fix-11`, through the lock wrapper, HEAD
  `88f45f86`): the selection
  `^(TestSQLiteObjectSource|TestIntegration|TestRunServeSQLiteSourceBinding|TestRunServeUniFiHostname|TestApply|TestDelete$|TestCLIReferenceDrift)`
  under race: exit 0, 43 passes, no skips, 346.0 s.
  `TestSQLiteObjectSourceIntegrationCleanupFinalizer` now also asserts that a
  live object whose finalizer was removed is armed by the next pass over its
  unchanged document (same loader and a restarted one, generation and UID
  kept), that two further passes over the terminating object report no
  deletion, and that the release audit row is `integration.finalize` by the
  file manager with the `released without cleanup` reason.
- Whole packages under race: `internal/store`, `internal/objects`,
  `ee/integration/plan` (`fix-10`): exit 0. `internal/store/kube` (`fix-12`): exit
  0, 87 passes, the three opt-in measurement skips, 74.9 s.
  `internal/store/sqlite` and `internal/api` (`fix-13`): exit 0 in 268.8 s and
  190.5 s. Every new or extended library and driver test again, twice, on the
  committed tree (`fix-16`): exit 0, 50 passes.
- Hygiene: `gofmt`, `go vet` and `CGO_ENABLED=0 go build ./...` clean (`fix-15`).
  Scoped lint through the lock wrapper on HEAD `88f45f86` (`fix-14`): zero issues
  over `internal/store`, `internal/store/{sqlite,kube,conformance}`,
  `internal/objects`, `ee/integration/plan` and `cmd/kudurru`. No generated
  artifact changed, so there is still no generated commit;
  `TestCLIReferenceDrift` passes in `fix-11`.

Retained failed runs of the review fixes (the negative controls and before-fix
runs were expected to fail; `fix-08a` was not): `fix-02` (build overlay, the patch without `metadata.resourceVersion`: both subtests fail with
`err=<nil> patches=1`, the stale write lands). `fix-04` (build overlay, the flag
set only after the digest: only the new planner test fails, with both digests
`abe407b9...`, the value the reviewer recorded). `fix-05`, `fix-07` and `fix-09`
(the new tests against the store and reconciler code as committed at `057eed2d`:
empty audit reasons, `deleted=1` on a pass over the terminating object,
`updated=0 unchanged=1 finalizers=[]` over an unarmed object, `objects: updated`
for a release). `fix-08a` is a transcript of the first run after the reconciler
fix, whose log file the passing rerun overwrote: it showed that the memory
reference driver rewrites the object on every `Apply`, and that the reconciler
defaults a document before it applies it, so the store cannot tell an omitted
`deletionPolicy` from an explicit `Retain` on the mounted path. The audit reason
therefore names the effective policy only, and the "unchanged without admission"
claim is proven on the real Kubernetes driver instead of the memory driver.
`fix-17` was not run: a negative control of the mounted test against the
`057eed2d` store and reconciler code waited 39 minutes for the machine-wide lock
behind other lanes and was canceled. The mounted test is therefore shown to pass
on the fixed tree (`fix-11`) but not shown to fail before the fix; the same
assertions fail before the fix at the library seam over real SQLite (`fix-07`,
`fix-09`).

Retained failed runs of the implementation: `race01` (two new SQLite subtests
read the bookmark of a resumed watch as an object event; the tests now skip bookmarks). `race04` (the
new mounted forget test built a receipt with a local-zone time; the repository's
exact comparison of the stored operation refused it, and the test now uses the
canonical UTC form the providers use; the other four tests of that run passed).
`lint01` (two findings in new tests: a `gosec` index warning and a deprecated
`FieldsV1.Raw` access; both rewritten).

Open after this package: the elected terminating job (DEL-3), the SQLite root
cleanup guards, local purge, finalization and removal of the refusal (DEL-4), the
API/CLI surface and release command (DEL-5), the operator guide and CRD field
description (DEL-6), the mounted lifecycle acceptance (DEL-7) and the Kubernetes
half (DEL-8). Until DEL-3 and DEL-4 land, nothing clears the finalizer: removing
the document of a SQLite Integration declared with `Delete` leaves it terminating
until it is declared with `Retain` or without a `deletionPolicy` line. The
cleanup plan and `Forget` have no production caller yet, and the root deleter is reachable only under an ordinary
(live object) guard. The memory reference driver has no deletion timestamp and
does not run the reserved-finalizer conformance case. On Kubernetes
`UpdateFinalizer` is on the driver only: the audit and identity-gate store
wrappers do not forward it, and no host arms the finalizer there yet.

Handed to the next packages by the review: (1) DEL-4 must check
`HasFinalizer(IntegrationCleanupFinalizer)` inside every intent and source
commit, as the map requires. The reconciler arms a declared object that was
stored unarmed on its next pass, but until that pass the object is unarmed, and
only the per-transaction check keeps a `Delete` instance from owning external
state in that window; DEL-4 should add a mounted test with a pre-existing
unarmed object when it removes the refusal. (2) A terminating Integration is
still released by any apply whose defaulted policy is `Retain`, including a
re-added document without the line or a restored older copy. This is now
documented, audited and logged, but it is not prevented; DEL-5 should consider
making release an explicit command only, with a reconciler diagnosis when a
document targets a terminating Integration (a new diagnosis reason is part of
the API contract, so it was not added here). (3) A Kubernetes `Force` takeover
whose body says `Retain` strips the finalizer without a specific reason: on
Kubernetes audit events come from the store wrapper (D156), not from the driver,
and the wrapper was not changed. (4) The memory reference
driver rewrites on every `Apply`, so there the reconciler's extra apply of an
unarmed `Delete` document counts as one update per loader start.

**Review and verification state at 68a709e2.** The review accepted DEL-1 and DEL-2 with six findings, two major (the Kubernetes finalizer patch lock was untested; the cleanup flag's place in the plan digest was not proven). The fixer resolved all six; the verifier confirmed both major findings resolved and reported one nit in the changelog draft (the "no edited, re-declared or force-applied document can add or remove it" wording), which the folded changelog does not repeat.


### F09b/F60c Integration `Delete` lifecycle, part 2 of 3: elected terminating job and SQLite cleanup — November 10, 2026 (D367)

This package lands DEL-3 (the host's terminating job) and DEL-4 (the SQLite
root: mode-aware guards, atomic local purge, finalization, release, and removal
of the refusal). An Integration with `deletionPolicy: Delete` now runs on
SQLite, and a delete request is followed by a cleanup instead of leaving the
object terminating forever. The refusal in `integrationRuntime.current` is gone
in the same commit that mounts the cleanup, so `Delete` never behaves as a
silent `Retain` (D239). The D367 row is restated in full in
`out/INT-DEL-B/decision.md`: the protocol is the one INT-DEL-A recorded, with
the points this package had to decide spelled out (see "Decided here" below).

Host (DEL-3, `ee/integration/host`). Every job holds one mode,
`ExecutionGuard.Mode`: normal, `cleanup` or `release`. `Host.disposition`
derives it from the captured object (`InstanceMode`): a live object runs the
ordinary lease; an object that is being deleted and carries the reserved
finalizer runs the cleanup job when its policy is `Delete` and the release job
under any other policy; one that is being deleted without the reserved
finalizer stays `Paused/Deleting` as before. A change of mode replaces the job
like any other change of authority, so the ordinary job is canceled and joined
before the terminating job is elected on the same per-instance key, and a
recreated object of the same name cannot start until the terminating job has
returned from its last root callback. The terminating lease
(`runTerminating`) repeats bounded runs, each under one operation timeout, on a
freshly captured frame, and takes a concurrency slot only while a run executes.
One cleanup run of a publisher provisions and checks the provider with the
captured named credentials (never `Start`), rereads every configured
destination completely, resolves pending intents by the existing recovery
rules, builds cleanup plans (`plan.Input.Cleanup`) and executes their deletes
through the same `applyPlan` an ordinary sync uses (durable intent, fence,
budgeted request, receipt, confirm), rereads, and forgets each ledger row whose
record is absent (`ledger.Repository.Forget`): a confirmed delete, or a fresh
complete snapshot that holds none of the baseline's provider IDs and no marked
row at the key. When no record, ledger row, operation or uncertain intent of
the incarnation is left it calls the root's `Purge` and `Finalize`. A provider
that does not publish (UniFi, webhook) owns nothing remotely: its cleanup
builds no provider, needs no credential and makes no provider call. Recovery
has one relaxation, in the cleanup mode only: a partially applied delete is
resumed although the credential or configuration digest changed since it was
recorded. Operator commands (`CaptureCommand`, check, preview, sync, invoke)
are refused for a terminating job with `ErrFenced`. The local status has phase
`Terminating`, a reason and counts (`Result.Remaining`, `Conflicts`,
`Pending`), follows the object's resource version across the job's own status
writes, and is published through the existing `Status` callback.

Blocking and pausing. Nothing is finalized while any of these holds, and each
has its own reason: `RecordsConflict` (a conflict at the name of an owned
record set, or a record set shared with another owner's rows),
`OwnershipUnproven` (a marker without ledger evidence, or ledger provider IDs
that still exist without the marker), `PendingRecovery`, `DestinationRemoved`
(ledger rows of a destination the spec no longer names, or of an instance whose
provider does not publish), `SnapshotIncomplete`, `LocalStateRemains`. Proven
record sets are still deleted and forgotten while others block.
`LicenseUnavailable` and `Suspended` start no job; `CredentialsUnavailable`
ends a publisher's lease before any provider call. `BudgetExhausted`,
`RateLimited`, `ProviderFailed` and `ProvisionFailed` retry with the existing
classified backoff; a blocked cleanup is re-evaluated at the resync interval,
at most every ten minutes, and at once when the object changes.

SQLite root (DEL-4, `cmd/kudurru`). `integrationGuardAdmits` is the one rule
for the captured cut (`current`), the pre-effect fence (`fence`) and every
transaction (`integrationInputsCurrent`): the Integration must be what the
guard's mode claims. Normal mode needs a live, unsuspended object, and one
whose policy is `Delete` must carry the finalizer, so an unarmed instance gets
no intent, budget charge or source commit (the host reports it as
`Paused/CleanupFinalizerMissing`); cleanup needs a deletion timestamp, the
finalizer, policy `Delete` and no suspension; release needs a deletion
timestamp and the finalizer under another policy and is the only mode that
runs without a license that allows integrations. License, key, writer-token
and publication pins are unchanged. A terminating instance selects no sources:
it does not make the capture prepare the primary image, and source, target and
group changes neither wake nor fence it. `integrationRuntime.purge` removes in
ONE command the record sets and zones the incarnation's source owns (each must
still be the UID and resource version of the captured cut), its source head and
chunks, its name allocations and its deliveries, withdraws its clients from the
global inventory, writes an `integration.purge` audit row, and then waits until
the published bundle shows none of it. Every delete names the exact UID and
resource version read in that transaction. A source object that carries
someone else's finalizer is only marked for deletion and reported
(`ErrRemains`). `integrationRuntime.finalize` verifies in ONE command that no
ledger row, operation, source head or chunk, name allocation, delivery or
source-owned object of the incarnation is left, removes the finalizer with
`CommandTx.UpdateFinalizer` by the guard's exact UID and resource version, and
writes an `integration.cleanup` audit row with what the finalizing lease
deleted, forgot and purged. Under the release mode it removes the finalizer
without any check or cleanup and records `integration.release` with what stays
behind. `stageIntegrationStatus` writes `Ready=False` with the terminating
reason and, in `status.records`, the remaining, blocked and uncertain counts.
The shared request budget row is charged like any other request and never
removed.

Decided here (all within D367; the row now says so):

- A missing credential pauses only a publisher's cleanup. Requiring a
  credential and a reachable controller to purge the local state of an inbound
  instance would trap exactly the operator who removed the controller.
- Only a partially applied delete is resumed across changed authority. Any
  other partially applied intent stays pending in cleanup (nothing is desired
  any more, so finishing a create or update is not authorized) and blocks with
  `PendingRecovery` until the operator releases the object. The Cloudflare
  adapter sends a record set's effects in one batch, so this needs a record
  set of more than 200 rows or a provider without atomic batches.
- Ledger evidence without the marker blocks, like the marker without ledger
  evidence: a record whose provider IDs still exist but no longer carry the
  marker was taken over by someone, and its ledger row is not forgotten.
- Proven record sets are deleted while others block. "Never finish partially"
  means never finalize; waiting for every record set to be provable would not
  make a release all-or-nothing either, because a conflict can appear between
  two runs.
- The terminating job holds a concurrency slot per run, not per lease, so a
  blocked cleanup cannot starve the four default slots.

Evidence under `.context/v020-final-20261110/evidence/INT-DEL-B/`:

- Host library under race, committed tree (`host07`, whole package: exit 0,
  84 passes, no skips, 12.5 s; the new tests five and ten more times in
  `host05` and `host06`: exit 0, 21.9 s and 43.3 s).
  New, over the real `Host.Run` loop, the real ledger repository on the memory
  store, and a provider whose every remote call goes through the host's
  request adapter (budget, fence, lease):
  `TestTerminatingDeleteElectsDeletesForgetsAndFinalizesOnce` (the ordinary
  provider is stopped before the cleanup provider is provisioned; one delete
  of the proven record set; a previous incarnation's row, a hand-made row and
  an external-dns row unchanged; no ledger or operation row left; budget row
  kept; one purge, one finalization under the cleanup mode),
  `TestTerminatingRetainIsReleasedWithoutProviderOrLicense` (unlicensed,
  suspended, credential unresolvable: one finalization under the release
  mode, no factory call, no request, no purge, no budget row),
  `TestTerminatingPausedReasonsMakeNoProviderCall` (six subtests: `license`,
  `suspended`, `publisher credential` with the durable status, `observer
  needs no credential`, `live object without the finalizer`, `another
  finalizer only`),
  `TestTerminatingLeaseLossBetweenApplyAndReceiptResolvesByReread` (the lease
  ends inside the provider's delete; the fenced lease records no receipt and
  leaves the ledger pending; the next lease confirms the absence with zero
  further provider mutations and zero adapter recoveries),
  `TestTerminatingBlockingConditionsNeverFinalize` (seven subtests: conflict
  at a name with owned rows, incomplete snapshot, marker without ledger
  evidence, ledger evidence without marker, record removed at the provider by
  someone else, destination removed from the spec, provider answer lost
  without a lease change; each checks the local and the published reason and
  counts, that the report follows the object's resource version, and that
  cleanup finishes once the cause is gone),
  `TestTerminatingBudgetExhaustionMakesNoRequestAndResumes` (the request
  counter does not move while the window is full; the next window finishes
  the cleanup; the budget row keeps its UID),
  `TestTerminatingReplacementWaitsForCleanupJoin` (a recreated UID of the
  same name builds no provider while the old job is inside a root callback),
  `TestTerminatingCleanupContinuesAcrossBoundedRuns` (four record sets under
  a 750 ms run bound with every second delete slower than the rest of its
  run: one lease, four runs, each record set deleted exactly once),
  `TestTerminatingBlockedJobHoldsNoConcurrencySlot`,
  `TestTerminatingJobRefusesCommands`, `TestInstanceModeFollowsTheStoredObject`
  and `TestCleanupRecoveryResumesOnlyDeletesAcrossChangedAuthority` (a
  partially applied delete is resumed in the cleanup mode after the config
  and credential digests changed, is left pending by an ordinary sync and by
  a cleanup input under an ordinary guard; a partially applied create stays
  pending in cleanup).
- Negative controls of the host tests, by build overlay with the worktree
  unchanged (`negative01` to `negative05`, each fails exactly its target):
  no relaxation in recovery, and the relaxation not bound to the cleanup
  guard (`TestCleanupRecoveryResumesOnlyDeletesAcrossChangedAuthority/partial_delete`);
  a ledger row forgotten although its provider IDs still exist
  (`.../ledger_evidence_without_marker`); a license-gated release
  (`TestTerminatingRetainIsReleasedWithoutProviderOrLicense`); a terminating
  job that accepts a command capture (`TestTerminatingJobRefusesCommands`).
- Mounted SQLite runtime under race, through the lock wrapper (`mounted02`,
  selection
  `^(TestIntegration|TestSQLiteObjectSource|TestRunServeSQLiteSourceBinding|TestRunServeUniFiHostname|TestApply|TestDelete$|TestCLIReferenceDrift)`:
  exit 0, 50 passes, no skips, 372.0 s; no Go file changed between that run
  and the commits). New, on the real SQLite writer, publisher, host, ledger
  and budget:
  `TestIntegrationDeleteCloudflareCleanupRemovesOnlyOwnedRecordsAndFinalizes`
  (the registered Cloudflare adapter and SDK against the verified-TLS peer of
  its canonical origin, on an `upsert-only` zone: the object is stored armed;
  with the peer held, the delete request leaves a terminating object with its
  spec and credential reference, the host reports `Terminating` and refuses a
  sync; then exactly one batch whose delete effects name exactly the two
  published provider IDs; a foreign-marker row, a hand-made row and an
  external-dns record with its heritage TXT are byte-identical; no ledger or
  operation row; budget row kept; the operator's own record set untouched;
  the audit trail ends with `integration.finalize` and `integration.cleanup`
  ("deleted 1 provider record sets", "forgot 1 ledger rows and 2 operations");
  no credential bytes in the image; the name is free for a new armed
  incarnation), `TestIntegrationDeleteUniFiPurgesLocalStateAndFinalizes` (the
  UniFi adapter against the HTTPS controller double: no controller request
  during cleanup; the source-owned record set, source head, chunks and name
  allocation are gone, the inventory has no client of the source, the zone
  stops answering the generated name; the operator's record, its answer and
  the group's hand-added member stay; the purge's deletes and its
  `integration.purge` row are one unbroken run of the audit log),
  `TestIntegrationDeleteHeldSourceObjectKeepsTheInstanceTerminating`
  (`LocalStateRemains` while a generated record set carries another
  finalizer; finishes after that finalizer is gone and the object is declared
  again), `TestIntegrationDeleteReleaseNeedsNoLicenseCredentialOrProvider`
  (unlicensed runtime, unresolvable credential, terminating `Retain` object
  with the finalizer: gone with no provider built, the ledger row kept, no
  budget row, an `integration.release` audit row and no `integration.cleanup`),
  `TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes` (two subtests:
  `resolved` and `released`; `RecordsConflict` in the host status at the
  object's current revision and in the `Ready` condition, `status.records`
  1 remaining and 1 blocked, no delete effect; then either the cleanup
  finishes after the conflicting row is gone, or declaring `Retain` removes
  the object with no provider write and the ledger kept),
  `TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes` (the
  peer's request counter does not move while the credential file is missing;
  restoring it finishes the cleanup),
  `TestIntegrationGuardAdmitsOnlyTheObjectItsModeClaims` (nine object states
  under each of the three modes) and the rewritten
  `TestIntegrationRuntimeDeleteRunsArmedAndRefusesUnarmedOrUnlicensed` (three
  subtests: an armed `Delete` instance commits its source; one whose finalizer
  was removed is `Paused/CleanupFinalizerMissing` with zero polls, no source
  and no budget row until an apply arms it; unlicensed is unchanged). It
  replaces `TestIntegrationRuntimeRefusesDeleteAndUnlicensedExecution`, which
  asserted the refusal. The unchanged
  `TestIntegrationRuntimeActualLedgerForgetIsFencedAndAtomic` and
  `TestSQLiteObjectSourceIntegrationCleanupFinalizer` pass in the same run.
- Negative control of the mounted tests (`negative06`, build overlay that puts
  the old refusal back into `integrationRuntime.current`): the Cloudflare and
  UniFi lifecycle tests and the `delete` and `unarmed` subtests fail; the
  release test and the `unlicensed` subtest pass, as they do not depend on a
  `Delete` instance running.
- Other integration libraries under race, committed tree (`race06`):
  `ee/integration/cloudflare` 9.8 s, `unifi` 6.9 s, `webhook` 2.9 s, `plan`
  1.2 s, `ledger` 3.6 s, `inbound` 14.2 s, all exit 0.
- Drift, committed tree (`drift02`): `TestOpenAPIDrift`, `TestReferenceDrift`
  and `TestAggregatedRolesDrift` pass; `TestCLIReferenceDrift` passes in
  `mounted02`. No generated artifact changed, so there is no generated commit.
- Hygiene: `gofmt`, `go vet ./ee/integration/...` and `CGO_ENABLED=0 go build
  ./...` clean (`fmt-vet-build01`). Scoped lint through the lock wrapper
  (`lint01`, the same Go sources as the commits): zero issues over
  `./cmd/kudurru/...` and `./ee/integration/host/...`. No dependency was
  added.

Retained failed runs: `host01` (the first run of the new host tests; only
`TestTerminatingDeleteElectsDeletesForgetsAndFinalizesOnce` failed, in its
setup: the ordinary sync reports the three seeded rows of other owners as
conflicts, so the instance is `Degraded/ConflictsOrPending`, not `Ready`, and
the helper now waits for the reported publication instead). `mounted01` (the
first mounted run, seven of the eight selected tests passed;
`TestIntegrationDeleteUniFiPurgesLocalStateAndFinalizes` failed at its
inventory assertion, because the test read the published bundle in the instant
after finalization, when `Publisher.CaptureHTTP` refuses a capture until the
publisher has caught up with that last commit; the assertion now waits for the
capture). Neither changed product code. `negative01` to `negative06` are the
negative controls above and were expected to fail.

Review fixes (accept-with-fixes review; four source commits on top of the
implementation: `fix(integration): bound cleanup history reads and report
counts`, `test(integration): prove root's mode rule and purge refusals`,
`docs(integrations): document cleanup limits and every reason`, `test(integration):
wait for the recorded finalization`):

- Root's mode rule now has tests that fail without it.
  `TestIntegrationRootRefusesAModeTheStoredObjectDoesNotAllow` calls
  `current`, the stored-object fence (`storedCurrent`, split out of `fence`),
  `integrationInputsCurrent` inside a real SQLite command, and the full
  `integrationRuntime.command`, for eight stored states (retained, armed,
  unarmed, suspended, cleanup, frozen, release, held by another finalizer)
  under each of the three modes. Each guard names the object's exact UID and
  resource version, so only the mode can refuse it; exactly four guards are
  admitted, and a refused one leaves no ledger, operation, budget or source
  head row. `TestIntegrationRuntimeRootRefusesAJobTheStoredObjectDoesNotAllow`
  (two subtests) mounts a host whose capture misreports the object (an
  unarmed `Delete` object shown armed; a live object shown deleting): root
  alone gives the job no provision, poll, source, budget charge, purge or
  finalization.
- Cleanup no longer loads the whole operation history. Ledger rows are read
  per configured destination, as a sync reads them, and only counted
  owner-wide (`ledgerCounts`, 100,000-row bound); a ledger row is forgotten
  with one bounded batch of its operations (`operationBatch`: 4,096
  operations, 8 MiB); the rest of the history is forgotten a stored page at a
  time (`operationPages`, no row bound, ends with the run's context) once no
  ledger row is left, repeated until a complete read finds none. A bounded
  read that meets more than it admits reports the new reason
  `CapacityExceeded` instead of `CleanupFailed`.
- `Result.Remaining` is counted when a run starts and carried on every early
  return (budget, incomplete snapshot, provider failure, missing credential,
  provider unavailable); a run fenced before it could count keeps the last
  run's counts.
- A release job still runs without a license, but again only while root's
  serving lifetime lasts (new `Options.Alive`): the earlier license bypass had
  also dropped that part of `Allowed`.
- New mounted `TestIntegrationDeletePurgeFencesAChangedObjectAndFinalizeRefusesLeftovers`:
  a generated record set changed after the cut fences the purge as one
  command (nothing removed); the next purge removes it with a queued
  `IntegrationDelivery` (Deliveries 1); a delivery stored between purge and
  finalization makes `finalize` refuse with `ErrRemains` while the finalizer
  stays; a later purge removes it and finalization succeeds; two
  `integration.purge` audit rows. `TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes`
  now asserts that root's capture selects the publisher's record set while it
  lives and nothing once it terminates (`needsSources`).
  `TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes` now
  asserts Remaining 1 and `status.records.managed` 1 on the credential stop.
- The UniFi purge test reads both names from one successfully captured
  bundle in one wait, so a refused capture can neither satisfy the negative
  wait nor fail the positive check.
- Docs: the operator guide's reason table adds `CapacityExceeded`,
  `CleanupFailed`, `ProviderUnavailable` and `ReleaseFailed`, explains the
  counts, says `LicenseUnavailable` and `Suspended` are host-status only, and
  lists the bounds; restart and failover resumption is worded as the design.

Evidence of the fixes (same directory, `fix-*`):

- Host package under race, whole package (`fix-host04-package-v`): exit 0,
  87 passes, no skips, 53.0 s. New: `TestTerminatingLongOperationHistoryIsForgottenInBoundedSteps`,
  `TestOperationReadsAreBoundedWhateverTheHistory` (a history of 100,179
  rows and 33.7 MB is read and batched), `TestTerminatingCapacityIsReportedAndRetried`;
  remaining-count assertions added to the blocked, budget and slot tests.
- Host negative controls by build overlay (`fix-negative-h1` to `h7`, each
  exit 1 on its target): remaining not counted at run start; a fenced run
  reporting zero; the operation read keeping the 100,000-row bound; an
  unbounded batch; capacity not named; leftovers in one pass; a release that
  ignores the serving lifetime.
- Mounted selection under race through the lock
  (`fix-mounted02-selection`, `^(TestIntegrationDelete|TestIntegrationRuntime|TestIntegrationForget|TestIntegrationAdmin|TestIntegrationOperation|TestIntegrationRoot|TestIntegrationGuard)`):
  exit 0, 31 top-level passes, no skips, 219.4 s.
- Mounted negative controls by build overlay, worktree unchanged, each exit 1:
  `fix-negative-m1-root-rule-always-admits` (`integrationGuardAdmits`
  returns true: both new root tests fail, every site and both subtests);
  `fix-negative-m2-purge-no-cut-check` and
  `fix-negative-m3-finalize-no-remains-check` (the purge/finalize test
  fails); `fix-negative-m4-terminating-publisher-needs-sources` (the blocked
  test fails at the "selects nothing once terminating" wait).
- Repetition, `-count=20` under race of the UniFi purge test and the new
  purge/finalize test: `fix-mounted03-unifi-and-purge-x20` failed once (exit
  1, 369.7 s): the purge/finalize test read its record of `Finalize` calls
  after the object was gone, which is before the wrapper records the
  successful call, so it saw only the `ErrRemains` refusal. A fourth commit,
  `test(integration): wait for the recorded finalization`, waits for that
  record; `fix-mounted04-unifi-and-purge-x20` then passed 20 of 20 for each
  test (exit 0, 365.1 s). The UniFi test did not fail in 40 runs after its
  fix.
- Scoped lint through the lock (`fix-lint01`): zero issues over
  `./cmd/kudurru/...` and `./ee/integration/host/...`. `gofmt -l` clean,
  `go build ./...` and `go vet ./ee/integration/host/` clean
  (`fix-fmt-vet-build01`). No generated artifact changed; no dependency
  added.
- Retained failed runs: `fix-host01-first-run` (written by an earlier fixer
  run that was cut off; the first version of the long-history test timed out
  at `ProviderFailed` with two rows left, after 67.5 s; the uncommitted code
  was changed before `fix-host02`, where the whole new set passed, and the
  exact change cannot be reconstructed from what was retained).
  `fix-mounted01-first-run` passed (exit 0).

Open after this package: the API and CLI surface (DEL-5: `deletionPolicy`,
`terminating` and `deletionRequestedAt` in the summary, the reviewed `release`
command and its permission, CLI columns), the CRD field description, tracker
edits and the remaining operator-guide detail (DEL-6), the rest of the mounted
acceptance list (DEL-7), and the Kubernetes half (DEL-8). Not shown by a mounted
test in this package, only by the host-level tests over the memory ledger:
lease loss and takeover in the middle of a cleanup (DEL-7 cases 4 to 6: restart
with an unreadable list, an ambiguous batch, replica promotion while a batch is
held), license expiry after the delete request (case 7; the host tests cover
a cleanup that starts unlicensed and resumes, not an expiry in the middle of a
run), and the SDK-level bounds (at most 200 effects per batch, one budget
charge per HTTP attempt).
A run that cannot settle a pending intent stops
there, before it deletes anything else of that instance.
The summary in the `integration.cleanup` audit row counts only what the
finalizing lease did itself; earlier leases and runs are in their own audit
rows. Known limit left open by the fixes: the root's purge and finalization scan
every row of the kinds they check in one transaction, bounded at 200,000 rows
of a kind store-wide (`integrationScanLimit`). The private rows carry no owner
label or prefix to select by, so the operations of every other Integration
count; a store holding more than 200,000 operations cannot finalize a
`Delete` cleanup (`CapacityExceeded` after the provider records are gone; the
operator must release). Lifting it needs an owner-indexed read of the private
kinds. Known limits that this package did not change: a purged source keeps an
empty entry in the inventory state (the inventory has no source removal, and
its removal watermarks reference the source), which counts toward the 128
inventory sources; `Retain` still leaves ledger rows, operations, allocations
and source rows behind; an ordinary lease still holds one of the four default
concurrency slots for its whole lifetime, so a fifth instance, terminating or
not, waits for a slot. On Kubernetes no Integration executes, so `Delete` has
no effect there and the host never arms the finalizer.

**Review and verification state at 68a709e2.** The review accepted DEL-3 and DEL-4 with seven findings, two major, and listed seven weakened guards. The fixer resolved six and rebutted one; the verifier confirmed both major findings resolved and reported **five new problems** (`followups/INT-DEL-B-verify.json`): the docs named the wrong ceiling (the store's 100,000-object or 256 MiB generation read, not 200,000 rows); a long history ran at the 30 s bound and could stall for a resync interval; no mounted test covered a history longer than one stored page; a failed count at the start of a run reported zero remaining; and `e6ab72b4` is typed `test` but changes production code and must travel with `3e71816d`. INT-DEL-C took them up: `fe3afccb` names the store limit, `35feaacd` fits the forget commits to the run bound, `1969930c` adds a mounted multi-page SQLite test, its changelog draft states the kept counts after a failed first count, and both commits of the `e6ab72b4` pair are merged. INT-DEL-C's verifier checked its own review finding, not these five item by item; INT-OPEN is to confirm each in the merged code and has not run.


### F09b/F60c Integration `Delete` lifecycle, part 3 of 3: API/CLI, release command, operator docs and mounted acceptance — November 10, 2026 (D367)

This package lands DEL-5 (API and CLI terminating state and the reviewed
release command), the code-side half of DEL-6 (operator guide, architecture
page and diagram, CRD field description), and the rest of DEL-7 (mounted
restart, failover and license-expiry acceptance). The final D367 row is in
`out/INT-DEL-C/decision.md` and supersedes the drafts of INT-DEL-A and
INT-DEL-B: it adds the operator surface and the release command and changes
nothing that those packages decided.

API and CLI (DEL-5). `IntegrationSummary` (`GET /integrations`,
`GET /integrations/{namespace}/{name}`, `kudurru integrations get|list`)
carries `deletionPolicy`, `terminating` and `deletionRequestedAt`, read from
the stored object, and, while the host's status for that exact revision is
`Terminating`, its `remaining`, `blocked` and `uncertain` counts.
`kudurru integrations list` adds the columns POLICY, REASON and DELETION
REQUESTED. Check, preview, sync and act on a terminating instance were already
refused by the host (`ErrFenced`); they now have a mounted test and answer
`409` with no provider request. The new route
`POST /integrations/{namespace}/{name}/release` and
`kudurru integrations release NAME --uid --revision` are root's
(`integrationRuntime.IntegrationRelease`, `cmd/kudurru/integration_release.go`):
the object in the request's published snapshot and the one the command reads
must both be the reviewed UID and resource version, be deleting and carry the
reserved cleanup finalizer (under any policy). One current command
re-authorizes the original credential for the new permission
`integrations.release` (admin only), counts what stays, removes the finalizer
with `CommandTx.UpdateFinalizer` by that exact identity and writes an
`integration.release` audit row (`released without cleanup by the reviewed
release command: deletionPolicy is ...; removed kudurru.io/integration-cleanup.
Retained for uid ...: N ledger rows, M operations, ...`). The answer is
`{released, ledgerRows, operations}`. It needs no license, credential or
provider and does not take the per-instance lease: a running cleanup is
fenced at its next effect, and a provider request already in flight leaves
its ledger row pending. A live object, a stale UID or revision and a follower
(not the elected writer) are `409`; mode, digest or params in the body are
`400`; a viewer is `403`. The route is refused in static mode like sync and
act. Decided here (in D367): the release removes the finalizer instead of
rewriting `spec.deletionPolicy` to `Retain` as the map suggested, because an
API spec write would have to impersonate the file reconciler's manager or
take the object over with `Force` and would rewrite what the operator
declared, while the exact-identity metadata write is the one the cleanup
itself uses; and it has its own permission rather than reusing
`integrations.sync`, because abandoning the cleanup promise is a different
authority than publishing.

Fix found by the acceptance. A pending intent that a complete reread showed
as completed (no receipt recorded) was settled by `RecordReceipt` with
`ObservedAt` straight from `time.Now`; the SQLite command returns the written
object with the monotonic reading, the store reads it back without it, and
`Confirm` compared the two with `reflect.DeepEqual` and refused with
`ErrInvalid`. The intent then stayed pending forever (`CleanupFailed` in a
cleanup, `Degraded` in an ordinary sync). `fix(integration): record recovery
receipts in UTC` records it in UTC without a monotonic reading, as the
adapters already do (`ee/integration/host/engine.go`).

Five problems the verifier of INT-DEL-B reported (`followups/INT-DEL-B-verify.json`,
`newProblems`), each resolved here:

1. The documented history ceiling. `docs/deploy/integrations.md` and
   `docs/architecture/integrations.md` said an instance's history has no
   limit and that the open limit is the 200,000-row scan of the purge and
   the finalization. Root's capture starts with `ReadGeneration`, which
   refuses a store of more than 100,000 objects or 256 MiB of documents of
   all kinds (`replication.Limits` defaults; `ReplicationLimits` is never
   set outside the store package), so no Integration job gets a frame past
   it and the 200,000-row bound is never reached first. Both pages now name
   the store limit as what bounds a history (and the comment at
   `integrationScanLimit` says the same). This is by code reading, like the
   verifier's finding: no test fills a store to 100,000 objects (the
   verifier's probe ran out of memory while seeding).
2. Cleanup of a long history progressing only at the run bound, and
   stalling. Two causes. (a) The `invalid input` after a provider delete cut
   before its receipt is the monotonic-clock receipt that
   `fix(integration): record recovery receipts in UTC` (cce5d712) fixes: the
   next run confirms the delete by a reread and records the receipt, instead
   of `CleanupFailed` until the resync interval. (b) A `Forget` commit
   carried up to 1,024 operations whatever the store's speed, so a commit
   that cannot finish in one run's bound was cut at the same place every run
   and reported as `ProviderFailed` (the verifier's 5 s host probe never
   progressed). Now a run that its bound cuts inside a `Forget` without any
   progress sets the lease's commit size to half of what that `Forget` tried
   and runs again at once (`CleanupInProgress`), and a run that made
   progress yields before its next commit once less of its bound is left
   than one and a half times the last commit took, or an eighth of the
   bound. Provider deletes come before any history work in a run, so the
   history no longer uses up the bound before them.
   (c) Found by the review of this package: a run that its bound cut
   without progress in any other local step (a ledger count, a ledger or
   history read, a fence, an intent, receipt, confirmation or abandonment
   commit of the shared apply and recovery path, the purge or the
   finalization) was still reported as `ProviderFailed`, as the reviewer's
   second race run of the multi-page test showed
   (`Terminating/ProviderFailed` with `pending=1`, a cut intent or receipt
   commit). The lease now notes a cut local step
   (`terminatingLease.local`; `applyPlan` and `recover` take the lease, nil
   in a sync, whose behavior is unchanged), and such a run reports
   `CleanupFailed` with the ordinary temporary-failure backoff.
   `ProviderFailed` stays for a run cut inside a provider request,
   including the fence and budget charge the request itself makes.
3. No mounted test of a history longer than one stored page:
   `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` (real SQLite,
   keyset pages, 1,001 operation rows over four pages).
4. The failed count overwriting the kept counts: a run whose first ledger
   count fails is now uncounted, like a fenced run, and keeps what the last
   run reported.
5. Commit hygiene: see the note to the integrator (e6ab72b4 and 3e71816d
   must be picked together).

Docs (DEL-6, code side). `docs/deploy/integrations.md`: the summary fields,
the `409` refusals, resumption after restart, failover, license expiry and a
missing credential, and the release command with its permission and audit
row. `docs/architecture/integrations.md`: the release command in the
lifecycle state diagram and the terminating-job section; the paragraph that
said no mounted restart or promotion test existed now names them. The Go
comment of `spec.deletionPolicy` (and so the three CRD copies and the CRD
reference) states what `Retain` and `Delete` do and that the policy has no
effect on Kubernetes yet. `grep -r "refused before provider work" docs
docs-site/content` returns nothing.

Evidence under `.context/v020-final-20261110/evidence/INT-DEL-C/`:

- Review fixes (bd7a5658, 99862457, docs 643c7350 and 0fac45e0), all
  under race. New host tests `TestTerminatingRunCutInALocalReadIsNoProviderFailure`
  (stalls the operation reads after the provider delete; 2.1 s) and
  `TestTerminatingRunCutInALocalCommitIsNoProviderFailure` (stalls the
  intent write before any provider delete; 1.1 s), `fix-host04-local-commit`.
  Each fails without its change: `fix-negative02-local-cut-before`
  (before bd7a5658: times out in `Terminating/ProviderFailed`, 10.1 s) and
  `fix-negative03-local-commit-unnoted` (overlay with the noting in
  `applyPlan` and `recover` disabled, worktree unchanged: times out in
  `Terminating/ProviderFailed`, 10.1 s). `fix-negative01-local-cut-before`
  is a retained first draft of the read test that passed before the fix:
  it stalled the first ledger count, which a lease reaches before it has
  inspected its provider, so it never took the publisher branch; the test
  was moved to the operation read. `fix-host05-package`: `ee/integration/host`
  54.8 s and `ee/integration/ledger` 6.5 s, ok. Scoped lint `fix-lint03`:
  0 issues over `./ee/integration/host/...` (`fix-lint02` only met another
  lane's parallel golangci-lint and did not run). Mounted:
  `fix-mounted01-selection` (after bd7a5658 only; 13 of 13 passed, 207.7 s,
  but the multi-page test still showed `Terminating/ProviderFailed` with
  `pending=1`, which led to 99862457) and `fix-mounted02-selection-x2` on
  the final tree (`-race -count=2 -run 'TestIntegrationDelete|TestIntegrationAdminTerminatingStateAndReviewedRelease'`):
  26 of 26 passed, 0 skipped, 387.1 s;
  `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` 94.0 s and
  87.3 s, with the status sequences Election, CleanupInProgress,
  AuthorityChanged and Election, CleanupInProgress, and no
  `ProviderFailed`. The only `ProviderFailed` lines in that log are the
  `unreadable` and `ambiguous` subtests of
  `TestIntegrationDeleteResumesAfterRestart`, whose peer answers with
  errors on purpose. No generated artifact changed.

- Final mounted selection under race through the lock on the committed
  tree (`mounted13-selection-committed`, same `-run` as below plus the
  drift tests): 34 top-level tests, 34 passed, 0 skipped, 326.2 s, including
  `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` (104.3 s),
  `TestIntegrationAdminTerminatingStateAndReviewedRelease` (5.8 s),
  `TestIntegrationDeleteResumesAfterRestart` (16.7 s),
  `TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes` (7.5 s),
  `TestIntegrationDeleteLicenseExpiryPausesCleanup` (18.3 s),
  `TestIntegrationDeleteRetainLeavesProviderRecordsAndLedger` (3.9 s),
  `TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes`
  (5.4 s), `TestIntegrationDeleteCloudflareCleanupRemovesOnlyOwnedRecordsAndFinalizes`
  (4.9 s, foreign and unowned rows byte-identical) and
  `TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes` (10.2 s,
  release by declaring `Retain`). (The log's last line says `exit=0` from
  the grep of the pipeline; the `ok ... 326.162s` line is the test result.)
- Long-history mounted test: `mounted10-history-pages` (retained, failed
  on the test's own page count, off by one, before any cleanup),
  `mounted11-history-pages` (retained, failed at the fixture's 15 s await
  before any provider delete: under race each root capture of a store this
  size is slow), `mounted12-history-pages` (passed, 88.1 s, three runs of
  one lease, finalized 33 s after the delete request), then `mounted13`.
- Host (`host01-new-tests`, `host02-package`): `ee/integration/host` under
  race 52.7 s and `ee/integration/ledger` 5.7 s, exit 0;
  `TestTerminatingForgetCommitsShrinkToTheRunBound` 17.7 s.
  Negative controls by build overlay, worktree unchanged:
  `negative02-forget-no-shrink` (no halving: times out after 91.7 s in
  `Terminating/ProviderFailed` with `Remaining:1`, the verifier's symptom)
  and `negative03-uncounted-ledger-failure` (a failed count reports
  `Remaining:0, Conflicts:0` after `Remaining:1, Conflicts:1`).
- Scoped lint through the lock (`lint02`): 0 issues over `./cmd/kudurru/...`
  and `./ee/integration/host/...`. Drift on the final tree: `drift02-committed`.
- Earlier mounted selection under race through the lock
  (`mounted07-selection`, `-run '^(TestIntegrationDelete|TestIntegrationAdmin|TestIntegrationCLI|TestIntegrationRuntimeDelete|TestIntegrationGuard|TestIntegrationRoot|TestIntegrationForget|TestIntegrationRuntimeActual|TestSQLiteObjectSourceIntegrationCleanupFinalizer|TestCLIReferenceDrift)'`):
  34 top-level tests, 33 passed and one failed, 225.2 s. The failure was
  the first version of `TestIntegrationAdminTerminatingStateAndReviewedRelease`,
  which read the summary before the job's own status write had been
  published (its revision lagged the stored object); the test now waits until
  the summary names the stored revision, and `mounted08-admin-x3` passed it
  3 of 3 (exit 0, 21.1 s). Every other test of the selection passed in
  `mounted07`, including all INT-DEL-B mounted tests unchanged and:
  `TestIntegrationDeleteRetainLeavesProviderRecordsAndLedger` (4.5 s),
  `TestIntegrationDeleteResumesAfterRestart` with subtests `unreadable` and
  `ambiguous` (19.7 s), `TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes`
  (9.2 s), `TestIntegrationDeleteLicenseExpiryPausesCleanup` with subtests
  `renewed` and `released` (18.9 s), and `TestIntegrationAdminCLIActualRouter`.
- What the new tests prove. Restart: the whole serving runtime is closed,
  the SQLite file closed and reopened, and a new runtime started; while the
  record list was unreadable nothing was deleted, and after a batch applied
  but answered 503 the intent stayed pending and was not resent; after the
  restart exactly one delete batch in total, each published provider ID
  deleted once, no ledger or operation row, budget row kept. Failover: the
  replica is installed from the image taken while the delete batch is held
  at the peer and then promoted; the held batch completes at the peer, the
  old store still has the pending intent, the finalizer and no
  `integration.cleanup`; the promoted writer finishes with no second batch
  and writes its `integration.cleanup` summary. License expiry: the license
  clock passes expiry plus grace while the batch is held; the cleanup is
  `Terminating/LicenseUnavailable`, the peer's request counter does not move
  across a wake and 1.3 s, the object keeps the finalizer; a renewed license
  finishes with exactly one batch, and the reviewed release without a license
  removes the object with no request and keeps the ledger row. Retain
  control: the object is gone in the delete request, no request reaches the
  peer, both published rows and the ledger row stay, and no cleanup or
  release audit row is written. API/CLI: see above; the release keeps both
  published rows at the peer and the ledger row, and the audit trail has
  `integration.release` and no `integration.cleanup`.
- Retained failed runs: `mounted01-first-run` (exit 1, 83.7 s: the failover
  test failed with `CleanupFailed`, and the admin test failed on a live-object
  `statusKnown` assertion that was wrong, see "Known gaps"; the restart,
  license and Retain tests passed). `mounted02-diagnostic-overlay` to
  `mounted06-diagnostic-overlay` are diagnostic runs by build overlay
  (worktree unchanged) that added the error to the host's log line and then
  located it: `integration ledger: invalid input at repository.go:162`, the
  `reflect.DeepEqual` of `currentOperation` inside `Confirm`, called from
  `recover` at `engine.go:687`; the two receipts differed only in the internal
  representation of `ObservedAt` (equal by `Time.Equal`, both UTC). That led
  to the fix commit.
- Negative controls by build overlay with the worktree unchanged:
  `negative04-release-admits-live-object` (the release check without the
  deletion timestamp and finalizer clauses) and
  `negative05-recovery-receipt-monotonic` (cce5d712 reverted) each fail
  their target test: `TestIntegrationAdminTerminatingStateAndReviewedRelease`
  fails with "a live object was released 200" (4.0 s), and
  `TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes` stays
  `Terminating/CleanupFailed` with `Pending:1` on the promoted writer
  (22.0 s).
  (`negative01-release-admits-live-object` and `mounted09-selection-committed`
  are empty: the earlier run of this package was cut off by the
  machine restart before they wrote anything.)
- Libraries under race (`race01-libraries`): `ee/integration/host` 40.8 s,
  `internal/api` 264.6 s, `internal/apiclient` 1.8 s, `internal/rbac` 1.0 s,
  exit 0. `go vet ./cmd/kudurru ./internal/api` clean (`vet01`). Scoped lint
  through the lock (`lint01`): 0 issues over `./cmd/kudurru/...`,
  `./internal/api/...` and `./ee/integration/host/...`. `gofmt -l` clean.
- Generated (`gen01`, `gen02-docs`): `make generate`, `make client-go`,
  `bun run gen:api`, `make docs-gen`; `make rbac-manifests` and
  `make deploy-assets` changed nothing. Drift on the committed tree
  (`drift01-committed`): `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestReferenceDrift`, `TestAggregatedRolesDrift`, `TestCLIReferenceDrift`.

What remains open. The Kubernetes half (DEL-8): no Integration executes on
Kubernetes, so `Delete` has no effect there, nothing arms the finalizer, and
the release route is not mounted (the integration administration exists only
on SQLite). The tracker rows (completion-plan.md:94 and :185, gaps.md,
followup-audit.md:82/587), the decision log, the changelog and the stale
sample at dev-docs/design-record.md:1062 (it nests `deletionPolicy` under
`spec.cloudflare`; the type has it at `spec.deletionPolicy`) are the
integrator's to edit. Not covered by a mounted test: an ambiguous batch
resolved by the same writer without a restart (host-level only), replica
promotion of a UniFi or webhook instance's cleanup, the SDK-level bound of 200
effects per batch and one budget charge per HTTP attempt (adapter tests
only), a credential rotated between a partial delete and its recovery
(host-level only), and the release racing an in-flight batch (the outcome is
documented, not tested). Known gap found and not changed: the summary of a
live instance shows `statusKnown: false` after the instance's own status
write, because the host's status keeps the revision it captured; only a
terminating instance's report follows the object's revision (INT-DEL-B), so
the dashboard lane should not treat `statusKnown: false` on a live object as
an error. The known limits INT-DEL-B listed (the empty inventory source entry, the
rows `Retain` leaves, the per-lease concurrency slot) are unchanged; the
200,000-row scan bound it listed is replaced by the store limit above (100,000
objects or 256 MiB of all kinds), which is not raised here and stops every
Integration job, not only the cleanup. A long history still costs one run
bound per few commits on a slow store (the 26,000-operation SQLite probe of
the verifier took 6 min 51 s); this package makes such a cleanup converge,
not faster, and the 26,000-operation case was not rerun after the change.
The halved commit size never grows again within the lease (a new lease
starts at 1,024). A slow store can still show a transient `CleanupFailed`
with backoff before such a cleanup converges, and a run cut inside a provider
request by a slow local fence or budget charge that the request makes still
shows `ProviderFailed`.

**Review and verification state at 68a709e2.** The review accepted the package with two findings; the fixer fixed the minor one (a terminating run cut in a local step was reported as a provider failure). The second verifier confirmed it resolved (mounted `TestIntegrationDelete` selection 26 of 26 under race, twice) and reported **two new problems, open**: (1) minor, a regression in the opposite direction: `applyPlan` and the receipt after `resume.Recover` (`ee/integration/host/engine.go`) now call `st.local` on a save error, so a provider request that hangs to the run bound and returns a partial receipt with its error (the Cloudflare adapter does this) is reported as a local `CleanupFailed`; (2) cosmetic, two unwrapped long lines. Still not done: the Kubernetes integration host (F09b-K0..K8, DEL-8), so `deletionPolicy` has no effect on Kubernetes. The release route needed a follower class at merge (`68a709e2`).


### OBS-5 ClickHouse and Loki query-log sinks — November 10, 2026 (D371)

`querylog.sinks` gains two types beside `slog`, both built on the sink host of
OBS-1. `type: clickhouse` sends one `INSERT INTO <db>.<table> FORMAT
JSONEachRow` per batch to the ClickHouse HTTP interface
(`internal/querylog/sinks/clickhouse`); `type: loki` sends one JSON push per
batch to `/loki/api/v1/push` (`internal/querylog/sinks/loki`). No SDK is used:
the shared client (`internal/querylog/sinks/httpbatch`) is the standard library
HTTP client, one POST per batch on its own connection, gzip by default,
redirects never followed. Both export the same flat JSON record per query
(`internal/querylog/sinks/record`), built from the event after the exclusions
and the privacy transform, so a destination holds exactly what the local query
log keeps and `nothing` exports nothing. Loki streams carry only the fixed
labels of the settings, `node` and `status_class`; client, client name,
domain, query type, status and trace ID are structured metadata.

A batch is marked retryable (and repeated within the sink's attempt budget)
only when no connection was established or the destination answered 408, 429
or 5xx, with `Retry-After` honored up to the host's 30 s cap. A 5xx is not
proof that nothing was stored (a gateway's 502 or 504, a ClickHouse insert that
failed part way); the repeat is accepted because the documented table and Loki
drop the duplicate, and `attempts: 1` turns it off. A body of 64 KiB or more
asks for `100 Continue` first, and the client's connections hide the write
error of a peer reset, so that an answer a destination sent before resetting
the connection under the body is read (ClickHouse sends `100 Continue`, then
refuses a wrong password and resets). A 2xx is believed only once the
transport reports the body written whole on a connection that was not reset
(bounded at 5 s); otherwise it is an unknown outcome, dropped and not repeated. Every other
answer, and a request that was sent and got no answer, drops the batch and is
counted per sink in `kudurru_querylog_dropped_total{stage="sink",sink}` and
`kudurru_sink_errors_total{sink}`. Errors name the status and, for ClickHouse,
the numeric `X-ClickHouse-Exception-Code`; the response body is never read.

Settings hold credential references only (`passwordRef`, `bearerTokenRef`,
`caRef`); a URL with user information or a query is refused without repeating
it. The settings validator refuses everything a sink factory would refuse, and
refuses a Secret reference on any driver but Kubernetes. At runtime a file or
environment variable must be granted by the node's own
`credentials.allowedFiles` / `credentials.allowedEnvironment`; on Kubernetes a
Secret is read with the server's ServiceAccount, only in its own namespace and
only with the label `kudurru.io/query-log-sink: "true"`
(`cmd/kudurru/querylog_sink_credentials.go`). A password or token is read when
the sink opens and again after a 401 or 403; a changed value makes the refused
batch retryable, which is how rotation is picked up. `caRef` is read only at
open. A referenced value holds at most 64 KiB (the resolver's bound, which the
CA check now matches); a larger one is refused with `credential is
unavailable: it holds more than 64 KiB` (`credentials.ErrTooLarge`, still
`ErrUnavailable`). The help of `credentials.allowedFiles` and
`credentials.allowedEnvironment` and the operator page say that a grant is not
bound to a destination. `docs/deploy/query-log-sinks.md` documents the record, the ClickHouse
table DDL, the Loki label and structured-metadata mapping, credentials and
retry rules; tests keep the page and the code in step.
`docs/architecture/observability.md` gains the HTTP sink flow and diagram.

Evidence under `.context/v020-final-20261110/evidence/OBS-HTTP-SINKS/`:

- `root-race06` (`-race -count=1`, regex over every query-log test, the reload
  subscriber tests, `TestForwardedShapes`, `TestCLIReferenceDrift`,
  `TestNoTemplateExecution` in `./cmd/kudurru`): exit 0, 22 top-level passes,
  no skips, 148 s. Includes the mounted
  `TestRunServeQuerylogClickHouseAndLokiSinks` (SQLite: the settings command
  adds both sinks with granted files, ClickHouse behind TLS with a private CA;
  real UDP queries exported and compared with `GET /queries`; 503 repeated
  within the budget, 400 dropped at once; rotated password picked up; no
  credential value under the data directory) and
  `TestRunServeKubeQuerylogSinksReadSecretCredentials` (envtest: Secret keys
  named in the settings file, the Secret rotated through the API server, an
  unlabeled Secret never sent).
- `root-race07` (`-race -count=2`, the five new root tests): exit 0, 10 of 10
  passes (`TestRunServeQuerylogClickHouseAndLokiSinks` 20.2 s and 19.2 s,
  `TestRunServeKubeQuerylogSinksReadSecretCredentials` 10.0 s and 9.1 s).
- `lib-race01`: `-race -count=1` over `./internal/querylog/...` and
  `./internal/config/...` (11 packages ok, among them `sinks`, `clickhouse`,
  `httpbatch`, `loki`, `record`; `sqlitelog` 330 s), plus
  `TestCandidateReturnsItsQueryLogSinkReferences` in `internal/servingbuild`
  and `Settings|Schema|OpenAPIDrift` in `internal/api`: all exit 0. The two
  `TestLive*` tests skip there (no server URLs set).
- `live01` to `live03`, `live-probe01`, `live-setup.txt`: the optional
  `TestLiveClickHouseReadsTheDocumentedTable` and
  `TestLiveLokiServesTheDocumentedMapping` against real ClickHouse
  26.11.1.1867 and Loki 3.7.9 run as plain local processes on loopback (no
  container). `live01` is a retained failure caused by the test's own database
  renaming (fixed before commit); `live02` and `live03` (`-count=2`) pass.
  `live-probe01` records the status and exception codes the operator page
  names (403/516 wrong password, 403/497 missing grant, 404/60 and 404/81
  missing table or database, 400/41 unparsable row; Loki 204 for repeated
  pushes, 400 for an entry too old).
- `mutation01` (six mutations of `httpbatch/client.go`: an unknown outcome
  marked retryable, redirects followed, 4xx retryable, 5xx not retryable, the
  refused credential not reread, the response body in the error) and
  `mutation02` (three overlays of `querylog_sink_credentials.go`: an ungranted
  file read, a Secret used without the label, no Secret reader on Kubernetes):
  every mutation is caught by at least one test; the restored tree passes.
- `lint01`: retained failure, six gosec G101 findings on made-up test
  credentials and one revive unused-parameter in tests; fixed with annotated
  `//nolint:gosec // G101: ...` comments and `_`. `lint02` (retained) then
  showed two more G101 findings (the Secret label name, a rotated test
  password), fixed the same way; `lint03` was refused because another lane's
  golangci-lint was running; `lint04` (`--max-issues-per-linter=0
  --max-same-issues=0` over `./internal/querylog/...`, `./internal/config/...`,
  `./cmd/kudurru/...`): 0 issues. `light-race02`: `-race` over
  `./internal/querylog/sinks/...` and `go vet` after the fix, exit 0.
- `drift01` (after `gen03-make-generate`, `gen04-docs-gen`): `TestOpenAPIDrift`,
  `TestReferenceDrift`, `TestAggregatedRolesDrift`,
  `TestSettingsAdmissionContainsOnlyPublicClusterKeys`, `TestCLIReferenceDrift`,
  `internal/store/validator` and `deploy`: all pass. `size01`: `bin/kudurru`
  81,350,816 bytes with `CGO_ENABLED=0` (budget 100,000,000). `docs-site01`:
  Hugo builds 164 pages with `--panicOnWarning`.
- Retained interrupted runs: `root-race01-withdrawn` (terminated), `root-race02`
  (a mount-test PATCH refused with 400 before the validator and test were
  aligned), `root-race05` (cut off by the machine restart; rerun as
  `root-race06`).

Review fixes (after the accept-with-fixes review of 8c090646), logs `fix-*`:

- `fix-mutation01` to `fix-mutation03`: with the `Expect` header removed,
  `TestPostLargeBatchSeesARefusalAtTheHeaders` fails (69,481 body bytes
  written into a refusal, the early 503 lost in 8 of 20 attempts against the
  new `sinktest` mode `AnswerBeforeBody`). `fix-light-race01`: the fixed tree,
  `-race -count=5`, passes.
- `fix-live-probe01`: real ClickHouse 26.11.1.1918 on loopback, wrong
  password, 20 posts per case with `Expect` only: the 403 was still lost in 0,
  1, 1 and 5 of 20 at 1000 rows, 5000 rows gzip, 5000 and 10000 rows plain,
  because ClickHouse sends `100 Continue` before it checks the credential and
  then resets the connection (`fix-live-setup.txt`, curl trace). This led to
  the connection wrapper (`answerConn`). `fix-mutation06` and `fix-mutation07`:
  with the write error passed on again, `TestPostLargeBatchSeesARefusalAfterContinue`
  (new mode `AnswerAfterContinue`) loses the 503 in 3 of 20 and 22 of 50
  attempts. `fix-live-probe02` and `fix-live-probe03` (100 posts per case):
  every 403 arrives; a correct password stores each batch in 37 to 91 ms.
- `fix-live01`, `fix-live02`: `TestLiveClickHouseReadsTheDocumentedTable` and
  `TestLiveLokiServesTheDocumentedMapping` against ClickHouse 26.11.1.1918 and
  Loki 3.7.9 (plain processes, `KUDURRU_REQUIRE_SINK_SERVERS=1`) pass; a
  1.6 MB push to Loki through the client (the `100 Continue` path) is stored
  in 106 ms plain and 219 ms gzip, so Loki answers `100 Continue` without the
  2 s wait. The probe file (`fix-live-probe.go.txt`) was applied with
  `-overlay` only.
- `fix-light-race04` to `fix-light-race07`: retained flaky runs of the first
  version of `TestPostLargeBatchSeesARefusalAfterContinue` under machine load
  (another lane's `-race -count=30`): its "a body was cut" check failed in 5
  of 40 (`fix-light-race06`) and 2 of 40 (`fix-light-race07`) runs. The cause
  was a real gap: Go's transport returns an answer that arrives while the body
  is still being written, so a 2xx at the headers could be taken for a stored
  batch. Fixed by waiting for the transport's `WroteRequest` report before a
  2xx counts; `fix-light-race08` (`-race -count=40`) passes 40 of 40, and
  `fix-mutation08` (every 2xx believed) fails at the first attempt.
  `fix-light-race09`: `-race -count=3` over `internal/credentials` and
  `./internal/querylog/sinks/...`, all pass.
- `fix-mutation04` (CA bound back to 1 MiB) and `fix-mutation05`
  (`internal/credentials` without `ErrTooLarge`) fail the new size checks.
- `fix-light-race02`, `fix-light-race03`: `-race` (`-count=2`) over
  `internal/credentials` and `./internal/querylog/sinks/...`, all pass.
  `fix-build-vet01`: `CGO_ENABLED=0 go build ./...`, `go vet`, `gofmt -l`
  clean.
- `fix-lint01`: retained failure, two staticcheck QF1008 findings in
  `sinktest` (embedded `Reader` in a selector), fixed in the last source
  commit; `fix-lint02` was refused (another lane's golangci-lint was running);
  `fix-lint03` over `./internal/querylog/...`, `./internal/credentials/...`,
  `./internal/config/...`: 0 issues.
- `fix-gen01-docs-gen` (`make docs-gen`, only the two `credentials.*` rows of
  `docs-site/content/reference/configuration.md` changed), then
  `fix-drift01` (`TestReferenceDrift`,
  `TestSettingsAdmissionContainsOnlyPublicClusterKeys`, `internal/api`
  `Settings|Schema|OpenAPIDrift`, `-race` over `internal/config`,
  `internal/store/validator`, `deploy`) and `fix-drift02`
  (`TestReferenceDrift` on the final tree): all pass.
- `fix-root-race01` (before the last source commit) and `fix-root-race02`
  (final tree), `-race -count=2` in `./cmd/kudurru` over `TestQuerylogRuntime*`,
  `TestQuerylogSink*`, `TestRunServeQuerylogClickHouseAndLokiSinks`,
  `TestRunServeKubeQuerylogSinksReadSecretCredentials`, `TestCLIReferenceDrift`
  and `TestProviderStatic*` (the stored-provider users of the resolver): 14
  top-level tests, 28 of 28 passes each, 86 s.
- `make test-sinks` (Makefile, CONTRIBUTING.md) starts ClickHouse and Loki in
  Docker and runs the live tests; Docker is not running on this machine, so
  the target itself was checked only with `make -n`.

Open: no dashboard view of sink state or credential errors (state is metrics
and log warnings only; `querylog.SinkHost.Status()` is not mounted on a route);
the settings mock `dashboard/src/mocks/data/settings-schema.json` does not
carry the new nested objects; no Grafana panels or alerts for the per-sink
series. The ClickHouse table is created by the operator, never by the server.
The `caRef` bundle is read only when a sink opens, so a rotated CA needs a
settings change or a restart. `make test-sinks` exists but was never run (no
Docker daemon here) and is not wired into CI; its images
(`clickhouse/clickhouse-server:25.8`, `grafana/loki:3.7.9`) are unverified
defaults. 502 and 504 are still repeated like any 5xx. The full `./cmd/kudurru` suite, `test/e2e`, the performance gate and hosted CI
were not run in this lane. Mixed-version acceptance of `querylog.sinks`
entries of the new types by older nodes was not tested.

**Review and verification state at 68a709e2.** The review found no blocker or major defect and five minor findings (a reset after an early answer lost the answer; the unreachable 1 MiB CA bound; the credential grants' help text; the changelog's "certainly not stored" for a 5xx; the live tests had no make target). The fixer reports all five fixed in eight commits (`make test-sinks` among them). **The fix commits were never verified.** OBS-OPEN is to verify them adversarially; it has not run.


### OBS-2 dnstap emit with the audited raw override — November 10, 2026 (D372)

A `querylog.sinks` entry of `type: dnstap` now sends every recorded query as
`dnstap.Dnstap` messages in Frame Streams to a collector on a Unix socket
(`unix:/path`), TCP (`tcp:host:port`) or TLS (`tls:host:port`, optional `caRef`,
`certRef`+`keyRef`, `serverName`). It runs on the D371 sink host like the HTTP
sinks: its own bounded queue, batches, timeout and attempts, so a collector that is
down or stops reading loses only its own messages, which are counted, and the sink
reconnects with the next batch. The sink writes the bidirectional handshake (READY,
ACCEPT, START, data frames, STOP, FINISH). Local zone and local record answers are
`AUTH_QUERY`/`AUTH_RESPONSE` (with `query_zone`), all others
`CLIENT_QUERY`/`CLIENT_RESPONSE`; `types` selects them, and the opt-in
`FORWARDER_QUERY`/`FORWARDER_RESPONSE` pair summarizes an upstream exchange from
what the event knows (question, rcode, the client's times, the upstream in `extra`).
Identity defaults to the node name, version is `kudurru/<version>`, `extra` carries
status, list, upstream, client name and trace ID. The protobuf encoding and the
framing are hand-written in `internal/querylog/sinks/dnstap`; no module was added to
the binary (`google.golang.org/protobuf`, already in the build, became a direct
requirement only for the schema test). The test collector (`dnstaptest`) reads
the framing with its own strict Frame Streams reader, written from the fstrm
description and importing nothing from the sink, and a golden test pins
hand-written handshake bytes to both that reader and the sink's encoder.

Default output follows `querylog.privacy`: each message is rebuilt from the
transformed event (question, header flags, rcode, never answer records, the logged
client address, no port), `hidden.` names under the hiding modes, and nothing under
`nothing`. `raw: true` sends the query as received and the answer as the pipeline
produced it, with the real client address and port, under every privacy mode.
Exclusions (`querylog.exclude.*`, the health name, the built-in dig) run first and
bind raw too. Raw is admin-only: a settings change of `querylog.sinks`, or of any
`querylog.exclude.*` key, that leaves a raw sink in place needs the new permission `querylog.raw` (only `admin`, through
`*.*`) on top of `settings.write`; the API audits a refusal as denied and the
accepted change as `querylog.raw`, and the complete SQLite runtime re-authorizes
`querylog.raw` against the original credential inside the commit transaction. Every
node records `querylog.raw.export` before a raw sink opens, so raw from the settings
file or a ConfigMap is audited too; a raw sink whose audit event is not recorded
does not connect, and the host retries the open. DNS wire bytes exist only while the
route holds a raw sink: the log observer asks the route (`WireRetainer`) and packs
only then, the collector copies the event only for such a route and clears
`Event.Wire` before the primary sink, the tail and every other sink.

Evidence under `.context/v020-final-20261110/evidence/OBS-DNSTAP-EMIT/`:

- `race08.log`: `go test -race -run 'TestQuerylog|TestRunServeQuerylog|TestOpenAPIDrift|TestReferenceDrift|TestCLIReferenceDrift|TestSettings' ./cmd/kudurru`, 22 tests pass in 109.6 s, among them `TestRunServeQuerylogDnstapSinkPrivacyAndRaw` (14.6 s: mounted serve on SQLite under `hide_domains_clients`, sink added through PATCH /settings, real UDP queries read from a Unix socket Frame Streams reader; hidden name, `0.0.0.0`, no port, no answer without raw; the exact exchange with ID, answer, `127.0.0.1` and a port with raw; nothing for `*.skip.example` either way; STOP/FINISH on every replaced stream), `TestQuerylogRuntimeRawDnstapWaitsForItsAuditEvent`, `TestQuerylogSinkTypesHaveFactories`, the existing sink mount tests and `TestCLIReferenceDrift`.
- `race06.log`, `race07.log`, `race09.log`: `-race` over `internal/querylog`, `internal/querylog/sinks/...` (including `dnstap`: `TestSinkFrameStreamsOverUnixTCPAndTLS`, `TestSinkRawSendsTheWireMessages`, `TestSinkRawRequiresItsAuditEvent`, `TestSinkTypesAndPrivacy`, `TestSinkReconnectsAndDropsBoundedOnTheHost`, `TestFrameMatchesTheDnstapSchema` with `dynamicpb` against a descriptor of `dnstap.proto`), `internal/stages/log` (`TestObserverRetainsWireOnlyForARawRoute`), `internal/config`, `internal/rbac`: all pass.
- `unit02-api.log`, `unit03-api.log`, `race04-api.log`: `TestSettingsRawDnstapSinkNeedsQuerylogRaw` (a `settings.write`-only principal is refused with 403 and audited denied twice, the administrator's change is audited as `querylog.raw`, turning raw off needs only `settings.write`); the full `internal/api` race run passed in 256 s before the lint-only edits.
- `fuzz01.log`, `fuzz02.log`: `FuzzDnstapEncode` and `FuzzFrameStreamRead` (test/fuzz, listed in `hack/fuzz.sh`) ran 60 s and 30 s each without a finding.
- `lint04.log`: scoped golangci-lint over `internal/querylog/...`, `internal/stages/log/...`, `internal/config/...`, `internal/api/...`, `test/fuzz/...`, `cmd/kudurru/...`: 0 issues. `race10.log`: dnstap and test/fuzz raced again after the lint edits, pass.
- `gen02.log` then `drift02.log`: after the generated commit, `TestCLIReferenceDrift`, `TestAggregatedRolesDrift`, `TestReferenceDrift` and `TestOpenAPIDrift` pass. `drift01.log` is a retained setup failure (package paths given without `./`), not a drift.
- Review fixes (`fix-*.log`):
  - `fix-unit01-api.log`, `fix-race04-api.log` (`-race -count=2 -run TestSettings ./internal/api`, pass in 9.3 s): `TestSettingsRawDnstapSinkNeedsQuerylogRaw` now also refuses a `settings.write`-only principal that changes `querylog.exclude.clients`, `.domains` or `.sudn` while a raw sink is in place (403, audited denied, nothing written; 5 denied and 2 granted `querylog.raw` events in all). `fix-unit02-api-without-fix.log` is the retained run without the gate change: the exclusion PATCH returns 200 with `changed: [querylog.exclude.clients]`. `fix-unit03-config.log`: `TestRawQuerylogChange` pins the shared rule.
  - `fix-cmd01.log`, `fix-race01.log` (`-race -count=2`, 16 tests pass in 52.1 s, with the neighboring `TestServingCurrentCommand*`, `TestServingCanonicalSettings*` and `TestServingBrandingOriginalCredentialRevocationIncludesNoop`): `TestServingSettingsRawDnstapReauthorizesQuerylogRaw` drives the complete SQLite runtime's `WriteSettings`, bypassing the API gate, with a token restricted to a custom role holding `settings.write` but not `querylog.raw`; a raw sink and an exclusion change under one are refused at commit with the generation unchanged, a plain sink by the same credential and both changes by the administrator persist. `fix-cmd02-without-commit-check.log` is the retained run with the loop over the extra permissions disabled: the first step persists and the test fails (its message prints the two values in swapped order; fixed afterward).
  - `fix-race02.log` (`-race -count=2` over the dnstap sink, `dnstaptest`, `internal/querylog`, `internal/stages/log`, `internal/config`, `test/fuzz`): pass, including `TestFrameReaderGolden` and `TestFrameReaderRefuses` and every sink test now read by the independent framing reader. `fix-fuzz01.log`: `FuzzFrameStreamRead` (now differential: the sink's reader must return every frame the strict reader accepts identically) and `FuzzDnstapEncode` (read with the strict reader), 20 s each, no finding.
  - `fix-mount03.log`, `fix-race03.log` (`-race -count=2`, pass; the mount takes 12.2 s and 12.4 s): `TestRunServeQuerylogDnstapSinkPrivacyAndRaw` now excludes the client 127.0.0.2 and queries from it before and after raw is on, and none of its queries reaches the collector; the node holds a test license with the audit feature, and `/audit` returns one `querylog.raw` success by `admin` and at least one `querylog.raw.export` by `System/query-log` for the sink. Retained: `fix-mount01.log` (the test's audit read hit the 32 KiB response cap of the shared request helper; now filtered by kind), `fix-mount02.log` (the test expected a node name on the stored event, which the node's own SQLite audit store does not keep; the check now uses the reason naming the target), `fix-mount04-without-client-exclusion.log` (the exclusion removed: the excluded client's raw query reaches the collector and the test fails).
  - `fix-lint01.log`: scoped golangci-lint over `internal/config`, `internal/api`, `internal/querylog`, `test/fuzz`, `cmd/kudurru`: 0 issues. `fix-gen01.log`, `fix-gen02.log` then `fix-drift01.log`: after the second generated commit (CRD help text), `TestOpenAPIDrift`, `TestReferenceDrift`, `TestAggregatedRolesDrift` and `TestCLIReferenceDrift` pass.
- Retained failures: `race01.log` and `race05.log` show `TestRunServeQuerylogDnstapSinkPrivacyAndRaw` failing with HTTP 400 on PATCH /settings. The cause was the store's CRD validator, whose `querylog.sinks[].type` enum had not been regenerated (`Unsupported value: "dnstap"`); after `make generate` (`gen02.log`) the test passes. The integrator must regenerate after cherry-picking, or the mounted test fails the same way. `race01.log` also shows `TestQuerylogSinkTypesHaveFactories` failing from a working state before the dnstap entry was added to the test. `lint02.log` and `lint03.log` list the lint findings fixed in the last source commit.

Open: no API route, CLI or dashboard surface shows sink state (`SinkStatus.Raw` exists
but `SinkHost.Status()` is still not mounted); no test reads the stream with a
third-party Frame Streams implementation (DNS-collector or fstrm): the independent
reader is a second hand-written implementation in this repository; the raw gate
treats every exclusion change under a raw sink alike, so adding an exclusion also
needs `querylog.raw`; renaming a client that a name in `querylog.exclude.clients`
refers to is governed by `clients.write`, not `querylog.raw`; the settings import of
a migration does not produce `querylog` keys today, so it is not gated;
`FORWARDER_*` messages are summaries, not the real upstream exchange; `dnstap
listen` (OBS-3) is not part of this package; the response wire is the pipeline's
answer before transport truncation or padding; the full `./cmd/kudurru` suite,
`test/e2e`, the paired performance gate (F20d/F63) and mixed-version behavior were
not run in this lane.

**Review and verification state at 68a709e2.** The review found no blocker or major defect and four minor findings; the fixer resolved them, and the second verifier confirmed all four resolved, with no weakened check. One new minor problem is open: under `hide_domains_clients` privacy the mounted test cannot tell an excluded client's raw-off frame from any other and does not count frames, so the mount proves client exclusion only in the raw-on phase (the raw-off case is covered at the collector seam in `internal/querylog/raw_test.go`).


### F28-HA-2 Managed anchor history across promotion, rejoin and conflict resolution — November 10, 2026 (D369)

The map predicted from code reading that a member cannot install the
authoritative branch when its own managed owner generation is equal or higher.
The prediction reproduced. Every authenticated root observation by an elected
writer adds one generation to the accepted owner, and `InstallReplica` and
`ResolveConflict` accepted a different managed owner only with a strictly greater
envelope and pure generation and an update time that was not earlier. Two writers
of one history count separately. `race01` runs the new store tests on unmodified
lane head `f9818a59`: an old primary at owner generation 3 cannot install its
promoted follower's branch at generation 2 or at a different generation 3, a
losing conflict branch at generation 4 cannot adopt the audited resolution's
branch at generation 3, and every install returns
`trustanchors/accepted: original owner changed`. `final-neg08` shows the same
through two mounted runtimes and the production puller: the old primary stays
fenced with `reason="dns_only"`. It would have stayed there until the promoted
writer's generation passed its own, about a day at the real root and never if
that updater had stopped.

A different managed owner is now ordered by authority term first (D369).
`InstallReplica` adopts it when the image's term has a strictly higher epoch than
the installed image's term. `ResolveConflict` and `ResolveAudited` adopt the
branch they keep in the transaction that allocates the new term. Neither compares
generations or update times across branches. Adoption still requires the same
cluster, history, initialization time, policy, seed digest and resolver domain,
and the incoming state must retain every recovery record the member accepted
(`RetainsRecoveryHistory`, unchanged). An unchanged owner, the first install and
the first managed owner over a dormant one are not adoptions and keep their
rules; a managed owner is never replaced by a dormant one; every install inside
one term keeps the strict successor rule. `cluster resolve --offline` keeps the
resolving member's own image (D361), so the adoption there happens on the other
members, through their scheduled pulls under the new term.

No revocation is lost. In the same transaction as the owner rows, the image and
the quarantine of the abandoned branch, the node-local carrier is rebased onto
the adopted owner. It keeps every key the abandoned owner revoked and every key
the member's own supervisor denied, and its floor becomes the adopted generation
minus one. Adoption requires the adopted owner to revoke every one of those keys,
the rule an install inside one term already applies to a pending denial
(`TestAnchorLocalInstallCannotOmitPendingDeny`). Otherwise the install, or a
resolution that would keep such a branch, is refused with `accepted.ErrCAS`
before any write: the member stays fenced, keeps answering DNS-only from its own
branch, which already revoked the key (a follower whose own supervisor denied it
stays retired, as before), and takes no later image of the new term. The first
scheduled pull after the canonical writer has recorded the same revocation
adopts the branch, and the member publishes at once. The first form of this
package installed such a branch and kept its generation retired instead; the
review showed that turned a member answering DNS-only into one answering
`SERVFAIL` to every query, with equal trust safety and the same wait, and the
fix commits replaced it (see "Review fixes" below). A denial receipt that named a generation above the adopted owner
is restated at the adopted owner, because the carrier format (unchanged) cannot
name one; a terminal carrier and a denial that no longer fits its bounded list
stay terminal.

The abandoned branch's floor can lie at or above the adopted generation, so the
retirement gate needed one transition that is not monotone. `Change.StageRebase`
takes the exact floor committed with the adopted owner. It requires an installed
mark of the same domain and a floor that retires every generation below the
target, and a confirmed installation clears the registration. A restart restores
the same gate from the committed carrier. The adopted owner can be one the member
held before (the old primary returns to the owner its promoted follower never
left), and a write attempt captured for that owner before the split would find
its expected mark installed again. The gate therefore counts confirmed rebases
and refuses every write attempt captured before one. The source's exact cursor
identity refuses such an attempt independently; `final-neg07` removes the gate's
count and shows the attempt then stopped by the source with
`original owner changed`. `Stage` keeps its monotone rule.

Lane commits on `wip/v020/f28`: `90fe321b` (gate), `bf3addf9` (store rule, store
tests, architecture and operator docs), `3d374c08` (mounted tests) and `febbcd46`
(six lines in `docs/deploy/sqlite-join.md` only). The package changes no API
route, DTO, setting, CRD, permission or CLI text, so there is no generated
commit.

Evidence under `.context/v020-final-20261110/evidence/F28-HISTORY/` (default race
detector; real SQLite member databases; every owner produced by the pure state
machine from signed DNSKEY responses; no forged anchor state or sample). The
`final-*` logs ran in one batch on lane head `3d374c08` with a clean worktree
(`final-batch.sh`; each log records the head and the count of uncommitted paths,
`final-tree.log` the state after it). `febbcd46` was committed after the batch
and touches documentation only.

- `final-race01`, storage composition (`./internal/trustanchors/...`,
  `./internal/store/sqlite`, `./internal/runtimeowners`,
  `./internal/store/follower`): exit 0, 1,151 pass entries (433 top-level tests),
  zero failures, zero skips; the SQLite package took 273.2 s. It contains:
  - `TestAnchorHistoryRejoiningPrimaryAdoptsPromotedBranch/lower-generation`
    (0.93 s) and `/equal-generation` (1.01 s): an old primary commits one
    observation its follower never pulls; the follower is promoted and, in the
    second case, polls as the new writer. The old primary is fenced by the
    promoted term and installs that branch. It becomes a follower on the adopted
    owner, the carrier advances exactly once with the floor at the adopted
    generation minus one, the registration of the abandoned branch and an earlier
    registration of the shared owner are no longer current, a publication attempt
    captured before the split is refused by the gate, the adopted owner registers
    and reads, and the abandoned image with its owner is in quarantine. A
    physical close and reopen restores the same carrier and publishes again, and
    the next canonical observation installs under the strict rule.
  - `TestAnchorHistoryLosingBranchAdoptsAuditedResolution` (1.13 s): two members
    promoted from one image into the same epoch both poll; the loser polls twice.
    A conflicted member installs nothing. `ResolveAudited` keeps the first
    branch without changing its anchor cut; the loser observes the new term and
    adopts generation 3 over its own generation 4, with its branch quarantined.
  - `TestAnchorHistoryResolutionKeepingRemoteBranchAdopts` (0.91 s): the Store
    API keeps the other member's branch; the resolving member adopts it in the
    resolution transaction and continues that history as the new writer.
  - `TestAnchorHistoryAdoptionKeepsLocalRevocationDenied`:
    (first form, replaced by the review fixes; the final form is described
    under "Review fixes") `/revoked-by-old-primary` (0.94 s) adopted a branch
    that still trusts a key the old primary revoked and kept the adopted
    generation retired until the canonical tombstone. `/denied-by-follower`
    (0.94 s) was the same for a follower's own deny-only observation. `/revoked-by-canonical-branch`
    (0.68 s) adopts a branch that revoked a key the old primary still trusted.
  - `TestAnchorHistorySameTermInstallStaysStrict` (four subtests): a different
    owner at a lower or an equal generation inside one term is still
    `accepted.ErrCAS` with nothing changed, on an ordinary follower and on a
    member that adopted a branch earlier. These four re-seal an image no honest
    writer can produce, and say so.
  - `TestAnchorHistoryAdoptionKeepsIdentityAndRecoveryRefusals` (four subtests):
    under a higher term, another history, other seeds and a dormant owner are
    refused, and so is the promoted branch for an old primary that holds a real
    `Recover` record the branch lacks. Nothing changes and nothing is
    quarantined.
  - `TestAnchorHistoryAdoptionCarrierRules` (four subtests, first form): a
    receipt above the adopted generation is restated, a terminal carrier stays
    terminal, a revocation that does not fit closes the carrier, and a carrier
    bound to another member or owner is refused.
  - `TestCursorRebase*` (five tests, eight subtests) in the gate package: lower,
    equal and higher targets on the rebased floor with the old registration and
    write attempt retired; a rebased floor that keeps the target retired, and a
    terminal one; rollback and unknown completion; refusal of a foreign domain,
    a malformed mark, a shallow floor, the same mark and an unbound gate; and a
    return to an earlier owner whose old write attempt must not run.
  - Every existing anchor test of those packages, including
    `TestAnchorLocalInstallCannotOmitPendingDeny`,
    `TestAnchorLocalInstallRetainsDenyAndRequiresTombstoneCoverage`,
    `TestAnchorInstallCannotAdoptSuccessorBaseline`,
    `TestCursorFullExpectedIdentityAndRegressionRefusal` and the recovery tests.
- `final-race02`, the affected command selection (`Anchor|Doctor|Primary|Cluster|
  Join|Promot|Member|Enroll|Fenc|Replica|Follower|CLIReference|ManagedTopology|
  ManagedEdges|ServingRuntime` over `./cmd/kudurru`): exit 0, 331 pass entries
  (171 top-level tests), zero failures, one skip (`TestDoctorDatabase`, no
  PostgreSQL DSN), 468.8 s. Against F28-FENCED's `fix-race07` it adds exactly:
  - `TestAnchorHistoryRejoiningPrimaryAdoptsPromotedBranchAndPublishes/
    lower-generation` (3.51 s) and `/equal-generation` (3.87 s): two actual
    managed/direct runtimes on one signed in-process root with two seed keys.
    The old primary runs the bootstrap and one more production root operation.
    The follower's database is promoted for real and, in the second case,
    physically reopened under an actual elector whose writer runs its own
    operation. The old primary observes the promoted term, publishes `dns_only`
    with the managed seal of its own image and emits through the production
    output guard, then `follower.Puller.Pull` installs the promoted branch. It is
    a follower on the promoted image, both members hold the same owner version,
    complete publication resumes with the managed seal of the adopted image, the
    abandoned owner is in quarantine, a second pull changes nothing, and root
    reads are exact: one supervision poll in the fenced role and one in the
    follower role, none more inside the cooldown.
  - `TestAnchorHistoryRejoiningPrimaryKeepsItsRevocationUntilCanonical`
    (4.10 s, first form, replaced by the review fixes): the root starts serving the first key with its self-signed REVOKE;
    the old primary's operation takes the two-phase lane and still has one
    trusted key. The successor is promoted from the image before that. The old
    primary serves its own branch as DNS-only data, pulls and installs the
    promoted branch, which trusts both keys. From then on the Store refuses
    resolver input, no publication of the adopted generation exists, the output
    guard refuses to emit and protected HTTP is closed, across the new role's
    supervision poll and an unchanged pull. After the promoted writer's own
    operation records the REVOKE and the next pull installs it, the member
    publishes completely with one trusted key.
  - Every earlier anchor, follower, supervisor, edge, cluster, join and
    promotion test in that selection, and `TestCLIReferenceDrift`.
- `final-race03`: `./internal/cluster/...` (six packages with tests, including
  `replica` and `enrollment/persistence`, which install and resolve through the
  changed seam): all `ok`.
- `final-race04`: the separate-process enrolled scenarios, which use static
  anchors: `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (67.5 s),
  `TestEnrolledClusterProcessesPromoteAndRejoin` (79.3 s) and
  `TestEnrolledProcessConflictResolution` (55.8 s) pass, zero skips, 217.5 s.
  They show that the changed install and resolution calls behave as before for
  owners that are not managed.
- `final-lint01`: scoped golangci-lint v2.15.0 over `./internal/store/sqlite/...`,
  `./internal/trustanchors/...` and `./cmd/kudurru/...`: zero issues.
  `final-vet01`: `gofmt -l` prints nothing for the touched packages and `go vet`
  is clean. `final-drift01`: `TestOpenAPIDrift`, `TestReferenceDrift` and
  `TestAggregatedRolesDrift` pass.
- `drift02` (light run on the final lane head `febbcd46`, clean worktree): the
  same three drift tests and the `./deploy` package pass after the docs-only
  commit.

Retained failed runs and what they showed. The negative runs replace production
files through `go test -overlay` and never touch the worktree (`mutations.py`;
each log starts with the replaced files as a diff):

- `race01` (unmodified production code, first form of the store tests; 10
  failing entries in 5 tests): the predicted defect, as above. The identity and
  recovery refusals pass there, as they should.
- `final-neg08` (the committed tests against the six SQLite store files of
  `f9818a59`): both mounted tests fail at the pull with
  `original owner changed`, the old primary still `dns_only`. `race05` is the
  same on the uncommitted tree.
- `final-neg01` is not a negative run. Its overlay was built from `HEAD` after
  the commits, so it replaced nothing, and the mounted tests passed a second
  time (4 pass entries, 13.7 s). `mutations.py` now pins the base commit and
  `final-neg08` is the rerun.
- `final-neg02` (adoption forgets what the member knew to be revoked): the two
  denial flows end with the floor one too low, a receipt is not restated and a
  revocation that does not fit is dropped.
- `final-neg03` (every install may adopt): a lower and an equal generation are
  accepted inside one term, and a successor without the tombstone erases a
  carried denial.
- `final-neg04` (adoption ignores recovery history): the old primary that holds
  a recovery record installs the branch that lacks it.
- `final-neg05` (the abandoned branch's floor is kept): the install succeeds and
  the adopted owner stays retired in the rejoin, audited resolution and
  remote-branch tests.
- `final-neg06` (a carried denial does not retire the adopted generation): both
  denial flows leave the floor open under an owner that trusts the key.
  The rule it mutated was replaced by the review fixes (a pending carried key
  now refuses the adoption); `fix-neg01` is the negative for the final rule.
- `final-neg07` (the gate ignores its rebase count): the gate test's old write
  attempt runs, and in the store test that attempt is refused only by the
  source's cursor identity.
- `race04`: the first mounted run. The rejoin test passed; the revocation test
  failed on its own helper, which passed a 90-second context to a publication
  capture that needs at most 30 seconds (`retirement: invalid input`). `race06`
  is the rerun after that test fix.
- `mut01`: a mutation that did not compile. `mut02` to `mut06` are the first
  runs of the five store mutations, before the gate counted rebases.
- `lint01` (before the commits): two findings in the new mounted test file only,
  a gosec G115 conversion and revive's context-as-argument. Both were fixed
  before the commits.

`race02` is the first passing run of the store tests (19 pass entries) and
`race03` the first storage composition run, both before the gate counted rebases
and before the stale-attempt assertions existed.

Review fixes (verdict "accept-with-fixes", two minor findings). Fix commits on
`wip/v020/f28`: `8a047222` (store rule, store and mounted tests, architecture
page) and `06afcb73` (operator page only). No earlier fixer commits or
uncommitted work existed when these began.

- A carried revocation the adopted owner lacks no longer turns a DNS-only member
  into a `SERVFAIL` one. `anchorAdoption` and `anchorLocalForAdoption` refuse the
  adoption with `accepted.ErrCAS` unless the adopted owner revokes every key the
  abandoned owner revoked and every key in the node-local denial, and the
  carrier floor on adoption is always the adopted generation minus one. This
  restores on the adoption lane the same-term guard the first form bypassed. The
  D369 row records the rejected alternative and its availability cost.
- `docs/deploy/sqlite-join.md` now says that such a member stays fenced and
  answers DNS from its own data, takes no later data from the primary, how to
  recognize the state until F28-GATE-4, that no command clears a carried denial,
  and the options: replace it with a newly joined receiver or keep it out of
  service.

Evidence (`fix-*` logs, default race detector):

- `fix-store03` (lane head `06afcb73`, clean tree): `./internal/store/sqlite`,
  `./internal/trustanchors/retirement`, `./internal/store/follower`, `-v`: all
  `ok` (417.7 s, 1.2 s, 6.6 s), 906 pass entries (365 top-level), zero failures,
  zero skips. It contains `TestAnchorHistoryAdoptionKeepsLocalRevocationDenied`
  in its final form: `/revoked-by-old-primary` (1.33 s) refuses the promoted
  branch with nothing changed and nothing quarantined, across a physical
  restart and a canonical successor without the tombstone, then adopts the
  canonical tombstone with the floor at the adopted generation minus one, one
  trusted key, the follower role in the promoted term and the abandoned owner in
  quarantine; `/denied-by-follower` (1.22 s) is the same for a follower's own
  denial and keeps its first receipt; `/resolution-keeping-a-branch-that-trusts-it`
  (1.08 s, new) shows `ResolveConflict` refusing to keep a remote branch that
  still trusts a key the resolving member revoked, with the conflict unchanged;
  `/revoked-by-canonical-branch` (1.01 s) is unchanged. And
  `TestAnchorHistoryAdoptionCarrierRules` (2.81 s, five subtests):
  `/pending-key-refused` (new), `/restated-receipt` (now with an adopted owner
  that carries the tombstone), `/terminal-stays-terminal`, `/full-denial-refused`
  (a full carrier with denials the adopted owner does not revoke is refused; it
  replaces the saturation subtest, whose fixture is now refused before
  saturation, and the saturation branch stays the same as in
  `anchorLocalForOwner`) and `/bound-to-something-else`. Every other test of
  the three packages, including `TestAnchorLocalInstallCannotOmitPendingDeny`,
  passes unchanged.
- `fix-mounted01` (tests uncommitted on `febbcd46`, the same content as
  `8a047222`): `^TestAnchorHistory` over `./cmd/kudurru`, `-race -count=2`:
  exit 0, 8 pass entries, 33.4 s. In its final form
  `TestAnchorHistoryRejoiningPrimaryKeepsItsRevocationUntilCanonical` (5.87 s,
  5.42 s) shows two production pulls refused with `accepted.ErrCAS` while the
  old primary stays fenced on its own image and owner, nothing quarantined,
  `dns_only` with the managed seal of its own image, emitting through the
  production output guard with one trusted key, protected HTTP closed and no
  second supervision poll; after the promoted writer records the REVOKE the
  next pull adopts it, the member follows the promoted term, publishes
  completely with one trusted key, and the abandoned owner is in quarantine.
- `fix-neg01` (store tests against `anchor_local.go` and `anchor_command.go` of
  `febbcd46` through `-overlay`): fails as it should, in the two denial flows,
  the new resolution refusal, `/pending-key-refused` and `/full-denial-refused`.
  `fix-neg02` (the mounted test against the same two files): fails at the
  first pull, which adopts the branch that trusts the revoked key.
- `fix-store01` (retained failure): the first form of the rewritten carrier
  fixture failed its own precondition, because the promoted writer's tombstone
  reached the old primary's receipt generation; the fixture now lets the old
  primary run one more generation ahead. `fix-store02` is the passing rerun of
  the anchor selection (non-verbose).
- `fix-lint01`: scoped golangci-lint v2.15.0 over `./internal/store/sqlite/...`
  and `./cmd/kudurru/...` on `06afcb73`: 0 issues. `fix-vet01`: `gofmt -l`
  empty and `go vet` clean for the store and retirement packages.

Not rerun after the fixes: `./internal/cluster/...`, `test/e2e`, the drift tests
(the fixes change no generated input) and the wider `./cmd/kudurru` selection of
`final-race02`.

What remains open. Everything here is one process: the mounted tests run two
runtimes with real member databases, an actual elector, production root
operations and the production puller, but the promoted term reaches the old
primary through `Store.ObserveAuthority` and the image through an in-process
source, not through member TLS between separate processes. That acceptance,
including the three-node conflict with `cluster resolve --offline` in managed
mode, is F28-HA-3; the store test covers the audited resolution with real
member databases. The mounted tests assert publication and the production output
guard, not UDP or TCP exchanges. Three refusals are deliberate and leave a member
fenced on its own last-good DNS until an operator replaces it with a newly
joined receiver: the returning member holds an `anchors recover` record the
current primary lacks; the current primary recovered after the split at a pure
generation the returning member had already reached (`RetainsRecoveryHistory`
compares generations of two branches there, and this package did not relax it);
and the returning member holds managed history while the current primary's
owner is still dormant. The first and the third have tests (the third with a
re-sealed image); the second is read from the code. A member that carries a
revocation refuses the canonical branch until its writer records the same one,
which follows that writer's refresh schedule, and serves its own branch
DNS-only meanwhile. If the canonical writer can no longer observe it (the root
stopped publishing the key, or the canonical branch never knew the key), the
member stays fenced and never follows that writer again; no command clears a
carried denial, and `docs/deploy/sqlite-join.md` tells the operator to replace
the member with a newly joined receiver or keep it out of service. Neither that state nor a refused adoption is
visible online: `anchors status --offline` does not print the node-local
carrier, the puller's error is not logged, and the coordinator status of a
fenced member reports only `not_follower`. Operator-visible status is
F28-GATE-4. Two further points are read from the code and have no test here. A
signed REVOKE that a fenced member verifies while the adoption commits still
closes anchor admission for that process, as it already did for any install
(D297); after a restart the supervisor polls again. A follower with a pending local denial that is
promoted keeps its own owner, which still trusts the key; its writer cannot
commit an observation that omits the REVOKE (`original owner changed`, the
reviewer's `TestReviewProbePendingDenialMemberPromoted`). That is promotion,
not adoption, and unchanged by this package. The opt-in
pinned dnsdist edge test was not rerun, and the full `./cmd/kudurru` package was
not run (the integrator runs `hack/go-test.py`).

**Review and verification state at 68a709e2.** The review found no blocker or major defect and three findings (two minor, one nit). The fixer resolved both minor findings in two commits, one of which (`8a047222`, merged as `383c729b`) changed production behavior: a member refuses adoption while a carried revocation is pending. The lane ran no verification of the fix; F28-HISTORY-RECHECK re-examined it (next sections).


### F28-GUARDS managed trust-anchor guards, image-less refusal and mixed-version position — November 10, 2026 (D370)

Covers map entries F28-GATE-1, F28-EF-2, F28-EF-1, F28-MV-1 and F28-MV-3 (operator text; the decision text is D370).

**Kubernetes refuses managed anchors (F28-GATE-1).** Config validation now refuses `upstreams.recursion.anchorMode: managed` unless `store.driver` is `sqlite`, and the error names the remedy. Before, the Kubernetes driver accepted the setting and built the resolver from the seeds as plain static anchors, so it promised RFC 5011 and did nothing. On the Kubernetes driver the validation runs at startup, on every settings reload through the settings manager (`settings.Manager.Refresh`, which the settings watcher calls for the shared settings ConfigMap) and in settings PATCH validation, which refuses with 400 before anything is stored. A refused reload keeps the last good configuration and section status. The schema help, the generated configuration and CRD references and the architecture page now say that managed anchors are SQLite-only.

**Image-less nodes get an actionable refusal (F28-EF-2).** Two kinds of start now fail with one specific error. The first is an unenrolled receiver started with `managedTopology: direct` or `edges`: a join that never installed its first image, whether before or after the primary accepted it. The second is any node without an installed image started with `edges`. The error names `kudurru cluster join --offline` and `kudurru cluster join-reconcile --offline` and points to the new runbook. Before, these starts failed with the generic "root anchor policy unavailable or changed", or with a raw store error ("replication: read only: initial image not installed") in the edges lane. The refusal comes before any edge observation or root fetch and writes nothing, and a test shows this through the real `runServe` path, not only the serving runtime. That keeps D319: there is no waiting lane and nothing is recaptured, because the offline join (D355/D356) is the only first-image lane. A direct follower enrolled without an image keeps its existing, tested wait for installation. It contacts no edge, and no production path creates such a follower. A new runbook, "Add a follower behind managed edges", gives the order: edge backends, offline join or join-reconcile, host settings, start.

**Joined follower behind managed edges (F28-EF-1).** A new test follows the supported order in process. A primary bootstraps managed trust through its elected writer. The receiver is joined with the real offline `cluster join` CLI, and its first image carries the managed owner. The follower then starts with edges topology. Each edge reports the runbook's fleet: the primary's backend and the follower's. Startup authenticates the two-edge fleet once and adopts the observation. An edge that lists the follower's backend zero times, twice, or with another internal address refuses the start after the first edge, before any root acquisition, with the follower's authority unchanged. The deny-only supervisor makes exactly one root acquisition, and the runtime publishes the accepted managed root through the production output guard. After a physical restart the follower observes the whole fleet once more and serves the same owner.

**Format guards (F28-MV-1).** New golden documents are committed in the tree:

- pure format 1 and format 2 (the bytes the recovery commit wrote on November 8; the pre-recovery codec of be4446fc re-encoded the format-1 bytes unchanged);
- owner envelope rows: a dormant root, plus managed roots wrapping each pure golden;
- a managed local carrier.

The new tests show that:

- decode then encode is byte-identical, and the version digests are pinned;
- unknown or relabeled pure formats (3, 0, string, format 1 with recoveries, format 2 without) are refused, and so are unknown envelope, chunk and local formats;
- a receiver refuses an image whose envelope, chunk or pure document has an unknown format, and its image, authority and local carrier stay unchanged; a reframing control, and the unmodified image installing afterward, show that each refusal comes from the format alone;
- a durable local carrier in an unknown format is refused when read.

A tripwire test pins the tuple: store schema 9, the nine required features, pure format 1, recovery format 2, envelope format 1, local carrier format 1 and the row namespaces.

**Mixed-version operator text (F28-MV-3).** The recovery runbook step and the `kudurru anchors` help now tell operators what to check: every member runs v0.2.0 or later. They also say that the only earlier release, v0.1.0, has no cluster replication and cannot be a member. A first draft said that older members refuse v0.2.0 images by schema, which no released binary does; the review round removed it. The architecture page replaces "Full mixed-version fleet operational acceptance remains separate" with the position and its evidence. Recovery format 2 was added at schema 8 (c4b39bd9), before schema 9 (9fd961e3). No release contains either commit, and v0.1.0 (schema 1) has no managed anchors, so no format-1-only binary was released.

Evidence under `.context/v020-final-20261110/evidence/F28-GUARDS/`:

- `config-race01`: `go test -race ./internal/config`, ok in 23.8 s. It includes `TestManagedRootAnchorsRequireSQLiteDriver` (four driver and mode cases plus the cluster document) and `TestManagedRootAnchorsKubeReloadKeepsPrevious` (a real `WatchSettings` reload on the Kubernetes driver).
- `pure-race01`: `go test -race ./internal/trustanchors/...` (five packages), all ok, 30.2 s for the slowest. It includes `TestCodecGoldenFormatsAreByteStable` (2 subtests), `TestCodecRejectsUnknownFormat` (7), `TestOwnerGoldenRowsAreByteStable` (3) and `TestOwnerRejectsUnknownFormat` (3).
- `store-race01`: `go test -race ./internal/store/sqlite ./internal/store/replication ./internal/config`, all ok (sqlite 386.1 s). It includes `TestAnchorLocalGoldenRecordIsByteStable`, `TestAnchorFormatGateTuple` and `TestAnchorInstallRefusesUnknownFormatsUnchanged` (4 subtests, 0.91 s in an earlier focused run).
- `race03`: `-race -run '^(TestAnchorNoImageStartupRefusesWithRemedy|TestAnchorEdgeActivationJoinedFollower)$' ./cmd/kudurru`, ok. `TestAnchorNoImageStartupRefusesWithRemedy` passed in 3.07 s, with six subtests: unenrolled, incomplete-join and empty-follower, each under edges and direct. `TestAnchorEdgeActivationJoinedFollower` passed in 4.69 s. No skips.
- `race04-prechange`: the same EF-2 test with `serving_anchor_activation.go` reverted to the lane base. It fails as expected: five of six subtests got "root anchor policy unavailable or changed" or "replication: read only: initial image not installed", and only the documented direct empty-follower wait passed.
- `race05`: anchor regression selection `-race -run '^(Test.*Anchor|TestManagedTopology|TestManagedEdges|TestClusterJoin|TestCLIReferenceDrift|TestOfflineAnchor)' ./cmd/kudurru` on the final head, ok in 330.5 s. It was not run with `-v`, so there are no per-test counts.
- `drift01`: `TestOpenAPIDrift`, `TestReferenceDrift` and `TestAggregatedRolesDrift`, plus `./deploy/...`, `./internal/config/crdschema` and `./internal/store/validator`, all ok after the generated commit.
- `gen01`: `make generate` and `make docs-gen`. `lint02`: scoped golangci-lint v2.15.0 over cmd/kudurru, internal/config, internal/trustanchors and internal/store/sqlite, 0 issues.
- Review-fix round (November 11):
  - `fix-settings01`: `-race -count=2 -run 'TestKubeRefreshRefusesManagedRootAnchors|TestSettingsPatchRefusesManagedRootAnchorsOnKube' ./internal/settings ./internal/api`, both ok and passing both times (0.37 s and 0.39 s; 1.21 s and 0.77 s). The first drives `settings.Manager.Refresh` on the Kubernetes driver, the path the settings watcher uses there: a shared UI-owned upstreams section switched to anchorMode managed is refused, and the configuration and section status stay the last good ones. The second sends a kube `PATCH /api/v1/settings` with anchorMode managed and gets 400 naming the driver requirement, with nothing stored.
  - `fix-settings02-prechange`: the same two tests with the driver check disabled. Both fail: Refresh applied `upstreams.recursion.anchorMode`, and the PATCH returned 200 with `persisted: true`.
  - `fix-race01`: `-race -count=2 -v -run '^(TestRunServeAnchorNoImageRefusesBeforeContact|TestAnchorEdgeActivationJoinedFollower|TestAnchorNoImageStartupRefusesWithRemedy|TestDirectAnchorEnrolledFollowerWaitsWithoutInitializing)$' ./cmd/kudurru`, ok in 24.4 s, all passing both times with no skips. `TestRunServeAnchorNoImageRefusesBeforeContact` (2 subtests, direct and edges) took 2.49 s each time; `TestAnchorEdgeActivationJoinedFollower`, now with a two-backend fleet and three refused memberships, took 5.05 s and 5.12 s; `TestAnchorNoImageStartupRefusesWithRemedy` (6 subtests) took 2.66 s and 2.84 s.
  - `fix-race02-prechange`: the two changed cmd/kudurru tests with the image-less check and the exactly-once membership check disabled. Both fail: runServe returned the generic "root anchor policy unavailable or changed" for direct and edges, and the joined follower started behind an edge that did not list it.
  - `fix-gen01`: `make docs-gen` for the reworded `kudurru anchors` help (only `docs-site/content/reference/cli.md` changed).
  - `fix-lint01`: scoped golangci-lint v2.15.0 over cmd/kudurru, internal/settings and internal/api, 0 issues. `fix-drift01`: `TestCLIReferenceDrift` and `TestReferenceDrift` under race after the new generated commit, both ok.
- `golden-gen/`: the throwaway generators, which were not committed, and their logs for the owner rows and the local carrier.

Retained failed runs:

- `race01`: the unenrolled fixture called `BindAnchorLifecycle` without a deadline ("retirement: invalid input").
- `race02`: `OpenExisting` requires a deadline of at most 30 s. A second accepted join reused the invitation's fixed address and was refused; templates and copies replaced it.
- `lint01`: four gosec G304/G703 findings on test file reads and one ST1008 finding; fixed in a separate commit.

What remains open:

- F28-EF-1 stays open. The opt-in pinned dnsdist test `TestRunServeManagedEdgePinnedDaemonFollower` (KUDURRU_ANCHOR_EDGE_LIVE=1, two backends) was not written or run, because no Docker daemon is available on this machine. EF-1 is therefore proven in process only: authenticated edge peers that report a two-backend fleet, but no dnsdist, no UDP/TCP exchanges through either pool, no edge child restart after adding the backend and no cached-answer check. Runbook step 1 is documented but has not been exercised. The live test belongs next to `TestRunServeManagedEdgePinnedDaemonRestart` and must run where the pinned dnsdist image and a Docker socket are available before "initial empty edge-follower installation" is credited.
- The direct empty-follower case keeps waiting instead of being refused as the map suggested, because an existing test and the architecture text pin that behavior.
- Outside this package: an unenrolled receiver started with the default `managedTopology: disabled` still fails `runServe` with the opaque `providerhost: invalid input` (seen in the reviewer's probe), and `CHANGELOG.md` line 194 still says "initial empty edge-follower installation remain separate", which remains true while EF-1 is open.
- Released-binary mixed-version process acceptance (F28-MV-2) belongs to the upgrade lane.
- The full `./cmd/kudurru` package was not run; the integrator runs `hack/go-test.py`.

**Review and verification state at 68a709e2.** The review accepted the two production changes with eight findings, one major. The fixer was partial: four minor findings fixed, one rebutted (the direct empty-follower wait is kept because an existing test and the architecture text pin it). The verifier confirmed the **major finding unresolved**: `TestRunServeManagedEdgePinnedDaemonFollower` (KUDURRU_ANCHOR_EDGE_LIVE=1) does not exist, so F28-EF-1 "initial empty edge-follower installation" is proven in process only and is not credited. The package for it, F28-EF1-LIVE, never started; the private Docker daemon with the pinned dnsdist image is now available on this machine.


### F28-HA-2 recheck: adoption under adversarial sequences, and what ends every refused state — November 11, 2026 (D369)

**Verdict.** The unverified fix `8a047222` (refuse an adoption while a carried revocation is pending) and the term-ordered adoption path behave as D369 says. No sequence of promotion, rejoin, conflict resolution or restart that this package built made a member trust a root key it had seen revoked, forget a revocation, replace managed history with a dormant owner, or adopt a branch D369 refuses, and a refusal changes nothing on the refusing member or on any other member. No production code changed. One defect was found, in the operator text: the documented way out of three states ("replace the member with a newly joined receiver, a new invitation and a new data directory") cannot be carried out for the member's own name or internal address, because enrollment refuses both for an active member and no member revocation is mounted. That text is corrected. Two limits remain that documentation cannot close; both are stated under "What remains open".

**What was examined.** The sequences below are at store level on real member databases, with every owner produced by the pure state machine from signed DNSKEY responses; one state is also driven through two mounted runtimes. The two re-sealed images model owners no honest writer holds in that term, and the tests say so.

- *Restoring revoked trust.* An old primary records a REVOKE its followers never pull. It then refuses, in turn: the promoted follower (term 2), that writer's later observation without the REVOKE, a second promoted follower (term 3) before and after its own observation, and, after it has adopted term 3's tombstone, a stale follower promoted from the image before that tombstone (term 4). It restarts three times in between. It also refuses the trusting owner re-sealed at a higher generation, once under the higher term and once inside the term it adopted. At every step its committed owner lists the key as revoked, its projection carries no DS for it and its local carrier denies it.
- *Losing a revocation in a resolution.* After equal-epoch promotions, keeping the branch that still trusts the key leaves the member that revoked it fenced on its own branch until the kept branch's writer records the REVOKE; keeping the branch that revoked it hands the tombstone to the other member.
- *Ordering by anything but the term.* Two members replace each other as primary three times. Each returning member adopts the other's owner: two generations lower and last updated earlier, two generations lower again in the other direction, and at the same generation with another digest. Every abandoned owner is in quarantine, each adoption advances the carrier once, and a restart restores the last adopted cut.
- *A refusal as a weapon.* A refused pull leaves the member's registered resolver input current and readable, and a publication captured before the pull completes after it. A member without the revocation follows the same primaries through the same terms. The refusing member cannot be promoted (`replication.ErrFenced`) and cannot be resolved (`replication.ErrCAS`), and both refusals leave it unchanged.
- *A second revocation while refusing.* The refusing member's own deny-only check records a second key. It then retires its own branch and follows only after the primary has recorded both keys; one is not enough.
- *A revocation verified while an adoption commits.* The denial was captured for the abandoned owner. Committing it is refused with nothing written, and anchor admission closes for the process (D297). After a restart the adopted cut binds and publishes, the next check records the denial against it, a successor without the tombstone is refused and the one with it installs.
- *Clock.* Adoption compares no update times (pinned by the first alternating adoption, whose adopted owner was updated earlier than the abandoned one). The store has no clock seam, so a wall-clock step during an install was not driven; what depends on it (`anchorOriginalLifetime.check`) refuses before commit.

**Every state that holds a member back, and what ends it.** Each line is pinned by a test; every refusal is also shown unchanged across a restart and later canonical images.

| State | The member answers | What ends it | Test |
|---|---|---|---|
| Its owner revoked a key the primary has not | DNS-only from its own branch | The primary's next root check records the REVOKE | `TestAnchorRecheckRevocationHeldAcrossTermsRestartsAndStalePromotion` |
| The same, and the root has stopped publishing the key (the primary keeps it Missing and trusted) | DNS-only | Replacement only | `TestAnchorRecheckRevocationThePrimaryCanNeverRecord/root-stopped-publishing-the-key` |
| The same, and the primary never held the key (the member saw it appear and get revoked) | DNS-only | Replacement only | `.../primary-never-knew-the-key` |
| Its own root check denied a key | `SERVFAIL` | An installed owner that revokes it; promotion does not end it | `TestAnchorRecheckPromotedMemberWithPendingDenial`, `TestAnchorRecheckRefusingMemberDeniesAnotherKey` |
| The primary recovered after the split at or below the member's pure generation | DNS-only | Replacement only | `TestAnchorRecheckRecoveryHistoryDisagreement/primary-recovered/member-level-with-the-record`, `/member-past-the-record` |
| The member holds a recovery the primary lacks (also after the primary recovers with the same key) | DNS-only | Replacement only | `.../member-recovered` |
| Managed history against a dormant primary | DNS-only | The primary bootstraps managed anchors from the same seeds (other seeds: replacement only) | `TestAnchorRecheckManagedMemberAndDormantPrimary/same-seeds`, `/other-seeds`; mounted: `TestAnchorHistoryManagedMemberFollowsOnceThePrimaryStartsManagedAnchors` |
| A REVOKE verified but not written | `SERVFAIL` | Restart | `TestAnchorRecheckRevocationVerifiedWhileAdoptionCommits` |

A replacement is a new receiver joined from the primary. In each "replacement only" test it takes the primary's owner, a carrier that denies exactly the keys that owner revokes, follows and publishes; the refused database stays as it was. Where the root stopped publishing the key, the test asserts what the operator page says: the replacement trusts that key again, because the cluster's state does. `TestReplacementReceiverNeedsItsOwnNameAndAddress` (enrollment) shows that an invitation for the active member's name or address is refused in five combinations with the roster unchanged, that the old identity cannot take the new place, and that a new name and address is admitted beside the old member.

**Operator text.** `docs/deploy/sqlite-join.md`, "A member returns with different trust-anchor state", now says what a refusing member does, that it is not a promotion candidate, how to recognize it with the two offline commands that exist today (the mounted test pins the `cluster resolution --offline` review of a refusing and of a following member), what ends each case, how to replace a member under a new node name and internal address and which trust the replacement holds, and three other states in which a managed member answers `SERVFAIL`. `docs/deploy/runbooks.md` points to it. `docs/architecture/upstreams-and-cache.md` carries the same states as one table.

**What F28-GATE-4 must show**, because none of this is visible online today:

1. Per member, readable on the primary and on the member (a fenced member's HTTP API is closed, so the member's own view has to survive that, for example as metrics and a log line on change): authority mode, term epoch, the term of the installed image, and whether the last pull was installed, unreachable or refused.
2. For a refused pull, the reason class and since when: carried revocation (with the key tags the primary's owner does not revoke), recovery-history disagreement, managed history against a dormant primary, another history, policy or seed digest. A refusal that has lasted longer than one refresh interval of the primary deserves a warning, and the continuing cost is worth showing: a refusing member fetches the complete image at every pull interval.
3. The node-local carrier: denied key tags with the generation of the first receipt, which of them the installed owner does not revoke yet, the floor against the owner generation (retired or not), and terminal.
4. The owner: envelope and pure generation, every key with tag, status (AddPend, Valid, Missing, Revoked), first seen and remaining hold-down, quarantined, the number and generations of recovery records, next refresh, phase (dormant or managed). `anchors status --offline` prints none of the keys today.
5. The workers: on the primary, updater running, waiting for its clock, in transport retry (next attempt), or stopped for the term (with the error); on other members, the deny-only check's last result and whether it stopped for the authority epoch.
6. Anchor admission closed for this process (restart needed), which today is only an error from the next capture.
7. Promotion readiness: that a member with a pending local denial would answer `SERVFAIL` as the primary, and that a refusing member is not a candidate.

Evidence under `.context/v020-final-20261110/evidence/F28-HISTORY-RECHECK/` (default race detector; `final-race01` ran from `final-batch.sh` and the other `final-*` logs from `final-steps.sh`; each log starts with the head and the number of uncommitted paths):

- `final-race01`, storage composition (`./internal/trustanchors/...`, `./internal/store/sqlite`, `./internal/runtimeowners`, `./internal/store/follower`, `./internal/cluster/enrollment/...`, `-race -v`), head `bf8baf3f`, clean tree: exit 0, 1,410 pass entries (511 top-level tests), zero failures, zero skips; the SQLite package took 395.3 s. It contains the ten new store tests, 20 pass entries: `TestAnchorRecheckRevocationHeldAcrossTermsRestartsAndStalePromotion` (4.72 s), `TestAnchorRecheckResolutionKeepsRevocationOnEitherBranch` (2 subtests, 2.96 s), `TestAnchorRecheckAlternatingPrimariesAdoptEachOther` (2.02 s), `TestAnchorRecheckRecoveryHistoryDisagreement` (4 subtests, 6.08 s), `TestAnchorRecheckRevocationThePrimaryCanNeverRecord` (2 subtests, 4.01 s), `TestAnchorRecheckManagedMemberAndDormantPrimary` (2 subtests, 2.74 s), `TestAnchorRecheckPromotedMemberWithPendingDenial` (1.30 s), `TestAnchorRecheckRefusingMemberDeniesAnotherKey` (1.98 s), `TestAnchorRecheckRevocationVerifiedWhileAdoptionCommits` (1.42 s) and `TestAnchorRecheckAdoptionCoversBothSourcesOfRevocation` (1.09 s); `TestReplacementReceiverNeedsItsOwnNameAndAddress` (0.03 s); and every `TestAnchorHistory*` test of F28-HISTORY (7 tests, 19 subtests).
- `final-race04`, the store and enrollment selection (`^(TestAnchor(Recheck|History)|TestReplacementReceiver)`) on the final head `90740ec6`, clean tree: exit 0, 47 pass entries (18 top-level), zero failures, zero skips, 52.0 s and 1.1 s. `final-race03` is the same selection on `39516ab6` with the same counts. Three commits followed `final-race01`'s head: `526b0231` (two documentation files), `39516ab6` (one comment in the new test file) and `90740ec6` (the failure text of the shared refusal helper).
- `final-race02`, the command selection (`^(Test.*Anchor|TestManagedTopology|TestManagedEdges|TestServingRuntime|TestCLIReferenceDrift)` over `./cmd/kudurru`, `-race -v`) on `39516ab6`: exit 0, 196 pass entries (92 top-level), zero failures, zero skips, 275.6 s. It contains the new `TestAnchorHistoryManagedMemberFollowsOnceThePrimaryStartsManagedAnchors` (2.57 s): two actual managed/direct runtimes on one signed in-process root; the follower's image holds a dormant owner; the old primary bootstraps through the production root operation; the follower's database is promoted for real; the production puller is refused on the old primary with `accepted.ErrCAS`, which stays fenced on its own image and owner, emits DNS-only through the production output guard with protected HTTP closed, and whose offline review reads fenced, `no_conflict`, not resolvable, with an installed image of a lower term; the promoted database is reopened under an actual elector and its writer bootstraps from the same configuration; the next pull adopts that owner, complete publication resumes, the abandoned owner is in quarantine and the review reads follower. The two earlier mounted history tests pass unchanged (8.75 s and 4.77 s).
- Negative runs. They replace production files through `go test -overlay` and never touch the worktree (`mutations.py`; each log starts with the replaced files as a diff). All ran on `39516ab6` over `^TestAnchor(Recheck|History)`; the one later commit changes no assertion:
  - `final-neg04` (adoption checks no carried revocation, the behavior before `8a047222` at the point of decision): 7 tests fail, among them the cross-term test, the resolution that keeps the trusting branch, both never-record cases and the second-key test: the member adopts the branch that trusts its revoked key.
  - `final-neg02` (adoption ignores the local denial): 4 tests fail, including `TestAnchorRecheckRefusingMemberDeniesAnotherKey`. `final-neg03` (adoption ignores the abandoned owner's revoked keys): only `TestAnchorRecheckAdoptionCoversBothSourcesOfRevocation` fails. The first run of that mutation (`neg03`) passed every test, which is why that unit test was added: every carrier a member can commit lists the owner's revoked keys in the denial too, except a terminal one, so the second loop of `anchorAdoptionCovers` is defense in depth that no member flow can reach.
  - `final-neg05` (adoption ignores recovery history): `TestAnchorRecheckRecoveryHistoryDisagreement` fails in its three refusal subtests. The existing `recovery-prefix` subtest passes under this mutation: since `8a047222` the carried-revocation check refuses that fixture first, so it no longer isolates the recovery check. `member-recovered` does, because there the primary has recorded the same REVOKE and recovered with the same key.
  - `final-neg06` (an install never adopts by term, the rule before D369's history part): 10 tests fail, including the alternating primaries, the dormant primary with the same seeds, and the resolution that keeps the revoking branch. `final-neg10` runs the mounted tests against the same mutation: all three fail at the pull, the new one with `old primary cannot adopt the primary's first managed owner`.
  - `final-neg07` (adoption keeps the abandoned branch's floor): 5 tests fail, including `TestAnchorRecheckAlternatingPrimariesAdoptEachOther`.
  - `final-neg08` (adoption ignores domain, policy and seed digest): every test passes. The check is backed by the carrier's own domain binding in `anchorLocalForAdoption`. `final-neg09` removes both: the two other-seeds subtests fail because the install no longer returns `accepted.ErrCAS` but `retirement: effect completed or uncertain; callback refused`. Only the retirement gate still refuses then, and (read from the code) `StageRebase` refuses by failing its scope, which closes the gate. The two store checks are what keep that refusal clean.
- `final-lint02`: scoped golangci-lint v2.15.0 over `./internal/store/sqlite/...`, `./internal/cluster/enrollment/...` and `./cmd/kudurru/...` on `90740ec6`: @@LINT02@@ (`final-lint01`, on `39516ab6`: 0 issues). `final-vet02` (and `final-vet01`): `gofmt -l` prints nothing for the touched packages and `go vet` is clean. `final-drift01`: `TestOpenAPIDrift`, `TestReferenceDrift` and `TestAggregatedRolesDrift` pass, and `TestCLIReferenceDrift` passes in `final-race02` (the package touches no generated input, so there is no generated commit). `final-docs01`: `make docs-site` with the pinned Hugo 0.167.0 and `--panicOnWarning` builds 163 pages on `39516ab6`, exit 0, so every link in the changed pages resolves to a page; fragments are checked only by `make docs-check-links`, which was not run.

Retained runs that failed or did not run, and what they showed:

- `final-neg01` (the two production files exactly as before `8a047222`) does not compile on the final head: the files of `febbcd46` have no `anchorAdoptionCovers`, which the unit test added last calls. `neg01` is the same mutation on the tree before that test existed (uncommitted, base `43092a90`): 6 tests fail, four of the new ones and two of F28-HISTORY's.
- `neg04`: a mutation that did not compile (an unused variable); `final-neg04` is its corrected form.
- `race03-recheck-first`: the first run of the new store tests. Six passed; `TestAnchorRecheckPromotedMemberWithPendingDenial` failed on its own helper, which expected `retirement.ErrRetired` where a member with a pending denial is refused earlier, at publication capture, with `ErrAnchorUnavailable`. The helper now asserts the committed floor and that refusal.
- `final-race02-not-run`: the sequential batch (`final-batch.sh`) waited 16 minutes for the machine-wide heavy lock at its second step and was stopped; `final-steps.sh` ran the remaining steps as separate jobs. No test ran in that log.
- `race01-baseline` (the interrupted first implementer's only artifact) and `race02-baseline`: the F28-HISTORY store tests on the unmodified lane head `43092a90`, both `ok`. `race04-recheck-store`, `race05-mounted-first`, `lint01` and `neg02` to `neg08` are the first passing or failing runs on the uncommitted tree, superseded by the `final-*` logs.

What remains open.

- **A revocation the primary can never record has no recovery that keeps it.** The member is recoverable (replace it), and no database that recorded the revocation ever trusts the key again. But the replacement, like every other member, then trusts a key that one member verified as revoked, and nothing records that revocation in the cluster's history. The only complete fix is an audited operator revocation on the primary. This package did not build it: it is a permanent trust change without cryptographic proof, a mistaken one cannot be undone (not even by `anchors recover`, which never revives a revoked key), and it needs its own design and an owner decision. Read from the code, not tested here: the same command would also unblock a node that missed a whole rollover, whose only trusted key is gone from the root; such a node is not quarantined, so `anchors recover` refuses it.
- **A refusing member whose primary is lost for good cannot be recovered.** It cannot be promoted or resolved, and a replacement needs a primary. This predates D369 for the seconds in which any rejoining member is fenced; a refusal makes the window as long as the refusal. Closing it means letting `cluster resolve --offline` keep the branch of a fenced member that holds no image of its term, which relaxes the guard that refuses a resolution without a recorded conflict or divergence (D361). That is an authority change for the HA lane to decide, and it was not made here.
- **`RetainsRecoveryHistory` compares generations of two branches under adoption.** Whether a returning member can follow a primary that was recovered after the split depends on which of the two observed more often (`member-behind-the-record` adopts, `member-level-with-the-record` does not). The refusal is safe and D369 keeps it deliberately; it is arbitrary, and the tests now pin both sides so that a later relaxation is a visible decision.
- **No member removal.** A replaced member stays in the roster, keeps its name and address reserved and counts toward 64 active members. The replacement procedure works around it; a `cluster` command that revokes a member (the enrollment transition exists and is tested, without a production caller) would let a replacement reuse them.
- A refusing member repeats a complete image fetch every pull interval (5 s by default) for as long as it refuses, as every rejected replication does. Nothing backs off.
- Not driven here: the process-level versions of these states over member TLS (F28-HA-3), UDP or TCP answers of a refusing member (the mounted tests assert publication and the production output guard), and the claim that a promoted member with a pending denial keeps its HTTP API closed (the store test shows that it can register no resolver input in either role; the operator page therefore only says that it answers `SERVFAIL`). The docs-site link check (`make docs-check-links`) was not run; the Hugo build with fatal link warnings was.
- The full `./cmd/kudurru` package was not run; the integrator runs `hack/go-test.py`.

**Review and verification state at 68a709e2.** The implementer stopped when the credits ran out, after it had committed its work and written the section above, and before it returned a result; the lane stopped with it. Its commits were merged without a review or a verification pass. The `final-*` logs under `evidence/F28-HISTORY-RECHECK/` are its evidence; `final-race02-not-run.log` records a step that did not run.


### FLAKE-OIDC-WRITER fixture teardown order — November 11, 2026 (no decision row)

**Answer to the integrator's question:** the failure predates the first
completion batch. The commits between `33ee04bc` and `de0d249a` did not introduce
it, and the measurements show no worsening: with the same command the base and
the pushed head fail at the same rate. It is a defect of the test fixtures, not of
the product, and no commit of the batch is at fault.

| Measurement (unmodified test, one prebuilt race binary per tree, rounds interleaved) | base `33ee04bc` | head `de0d249a` | with the fix `796c04e3` |
|---|---|---|---|
| `GOMAXPROCS=4 taskset -c 0 <binary> -test.count=10 -test.timeout=90m -test.run '^TestCLIOIDCBrowserActualTLSProofRefusals$'`, three rounds (`rate2.sh`, logs `contention03` to `contention09`) | 3 failing runs of 30 (1, 1, 1) | 3 failing runs of 30 (3, 0, 0); pilot 2 of 4 | 0 of 10 |
| `taskset -c 0-3 <binary> -test.count=30 -test.timeout=90m -test.run '^TestCLIOIDCBrowserActualTLSProofRefusals$'`, two rounds (`rate.sh`, logs `rate-*-round1`, `-round2`) | 0 failing runs of 60 | 3 failing runs of 60 | (200 runs queued) |

Every failure on either tree is the same line and message,
`serving_http_oidc_test.go:113: browser writer ended context canceled`. The
difference in the second row (0 against 3 of 60) is not significant (one-sided
Fisher exact p = 0.12) and disappears in the first row, where the failure is
frequent enough to compare. The files that produce the failure
(`cmd/kudurru/serving_http_oidc_test.go`, `serving_runtime_test.go`,
`login_oidc_test.go`, `serving_lifecycle.go`, `internal/store/sqlite/authority.go`,
`internal/cluster/runner.go`) are byte-identical on both commits
(`identical01-fixture-and-lifecycle-files.log`). The same failure class is also in
hosted CI eleven days before the batch: run 41214453524 of October 31, 2026,
`TestServingCurrentCommandFinalCredentialClock/expiry/noop:
serving_command_test.go:68: context canceled` (the command fixture, built from
the same `runServingFixture` plus an elector under `t.Context()`;
`.context/ci-41214453524-failed-private.log`).

Mechanism (confirmed by four instrumented failures, `diag02`): at the end of a
test the testing package cancels `t.Context()` before it calls the first cleanup
(`testing.(*common).runCleanup`). The fixture ran the always-on runtime
(`runServingFixture`: `s.Run`) and the elector (`Elector.Run` around
`s.RunWriter`) under two sibling children of `t.Context()`. Go cancels the
children of a context in map order, one goroutine walking them. When the
always-on context was reached first, `Run` returned in about 0.4 ms, its
`defer s.cancel()` ended the runtime lifetime (and the provider coordinator's
`Run` closed the coordinator), `RunWriter` returned `context.Canceled`, and the
fixture evaluated `ctx.Err() == nil` on the term context 70 to 250 microseconds
before the same cancellation reached the elector context.

State when this draft was written:

- fix committed on the lane branch: `796c04e3 test(serving): stop fixture
  runtimes after their writers` (test files only).
- unamplified, valid control (`rate.sh`, same command per tree, one binary per
  tree built from a clean checkout, rounds interleaved base/head/base/head, CPUs
  0-3, `-test.count=30` per round): base `33ee04bc` 0 failing runs of 60
  (`rate-base-33ee04bc-round1.log`, `-round2.log`); head `de0d249a` 3 failing runs
  of 60 (`rate-head-de0d249a-round1.log`: `credential`; `-round2.log`:
  `credential`, `azp`).
- amplified with extra sibling contexts under `t.Context()` (same scratch test on
  both trees, build overlay only): 20,000 siblings base 16 of 36 fixtures, head 19
  of 36; 2,000 siblings base 23 of 54, head 22 of 54; 500 siblings base 11 of 54
  (the head run at 500 was cut off by the machine restart). Every failure is
  `serving_http_oidc_test.go:113: browser writer ended context canceled`.
- deterministic reproduction: `TestServingFixtureAlwaysOnRuntimeOutlivesTestContext`
  fails 20 of 20 with the old fixtures (`det03-before-fix-overlay-x20.log`) and
  passes 20 of 20 with the fix (`det04-after-fix-x20.log`).
- still to run: the contention comparison of the two trees, 200 consecutive runs
  of the original test, the hosted batch replays, the root selections, lint.

**Review and verification state at 68a709e2.** The implementer stopped when the credits ran out, before it returned a result or finished this section; its single commit (`796c04e3`, merged as `7f351615`) was merged without a review or a verification pass. The confirmation runs it had started finished and are retained under `evidence/FLAKE-OIDC-WRITER/`: `confirm05-original-fixed-x50-{1..4}` ran `TestCLIOIDCBrowserActualTLSProofRefusals` 200 times on the fixed tree on CPUs 0-3, exit 0 each; `confirm02-batch-oidc-fixed-{1..5}`, five replays of the hosted batch that held the test, passed; `confirm06` (the same batch on base `33ee04bc`) passed; `confirm07` (head `de0d249a`) was cut off. Two retained failures on the fixed tree: `confirm03` (root writer, anchor, member and query-log selection, 294 passes) failed `TestServingBlockingWriterCanceledAfterCommitBeforeObservation/blocking` with `serving_runtime_test.go:140: providerresources: owner closed`, the failure hosted CI 43045349810 then showed on `68a709e2`; `confirm04` (fixture users, 524 passes) failed `TestServingHTTPClusterActivationIdentityRefusalAndRollback/invalid-member` with `replication: writer fenced`. Neither was run on the base for comparison, so whether the teardown change causes the `owner closed` failure is undetermined; CI-RED is to find out by mechanism and has not run. `confirm08` (lint) found the two errcheck issues that `68a709e2` fixed. The fixture rule in this package's draft decision row is not a product decision and was not added to the decision log.


### DOCS-VERSION guides and migration comparisons retargeted to v0.2.0 — November 10, 2026

Map entry G21-15 (and item 8 of G25-OPDOCS, which DOCS-OPS left for this
package). No decision number: no design decision was needed. Base commit
`5f10ed88` (lane branch `wip/v020/docs`, after DOCS-OPS). Seven source
commits: `66c45c13` (site and guide framing), `fe568c4e` (migration index,
cutover checklist, the two offline converter pages), `d9c62d1e` (Pi-hole,
AdGuard Home and Technitium guides), `71613d38` (administrator guide chapters
1, 2, 5 and 7), `56c99075` (deployment pages and the client identity page),
`2aa4868e` (architecture pages) and `319529fc` (Enterprise page, separate so
that it can be dropped). Three review-fix commits on top: `59c218fb`
(migration pages: the undownloadable list URL), `bf4d1af5` (restart effect
and the SQLite zone verdict in `zones.md` and chapter 7) and `a10afdf1`
(address matchers win over a client ID). No generated commit: the package
changes no generator input.

At the base, 40 lines outside the generated reference named `v0.1.0` and 3
more said "version 0.1". Now 12 lines name `v0.1.0`, and each is history that
is still true: DHCPv4 "included since v0.1.0" (8 lines), "v0.1.0 is the first
release" with its release page (2 lines), and the kustomize note about the
release candidates (2 lines). No line says "version 0.1". The framing sentences of `docs/guide/README.md` and
`docs-site/content/_index.md` name v0.2.0 ("clay copies"), the release name of
the README's roadmap.

Each rewritten statement was checked against the code, and the statements
about SQLite were also checked against real `kudurru serve` processes on
loopback (binary built from the lane head; the Go code of the head is that of
`5f10ed88`). What the pages now say:

- **Migration index.** The kudurru column of the comparison is v0.2.0: four
  served zone types, RFC 2136 updates, opt-in validation of forwarded answers,
  adblock modifiers, per-policy rewrites, authenticator codes on SQLite, the
  enrolled SQLite cluster. "Follower and Raft stores are deferred" is replaced
  by the three ways to run several nodes. The review date of the kudurru
  column is its own; the other three columns keep theirs, because they were
  not reviewed again. Store-dependent features are named with a link to the
  table of the architecture overview instead of a copy of it.
- **Technitium.** Secondary and stub zones and dynamic updates are no longer
  "Deferred"; the table says what kudurru serves and that the importer still
  skips them, and a new section 3 shows a secondary and a stub zone (the later
  sections are renumbered; nothing linked to their anchors).
- **AdGuard Home.** The seven DNS modifiers are applied instead of "counted as
  unsupported and skipped"; `$badfilter` and `@@` act inside their own
  block-list object; an unknown modifier or value counts as invalid.
  ClientIDs can move with DoT, DoQ and DoH clients, unless an IP, MAC, CIDR,
  hostname or name-glob matcher matches the arriving address (the first
  matching tier wins; `IdentifyAt` in `internal/policy/index.go`). The client
  identity page and the shared-address runbook say the same.
- **Pi-hole.** "No local validation" is corrected (it was already wrong for
  recursive groups), the blocking pause is node-local only on static twins,
  and MAC and hostname clients have a stated condition.
- **Cutover checklist.** Client matchers by source of identity; secondary
  zones and updates available; a new "Apply results" subsection (below).
- **Administrator guide.** Chapter 5: the client ID and MAC matchers are no
  longer "Reserved", with what each needs. Chapter 7: Default, Validate and Do
  not validate of a forward zone, temporary exceptions instead of "no list of
  exceptions", four zone types instead of "three", and a caution about
  schema-only zone types. Chapters 1 and 2 lose "In version 0.1" and "v0.1.0".
- **Deployment pages.** `client-identity.md` gains a table of where each
  matcher gets its identity (the guides link to it for the question of which
  installation has which source); `runbooks.md` no longer speaks of "the
  version 0.1 listeners"; `kustomize.md` is corrected (see below).
- **Enterprise page.** The integrations row names the background
  integrations on a SQLite node instead of calling them planned.

Four things the checks found that were wrong before this package and are not
version sentences:

- `docs/deploy/kustomize.md` said that releases up to v0.1.0 keep the
  kustomize tree under `config/`. The v0.1.0 tag has `deploy/kustomize/`;
  only the release candidates up to `v0.1.0-rc.6` have `config/` (`git
  ls-tree` on all eight tags).
- All three product guides said that the query for an allowed name "must show
  an allow-list decision". `kudurru query` prints `status: forwarded` or
  `cached` and names no list. The guides now say what it prints.
- `docs/architecture/zones.md` said that `external`, `catalog` and
  `secondaryCatalog` zones "are accepted but get `Ready=False, reason
  Unsupported`". The controller writes that verdict on both stores, but on
  SQLite the API cannot show it because no generation is published (see the
  defect below). The first version of this package said the SQLite runtime
  had no such verdict; the review found the verdict in `state.db`
  (`review-probe01.log`), and the page now says so.
- `docs-site/content/migrate/pihole.md` said "DNSSEC validation: No local
  validation", which contradicted the index page.

Three product defects that a migration meets are described in the pages,
marked "in this version", because the pages cannot remove them:

1. **A zone of an unserved type stops a SQLite node.** `kudurru apply` of a
   `DNSZone` with `type: catalog`, `secondaryCatalog` or `external` is
   accepted ("created"). `servingzones.Prepare` then refuses every candidate
   while the object exists: readiness reports `serving(complete serving
   generation unavailable: prepare_failed)`, every API request answers
   `503 generation_pending`, also the read and the deletion of that object,
   the node's own zones get no answer (timeout; a forwarded public name still
   answered), and the node does not become ready after a restart. After the
   restart it answers SERVFAIL for every name, forwarded names included
   (`review-probe05.log`). Putting back a copy of the data directory made
   before the apply recovers it. New
   finding of this package; not in the tracker. The migration index, the
   Technitium table, chapter 7 and `zones.md` now say not to apply these
   types.
2. **One run of `kudurru apply` applies one new document on SQLite**
   (found by DOCS-OPS, confirmed here on the guides' own files): the
   three-document examples of the migration guides need three runs, the
   two-document examples two. The cutover checklist has "Apply results", and
   each guide points to it at its first example.
3. **A list URL that has never been downloaded stops a SQLite node** (found
   by the review of this package). An enabled `BlockList` URL source without
   a published digest and body makes list preparation return `ErrIncomplete`
   (`internal/listfetcher/prepare.go`), which refuses the whole serving
   candidate, even when no policy refers to the list. The effect is that of
   defect 1: API 503 `generation_pending` for every request including the
   deletion of the list, own zones unanswered, `prepare_failed`, SERVFAIL for
   every name after a restart. Unlike defect 1 it clears by itself at the
   first successful download attempt (the fetcher backs off: 1, 2, 4 minutes;
   `review-probe09.log`). A source with `enabled: false` is harmless. The
   Pi-hole and AdGuard first examples reach it on a host without outbound
   HTTPS (`review-probe07.log`), and so does `kudurru import adguard --apply
   --wait` with the repository's fixture (`review-probe02.log`,
   `review-probe03.log`). It has no owner under `out/` yet; the reviewer
   considers it release-blocking for the product. The fixing change should
   either compile the candidate without a never-downloaded source or refuse
   the write, and let the API delete an object while no generation is
   current. Until then the migration overview, "Apply results" in the cutover
   checklist, the offline import page, the three product guides, chapter 7
   and `zones.md` warn about it: download each URL from the kudurru host
   first, apply URL lists one at a time, keep a copy of the data directory,
   do not restart. The "refused once ... run it again" sentence of the
   checklist is replaced by one that says when repeating does not help.

Evidence under `.context/v020-final-20261110/evidence/DOCS-VERSION/`:

- Review fixes, binary built at `a10afdf1` (`fix-build02.log`, a light run
  with `go build -p 2`; `fix-build01.log` waited about 20 minutes on the
  heavy lock and was withdrawn; the header line "lock held" of both comes
  from the script text). `verify.py` gained a section `unfetchable` (a node of
  its own: a `BlockList` with the URL `https://lists.example.test/deny.txt`
  and no policy; API read, repeated apply and delete refused with
  `generation_pending`, own zone unanswered, forwarded name still answered,
  `prepare_failed`, after a restart not ready in 30 s and SERVFAIL for the own
  and a forwarded name, recovery by putting back the copy of the data
  directory, the list gone with it); a check of tier precedence in `identity`
  (a CIDR group on 127.0.0.0/8 makes `/dns-query/phone` select the CIDR
  group's policy and not the client-ID group's, and removing it restores the
  client ID); and the two zone-transfer checks now retry for up to 20 s,
  because a transfer that starts while the node publishes a change can be cut
  off (`review-verify02.log`: 133 of 135, both transfer checks with `SOA
  lines=0`; `review-probe08.log`: 11 to 17 of 27 to 41 AXFR attempts in the
  3 s after an unrelated apply ended with `communications error ... end of
  file`). That cut-off transfer is a SQLite node behavior worth a follow-up;
  the docs do not promise otherwise. `fix-verify01.log` (sections zones,
  identity, unfetchable): 53 pass, 0 fail. Four full runs of `verify.py` at `a10afdf1`
  (each about 3 to 3.5 minutes): `fix-verify03.log`, `fix-verify04.log` and
  `fix-verify05.log` 159 of 159 pass. `fix-verify02.log` is a retained
  failure, 157 of 159: `spec.dnssec does not sign` saw no answer for the new
  signed zone within the 15 s wait on the busy node (now 30 s; the check
  still requires NOERROR and no RRSIG), and on node-x the own zone answered
  SERVFAIL instead of timing out after the catalog zone (the pages say
  "stops answering", which both are; the check now accepts either and still
  requires that the record is not answered). Runs 04 and 05 are after those
  two adjustments; run 03 had only the first set of changes. The earlier
  single-run count "135 checks pass" of `run11-all.log` is superseded by
  these counts.
- `fix-gates-light02.log` at head `a10afdf1`: guide checker 67 files, 0
  errors; `make version-check` PASS; `TestReferenceDrift` ok; `make
  docs-site` 165 pages; `make docs-check-links` passed; the acceptance grep
  and the stale-marker search unchanged. `fix-gates-light01.log` is a
  retained failed run: chapter 7's caution had 8 sentences (limit 6) and was
  split into two paragraphs.

- `run11-all.log` (1 min 57 s): 135 checks pass, 0 fail, every section of
  `verify.py`: importers present (adguard, bind, blocky, pihole, technitium;
  no coredns); secondary zone transfers and answers on a second node, its
  records are read-only; stub zone answers through the discovered authority;
  `spec.dnssec.enabled` signs nothing; AXFR out, and IXFR for an older serial
  in AXFR form; RFC 2136 with a stored TSIG key (signed accepted, unsigned
  `REFUSED`); 18 checks of modifier rules (`$dnstype`, `$dnsrewrite`,
  `$important`, `$badfilter`, `$denyallow`, `$client`, unknown modifier,
  unknown `$dnstype`, hosts rewrite, `@@` and `$badfilter` across two list
  objects; list status `unsupported: 1, invalid: 3`); `Policy.spec.rewrites`
  for an exact name and `*.suffix`, and not for a name in an own zone; client
  ID on DoH path and DoT server name selects a group and its policy, other
  IDs and plain DNS do not; `dnssec.enabled` off by default, AD only after it
  is on, a negative trust anchor removes AD for its domain, an expiry nine
  days away is refused, a forward zone's `dnssecValidation` wins in both
  directions; an RPZ feed transferred from another node answers its local
  data; blocking state `scope: cluster` on a SQLite node and `scope: node` on
  a static twin; a catalog zone on a node of its own (accepted, then 503 for
  read and delete, no answer for the own zone, not ready); and the manifests
  and commands of the three product guides, read from the Markdown files and
  run as written on one node for each guide (35 checks).
- `run12-examples-head.log` (1 min 9 s): the guide examples and the catalog
  check again with the worktree at the committed head `319529fc`, 49 pass. On
  new nodes the three-document files of the guides needed 3 runs of `kudurru
  apply` each, the two-document files 2 runs and the one-document files 1.
- `probe-catalog01.log`, `probe-external01.log`,
  `probe-secondaryCatalog01.log`: one SQLite node after each unserved type
  (state at 1, 3, 6, 12 and 20 s, unrelated writes, readiness, delete,
  restart). `probe-restore01.log`: 13 checks pass, recovery by putting back
  the data directory.
- `run-offline01.log`: `kudurru import blocky` and `import adguard` produce a
  plan with the plain binary and no server.
- `gates-light02.log` at head `319529fc`: `hack/guide-lint.py` 67 files, 0
  errors; `make version-check` PASS; `TestReferenceDrift` ok (0.045 s);
  `make docs-site` 165 pages with `--panicOnWarning`; `make
  docs-check-links` 162 documents passed; the acceptance grep and the search
  for stale markers in the nine files of the map entry (empty).
  `gates-light01.log` is the same before the commits.
- `build02.log`: the binaries (plain and `-tags=licensetest`), built as a
  light run with `go build -p 2`. `build01.log`: the same build waited 15
  minutes on the heavy lock behind three lanes and was withdrawn.
  `head-check01.log`: no path other than Markdown changed since that build.
- `gates-heavy01.log`: `TestCLIReferenceDrift` at head `319529fc` through
  the heavy lock, ok (0.027 s), worktree clean before and after. No file of
  `cmd/kudurru` changed in this package.
- Retained failed runs and what they showed. `run01-multi.log` (6 pass, 1
  fail): the check expected one run of `kudurru apply` for the Pi-hole
  guide's three documents; it took three. `run02-zones.log` (16 pass, 9
  fail): the catalog zone was applied on the shared node and every later
  check failed with `generation_pending`, which is how the first defect was
  found. `run03-zones.log` (22 pass, 1 fail): `kudurru query` on a new
  secondary answered `servfail_unsynced` once directly after its first
  transfer, while real DNS answered; the check now waits. `run06-rpz-blocking.log`
  (10 pass, 1 fail): the JSON output of `kudurru blocking disable` has no
  `scope`; the API's blocking state has, and the Pi-hole row says so.
  `run07-examples.log` (36 pass, 2 fail): `kudurru records list` and `zones
  export` directly after `kudurru import bind` answered `unavailable:
  committed zone read: serving: generation pending`; the checklist and the
  Technitium guide now say to run the command again. `run09-all.log` (132
  pass, 2 fail) and `run10-all.log` (134 pass, 1 fail): mistakes of the
  script (an object name used by two sections; a node without an upstream).

Not verified by a run: every Kubernetes statement (no cluster here; they rest
on the code, on the shared stage chain of `serve.go` and on the envtest tests
that DOCS-OPS cites); the enrolled cluster, static twins beyond the blocking
scope, and authenticator codes (DOCS-OPS ran them: `run-cluster*.log`,
`run-totp01.log`); client IDs on DoQ and DoH3 (code only; DoT and DoH were
run); `$ctag` and the name form of `$client` (code only); MAC matching with a
real inventory source; a provider run of an integration; recovery by putting
back the data directory on anything other than a standalone node (the
`unfetchable` section and `probe-restore01.log` are standalone nodes); a
list URL that cannot be downloaded on an enrolled cluster or on Kubernetes.

What remains open: the three defects above are product work, and the pages
carry "in this version" sentences that the fixing change has to lift
(`docs-site/content/migrate/_index.md`, `cutover.md` "Apply results" and the
three pointers to it, `offline-config.md`, the URL sentences of the Pi-hole
and AdGuard examples, `docs/guide/admin/07-zones-and-upstreams.md`,
`docs/architecture/zones.md`). If `CORE-APPLY-MULTI` lands, lift the
one-document paragraph of "Apply results", line 35 of the changelog draft
and the DOCS-OPS paragraph in `docs/deploy/sqlite-cluster.md`. The
Enterprise commit `319529fc` calls the integrations shipped while
`website/src/content.ts` still calls them planned; pick both together or
drop `319529fc`. The same one-document behavior affects the
`kudurru apply` examples outside the migration pages, which this package did
not change. Sentences that name v0.2 as unfinished work stay as they were
(`docs/deploy/response-processing.md:112`, `docs/deploy/iam-upgrade.md:106`),
as do the passages that other lanes own (managed trust anchors in
`dnssec-validation.md`, `integrations.md`, the follower section of
`sqlite-join.md`). The first paragraph of `docs/deploy/client-identity.md`
still says "its management UI and Kubernetes execution remain separate
work"; the new table below it defers to `integrations.md`. The root
`README.md` still describes v0.1.0 in lines 10 to 24 and 44 to 58; it is
integrator-owned.

**Review and verification state at 68a709e2.** The review accepted the seven docs commits with six findings; the fixer was partial (five addressed, one rebutted). The verifier resolved none and left **one major finding unresolved**: the migration pages do not keep a reader out of the state in which a never-downloaded BlockList URL stopped a SQLite node, and "refused once ... Run it again" misleads. It reported **four new problems, open**: `offline-config.md` advises `--select-object` to separate list URLs, which cannot work because the converters put up to 256 URL sources in one BlockList; "minutes apart" understates the fetcher's backoff (one minute doubling to a one-hour cap); `cutover.md` names `prepare_failed` where the reason alternates with `preparing`; and the client-ID sentences say any CIDR matcher wins, while a catch-all `0.0.0.0/0` or `::/0` group comes after the client-ID tier. The owner had the package merged with these open. Since then the code has changed under three of its warnings: one apply applies every document on SQLite (D378), zones the SQLite image cannot serve are refused at write, and a never-downloaded list source is served empty (D373), so the pages warn about defects that the merged code fixes. The commit `docs(enterprise): list background integrations as shipped on SQLite` (`322123ed`) disagrees with `website/src/content.ts`, which needs the owner's authorization to change. DOCS-RECONCILE is to correct all of this; it has not run.
