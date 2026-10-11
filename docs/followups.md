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

## Store and drivers

| Item | Source | Target |
|---|---|---|
| Memory driver: deliver initial Watch events from a goroutine for any size; close or re-list instead of silently dropping events for slow watchers; add a conformance case for the slow-watcher contract | zonecache, controller reports | M2 (hardening) |
| Kube driver: expose a last-contact signal (`Info().LastContact`) so zonecache stops refreshing the snapshot timestamp during an apiserver outage | zonecache report | M2 |
| Kube driver: remove `PersistSnapshots`/`MaterializedSnapshot` now that zonecache owns local.db; update the spike tests | zonecache report | M2 |
| Cold start does not check cluster ID: a local.db copied from another cluster is served until the store syncs | zonecache report | M2 |
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

## Auth

| Item | Source | Target |
|---|---|---|
| `roles_snapshot` is frozen at login: bump `sessionsValidAfter` when a RoleBinding is deleted or narrowed, or intersect sealed binding identities with live bindings at Authorize time | auth spike | M3 |
| Export an rbac subject-matching helper so `oidc.BindingResolver` stops duplicating it | auth spike | M3 |
| Wire `Registry.Register`, `session.Authenticator`, `ValidAfter` from the store snapshot, keys from `KUDURRU_ENCRYPTION_KEY[_PREVIOUS][_FILE]` into the API server | auth spike | M3 |
| `autoProvision: never` and `syncGroups` once the user store exists; RP-initiated logout | auth spike | v0.2 |

## Build, CI, docs

| Item | Source | Target |
|---|---|---|
| CI binary-size budget step (D75) | validator spike | M2 |
| Bench job on a fixed runner class; `make envtest` caching in CI | blocking, store-kube spikes | M2 |
| NOTICE: client-go and controller-runtime are linked dependencies, not test data; move them under the right heading | controller report | M2 |
| architecture.md: restore the §7.1 step 9/11 bullets overwritten by decision text; document `upstreams.ecs` in §6.2; add `UpstreamGroup` to the §3.3 Request snippet | cache-upstream report | M2 |
| Hardware-bound spikes (a) dnsdist console under YAML, (b) PROXYv2 + chashed with dnsdist, (f) kind + MetalLB mixed-protocol Service, (h) keepalived 2.4.3 on two VMs | week-1 spikes | project owner's hardware |
