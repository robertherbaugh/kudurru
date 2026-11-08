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
maintains design/contracts/evidence, and runs composed checks. The parent now owns continued
integration and qualification of RFC 5011 trust anchors, stored TSIG activation
and bounded lazy-DFA matching. Retained isolated handoffs supply source and
evidence; they do not authorize parallel checks. Original proofs, ownership,
transactions and final credential/license checks remain required. No refused
mutation replay.

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
   rollback, followed by real Windows/DC/member/DHCP acceptance. The owner has no Windows or two-VM lab and authorized disposable Docker
   acceptance instead. Implement isolated container keepalived/AD protocol
   checks and provide a concrete Windows lab prompt. Samba cannot certify
   Windows DC/member/DHCP behavior; retain that exact external gate.
6. Finish all public contracts, upgrade/failure, performance/interoperability
   and deployment/release gates on the final artifacts. Publishing a release,
   changing release branches or deploying to production is not part of this work.

External decision F23 needs an explicit documented disposition;
where implementation can satisfy the original requirement, do that work.
Fixed-class performance and Windows/two-VM acceptance require their named
runners. Unit tests cannot substitute for those results.

## Complete subrequirement inventory

| ID | Requirement | Workstream | Current work state |
|---|---|---|---|
| F01 | Cold-start cluster-ID check when kube has never reached the API | Store and HA | Integrated 1bf8586; actual startup/identity/health races pass |
| F02a | SQLite remaining §11.4 audit columns, including authz path | Store and HA | Integrated aa3784c; schema-6 attribution and actual mounted races pass |
| F02b | SQLite secret redaction in before/after | Store and HA | Integrated aa3784c; raw-row redaction, historical integrity and export races pass |
| F02c | SQLite cluster epoch / primary fencing setters | Store and HA | Implemented; final gates open |
| F02d | `InternalKinds` once internal kinds exist | Store and HA | Implemented; final gates open |
| F03 | `WatchOptions.ResourceVersion`: replay-as-Added vs true resume | Store and HA | Implemented exact Kubernetes retained-event resume; actual envtest history/namespace/private-kind races and terminal-error/backpressure tests pass |
| F04 | Shared sqlite/kube table rejecting the same inputs | Store and HA | Integrated d4075f5; scoped real-driver races pass |
| F05 | Cache parsed endpoints per record-set object | Zones and AD | Integrated eb20ac7; composed actual-driver and parser races pass |
| F06a | Separate NOTIFY TSIG key field in ZonePolicy | Zones and AD | Integrated c80c6f2/46d7641; actual sender and composed SQLite/Kubernetes schema races pass |
| F06b | Per-zone `notifySource` | Zones and AD | Integrated c80c6f2/46d7641; actual source/refusal races and UI unit tests pass |
| F07a | Encrypted store-backed internal TSIGKey | Zones and AD | Active D292: SQLite captured listener and secondary/RPZ activation integrated f725fdf; merged mounted races and scoped lint pass. SQLite management/API/generated clients now composed on schema8; final-clock and actual commit-error cases pass, external API races17 and full lint pass. Mounted DNS startup refusal and own-UPDATE acknowledgment corrected in D313; core races581/4 opt-in-helper skips, corrected output races14 and full lint pass. Observed Kubernetes identity prerequisite integrated 038fc54f. D314 startup-only stored keys now mount all six transports, secondary and NOTIFY; actual envtest restart/outage/wrong-owner races, 650 core race entries, 31 compatibility entries, full lint and vet pass. Kubernetes management/current authority and final gates remain open |
| F07b | TSIG on DoH/DoQ/DoH3 | Zones and AD | Integrated a6bf8e1; actual encrypted wire and native mutation-refusal races pass; immutable installed keyrings |
| F08 | `doctor` self-NOTIFY / egress-address round trip | Zones and AD | Integrated c46f8c9; authenticated SQLite native/CLI races pass; Kubernetes qualification remains open |
| F09a | Per-integration elector, rather than one global Runner lease | Zones and AD | Implemented; final gates open |
| F09b | Production integration-host wiring and failover | Zones and AD | SQLite mount integrated 687371f; Kubernetes atomic-source and Delete dependencies remain |
| F10a | Reload inline objects without restart | Zones and AD | Integrated 71c0393; whole-source refusal and actual mounted reload races pass |
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
| F16 | Current-hour query stats beyond 100k distinct names | DNS and resolver | Integrated 8b98f41; exact local/central index, real migrations/races and 1M-row scale checks pass; parent composed PostgreSQL/API/SQLite races pass |
| F17 | `$important` / `$badfilter` | DNS and resolver | Implemented; final gates open |
| F18a | `control.allowedNetworks` / `control.blockedQTypes` config | DNS and resolver | Integrated 50773f9; generated schemas and composed mounted/publication races pass |
| F18b | Reverse-PTR hostname cache | DNS and resolver | Integrated 83c1096; bounded async PTR, actual UDP and mounted SQLite policy/expiry races; scoped lint pass |
| F18c | UniFi hostname source | DNS and resolver | Integrated 380f43f; actual TLS/SQLite/native/PROXYv2/dnsdist lifecycle races pass. Fixture correction 4337c79 retains the original five-second held-CNAME deadline; repeated focused and full mounted acceptance pass, with earlier failed commands retained in the ledger |
| F19a | Durable replicated blocking toggle on complete SQLite path | Store and HA | Implemented; final gates open |
| F19b | Kube runtime annotation publisher/watcher or equivalent cluster toggle | Store and HA | Implemented D301: original bounded ConfigMap CAS, confirmed local application and read reconciliation on all replicas. Actual two-runServe/API-server DNS, shared expiry, restart and outage acceptance passes; driver conflict/lost-reply, original credential/cancellation and audit races plus vet pass. Earlier fixture failures retained; configured lint and final supported-driver gates remain |
| F20a | AdGuard black-box conformance implementation | DNS and resolver | Implemented; final gates open |
| F20b | Real AdGuard/container qualification and CI evidence | DNS and resolver | Integrated; final gates open |
| F20c | Move benchgate to `hack/` | DNS and resolver | Integrated; sample-count/processor/CPU and numeric integrity guards pass race25, vet and scoped lint. Retained full gate findings remain byte-identical FAIL; final gates open |
| F20d | Lazy-DFA regex engine | DNS and resolver | Integrated D293 with parent modifier-aware fingerprint/parser fixes. Root core race835 (2 actual helper/comparator skips), mounted API/command race47, pinned comparator46 and vet pass. Original12 single diagnostic has all58 metrics: unchanged1M 67115699ns/19allocs; regex1000 p99 18276ns remains adverse. Historical five-process arithmetic FAIL remains 36 measured regressions plus20 missing zone keys, with CPU differences and official/static gates open. Sparse shared-batch dispatch passes focused race88/core race452 (2 actual skips) and vet; one original12 sample improves policy Evaluate miss to740.5ns but regex1000 p99 17691ns and unchanged18allocs remain adverse. Fingerprint allocation fix passes focused101/core race641 (2 actual skips), pinned comparator46 and vet; original1M diagnostic reaches8allocs at79681673ns, preserving first10alloc guard failure and all earlier performance evidence. Proven anchored-start fallback passes focused race98/core467, pinned comparator46, vet and bounded oracle fuzz157288 executions; original69/all291 metrics retain3 arithmetic regressions and original12 regex1000 p99 18550ns remains adverse. Full configured root lint v2.15.0 passes. Complete original five-process blocking+zone gate on 859bacf6 exits1 with four regressions and no missing metrics; actual source-profile-guided literal refinement passes focused93/core656 races, pinned comparator46, fuzz197295, vet and scoped lint. Original regex1000 diagnostic p99 improves18226 to1455ns; final-source five-process/fixed-class gates remain open |
| F21a | `upstreams.importFile` dnsmasq import | DNS and resolver | Implemented; bounded canonical translation, refusal/ownership/restart and real mounted DNS routes pass |
| F21b | Response-cache inspect REST | DNS and resolver | Implemented bounded exact-key metadata read; real mounted DNS/API and held-publication/ABA races pass; generated 157-operation client and API/reference contracts updated |
| F21c | Cache/strategy benchmark limits | DNS and resolver | Original workloads included in the new collector; named-class calibration and absolute limits remain open |
| F22a | QUIC max connections / TCP queries / idle timeout config keys | DNS and resolver | Implemented; actual transport/config/mounted races pass |
| F22b | DoH `Alt-Svc` | DNS and resolver | Implemented; actual HTTPS/HTTP3 and full transport races pass |
| F22c | RFC 8467 response padding | DNS and resolver | Implemented; actual encrypted/plaintext/TSIG and full transport races pass |
| F22d | DoH path client IDs | DNS and resolver | Implemented; final gates open |
| F23 | Separate public blocked-services/safe-search data repo (D120) | DNS and resolver | External acceptance/decision open |
| F24a | Rate-limit slip metric | DNS and resolver | Integrated; final gates open |
| F24b | Per-ClientGroup rate-limit exemptions | DNS and resolver | Integrated 3234ad4; actual mounted UDP/TCP/publication, real Kubernetes admission and 13 UI cases pass; generated CRDs included |
| F25a | Multi-socket DoT | DNS and resolver | Integrated eeb68c2; real TLS/rotation/limits/shutdown and local socket benchmarks pass |
| F25b | Multi-socket DoQ/DoH3 with connection-ID-aware routing | DNS and resolver | Implemented D311: actual IPv4/IPv6 routing, QUIC v1/v2/Retry, repeated migration and failure cleanup; final upstream CID race39, transport/config race706 (4 existing skips), mounted signed restart1 and vet pass. Local one/four-socket sample improves DoQ 12.33% but worsens HTTP3 12.54%; full configured root lint now passes; fixed-class/final gates remain open |
| F26a | Privacy reload preserves daily pseudonym identity | DNS and resolver | Implemented; lifecycle/concurrency/production reload races pass |
| F26b | Cache growth retains initial shard count | DNS and resolver | Integrated 62990dd; safe resharding, mounted reload, races and local growth benchmarks pass |
| F26c | Blocking refresh/degraded settings hot reload | DNS and resolver | Integrated 38153fa; real download/publication and scheduler/readiness races pass; generated inheritance schema included |
| F26d | Querylog enabled/sink hot reload | DNS and resolver | Integrated 08ee759/9a62eb4; captured sink/drain, mounted reload and disabled-restart history races pass |
| F26e | Auth settings hot reload | IAM and access | Implemented D290 on complete SQLite: five controls, exact host locks and original-view cookies; mounted TLS/router/restart/current-command races pass. Legacy/Kubernetes and host inputs remain restart-bound |
| F27 | RFC 8198 aggressive NSEC/NSEC3 cached synthesis | DNS and resolver | Implemented D296: bounded complete-proof reuse with original expiry and cumulative hash reserve; full affected races, actual BIND authority and delayed outer-cache acceptance pass. Final merged/release gates remain |
| F28 | RFC 5011 automatic trust-anchor updates | DNS and resolver | Active D297: schema8/local denial, common mode drain, committed builder/root seal, actual output and raw-cache insertion prerequisites composed. DNS-server race344/skip4, root output17, raw-cache stage45 and final builder/cache31 entries pass; scoped lint passes. Original failed commands retained. API dig final handoff now has 36 API and 4 Store-backed query race passes on final production, plus scoped lint0; earlier failed commands retained. Native mutation admission now passes full native race228/skip2, builder/root28 and real signed managed-denial3; scoped lint0, prior lint/preparation failures retained. Immutable edge bypass and installed acknowledgment now pass actual pinned YAML/Lua warm-cache, source-epoch, crash and lost-ack races (final3 entries), with lint0; earlier CLI/live/lint failures retained. Private updater scheduler now passes 29 race entries/15 top-level cases and scoped lint0; original lint failure retained. One-shot follower socket acquisition/restart/promotion composition now passes combined37 race entries/18 top-level cases and lint0. New private follower disposition/poller passes SQLite128/root50 default-race entries and focused vet0; full configured root lint now passes. Follower loop remains unmounted. D315 retained directory policy now passes focused race21, actual pinned restart3 and full edge race189/2 opt-in live skips; malformed/downgraded policy refuses without repair. Backend-to-edge activation admission, elected mount, full retirement acceptance and interoperability remain; managed activation disabled |
| F29a | Expiring negative trust anchors for forwarded validation | DNS and resolver | Implemented; final gates open |
| F29b | Negative trust anchors for recursive groups | DNS and resolver | Integrated ddc5f2d; native/socket, captured-publication and actual Kubernetes schema races pass |
| F30 | DNSSEC validation of forwarded answers | DNS and resolver | Implemented; final gates open |
| F31 | DNS cookies to authoritative servers | DNS and resolver | Integrated 668203c; composed native/socket/DNSSEC/cache races pass |
| F32 | Persist node-scope API changes or remove write half | Operations and UI | Integrated ffcf20e/bd47907; SQLite durable reset/host precedence; unsupported node writes refused with UI capability; actual restart/envtest/UI checks pass |
| F33 | Remote `GET /nodes/{name}/health` | Operations and UI | Integrated; final gates open |
| F34 | `/auth/me` exposes zone-scoped permissions | IAM and access | Implemented; final gates open |
| F35a | Read external-dns `DNSEndpoint` CRs directly | Zones and AD | Implemented D302/D303: elected bounded read-only source reconciles UID-owned native children with original source/zone checks and destination CAS. Actual Kubernetes permissions/watch/refusal and mounted DNS/restart/deletion races pass; legacy ownership regressions, vet, docs drift and Helm rendering pass. Separate API objects are not an atomic transaction; multi-node lease-loss/partition and final G09/G21/G22 gates remain |
| F35b | `kudurru externaldns-webhook` sidecar subcommand | Zones and AD | Implemented D288 loopback relay to existing webhook; actual TLS/router and Kubernetes-backed CLI CRUD/ownership, cancellation/refusal races and lint pass |
| F35c | external-dns webhook authentication | Zones and AD | Integrated; final gates open |
| F36 | Serve object lists from live snapshot instead of store | Operations and UI | Implemented; final gates open |
| F37 | Adopt generated OpenAPI Go client underneath CLI client | Operations and UI | Integrated a106708 plus generated 156-operation dispatch; complete adapter/generator and selected actual-router CLI races pass |
| F38 | Dedicated SQLite authz path/request/client audit columns | Observability and release | Integrated aa3784c; dedicated authenticated attribution columns |
| F39a | Local edge resolver-cache invalidation | Edge and VIP | Implemented; final gates open |
| F39b | Remote edge blocking-generation pull / resolver invalidation | Edge and VIP | Implemented D283: bounded authenticated original-publication grants, safe uncached startup, both-pool client/ACL bypass and actual dnsdist acceptance; merged topology/release gates remain |
| F40a | Host-only `edge.*` / `vip.*` settings keys | Edge and VIP | Integrated 52ec29d; 70 typed keys, actual render/precedence/API-refusal races and chart/Compose checks pass; generated reference/mock updated |
| F40b | Keepalived provider events to audit/SSE in serve | Edge and VIP | Integrated 380458f; bounded private FIFO journal to audit/SSE, original token/source expiry and source refusal races pass |
| F41a | ServiceProvider current Owner | Edge and VIP | Integrated 1c934e7/9990ede; bounded host-selected provider evidence, actual envtest/mounted read/expiry and Node replacement races pass; physical ownership remains separate |
| F41b | Chart provider annotations | Edge and VIP | Implemented; actual Helm/Go parity across 16 supported modes and four topology refusals pass; owner observation separate |
| F42 | Per-VRID `--hmac-key-file` in chart | Edge and VIP | Integrated 49a9cf8/376d63d/e760dd7; chart/CLI races and real keepalived staging/preflight pass |
| F43a | dnsdist console backend diff-apply | Edge and VIP | Integrated e60001f/88ca477; targeted first-party health qualifies same-PID named additions; actual pinned YAML/Lua and default races pass; legacy structural fallback explicit |
| F43b | `reloadAllCertificates()` without restart | Edge and VIP | Integrated e60001f; real pinned all-frontend leaf checks, held DoT, invalid-pair retention and child-restart acceptance pass |
| F43c | Console spike (a) gates both changes | Edge and VIP | D275: actual dnsdist 2.1.3 YAML console, live DNS/cert/invalid-cert/PID/restart checks pass; established-connection limit recorded |
| F44a | Persist grant revocation across restart/regrant | IAM and access | Implemented; final gates open |
| F44b | Cluster-wide production use on every advertised driver/route | IAM and access | D294 public SQLite RoleBinding API/CLI integrated c44b67d; actual TLS/router/replica focused races and lint pass. Merged 180-operation contracts pass; file/inline security effects integrated 91443d7 with real OP/startup/restart/replica races and lint. Kubernetes accepted-authority and final composed gates remain open |
| F45a | Stored OIDC identities with independent UID | IAM and access | Implemented; final gates open |
| F45b | External per-user disable / log out everywhere administration | IAM and access | Implemented D289 on complete SQLite: exact UID/revision API/CLI/UI, atomic credential/audit revocation, actual TLS/router/restart/replica races, nine dashboard tests and lint/contracts pass; unsupported Stack explicitly refuses |
| F46 | Share login lockout counters across replicas | IAM and access | Kubernetes password admission implemented D304 with actual two-runServe/CAS/restart/outage races. SQLite login implemented D305: original-command counters and enrollment, typed pre-mutation refusals, exact rollback and physical follower restart; focused race11/full owner-auth race324 and vet pass. Browser own-commit regression fixed D306; complete HTTP race 108/58 top-level plus final browser race 52/23 and vet pass, earlier failures retained. SQLite TOTP management shares counters in D307: focused race 24, full security/loginlimit race 110 and vet pass. SQLite password changes share counters in D308: full security race 105, focused HTTP race 18 and vet pass. SQLite browser/CLI request budgets mounted D309 before the first auth capture: full owner race 45, final focused race 32 including installed Chrome, and vet pass; earlier concurrency/partial-fixture failures retained. Kubernetes browser admission mounted D310 with backend race 22, two actual mounted replica/restart/outage passes, full Stack race 46 and vet pass. Enrolled-replica forwarding and final gates remain |
| F47 | Bound offline Kubernetes SA tokens or periodically TokenReview | IAM and access | Integrated 101b614/9e7b177/41a7694; real envtest and races pass |
| F48a | `autoProvision: never` and `syncGroups` with user store | IAM and access | Implemented; final gates open |
| F48b | RP-initiated logout | IAM and access | Integrated 2947b02/6f278bf; actual TLS OP/login/revocation/logout/callback, source/credential/expiry refusals, scoped races and dashboard tests pass |
| F49 | Optional external-dns shared-secret header / tokenFile | IAM and access | Integrated; final gates open |
| F50 | Separate Kubernetes vs Kudurru SA binding subject or document overlap | IAM and access | D254 documents shared namespace/name compatibility; actual binding engine/token-scope races pass |
| F51 | Optional HTTPS-only catalogue and BlockList downloads | Operations and UI | Integrated a15480e; HTTP/TLS, redirect, real startup and API refusal races pass |
| F52 | Dig answer reports matched exclusion, avoiding second request | Operations and UI | Integrated 8879459; original selected-rule evidence, mounted/API races and 28 domain UI cases pass |
| F53 | Query-time exact ClientGroup scope for allow/deny lists (D143) | Operations and UI | Integrated 926b2a3; reproduced shared-policy leak fixed, mounted native/cache/publication races and actual Kubernetes admission pass |
| F54a | Regex “overrides block list” explanation | Operations and UI | Implemented; original-publication API/SQLite races and 32 domain UI cases pass; concrete-name limits explicit |
| F54b | Regex allow/deny cache invalidation | Operations and UI | Implemented D283: local accepted-publication flush and remote cache replacement; actual regex allow/deny, late response and two-client safety acceptance pass; asynchronous convergence explicit |
| F55 | Per-digest index for paginated URL entries | Operations and UI | Implemented; bounded parsed indexes, original-source/eviction races and two-million-rule local benchmark pass |
| F56 | Self-host Inter/JetBrains Mono in Kudurru theme | Operations and UI | Integrated 13c0c9e; 7 browser/9 theme tests and 480.5 KiB entry payload pass |
| F57 | Nested Shape help for list/map elements/keys | Operations and UI | Integrated; final gates open |
| F58a | Astryx record-delete AlertDialog | Operations and UI | Implemented; final gates open |
| F58b | `/queries/export` API and query-log link | Operations and UI | Implemented; filtered bounded download, API/privacy races, 20 UI cases and actual mounted DNS/history export pass |
| F59 | `--context` chooses kubeconfig/direct CRD verbs | Operations and UI | Implemented explicit direct CRD transport; real envtest context/CRUD/RBAC/ownership and replacement-refusal races pass |
| F60a | Offline `import adguard` / `import blocky` | Store and HA | Integrated; final gates open |
| F60b | `cluster status` / `promote` / `join-token` | Store and HA | Status CLI implemented on the actual authenticated route with table/wide/JSON/YAML; real-router race9 and vet pass. Peer/coordinator original-deadline admission and actual command commit/no-op expiry now pass 160 affected race entries with the real Kubernetes Lease handoff, vet and scoped lint. Enrollment, join-token, promotion and forwarding remain open; status grants no promotion eligibility |
| F60c | `integrations …` CLI | Store and HA | Active D291/D295/D298: reviewed API/CLI, standalone conformance and SQLite Tier-B clients/actions mount implemented; mounted TLS/router races, full provider races and scoped lint pass. Merged 180-operation contracts pass; hooks/record ownership, remaining driver/restart and final contract gates remain |
| F60d | Zone option writes via PUT/PATCH | Store and HA | Integrated c9f14dc; actual-router/current SQLite final-expiry races and lint pass |
| F61a | CLI browser loopback + PKCE login | IAM and access | Integrated 4fddbce; original public CLI ID-token bearer storage, real TLS OP/loopback/installed Chrome, scoped races and lint pass; merged 169-operation contracts and selected CLI login checks pass |
| F61b | CLI device-code login | IAM and access | Integrated b09fc77; actual TLS RFC 8628 polling/refusals, raw public ID-token storage, original expiry/source/cancellation, scoped races and lint pass; merged 169-operation contracts and selected CLI login checks pass |
| F62a | CLI conformance against actual API router | Operations and UI | Integrated 6938fbb; actual router/CLI races and scoped lint pass |
| F62b | §10 documents ApplyResult errorKey/hint, wrapped settings, SSE event | Operations and UI | Integrated c439532; exact wire contracts and API serialization races pass |
| F63 | Fixed-class benchmark job, including UDP sockets and Persist200k | Observability and release | Manual class-parameterized workflow and eight-suite fresh-process collector implemented; control tests4, gate race37, vet and scoped lint pass. Actual local expanded collection completes 40 native commands and seven shape/absolute checks, 136 cases/592 metrics/five samples each; separate original relative gate exits 1 with three suffix-miss regressions. Named class choice, calibrated baselines/limits and hosted qualification remain open |
| F64a | `kudurru_cluster_fenced` metric/dashboard/alert | Store and HA | Implemented and scoped tests pass; final gates open |
| F64b | `/healthz` snapshotAge / leader | Store and HA | Implemented; actual kube outage/identity/health races pass |
| F64c | DNSSEC signing-key expiry metric | Store and HA | Original v1.0 subtarget; not a v0.2 closure |
| F65 | `cluster.role: static` / Compose read-only HA membership | Store and HA | Implemented D300 static SQLite twins: bounded host peers, local leadership, enrolled-database refusal, API/UI/public block-page read-only boundaries, authenticated status/digest reads and node-local controls. Two actual runServe assemblies pass peer-loss/restart acceptance; full affected package races and vet pass after one enum-fixture correction. Separate-process DNS/status/body/refusal/local-control/peer-loss/restart acceptance and E2E vet also pass; full configured root lint now passes. Enrolled primary/follower authority is separate F60b/G11 work |
| F66 | Separate querylog.db / local.db path keys and volumes | Observability and release | Integrated b3f6986/5048c61; actual restart, chart/Compose and volume ownership checks pass |
| F67 | E2E-2 static SQLite cluster, peer list bodies, correct leader | Store and HA | Actual two-runServe static membership/status/body/refusal/local-control/peer-loss/restart race passes. Separate server-process acceptance also passes with actual DNS, authenticated body reads, local-only control and durable identity/session restart; enrolled primary/follower promotion, partition/rejoin and final G11/G22/G24 remain open |
| F68a | AdGuard Home black-box e2e | Zones and AD | Integrated; final gates open |
| F68b | Knot catalog consumer interoperability | Zones and AD | Integrated 6aa58ed; actual pinned Knot 3.6.2 catalog/NOTIFY/TSIG acceptance and repeated races pass |
| F68c | Two-VM keepalived failover acceptance | Zones and AD | Container alternative passes 12/12; original VM qualification remains external |
| F69 | Central reads for retired node via scope=node&node=other | Observability and release | Integrated a4e38ce; actual PostgreSQL isolation and mounted history/export races pass |
| F70 | Prune unreferenced central dictionaries | Observability and release | Integrated eef61bd/b1a29e8/a7304d8; actual PostgreSQL 17 races and lint pass |
| F71 | Summarize facts of permanently stopped sources' last partial hour | Observability and release | Integrated 123148b/f8fbb85; source-owned recovery and real PostgreSQL races pass; ambiguous legacy input retains explicit provenance gate |
| F72a | Diagnosis page: missing AuditLog CRD and object-count recommendation | Observability and release | Integrated e77d025; bounded real-envtest and composed auth races pass |
| F72b | Same AuditLog checks in `kudurru doctor` | Observability and release | Integrated e77d025; actual router/CLI and composed races pass |
| F73 | AuditLog verification-key lifecycle independent of session rotations | Observability and release | Integrated d3604c1; independent trust, historical retention, real envtest and mounted races pass; explicit upgrade provisioning required |
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

