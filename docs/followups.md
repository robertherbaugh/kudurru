# Follow-ups

Open items that are not on the milestone exit criteria they came from. Each
names its source (spike or milestone report) and the milestone where it should
land. Close an item by deleting its row and citing the commit. Items that need
the project owner's hardware (two VMs, a real dnsdist driven with kdig) are
collected in the "Needs the project owner's hardware" section and in
docs/release-checklist.md; Docker and kind run on GitHub's runners.

## Store and drivers

| Item | Source | Target |
|---|---|---|
| Cold-start cluster-ID check is skipped while the kube driver has never reached the apiserver (its cluster ID is unknown then, the very outage case); compare against an independent source such as a node setting or the downward API | hardening report | v0.2 |
| sqlite: remaining §11.4 audit fields (incl. `auth.DecisionFrom(ctx).Path`) via migration 2; secret redaction in before/after; `cluster_epoch`/`primary_id` setters for v0.2 fencing; `InternalKinds` once internal kinds exist (the daily `PruneAudit` leader loop and `WithActor` from the API principal landed in docs/m5/burndown-server.md) | store-sqlite report | v0.2 |
| `WatchOptions.ResourceVersion` semantics: replay-as-Added (documented) vs true resume | store-kube spike | v0.2 |
| conformance: shared "rejects the same inputs" table run against sqlite and kube | validator spike | v0.2 (moved from M2: test-only, both drivers already pass the conformance suite) |

## Zone, controller, objects

| Item | Source | Target |
|---|---|---|
| Controller zone builds parse every record per zone reconcile (paced Apply → status p50 0.4 ms → 2-3 ms at 1,000 record sets); cache parsed endpoints per record-set object and copy RRs into each build | docs/m2/xfr.md | v0.2 (moved from M3: performance only; status writes stay in the low milliseconds at 1,000 record sets) |
| A NOTIFY key field in `ZonePolicy` (NOTIFY is signed with the first `transferTSIGKeyRefs` key today) and a `notifySource` per zone | docs/m2/xfr.md | v0.2 |
| Store-backed internal `TSIGKey` kind behind `dnsserver.TSIGKeyring` (encrypted at rest, §4.1); TSIG on DoH/DoQ/DoH3 (signed requests are treated as unsigned there) | docs/m2/xfr.md | v0.2 (moved from M3 / v0.2: the node-config `tsigKeys` keyring covers v0.1) |
| `kudurru doctor` self-NOTIFY round trip reporting the egress address (§4.6) | docs/m2/xfr.md | v0.2 (moved from M4: a diagnostic, NOTIFY itself works) |
| DS at a delegation point where parent and child are both local routes to the child; needs a DS special case | zone report | v1.0 (DNSSEC) |
| Per-integration Leases: one Runner per elector name or an elector per runnable | controller report | v0.2 |
| objects: reload inline objects without a restart; bootstrap `Policy/default` priority as an explicit fallback (the starter blocklist is seeded through `bootstrap.Options.StarterBlockList`, cmd/kudurru/serve.go:198; `cluster.role` moved to v0.2 with the static membership in fdd6dc5) | objects report | v0.2 (moved from M2/M3: a restart applies inline changes; merged rows) |

## Dataplane

