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
| sqlite: remaining §11.4 audit fields (incl. `auth.DecisionFrom(ctx).Path`) via migration 2; secret redaction in before/after; `cluster_epoch`/`primary_id` setters for v0.2 fencing; `InternalKinds` once internal kinds exist (the daily `PruneAudit` leader loop and `WithActor` from the API principal landed in dev-docs/milestones/m5/burndown-server.md) | store-sqlite report | v0.2 |
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
| Multi-socket listeners for DoT (same code path, left at one socket) and DoQ/DoH3 (needs QUIC connection-ID aware routing) | hardening report | v0.2 (moved from M3: one socket limits throughput only) |
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
| Audit: the sqlite recorder folds authz path, request id and client address into `reason` until audit migration 2 adds columns | api report | v0.2 (moved from M3: the reason field carries the values until migration 2) |
## Edge and VIP

| Item | Source | Target |
|---|---|---|
| Edge resolver-cache flush for remote edges. Local edges (sidecar, compose) are flushed by `dnsdist.CacheFlusher` wired into serve (blocking toggle changes and every compiled list snapshot). Remote edges (edge Deployment/DaemonSet) need a pull path, e.g. a blocking generation in `internalapi.ZoneList` that the supervisor's zone poll turns into a resolver flush | dev-docs/milestones/m3/edge.md, burndown-edge-release | v0.2 |
| `edge.*`/`vip.*` settings keys (hostOnly) and serve wiring of `vip.NewKeepalived` (Events → audit `cluster.vip.changed`, event `vip.master_changed`) | dev-docs/milestones/m3/edge.md | v0.2 (the Nodes/Edge page now reads live supervisor status; audit events and settings keys remain) |
| `vip.ServiceProvider` (kube-vip/MetalLB): Owner from the Service status / announcing node; chart annotations from `Annotations()` | dev-docs/milestones/m3/edge.md | v0.2 (moved from M4: MetalLB and kube-vip announce without it; only Owner reporting waits) |
| `--hmac-key-file` per VRID in the chart (one shared key file today; compose uses the single-file form too) | dev-docs/milestones/m3/edge.md | v0.2 |
| v0.2 console diff-apply (`newServer`/`setDown`/`rmServer`) and `reloadAllCertificates()` instead of restarts, gated on spike (a) (D17) | §3.6, dev-docs/milestones/m3/edge.md | v0.2 |

## Auth

| Item | Source | Target |
|---|---|---|
| Binding revocation times (`rbac.BindingSource`, which stops honoring a sealed session grant once a binding change removed it) live in memory per replica; after a restart only the "a live binding still provides it" rule applies. Persist a revocation epoch (state.db / the runtime ConfigMap) or bump a cluster-wide grants-valid-after on narrowing | dev-docs/milestones/m3/auth.md | v0.2 |
| OIDC identities have no stored record: no per-user "log out everywhere" or disable, and an OIDC user's personal tokens stay valid until expiry while bindings still provide their frozen grants. Needs the v0.2 user store (with `autoProvision: never`, `syncGroups`) | dev-docs/milestones/m3/auth.md | v0.2 |
| Login lockouts (5/IP → 5 min, 10/user → 15 min) are in memory per replica, so N replicas allow N× the attempts; share counters through the store if that matters | dev-docs/milestones/m3/auth.md | v0.2 |
| Projected ServiceAccount credentials now require an original lifetime of at most ten minutes on JWKS and TokenReview paths, with expiry retained through authorization and cache bounds (F47 closed). Offline deletion is not immediately observable; explicit 600-second audience kudurru projection is required | dev-docs/milestones/m3/auth.md | v0.2 |
| `autoProvision: never` and `syncGroups` once the user store exists; RP-initiated logout | auth spike | v0.2 |

## Security (dev-docs/milestones/m5/security.md)

| Item | Source | Target |
|---|---|---|
| A `ServiceAccount` binding subject matches a kudurru service account and a Kubernetes ServiceAccount of the same namespace/name; consider a subject field (`provider: kubernetes|kudurru`) or documenting the overlap in §8.5 | dev-docs/milestones/m5/security.md | v0.2 |

## Security (dev-docs/milestones/m5/security-v0.1-round.md)

| Item | Source | Target |
|---|---|---|
| Repository settings for the release pipeline (cannot live in the repository): a branch ruleset refusing `v*` branch names, required review with CODEOWNERS on `release` and `release-candidate` (`.github/CODEOWNERS` for `/.github/` and `hack/release-tag.sh`), deployment branches for a `release` environment | dev-docs/milestones/m5/security-v0.1-round.md | release operations (verify for each release) |
| Optional `httpsOnly` for `blocking.catalogue.adguardServicesURL` (and BlockList URLs): a plain-http document can be changed in transit | dev-docs/milestones/m5/security-v0.1-round.md | v0.2 |

## Dashboard (dashboard/)

| Item | Source | Target |
|---|---|---|
| "Is it allowed?" makes a second request to find list exceptions; the dig answer could report a matched exclusion itself | allowlist stream (D144) | v0.2 |
| Allowlist/Denylist: a group list applies through the group's policies, so a policy covering several groups applies it to all of them; a query-time group scope in the policy engine would make the scope exact | allowlist stream (D143) | v0.2 |
| Regex allow/deny entries get no "overrides block list" information and no response-cache flush (there is no single name to look up or flush) | allowlist stream | v0.2 |
| Entries browser re-parses the whole cached body for every page of a URL source; add a per-digest index if multi-million-line lists are slow (concurrent parses are capped at 4 per node since dev-docs/milestones/m5/security-v0.1-round.md V15) | allowlist stream | v0.2 |
| Self-hosted Inter/JetBrains Mono shipped in the Kudurru theme (F56), with pinned OFL packages, variable normal/italic Latin faces, local prefix-safe assets and system fallbacks | UX round 3 (D140) | v0.2 |
| Record delete confirmation as an Astryx AlertDialog; `GET /queries/export` link in the query log | ui report | v0.2 (moved from M4: the API has no `GET /queries/export` route yet) |
## CLI

| Item | Source | Target |
|---|---|---|
| `--context` selecting a kubeconfig so CRD verbs go straight to the apiserver (§3.5); a context is always a kudurru API endpoint today | dev-docs/milestones/m3/cli.md | v0.2 (moved from M4: a context is a kudurru API endpoint in v0.1) |
| Remaining §3.5 verbs: `cluster status\|promote\|join-token`, `integrations …`, zone option writes (`PUT/PATCH /zones/{zone}`) (`lameduck` landed in dev-docs/milestones/m5/burndown-server.md, `health --vrrp` in e8b36b4; live Pi-hole and Technitium API migrations in D169; offline AdGuard/Blocky subsets in `32f9e81`/`f01d4db`) | dev-docs/milestones/m3/cli.md | v0.2 (merged rows) |
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
| Separate path keys for `querylog.db` and `local.db` so `persistence.querylog` and `persistence.local` can be distinct volumes; today either switches `/var/lib/kudurru` to one per-pod claim | deploy report | v0.2 |

## Needs the project owner's hardware

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
| Central dictionaries are never pruned (the nodes prune theirs by last_seen); prune entries no rollup or fact row references | D160 | v0.2 |
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