Current integrated static check: golangci-lint v2.15.0 scans all root packages
with the unchanged repository rules and passes with zero issues (November 8).
Earlier unavailable/failed lint evidence remains in the audit; this does not
complete the remaining implementation, performance or external release gates.

Composed November 8 qualification on 19d6c729 passes full SQLite/trust-anchor
race suites (964 entries), serving/build/primary/runtime-owner races (477),
actual PostgreSQL central/audit races (244), and the separately enabled original
million-row central scale test (one). The initial scale skip remains recorded;
the explicit scale invocation passes without skips. `make size` produces an
80,384,160-byte CGO-disabled static ELF under the original 100,000,000-byte limit.
Exact outcomes and artifact qualifications are in the audit and gap ledger;
remaining implementation, performance and external release gates stay open.

The measured zone CNAME tail has a one-allocation answer-assembly refinement;
full zone/cache/primary/authoritative races pass 659 entries with zero skips,
plus vet and scoped lint. The unchanged original five-process run on 6cace80f now completes all ten
commands and 439 metrics, but the actual gate still exits 1: exact-list miss p50
158 ns versus 109 ns remains regressed. Regex-1000 p99 is 1,593 ns and CNAME
p99 is 1,291 ns with one allocation; no absolute-limit violation or missing
metric. Prior failures remain recorded; fixed-class qualification remains open.

The separate expanded eight-suite collector on 63a52742 completes all 40 native
commands and seven shape/absolute checks with exit 0; its original relative
comparison still exits 1 (three suffix-miss regressions). It records 136 cases,
592 metric keys and 2,960 values with five samples each. Earlier failures remain
retained. Named-class execution and new cache/strategy calibration remain open;
local recording success is not release qualification.