| Item | Source | Target |
|---|---|---|
| Blocking: `$important`/`$badfilter` (considered for v0.1 in the policy package and left to the v0.2 modifier slow path, §7.3; v0.1 counts them unsupported) | blocking spike, policy report | v0.2 |
| Config keys for the control stage (`control.allowedNetworks`, `control.blockedQTypes`) and a hostname source (`policy.HostnameResolver`: reverse PTR cache, DHCP leases, UniFi) for hostname/nameGlob client matchers | policy report | v0.2 (moved from M3 / v0.2: new configuration surface, not an exit criterion) |
| Blocking toggle replication: a `blocking.Publisher` writing `kudurru.io/blocking-disabled-until` on the `kudurru-runtime` ConfigMap (kube) or state.db (sqlite), and a watcher calling `Toggle.Apply` (`POST /dns/blocking` toggles the node-local Toggle since docs/m3/api.md) | policy report | v0.2 (moved from M3: the toggle stays per node in v0.1; the release notes list it) |
| Blocking: conformance corpus black-box against an AdGuard Home container (needs Docker); move benchgate to hack/; lazy-DFA regex engine for regex-heavy users | blocking spike | v0.2 (moved from M2: the AdGuard Home corpus can run on a GitHub Docker runner) |
| Upstream: `upstreams.importFile` for dnsmasq import; cache inspect over REST (`Set.Status()` is in GET /stats/upstreams and flush in POST /cache/flush since docs/m3/api.md); cache and strategy benchmarks in `bench/limits.txt` | cache-upstream report | v0.2 (moved from M3: additions, nothing in v0.1 depends on them) |
| dnsserver: config keys for `Limits.QUICMaxConns`, `MaxTCPQueries`, idle timeouts; DoH `Alt-Svc`; RFC 8467 padding; DoH path client IDs | dnsserver spike | v0.2 (moved from M2/M3: the limits have code defaults) |
| Transfer chunks are budgeted by uncompressed size (69 messages of ~42 KB for 100k records); budget by compressed size for fewer, fuller messages | docs/m2/xfr.md | later |
| D60 blocked-services and safe-search catalogue in a separate public data repository: v0.1 embeds a kudurru-authored catalogue (`internal/listdata/catalogue`) that `blocking.catalogue.url` can replace at runtime (D120); move the data to its own repository once the project owner creates it | D120 | v0.2 (the owner creates and publishes the repository) |
| ratelimit: a slip counter next to `kudurru_dns_dropped_total{reason="ratelimit"}`; exemptions per ClientGroup (`ratelimit.*` reload without a restart landed in docs/m5/burndown-server.md) | hardening report | v0.2 |
| Multi-socket listeners for DoT (same code path, left at one socket) and DoQ/DoH3 (needs QUIC connection-ID aware routing) | hardening report | v0.2 (moved from M3: one socket limits throughput only) |
| Settings hot reload caveats: a `querylog.privacy`/`anonymize`/`exclude.*` reload builds a new privacy transform, so `pseudonym` mode draws a new HMAC key (pseudonyms change at the reload); `cache.maxEntries` growth keeps the shard count chosen at start (very large growth means large shards); `blocking.refreshInterval`, `blocking.allowDegraded`, `querylog.enabled/sink`, `auth.*` still need a restart (restartRequired) | docs/m5/burndown-server.md | v0.2 |

## API (docs/m3/api.md)

