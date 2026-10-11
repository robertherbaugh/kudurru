# Follow-ups

Open items that are not on the milestone exit criteria they came from. Each
names its source (spike or M1 report) and the milestone where it should land.
Close an item by deleting its row and citing the commit.

## M1 exit criteria carried over (docs/architecture.md §13.1)

| Item | Why it slipped | Target |
|---|---|---|
| Multi-socket `SO_REUSEPORT` UDP/TCP listeners (N = GOMAXPROCS, CoreDNS multisocket) | dnsserver spike scoped to protocols; single socket per listener today | M2 (hardening package) |
| Cache `Dump`/`Load` across restarts into local.db | cache package exposes the bytes; no owner for the local.db blob yet | M2 (hardening package) |
| ingress per-prefix QPM rate limiting with TC=1 then drop (§7.1 step 2) | not started | M2 (hardening package) |
| `kudurru_store_*` metrics (write latency, feed lag, watcher count) for the sqlite driver | not started | M2 |
| `kudurru_upstream_responses_total{upstream,rcode}`, `kudurru_upstream_inflight{upstream}`, proto label on requests | partial metric set | M2 |
| Unsynced rule refinement (D80): SERVFAIL only for names in primary zones while forwarding continues; a stale local.db gates only primary zones | authoritative.Source has a single `Synced()` bit | M2 (xfr/zone package) |

## Store and drivers

| Item | Source | Target |
|---|---|---|
| zonecache: refresh the local.db timestamp (`Touch`) from `store.Info().LastContact` instead of the wall clock when it is non-zero; the kube driver exports it since M2 (docs/m2/hardening.md) | hardening report | M2 (zonecache owner) |
| Cold-start cluster-ID check is skipped while the kube driver has never reached the apiserver (its cluster ID is unknown then, the very outage case); compare against an independent source such as a node setting or the downward API | hardening report | v0.2 |
| sqlite: daily leader loop calling `PruneAudit` (400 d); remaining §11.4 audit fields via migration 2; secret redaction in before/after; `cluster_epoch`/`primary_id` setters for v0.2 fencing; `InternalKinds` once internal kinds exist | store-sqlite report | M3 / v0.2 |
| Enforce the 1.5 MiB object cap (etcd) in the sqlite driver (the API refuses larger objects with 413 since docs/m3/api.md) | validator spike | M3 |
| sqlite: `WithActor` from the auth principal in the API middleware (`auth.PrincipalFrom(ctx).ID`, docs/m3/auth.md); daily leader loop calling `PruneAudit` (400 d); remaining §11.4 audit fields (incl. `auth.DecisionFrom(ctx).Path`) via migration 2; secret redaction in before/after; `cluster_epoch`/`primary_id` setters for v0.2 fencing | store-sqlite report | M3 / v0.2 |
| Enforce the 1.5 MiB object cap (etcd) in the sqlite driver and the API | validator spike | M3 |
| `WatchOptions.ResourceVersion` semantics: replay-as-Added (documented) vs true resume | store-kube spike | v0.2 |
| conformance: shared "rejects the same inputs" table run against sqlite and kube | validator spike | M2 |

## Zone, controller, objects

