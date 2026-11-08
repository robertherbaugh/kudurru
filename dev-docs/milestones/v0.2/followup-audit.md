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