| Item | Source | Target |
|---|---|---|
| `PATCH /node/config` changes stay in memory (log.level applies at once; nothing persists them, the node file is the operator's): persist node-scope API changes somewhere (a `node.d/` overlay?) or drop the route's write half | docs/m5/burndown-server.md | v0.2 |
| Scatter-gather over the internal listener for `scope=cluster` stats/log calls, `GET /nodes` peer readiness and `GET /nodes/{name}/health` of other nodes (501 today) | api report | v0.2 (moved from M4: the routes answer 501 explicitly) |
| `GET /auth/me` permissions are evaluated at cluster scope; zone-scoped grants show only in `roles` | api report | v0.2 |
| external-dns: read `DNSEndpoint` CRs directly (§5.6) and a `kudurru externaldns-webhook` sidecar subcommand; the webhook has no authentication (loopback or NetworkPolicy) | api report | v0.2 (moved from M4: the webhook provider covers external-dns in v0.1) |
| Object list routes read the store (an apiserver round trip in kube mode); serve them from the zonecache snapshot when it is live | api report | v0.2 (moved from M4: performance only) |
| CLI: switch from the hand-written `internal/apiclient` transport to the generated `internal/apiclient/openapi` client and put it under `apiclient.Client` (surface listed in docs/m3/cli.md) | api report, docs/m3/cli.md | v0.2 (moved from M3/M4: internal refactor, no user-visible change; merged rows) |
| Audit: the sqlite recorder folds authz path, request id and client address into `reason` until audit migration 2 adds columns | api report | v0.2 (moved from M3: the reason field carries the values until migration 2) |
## Edge and VIP

| Item | Source | Target |
|---|---|---|
| Edge resolver-cache flush for remote edges. Local edges (sidecar, compose) are flushed by `dnsdist.CacheFlusher` wired into serve (blocking toggle changes and every compiled list snapshot). Remote edges (edge Deployment/DaemonSet) need a pull path, e.g. a blocking generation in `internalapi.ZoneList` that the supervisor's zone poll turns into a resolver flush | docs/m3/edge.md, burndown-edge-release | v0.2 |
| `edge.*`/`vip.*` settings keys (hostOnly) and serve wiring of `vip.NewKeepalived` (Events → audit `cluster.vip.changed`, event `vip.master_changed`; the Nodes/Edge pages) | docs/m3/edge.md | v0.2 (moved from M4: the supervisors run without it; only the local node's VIP view and audit events wait) |
| `vip.ServiceProvider` (kube-vip/MetalLB): Owner from the Service status / announcing node; chart annotations from `Annotations()` | docs/m3/edge.md | v0.2 (moved from M4: MetalLB and kube-vip announce without it; only Owner reporting waits) |
| `--hmac-key-file` per VRID in the chart (one shared key file today; compose uses the single-file form too) | docs/m3/edge.md | v0.2 |
| v0.2 console diff-apply (`newServer`/`setDown`/`rmServer`) and `reloadAllCertificates()` instead of restarts, gated on spike (a) (D17) | §3.6, docs/m3/edge.md | v0.2 |

## Auth

| Item | Source | Target |
|---|---|---|
| Binding revocation times (`rbac.BindingSource`, which stops honouring a sealed session grant once a binding change removed it) live in memory per replica; after a restart only the "a live binding still provides it" rule applies. Persist a revocation epoch (state.db / the runtime ConfigMap) or bump a cluster-wide grants-valid-after on narrowing | docs/m3/auth.md | v0.2 |
| OIDC identities have no stored record: no per-user "log out everywhere" or disable, and an OIDC user's personal tokens stay valid until expiry while bindings still provide their frozen grants. Needs the v0.2 user store (with `autoProvision: never`, `syncGroups`) | docs/m3/auth.md | v0.2 |
| Login lockouts (5/IP → 5 min, 10/user → 15 min) are in memory per replica, so N replicas allow N× the attempts; share counters through the store if that matters | docs/m3/auth.md | v0.2 |
| Projected ServiceAccount tokens verified offline by JWKS are not re-checked against a deleted pod or ServiceAccount within their lifetime (TokenReview would catch it); cap the accepted `exp - iat` or re-validate with TokenReview every few minutes | docs/m3/auth.md | v0.2 |
| `autoProvision: never` and `syncGroups` once the user store exists; RP-initiated logout | auth spike | v0.2 |

## Security (docs/m5/security.md)

| Item | Source | Target |
|---|---|---|
| external-dns webhook: optional shared-secret header (`externalDNS.webhook.tokenFile`, the external-dns webhook provider can send custom headers) so the listener can leave loopback without relying on a NetworkPolicy | docs/m5/security.md | v0.2 |
| A `ServiceAccount` binding subject matches a kudurru service account and a Kubernetes ServiceAccount of the same namespace/name; consider a subject field (`provider: kubernetes|kudurru`) or documenting the overlap in §8.5 | docs/m5/security.md | v0.2 |

## Security (docs/m5/security-v0.1-round.md)

| Item | Source | Target |
|---|---|---|
| A password change or `kudurru admin reset-password` revokes sessions but not the user's API tokens; offer revoking them with a reset (`--revoke-tokens`, and a checkbox on the reset dialog) | docs/m5/security-v0.1-round.md | v0.1.x |
| Approving a request whose entry already exists overwrites that entry's comment with the approver's note; keep the comment of an entry the approval did not add | docs/m5/security-v0.1-round.md | v0.1.x |
| Approve ends an expired earlier approval (`end`) before adding its entry; if the add fails the request stays marked approved with its entry removed. Write the ended state first, or roll it back | docs/m5/security-v0.1-round.md | v0.1.x |
| Release workflows: pin third-party actions by SHA with Dependabot for `github-actions`; pin goreleaser, helm and cosign versions; run `make test` and `make helm-check` in a separate read-only job before the release job and disable setup-go's cache there; refuse to promote a candidate whose publish failed (`gh release view`); image build provenance (`attest-build-provenance` with `subject-digest`, `push-to-registry`) and SBOMs for the supervisor images | docs/m5/security-v0.1-round.md | v0.1.x |
| Repository settings for the release pipeline (cannot live in the repository): a branch ruleset refusing `v*` branch names, required review with CODEOWNERS on `release` and `release-candidate` (`.github/CODEOWNERS` for `/.github/` and `hack/release-tag.sh`), deployment branches for a `release` environment | docs/m5/security-v0.1-round.md | before v0.1.0 |
| Optional `httpsOnly` for `blocking.catalogue.adguardServicesURL` (and BlockList URLs): a plain-http document can be changed in transit | docs/m5/security-v0.1-round.md | v0.2 |

## Dashboard (web/)

| Item | Source | Target |
|---|---|---|
| "Is it allowed?" makes a second request to find list exceptions; the dig answer could report a matched exclusion itself | allowlist stream (D144) | v0.2 |
| Allowlist/Denylist: a group list applies through the group's policies, so a policy covering several groups applies it to all of them; a query-time group scope in the policy engine would make the scope exact | allowlist stream (D143) | v0.2 |
| Regex allow/deny entries get no "overrides block list" information and no response-cache flush (there is no single name to look up or flush) | allowlist stream | v0.2 |
| Entries browser re-parses the whole cached body for every page of a URL source; add a per-digest index if multi-million-line lists are slow (concurrent parses are capped at 4 per node since docs/m5/security-v0.1-round.md V15) | allowlist stream | v0.2 |
| Self-host the brand fonts (Inter, JetBrains Mono; docs/brand) in the Kudurru theme instead of the system fallback stack | UX round 3 (D140) | v0.2 |
| Settings schema `Shape` has no help field, so list items and map keys show no help of their own (fields and enum options do) | UX round 3 | v0.2 |
| Record delete confirmation as an Astryx AlertDialog; `GET /queries/export` link in the query log | ui report | v0.2 (moved from M4: the API has no `GET /queries/export` route yet) |
## CLI

| Item | Source | Target |
|---|---|---|
| `--context` selecting a kubeconfig so CRD verbs go straight to the apiserver (§3.5); a context is always a kudurru API endpoint today | docs/m3/cli.md | v0.2 (moved from M4: a context is a kudurru API endpoint in v0.1) |
| Remaining §3.5 verbs: `import pihole\|adguard\|blocky\|technitium`, `cluster status\|promote\|join-token`, `integrations …`, zone option writes (`PUT/PATCH /zones/{zone}`) (`lameduck` landed in docs/m5/burndown-server.md, `health --vrrp` in e8b36b4) | docs/m3/cli.md | v0.2 (merged rows) |
| Browser (loopback + PKCE) and device-code `kudurru login` (§8.3) | docs/m3/cli.md | v0.2 |
| CLI conformance test against the real `internal/api` router instead of the fake in `cmd/kudurru/fakeapi_test.go`; document `ApplyResult.errorKey/hint`, the wrapped settings body and the SSE event name in architecture.md §10 | docs/m3/cli.md | v0.2 (moved from M3: test-only) |

## Build, CI, docs

| Item | Source | Target |
|---|---|---|
| Bench job on a fixed runner class (also for `BenchmarkUDPSockets` and `BenchmarkPersist200k`) | blocking, store-kube spikes; hardening report | v0.2 (moved from M2: needs a fixed runner class, the owner's call) |

## Deployment (binary behaviour the chart and compose files need)

| Item | Source | Target |
|---|---|---|
| Metrics the §11.6 dashboard and alerts still lack: `kudurru_cluster_fenced` (v0.2), `kudurru_dnssec_key_expiry_timestamp_seconds` (v1.0); `snapshotAge`/`leader` in the `/healthz` JSON (§5.2 human probe). The serial-divergence, edge and VIP panels and alerts shipped in the v0.1 fix round | deploy report | v0.2 (moved from M3-M5: each waits for its feature; merged rows) |
| Settings key `cluster.role: static`: the compose HA read-only mode cannot be expressed until it exists (the chart renders OIDC providers through `auth.oidc.providers`); lands with the `cluster.static` membership of E2E-2 | deploy report | v0.2 (compose HA) |
| Separate path keys for `querylog.db` and `local.db` so `persistence.querylog` and `persistence.local` can be distinct volumes; today either switches `/var/lib/kudurru` to one per-pod claim | deploy report | v0.2 |

## Needs the project owner's hardware

| Item | Source | Target |
|---|---|---|
| Spikes (a) dnsdist console directives under YAML, (b) PROXYv2 + chashed with a real dnsdist and `kdig +quic/+tls/+https`, (h) keepalived 2.4.3 on two VMs (lameduck keeps the VIP, `vip drain` moves it, node loss moves it). Spike (f), kind + MetalLB, runs on GitHub's runners in the kind workflow (hack/kind-e2e.sh) | week-1 spikes, dnsserver spike | owner's hardware (GA gate, docs/release-checklist.md §3) |

## End to end and fuzzing (docs/m5/e2e.md)

Bugs found by the M5 e2e package. E2E-1, E2E-3, E2E-4, E2E-5 and E2E-6 were fixed after M5 (default-group hints fall through to conditional forwarding; `node.lameduck` drain window, default 2s; `keys.Load` reports `Generated` only for a key it created; readiness waits for the first live snapshot; keepalived paths allow braces only as `{vrid}`, 0a25647) and their tests now assert for real. The remaining items keep a fuzz-input filter or a design note naming the id.

| Item | Source | Target |
|---|---|---|
| E2E-2: sqlite nodes cannot form a cluster. serve.go hard-codes `cluster.Self` membership for the sqlite driver, so node B cannot be pointed at node A's internal listener for list bodies, and every sqlite node reports itself as leader. The two-node test drives `/internal/status` and `/internal/lists/{digest}` with the derived credentials instead. Needs a `cluster.static` setting (peers, leader) feeding `cluster.Static` | docs/m5/e2e.md | v0.2 (compose HA) |
| e2e still missing from §13.1: the apiserver-outage test (kill the control plane, restart a pod, answers from local.db; envtest can stop the apiserver, spike 3 did), the 2-VM keepalived failover, the AdGuard Home black-box corpus and the Knot catalog consumer (both need only a Docker runner). kind + Helm with MetalLB is the kind workflow (hack/kind-e2e.sh) | docs/m5/e2e.md | v0.2 (moved from M5: apiserver-outage on envtest, AdGuard Home and Knot on a Docker runner); the 2-VM failover stays a GA gate (docs/release-checklist.md §3) |