| Item | Source | Target |
|---|---|---|
| `expiresAt`: the zone hash now leaves expired records out, but only a reconcile after the expiry moves the serial; requeue the zone at `Zone.NextExpiry()` | controller report, docs/m2/xfr.md | M2 |
| Controller zone builds parse every record per zone reconcile (paced Apply → status p50 0.4 ms → 2-3 ms at 1,000 record sets); cache parsed endpoints per record-set object and copy RRs into each build | docs/m2/xfr.md | M3 |
| A NOTIFY key field in `ZonePolicy` (NOTIFY is signed with the first `transferTSIGKeyRefs` key today) and a `notifySource` per zone | docs/m2/xfr.md | v0.2 |
| Store-backed internal `TSIGKey` kind behind `dnsserver.TSIGKeyring` (encrypted at rest, §4.1); TSIG on DoH/DoQ/DoH3 (signed requests are treated as unsigned there) | docs/m2/xfr.md | M3 / v0.2 |
| `kudurru doctor` self-NOTIFY round trip reporting the egress address (§4.6) | docs/m2/xfr.md | M4 |
| DS at a delegation point where parent and child are both local routes to the child; needs a DS special case | zone report | v1.0 (DNSSEC) |
| Builder could reject data below a DNAME owner with a condition (RFC 6672 §2.4) | zone report | M2 |
| `make bench` gate for `BenchmarkLookup100k` after re-recording the baseline | zone report | M2 |
| api/v1alpha1: reasons `AsExpected`, `ZoneNotFound`, `NotSelected`, `MissingReference`, `Unsupported` (today constants in internal/controller); `BlockListStatus.unsupported`; `ListSource.subdomains` | controller, blocking reports | M2 |
| api/v1alpha1: `BlockListStatus.unsupported`; `ListSource.subdomains` (the condition reasons moved to api/v1alpha1 in docs/m2/xfr.md) | controller, blocking reports | M2 |
| BlockList status ownership: controller owns Ready/Degraded, listfetcher writes only sources/entries/digests | controller report | M2 (listfetcher) |
| Per-integration Leases: one Runner per elector name or an elector per runnable | controller report | v0.2 |
| objects: `cluster.role` key driving `Options.Static` (the API answers 503 read_only from `CheckAPIWrite` since docs/m3/api.md); reload inline objects without restart; starter blocklist into `bootstrap.Options.StarterBlockList` | objects report | M2/M3 |
| objects: `cluster.role` key driving `Options.Static` (the API answers 503 read_only from `CheckAPIWrite` since docs/m3/api.md); reload inline objects without restart; bootstrap `Policy/default` priority as explicit fallback | objects report | M2/M3 |
| controller `TestKubeDriver` (envtest) reads the SerialBumped events right after the serial moves; under full-suite load the last Event can lag the status write (failed once in the M2 hardening run, passed alone 3/3): wait for the Event too. Likewise `TestMeasureRecordSetChanges/paced_updates` asserts exactly 2,000 status writes and sees 2,001-2,003 with `-cpu 1,2` (reproduced at e56cbea with the M1 memory driver, so not caused by the M2 changes) | hardening report | M2 |
| objects: `cluster.role` key driving `Options.Static` (the API answers 503 read_only from `CheckAPIWrite` since docs/m3/api.md); reload inline objects without restart; starter blocklist into `bootstrap.Options.StarterBlockList`; bootstrap `Policy/default` priority as explicit fallback | objects report | M2/M3 |

## Dataplane

| Item | Source | Target |
|---|---|---|
| Blocking: `$important`/`$badfilter` (considered for v0.1 in the policy package and left to the v0.2 modifier slow path, §7.3; v0.1 counts them unsupported) | blocking spike, policy report | v0.2 |
| dnsserver: honour `pipeline.ErrDrop` (send nothing on UDP/DoQ, close TCP/DoT/DoH) instead of answering SERVFAIL | policy report | M2 (integration) |
| Config keys for the control stage (`control.allowedNetworks`, `control.blockedQTypes`) and a hostname source (`policy.HostnameResolver`: reverse PTR cache, DHCP leases, UniFi) for hostname/nameGlob client matchers | policy report | M3 / v0.2 |
| Blocking toggle replication: a `blocking.Publisher` writing `kudurru.io/blocking-disabled-until` on the `kudurru-runtime` ConfigMap (kube) or state.db (sqlite), and a watcher calling `Toggle.Apply` (`POST /dns/blocking` toggles the node-local Toggle since docs/m3/api.md) | policy report | M3 |
| Blocking: conformance corpus black-box against an AdGuard Home container (needs Docker); FuzzParse in CI; move benchgate to hack/; lazy-DFA regex engine for regex-heavy users | blocking spike | M2 / later |
| Upstream: `upstreams.importFile` for dnsmasq import; cache inspect over REST (`Set.Status()` is in GET /stats/upstreams and flush in POST /cache/flush since docs/m3/api.md); cache and strategy benchmarks in `bench/limits.txt` | cache-upstream report | M3 |
| dnsserver: config keys for `Limits.QUICMaxConns`, `MaxTCPQueries`, idle timeouts; DoH `Alt-Svc`; RFC 8467 padding; DoH path client IDs | dnsserver spike | M2/M3 |
| dnsserver: per-message write deadline on transfer streams (miekg sets none; a client that stops reading holds the connection until the kernel gives up); needs the TCP conn wrapper in the listener code | docs/m2/xfr.md | M2 (hardening) |
| Transfer chunks are budgeted by uncompressed size (69 messages of ~42 KB for 100k records); budget by compressed size for fewer, fuller messages | docs/m2/xfr.md | later |
| Run the dnsdist half of spike (b) and `kdig +quic/+tls/+https` once a dnsdist binary is available | dnsserver spike | hardware |
| pipeline: dedicated `servfail_unsynced` status instead of reusing `servfail_upstream` | zone report | M2 |
| log observer: a rate-limited drop (Response with a nil Msg, status `blocked_rate_limit`) is counted in `kudurru_dns_responses_total` with rcode SERVFAIL although nothing was sent; use an `rcode="none"` label for drops | hardening report | M2 (log stage) |
| ratelimit: apply `ratelimit.*` changes without a restart (cluster-scope keys); a slip counter next to `kudurru_dns_dropped_total{reason="ratelimit"}`; exemptions per ClientGroup once the policy package lands | hardening report | M3 |
| Multi-socket listeners for DoT (same code path, left at one socket) and DoQ/DoH3 (needs QUIC connection-ID aware routing) | hardening report | M3 |

