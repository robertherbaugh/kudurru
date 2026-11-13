# Follow-ups

Open items that are not on the milestone exit criteria they came from. Each
names its source (spike or milestone report) and the milestone where it should
land. Close an item by deleting its row and citing the commit. Items that need
the project owner's hardware (two VMs, a real dnsdist driven with kdig) are
collected in the "Needs the project owner's hardware" section and in
dev-docs/release-checklist.md; Docker and kind run on GitHub's runners.

For v0.2, use the [item-by-item evidence audit](milestones/v0.2/followup-audit.md)
and [implementation ledger](milestones/v0.2/gaps.md) alongside this original
requirements list. They split bundled rows, identify already implemented work,
and record subsequent closures with validation; older descriptions here are
not a current feature-completeness claim. Requirements and release gates remain
in scope unless explicitly retargeted by the owner.

Closed rows from this pass: remote node health (`49f9e8b`), central Grafana
panels (`60a3af1`), nested settings help (`d37f4ad`), audit verification API/CLI
(`3df864c`, `a097836`), and optional webhook authentication (`5fc97d7`).
The evidence audit preserves their original requirements and limits.

## Store and drivers

| Item | Source | Target |
|---|---|---|
| Cold-start cluster identity now requires an independently provisioned host pin for offline admission, with guarded initial online verification; F01 closure and compatibility limits are in the v0.2 audit | hardening report | v0.2 |
| SQLite attribution/redaction shipped in aa3784c (F02a/b/F38): schema 6 authenticates dedicated request/client/authz fields and redacts new documents before cap/hash. Historical source evidence stays unchanged. Epoch fencing and private-kind registration are implemented separately; final gates remain in the audit | store-sqlite report; D249 | v0.2 |
| `WatchOptions.ResourceVersion` semantics: replay-as-Added (documented) vs true resume | store-kube spike | v0.2 |
| conformance: shared "rejects the same inputs" table run against sqlite and kube | validator spike | v0.2 (moved from M2: test-only, both drivers already pass the conformance suite) |

## Zone, controller, objects

| Item | Source | Target |
|---|---|---|
| Per-record parsed RR reuse is implemented in `eb20ac7`, with bounded retention, detached copies and identity/TTL/expiry invalidation; composed races and local changed-record benchmarks pass | dev-docs/milestones/m2/xfr.md; F05 | v0.2 (implementation complete; final performance/release qualification remains) |
| Per-zone NOTIFY signing key and source shipped in the v0.2 review (F06): separate `notifyTSIGKeyRef`, `notifySource`, compatible empty defaults, current-policy/key refusal and primary-zone wizard/settings controls | dev-docs/milestones/m2/xfr.md | v0.2 |
| Store-backed internal `TSIGKey` kind behind `dnsserver.TSIGKeyring` (encrypted at rest, §4.1); TSIG on DoH/DoQ/DoH3 (signed requests are treated as unsigned there) | dev-docs/milestones/m2/xfr.md | v0.2 (moved from M3 / v0.2: the node-config `tsigKeys` keyring covers v0.1) |
| `kudurru doctor` self-NOTIFY round trip reporting the egress address (§4.6) | dev-docs/milestones/m2/xfr.md | v0.2 (moved from M4: a diagnostic, NOTIFY itself works) |
| DS at a delegation point where parent and child are both local routes to the child; needs a DS special case | zone report | v1.0 (DNSSEC) |
| Per-integration Leases: one Runner per elector name or an elector per runnable | controller report | v0.2 |
| objects: reload inline objects without a restart; bootstrap `Policy/default` priority as an explicit fallback (the starter blocklist is seeded through `bootstrap.Options.StarterBlockList`, cmd/kudurru/serve.go:198; `cluster.role` moved to v0.2 with the static membership in fdd6dc5) | objects report | v0.2 (moved from M2/M3: a restart applies inline changes; merged rows) |

## Active Directory DNS replacement (D222)

The [functional outline](active-directory-dns.md) is the scope and acceptance
matrix. These items are required for complete replacement in a single-domain
forest; a read-only import or permanent DC forwarder does not satisfy it.

| Item | Source | Target |
|---|---|---|
| Prove server-side Kerberos/GSS-TSIG and TKEY against real Windows DC, member, and DHCP registrations; SPN/key provisioning, rotation, edge/VIP routing, dependency licensing, and cgo-free build | D222, outline §§5, 10 | v0.2 feasibility gate |
| Design atomic RFC 2136 commits, principal/record ownership and AD permission translation, imported-record adoption, durable storage and replication of content/security/aging state, and fenced failover for each supported deployment mode | D222, outline §§4, 5, 7 | v0.2 |
| Discovery and import with metadata/blocker reports, shadow comparisons, final write handoff, source reconciliation, and rollback that retains post-cutover updates | D222, outline §§3, 4, 8 | v0.2 |
| Windows DHCP interoperability including enabled name protection, aging/scavenging, administration, diagnostics, and backup/restore | D222, outline §§6, 7 | v0.2 |
| Windows acceptance matrix with all DC DNS services stopped: domain/DC/member lifecycle, AD health, secure registrations, DHCP, permissions, aging, scale, outage/failover, and rollback; publish tested OS versions and deployment guarantees | D222, outline §9 | v0.2 release gate for claiming AD DNS replacement |

## Dataplane

