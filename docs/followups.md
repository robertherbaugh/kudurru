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
| sqlite: `WithActor` from the auth principal in the API middleware; daily leader loop calling `PruneAudit` (400 d); remaining §11.4 audit fields via migration 2; secret redaction in before/after; `cluster_epoch`/`primary_id` setters for v0.2 fencing; `InternalKinds` once internal kinds exist | store-sqlite report | M3 / v0.2 |
| Enforce the 1.5 MiB object cap (etcd) in the sqlite driver and the API | validator spike | M3 |
| `WatchOptions.ResourceVersion` semantics: replay-as-Added (documented) vs true resume | store-kube spike | v0.2 |
| Serve internal kinds (Secrets labelled kudurru.io/kind) in the kube driver; `SnapshotOptions.Safe` strips them | store-kube spike | M3 |
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
| objects: `cluster.role` key driving `Options.Static` and `CheckAPIWrite` in the API middleware; reload inline objects without restart; starter blocklist into `bootstrap.Options.StarterBlockList` | objects report | M2/M3 |
| objects: `cluster.role` key driving `Options.Static` and `CheckAPIWrite` in the API middleware; reload inline objects without restart; bootstrap `Policy/default` priority as explicit fallback | objects report | M2/M3 |
| controller `TestKubeDriver` (envtest) reads the SerialBumped events right after the serial moves; under full-suite load the last Event can lag the status write (failed once in the M2 hardening run, passed alone 3/3): wait for the Event too. Likewise `TestMeasureRecordSetChanges/paced_updates` asserts exactly 2,000 status writes and sees 2,001-2,003 with `-cpu 1,2` (reproduced at e56cbea with the M1 memory driver, so not caused by the M2 changes) | hardening report | M2 |
| objects: `cluster.role` key driving `Options.Static` and `CheckAPIWrite` in the API middleware; reload inline objects without restart; starter blocklist into `bootstrap.Options.StarterBlockList`; bootstrap `Policy/default` priority as explicit fallback | objects report | M2/M3 |
| Validator/API: validate unstructured input then the typed object (or default `spec.policy` to `{}` in the CRD) | objects report | M3 |

## Dataplane

| Item | Source | Target |
|---|---|---|
| Blocking: `$important`/`$badfilter` (considered for v0.1 in the policy package and left to the v0.2 modifier slow path, §7.3; v0.1 counts them unsupported) | blocking spike, policy report | v0.2 |
| dnsserver: honour `pipeline.ErrDrop` (send nothing on UDP/DoQ, close TCP/DoT/DoH) instead of answering SERVFAIL | policy report | M2 (integration) |
| Config keys for the control stage (`control.allowedNetworks`, `control.blockedQTypes`) and a hostname source (`policy.HostnameResolver`: reverse PTR cache, DHCP leases, UniFi) for hostname/nameGlob client matchers | policy report | M3 / v0.2 |
| Blocking toggle replication: a `blocking.Publisher` writing `kudurru.io/blocking-disabled-until` on the `kudurru-runtime` ConfigMap (kube) or state.db (sqlite), a watcher calling `Toggle.Apply`, and `POST /dns/blocking` | policy report | M3 |
| Blocking: conformance corpus black-box against an AdGuard Home container (needs Docker); FuzzParse in CI; move benchgate to hack/; lazy-DFA regex engine for regex-heavy users | blocking spike | M2 / later |
| Upstream: `upstreams.importFile` for dnsmasq import; REST exposure of `Set.Status()` and cache flush/inspect; cache and strategy benchmarks in `bench/limits.txt` | cache-upstream report | M3 |
| dnsserver: config keys for `Limits.QUICMaxConns`, `MaxTCPQueries`, idle timeouts; DoH `Alt-Svc`; RFC 8467 padding; DoH path client IDs | dnsserver spike | M2/M3 |
| dnsserver: per-message write deadline on transfer streams (miekg sets none; a client that stops reading holds the connection until the kernel gives up); needs the TCP conn wrapper in the listener code | docs/m2/xfr.md | M2 (hardening) |
| Transfer chunks are budgeted by uncompressed size (69 messages of ~42 KB for 100k records); budget by compressed size for fewer, fuller messages | docs/m2/xfr.md | later |
| Run the dnsdist half of spike (b) and `kdig +quic/+tls/+https` once a dnsdist binary is available | dnsserver spike | hardware |
| pipeline: dedicated `servfail_unsynced` status instead of reusing `servfail_upstream` | zone report | M2 |
| log observer: a rate-limited drop (Response with a nil Msg, status `blocked_rate_limit`) is counted in `kudurru_dns_responses_total` with rcode SERVFAIL although nothing was sent; use an `rcode="none"` label for drops | hardening report | M2 (log stage) |
| ratelimit: apply `ratelimit.*` changes without a restart (cluster-scope keys); a slip counter next to `kudurru_dns_dropped_total{reason="ratelimit"}`; exemptions per ClientGroup once the policy package lands | hardening report | M3 |
| Multi-socket listeners for DoT (same code path, left at one socket) and DoQ/DoH3 (needs QUIC connection-ID aware routing) | hardening report | M3 |

## Auth

| Item | Source | Target |
|---|---|---|
| `roles_snapshot` is frozen at login: bump `sessionsValidAfter` when a RoleBinding is deleted or narrowed, or intersect sealed binding identities with live bindings at Authorize time | auth spike | M3 |
| Export an rbac subject-matching helper so `oidc.BindingResolver` stops duplicating it | auth spike | M3 |
| Wire `Registry.Register`, `session.Authenticator`, `ValidAfter` from the store snapshot, keys from `KUDURRU_ENCRYPTION_KEY[_PREVIOUS][_FILE]` into the API server | auth spike | M3 |
| `autoProvision: never` and `syncGroups` once the user store exists; RP-initiated logout | auth spike | v0.2 |

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

## Build, CI, docs

| Item | Source | Target |
|---|---|---|
| Bench job on a fixed runner class (also for `BenchmarkUDPSockets` and `BenchmarkPersist200k`) | blocking, store-kube spikes; hardening report | M2 |
| architecture.md: restore the §7.1 step 9/11 bullets overwritten by decision text; document `upstreams.ecs` in §6.2; add `UpstreamGroup` to the §3.3 Request snippet | cache-upstream report | M2 |
| architecture.md: §11.1 gains `kudurru_store_watch_catchups_total{kind}` (driver feed catch-ups; `..._resyncs_total` is zonecache's re-list counter), `kudurru_store_watchers`, `kudurru_ratelimit_buckets`; §6.2 gains `listeners.sockets` and `ratelimit.*`; a decision row for the limiter running before the FORMERR checks and for local.db `layout_version` | hardening report | M2 (integrator) |
| Hardware-bound spikes (a) dnsdist console under YAML, (b) PROXYv2 + chashed with dnsdist, (f) kind + MetalLB mixed-protocol Service, (h) keepalived 2.4.3 on two VMs | week-1 spikes | project owner's hardware |