## API (docs/m3/api.md)

| Item | Source | Target |
|---|---|---|
| `POST /blocklists/{ns}/{name}/refresh` answers 501 until the list fetcher exposes an on-demand refresh (`api.Options.Refresher`, e.g. a `listfetcher.Fetcher.RefreshNow(ns, name)` on the leader) | api report | M3 |
| `DELETE /querylog` answers 501 until `sqlitelog.Writer` exports a batched purge (`api.Options.Purger`) | api report | M3 |
| `PATCH /settings` and `/node/config` change the in-memory configuration only: a `SettingsWriter` (Kudurru singleton / kudurru.yaml) and live reload in the stages that read the keys | api report | M3 |
| Scatter-gather over the internal listener for `scope=cluster` stats/log calls, `GET /nodes` peer readiness and `GET /nodes/{name}/health` of other nodes (501 today) | api report | M4 |
| Scalar docs page (huma default) loads its script from unpkg.com and ignores `listeners.api.pathPrefix`; serve an embedded copy (MIT) under the strict CSP | api report | M3 |
| `IAMReader` backed by the auth store for the read-only `/iam/*` routes (empty lists until wired) | api report | M3 |
| `GET /auth/me` permissions are evaluated at cluster scope; zone-scoped grants show only in `roles` | api report | v0.2 |
| external-dns: read `DNSEndpoint` CRs directly (§5.6) and a `kudurru externaldns-webhook` sidecar subcommand; the webhook has no authentication (loopback or NetworkPolicy) | api report | M4 |
| Object list routes read the store (an apiserver round trip in kube mode); serve them from the zonecache snapshot when it is live | api report | M4 |
| CLI: switch from the hand-written `internal/apiclient` transport to the generated `internal/apiclient/openapi` client | api report | M3 (integration) |
| Audit: the sqlite recorder folds authz path, request id and client address into `reason` until audit migration 2 adds columns | api report | M3 |
## Edge and VIP

| Item | Source | Target |
|---|---|---|
| `/internal/zones` on the internal server: `internalapi.PathZones` returning `internalapi.ZoneList{node, zones:[{name, serial, type}]}` (primary and secondary zones) from the zone cache; the client (`internalapi.Client.Zones`) and the edge poller exist and tolerate the 404 | docs/m3/edge.md | M3 (integrator/zones owner) |
| D51 drain path: ingress must stop answering `health.kudurru.internal.` TXT with NOERROR while the node is lameduck (SERVFAIL), so dnsdist's must_resolve check drains it; lameduck never writes the track file (`vip.WithReason(ctx, vip.ReasonLameduck)` refuses Fault) | docs/m3/edge.md | M3 (ingress owner) |
| health.go: call `installVRRPHealth(cmd)` in `newHealthCmd`; then `kudurru health --vrrp` works and `--probe-command "/usr/local/bin/kudurru health --vrrp"` can become the rendered default (§3.6 wording; `kudurru vip probe` today) | docs/m3/edge.md | M3 (CLI) |
| Compose/chart: name static backends (`--backends=node-a=10.0.0.11:5353,...`) so the 2.1 validation function checks node ids; `--hmac-key-file` per VRID or the single-file form; edge ServiceAccount get/list/watch on `discovery.k8s.io/endpointslices`; keepalived DaemonSet `--peers-file` from the Node list | docs/m3/edge.md | M4 (deploy/chart) |
| Flush edge packet caches (`DELETE /api/v1/cache?pool=resolver`) on `POST /dns/blocking` and list refreshes: the resolver pool caches blocked/unblocked answers up to their TTL | docs/m3/edge.md | M4 |
| `edge.*`/`vip.*` settings keys (hostOnly) and serve wiring of `vip.NewKeepalived` (Events → audit `cluster.vip.changed`, event `vip.master_changed`; the Nodes/Edge pages) | docs/m3/edge.md | M4 (integrator) |
| `vip.ServiceProvider` (kube-vip/MetalLB): Owner from the Service status / announcing node; chart annotations from `Annotations()` | docs/m3/edge.md | M4 |
| `kudurru doctor` warns when `vip run --legacy` (unauthenticated VRRP) is in use | docs/m3/edge.md | M4 (CLI) |
| First real `make edge-check` run (dnsdist `--check-config` on 12 golden files, `keepalived -t` on 4); the dynblocks-ebpf case may need `--cap-add SYS_ADMIN`; promote the edge workflow to required | docs/m3/edge.md | M4 (Docker runner) |
| v0.2 console diff-apply (`newServer`/`setDown`/`rmServer`) and `reloadAllCertificates()` instead of restarts, gated on spike (a) (D17) | §3.6, docs/m3/edge.md | v0.2 |