| Item | Source | Target |
|---|---|---|
| DHCPv4 deployment acceptance with the target router/relay hardware and systemd/container packaging ([procedure](../docs/deploy/dhcp-cutover.md)). Disconnected stock-dhcpcd lifecycle, relay and multi-subnet tests pass; production LAN acceptance remains operator-specific | D171–D179 | v0.1.0 deployment acceptance |
| DHCPv6 protocol coverage and coordinated IPv6 ownership | D176; explicitly excluded by project owner from this DHCPv4 expansion | beyond this DHCPv4 pass |
| Query-log stats reads over the current hour scan its facts once it has more than 100,000 distinct names (1.8 s at 1M rows at 2,000 q/s): per-domain rollups for shorter buckets, or an HLL sketch per hour for unique domains (dev-docs/milestones/m2/querylog.md) | dev-docs/milestones/m5/measurements-v0.1.md | v0.2 |
| Query-log summary and client counts can omit newly written previous-hour events when a one-hour window ends in the first minute of an hour; add deterministic boundary coverage and correct rollup initialization or reads | PR #75 local validation; reproduced on `origin/main` with `TestClusterStatsMerge` and a window ending at HH:00:30 | Next query-log correctness pass |
| Blocking: `$important`/`$badfilter` (considered for v0.1 in the policy package and left to the v0.2 modifier slow path, §7.3; v0.1 counts them unsupported) | blocking spike, policy report | v0.2 |
| Control configuration shipped in the v0.2 review (F18a): `control.allowedNetworks` and `control.blockedQTypes`, immutable captured rules, validated hot reload and DNS64 dependency admission. Additional hostname sources remain open (F18b/c): reverse PTR cache and UniFi; DHCP lease names are implemented | policy report; docs/deploy/dns-controls.md | v0.2 |
| Blocking toggle replication: a `blocking.Publisher` writing `kudurru.io/blocking-disabled-until` on the `kudurru-runtime` ConfigMap (kube) or state.db (sqlite), and a watcher calling `Toggle.Apply` (`POST /dns/blocking` toggles the node-local Toggle since dev-docs/milestones/m3/api.md) | policy report | v0.2 (moved from M3: the toggle stays per node in v0.1; the release notes list it) |
| Blocking: lazy-DFA regex engine for regex-heavy users (pinned official AdGuard black-box CI landed in `f5d5316`; benchgate moved to hack/ in `a2bb3bf`) | blocking spike | v0.2 (remaining engine work; corpus/scale qualification stays in the release gate) |
| Upstream: `upstreams.importFile` for dnsmasq import; cache inspect over REST (`Set.Status()` is in GET /stats/upstreams and flush in POST /cache/flush since dev-docs/milestones/m3/api.md); cache and strategy benchmarks in `bench/limits.txt` | cache-upstream report | v0.2 (moved from M3: additions, nothing in v0.1 depends on them) |
| dnsserver: negotiated encrypted RFC 8467 response padding shipped in the v0.2 review (F22c). Native DoH `Alt-Svc` for an active matching HTTP/3 endpoint shipped in the v0.2 review (F22b). Config keys for `Limits.QUICMaxConns`, `MaxTCPQueries` and idle timeouts shipped in the v0.2 review (F22a); DoH path client IDs are implemented (F22d) | dnsserver spike | v0.2 (moved from M2/M3: the limits have code defaults) |
| Transfer chunks are budgeted by uncompressed size (69 messages of ~42 KB for 100k records); budget by compressed size for fewer, fuller messages | dev-docs/milestones/m2/xfr.md | later |
| D60 blocked-services and safe-search catalogue in a separate public data repository: v0.1 embeds a kudurru-authored catalogue (`internal/listdata/catalogue`) that `blocking.catalogue.url` can replace at runtime (D120); move the data to its own repository once the project owner creates it | D120 | v0.2 (the owner creates and publishes the repository) |
| ratelimit: exemptions per ClientGroup (slip counter landed in `661174c`; `ratelimit.*` reload without a restart landed in dev-docs/milestones/m5/burndown-server.md) | hardening report | v0.2 |
| Multi-socket DoT implemented; DoQ/DoH3 now use immutable CID-routed Linux groups (D311), with a pinned quic-go migration fix. Actual transport/migration races pass; local DoQ improves and HTTP3 regresses in one sample. Fixed-class performance, configured lint and final release checks remain. | hardening report | v0.2 |
| Settings hot reload caveats: exclusion-only reloads preserve daily pseudonym identity (F26a closed); privacy mode/method changes retain separate lifecycles; `cache.maxEntries` growth keeps the shard count chosen at start (very large growth means large shards); `blocking.refreshInterval`, `blocking.allowDegraded`, `querylog.enabled/sink`, `auth.*` still need a restart (restartRequired) | dev-docs/milestones/m5/burndown-server.md | v0.2 |

## Recursive resolver (D170)

| Item | Source | Target |
|---|---|---|
| Aggressive use of DNSSEC-validated NSEC/NSEC3 ranges to answer NXDOMAIN and NODATA from the resolver cache (RFC 8198) | D170 | v0.2 |
| Automatic root trust-anchor updates (RFC 5011): new anchors come with releases or `upstreams.recursion.trustAnchors` | D170 | v0.2 |
| Negative trust anchors: a list of zones whose broken DNSSEC is ignored, so an operator can work around a provider's signing outage without turning validation off | D170 | v0.2 |
| DNSSEC validation of forwarded answers (the `dnssec` stage, §7.1 step 10); only recursive groups validate | D170 | v0.2 |
| DNS cookies (RFC 7873, RFC 9018) to authoritative servers implemented in `668203c`: bounded peer state, rotation, exact echo checks and budgeted BADCOOKIE/TCP recovery; composed socket/DNSSEC/cache races pass | D170; F31 | v0.2 (implementation complete; final interoperability/release qualification remains) |
| A local copy of the root zone (RFC 8806) for recursive groups | D170 | later |

