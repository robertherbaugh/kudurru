# v0.2 continuing completion plan

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
5. Preserve the AD preview handoff under D353. The owner deferred the remaining
   AD implementation and Windows matrix to a future agent; no Windows lab is
   assigned. Continue shared DNS/HA and non-AD deployment acceptance. Existing
   Linux/Samba protocol evidence remains distinct from Windows qualification.
6. Finish all public contracts, upgrade/failure, performance/interoperability
   and deployment/release gates on the final artifacts. Publishing a release,
   changing release branches or deploying to production is not part of this work.

F23 is published with explicit owner authorization; its immutable catalog URL
and provenance are documented. Continue the remaining implementation and gates.
Fixed-class performance and non-AD deployment acceptance require their named
runners. Windows acceptance remains deferred preview work under D353. Unit
tests cannot substitute for any external result.

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
| F08 | `doctor` self-NOTIFY / egress-address round trip | Zones and AD | Implemented D261/D331; authenticated SQLite native/CLI and actual Kubernetes stored/file/restart/refusal races pass; final gates open |
| F09a | Per-integration elector, rather than one global Runner lease | Zones and AD | Implemented; final gates open |
| F09b | Production integration-host wiring and failover | Zones and AD | SQLite mount integrated 687371f; Kubernetes atomic-source and Delete dependencies remain |
| F10a | Reload inline objects without restart | Zones and AD | Integrated 71c0393; whole-source refusal and actual mounted reload races pass |
| F10b | `Policy/default` as explicit fallback | Zones and AD | Implemented; final gates open |
| F11a | Server-side Kerberos/GSS-TSIG/TKEY feasibility implementation | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F11b | Real Windows DC/member/DHCP; SPN provisioning/rotation; edge/VIP routing | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F12a | Atomic RFC 2136 storage for each advertised driver | Zones and AD | Queued: partial |
| F12b | Principal/record ownership and AD permission translation | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F12c | Imported-record adoption for legitimate AD registrants | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F12d | Durable content/security/aging replication and fenced failover | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F13a | AD discovery/import metadata and blocker reports | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F13b | Shadow, final write handoff, reconciliation, lossless rollback | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F14a | Windows DHCP, enabled name protection and shared credentials | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F14b | Aging/scavenging with static-record protection | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F14c | AD administration/diagnostics/backup and restore | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
| F15 | Windows acceptance with every DC DNS service stopped | Zones and AD | Preview; remaining AD implementation and Windows acceptance deferred by owner (D353); original requirements retained |
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
| F21c | Cache/strategy benchmark limits | DNS and resolver | Original cache/strategy ceilings calibrated from Ubicloud reference and enforced; paired Ubicloud 42554034011 on 9963dec2 passes both scopes and all absolute limits. Earlier failures remain preserved; later-source/final qualification remains open |
| F22a | QUIC max connections / TCP queries / idle timeout config keys | DNS and resolver | Implemented; actual transport/config/mounted races pass |
| F22b | DoH `Alt-Svc` | DNS and resolver | Implemented; actual HTTPS/HTTP3 and full transport races pass |
| F22c | RFC 8467 response padding | DNS and resolver | Implemented; actual encrypted/plaintext/TSIG and full transport races pass |
| F22d | DoH path client IDs | DNS and resolver | Implemented; final gates open |
| F23 | Separate public blocked-services/safe-search data repo (D120) | DNS and resolver | Complete D333: owner-authorized public robertherbaugh/kudurru-data at 13d8d42b; all published blobs and HTTPS catalog verified, pinned update/provenance contract documented; consumer races pass |
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
| F28 | RFC 5011 automatic trust-anchor updates | DNS and resolver | Active D297/D315–D319: schema8 accepted history/local denial, original receipt authentication, complete managed publication, common mode/output retirement, native/raw cache admission and installed edge-bypass prerequisites are integrated. Direct SQLite activation mounts the elected updater and independent follower supervisor. Runtime terminal writer failures survive host/Runner restarts; follower terminal results survive same-term replica installs and their incarnation increments, while role changes join old sockets. Actual native recursive signed DNSKEY passes all six transports and restart without rebootstrap/progress (ordinary1, combined direct race2). Final follower/root/runtime race106 and actual runServe follower revoke/all-six-output/restart race1 pass without skips. Config/reference354 pass; generator and final full configured lint pass. Fresh same-term installed-source acquisition after successful normal polling now passes27 follower race entries/no skips; held and terminal failures remain retained. Prior failures, exact scopes and earlier qualified prerequisites remain in the gap/audit ledgers. Canonical writer all-anchor-loss/output-drain and physical same-database restart now pass ordinary1/race1; managed/static/disabled restart preserves quarantine and refuses fallback. Explicit edge-fleet activation now captures one original local cut and adopts authenticated installed persistent bypass from every declared peer; pinned dnsdist/backend UDP/TCP both-pool restart acceptance and composed race133 pass with no skips. D320 recovery primitives now preserve bounded explicit trust history and tombstones through original SQLite commit, follower installation and physical restart; corrected focused recovery race2 passes. The first broad SQLite race timed out after975 passing entries; the final sequential full trustanchors/SQLite/runtimeowners default-race suite passes1026 without failures/skips, preserving the earlier timeout. D321 offline status/recovery now requires the original exact digest and independently trusted DS, strictly opens the existing current-schema database, and commits the OS-attributed audit with trust; strict-open race17 and actual command/native-six-transport/physical-restart race8 pass without skips. Pre-change codec compatibility race1 preserves format1 and rejects recovery format2. Fleet mixed-version acceptance, initial empty edge-follower installation, full enrolled HA orchestration, RFC interoperability and final gates remain open |
| F29a | Expiring negative trust anchors for forwarded validation | DNS and resolver | Implemented; final gates open |
| F29b | Negative trust anchors for recursive groups | DNS and resolver | Integrated ddc5f2d; native/socket, captured-publication and actual Kubernetes schema races pass |
| F30 | DNSSEC validation of forwarded answers | DNS and resolver | Implemented; final gates open |
| F31 | DNS cookies to authoritative servers | DNS and resolver | Integrated 668203c; composed native/socket/DNSSEC/cache races pass |
| F32 | Persist node-scope API changes or remove write half | Operations and UI | Integrated ffcf20e/bd47907; SQLite durable reset/host precedence; unsupported node writes refused with UI capability; actual restart/envtest/UI checks pass |
| F33 | Remote `GET /nodes/{name}/health` | Operations and UI | Integrated; final gates open |
| F34 | `/auth/me` exposes zone-scoped permissions | IAM and access | Implemented; final gates open |
| F35a | Read external-dns `DNSEndpoint` CRs directly | Zones and AD | Implemented D302/D303: elected bounded read-only source reconciles UID-owned native children with original source/zone checks and destination CAS. Actual Kubernetes permissions/watch/refusal and mounted DNS/restart/deletion races pass; legacy ownership regressions, vet, docs drift and Helm rendering pass. Separate API objects are not an atomic transaction; two-replica lease-loss/partition acceptance passes (November 9); final G09/G21/G22 gates remain |
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
| F46 | Share login lockout counters across replicas | IAM and access | Kubernetes password admission implemented D304 with actual two-runServe/CAS/restart/outage races. SQLite login implemented D305: original-command counters and enrollment, typed pre-mutation refusals, exact rollback and physical follower restart; focused race11/full owner-auth race324 and vet pass. Browser own-commit regression fixed D306; complete HTTP race 108/58 top-level plus final browser race 52/23 and vet pass, earlier failures retained. SQLite TOTP management shares counters in D307: focused race 24, full security/loginlimit race 110 and vet pass. SQLite password changes share counters in D308: full security race 105, focused HTTP race 18 and vet pass. SQLite browser/CLI request budgets mounted D309 before the first auth capture: full owner race 45, final focused race 32 including installed Chrome, and vet pass; earlier concurrency/partial-fixture failures retained. Kubernetes browser admission mounted D310 with backend race 22, two actual mounted replica/restart/outage passes, full Stack race 46 and vet pass. D363 enrolled-follower local login forwards to the primary's shared admission with real two-process acceptance; OIDC/browser/device logins on followers and final gates remain |
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
| F60b | `cluster status` / `promote` / `join-token` | Store and HA | Status CLI implemented on the actual authenticated route with table/wide/JSON/YAML; real-router race9 and vet pass. Peer/coordinator original-deadline admission and actual command commit/no-op expiry now pass 160 affected race entries with the real Kubernetes Lease handoff, vet and scoped lint. Retained enrolled TLS listener startup and the typed root blocking executor are mounted with scoped race/lint evidence recorded below. Reviewed first-primary activation API/CLI is mounted (D345); reviewed join-token issuance is mounted (D347); retained enrolled acquisition/publication/restart is mounted (D351), with 444 affected race entries and scoped lint passing; operator promotion API/CLI is mounted (D354), with actual TLS-loss, original-proof, audit, publication and restart races; initial remote join is mounted (D355) and explicit incomplete-join reconciliation retains original intent (D356); original follower blocking forwarding is mounted (D357), with 254 compatibility race entries and scoped lint/SAST passing; D359 bounded peer authority observation and two-process promoted rejoin pass, including 321 subsequent compatibility race entries; D360 fenced/conflicted DNS-only publication and D361 offline audited branch recovery pass three-process conflicting-promotion acceptance with 748 SQLite, 433 cluster and 274 root race entries; D362 typed follower forwarding of canonical object writes and zone commands passes real two-process acceptance; settings, IAM/login admission and other domains remain open; status grants no promotion eligibility |
| F60c | `integrations …` CLI | Store and HA | Active D291/D295/D298: reviewed API/CLI, standalone conformance and SQLite Tier-B clients/actions mount implemented; mounted TLS/router races, full provider races and scoped lint pass. Merged 180-operation contracts pass; hooks/record ownership, remaining driver/restart and final contract gates remain |
| F60d | Zone option writes via PUT/PATCH | Store and HA | Integrated c9f14dc; actual-router/current SQLite final-expiry races and lint pass |
| F61a | CLI browser loopback + PKCE login | IAM and access | Integrated 4fddbce; original public CLI ID-token bearer storage, real TLS OP/loopback/installed Chrome, scoped races and lint pass; merged 169-operation contracts and selected CLI login checks pass |
| F61b | CLI device-code login | IAM and access | Integrated b09fc77; actual TLS RFC 8628 polling/refusals, raw public ID-token storage, original expiry/source/cancellation, scoped races and lint pass; merged 169-operation contracts and selected CLI login checks pass |
| F62a | CLI conformance against actual API router | Operations and UI | Integrated 6938fbb; actual router/CLI races and scoped lint pass |
| F62b | §10 documents ApplyResult errorKey/hint, wrapped settings, SSE event | Operations and UI | Integrated c439532; exact wire contracts and API serialization races pass |
| F63 | Fixed-class benchmark job, including UDP sockets and Persist200k | Observability and release | Manual class-parameterized workflow and eight-suite fresh-process collector implemented; control tests4, gate race37, vet and scoped lint pass. Actual local expanded collection completes 40 native commands and seven shape/absolute checks, 136 cases/592 metrics/five samples each; separate original relative gate exits 1 with three suffix-miss regressions. Owner selected Ubicloud 8-core Ubuntu 24.04 (ubicloud-standard-8-ubuntu-2404), retaining GOMAXPROCS6. Original cache/strategy limits are calibrated. Paired Ubicloud run42518988512 performs actual strict same-VM comparisons and fails one blocking/eight cache metrics; strategy passes. New paired run42522360297 on91cb21bb passes blocking-zone/upstream and cache memory but fails cache hit/Set latency; all811 five-value vectors and failures retained. Initial socket/persistence bounds now calibrated from first hosted recording42514599389 (D335); current-source and final gates stay open |
| F64a | `kudurru_cluster_fenced` metric/dashboard/alert | Store and HA | Implemented and scoped tests pass; final gates open |
| F64b | `/healthz` snapshotAge / leader | Store and HA | Implemented; actual kube outage/identity/health races pass |
| F64c | DNSSEC signing-key expiry metric | Store and HA | Original v1.0 subtarget; not a v0.2 closure |
| F65 | `cluster.role: static` / Compose read-only HA membership | Store and HA | Implemented D300 static SQLite twins: bounded host peers, local leadership, enrolled-database refusal, API/UI/public block-page read-only boundaries, authenticated status/digest reads and node-local controls. Two actual runServe assemblies pass peer-loss/restart acceptance; full affected package races and vet pass after one enum-fixture correction. Separate-process DNS/status/body/refusal/local-control/peer-loss/restart acceptance and E2E vet also pass; full configured root lint now passes. D349 corrects repeated startup validation; the original two-core static peer-loss/restart race test now passes with all deadlines unchanged. Earlier current/parent node-b failures remain preserved; final hosted gates stay open. Enrolled primary/follower authority is separate F60b/G11 work |
| F66 | Separate querylog.db / local.db path keys and volumes | Observability and release | Integrated b3f6986/5048c61; actual restart, chart/Compose and volume ownership checks pass |
| F67 | E2E-2 static SQLite cluster, peer list bodies, correct leader | Store and HA | Actual two-runServe static membership/status/body/refusal/local-control/peer-loss/restart race passes. Separate server-process acceptance also passes with actual DNS, authenticated body reads, local-only control and durable identity/session restart; D349 corrects the reproduced node-b startup validation failure; the original two-core race acceptance now passes, with earlier failures preserved. Enrolled primary/follower promotion, partition/rejoin and final G11/G22/G24 remain open |
| F68a | AdGuard Home black-box e2e | Zones and AD | Integrated; final gates open |
| F68b | Knot catalog consumer interoperability | Zones and AD | Integrated 6aa58ed; actual pinned Knot 3.6.2 catalog/NOTIFY/TSIG acceptance and repeated races pass |
| F68c | Two-VM keepalived failover acceptance | Zones and AD | Container alternative passes 12/12; independent-VM acceptance explicitly deferred by owner to follow-up (D358), unqualified and outside this completion pass |
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
| G24 | Applicable release checklist, Compose/Kubernetes, licensing and artifact checks; independent two-VM VIP acceptance is owner-deferred follow-up (D358), not passed; no release publication implied. |
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