## Auth

| Item | Source | Target |
|---|---|---|
| Binding revocation times (`rbac.BindingSource`, which stops honouring a sealed session grant once a binding change removed it) live in memory per replica; after a restart only the "a live binding still provides it" rule applies. Persist a revocation epoch (state.db / the runtime ConfigMap) or bump a cluster-wide grants-valid-after on narrowing | docs/m3/auth.md | v0.2 |
| OIDC identities have no stored record: no per-user "log out everywhere" or disable, and an OIDC user's personal tokens stay valid until expiry while bindings still provide their frozen grants. Needs the v0.2 user store (with `autoProvision: never`, `syncGroups`) | docs/m3/auth.md | v0.2 |
| Login lockouts (5/IP → 5 min, 10/user → 15 min) are in memory per replica, so N replicas allow N× the attempts; share counters through the store if that matters | docs/m3/auth.md | v0.2 |
| Projected ServiceAccount tokens verified offline by JWKS are not re-checked against a deleted pod or ServiceAccount within their lifetime (TokenReview would catch it); cap the accepted `exp - iat` or re-validate with TokenReview every few minutes | docs/m3/auth.md | v0.2 |
| Chart: the release-namespace Role needs `delete` on `secrets` (token revocation and user/RoleBinding deletion delete the Secrets of internal kinds); today it grants get/list/watch/create/update/patch | docs/m3/auth.md | M3 (chart owner) |
| CLI: `kudurru admin reset-password`, `create-token` and `list-users` as a client of the admin-socket protocol, and `kudurru token generate` (prints a fresh kdr_ token for `KUDURRU_BOOTSTRAP_TOKEN`; the bootstrap error message names it) | docs/m3/auth.md | M3 (CLI) |
| architecture.md: §6.2 `auth.adminSocket.{path,group}` (instead of `listeners.socket`), `auth.kubernetes.mode`; §8 the internal kinds live in `api/internalkinds/v1alpha1` (Go's internal rule forbids `api/internal`); `POST /api/v1/auth/password`; `GET /auth/status` `localLoginMode`; Node principals (internal token) hold implicit viewer+auditor; §5.3 secrets need delete | docs/m3/auth.md | M3 (integrator) |
| `autoProvision: never` and `syncGroups` once the user store exists; RP-initiated logout | auth spike | v0.2 |

## Security (docs/m5/security.md)

| Item | Source | Target |
|---|---|---|
| Principals whose only binding is zone-scoped (zone-admin) get 403 `tokens.own at scope cluster` on `/auth/tokens`: `stack.Guard` checks the permission at cluster scope although every built-in role holds `tokens.own` (the API forwards these routes as `authenticated` for that reason). Fail-closed, but zone-admins cannot mint tokens; check `tokens.own` at any scope the principal holds (keeping token scopes as restrictions) | docs/m5/security.md | v0.1.x |
| Token `lastUsedIP` records `RemoteAddr` (the reverse proxy behind `listeners.api.trustedProxies`); give `tokens.Options` the API's ClientIP like the stack (`apiClientIP` in serve.go) | docs/m5/security.md | v0.1.x |
| external-dns webhook: optional shared-secret header (`externalDNS.webhook.tokenFile`, the external-dns webhook provider can send custom headers) so the listener can leave loopback without relying on a NetworkPolicy | docs/m5/security.md | v0.2 |
| OIDC docs: warn that the default username chain starts at `preferred_username`, which some IdPs let users edit; recommend group bindings or `claimMappings.username.claim: sub` for `User` subjects | docs/m5/security.md | M5 docs |
| A `ServiceAccount` binding subject matches a kudurru service account and a Kubernetes ServiceAccount of the same namespace/name; consider a subject field (`provider: kubernetes|kudurru`) or documenting the overlap in §8.5 | docs/m5/security.md | v0.2 |
| `TestSupervisorPeersFile` (internal/edge/keepalived) times out waiting for the HUP after the peers file changes on the shared M5 machine, also with the package and its only kudurru dependency (internal/edge) as on 347f4c7; the supervisor restarts the fake keepalived instead. Investigate whether the fake exits early under load or the restart path wins the race | M5 security run | M5 |

## Dashboard (web/)

| Item | Source | Target |
|---|---|---|
| api package: serve the shapes the SPA assumes (docs/m3/ui.md "API routes and shapes"; settings schema JSON names, SSE event `query` + `?lastEventId=`, `transfer` on zone rows, `managedBy` on record rows, `/clients` inventory); then `bun run gen:api` and alias `web/src/api/types.ts` to the generated schemas | ui report | M3 (api + ui) |
| Honour `kudurru.io/enabled: "false"` on BlockList/ClientGroup/Policy (controller, policy compiler, list resolver) or add `comment`/`enabled` spec fields to v1alpha1 (decision row) | ui report | M3 |
| Config key for the URL prefix (e.g. `listeners.api.pathPrefix`) feeding `web.Options.BasePath` and the API mount | ui report | M3 (config + integrator) |
| `GET /api/v1/doctor` so Tools shows the same checks as `kudurru doctor` | ui report | M4 |
| Embed pre-compressed dashboard assets to cut the binary impact from ~1.8 MB to ~0.55 MB (D75 headroom) | ui report | M4 |
| Playwright end-to-end run of the dashboard against `kudurru serve` in CI | ui report | M4 |
| Record delete confirmation as an Astryx AlertDialog; `GET /queries/export` link in the query log | ui report | M4 |
## CLI

| Item | Source | Target |
|---|---|---|
| `--context` selecting a kubeconfig so CRD verbs go straight to the apiserver (§3.5); a context is always a kudurru API endpoint today | docs/m3/cli.md | M4 |
| Remaining §3.5 verbs: `import pihole\|adguard\|blocky\|technitium`, `lameduck`, `health --vrrp`, `cluster status\|promote\|join-token`, `integrations …`, zone option writes (`PUT/PATCH /zones/{zone}`) | docs/m3/cli.md | M4 / v0.2 |
| Browser (loopback + PKCE) and device-code `kudurru login` (§8.3) | docs/m3/cli.md | v0.2 |
| Put the generated `internal/apiclient/openapi` client under `apiclient.Client` (surface listed in docs/m3/cli.md) | docs/m3/cli.md | M4 |
| CLI conformance test against the real `internal/api` router instead of the fake in `cmd/kudurru/fakeapi_test.go`; document `ApplyResult.errorKey/hint`, the wrapped settings body and the SSE event name in architecture.md §10 | docs/m3/cli.md | M3 (integrator) |

## Build, CI, docs

| Item | Source | Target |
|---|---|---|
| Bench job on a fixed runner class (also for `BenchmarkUDPSockets` and `BenchmarkPersist200k`) | blocking, store-kube spikes; hardening report | M2 |
| architecture.md: restore the §7.1 step 9/11 bullets overwritten by decision text; document `upstreams.ecs` in §6.2; add `UpstreamGroup` to the §3.3 Request snippet | cache-upstream report | M2 |
| architecture.md: §11.1 gains `kudurru_store_watch_catchups_total{kind}` (driver feed catch-ups; `..._resyncs_total` is zonecache's re-list counter), `kudurru_store_watchers`, `kudurru_ratelimit_buckets`; §6.2 gains `listeners.sockets` and `ratelimit.*`; a decision row for the limiter running before the FORMERR checks and for local.db `layout_version` | hardening report | M2 (integrator) |
| Hardware-bound spikes (a) dnsdist console under YAML, (b) PROXYv2 + chashed with dnsdist, (f) kind + MetalLB mixed-protocol Service, (h) keepalived 2.4.3 on two VMs | week-1 spikes | project owner's hardware |
| architecture.md: record the deploy-package decisions (docs/m3/deploy.md "Architecture changes"): node file at /etc/kudurru/node, preStop sleep instead of `kudurru lameduck 5s`, `ip_unprivileged_port_start` sysctl instead of a file capability, StatefulSet for per-pod persistence in mode=kubernetes, seed objects as post-install hook resources, keepalived as a second container of the edge DaemonSet, CRD chart copies with `helm.sh/resource-policy: keep`, kustomize bases rendered from the chart | deploy report | M3 (integrator) |

## Deployment (binary behaviour the chart and compose files need)

| Item | Source | Target |
|---|---|---|
| Read the settings file the chart mounts (`/etc/kudurru/settings/settings.yaml`, e.g. a `KUDURRU_SETTINGS_FILE` variable or a second `--config` layer) and hot-reload cluster-scope keys on change (D25); then drop the merge of `config.settings` into the node file in `kudurru.nodeConfig` so settings changes stop rolling pods | deploy report | M3 (config/settings) |
| Leader address with `dnsPolicy: Default`: serve.go dials `<pod>.<KUDURRU_HEADLESS_SERVICE>:8082`, which the node resolver usually cannot resolve; look the leader pod's IP up in the headless EndpointSlice (RBAC already granted) and keep the name only for TLS. Until then replicas fall back to fetching lists themselves after 10 min | deploy report | M3 (cluster) |
| Supervisors `kudurru edge run` / `kudurru vip run` with the flags the chart and compose files render (`--backends`, `--backends-from endpointslice:<ns>/<svc>`, `--backend-port`, `--proxy-protocol-secret-file`, `--readiness-address` with `/healthz/ready` and `/healthz/live`, `--dot/--doh/--doq/--doh3`, `--authoritative-cache`, `--dynblocks`, `--ebpf`, `--peers-from nodes:<selector>`, `--node-name`) and the published images | deploy report | M4 |
| CLI used by the docs and runbooks: `kudurru lameduck` (the chart uses a preStop sleep meanwhile), `health --vrrp`, `vip drain`, `admin reset-password`, `backup`/`restore` | deploy report | M3 / M4 |
| Metrics the §11.6 dashboard and alerts still lack: `kudurru_zone_serial{zone}` (serial-divergence panel and alert), `kudurru_edge_backend_up`, `kudurru_vip_master{vip}`, `kudurru_cluster_fenced` (v0.2), `kudurru_dnssec_key_expiry_timestamp_seconds` (v1.0); `snapshotAge`/`leader` in the `/healthz` JSON (§5.2 human probe). Add the panels/alerts in deploy/dashboard_test.go and deploy/prometheus/rules.yaml when they land | deploy report | M3 / M4 |
| Settings key `externalDNS.webhook.listen` (rendered by `externalDNS.webhook.enabled`, accepted by the deploy tests' `pendingKeys` allowlist until the key exists) | deploy report | M3 (API) |
| Settings keys for OIDC identity providers and `cluster.role: static`: the chart's `auth.oidc` (§5.2) and the compose HA read-only mode cannot be expressed until they exist | deploy report | M3 (auth, objects) |
| Consumers of `KUDURRU_ADMIN_PASSWORD_FILE` and `KUDURRU_BOOTSTRAP_TOKEN_FILE` (the chart and compose files already mount them) | deploy report | M3 (auth) |
| Generate the aggregated `kudurru-viewer/editor/admin/auditor` ClusterRoles from `internal/rbac/permissions.yaml` (D31); `charts/kudurru/templates/rbac.yaml` carries a hand-written copy | deploy report | M3 (rbac) |
| Separate path keys for `querylog.db` and `local.db` so `persistence.querylog` and `persistence.local` can be distinct volumes; today either switches `/var/lib/kudurru` to one per-pod claim | deploy report | v0.2 |
| hostNetwork pods cannot use the `ip_unprivileged_port_start` sysctl: binding :53 as uid 65532 there needs `cap_net_bind_service=+ep` on the binary, which ko cannot set; build the release image with a file capability (or document a host sysctl) | deploy report | M4 |
| Pin `gcr.io/distroless/static:nonroot` and `:debug-nonroot` by digest in `.goreleaser.yaml` and the Dockerfile, bumped by the CVE job; §6.1's `-debug` "adds dig" is not possible on distroless (busybox `nslookup` only) | deploy report | release checklist |
