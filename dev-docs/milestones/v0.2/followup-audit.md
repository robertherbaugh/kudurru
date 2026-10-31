# v0.2 follow-up evidence audit

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
| F23 | Separate public blocked-services/safe-search data repo (D120) | External | `internal/listdata/catalogue/` remains embedded; `cmd/kudurru/catalogue.go` supports replacement URL/provenance. | Owner creates/publishes separate permissively licensed data repo; pin provenance/update behavior. Existing override feature does not perform that publication; G07, G24. |
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
| G24 / deployment/release | F15, F23, F41–F43, F56, F63–F68, F74; applicable release-checklist, license/artifact and two-VM evidence remain open. |

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