## API (dev-docs/milestones/m3/api.md)

| Item | Source | Target |
|---|---|---|
| `PATCH /node/config` changes stay in memory (log.level applies at once; nothing persists them, the node file is the operator's): persist node-scope API changes somewhere (a `node.d/` overlay?) or drop the route's write half | dev-docs/milestones/m5/burndown-server.md | v0.2 |
| `GET /auth/me` permissions are evaluated at cluster scope; zone-scoped grants show only in `roles` | api report | v0.2 |
| external-dns: read `DNSEndpoint` CRs directly (§5.6) and a `kudurru externaldns-webhook` sidecar subcommand (optional webhook authentication landed in `5fc97d7`) | api report | v0.2 (moved from M4: the webhook provider covers external-dns in v0.1) |
| Object list routes read the store (an apiserver round trip in kube mode); serve them from the zonecache snapshot when it is live | api report | v0.2 (moved from M4: performance only) |
| CLI: switch from the hand-written `internal/apiclient` transport to the generated `internal/apiclient/openapi` client and put it under `apiclient.Client` (surface listed in dev-docs/milestones/m3/cli.md) | api report, dev-docs/milestones/m3/cli.md | v0.2 (moved from M3/M4: internal refactor, no user-visible change; merged rows) |
| Audit: schema 6 stores and authenticates dedicated authz/request/client columns; the historical reason suffix remains compatible without becoming trusted parsed metadata (F38 closed) | api report; D249 | v0.2 |
## Edge and VIP

| Item | Source | Target |
|---|---|---|
| Edge resolver-cache flush for remote edges. Local edges (sidecar, compose) are flushed by `dnsdist.CacheFlusher` wired into serve (blocking toggle changes and every compiled list snapshot). Remote edges (edge Deployment/DaemonSet) need a pull path, e.g. a blocking generation in `internalapi.ZoneList` that the supervisor's zone poll turns into a resolver flush | dev-docs/milestones/m3/edge.md, burndown-edge-release | v0.2 |
| `edge.*`/`vip.*` settings keys (hostOnly) and serve wiring of `vip.NewKeepalived` (Events → audit `cluster.vip.changed`, event `vip.master_changed`) | dev-docs/milestones/m3/edge.md | v0.2 (the Nodes/Edge page now reads live supervisor status; audit events and settings keys remain) |
| `vip.ServiceProvider` (kube-vip/MetalLB): Owner from the Service status / announcing node; chart annotations from `Annotations()` | dev-docs/milestones/m3/edge.md | v0.2 (moved from M4: MetalLB and kube-vip announce without it; only Owner reporting waits) |
| Per-VRID Helm HMAC references shipped in 376d63d (F42/D253), with private staging and exact mapping; shared defaults retained and coordinated restart required (docs/deploy/vrrp-keys.md) | dev-docs/milestones/m3/edge.md | v0.2 |
| v0.2 console diff-apply (`newServer`/`setDown`/`rmServer`) and `reloadAllCertificates()` instead of restarts, gated on spike (a) (D17) | §3.6, dev-docs/milestones/m3/edge.md | v0.2 |

## Auth

| Item | Source | Target |
|---|---|---|
| Binding revocation times (`rbac.BindingSource`, which stops honoring a sealed session grant once a binding change removed it) live in memory per replica; after a restart only the "a live binding still provides it" rule applies. Persist a revocation epoch (state.db / the runtime ConfigMap) or bump a cluster-wide grants-valid-after on narrowing | dev-docs/milestones/m3/auth.md | v0.2 |
| OIDC identities have no stored record: no per-user "log out everywhere" or disable, and an OIDC user's personal tokens stay valid until expiry while bindings still provide their frozen grants. Needs the v0.2 user store (with `autoProvision: never`, `syncGroups`) | dev-docs/milestones/m3/auth.md | v0.2 |
| Login lockouts: Kubernetes password admission shares durable counters (D304); complete SQLite login composes counters and enrollment in one command (D305). TOTP setup/confirm/disable and self-service password changes share counters in D307/D308. SQLite browser/CLI request budgets are shared before initial auth capture (D309); Kubernetes browser admission shares the same bounded document policy in a separate private Secret (D310); enrolled-replica forwarding remains. Browser postcommit publication regression is fixed in D306; its failed runs remain recorded. | dev-docs/milestones/m3/auth.md | v0.2 |
| Projected ServiceAccount credentials now require an original lifetime of at most ten minutes on JWKS and TokenReview paths, with expiry retained through authorization and cache bounds (F47 closed). Offline deletion is not immediately observable; explicit 600-second audience kudurru projection is required | dev-docs/milestones/m3/auth.md | v0.2 |
| Complete SQLite identities implement `autoProvision: never` and `syncGroups`; RP-initiated browser logout now uses original enrolled proof and trusted end-session discovery (F48b/D282). Legacy Kubernetes identity migration remains separately tracked | auth spike | v0.2 |

## Security (dev-docs/milestones/m5/security.md)

| Item | Source | Target |
|---|---|---|
| ServiceAccount provider overlap is explicitly documented in §8.5 and the Kubernetes token guide (D254/F50): exact namespace/name across both providers, provider-specific lifecycle and narrowing token scopes; provider isolation is not claimed | dev-docs/milestones/m5/security.md | v0.2 |

## Security (dev-docs/milestones/m5/security-v0.1-round.md)

| Item | Source | Target |
|---|---|---|
| Repository settings for the release pipeline (cannot live in the repository): a branch ruleset refusing `v*` branch names, required review with CODEOWNERS on `release` and `release-candidate` (`.github/CODEOWNERS` for `/.github/` and `hack/release-tag.sh`), deployment branches for a `release` environment | dev-docs/milestones/m5/security-v0.1-round.md | release operations (verify for each release) |
| Optional `httpsOnly` for `blocking.catalogue.adguardServicesURL` (and BlockList URLs): a plain-http document can be changed in transit | dev-docs/milestones/m5/security-v0.1-round.md | v0.2 |

## Dashboard (dashboard/)

| Item | Source | Target |
|---|---|---|
| "Is it allowed?" now consumes original-query disabled-rule evidence (8879459, D255/F52); no second list read or browser regex matching. Evidence is bounded and does not override the final verdict | allowlist stream (D144) | v0.2 |
| Allowlist/Denylist: a group list applies through the group's policies, so a policy covering several groups applies it to all of them; a query-time group scope in the policy engine would make the scope exact | allowlist stream (D143) | v0.2 |
| Regex allow/deny entries get no "overrides block list" information and no response-cache flush (there is no single name to look up or flush) | allowlist stream | v0.2 |
| Entries browser re-parses the whole cached body for every page of a URL source; add a per-digest index if multi-million-line lists are slow (concurrent parses are capped at 4 per node since dev-docs/milestones/m5/security-v0.1-round.md V15) | allowlist stream | v0.2 |
| Self-hosted Inter/JetBrains Mono shipped in the Kudurru theme (F56), with pinned OFL packages, variable normal/italic Latin faces, local prefix-safe assets and system fallbacks | UX round 3 (D140) | v0.2 |
| Record delete confirmation as an Astryx AlertDialog; `GET /queries/export` link in the query log | ui report | v0.2 (moved from M4: the API has no `GET /queries/export` route yet) |
## CLI

| Item | Source | Target |
|---|---|---|
| Explicit `--transport kubernetes` now selects a kubeconfig context for generic CRD verbs (F59/D285), preserving default API contexts, Kubernetes RBAC/ownership and bounded original commands; actual envtest acceptance passes | dev-docs/milestones/m3/cli.md | v0.2 (moved from M4: a context is a kudurru API endpoint in v0.1) |
| Remaining §3.5 verbs: `cluster status\|promote\|join-token`, `integrations …`, zone options now use reviewed `zones options get/set` through existing object PUT (F60d) (`lameduck` landed in dev-docs/milestones/m5/burndown-server.md, `health --vrrp` in e8b36b4; live Pi-hole and Technitium API migrations in D169; offline AdGuard/Blocky subsets in `32f9e81`/`f01d4db`) | dev-docs/milestones/m3/cli.md | v0.2 (merged rows) |
| Browser (loopback + PKCE) and device-code `kudurru login` (§8.3) | dev-docs/milestones/m3/cli.md | v0.2 |
| CLI conformance test against the real `internal/api` router instead of the fake in `cmd/kudurru/fakeapi_test.go`; document `ApplyResult.errorKey/hint`, the wrapped settings body and the SSE event name in design-record.md §10 | dev-docs/milestones/m3/cli.md | v0.2 (moved from M3: test-only) |

## Build, CI, docs

| Item | Source | Target |
|---|---|---|
| Bench job on a fixed runner class (also for `BenchmarkUDPSockets` and `BenchmarkPersist200k`) | blocking, store-kube spikes; hardening report | v0.2 (moved from M2: needs a fixed runner class, the owner's call) |

## Deployment (binary behaviour the chart and compose files need)

| Item | Source | Target |
|---|---|---|
| Metrics the §11.6 dashboard and alerts still lack: `kudurru_dnssec_key_expiry_timestamp_seconds` (v1.0). `snapshotAge`/`leader` in the `/healthz` JSON (§5.2 human probe) shipped in the v0.2 review (F64b). The serial-divergence, edge and VIP panels and alerts shipped in the v0.1 fix round; SQLite fencing metric/panel/alert shipped in the v0.2 review (F64a) | deploy report | v0.2 (moved from M3-M5: each waits for its feature; merged rows) |
| Settings key `cluster.role: static`: the compose HA read-only mode cannot be expressed until it exists (the chart renders OIDC providers through `auth.oidc.providers`); lands with the `cluster.static` membership of E2E-2 | deploy report | v0.2 (compose HA) |
| Separate querylog/local directories and additive Helm/Compose volumes shipped (F66), preserving the original data claim; explicit offline migration and mounted restart/volume checks are documented | deploy report; D250 | v0.2 |

## Needs the project owner's hardware

The owner deferred the independent two-VM keepalived test to follow-up on
November 9, 2026 (D358): no disposable VM pair exists. Keep the original test
below unqualified; container and kind/MetalLB checks retain their own evidence.

| Item | Source | Target |
|---|---|---|
| Spikes (a) dnsdist console directives under YAML, (b) PROXYv2 + chashed with a real dnsdist and `kdig +quic/+tls/+https`, (h) keepalived 2.4.3 on two VMs (lameduck keeps the VIP, `vip drain` moves it, node loss moves it). Spike (f), kind + MetalLB, runs on GitHub's runners in the kind workflow (hack/kind-e2e.sh) | week-1 spikes, dnsserver spike | deployment acceptance (dev-docs/release-checklist.md §3) |

## End to end and fuzzing (dev-docs/milestones/m5/e2e.md)

Bugs found by the M5 e2e package. E2E-1, E2E-3, E2E-4, E2E-5 and E2E-6 were fixed after M5 (default-group hints fall through to conditional forwarding; `node.lameduck` drain window, default 2s; `keys.Load` reports `Generated` only for a key it created; readiness waits for the first live snapshot; keepalived paths allow braces only as `{vrid}`, 0a25647) and their tests now assert for real. The remaining items keep a fuzz-input filter or a design note naming the id.

| Item | Source | Target |
|---|---|---|
| E2E-2: sqlite nodes cannot form a cluster. serve.go hard-codes `cluster.Self` membership for the sqlite driver, so node B cannot be pointed at node A's internal listener for list bodies, and every sqlite node reports itself as leader. The two-node test drives `/internal/status` and `/internal/lists/{digest}` with the derived credentials instead. Needs a `cluster.static` setting (peers, leader) feeding `cluster.Static` | dev-docs/milestones/m5/e2e.md | v0.2 (compose HA) |
| e2e still missing from §13.1: the 2-VM keepalived failover and the Knot catalog consumer (Docker runner). The first-party AdGuard Home black-box corpus runs against the pinned official executable in CI (`f5d5316`); full corpus/scale qualification remains. Actual outage startup and envtest recovery are covered by cmd/kudurru/serve_offline_test.go and serve_recovery_test.go; kind + Helm with MetalLB runs in hack/kind-e2e.sh | dev-docs/milestones/m5/e2e.md, dev-docs/milestones/m5/features-v0.1.md | v0.2 (Knot and final AdGuard qualification); verify 2-VM failover for each deployment (dev-docs/release-checklist.md §3) |

## Central database and cluster-wide audit (D155-D161)

| Item | Source | Target |
|---|---|---|
| Read a retired node's data with `scope=node&node=<other>` from the central database (each reader query needs a node condition); live peers are routed over the internal listener in v0.1 | D161 | v0.2 |
| Reference-safe bounded central dictionary pruning shipped (F70), with schema-4 constraints, cached-ID locking and durable sweep progress; actual PostgreSQL concurrency/restart tests pass | D160; D248 | v0.2 |
| Statistics of a node that stops for good inside an hour it has not summarised lose that part hour (its facts are kept); the leader could summarise the facts of sources gone for two hours | D160 | v0.2 |
| AuditLog CRD availability and bounded namespace batch count diagnostics against the 50,000 recommendation now ship on Diagnosis and in `kudurru doctor` (F72a/b); incomplete scans remain explicit and late CRD availability needs restart to activate shipping | D157 | v0.2 |
| Decouple AuditLog verification keys from session-key rotation or retain a verification key ring; today current/previous keys must cover all retained sealed batches before another rotation | docs/deploy/runbooks.md, PR #55 | v0.2 |

## License portal (license-server/)

| Item | Source | Target |
|---|---|---|
| Step-up re-authentication (a sign-in within minutes, `max_age`) for granting roles and other superadmin actions | design critique, security | after go-live |
| Signing-key lifecycle in the portal: several keys, "trusted since" from release tags, a compromised flag and bulk re-issue under a new kid | design critique, completeness | when a second kid exists |
| A signer outside the portal's process (Vault transit or a separate container) behind the `Signer` interface | design critique, security | when the portal grows beyond one operator |
| Expiry reminders by mail (45, 30 and 7 days) and mail to the requester when a request is decided in bulk | product research | after go-live |
| RP-initiated logout, several identity providers, a metrics endpoint, audit export | design spec | later |
| Dashboard `LicensePage` dates in UTC, and the "no restart" wording in LicensePage, the website, licensing.md and helm.md: the flag and `KUDURRU_LICENSE_KEY` are read once at startup | design critique, product | next licensing round |
| `license.Manager` logs "license key replaced" when a key with the same jti but other claims or expiry arrives (today it logs only state, id or source changes) | design critique, compatibility | next licensing round |

## v0.2 completion pass: open at 68a709e2

Open findings of the packages the owner had merged on November 11, 2026
before their verification was complete. Each section of the
[completion plan](milestones/v0.2/completion-plan.md) ends with the package's
state; this table lists what is still to do. Close a row by deleting it and
citing the commit.

| Item | Source | Target |
|---|---|---|
| SQLite upgrade: the `503` an API write can get just after readiness is handled only by the test; `TestRunServeSQLiteUpgradeWithLegacyPlaceholders` failed in the composed run; a merged CNAME's TTL now changes on upgrade (stated in the changelog's upgrade notes, but no test covers it for CNAME) | QUAL-SQLITE-UPGRADE-2 verify | v0.2 |
| Review nits with no recorded state (the fixer neither addressed nor rebutted them): DOCS-ARCH, three (the `cliClientID` CRD field comment still says "`kudurru login` (v0.2)"; the corrected group tooltip and host-name help have no asserting test; `cmd/kudurru/doctor_notify_kube.go` still calls the credential source an immutable startup inventory); QUAL-KUBE-UPGRADE, two (`TestEnvtestCRDUpgradeFromV01` creates no legacy IAM Secret, so that path is covered at process level only; its ledger named a base commit that its first three commits precede); INT-DEL-C, one (`docs/deploy/integrations.md` "When the license expires or is suspended" mixes `LicenseUnavailable` with the Integration's own `Suspended`) | completion-pass journals | v0.2 |
| Fix commits never verified: DOCS-ARCH | completion-pass journals | v0.2 |
| Observed by the docs lane on November 10 and not rechecked at 68a709e2: on SQLite a signed update is answered `SERVFAIL` instead of `REFUSED` when the zone or the key's grants do not allow it; `nsupdate` reports a wrong TSIG key as "clocks are unsynchronized" (zero signing time in the error answer); `cluster status`, `nodes` and the license node count describe only the answering node; an enrolled cluster issues at most 64 invitations in its lifetime; a Kubernetes backup and restore recreates `rfc2136-<zone>` as API-owned (from the code) | DOCS-OPS | v0.2 |
| No command removes an enrolled member; no audited operator revocation on the primary for a root-key revocation it can never record | F28-HISTORY-RECHECK | owner decision |
| An unpinned Kubernetes node exits during an API-server outage instead of staying up and unready (G22-WP4 item 2); the fencing loop in `kube_cluster_test.go` counts a failed `/healthz` as not leader | QUAL-KUBE-UPGRADE (D373) | v0.2 |
| Data race between `TestServingAdoptionActualHTTPFinalExpiryRollsBackAllZones`'s `opts.Now` replacement and the integration host's refresh | HA-FIX | v0.2 |
| Paired benchmark and fixed-class run on the final head; PR body | G23, G25 | v0.2 |

## v0.2 completion pass: open at 17b7c87a

Open findings of the packages merged after 68a709e2, up to the qual lane head
17b7c87a (the PR branch at 85913663 plus RPZ-OPEN). The
[completion plan](milestones/v0.2/completion-plan.md) has each package's state.
Rows of the section above that this pass closed were deleted: the hosted
failures of 68a709e2 (CI-RED, `80747d40`, `2074cc8c`, `c12125ac`), the free-port
race and the adapter/auth-cookie failures (FLAKE-PORT-RACE, FLAKE-PRIMARY-UNAVAILABLE),
the long-apply gap (CORE-APPLY-OPEN, CORE-OPEN3; residual below), the over-broad
D377 (CORE-RETAINED-SERVFAIL), the status-write SERVFAIL (CORE-STATUS-WRITE-SERVFAIL),
the missing live follower-behind-edges test (F28-EF1-LIVE, `de627369`), the
cleanup partial-receipt regression and INT-DEL-B's follow-ups (INT-OPEN, `055353fb`),
the dnstap and OBS-FANOUT gaps (OBS-OPEN) and the overtaken docs (DOCS-RECONCILE);
the never-verified fixes of HA-REFUSAL and OBS-HTTP-SINKS were verified by HA-OPEN
and OBS-OPEN, and a refusing member whose primary is lost can abandon the lost
term (F28-HISTORY-RECHECK, D369).

| Item | Source | Target |
|---|---|---|
| Hosted CI is red on every pushed head after 68a709e2 (ee76c04b to 85913663). Most failures are sign-in or `POST /cluster/activate` answering `503 unavailable` right after a node reports ready: a command held to the publication it started on is refused when a background publication (a zone status write) lands first (D313). Seen in `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled`, `TestRunServeDirectAnchorsFencedAndConflictRestart`, `TestStaticClusterProcessesReadOnlyPeerLossRestart`, `TestEnrolledClusterProcessesPromoteAndRejoin`, `TestAnchorUpgradeStaticTwinV010` and `TestV01BinaryRefusesEnrolledDirectory`. Fix is the integrator's D399 (CMD-RECAPTURE), not merged at 17b7c87a; CI-RED-3's quiet wait in `enrolled_conflict_test.go` is a stopgap to remove after it | hosted CI; CI-RED-2, CI-RED-3 verify | v0.2 |
| Hosted e2e `TestEnrolledManagedAnchorsPromoteAndRejoin` fails with "rejoin acquisitions 5" on f6afe7d6, b9b66cba, b6e09f9d, 3a6ab137 and 85913663 (the integrator's hand merge of the F28 and HA test flows); test-only data race in `TestAnchorEdgeActivationJoinedFollower` (`joinPrimaryOn` writes `p.opts.Config.Node.DataDir` at `cluster_join_test.go:51` on a running runtime) seen on 85913663 | hosted CI; F28-REJOIN-COUNT | v0.2 |
| Readiness under CPU starvation: the readiness budget of `startStaticMount` has no run that turns a failing condition into a pass (under the reproducing condition the test fails at its own 5 s HTTP timeout), the multi-P credit is not exercised, and the other mounted tests keep fixed 20 s readiness loops | CI-RED verify; FLAKE-CLOCK-AND-READINESS verify | v0.2 |
| A UDP answer resolved from a serving generation that a republication replaces before the write is dropped silently (`serving: dependency changed`, `internal/dnsserver/native_capture.go`), so any settings or data commit can drop UDP answers in flight; resolve once more against the current generation or answer SERVFAIL, then tighten the refused-fenced check of `test/e2e/enrolled_cluster_test.go` | HA-OPEN verify | v0.2 |
| Back-to-back RFC 2136 UPDATEs through an enrolled follower: after the first SERVFAIL every later update of the burst fails (5 or 6 of 8 in the verifier's run, not applied); two of eight enrolled e2e tests failed locally at 71ac05f0 on this; the stale comment on `enrolledFollowerBurst` names the wrong cause | HA-UPDATE-ROUTING verify; F28-PROCESS merge | v0.2 |
| Through a follower, 84 of 200 new documents of one apply were refused with "the follower's source changed" or "cluster authority changed", four repeats created none, and repeats reported fewer documents unchanged than had been created | DOCS-RECONCILE implementer | v0.2 |
| Long multi-item request on SQLite: an unexplained 1.39 s DNS gap in about one run in a hundred without race (and one refused read in 1 of 15 runs on one CPU); under race on a loaded machine most of a second caller's reads are refused with `generation_pending`; the non-race run is a CI gate with thin margin; `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` fails about half the time without race on the loaded lab machine on its read assertion | CORE-APPLY-OPEN and CORE-OPEN3 verify; integrator | v0.2 |
| Status-write SERVFAIL fix: the silent-probe bound of `serve_status_write_test.go` has less headroom than its comment states (10 of 24 seen); the fixer's mounted and lint evidence is incomplete (`fix-03-mounted.log`, `fix-04-lint.log`) | CORE-STATUS-WRITE-SERVFAIL verify | v0.2 |
| Managed root anchors: F28-HA-4 is proven only by `TestSoakEnrolledAnchorsOfflineRecovery`, which skips without `KUDURRU_ANCHOR_SOAK=1` and passed once; refused-adoption and fenced status have no real-path or scraped-metric test; never-SERVFAIL tolerates SERVFAIL before a role's first validated answer; one undiagnosed run had a healed conflicted member answer SERVFAIL for its own zone (`dns_only`); a just-promoted or restarted managed primary answers writer commands 503 until its updater's first step; the capture-phase `ErrCAS` retry and the abandoned-term return have no test; F28-OPEN6 was backed out because `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` then failed every run | F28-PROCESS, F28-EF1-LIVE, F28-HISTORY-RECHECK verify; integrator | v0.2 |
| Tier-B hooks and webhook records: the mounted 24 h expiry and budget-exhaustion cases, a mixed-version test for the IntegrationEventCursor kind and a negative run of the A1 audit rows are missing; with no Integration lease running nothing prunes; a hookless owner whose lease restarts more often than every 15 minutes never runs retention; deferred topics `query.blocked`, `vip.master_changed`, `client.seen`, `client.left`; no per-namespace event filter | F60C-HOOKS, F60C-OPEN | v0.2 (topics later) |
| Kubernetes integration host: K3 and K7 stay partial (two-replica budget sharing with hand-placed Leases only, credential rotation proven by order only, no holder-change, partition or mid-cleanup lease-loss qualification, expired license and re-created Integration proven at the adapter only); UniFi record publication and Tier-B webhooks are refused on Kubernetes; native Secret credential references are disabled; definite head-write rejections (403, 422) are reported as outcome unknown; the CLI `get` is not run on Kubernetes | F09B-KUBE-ROOT, F09B-KUBE-SOURCES, F09B-OPEN | v0.2 |
| Zone groups: a zone deleted between `authorizeWrite`'s read and `writeObject`'s read is recreated by a group member's looping PUT without a cluster-scope check (code trace); a zone-group read error makes lists omit group zones with HTTP 200; the mounted tests retry the SQLite `503 unavailable` at reads, PUTs and sign-in. Impersonation: `impersonateVerbDenied` matches any 403 containing " cannot impersonate" | IAM-ZONEGROUP and IAM-IMPERSONATION verify (CORE-OPEN4) | v0.2 |
| Edge blocks: the five-minute restore hold measures wall time since the first lower sighting, so two pulls five minutes apart accept a stale revision at once; after a deliberate restore new blocks are delayed up to five minutes (not in the runbook); an owner whose only backend misses a pull has its blocks cleared and reinstalled | EDGE-DYNBLOCK verify | v0.2 |
| Known issue: the pinned dnsdist 2.1.3 edge never answers a DoQ or DoH3 request whose QUIC FIN arrives in its own empty frame. The live client-ID test's canary fails once an image bump fixes it (then drop the retry and the known-issue text); the canary cannot tell the defect from another failure; `h3Post` leaks a goroutine on a dial failure; one DoQ loss was attributed to the defect by inference only | EDGE-CLIENT-ID | dnsdist upstream; after v0.2.0 |
| RPZ: the SQLite compile window after a restart was never measured; a signed feed restarted without the apiserver fails closed instead of reusing its persisted copy (D390); the fake API data and a golden file keep the old pending message; on Kubernetes the dig explanation and policy reads report no RPZ fields; with blocks on two ports answering one name, the CoreDNS import report still says the dropped block's name is imported and gets that block's answer on every listener, and the port-drop line counts omitted answers rather than names (RPZ-OPEN verify); a pending list of any kind, not only an RPZ one, makes a decision filter (RPZ-OPEN review nit) | RPZ-KUBE, RPZ-DIAG, RPZ-OPEN | v0.2 |
| Dashboard: every non-admin session polls `GET /cluster` every 30 s and gets an audited 403; no mounted follower test for QueryLogPage, ClientDetailPage, EntriesBrowser, the migration pages, ExternalUsers and RecoverAuthenticatorButton; no clean full vitest run at the follower fix head; authenticator and local-user controls are not hidden from the `/auth/status` capabilities on Kubernetes; no views for RPZ feeds, query-log sinks, edge blocks or zone-group scopes at this head | DASH-FOLLOWER verify; IAM-IMPERSONATION, RPZ-DIAG, OBS-OPEN | v0.2 |
| Settings after the first 0.2 start: `PreparedImage.CommittedDynamicOwnership` rebuilds the zone's dynamic index on every publication; `docs/deploy/runbooks.md` (near line 600) and `docs/deploy/compose-ha.md` (near line 108) say a later file edit of any cluster key is not applied, which is inaccurate for restart-required keys; static twins cannot change a cluster setting after their first 0.2 start; `TestAnchorUpgradeStaticTwinV010/LaterFileEditNotApplied` does not assert the seeded value | QUAL-OPEN2 and QUAL-MIXED-VERSION verify | v0.2 |
| Import step dispatch: the re-capture before a step's body (`57cef0e6`) refines the dispatch-once rule without a decision row (D399 generalizes it; not merged); a later import step is no longer bounded by 5 s | FLAKE-CLOCK-AND-READINESS verify | v0.2 |
| Evidence gaps: G21-PERMISSIONS `fix-bench01.log` and `fix-lint02.log` are empty; FLAKE-PORT-RACE's ledger points at unfinished logs (use `verify-01-mounted-race.log`, `verify-03-lint.log`); `dev-docs/design-record.md` still mentions `settings.operatorSeesQueryLog`, which does not exist; the permission scan's allowlist matches by bare name | G21-PERMISSIONS, FLAKE-PORT-RACE verify | v0.2 |
| Low-severity notes left open: HA-SELF (200 for any token authentication failure on the new-hash view; every seal error reported as `committed_output_unavailable`; no mounted follower audit test); FLAKE-PRIMARY-UNAVAILABLE (`RunnableStatus.LastError` drops an error at term end); OBS-TRACING (about 5 KB per traced query); CORE-RETAINED-SERVFAIL (stale `UpstreamContacted` comment; untested non-deadline refusal after contact); F09B-OPEN (joined budget error classification untested; timing-dependent bounds); QUAL-OPEN3 wording of the measured percentages | verify results in `followups/*.json` | v0.2 |
| Hosted job for the local-only interop suites: start a private dockerd on the runner and run `hack/test-knot-catalog.sh` (Knot `cznic/knot@sha256:174b0fcb24c6c3bc5065c9d1ce46b588eec607a5f32bc40ff0cda60150f00bd9`, knotd 3.6.2; `TestKnotCatalogConsumer` and `TestKnotInboundSecondaryTransfer`, last unskipped pass at `2e4e89fc`) and `hack/test-dnsdist-live.sh` (`powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837`; all live dnsdist tests: F43 YAML/Lua, F28, F39b (both tests), anchor edge and F18c, last unskipped pass at `d2039002`, `out/QUAL-OPEN3`). Until then both are local-only evidence | QUAL-INTEROP, G23-R5; D383 | after v0.2.0 |
| Decide whether the secondary receiver should accept a signed IXFR reply that is one SOA at the client's serial without AA (Knot 3.6.2 sends it; BIND sets AA). Today such a reply is refused (`secondary: non-authoritative response`); a refresh against one consistent primary never asks for it, but it is reachable when the SOA poll and the IXFR reach different instances behind one address or the primary moves back to the stored serial between them. Effect: one refresh error, no renewed confirmation for that cycle, no data loss | QUAL-INTEROP review; `test/interop/knot_secondary_test.go` | v0.2 or later |
| Authoritative no-response window after a canonical commit. An authoritative UDP answer captured from a publication that a later commit supersedes is withheld (D313, D332), so the client gets no response until the next publication is installed. A DNSZone write also gives that zone fail-closed SERVFAIL coverage until its new lifecycle source is admitted. One zone write is several commits, and under `-race` on 2 shared CPUs the windows lasted up to about 1.3 s per commit (QUAL-OPEN3 `diag05`, `diag06`, `diag07`). Without `-race` and with `CGO_ENABLED=0` (mounted `runServe` test binary, 8 shared CPUs, `fix-measure03-release.log`): any canonical commit, even an unrelated BlockList write, left an authoritative zone unanswered for up to 586 ms; a write to the zone or its records gave SERVFAIL then no response for up to 860 ms; a forward zone gave SERVFAIL for up to 558 ms after its own write. dnsdist does not retry such a UDP query, so a client behind an edge sees a timeout, and a busy cluster that commits often loses that share of authoritative queries. Not changed for v0.2.0; shipping it as is needs the owner's explicit acceptance (D383). Next: repeat the measurement on the packaged release binary with a realistic object set; add a test that asserts an upper bound on the unanswered window after a commit (the live suites now wait it out, so no test bounds it: QUAL-OPEN3 `fix-measure-window.patch` is a starting point); keep the authoritative answer path from being invalidated by a commit that does not touch the zone. Then decide whether a withheld authoritative answer should become an empty SERVFAIL (as D332 already allows for unavailable local data), or whether the publisher should shorten the window, for example by keeping the admitted model of a zone whose only change is a query policy. | QUAL-OPEN3 (D383), live tests `TestRunServeRemoteEdgeAuthoritativeCacheACL` and `TestRunServeManagedEdgePinnedDaemonRestart` | after v0.2.0 |