Promotion authorization now retains an original pinned proof through the store's
final commit check, including idempotent success (D322). Focused race21 and full affected race1,192 pass with no failures or skips;
configured scoped lint passes with zero issues. This closes an admission prerequisite,
not enrolled HA orchestration, forwarding or the operator route. The exact-list
performance failure remains open: a diagnostic original timed-suite profile
reproduces 169 ns p50; no matcher or gate edits or replacement samples were made.

D323 supplies independent persistent member TLS identity with exact original
member CA trust and standard mutual TLS verification. Expanded race21 passes
without failures/skips; configured scoped lint passes with zero issues. Durable membership, one-time enrollment, root
mounting and final revocation/output barriers remain open; this prerequisite
is not full F60b/G11 completion.

D324 adds an explicit replication client bound to original independent member
trust before HTTP, with no cross-enrollment connection reuse or mutation replay.
Focused race4 and composed transport/follower/identity race112 pass without
failures/skips; configured scoped lint passes with zero issues. Durable membership and production enrollment/root mounts remain open.

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

The selected Ubicloud benchmark runner is now executing the exact candidate
9cc2ae04 in hosted run 42514599389; recording success is not a release gate.
Fresh security run 42514564273 exposed stale standalone QUIC module pins and
reviewed upstream fixture warnings. Patched pins and narrowly documented scan
findings pass local configured Semgrep/Trivy; exact evidence is in gaps.md.
Fresh CI's BIND denial-zone readiness failure is reproduced locally and remains
under investigation. Historical green runs do not qualify this new candidate.

The November 8 hosted settings/audit failures are corrected without migration or
host-scope relaxations. Full affected races pass 1,037 entries; the sole asset
skip passes separately on the actual Kubernetes control plane. The first failed
run and diagnosed flush-test deadlock remain recorded in gaps.md. The BIND
readiness correction also passes the full local comparator suite; fresh hosted
CI/security and remaining implementation/release qualification are still open.

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

Provider acquisition now preserves notifications across reconciliation and waits
for actual changes after success (D343). All 73 coordinator races and scoped lint
pass; unchanged static lifecycle passes at four/eight scheduler threads, while
the two-thread failure remains recorded. Hosted 5afee4cb CI fails first-image
follower/pending-TSIG startup and one signed HTTP/3 exchange; these are current
correction work, and no final-source gate is marked complete.

The exact first-image follower listener regression is corrected (D344): all 21
composed storage/root race entries and scoped lint pass without changed original
fixtures or deadlines. The earlier HTTP/3 refusal did not reproduce; hosted
final-source qualification and the other open items remain required.


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
